//go:build amd64

package jitcall

// Call invokes a JIT-compiled block via direct function pointer.
// Implemented in call_amd64.s — no cgo overhead.
//
//go:noescape
func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64, budget uint64) Result

// CallResv is Call plus RV8 LR/SC reservation-state copy-in/copy-out.
//
//go:noescape
func CallResv(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	resvAddr *uint64, resvValid *uint64, budget uint64) Result

// CallAOT is the AOT-aware variant of Call. In addition to the
// standard register and memory pointers, it publishes four values
// into the sret buffer at offsets 88/96/104/112 so JIT-emitted JALR
// sequences can perform a mask-bounded decoder_cache lookup without
// going through Go.
//
//go:noescape
func CallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	decoderCacheBase uintptr, decoderCacheMask uint64,
	vaddrBegin uint64, segSize uint64, budget uint64) Result

// CallAOTResv is CallAOT plus RV8 LR/SC reservation-state copy-in/copy-out.
//
//go:noescape
func CallAOTResv(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	decoderCacheBase uintptr, decoderCacheMask uint64,
	vaddrBegin uint64, segSize uint64,
	resvAddr *uint64, resvValid *uint64, budget uint64) Result
