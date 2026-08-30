typedef long s64;

enum {
	AT_FDCWD = -100,
	O_RDWR = 2,
	PROT_READ = 1,
	PROT_WRITE = 2,
	MAP_SHARED = 1,
	BREADCRUMB_BASE = 0x10002000,
	BREADCRUMB_SIZE = 0x1000,
	BREADCRUMB_REG_CONTROL = 0x10,
	BREADCRUMB_REG_TARGET_PID = 0x68,
	BREADCRUMB_CONTROL_START = 1,
};

struct timespec {
	s64 tv_sec;
	s64 tv_nsec;
};

static volatile unsigned long sink;

static long sys0(long n) {
	register long x10 __asm__("a0");
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "=r"(x10) : "r"(x17) : "memory");
	return x10;
}

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

static long sys4(long n, long a0, long a1, long a2, long a3) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x12 __asm__("a2") = a2;
	register long x13 __asm__("a3") = a3;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x13), "r"(x17) : "memory");
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

static void spin(unsigned long n) {
	unsigned long i;
	for (i = 0; i < n; i++) {
		sink += i + 1;
		__asm__ volatile("" : "+m"(sink) :: "memory");
	}
}

void _start(void) {
	static const char devmem[] = "/dev/mem";
	struct timespec req;
	long fd;
	long page;

	fd = sys4(56, AT_FDCWD, (long)devmem, O_RDWR, 0);
	if (fd < 0) {
		exit_code(80);
	}
	page = sys6(222, 0, BREADCRUMB_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, BREADCRUMB_BASE);
	if (page < 0) {
		exit_code(81);
	}
	sys1(57, fd);

	*(volatile unsigned int *)(page + BREADCRUMB_REG_TARGET_PID) = (unsigned int)sys0(172);
	*(volatile unsigned int *)(page + BREADCRUMB_REG_CONTROL) = BREADCRUMB_CONTROL_START;

	spin(256);
	req.tv_sec = 0;
	req.tv_nsec = 10000000;
	if (sys2(101, (long)&req, 0) != 0) {
		exit_code(82);
	}
	spin(256);
	exit_code(0);
}
