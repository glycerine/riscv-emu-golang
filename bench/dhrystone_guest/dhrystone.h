/*
 * RV64 freestanding Dhrystone header.
 *
 * This overrides the vendored xendor/dhrystone/dhrystone.h because that one
 * pulls in <stdio.h>, <string.h>, and <sys/*.h>. We provide the same type
 * definitions and macros, but with all stdlib dependencies replaced by
 * our own stubs declared here.
 *
 * Shares the _DHRYSTONE_H include guard so the vendored header is a no-op
 * if accidentally re-included.
 */
#ifndef _DHRYSTONE_H
#define _DHRYSTONE_H

#define Version "C, Version 2.2"

/* Timer: Use RISC-V cycle CSR (rdcycle). */
#define HZ             1000000
#define Too_Small_Time 1
#define CLOCK_TYPE     "rdcycle()"

static inline long dhry_rdcycle(void) {
    unsigned long v;
    __asm__ volatile ("rdcycle %0" : "=r"(v));
    return (long)v;
}

#define Start_Timer() Begin_Time = dhry_rdcycle()
#define Stop_Timer()  End_Time   = dhry_rdcycle()

#define Mic_secs_Per_Second 1000000
#ifndef NUMBER_OF_RUNS
#define NUMBER_OF_RUNS 500000
#endif

/* Struct assignment fallback (Dhrystone supports both memcpy-based and
 * compiler-native struct copy). We use the compiler-native form. */
#define structassign(d, s) d = s

/* Enumerations — prefer the C enum form. */
typedef enum { Ident_1, Ident_2, Ident_3, Ident_4, Ident_5 } Enumeration;

/* General definitions replacing <stdio.h>/<string.h> — stubs live in port.c. */
int printf(const char *fmt, ...);
void debug_printf(const char *str, ...);
char *strcpy(char *dst, const char *src);
int   strcmp(const char *a, const char *b);

#define Null 0
#define true  1
#define false 0

typedef int   One_Thirty;
typedef int   One_Fifty;
typedef char  Capital_Letter;
typedef int   Boolean;
typedef char  Str_30[31];
typedef int   Arr_1_Dim[50];
typedef int   Arr_2_Dim[50][50];

typedef struct record {
    struct record *Ptr_Comp;
    Enumeration    Discr;
    union {
        struct {
            Enumeration Enum_Comp;
            int         Int_Comp;
            char        Str_Comp[31];
        } var_1;
        struct {
            Enumeration E_Comp_2;
            char        Str_2_Comp[31];
        } var_2;
        struct {
            char Ch_1_Comp;
            char Ch_2_Comp;
        } var_3;
    } variant;
} Rec_Type, *Rec_Pointer;

/* alloca — GCC built-in, works in freestanding. */
#define alloca(n) __builtin_alloca(n)

/* setStats stub — used by riscv-tests util.h normally. */
static inline void setStats(int enable) { (void)enable; }

#endif /* _DHRYSTONE_H */
