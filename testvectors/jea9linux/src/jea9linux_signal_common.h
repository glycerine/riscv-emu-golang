#ifndef JEA9LINUX_SIGNAL_COMMON_H
#define JEA9LINUX_SIGNAL_COMMON_H

enum {
	SYS_EXIT = 93,
	SYS_TGKILL = 131,
	SYS_SIGALTSTACK = 132,
	SYS_RT_SIGACTION = 134,
	SYS_RT_SIGPROCMASK = 135,
	SYS_RT_SIGRETURN = 139,
	SYS_GETPID = 172,
	SYS_GETTID = 178,

	SIG_BLOCK = 0,
	SIG_UNBLOCK = 1,

	SIGUSR1 = 10,
	SIGSEGV = 11,
	SIGURG = 23,

	SA_ONSTACK = 0x08000000,
};

struct jea9_sigaction {
	unsigned long handler;
	unsigned long flags;
	unsigned long restorer;
	unsigned long mask;
};

struct jea9_stack {
	unsigned long sp;
	unsigned long flags;
	unsigned long size;
};

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

static long sys3(long n, long a0, long a1, long a2) {
	register long x10 __asm__("a0") = a0;
	register long x11 __asm__("a1") = a1;
	register long x12 __asm__("a2") = a2;
	register long x17 __asm__("a7") = n;
	__asm__ volatile("ecall" : "+r"(x10) : "r"(x11), "r"(x12), "r"(x17) : "memory");
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
	sys1(SYS_EXIT, code);
	for (;;) {
	}
}

static void signal_restorer(void) {
	sys0(SYS_RT_SIGRETURN);
	for (;;) {
	}
}

static long install_signal(long sig, void (*handler)(long, void *, void *), unsigned long flags) {
	struct jea9_sigaction act;
	act.handler = (unsigned long)handler;
	act.flags = flags;
	act.restorer = (unsigned long)signal_restorer;
	act.mask = 0;
	return sys4(SYS_RT_SIGACTION, sig, (long)&act, 0, 8);
}

static long send_self_signal(long sig) {
	long pid = sys0(SYS_GETPID);
	long tid = sys0(SYS_GETTID);
	return sys3(SYS_TGKILL, pid, tid, sig);
}

#endif
