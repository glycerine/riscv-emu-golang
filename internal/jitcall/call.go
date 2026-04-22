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

// CallAOT invokes a JIT-compiled block via direct function pointer.
// Implemented in call_amd64.s — no cgo overhead.
//
// In addition to the standard register and memory pointers, it
// publishes values into the sret buffer so the JIT can read them as
// [RBX+offset]:
//
//   [RBX+88..112]  decoder_cache state (JALR fast path)
//   [RBX+120]      dispatch PC (function re-entry target)
//
// When the JIT has no AOT segment installed, pass zeros for
// decoderCacheBase/Mask/vaddrBegin/segSize; the JALR bounds check
// immediately fails and falls through to the 2-way IC fallback.
// The pc parameter must always be set to the guest entry PC so the
// dispatch table in the prologue can route mid-function re-entry.
//
//go:noescape
func CallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	decoderCacheBase uintptr, decoderCacheMask uint64,
	vaddrBegin uint64, segSize uint64,
	pc uint64) Result
