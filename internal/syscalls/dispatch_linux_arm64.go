//go:build linux && arm64

package syscalls

import "unsafe"

// CallDispatch is a Go-ABI convenience wrapper around the native ARM64
// dispatcher. Return value semantics match the JIT contract: 0=handled,
// 1=fallback to the Go ecall path.
//
//go:noescape
func CallDispatch(xptr unsafe.Pointer, memBase uintptr, memMask uint64) uint64

// DispatchAddr returns the absolute code address of the ARM64 dispatcher.
//
//go:noescape
func DispatchAddr() uintptr

var jitDispatchWriteCallback uintptr

// RegisterWriteCallback installs a function pointer as the SYS_write handler.
// Pass 0 to restore the direct kernel syscall path.
func RegisterWriteCallback(addr uintptr) {
	jitDispatchWriteCallback = addr
}

// NullWriteCallbackAddr returns the built-in no-op callback address.
//
//go:noescape
func NullWriteCallbackAddr() uintptr
