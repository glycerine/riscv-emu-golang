package riscv

// flushIcache is a no-op on x86-64. The x86 architecture guarantees
// instruction cache coherency: stores to code pages are visible to
// subsequent instruction fetches without any explicit flush. Intel
// SDM Vol 3A §11.6 "Self-Modifying Code" documents this guarantee.
func flushIcache(addr uintptr, size int) {}
