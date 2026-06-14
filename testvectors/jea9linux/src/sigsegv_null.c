#include "jea9linux_signal_common.h"

static void handler(long sig, void *info, void *ucontext) {
	(void)ucontext;
	char *raw = (char *)info;
	unsigned long addr = *(unsigned long *)(raw + 24);
	if (sig == SIGSEGV && addr == 0) {
		exit_code(0);
	}
	exit_code(151);
}

void _start(void) {
	if (install_signal(SIGSEGV, handler, 0) != 0) {
		exit_code(150);
	}
	unsigned long value;
	__asm__ volatile("ld %0, 0(zero)" : "=r"(value) :: "memory");
	exit_code(152);
}
