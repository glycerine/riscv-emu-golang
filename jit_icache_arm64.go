//go:build arm64

package riscv

import "unsafe"

// flushIcache invalidates the instruction cache for [addr, addr+size).
// Required on ARM64 because the architecture has separate instruction
// and data caches (Harvard-style). After writing JIT code or patching
// chain exits, stale icache lines must be flushed or the CPU may
// execute old instructions.
//
// On macOS (Apple Silicon) this calls sys_icache_invalidate.
// On Linux this calls __builtin___clear_cache via CGO.
func flushIcache(addr uintptr, size int) {
	icacheFlush(unsafe.Pointer(addr), unsafe.Pointer(addr+uintptr(size)))
}
