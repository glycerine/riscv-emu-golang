#include "jea9linux_signal_common.h"

static volatile long seen;

static void handler(long sig, void *info, void *ucontext) {
	(void)info;
	(void)ucontext;
	seen = sig;
}

void _start(void) {
	if (install_signal(SIGUSR1, handler, 0) != 0) {
		exit_code(110);
	}
	send_self_signal(SIGUSR1);
	if (seen != SIGUSR1) {
		exit_code(111);
	}
	exit_code(0);
}
