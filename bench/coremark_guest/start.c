/*
 * Freestanding entry point for the RV64 CoreMark guest.
 *
 * CoreMark's main() is declared as `int main(int argc, char *argv[])` and
 * we've set MAIN_HAS_NOARGC=1 in core_portme.h — so we pass (0, NULL).
 */

int main(int argc, char *argv[]);

static inline void
guest_exit(int code)
{
    register long a0 __asm__("a0") = code;
    register long a7 __asm__("a7") = 93; /* Linux exit */
    __asm__ volatile ("ecall" :: "r"(a0), "r"(a7));
    __builtin_unreachable();
}

void
_start(void)
{
    main(0, (char **)0);
    guest_exit(0);
}

/* CoreMark internals call memcpy/memset/memcmp/strcpy occasionally (through
 * coremark.h or indirectly). Provide minimal freestanding versions so we
 * don't need to link libc. */

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

unsigned long
strlen(const char *s)
{
    const char *p = s;
    while (*p) {
        p++;
    }
    return (unsigned long)(p - s);
}

char *
strcpy(char *dst, const char *src)
{
    char *d = dst;
    while ((*d++ = *src++) != 0) {
    }
    return dst;
}

char *
strcat(char *dst, const char *src)
{
    char *d = dst;
    while (*d) {
        d++;
    }
    strcpy(d, src);
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
