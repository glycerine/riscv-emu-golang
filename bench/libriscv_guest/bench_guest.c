/*
 * bench_guest.c — RISC-V guest program for emulator benchmarking.
 *
 * Compiled to a static RV64 ELF with musl libc.
 * On macOS: zig cc -target riscv64-linux-musl (brew install zig)
 * On Linux: zig cc -target riscv64-linux-musl  (same command)
 *           or riscv64-linux-gnu-gcc -static
 *
 * Three workloads exercising different emulator subsystems:
 *
 *   fib(500M)        integer ALU + branch loop, zero memory traffic
 *   memstress(32KB)  sequential 64-bit store+load, tests memory throughput
 *   sieve(1M)        branchy byte-level access, tests ICache warmup
 */

#include <stdint.h>
#if !defined(__riscv)
#include <unistd.h>  /* _exit() for native builds */
#endif

/* ── syscall exit (used directly so we don't need stdio) ─────────────────── */

static inline void do_exit(int code) {
#if defined(__riscv)
    register long a0 asm("a0") = code;
    register long a7 asm("a7") = 93;
    asm volatile ("ecall" :: "r"(a0), "r"(a7));
    __builtin_unreachable();
#elif defined(__x86_64__) && defined(__linux__)
    asm volatile ("syscall" :: "a"(60), "D"(code));
    __builtin_unreachable();
#else
    /* macOS / other: use libc */
    _exit(code);
#endif
}

/* ── workloads ───────────────────────────────────────────────────────────── */

static uint64_t fib(uint64_t n) {
    uint64_t a = 0, b = 1;
    for (uint64_t i = 0; i < n; i++) {
        uint64_t t = a + b;
        a = b;
        b = t;
    }
    return a;
}

#define MEMSTRESS_ELEMS  4096
#define MEMSTRESS_ITERS  256

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

#define SIEVE_LIMIT  1000000

static uint32_t sieve(uint8_t *buf, uint32_t limit) {
    for (uint32_t i = 0; i <= limit; i++)
        buf[i] = 1;
    for (uint32_t i = 2; (uint64_t)i * i <= limit; i++)
        if (buf[i])
            for (uint32_t j = i * i; j <= limit; j += i)
                buf[j] = 0;
    uint32_t count = 0;
    for (uint32_t i = 2; i <= limit; i++)
        count += buf[i];
    return count;
}

/* ── static buffers ─────────────────────────────────────────────────────── */

static volatile uint64_t membuf[MEMSTRESS_ELEMS];
static uint8_t           sievebuf[SIEVE_LIMIT + 1];

/* ── main ───────────────────────────────────────────────────────────────── */

int main(void) {
    volatile uint64_t f = fib(500000000ULL);
    (void)f;

    volatile uint64_t ms = memstress(
        (volatile uint64_t *)membuf, MEMSTRESS_ELEMS, MEMSTRESS_ITERS);
    (void)ms;

    volatile uint32_t primes = sieve(sievebuf, SIEVE_LIMIT);
    (void)primes;

    do_exit(0);
    return 0;
}

#ifndef __linux__
/* freestanding! (no C stdlib) Define the entry point the linker looks for */
void _start(void) {
    /* Call your logic */
    main();
    
    /* Ensure we never return, as there is no OS to return to */
    do_exit(0);
}
#endif

