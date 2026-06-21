//go:build !linux && !darwin && !windows

package riscv

import (
	"fmt"
	"unsafe"
)

func guestAlloc(size uint64) (unsafe.Pointer, error) {
	return nil, fmt.Errorf("guest mmap is not implemented on this OS")
}

func guestFree(base unsafe.Pointer, size uint64) error {
	return fmt.Errorf("guest mmap is not implemented on this OS")
}

func guestZeroRange(base unsafe.Pointer, size uint64) error {
	return fmt.Errorf("guest mmap is not implemented on this OS")
}

func guestGuard(base unsafe.Pointer, size uint64) error {
	return fmt.Errorf("guest mmap is not implemented on this OS")
}

func guestUnguard(base unsafe.Pointer, size uint64) error {
	return fmt.Errorf("guest mmap is not implemented on this OS")
}
