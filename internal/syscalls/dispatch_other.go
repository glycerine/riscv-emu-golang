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
	switch regs[17] {
	case 63:
		bufVA := uintptr(regs[11])
		count := int(regs[12])
		if count < 0 {
			return 1
		}
		hostBuf := unsafe.Slice((*byte)(unsafe.Pointer(memBase+(bufVA&uintptr(memMask)))), count)
		n, err := syscall.Read(int(regs[10]), hostBuf)
		regs[10] = syscallResult(n, err)
		return 0
	case 64:
		fd := int(regs[10])
		bufVA := uintptr(regs[11])
		count := int(regs[12])
		if count < 0 {
			return 1
		}
		hostBuf := unsafe.Slice((*byte)(unsafe.Pointer(memBase+(bufVA&uintptr(memMask)))), count)
		n, err := syscall.Write(fd, hostBuf)
		regs[10] = syscallResult(n, err)
		return 0
	case 57:
		err := syscall.Close(int(regs[10]))
		regs[10] = syscallResult(0, err)
		return 0
	case 62:
		off, err := syscall.Seek(int(regs[10]), int64(regs[11]), int(regs[12]))
		if err != nil {
			regs[10] = syscallErrno(err)
		} else {
			regs[10] = uint64(off)
		}
		return 0
	case 96:
		regs[10] = 1
		return 0
	case 172:
		regs[10] = uint64(syscall.Getpid())
		return 0
	case 178:
		regs[10] = 1
		return 0
	case 214:
		regs[10] = 0
		return 0
	default:
		return 1
	}
}

func syscallResult(n int, err error) uint64 {
	if err != nil {
		return syscallErrno(err)
	}
	return uint64(n)
}

func syscallErrno(err error) uint64 {
	if errno, ok := err.(syscall.Errno); ok {
		return uint64(-int64(errno))
	}
	return ^uint64(0)
}

// DispatchAddr returns zero on non-amd64 until a native dispatcher exists.
func DispatchAddr() uintptr { return 0 }

var jitDispatchWriteCallback uintptr

func RegisterWriteCallback(addr uintptr) {
	jitDispatchWriteCallback = addr
}

// NullWriteCallbackAddr is unavailable without a native dispatcher.
func NullWriteCallbackAddr() uintptr { return 0 }
