/*
 * RV64 freestanding port implementation.
 * See core_portme.h for strategy notes.
 */
#include "coremark.h"
#include "core_portme.h"

/* Seeds — CoreMark requires these as volatile so a compiler can't
 * pre-compute the workload at compile time. */
#if VALIDATION_RUN
volatile ee_s32 seed1_volatile = 0x3415;
volatile ee_s32 seed2_volatile = 0x3415;
volatile ee_s32 seed3_volatile = 0x66;
#endif
#if PERFORMANCE_RUN
volatile ee_s32 seed1_volatile = 0x0;
volatile ee_s32 seed2_volatile = 0x0;
volatile ee_s32 seed3_volatile = 0x66;
#endif
#if PROFILE_RUN
volatile ee_s32 seed1_volatile = 0x8;
volatile ee_s32 seed2_volatile = 0x8;
volatile ee_s32 seed3_volatile = 0x8;
#endif
volatile ee_s32 seed4_volatile = ITERATIONS;
volatile ee_s32 seed5_volatile = 0;

/* Timer via RISC-V rdcycle CSR. Returns low 32 bits of the 64-bit counter —
 * sufficient for all sub-benchmark durations. */
CORETIMETYPE
barebones_clock(void)
{
    unsigned long cycles;
    __asm__ volatile ("rdcycle %0" : "=r"(cycles));
    return (CORETIMETYPE)cycles;
}

#define CLOCKS_PER_SEC             1000000000UL /* assumes ~1 GHz; only affects reported score */
#define GETMYTIME(_t)              (*_t = barebones_clock())
#define MYTIMEDIFF(fin, ini)       ((fin) - (ini))
#define TIMER_RES_DIVIDER          1
#define SAMPLE_TIME_IMPLEMENTATION 1
#define EE_TICKS_PER_SEC           (CLOCKS_PER_SEC / TIMER_RES_DIVIDER)

static CORETIMETYPE start_time_val, stop_time_val;

void start_time(void) { GETMYTIME(&start_time_val); }
void stop_time(void)  { GETMYTIME(&stop_time_val); }

CORE_TICKS
get_time(void)
{
    return (CORE_TICKS)(MYTIMEDIFF(stop_time_val, start_time_val));
}

secs_ret
time_in_secs(CORE_TICKS ticks)
{
    return ((secs_ret)ticks) / (secs_ret)EE_TICKS_PER_SEC;
}

ee_u32 default_num_contexts = 1;

void
portable_init(core_portable *p, int *argc, char *argv[])
{
    (void)argc;
    (void)argv;
    p->portable_id = 1;
}

void
portable_fini(core_portable *p)
{
    p->portable_id = 0;
}

/* No-op printf — no stdio channel in the freestanding guest. Could be wired
 * to an ecall for a write() syscall if diagnostics are ever needed. */
int
ee_printf(const char *fmt, ...)
{
    (void)fmt;
    return 0;
}
