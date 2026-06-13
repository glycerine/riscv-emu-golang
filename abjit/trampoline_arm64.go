//go:build arm64

package abjit

import (
	"bytes"
	"unsafe"
)

func callJIT(code, regFileBase uintptr)
func callJITImplAddr() uintptr

var gocallAddr uintptr
var retStubAddr uintptr

func init() {
	impl := callJITImplAddr()
	b := unsafe.Slice((*byte)(unsafe.Pointer(impl)), 0x100)

	// BLR R16, little-endian. The instruction immediately after this is
	// the restore/return path generated code jumps to when exiting.
	blrR16 := []byte{0x00, 0x02, 0x3f, 0xd6}
	callOff := bytes.Index(b, blrR16)
	if callOff < 0 {
		panic("abjit: BLR R16 not found in ARM64 callJIT")
	}
	retStubAddr = impl + uintptr(callOff+len(blrR16))
}
