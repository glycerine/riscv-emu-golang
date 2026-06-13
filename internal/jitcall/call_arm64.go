//go:build arm64

package jitcall

//go:noescape
func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64) Result

//go:noescape
func CallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64,
	decoderCacheBase uintptr, decoderCacheMask uint64,
	vaddrBegin uint64, segSize uint64) Result
