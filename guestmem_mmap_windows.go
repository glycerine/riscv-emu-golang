//go:build windows

package riscv

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func guestAlloc(size uint64) (unsafe.Pointer, error) {
	if size == 0 {
		return nil, fmt.Errorf("empty mmap")
	}
	if size > uint64(^uintptr(0)) {
		return nil, fmt.Errorf("size overflows uintptr: %d", size)
	}
	ptr, err := windows.VirtualAlloc(
		0,
		uintptr(size),
		windows.MEM_COMMIT|windows.MEM_RESERVE,
		windows.PAGE_READWRITE,
	)
	if err != nil {
		return nil, err
	}
	if ptr == 0 {
		return nil, fmt.Errorf("VirtualAlloc returned nil")
	}
	return unsafe.Pointer(ptr), nil
}

func guestFree(base unsafe.Pointer, size uint64) error {
	if base == nil {
		return nil
	}
	return windows.VirtualFree(uintptr(base), 0, windows.MEM_RELEASE)
}

func guestZeroRange(base unsafe.Pointer, size uint64) error {
	if size > uint64(^uint(0)>>1) {
		return fmt.Errorf("size overflows int: %d", size)
	}
	clear(unsafe.Slice((*byte)(base), int(size)))
	return nil
}

func guestGuard(base unsafe.Pointer, size uint64) error {
	return guestProtect(base, size, windows.PAGE_NOACCESS)
}

func guestUnguard(base unsafe.Pointer, size uint64) error {
	return guestProtect(base, size, windows.PAGE_READWRITE)
}

func guestProtect(base unsafe.Pointer, size uint64, protect uint32) error {
	if size > uint64(^uintptr(0)) {
		return fmt.Errorf("size overflows uintptr: %d", size)
	}
	var oldProtect uint32
	return windows.VirtualProtect(uintptr(base), uintptr(size), protect, &oldProtect)
}
