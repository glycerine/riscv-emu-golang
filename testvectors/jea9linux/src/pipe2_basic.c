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
	int fds[2];
	char in[3] = {'a', 'b', 'c'};
	char out[3] = {0, 0, 0};
	if (sys2(59, (long)fds, 0) != 0) {
		exit_code(160);
	}
	if (sys3(64, fds[1], (long)in, 3) != 3) {
		exit_code(161);
	}
	if (sys3(63, fds[0], (long)out, 3) != 3) {
		exit_code(162);
	}
	if (out[0] != 'a' || out[1] != 'b' || out[2] != 'c') {
		exit_code(163);
	}
	exit_code(0);
}
