#include "jea9linux_signal_common.h"

static char alt_stack[16384] __attribute__((aligned(16)));
static volatile long on_alt_stack;

static void handler(long sig, void *info, void *ucontext) {
	(void)sig;
	(void)info;
	(void)ucontext;
	unsigned long sp;
	__asm__ volatile("mv %0, sp" : "=r"(sp));
	unsigned long begin = (unsigned long)alt_stack;
	unsigned long end = begin + sizeof(alt_stack);
	if (sp >= begin && sp < end) {
		on_alt_stack = 1;
	}
}

void _start(void) {
	struct jea9_stack stack;
	stack.sp = (unsigned long)alt_stack;
	stack.flags = 0;
	stack.size = sizeof(alt_stack);
	if (sys2(SYS_SIGALTSTACK, (long)&stack, 0) != 0) {
		exit_code(130);
	}
	if (install_signal(SIGUSR1, handler, SA_ONSTACK) != 0) {
		exit_code(131);
	}
	send_self_signal(SIGUSR1);
	if (!on_alt_stack) {
		exit_code(132);
	}
	exit_code(0);
}
