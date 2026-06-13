//go:build linux

package riscv

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

func guestAlloc(size uint64) (unsafe.Pointer, error) {
	if size > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("size overflows int: %d", size)
	}
	b, err := unix.Mmap(
		-1, 0, int(size),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE,
	)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty mmap")
	}
	return unsafe.Pointer(&b[0]), nil
}

func guestFree(base unsafe.Pointer, size uint64) error {
	return unix.Munmap(unsafe.Slice((*byte)(base), size))
}

func guestZeroRange(base unsafe.Pointer, size uint64) error {
	return unix.Madvise(unsafe.Slice((*byte)(base), size), unix.MADV_DONTNEED)
}

func guestGuard(base unsafe.Pointer, size uint64) error {
	return unix.Mprotect(unsafe.Slice((*byte)(base), size), unix.PROT_NONE)
}

func guestUnguard(base unsafe.Pointer, size uint64) error {
	return unix.Mprotect(unsafe.Slice((*byte)(base), size), unix.PROT_READ|unix.PROT_WRITE)
}
