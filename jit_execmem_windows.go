//go:build windows

package riscv

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func pageAlignWindows(size int) (int, error) {
	if size <= 0 {
		return 0, fmt.Errorf("empty mmap")
	}
	pageSize := windows.Getpagesize()
	if size > int(^uint(0)>>1)-(pageSize-1) {
		return 0, fmt.Errorf("mmap size overflows int: %d", size)
	}
	return ((size + pageSize - 1) / pageSize) * pageSize, nil
}

// allocExec allocates executable memory using Windows virtual memory.
func allocExec(size int) ([]byte, error) {
	mapSize, err := pageAlignWindows(size)
	if err != nil {
		return nil, err
	}
	return virtualAllocSlice(mapSize, windows.PAGE_EXECUTE_READWRITE)
}

// allocRWAnon allocates anonymous memory with read/write permissions.
func allocRWAnon(size int) ([]byte, error) {
	mapSize, err := pageAlignWindows(size)
	if err != nil {
		return nil, err
	}
	return virtualAllocSlice(mapSize, windows.PAGE_READWRITE)
}

func virtualAllocSlice(size int, protect uint32) ([]byte, error) {
	ptr, err := windows.VirtualAlloc(
		0,
		uintptr(size),
		windows.MEM_COMMIT|windows.MEM_RESERVE,
		protect,
	)
	if err != nil {
		return nil, err
	}
	if ptr == 0 {
		return nil, fmt.Errorf("VirtualAlloc returned nil")
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size), nil
}

func freeMapped(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.VirtualFree(uintptr(unsafe.Pointer(&b[0])), 0, windows.MEM_RELEASE)
}

func withExecWrite(fn func()) {
	fn()
}
