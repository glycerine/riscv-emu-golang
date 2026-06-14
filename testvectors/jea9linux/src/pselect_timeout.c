typedef long s64;

struct timespec {
	s64 tv_sec;
	s64 tv_nsec;
};

static long sys1(long n, long a0) {
	register long x10 __asm__("a0") = a0;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x17) : "memory");
	return x10;
}

static long sys2(long n, long a0, long a1) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x17) : "memory");
	return x10;
}

static long sys6(long n, long a0, long a1, long a2, long a3, long a4, long a5) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x12 __asm__("a2") = a2;
	register long x13 __asm__("a3") = a3;
	register long x14 __asm__("a4") = a4;
	register long x15 __asm__("a5") = a5;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x13), "r"(x14), "r"(x15), "r"(x17) : "memory");
	return x10;
}

static void exit_code(long code) {
	sys1(93, code);
	for (;;) {
	}
}

void _start(void) {
	struct timespec timeout;
	struct timespec got;
	timeout.tv_sec = 0;
	timeout.tv_nsec = 1234;
	if (sys6(72, 0, 0, 0, 0, (long)&timeout, 0) != 0) {
		exit_code(170);
	}
	if (sys2(113, 1, (long)&got) != 0) {
		exit_code(171);
	}
	if (got.tv_sec != 0 || got.tv_nsec != 1234) {
		exit_code(172);
	}
	exit_code(0);
}
