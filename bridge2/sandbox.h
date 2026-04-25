#pragma once

#include <stdint.h>
#include <stddef.h>
#include <stdbool.h>
#include <stdatomic.h>

// ----------------------------------------------------------------------------
// Platform portability
// ----------------------------------------------------------------------------

#if defined(__x86_64__)
#  define CPU_RELAX()  __asm__ volatile("pause" ::: "memory")
#elif defined(__aarch64__)
#  define CPU_RELAX()  __asm__ volatile("yield" ::: "memory")
#else
#  define CPU_RELAX()  ((void)0)
#endif

// ----------------------------------------------------------------------------
// Futex abstraction (Linux: futex syscall, Darwin: __ulock)
// ----------------------------------------------------------------------------

#if defined(__linux__)
#  include <linux/futex.h>
#  include <sys/syscall.h>
#  include <unistd.h>

static inline void futex_wait(uint32_t* addr, uint32_t val) {
    syscall(SYS_futex, addr, FUTEX_WAIT_PRIVATE, val, NULL, NULL, 0);
}
static inline void futex_wake(uint32_t* addr) {
    syscall(SYS_futex, addr, FUTEX_WAKE_PRIVATE, 1, NULL, NULL, 0);
}

#elif defined(__APPLE__)
extern int __ulock_wait(uint32_t op, void* addr, uint64_t val, uint32_t timeout_us);
extern int __ulock_wake(uint32_t op, void* addr, uint64_t val);
#define UL_COMPARE_AND_WAIT  1
#define ULF_WAKE_ALL         0x00000100

static inline void futex_wait(uint32_t* addr, uint32_t val) {
    __ulock_wait(UL_COMPARE_AND_WAIT, addr, (uint64_t)val, 0);
}
static inline void futex_wake(uint32_t* addr) {
    __ulock_wake(UL_COMPARE_AND_WAIT | ULF_WAKE_ALL, addr, 0);
}
#else
#  error "Unsupported platform: need Linux or Darwin"
#endif

// ----------------------------------------------------------------------------
// Work item — fits in one cache line (64 bytes)
// ----------------------------------------------------------------------------

#define WORK_STATE_EMPTY   0
#define WORK_STATE_POSTED  1
#define WORK_STATE_DONE    2

#define OPCODE_SHUTDOWN    0xFFFFFFFF

typedef struct {
    _Atomic uint32_t state;
    uint32_t         opcode;
    uint64_t         arg0;
    uint64_t         arg1;
    uint64_t         arg2;
    uint64_t         result;
    uint8_t          _pad[24];
} work_item_t;

_Static_assert(sizeof(work_item_t) == 64, "work_item_t must be 64 bytes");

// ----------------------------------------------------------------------------
// SPSC ring buffer — head and tail on separate cache lines
// ----------------------------------------------------------------------------

#define RING_CAPACITY 1024   // must be power of 2

typedef struct {
    _Atomic uint64_t head;           // written by producer (Go side)
    uint8_t          _pad0[56];
    _Atomic uint64_t tail;           // written by consumer (C side)
    _Atomic uint32_t sleeping;       // 1 when C thread is in futex sleep
    uint8_t          _pad1[52];
    uint32_t         capacity;
    uint8_t          _pad2[60];
    work_item_t      items[RING_CAPACITY];
} spsc_ring_t;

// ----------------------------------------------------------------------------
// Guard-page-protected memory region
// ----------------------------------------------------------------------------

typedef struct {
    void*  code;
    size_t code_size;
    void*  data;
    size_t data_size;
    void*  stack_top;
    size_t stack_size;
} sandbox_mem_t;

// ----------------------------------------------------------------------------
// API
// ----------------------------------------------------------------------------

spsc_ring_t*  ring_create(void);
void          ring_destroy(spsc_ring_t* r);
bool          ring_push(spsc_ring_t* r, const work_item_t* item);
bool          ring_pop(spsc_ring_t* r, work_item_t* out);

sandbox_mem_t sandbox_mem_create(size_t code_sz, size_t data_sz, size_t stack_sz);
void          sandbox_mem_destroy(sandbox_mem_t* m);
void          sandbox_seal_code(sandbox_mem_t* m);

void          interpreter_thread_main(spsc_ring_t* ring);  // never returns
