#include "sandbox.h"

#include <sys/mman.h>
#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <sched.h>
#include <assert.h>
#include <unistd.h>
#include <time.h>

// ----------------------------------------------------------------------------
// Monotonic clock — nanoseconds (portable: Linux + Darwin)
// ----------------------------------------------------------------------------

static inline uint64_t now_ns(void) {
    struct timespec ts;
#if defined(__APPLE__)
    // CLOCK_MONOTONIC is available on Darwin since 10.12; it does NOT sleep.
    clock_gettime(CLOCK_MONOTONIC, &ts);
#else
    // CLOCK_MONOTONIC_RAW avoids NTP slew on Linux — better for intervals.
    clock_gettime(CLOCK_MONOTONIC_RAW, &ts);
#endif
    return (uint64_t)ts.tv_sec * UINT64_C(1000000000) + (uint64_t)ts.tv_nsec;
}

#define IDLE_SPIN_NS       UINT64_C(100000000)   // 100 ms — then sleep forever

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

static size_t page_size(void) {
    static size_t ps = 0;
    if (!ps) ps = (size_t)sysconf(_SC_PAGESIZE);
    return ps;
}

static size_t round_up_page(size_t n) {
    size_t ps = page_size();
    return (n + ps - 1) & ~(ps - 1);
}

// ----------------------------------------------------------------------------
// Ring buffer
// ----------------------------------------------------------------------------

spsc_ring_t* ring_create(void) {
    size_t sz = sizeof(spsc_ring_t);
    void* p = mmap(NULL, sz,
                   PROT_READ | PROT_WRITE,
                   MAP_SHARED | MAP_ANONYMOUS,
                   -1, 0);
    if (p == MAP_FAILED) {
        perror("ring mmap");
        return NULL;
    }
    spsc_ring_t* r = (spsc_ring_t*)p;
    atomic_store_explicit(&r->head, 0, memory_order_relaxed);
    atomic_store_explicit(&r->tail, 0, memory_order_relaxed);
    r->capacity = RING_CAPACITY;
    return r;
}

void ring_destroy(spsc_ring_t* r) {
    munmap(r, sizeof(spsc_ring_t));
}

bool ring_push(spsc_ring_t* r, const work_item_t* item) {
    uint64_t head = atomic_load_explicit(&r->head, memory_order_relaxed);
    uint64_t tail = atomic_load_explicit(&r->tail, memory_order_acquire);
    if (head - tail >= (uint64_t)r->capacity)
        return false;  // full
    r->items[head & (r->capacity - 1)] = *item;
    atomic_store_explicit(&r->head, head + 1, memory_order_release);
    return true;
}

bool ring_pop(spsc_ring_t* r, work_item_t* out) {
    uint64_t tail = atomic_load_explicit(&r->tail, memory_order_relaxed);
    uint64_t head = atomic_load_explicit(&r->head, memory_order_acquire);
    if (head == tail)
        return false;  // empty
    *out = r->items[tail & (r->capacity - 1)];
    atomic_store_explicit(&r->tail, tail + 1, memory_order_release);
    return true;
}

// ----------------------------------------------------------------------------
// Guard-page sandbox memory
// ----------------------------------------------------------------------------

sandbox_mem_t sandbox_mem_create(size_t code_sz, size_t data_sz, size_t stack_sz) {
    sandbox_mem_t m = {0};
    size_t ps = page_size();

    // --- Code region (plain RW until sealed) ---
    code_sz = round_up_page(code_sz);
    void* code = mmap(NULL, code_sz,
                      PROT_READ | PROT_WRITE,
                      MAP_PRIVATE | MAP_ANONYMOUS,
                      -1, 0);
    assert(code != MAP_FAILED);
    m.code      = code;
    m.code_size = code_sz;

    // --- Data region: [guard][data][guard] ---
    data_sz = round_up_page(data_sz);
    size_t data_total = ps + data_sz + ps;
    uint8_t* data_base = mmap(NULL, data_total,
                               PROT_READ | PROT_WRITE,
                               MAP_PRIVATE | MAP_ANONYMOUS,
                               -1, 0);
    assert(data_base != MAP_FAILED);
    mprotect(data_base,                    ps, PROT_NONE);  // underflow guard
    mprotect(data_base + ps + data_sz,     ps, PROT_NONE);  // overflow guard
    m.data      = data_base + ps;
    m.data_size = data_sz;

    // --- Stack region: [guard][stack] — stack grows DOWN ---
    // Guard is at the LOW end; RSP starts at top.
    stack_sz = round_up_page(stack_sz);
    size_t stack_total = ps + stack_sz;
    uint8_t* stack_base = mmap(NULL, stack_total,
                                PROT_READ | PROT_WRITE,
                                MAP_PRIVATE | MAP_ANONYMOUS,
                                -1, 0);
    assert(stack_base != MAP_FAILED);
    mprotect(stack_base, ps, PROT_NONE);  // overflow/underflow guard
    m.stack_top  = stack_base + stack_total;  // RSP starts here
    m.stack_size = stack_sz;

    return m;
}

