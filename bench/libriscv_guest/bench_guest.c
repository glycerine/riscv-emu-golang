/*
 * bench_guest.c — RISC-V guest program for emulator benchmarking.
 *
 * Compiled to RV64IMAC and loaded by both our Go emulator (future)
 * and libriscv for comparison. Provides three exported workloads:
 *
 *   fib(n)         — recursive Fibonacci: tests integer ALU + call stack
 *   memstress(n)   — sequential 64-bit read/write loop: tests memory throughput
 *   sieve(n)       — Sieve of Eratosthenes: tests branchy memory access
 *
 * Each function uses only the syscall ABI for I/O so it runs in any
 * minimal Linux-personality emulator (exit=93, write=64).
 *
 * Build:
 *   riscv64-linux-gnu-gcc -O2 -static -o bench_guest.elf bench_guest.c
 */

#include <stdint.h>

/* ── syscall stubs ─────────────────────────────────────────────── */

static inline long _syscall1(long nr, long a0) {
    register long ra0 asm("a0") = a0;
    register long ra7 asm("a7") = nr;
    asm volatile("ecall" : "+r"(ra0) : "r"(ra7) : "memory");
    return ra0;
}

static inline long _syscall3(long nr, long a0, long a1, long a2) {
    register long ra0 asm("a0") = a0;
    register long ra1 asm("a1") = a1;
    register long ra2 asm("a2") = a2;
    register long ra7 asm("a7") = nr;
    asm volatile("ecall" : "+r"(ra0) : "r"(ra1), "r"(ra2), "r"(ra7) : "memory");
    return ra0;
}

#define SYS_write  64
#define SYS_exit   93

static void do_exit(int code) {
    _syscall1(SYS_exit, code);
    __builtin_unreachable();
}

/* ── workloads ─────────────────────────────────────────────────── */

/*
 * fib — iterative Fibonacci to avoid stack depth issues.
 * Returns fib(n). For n=1000000000 this exercises the integer ALU
 * and branch predictor heavily with zero memory traffic.
 */
static uint64_t fib(uint64_t n) {
    uint64_t a = 0, b = 1;
    for (uint64_t i = 0; i < n; i++) {
        uint64_t t = a + b;
        a = b;
        b = t;
    }
    return a;
}

/*
 * memstress — sequential 64-bit store then load over a buffer.
 * buf must be provided by the caller (stack-allocated in main).
 * Tests memory bandwidth through the emulator's Load64/Store64 path.
 * n is number of 8-byte elements.
 */
static uint64_t memstress(volatile uint64_t *buf, uint64_t n, uint64_t iters) {
    uint64_t sum = 0;
    for (uint64_t iter = 0; iter < iters; iter++) {
        for (uint64_t i = 0; i < n; i++)
            buf[i] = i ^ iter;
        for (uint64_t i = 0; i < n; i++)
            sum ^= buf[i];
    }
    return sum;
}

/*
 * sieve — Sieve of Eratosthenes up to limit.
 * Returns count of primes found. Tests branchy byte-level memory
 * access — the pattern most likely to stress an emulator's ICache.
 */
static uint32_t sieve(uint8_t *buf, uint32_t limit) {
    for (uint32_t i = 0; i <= limit; i++)
        buf[i] = 1;
    for (uint32_t i = 2; (uint64_t)i * i <= limit; i++) {
        if (buf[i]) {
            for (uint32_t j = i * i; j <= limit; j += i)
                buf[j] = 0;
        }
    }
    uint32_t count = 0;
    for (uint32_t i = 2; i <= limit; i++)
        count += buf[i];
    return count;
}

/* ── main ──────────────────────────────────────────────────────── */

/*
 * Stack-allocate the working buffers so they sit in a known,
 * warmed region of guest memory — no malloc/brk needed.
 */
#define MEMSTRESS_ELEMS  4096          /* 32 KB of uint64_t */
#define MEMSTRESS_ITERS  256
#define SIEVE_LIMIT      1000000       /* primes up to 1M */

int main(void) {
    /* fib */
    volatile uint64_t f = fib(500000000ULL);
    (void)f;

    /* memstress */
    static volatile uint64_t membuf[MEMSTRESS_ELEMS];
    volatile uint64_t ms = memstress((volatile uint64_t*)membuf,
                                      MEMSTRESS_ELEMS, MEMSTRESS_ITERS);
    (void)ms;

    /* sieve */
    static uint8_t sievebuf[SIEVE_LIMIT + 1];
    volatile uint32_t primes = sieve(sievebuf, SIEVE_LIMIT);
    (void)primes;

    do_exit(0);
}
