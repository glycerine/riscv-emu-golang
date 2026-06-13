//go:build arm64 && !cgo

package riscv

import "unsafe"

// icacheFlush is a compile-time fallback for cgo-free ARM64 test binaries.
// It is sufficient for non-JIT cross-build gates; executable ARM64 backend
// tests must use a real cache flush path before parity can be claimed.
func icacheFlush(start, end unsafe.Pointer) {}
