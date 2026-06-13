//go:build !amd64 && !(linux && arm64)

// Package syscalls provides non-amd64 fallbacks for the JIT syscall fast path.
package syscalls

import (
	"syscall"
	"unsafe"
)

// CallDispatch is a test/debug convenience fallback. It implements the same
// guest-facing write fast path in Go, but DispatchAddr remains zero so JIT
// codegen will use the regular Go ecall path on unsupported native backends.
func CallDispatch(xptr unsafe.Pointer, memBase uintptr, memMask uint64) uint64 {
	regs := unsafe.Slice((*uint64)(xptr), 32)
	if regs[17] != 64 {
		return 1
	}
	fd := int(regs[10])
	bufVA := uintptr(regs[11])
	count := int(regs[12])
	if count < 0 {
		return 1
	}
	hostBuf := unsafe.Slice((*byte)(unsafe.Pointer(memBase+(bufVA&uintptr(memMask)))), count)
	n, err := syscall.Write(fd, hostBuf)
	if err != nil {
		regs[10] = uint64(-int64(err.(syscall.Errno)))
	} else {
		regs[10] = uint64(n)
	}
	return 0
}

// DispatchAddr returns zero on non-amd64 until a native dispatcher exists.
func DispatchAddr() uintptr { return 0 }

var jitDispatchWriteCallback uintptr

func RegisterWriteCallback(addr uintptr) {
	jitDispatchWriteCallback = addr
}

// NullWriteCallbackAddr is unavailable without a native dispatcher.
func NullWriteCallbackAddr() uintptr { return 0 }
