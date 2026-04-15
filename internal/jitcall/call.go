// Package jitcall provides a zero-cgo-overhead trampoline for calling
// JIT-compiled native code blocks from Go.
package jitcall

// Result is the return value from a JIT-compiled block.
// All fields are uint64 for simple assembly access.
// The C struct uses {uint64_t pc, uint64_t ic, uint64_t status, uint64_t fault_addr}.
type Result struct {
	PC        uint64 // next PC to execute
	IC        uint64 // number of instructions executed in this block
	Status    uint64 // 0=ok, 1=ecall, 2=ebreak, 3=load_fault, 4=store_fault, 5=illegal
	FaultAddr uint64 // guest address that faulted (when Status >= 3)
}

// Call invokes a JIT-compiled block via direct function pointer.
// Implemented in call_amd64.s — no cgo overhead.
//
//go:noescape
func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64) Result
