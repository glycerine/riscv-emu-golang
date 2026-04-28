package riscv

import (
	"bytes"
	"reflect"
	"unsafe"
)

var gocallAddr uintptr

func init() {
	gocallAddr = getCallAddr()
}

func getCallAddr() uintptr {
	impl := callJITImplAddr()
	b := unsafe.Slice((*byte)(unsafe.Pointer(impl)), 0x80)
	pattern := []byte{0x41, 0xFF, 0xD2} // CALL R10
	offset := bytes.Index(b, pattern)
	if offset < 0 {
		panic("abjit: CALL R10 not found in callJIT")
	}
	return impl + uintptr(offset)
}

func funcAddr(f any) uintptr {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func {
		panic("funcAddr: not a func")
	}
	return v.Pointer()
}
