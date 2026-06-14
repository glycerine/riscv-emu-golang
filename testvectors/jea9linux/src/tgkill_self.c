#include "jea9linux_signal_common.h"

static volatile long got_sig;
static volatile long got_code;
static volatile long got_pid;

static void handler(long sig, void *info, void *ucontext) {
	(void)ucontext;
	char *raw = (char *)info;
	got_sig = sig;
	got_code = *(int *)(raw + 8);
	got_pid = *(unsigned int *)(raw + 16);
}

void _start(void) {
	long pid = sys0(SYS_GETPID);
	if (install_signal(SIGURG, handler, 0) != 0) {
		exit_code(140);
	}
	send_self_signal(SIGURG);
	if (got_sig != SIGURG) {
		exit_code(141);
	}
	if (got_code != 0 || got_pid != pid) {
		exit_code(142);
	}
	exit_code(0);
}
