// SPDX-License-Identifier: MIT
//
// breadcrumbexec arms the riscv-emu breadcrumb MMIO device and then execs the
// requested target. It is deliberately a tiny C program so no Go runtime
// threads or signals exist before the target exec.

#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/types.h>
#include <unistd.h>

extern char **environ;

enum {
	BREADCRUMB_BASE = 0x10002000UL,
	BREADCRUMB_SIZE = 0x1000,
	BREADCRUMB_MAGIC = 0x31524342U,

	BREADCRUMB_REG_MAGIC = 0x00,
	BREADCRUMB_REG_CONTROL = 0x10,
	BREADCRUMB_REG_TRIP_PID = 0x44,
	BREADCRUMB_REG_TARGET_PID = 0x68,

	BREADCRUMB_CONTROL_ARM_NEXT_AS_RESET = 7,
};

static void usage(void)
{
	fprintf(stderr, "usage: breadcrumbexec [--] program [args...]\n");
}

static uint32_t mmio_read32(volatile uint8_t *base, uintptr_t off)
{
	return *(volatile uint32_t *)(base + off);
}

static void mmio_write32(volatile uint8_t *base, uintptr_t off, uint32_t value)
{
	*(volatile uint32_t *)(base + off) = value;
}

int main(int argc, char **argv)
{
	volatile uint8_t *page;
	uint32_t pid;
	uint32_t magic;
	int fd;

	if (argc > 1 && strcmp(argv[1], "--") == 0) {
		argc--;
		argv++;
	}
	if (argc < 2) {
		usage();
		return 2;
	}

	fd = open("/dev/mem", O_RDWR | O_SYNC | O_CLOEXEC);
	if (fd < 0) {
		fprintf(stderr, "breadcrumbexec: open /dev/mem: %s\n", strerror(errno));
		return 1;
	}

	page = mmap(NULL, BREADCRUMB_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd,
		    (off_t)BREADCRUMB_BASE);
	if (page == MAP_FAILED) {
		fprintf(stderr, "breadcrumbexec: mmap breadcrumb MMIO: %s\n",
			strerror(errno));
		close(fd);
		return 1;
	}

	magic = mmio_read32(page, BREADCRUMB_REG_MAGIC);
	if (magic != BREADCRUMB_MAGIC) {
		fprintf(stderr,
			"breadcrumbexec: bad breadcrumb MMIO magic: got 0x%08x want 0x%08x\n",
			magic, BREADCRUMB_MAGIC);
		munmap((void *)page, BREADCRUMB_SIZE);
		close(fd);
		return 1;
	}

	pid = (uint32_t)getpid();
	mmio_write32(page, BREADCRUMB_REG_TARGET_PID, pid);
	mmio_write32(page, BREADCRUMB_REG_TRIP_PID, pid);
	mmio_write32(page, BREADCRUMB_REG_CONTROL,
		     BREADCRUMB_CONTROL_ARM_NEXT_AS_RESET);

	munmap((void *)page, BREADCRUMB_SIZE);
	close(fd);

	execve(argv[1], &argv[1], environ);
	fprintf(stderr, "breadcrumbexec: execve %s: %s\n", argv[1], strerror(errno));
	return errno == ENOENT ? 127 : 126;
}
