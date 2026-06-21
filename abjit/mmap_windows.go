//go:build windows
// +build windows

package abjit

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mmapExec(size int) ([]byte, error) {
	if size <= 0 {
		return nil, fmt.Errorf("empty mmap")
	}
	ptr, err := windows.VirtualAlloc(
		0,
		uintptr(size),
		windows.MEM_COMMIT|windows.MEM_RESERVE,
		windows.PAGE_EXECUTE_READWRITE,
	)
	if err != nil {
		return nil, err
	}
	if ptr == 0 {
		return nil, fmt.Errorf("VirtualAlloc returned nil")
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size), nil
}

func munmapExec(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.VirtualFree(uintptr(unsafe.Pointer(&b[0])), 0, windows.MEM_RELEASE)
}

func withExecWrite(fn func()) {
	fn()
}
