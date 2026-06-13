//go:build arm64

package abjit

func callJIT(code, regFileBase uintptr)
func callJITImplAddr() uintptr

var gocallAddr uintptr
var retStubAddr uintptr
