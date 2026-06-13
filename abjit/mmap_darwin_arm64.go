//go:build darwin && arm64

package abjit

import (
	"runtime"
	"syscall"
)

func mmapExec(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_JIT)
}

func munmapExec(b []byte) error {
	return syscall.Munmap(b)
}

func withExecWrite(fn func()) {
	runtime.LockOSThread()
	pthreadJITWriteProtect(false)
	defer func() {
		pthreadJITWriteProtect(true)
		runtime.UnlockOSThread()
	}()
	fn()
}

func pthreadJITWriteProtect(enabled bool)
func libcPthreadJITWriteProtectNPTrampoline()

//go:cgo_import_dynamic libc_pthread_jit_write_protect_np pthread_jit_write_protect_np "/usr/lib/libSystem.B.dylib"
