enum {
	CLONE_VM = 0x00000100,
	CLONE_FS = 0x00000200,
	CLONE_FILES = 0x00000400,
	CLONE_SIGHAND = 0x00000800,
	CLONE_THREAD = 0x00010000,
	CLONE_SYSVSEM = 0x00040000,
	CLONE_CHILD_CLEARTID = 0x00200000,
	CLONE_CHILD_SETTID = 0x01000000,
};

static char child_stack[65536] __attribute__((aligned(16)));
static volatile long clone_result;
static volatile int child_tid_slot;
static volatile int child_done;

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
		CLONE_THREAD | CLONE_SYSVSEM | CLONE_CHILD_SETTID | CLONE_CHILD_CLEARTID;
	long stack_top = (long)(child_stack + sizeof(child_stack));
	RAW_CLONE(clone_result, flags, stack_top, 0, 0, (long)&child_tid_slot);
	if (clone_result == 0) {
		if (sys0(178) <= 1) {
			child_done = 81;
			exit_code(81);
		}
		child_done = 1;
		exit_code(0);
	}
	long tid = clone_result;
	if (tid <= 1) {
		exit_code(82);
	}
	if (child_tid_slot != tid) {
		exit_code(83);
	}
	for (int i = 0; i < 8 && !child_done; i++) {
		sys0(124);
	}
	if (!child_done) {
		exit_code(84);
	}
	if (child_done != 1) {
		exit_code(child_done);
	}
	if (child_tid_slot != 0) {
		exit_code(85);
	}
	exit_code(0);
}
