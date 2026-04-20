/*
 * Freestanding RV64 port for Dhrystone.
 *
 * Provides: _start entry point, exit via ecall, stubs for printf/debug_printf
 * (the Go harness measures wall time externally), and minimal string/memory
 * helpers that Dhrystone depends on.
 */

int main(int argc, char *argv[]);

static inline void
guest_exit(int code)
{
    register long a0 __asm__("a0") = code;
    register long a7 __asm__("a7") = 93;
    __asm__ volatile ("ecall" :: "r"(a0), "r"(a7));
    __builtin_unreachable();
}

void
_start(void)
{
    main(0, (char **)0);
    guest_exit(0);
}

/* Output is a no-op — we can't render text without stdio. Dhrystone's final
 * "Dhrystones per Second" output would be useful but we rely on the Go
 * benchmark framework instead, which measures cycles and wall time. */
int
printf(const char *fmt, ...)
{
    (void)fmt;
    return 0;
}

/* debug_printf is defined as a no-op in xendor/dhrystone/dhrystone.c, so
 * we don't redefine it here. */

/* String functions — freestanding needs these. */
char *
strcpy(char *dst, const char *src)
{
    char *d = dst;
    while ((*d++ = *src++) != 0) {
    }
    return dst;
}

int
strcmp(const char *a, const char *b)
{
    while (*a && *a == *b) {
        a++;
        b++;
    }
    return (int)(unsigned char)*a - (int)(unsigned char)*b;
}

unsigned long
strlen(const char *s)
{
    const char *p = s;
    while (*p) {
        p++;
    }
    return (unsigned long)(p - s);
}

/* Memory helpers — GCC emits calls to these for struct assignment etc. */
void *
memcpy(void *dst, const void *src, unsigned long n)
{
    unsigned char *d = dst;
    const unsigned char *s = src;
    while (n--) {
        *d++ = *s++;
    }
    return dst;
}

void *
memset(void *dst, int c, unsigned long n)
{
    unsigned char *d = dst;
    while (n--) {
        *d++ = (unsigned char)c;
    }
    return dst;
}

int
memcmp(const void *a, const void *b, unsigned long n)
{
    const unsigned char *pa = a, *pb = b;
    while (n--) {
        int d = (int)*pa++ - (int)*pb++;
        if (d) {
            return d;
        }
    }
    return 0;
}
