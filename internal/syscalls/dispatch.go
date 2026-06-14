//go:build amd64

// Package syscalls implements the direct-SYSCALL fast path for guest
// ECALLs. The hot-path dispatcher is written in Go assembly per OS
// (linux/darwin) using the System V AMD64 ABI so the JIT can call it
// with a single indirect-CALL — no cgo boundary, no Go scheduler
// interaction. This mirrors libriscv's api.system_call shape.
//
// SysV ABI contract (see dispatch_*.s):
//
//	RDI = &cpu.x[0]   (guest GPR array; x[17]=a7, x[10..15]=a0..a5)
//	RSI = memBase     (host base of guest memory)
//	RDX = memMask     (guest memory size - 1)
//
//	RAX on return:
//	  0 → syscall handled natively; JIT continues at pc+4
//	  1 → fallback; JIT emits jitEcall and existing Go OS layer handles it
//
// The Linux dispatchers handle a small native subset: read, write, close,
// lseek, getpid, gettid, plus lightweight brk and set_tid_address stubs used
// by the repo's benchmark Linux personality. Exit still falls back so the
// emulator can stop through the normal OS/note path.
//
// The JIT obtains the code address with DispatchAddr() and emits a
// MOVABS+CALL sequence at each ECALL site.
package syscalls

import "unsafe"

// CallDispatch is a Go-ABI convenience wrapper around the SysV
// dispatcher, used by tests and by code that wants to invoke the
// fast path from pure Go. The JIT hot path bypasses this and calls
// the dispatcher directly via DispatchAddr().
//
// Return value semantics match the SysV contract above (0=handled,
// 1=fallback).
//
// Implemented in dispatch_<goos>_amd64.s.
//
//go:noescape
func CallDispatch(xptr unsafe.Pointer, memBase uintptr, memMask uint64) uint64

// DispatchAddr returns the absolute code address of the SysV dispatcher
// entry. The JIT should emit this as a 64-bit immediate and CALL it
// with the documented ABI.
//
// Implemented in dispatch_<goos>_amd64.s.
//
//go:noescape
func DispatchAddr() uintptr

// jitDispatchWriteCallback is a C-ABI function pointer the dispatcher
// consults on the SYS_write fast path. When zero (the default), the
// dispatcher issues a direct kernel SYSCALL. When non-zero, the
// dispatcher CALLs the registered function instead — useful for
// apples-to-apples benchmarking against libriscv's no-kernel
// `null_stdout` callback.
//
// Callback ABI (System V AMD64):
//
//	int64 callback(uintptr fd, uintptr hostBuf, uintptr count)
//
// Return value is stored into guest x[10] as the write's result.
//
// Not safe for concurrent modification; set once before starting
// the JIT and clear afterwards.
var jitDispatchWriteCallback uintptr

// RegisterWriteCallback installs a C-ABI function pointer as the
// dispatcher's SYS_write handler. Pass 0 to restore the direct-SYSCALL
// fast path (the default). See NullWriteCallbackAddr for the builtin
// no-op callback used in benchmarks.
func RegisterWriteCallback(addr uintptr) {
	jitDispatchWriteCallback = addr
}

// NullWriteCallbackAddr returns the address of a built-in SysV-ABI
// callback that does no work and returns `count` as "bytes written".
// Mirrors libriscv's null_stdout for dispatch-only cost measurement.
//
// Implemented in dispatch_<goos>_amd64.s.
//
//go:noescape
func NullWriteCallbackAddr() uintptr
