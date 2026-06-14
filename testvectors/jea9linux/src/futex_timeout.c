typedef long s64;

enum {
	FUTEX_WAIT = 0,
};

struct timespec {
	s64 tv_sec;
	s64 tv_nsec;
};

static volatile int futex_word = 1;

static long sys2(long n, long a0, long a1) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x17) : "memory");
	return x10;
}

static long sys4(long n, long a0, long a1, long a2, long a3) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x12 __asm__("a2") = a2;
	register long x13 __asm__("a3") = a3;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x13), "r"(x17) : "memory");
	return x10;
}

static void exit_code(long code) {
	register long x10 __asm__("a0") = code;
	register long x17 __asm__("a7") = 93;
	__asm__ volatile("ecall" : : "r"(x10), "r"(x17) : "memory");
	for (;;) {
	}
}

void _start(void) {
	struct timespec timeout;
	struct timespec got;
	timeout.tv_sec = 0;
	timeout.tv_nsec = 10000000;
	long rc = sys4(98, (long)&futex_word, FUTEX_WAIT, 1, (long)&timeout);
	if (rc != -110) {
		exit_code(110);
	}
	rc = sys2(113, 1, (long)&got);
	if (rc != 0) {
		exit_code(111);
	}
	if (got.tv_sec != 0 || got.tv_nsec != 10000000) {
		exit_code(112);
	}
	exit_code(0);
}
