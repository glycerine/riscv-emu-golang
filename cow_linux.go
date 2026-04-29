//go:build linux

package riscv

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// COWRemap creates a copy-on-write mapping of the memory at sourceAddr
// with the given size. The returned pointer refers to a MAP_PRIVATE
// mapping backed by a memfd, so writes to it trigger CoW and do not
// affect the source.
//
// Release with COWUnmap when done.
func COWRemap(size uint64, sourceAddr unsafe.Pointer) (unsafe.Pointer, error) {
	// 1. Create anonymous file
	fd, err := unix.MemfdCreate("cow", 0)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	defer unix.Close(fd)

	// 2. Size it
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		return nil, fmt.Errorf("ftruncate: %w", err)
	}

	// 3. Map shared to populate from source
	shared, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap shared: %w", err)
	}

	// Copy source memory into the memfd
	src := unsafe.Slice((*byte)(sourceAddr), size)
	copy(shared, src)

	if err := unix.Munmap(shared); err != nil {
		return nil, fmt.Errorf("munmap shared: %w", err)
	}

	// 4. Re-map as MAP_PRIVATE (CoW). The memfd keeps a kernel
	// refcount; closing fd (deferred) will not unmap.
	cow, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap private: %w", err)
	}

	return unsafe.Pointer(&cow[0]), nil
}

// COWUnmap releases a region returned by COWRemap.
func COWUnmap(addr unsafe.Pointer, size uint64) error {
	b := unsafe.Slice((*byte)(addr), size)
	return unix.Munmap(b)
}
