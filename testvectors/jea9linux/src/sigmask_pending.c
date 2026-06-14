#include "jea9linux_signal_common.h"

static volatile long seen;

static void handler(long sig, void *info, void *ucontext) {
	(void)info;
	(void)ucontext;
	seen = sig;
}

void _start(void) {
	unsigned long mask = 1UL << (SIGUSR1 - 1);
	if (install_signal(SIGUSR1, handler, 0) != 0) {
		exit_code(120);
	}
	if (sys4(SYS_RT_SIGPROCMASK, SIG_BLOCK, (long)&mask, 0, 8) != 0) {
		exit_code(121);
	}
	send_self_signal(SIGUSR1);
	if (seen != 0) {
		exit_code(122);
	}
	if (sys4(SYS_RT_SIGPROCMASK, SIG_UNBLOCK, (long)&mask, 0, 8) != 0) {
		exit_code(123);
	}
	if (seen != SIGUSR1) {
		exit_code(124);
	}
	exit_code(0);
}