void sandbox_seal_code(sandbox_mem_t* m) {
    // Enforce W^X: flip code region to read+exec, remove write permission.
    mprotect(m->code, m->code_size, PROT_READ | PROT_EXEC);

#if defined(__aarch64__)
    // Flush instruction cache on ARM (required on Apple Silicon and aarch64 Linux)
    __builtin___clear_cache((char*)m->code, (char*)m->code + m->code_size);
#endif
}

void sandbox_mem_destroy(sandbox_mem_t* m) {
    size_t ps = page_size();
    if (m->code)
        munmap(m->code, m->code_size);
    if (m->data) {
        uint8_t* base = (uint8_t*)m->data - ps;
        munmap(base, ps + m->data_size + ps);
    }
    if (m->stack_top) {
        uint8_t* base = (uint8_t*)m->stack_top - m->stack_size - ps;
        munmap(base, ps + m->stack_size);
    }
    memset(m, 0, sizeof(*m));
}

// ----------------------------------------------------------------------------
// Interpreter thread — parked here permanently after CGo handoff
// ----------------------------------------------------------------------------

// Returns false for OPCODE_SHUTDOWN, true otherwise.
static bool dispatch(work_item_t* item) {
    switch (item->opcode) {
        case OPCODE_SHUTDOWN:
            item->result = 0;
            return false;
        case 0:  // NOP / ping
            item->result = 0xDEADBEEF;
            break;
        default:
            item->result = ~0ULL;
            break;
    }
    return true;
}

void interpreter_thread_main(spsc_ring_t* ring) {
    work_item_t item;
    uint32_t    spin         = 0;
    bool        sleeping     = false;   // true once we cross the 100 ms deadline
    uint64_t    idle_start   = 0;       // ns timestamp when we first went idle

    uint64_t last_head = atomic_load_explicit(&ring->head, memory_order_relaxed);

    for (;;) {
        if (ring_pop(ring, &item)) {
            // ── Work arrived ────────────────────────────────────────────────
            spin     = 0;
            sleeping = false;   // reset: next idle period starts fresh

            bool keep_going = dispatch(&item);

            // Write result back to the ring slot, then mark done.
            // The slot index is tail-1 because ring_pop already advanced tail.
            {
                uint64_t si = (atomic_load_explicit(&ring->tail, memory_order_relaxed) - 1)
                              & (ring->capacity - 1);
                ring->items[si].result = item.result;
                atomic_store_explicit(&ring->items[si].state,
                                      WORK_STATE_DONE,
                                      memory_order_release);
            }

            if (!keep_going) return;

            last_head = atomic_load_explicit(&ring->head, memory_order_relaxed);
            continue;
        }

        // ── Ring is empty ────────────────────────────────────────────────────

        if (sleeping) {
            // Already past the deadline — sleep indefinitely until woken.
            // futex_wait returns immediately if head changed since we read it,
            // so there is no race between the producer's store and our sleep.
            uint32_t h32 = (uint32_t)atomic_load_explicit(
                               &ring->head, memory_order_relaxed);
            futex_wait((uint32_t*)&ring->head, h32);
            last_head = atomic_load_explicit(&ring->head, memory_order_relaxed);
            continue;
        }

        // ── Still in the spin/yield phase ───────────────────────────────────

        if (spin == 0) {
            // First idle iteration: record when we started being idle.
            idle_start = now_ns();
        }

        if (++spin < 1000) {
            // Hot spin with back-off hint (~1–5 ns/iter).
            CPU_RELAX();
        } else if (spin < 5000) {
            // Yield timeslice — reduces contention, ~1 µs/iter.
            sched_yield();
        } else {
            // Check the 100 ms wall-clock deadline.
            if (now_ns() - idle_start >= IDLE_SPIN_NS) {
                // Crossed the threshold: transition to permanent sleep mode.
                // From here on, every idle iteration is a futex sleep.
                sleeping = true;
            } else {
                // Still within the window: brief sleep then re-check.
                // Reset spin counter so we keep calling now_ns() periodically
                // rather than every iteration (clock_gettime is ~20–50 ns).
                spin = 4000;
                sched_yield();
            }
            last_head = atomic_load_explicit(&ring->head, memory_order_relaxed);
        }
    }
}
