/*
 * hello.c — RISC-V guest that writes a fixed message in a tight loop.
 *
 * Built twice, with -DMSG='"..."' set on the command line, to produce
 * two ELFs that differ only in the string they print. Used by the
 * hello benchmark harness to compare ECALL+write throughput across
 * libriscv and our emulator.
 *
 *   hello_libriscv.elf  prints "Hello, libriscv!\n"
 *   hello_gocpu.elf     prints "Hello, Go CPU!\n"
 *
 * Cross-compile with the same zig setup the other guests use:
 *
 *   zig cc -target riscv64-freestanding ... \
 *       -DMSG='"Hello, libriscv!\n"' -o hello_libriscv.elf hello.c
 *
 * The loop count is fixed at ITERS (10000) so the caller can report
 * wall-clock total / ITERS as ns-per-ECALL.
 */

#include <stdint.h>

#ifndef MSG
#define MSG "Hello, world!\n"
#endif
#ifndef ITERS
#define ITERS 10000
#endif

static inline long do_write(int fd, const char *buf, unsigned long count) {
    register long a0 asm("a0") = fd;
    register long a1 asm("a1") = (long)buf;
    register long a2 asm("a2") = (long)count;
    register long a7 asm("a7") = 64;   /* Linux RISC-V SYS_write */
    register long ret asm("a0");
    asm volatile ("ecall"
                  : "=r"(ret)
                  : "r"(a0), "r"(a1), "r"(a2), "r"(a7)
                  : "memory");
    return ret;
}

static inline void do_exit(int code) {
    register long a0 asm("a0") = code;
    register long a7 asm("a7") = 93;   /* Linux RISC-V SYS_exit */
    asm volatile ("ecall" :: "r"(a0), "r"(a7));
    __builtin_unreachable();
}

static const char msg[] = MSG;
#define MSG_LEN (sizeof(msg) - 1)

void _start(void) {
    for (int i = 0; i < ITERS; i++) {
        do_write(1, msg, MSG_LEN);
    }
    do_exit(0);
}
