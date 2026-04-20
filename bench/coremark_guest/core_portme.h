/*
 * RV64 freestanding port of CoreMark for the Go RISC-V emulator benchmark.
 *
 * Strategy:
 *   - No stdio, no printf (routed to no-ops).
 *   - Timer uses the RISC-V cycle CSR (rdcycle) — stable across emulator runs.
 *   - _start is provided in start.c; main() is called with argc=1, argv=NULL
 *     via MAIN_HAS_NOARGC=0 / synthesized args (see start.c).
 *   - Exit via ecall (syscall 93, the same exit mechanism bench_guest uses).
 *
 * All CoreMark "score" output is suppressed — the Go benchmark framework
 * measures elapsed time and cpu.Cycle() to compute MIPS, which is what we
 * care about. CoreMark's self-validation CRCs are still computed internally.
 */
#ifndef CORE_PORTME_H
#define CORE_PORTME_H

/* Freestanding-compatible type definitions. We cannot include <stddef.h>
 * because it pulls in other headers. Define what we need directly. */
#ifndef NULL
#define NULL ((void *)0)
#endif

typedef unsigned long  size_t;
typedef long           ssize_t;

typedef signed short   ee_s16;
typedef unsigned short ee_u16;
typedef signed int     ee_s32;
typedef double         ee_f32;
typedef unsigned char  ee_u8;
typedef unsigned int   ee_u32;
typedef unsigned long  ee_ptr_int;   /* 64-bit — matches RV64 pointer width */
typedef size_t         ee_size_t;

#define align_mem(x) (void *)(4 + (((ee_ptr_int)(x)-1) & ~3))

/* Feature flags — all disabled. */
#define HAS_FLOAT      1
#define HAS_TIME_H     0
#define USE_CLOCK      0
#define HAS_STDIO      0
#define HAS_PRINTF     0

#define COMPILER_VERSION "gcc/clang RV64"
#define COMPILER_FLAGS   "freestanding -O2"
#define MEM_LOCATION     "STACK"

/* Timer: read RISC-V cycle CSR. 64-bit counter, wide enough for any run. */
#define CORETIMETYPE ee_u32
typedef ee_u32 CORE_TICKS;

/* SEED_VOLATILE: seeds come from volatile vars patched by the benchmark.
 * MEM_STATIC: put the work buffer in .bss instead of the stack. */
#define SEED_METHOD SEED_VOLATILE
#define MEM_METHOD  MEM_STATIC

#define MULTITHREAD 1
#define USE_PTHREAD 0
#define USE_FORK    0
#define USE_SOCKET  0

/* Our start.c calls main() with no arguments. */
#define MAIN_HAS_NOARGC 1
#define MAIN_HAS_NORETURN 0

extern ee_u32 default_num_contexts;

typedef struct CORE_PORTABLE_S {
    ee_u8 portable_id;
} core_portable;

void portable_init(core_portable *p, int *argc, char *argv[]);
void portable_fini(core_portable *p);

#if !defined(PROFILE_RUN) && !defined(PERFORMANCE_RUN) \
    && !defined(VALIDATION_RUN)
#define PERFORMANCE_RUN 1
#endif

/* Our ee_printf is a no-op — we have no output channel in the freestanding
 * guest, and the Go harness times the run externally. */
int ee_printf(const char *fmt, ...);

#endif /* CORE_PORTME_H */
