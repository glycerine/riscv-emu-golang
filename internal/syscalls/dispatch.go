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
