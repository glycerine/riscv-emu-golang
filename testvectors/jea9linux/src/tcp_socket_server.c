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

static void write_ready(void) {
	char msg[6] = {'r', 'e', 'a', 'd', 'y', '\n'};
	if (sys3(64, 1, (long)msg, 6) != 6) {
		exit_code(221);
	}
}

struct client_state {
	long fd;
	int kind;
	int stage;
};

static int state_index_for_data(unsigned long data) {
	if (data == 0x60) {
		return 0;
	}
	if (data == 0x61) {
		return 1;
	}
	return -1;
}

static void add_epoll_read(long epfd, long fd, unsigned long data, long code) {
	struct epoll_event ev;
	ev.events = 1;
	ev.data = data;
	if (sys4(21, epfd, 1, fd, (long)&ev) != 0) {
		exit_code(code);
	}
}

static void del_epoll(long epfd, long fd, long code) {
	if (sys4(21, epfd, 2, fd, 0) != 0) {
		exit_code(code);
	}
}

static void expect_write(long fd, char *buf, long n, long code) {
	if (sys3(64, fd, (long)buf, n) != n) {
		exit_code(code);
	}
}

static long read_ready(long fd, char *buf, long cap, long code) {
	for (long i = 0; i < cap; i++) {
		buf[i] = 0;
	}
	long n = sys3(63, fd, (long)buf, cap);
	if (n <= 0) {
		exit_code(code);
	}
	return n;
}

void start_c(unsigned long *sp) {
	int argc = (int)sp[0];
	char **argv = (char **)(sp + 1);
	if (argc < 2) {
		exit_code(220);
	}
	unsigned long port = parse_port(argv[1]);
	if (port == 0 || port > 65535) {
		exit_code(220);
	}
	int interleaving = 0;
	if (argc >= 3 && argv[2][0] == '1') {
		interleaving = 1;
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
		exit_code(222);
	}
	if (sys3(200, fd, (long)&addr, 16) != 0) {
		exit_code(223);
	}
	if (sys2(201, fd, 16) != 0) {
		exit_code(224);
	}

	write_ready();

	long epfd = sys1(20, 0);
	if (epfd < 3) {
		exit_code(225);
	}
	struct epoll_event out;
	add_epoll_read(epfd, fd, 0x51, 226);

	struct client_state states[2];
	states[0].fd = -1;
	states[0].kind = 0;
	states[0].stage = 0;
	states[1].fd = -1;
	states[1].kind = 0;
	states[1].stage = 0;
	for (int i = 0; i < 2; i++) {
		if (sys6(22, epfd, (long)&out, 1, -1, 0, 0) != 1) {
			exit_code(227);
		}
		if (out.events != 1 || out.data != 0x51) {
			exit_code(228);
		}
		long conn = sys4(242, fd, 0, 0, 0x800 | 0x80000);
		if (conn < 3) {
			exit_code(229);
		}
		states[i].fd = conn;
	}
	for (int i = 0; i < 2; i++) {
		add_epoll_read(epfd, states[i].fd, 0x60 + (unsigned long)i, 230);
	}

	char in[8];
	char pong[4] = {'p', 'o', 'n', 'g'};
	char goga[4] = {'g', 'o', 'g', 'a'};
	char c1reply0[8] = {'c', '1', 'r', 'e', 'p', 'l', 'y', '0'};
	char c1reply1[8] = {'c', '1', 'r', 'e', 'p', 'l', 'y', '1'};
	int done = 0;
	int c0_second_replied = 0;
	int deferred_c0 = -1;
	while (done < 2) {
		if (sys6(22, epfd, (long)&out, 1, -1, 0, 0) != 1) {
			exit_code(231);
		}
		if (out.events != 1) {
			exit_code(232);
		}
		int idx = state_index_for_data(out.data);
		if (idx < 0) {
			exit_code(233);
		}
		struct client_state *st = &states[idx];
		long n = read_ready(st->fd, in, 8, 234);
		if (st->stage == 0 && n == 4 && memeq(in, "ping", 4)) {
			st->kind = 1;
			st->stage = 1;
			expect_write(st->fd, pong, 4, 235);
		} else if (st->stage == 0 && n == 7 && memeq(in, "c1send0", 7)) {
			st->kind = 2;
			st->stage = 1;
			expect_write(st->fd, c1reply0, 8, 236);
		} else if (st->stage == 1 && st->kind == 1 && n == 4 && memeq(in, "heya", 4)) {
			if (interleaving == 1) {
				if (deferred_c0 >= 0) {
					exit_code(242);
				}
				st->stage = 3;
				deferred_c0 = idx;
				del_epoll(epfd, st->fd, 240);
			} else {
				st->stage = 2;
				done++;
				expect_write(st->fd, goga, 4, 237);
				c0_second_replied = 1;
				del_epoll(epfd, st->fd, 240);
			}
		} else if (st->stage == 1 && st->kind == 2 && n == 7 && memeq(in, "c1send1", 7)) {
			if (interleaving == 0 && !c0_second_replied) {
				exit_code(243);
			}
			if (interleaving == 1 && deferred_c0 < 0) {
				exit_code(244);
			}
			st->stage = 2;
			done++;
			expect_write(st->fd, c1reply1, 8, 238);
			del_epoll(epfd, st->fd, 241);
			if (interleaving == 1) {
				struct client_state *c0 = &states[deferred_c0];
				c0->stage = 2;
				done++;
				expect_write(c0->fd, goga, 4, 245);
				c0_second_replied = 1;
				deferred_c0 = -1;
			}
		} else {
			exit_code(239);
		}
	}
	exit_code(0);
}

__attribute__((naked)) void _start(void) {
	__asm__ volatile("mv a0, sp\n"
			 "call start_c\n");
}
