package riscv

// jit_call.go — Go function signatures for the assembly trampoline.

// JITResult is the return value from a JIT-compiled block.
// Layout must match the C struct exactly (verified by jit_test.go).
type JITResult struct {
	PC        uint64 // next PC to execute
	IC        uint64 // number of instructions executed in this block
	Status    int32  // 0=ok, 1=ecall, 2=ebreak, 3=load_fault, 4=store_fault, 5=illegal
	_         int32  // padding for alignment
	FaultAddr uint64 // guest address that faulted (when Status >= 3)
}

// JIT status codes returned by compiled blocks.
const (
	jitOK         = 0
	jitEcall      = 1
	jitEbreak     = 2
	jitLoadFault  = 3
	jitStoreFault = 4
	jitIllegal    = 5
)

// callJIT calls a JIT-compiled block via direct function pointer.
// Implemented in jit_call_amd64.s — no cgo overhead on the hot path.
//
// The native function follows the System V AMD64 ABI with struct return:
//   RDI = hidden pointer to JITResult (allocated on our stack)
//   RSI = x  (*[32]uint64, integer register file)
//   RDX = f  (*[32]uint64, float register file)
//   RCX = fcsr (*uint32)
//   R8  = memBase (uintptr)
//   R9  = memMask (uint64)
//
//go:noescape
func callJIT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64) JITResult
