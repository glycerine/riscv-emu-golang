static long sys0(long n) {
	register long x10 __asm__("a0");
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "=r"(x10) : "r"(x17) : "memory");
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
	if (sys0(172) != 1) {
		exit_code(70);
	}
	if (sys0(178) != 1) {
		exit_code(71);
	}
	exit_code(0);
}
