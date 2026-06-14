static long sys2(long n, long a0, long a1) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x17) : "memory");
	return x10;
}

static long sys3(long n, long a0, long a1, long a2) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x12 __asm__("a2") = a2;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x17) : "memory");
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
	unsigned long value = 0;
	long fd = sys2(19, 5, 0);
	if (fd < 3) {
		exit_code(130);
	}
	if (sys3(63, fd, (long)&value, 8) != 8 || value != 5) {
		exit_code(131);
	}
	value = 7;
	if (sys3(64, fd, (long)&value, 8) != 8) {
		exit_code(132);
	}
	value = 0;
	if (sys3(63, fd, (long)&value, 8) != 8 || value != 7) {
		exit_code(133);
	}
	exit_code(0);
}
