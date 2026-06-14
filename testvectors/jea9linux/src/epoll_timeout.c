typedef long s64;

struct timespec {
	s64 tv_sec;
	s64 tv_nsec;
};

struct epoll_event {
	unsigned int events;
	unsigned long data;
} __attribute__((packed));

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
	sys1(93, code);
	for (;;) {
	}
}

void _start(void) {
	struct epoll_event out;
	struct timespec got;
	long epfd = sys1(20, 0);
	if (epfd < 3) {
		exit_code(150);
	}
	if (sys6(22, epfd, (long)&out, 1, 7, 0, 0) != 0) {
		exit_code(151);
	}
	if (sys2(113, 1, (long)&got) != 0) {
		exit_code(152);
	}
	if (got.tv_sec != 0 || got.tv_nsec != 7000000) {
		exit_code(153);
	}
	exit_code(0);
}
