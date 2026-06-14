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
	char buf[32];
	long rc = sys3(63, 0, (long)buf, sizeof(buf));
	if (rc != 14) {
		exit_code(60);
	}
	long wc = sys3(64, 1, (long)buf, rc);
	if (wc != rc) {
		exit_code(61);
	}
	exit_code(0);
}
