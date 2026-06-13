//go:build arm64 && !cgo

package riscv

import "unsafe"

// icacheFlush cleans D-cache and invalidates I-cache for [start, end). The
// cgo-free implementation lives in jit_icache_arm64_nocgo.s so linux/arm64
// QEMU tests exercise real code-patch coherency.
func icacheFlush(start, end unsafe.Pointer)
