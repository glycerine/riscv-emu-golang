struct rlimit {
	unsigned long cur;
	unsigned long max;
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
	sys1(93, code);
	for (;;) {
	}
}

void _start(void) {
	struct rlimit lim;
	if (sys2(163, 3, (long)&lim) != 0) {
		exit_code(170);
	}
	if (lim.cur != 8UL * 1024UL * 1024UL || lim.max != 8UL * 1024UL * 1024UL) {
		exit_code(171);
	}
	if (sys4(261, 0, 7, 0, (long)&lim) != 0) {
		exit_code(172);
	}
	if (lim.cur != 1024 || lim.max != 1024) {
		exit_code(173);
	}
	if (sys4(261, 2, 7, 0, (long)&lim) != -3) {
		exit_code(174);
	}
	exit_code(0);
}
