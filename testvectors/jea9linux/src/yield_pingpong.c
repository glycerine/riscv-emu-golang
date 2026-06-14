enum {
	CLONE_VM = 0x00000100,
	CLONE_FS = 0x00000200,
	CLONE_FILES = 0x00000400,
	CLONE_SIGHAND = 0x00000800,
	CLONE_THREAD = 0x00010000,
	CLONE_SYSVSEM = 0x00040000,
};

static char child_stack[65536] __attribute__((aligned(16)));
static volatile long clone_result;
static volatile int index_slot;
static volatile int seq[4];

#define RAW_CLONE(result, flags, stack, ptid, tls, ctid) do { \
	register long x10 __asm__("a0") = (flags); \
	register long x11 __asm__("a1") = (stack); \
	register long x12 __asm__("a2") = (ptid); \
	register long x13 __asm__("a3") = (tls); \
	register long x14 __asm__("a4") = (ctid); \
	register long x17 __asm__("a7") = 220; \
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x13), "r"(x14), "r"(x17) : "memory"); \
	(result) = x10; \
} while (0)

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

static void exit_code(long code) {
	sys1(93, code);
	for (;;) {
	}
}

void _start(void) {
	long flags = CLONE_VM | CLONE_FS | CLONE_FILES | CLONE_SIGHAND |
		CLONE_THREAD | CLONE_SYSVSEM;
	long stack_top = (long)(child_stack + sizeof(child_stack));
	RAW_CLONE(clone_result, flags, stack_top, 0, 0, 0);
	if (clone_result == 0) {
		seq[index_slot++] = 2;
		exit_code(0);
	}
	long tid = clone_result;
	if (tid <= 1) {
		exit_code(90);
	}
	seq[index_slot++] = 1;
	sys0(124);
	seq[index_slot++] = 3;
	if (index_slot != 3 || seq[0] != 1 || seq[1] != 2 || seq[2] != 3) {
		exit_code(91);
	}
	exit_code(0);
}
