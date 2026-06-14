typedef long s64;

struct timespec {
	s64 tv_sec;
	s64 tv_nsec;
};

static long sys2(long n, long a0, long a1) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x17) : "memory");
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
	struct timespec req;
	struct timespec got;
	req.tv_sec = 0;
	req.tv_nsec = 10000000;
	long rc = sys2(101, (long)&req, 0);
	if (rc != 0) {
		exit_code(20);
	}
	rc = sys2(113, 1, (long)&got);
	if (rc != 0) {
		exit_code(21);
	}
	if (got.tv_sec != 0 || got.tv_nsec != 10000000) {
		exit_code(22);
	}
	exit_code(0);
}
