/*
 * bench_guest.c — RISC-V guest program for emulator benchmarking.
 *
 * Compiled to a static RV64 ELF and loaded by both our Go emulator
 * (future) and libriscv for comparison benchmarks.
 *
 * Designed to be freestanding: no libc dependency. Uses only inline
 * syscalls for exit so it runs under any minimal Linux-personality
 * emulator (syscall 93 = exit).
 *
 * Provides three workloads exercising different emulator subsystems:
 *
 *   fib(n)       — iterative Fibonacci: integer ALU + branch-heavy loop.
 *                  Zero memory traffic beyond the stack frame.
 *
 *   memstress()  — sequential 64-bit store then load over a 32KB buffer.
 *                  Tests emulator Load64/Store64 throughput directly.
 *
 *   sieve()      — Sieve of Eratosthenes to 1M: branchy byte-level access.
 *                  Tests ICache warmup on a non-sequential access pattern.
 *
 * Build (Linux, riscv64-linux-gnu-gcc):
 *   riscv64-linux-gnu-gcc -O2 -march=rv64imafd -mabi=lp64d -static \
 *       -o bench_guest.elf bench_guest.c
 *
 * Build (macOS, riscv64-unknown-elf-gcc from brew riscv-gnu-toolchain):
 *   riscv64-unknown-elf-gcc -O2 -march=rv64imac -mabi=lp64 \
 *       -nostdlib -nostartfiles -Wl,--gc-sections \
 *       -o bench_guest.elf bench_guest.c
 */

#include <stdint.h>

/* ── syscall ───────────────────────────────────────────────────────────── */

static inline void do_exit(int code) {
    register long a0 asm("a0") = code;
    register long a7 asm("a7") = 93; /* SYS_exit */
    asm volatile("ecall" :: "r"(a0), "r"(a7));
    __builtin_unreachable();
}

/* ── workloads ─────────────────────────────────────────────────────────── */

static uint64_t fib(uint64_t n) {
    uint64_t a = 0, b = 1;
    for (uint64_t i = 0; i < n; i++) {
        uint64_t t = a + b;
        a = b;
        b = t;
    }
    return a;
}

#define MEMSTRESS_ELEMS 4096   /* 32 KB of uint64_t */
#define MEMSTRESS_ITERS 256

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

#define SIEVE_LIMIT 1000000

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

/* ── static storage (avoids needing a heap / brk syscall) ─────────────── */

static volatile uint64_t membuf[MEMSTRESS_ELEMS];
static uint8_t           sievebuf[SIEVE_LIMIT + 1];

/* ── entry point ───────────────────────────────────────────────────────── */
/*
 * _start is used instead of main() so this compiles identically with:
 *   - riscv64-linux-gnu-gcc  (Linux cross, with glibc _start wrapper)
 *   - riscv64-unknown-elf-gcc (macOS brew, bare-metal, no glibc)
 *
 * libriscv's Linux personality sets up the stack and calls _start,
 * so this entry point works correctly in both cases.
 *
 * __attribute__((used)) prevents the linker from GC-ing _start when
 * -Wl,--gc-sections is passed (macOS bare-metal build).
 */
__attribute__((used))
void _start(void) {
    volatile uint64_t f = fib(500000000ULL);
    (void)f;

    volatile uint64_t ms = memstress(
        (volatile uint64_t *)membuf, MEMSTRESS_ELEMS, MEMSTRESS_ITERS);
    (void)ms;

    volatile uint32_t primes = sieve(sievebuf, SIEVE_LIMIT);
    (void)primes;

    do_exit(0);
}
