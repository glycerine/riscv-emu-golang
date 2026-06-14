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
	unsigned char mask[8];
	for (int i = 0; i < 8; i++) {
		mask[i] = 0xff;
	}
	long rc = sys3(123, 0, sizeof(mask), (long)mask);
	if (rc != 0) {
		exit_code(120);
	}
	if (mask[0] != 1) {
		exit_code(121);
	}
	for (int i = 1; i < 8; i++) {
		if (mask[i] != 0) {
			exit_code(122);
		}
	}
	exit_code(0);
}
