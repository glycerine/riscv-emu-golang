package abjit

import (
	"bytes"
	"reflect"
	"unsafe"
)

var gocallAddr uintptr
var retStubAddr uintptr

func init() {
	impl := callJITImplAddr()
	b := unsafe.Slice((*byte)(unsafe.Pointer(impl)), 0x80)

	callPattern := []byte{0x41, 0xFF, 0xD2} // CALL R10
	callOff := bytes.Index(b, callPattern)
	if callOff < 0 {
		panic("abjit: CALL R10 not found in callJIT")
	}
	gocallAddr = impl + uintptr(callOff)

	// retStub is the first RET (0xC3) after the CALL R10.
	retOff := bytes.IndexByte(b[callOff+3:], 0xC3)
	if retOff < 0 {
		panic("abjit: RET not found after gocall in callJIT")
	}
	retStubAddr = impl + uintptr(callOff+3+retOff)
}

func funcAddr(f any) uintptr {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func {
		panic("funcAddr: not a func")
	}
	return v.Pointer()
}
