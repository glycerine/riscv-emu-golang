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

// CallAOT is the AOT-aware variant of Call. In addition to the
// standard register and memory pointers, it publishes four values
// into the sret buffer at offsets 88/96/104/112 so JIT-emitted JALR
// sequences can perform a mask-bounded decoder_cache lookup without
// going through Go:
//
//   [RBX+88]  = decoder_cache base  (host pointer to segment's
//                                    DecoderData[] mmap)
//   [RBX+96]  = decoder_cache mask  (power-of-two size - 1)
//   [RBX+104] = vaddrBegin          (current segment's guest-VA start)
//   [RBX+112] = segSize             (current segment's guest-VA size)
//
// When the JIT has no AOT segment installed, callers may either use
// the plain Call (which doesn't write those slots) or use CallAOT
// passing zeros for all four; in that case the JALR bounds check
// immediately fails and the JALR dispatch falls through to the
// existing 2-way IC fallback.
//
//go:noescape
func CallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	decoderCacheBase uintptr, decoderCacheMask uint64,
	vaddrBegin uint64, segSize uint64) Result
