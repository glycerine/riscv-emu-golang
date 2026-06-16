package abjit

import "unsafe"

// State mirrors the shadow register file layout used by the JIT.
// Must be heap-allocated (callJIT's 65KB frame triggers morestack;
// stack-allocated State would be invalidated by the stack copy).
//
// Layout matches guestmem.go's RegFileBase() page:
//
//	Offset 0:   x[0..31]  — 32 × 8 = 256 bytes
//	Offset 256: f[0..31]  — 32 × 8 = 256 bytes
//	Offset 512: fcsr      — 4 bytes
//	Offset 516: (padding) — 4 bytes
//	Offset 520: memBase   — 8 bytes
//	Offset 528: memMask   — 8 bytes
//	Offset 608: resvAddr  — 8 bytes
//	Offset 616: resvValid — 8 bytes (0 or 1)
type State struct {
	X          [32]uint64
	F          [32]uint64
	FCSR       uint32
	_          uint32
	MemBase    uintptr
	MemMask    uint64
	PC         uint64
	Status     uint64
	FaultAddr  uint64
	DCBase     uintptr
	DCMask     uint64
	VAddrBegin uint64
	SegSize    uint64
	Cycles     uint64 // Reserved for future per-block instruction count (not currently populated)
	IC         uint64 // remaining guest-instruction budget at native entry/exit
	ResvAddr   uint64 // LR/SC reservation address for single-hart atomics
	ResvValid  uint64 // LR/SC reservation valid flag (0 or 1 for native code)
}

//go:noinline
func NewState() *State {
	return new(State)
}

func (s *State) RegFileBase() uintptr {
	return uintptr(unsafe.Pointer(s))
}

func Run(cb *CodeBuilder, s *State) {
	// call the assembly trampoline
	callJIT(cb.Addr(), s.RegFileBase())
}

// CallJIT calls JIT-compiled native code with the given register file base.
func CallJIT(code, regFileBase uintptr) {
	// call the assembly trampoline
	callJIT(code, regFileBase)
}

// GocallAddr returns the address of the CALL R10 instruction in the
// trampoline, used by the IR lowerer to emit gocall sequences.
func GocallAddr() uintptr {
	return gocallAddr
}

// RetStubAddr returns the address of the RET instruction in the
// trampoline. JIT exit thunks JMP here instead of emitting RET
// inline — keeps all RET instructions in Go-known code.
func RetStubAddr() uintptr {
	return retStubAddr
}
