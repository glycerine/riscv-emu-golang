enum {
	CLONE_VM = 0x00000100,
	CLONE_FS = 0x00000200,
	CLONE_FILES = 0x00000400,
	CLONE_SIGHAND = 0x00000800,
	CLONE_THREAD = 0x00010000,
	CLONE_SYSVSEM = 0x00040000,
	FUTEX_WAIT = 0,
	FUTEX_WAKE = 1,
	FUTEX_PRIVATE_FLAG = 128,
};

static char child_stack[65536] __attribute__((aligned(16)));
static volatile long clone_result;
static volatile int futex_word = 1;
static volatile int child_woke_parent;

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

static long sys1(long n, long a0) {
	register long x10 __asm__("a0") = a0;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x17) : "memory");
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
		futex_word = 0;
		long woke = sys4(422, (long)&futex_word, FUTEX_WAKE | FUTEX_PRIVATE_FLAG, 1, 0);
		if (woke != 1) {
			exit_code(100);
		}
		child_woke_parent = 1;
		exit_code(0);
	}
	long tid = clone_result;
	if (tid <= 1) {
		exit_code(101);
	}
	long rc = sys4(98, (long)&futex_word, FUTEX_WAIT, 1, 0);
	if (rc != 0) {
		exit_code(102);
	}
	if (futex_word != 0 || !child_woke_parent) {
		exit_code(103);
	}
	exit_code(0);
}
