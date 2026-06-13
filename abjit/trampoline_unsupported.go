//go:build !amd64

package abjit

func callJIT(code, regFileBase uintptr) {
	panic("abjit: native trampoline is not implemented on this host architecture")
}

func callJITImplAddr() uintptr { return 0 }

var gocallAddr uintptr
var retStubAddr uintptr
