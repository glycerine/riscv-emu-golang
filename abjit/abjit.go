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
type State struct {
	X       [32]uint64
	F       [32]uint64
	FCSR    uint32
	_       uint32
	MemBase uintptr
	MemMask uint64
}

//go:noinline
func NewState() *State {
	return new(State)
}

func (s *State) RegFileBase() uintptr {
	return uintptr(unsafe.Pointer(s))
}

func Run(cb *CodeBuilder, s *State) {
	callJIT(cb.Addr(), s.RegFileBase())
}
