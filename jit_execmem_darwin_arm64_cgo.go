//go:build darwin && arm64 && cgo

package riscv

/*
#include <pthread.h>
*/
import "C"

// Keep the libc call in cgo: Go assembly cannot share a package with cgo.
// withExecWrite pins the goroutine because this setting is per OS thread.
func pthreadJITWriteProtect(enabled bool) {
	var value C.int
	if enabled {
		value = 1
	}
	C.pthread_jit_write_protect_np(value)
}
