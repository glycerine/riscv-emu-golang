static long sys1(long n, long a0) {
	register long x10 __asm__("a0") = a0;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x17) : "memory");
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
	long initial = sys1(214, 0);
	if (initial <= 0 || (initial & 4095) != 0) {
		exit_code(80);
	}
	long grown = initial + 4096;
	if (sys1(214, grown) != grown) {
		exit_code(81);
	}
	volatile unsigned char *p = (volatile unsigned char *)(grown - 1);
	*p = 0x5a;
	if (*p != 0x5a) {
		exit_code(82);
	}
	if (sys1(214, initial) != initial) {
		exit_code(83);
	}
	exit_code(0);
}
