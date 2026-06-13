//go:build arm64 && darwin && cgo

package riscv

/*
#include <libkern/OSCacheControl.h>
*/
import "C"
import "unsafe"

func icacheFlush(start, end unsafe.Pointer) {
	C.sys_icache_invalidate(start, C.size_t(uintptr(end)-uintptr(start)))
}
