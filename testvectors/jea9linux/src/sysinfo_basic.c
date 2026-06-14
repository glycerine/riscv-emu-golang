static long sys1(long n, long a0) {
	register long x10 __asm__("a0") = a0;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x17) : "memory");
	return x10;
}

static void exit_code(long code) {
	sys1(93, code);
	for (;;) {
	}
}

void _start(void) {
	unsigned char raw[112];
	for (unsigned long i = 0; i < sizeof(raw); i++) {
		raw[i] = 0xA5;
	}
	if (sys1(179, (long)raw) != 0) {
		exit_code(180);
	}
	unsigned long uptime = *(unsigned long *)(raw + 0);
	unsigned long totalram = *(unsigned long *)(raw + 32);
	unsigned long freeram = *(unsigned long *)(raw + 40);
	unsigned int procs = *(unsigned short *)(raw + 80);
	unsigned int mem_unit = *(unsigned int *)(raw + 104);
	if (uptime != 42) {
		exit_code(181);
	}
	if (totalram != 64UL * 1024UL * 1024UL || freeram != 64UL * 1024UL * 1024UL) {
		exit_code(182);
	}
	if (procs != 1 || mem_unit != 1) {
		exit_code(183);
	}
	exit_code(0);
}
