//go:build darwin && arm64 && !cgo

package riscv

func pthreadJITWriteProtect(enabled bool)
func libcPthreadJITWriteProtectNPTrampoline()

//go:cgo_import_dynamic libc_pthread_jit_write_protect_np pthread_jit_write_protect_np "/usr/lib/libSystem.B.dylib"
