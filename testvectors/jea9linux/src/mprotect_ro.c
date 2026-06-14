static long sys3(long n, long a0, long a1, long a2) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x12 __asm__("a2") = a2;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x17) : "memory");
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
	register long x10 __asm__("a0") = code;
	register long x17 __asm__("a7") = 93;
	__asm__ volatile("ecall" : : "r"(x10), "r"(x17) : "memory");
	for (;;) {
	}
}

void _start(void) {
	long addr = sys6(222, 0, 4096, 3, 0x22, -1, 0);
	if (addr <= 0) {
		exit_code(100);
	}
	volatile unsigned char *p = (volatile unsigned char *)addr;
	p[0] = 0x33;
	if (sys3(226, addr, 4096, 1) != 0) {
		exit_code(101);
	}
	if (p[0] != 0x33) {
		exit_code(102);
	}
	if (sys3(226, addr, 4096, 5) != 0) {
		exit_code(103);
	}
	if (p[0] != 0x33) {
		exit_code(104);
	}
	exit_code(0);
}
