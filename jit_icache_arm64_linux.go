//go:build arm64 && linux

package riscv

/*
void icache_clear(void *start, void *end) {
    __builtin___clear_cache((char*)start, (char*)end);
}
*/
import "C"
import "unsafe"

func icacheFlush(start, end unsafe.Pointer) {
	C.icache_clear(start, end)
}
