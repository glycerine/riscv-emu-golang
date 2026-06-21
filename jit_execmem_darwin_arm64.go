//go:build darwin && arm64

package riscv

import (
	"runtime"
	"syscall"
)

// allocExec allocates executable memory using Apple's MAP_JIT contract.
func allocExec(size int) ([]byte, error) {
	pageSize := syscall.Getpagesize()
	mapSize := ((size + pageSize - 1) / pageSize) * pageSize
	mem, err := syscall.Mmap(
		-1, 0, mapSize,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE|syscall.MAP_JIT,
	)
	if err != nil {
		return nil, err
	}
	return mem, nil
}

// allocRWAnon allocates anonymous memory with read/write permissions.
func allocRWAnon(size int) ([]byte, error) {
	pageSize := syscall.Getpagesize()
	mapSize := ((size + pageSize - 1) / pageSize) * pageSize
	mem, err := syscall.Mmap(
		-1, 0, mapSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE,
	)
	if err != nil {
		return nil, err
	}
	return mem, nil
}

func freeMapped(b []byte) error {
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
