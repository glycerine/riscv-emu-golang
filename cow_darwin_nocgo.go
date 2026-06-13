//go:build darwin && !cgo

package riscv

import (
	"fmt"
	"syscall"
	"unsafe"
)

// COWRemap is the no-cgo Darwin fallback for CowClone. Without cgo we
// cannot call mach_vm_remap, so preserve fork correctness with an eager
// private copy. This is slower than the Mach CoW path but keeps no-cgo
// darwin/arm64 builds functional.
func COWRemap(size uint64, sourceAddr unsafe.Pointer) (unsafe.Pointer, error) {
	if size > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("COWRemap: size overflows int: %d", size)
	}
	n := int(size)
	b, err := syscall.Mmap(-1, 0, n,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("mmap copy fallback: %w", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("mmap copy fallback: empty mapping")
	}
	src := unsafe.Slice((*byte)(sourceAddr), n)
	copy(b, src)
	return unsafe.Pointer(&b[0]), nil
}

// COWUnmap releases a region returned by COWRemap.
func COWUnmap(addr unsafe.Pointer, size uint64) error {
	if size > uint64(^uint(0)>>1) {
		return fmt.Errorf("COWUnmap: size overflows int: %d", size)
	}
	return syscall.Munmap(unsafe.Slice((*byte)(addr), int(size)))
}
