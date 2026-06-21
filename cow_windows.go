//go:build windows

package riscv

import (
	"fmt"
	"unsafe"
)

// COWRemap is a Windows fallback for CowClone. Windows has section-backed
// copy-on-write views, but this path keeps clone correctness without adding
// another platform-specific mapping owner.
func COWRemap(size uint64, sourceAddr unsafe.Pointer) (unsafe.Pointer, error) {
	if size > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("COWRemap: size overflows int: %d", size)
	}
	dst, err := guestAlloc(size)
	if err != nil {
		return nil, err
	}
	copy(
		unsafe.Slice((*byte)(dst), int(size)),
		unsafe.Slice((*byte)(sourceAddr), int(size)),
	)
	return dst, nil
}

// COWUnmap releases a region returned by COWRemap.
func COWUnmap(addr unsafe.Pointer, size uint64) error {
	return guestFree(addr, size)
}
