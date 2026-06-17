struct sockaddr_in {
	unsigned short family;
	unsigned short port;
	unsigned int addr;
	unsigned char zero[8];
};

struct epoll_event {
	unsigned int events;
	unsigned int pad;
	unsigned long data;
};

struct timespec {
	long sec;
	long nsec;
};

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

static unsigned short htons(unsigned long v) {
	return (unsigned short)(((v & 255) << 8) | ((v >> 8) & 255));
}

static unsigned long parse_port(char *s) {
	unsigned long out = 0;
	while (*s >= '0' && *s <= '9') {
		out = out * 10 + (unsigned long)(*s - '0');
		s++;
	}
	return out;
}

static int memeq(const char *a, const char *b, long n) {
	for (long i = 0; i < n; i++) {
		if (a[i] != b[i]) {
			return 0;
		}
	}
	return 1;
}

static void short_sleep(long code, unsigned long millis) {
	struct timespec ts;
	ts.sec = (long)(millis / 1000);
	ts.nsec = (long)((millis % 1000) * 1000000);
	if (sys2(101, (long)&ts, 0) != 0) {
		exit_code(code);
	}
}

static void roundtrip(long fd, long epfd, char *send, long send_len, char *want, long want_len, long code) {
	if (sys3(64, fd, (long)send, send_len) != send_len) {
		exit_code(code);
	}
	char in[8];
	for (long i = 0; i < 8; i++) {
		in[i] = 0;
	}
	long got = 0;
	while (got < want_len) {
		struct epoll_event out;
		if (sys6(22, epfd, (long)&out, 1, -1, 0, 0) != 1) {
			exit_code(code + 1);
		}
		if (out.events != 1 || out.data != 0x61) {
			exit_code(code + 2);
		}
		long n = sys3(63, fd, (long)(in + got), want_len - got);
		if (n == -11) {
			continue;
		}
		if (n <= 0) {
			exit_code(code + 3);
		}
		got += n;
	}
	if (!memeq(in, want, want_len)) {
		exit_code(code + 4);
	}
}

void start_c(unsigned long *sp) {
	int argc = (int)sp[0];
	char **argv = (char **)(sp + 1);
	if (argc < 2) {
		exit_code(210);
	}
	unsigned long port = parse_port(argv[1]);
	if (port == 0 || port > 65535) {
		exit_code(210);
	}

	struct sockaddr_in addr;
	addr.family = 2;
	addr.port = htons(port);
	addr.addr = 0x0100007f;
	for (int i = 0; i < 8; i++) {
		addr.zero[i] = 0;
	}

	long fd = sys3(198, 2, 1 | 0x800 | 0x80000, 0);
	if (fd < 3) {
		exit_code(211);
	}
	if (sys3(203, fd, (long)&addr, 16) != 0) {
		exit_code(212);
	}

	long epfd = sys1(20, 0);
	if (epfd < 3) {
		exit_code(213);
	}
	struct epoll_event ev;
	ev.events = 1;
	ev.data = 0x61;
	if (sys4(21, epfd, 1, fd, (long)&ev) != 0) {
		exit_code(214);
	}

	int mode = 0;
	if (argc >= 3 && argv[2][0] == '1') {
		mode = 1;
	}
	unsigned long sleep_millis = 50;
	if (argc >= 4) {
		sleep_millis = parse_port(argv[3]);
	}
	if (mode == 0) {
		char ping[4] = {'p', 'i', 'n', 'g'};
		char pong[4] = {'p', 'o', 'n', 'g'};
		char heya[4] = {'h', 'e', 'y', 'a'};
		char goga[4] = {'g', 'o', 'g', 'a'};
		roundtrip(fd, epfd, ping, 4, pong, 4, 220);
		short_sleep(225, sleep_millis);
		roundtrip(fd, epfd, heya, 4, goga, 4, 226);
	} else {
		char c1send0[7] = {'c', '1', 's', 'e', 'n', 'd', '0'};
		char c1reply0[8] = {'c', '1', 'r', 'e', 'p', 'l', 'y', '0'};
		char c1send1[7] = {'c', '1', 's', 'e', 'n', 'd', '1'};
		char c1reply1[8] = {'c', '1', 'r', 'e', 'p', 'l', 'y', '1'};
		roundtrip(fd, epfd, c1send0, 7, c1reply0, 8, 230);
		short_sleep(235, sleep_millis);
		roundtrip(fd, epfd, c1send1, 7, c1reply1, 8, 236);
	}
	exit_code(0);
}

__attribute__((naked)) void _start(void) {
	__asm__ volatile("mv a0, sp\n"
			 "call start_c\n");
}
