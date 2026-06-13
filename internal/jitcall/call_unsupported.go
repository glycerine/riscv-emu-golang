//go:build !amd64

package jitcall

func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64) Result {
	panic("jitcall: native function-pointer trampoline is not implemented on this host architecture")
}

func CallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	decoderCacheBase uintptr, decoderCacheMask uint64,
	vaddrBegin uint64, segSize uint64) Result {
	panic("jitcall: native AOT function-pointer trampoline is not implemented on this host architecture")
}
