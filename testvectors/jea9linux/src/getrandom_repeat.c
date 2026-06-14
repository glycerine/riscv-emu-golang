typedef unsigned long u64;

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
	unsigned char buf[32];
	long rc = sys3(278, (long)buf, 32, 0);
	if (rc != 32) {
		exit_code(30);
	}
	unsigned char acc = 0;
	for (int i = 0; i < 32; i++) {
		acc |= buf[i];
	}
	if (acc == 0) {
		exit_code(31);
	}
	rc = sys3(278, (long)buf, 8, 0x8000);
	if (rc != -22) {
		exit_code(32);
	}
	exit_code(0);
}
