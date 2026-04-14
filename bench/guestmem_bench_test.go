// Package bench contains benchmarks for the riscv emulator subsystems.
// These benchmarks have no cgo dependency and run under plain `go test`.
//
// Run standalone:
//
//	go test -bench=. -benchmem ./bench/
//
// Run as part of the full comparison (requires `make bench-setup`):
//
//	make bench
package bench

import (
	"testing"

	"riscv"
)

// ---------------------------------------------------------------------------
// GuestMemory hot-path benchmarks
//
// Every instruction that touches memory pays at least one of these costs.
// Target: ≤2 ns/op for Load64/Store64 on a modern server CPU.
// ---------------------------------------------------------------------------

// BenchmarkGuestMem_Load64 measures the hot-path cost of a single
// naturally-aligned 64-bit load: check() + hostPtr() + dereference.
// This is the most frequent operation in the emulator's inner loop.
func BenchmarkGuestMem_Load64(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	if f := m.Store64(4096, 0xDEADBEEFCAFEBABE); f != nil {
		b.Fatal(f)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v, f := m.Load64(4096)
		if f != nil {
			b.Fatal(f)
		}
		_ = v
	}
}

// BenchmarkGuestMem_Store64 measures the hot-path cost of a single
// naturally-aligned 64-bit store.
func BenchmarkGuestMem_Store64(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.Store64(4096, uint64(i)); f != nil {
			b.Fatal(f)
		}
	}
}

// BenchmarkGuestMem_Store64Load64Pair measures one Store64 immediately
// followed by one Load64 at the same address.
//
// This is the direct equivalent of BenchmarkLibriscv_MemWriteRead64 and is
// the primary apples-to-apples memory comparison point between the two
// implementations. libriscv uses copy_to_guest + copy_from_guest; we use
// our bit-twiddled check() + bare pointer dereference.
func BenchmarkGuestMem_Store64Load64Pair(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	const addr = uint64(4096)
	val := uint64(0xDEADBEEFCAFEBABE)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.Store64(addr, val); f != nil {
			b.Fatal(f)
		}
		v, f := m.Load64(addr)
		if f != nil {
			b.Fatal(f)
		}
		val ^= v // prevent dead-code elimination
	}
}

// BenchmarkGuestMem_Load32 measures 32-bit loads — the width used for
// instruction fetch (non-compressed) and most integer arithmetic results.
func BenchmarkGuestMem_Load32(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	if f := m.Store32(4096, 0xDEADBEEF); f != nil {
		b.Fatal(f)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v, f := m.Load32(4096)
		if f != nil {
			b.Fatal(f)
		}
		_ = v
	}
}

// BenchmarkGuestMem_Fetch16 measures 16-bit instruction fetch.
// RVC compressed instructions make up 40–60% of a typical RV64GC binary.
// This is the single most frequent fetch operation in a compressed workload.
func BenchmarkGuestMem_Fetch16(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	// Two compressed NOPs back to back.
	if f := m.Store32(0, 0x40014001); f != nil {
		b.Fatal(f)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v, f := m.Fetch16(0)
		if f != nil {
			b.Fatal(f)
		}
		_ = v
	}
}

// BenchmarkGuestMem_Fetch32 measures 32-bit instruction fetch.
func BenchmarkGuestMem_Fetch32(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	// ADDI x0, x0, 0 — canonical 32-bit NOP.
	if f := m.Store32(0, 0x00000013); f != nil {
		b.Fatal(f)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v, f := m.Fetch32(0)
		if f != nil {
			b.Fatal(f)
		}
		_ = v
	}
}

// BenchmarkGuestMem_StridedLoad64 accesses 1024 addresses at 64-byte
// (cache-line) stride. Models sequential instruction fetch across a loop body.
func BenchmarkGuestMem_StridedLoad64(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	const stride = 64
	const n = 1024
	for i := uint64(0); i < n; i++ {
		if f := m.Store64(i*stride, i); f != nil {
			b.Fatal(f)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	acc := uint64(0)
	for i := 0; i < b.N; i++ {
		v, f := m.Load64(uint64(i%n) * stride)
		if f != nil {
			b.Fatal(f)
		}
		acc ^= v
	}
	_ = acc
}

// BenchmarkGuestMem_RandomLoad64 accesses pseudo-random aligned addresses.
// Models worst-case branch predictor behavior for the bounds check.
// The check branch is always not-taken; this tests predictor convergence.
func BenchmarkGuestMem_RandomLoad64(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	const n = 4096
	addrs := make([]uint64, n)
	x := uint64(0xDEADBEEF12345678)
	for i := range addrs {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		addrs[i] = (x & (riscv.Size64MB - 1)) &^ 7
		_ = m.Store64(addrs[i], x)
	}

	b.ResetTimer()
	b.ReportAllocs()
	acc := uint64(0)
	for i := 0; i < b.N; i++ {
		v, f := m.Load64(addrs[i%n])
		if f != nil {
			b.Fatal(f)
		}
		acc ^= v
	}
	_ = acc
}

// ---------------------------------------------------------------------------
// Bulk operation benchmarks — ELF loader and syscall data paths
// ---------------------------------------------------------------------------

// BenchmarkGuestMem_WriteBytes_1KB measures 1 KB bulk writes.
// Models a typical syscall write buffer transfer.
func BenchmarkGuestMem_WriteBytes_1KB(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	src := make([]byte, 1<<10)
	for i := range src {
		src[i] = byte(i)
	}

	b.SetBytes(1 << 10)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.WriteBytes(0, src); f != nil {
			b.Fatal(f)
		}
	}
}

// BenchmarkGuestMem_WriteBytes_4KB measures 4 KB bulk writes.
// Models ELF segment loading (page-granularity transfers).
func BenchmarkGuestMem_WriteBytes_4KB(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	src := make([]byte, 1<<12)
	for i := range src {
		src[i] = byte(i)
	}

	b.SetBytes(1 << 12)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.WriteBytes(0, src); f != nil {
			b.Fatal(f)
		}
	}
}

// BenchmarkGuestMem_ReadBytes_4KB measures 4 KB bulk reads.
// Models syscall buffer extraction (e.g. write(2) data from guest).
func BenchmarkGuestMem_ReadBytes_4KB(b *testing.B) {
	m, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Free()

	src := make([]byte, 1<<12)
	if f := m.WriteBytes(0, src); f != nil {
		b.Fatal(f)
	}
	dst := make([]byte, 1<<12)

	b.SetBytes(1 << 12)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.ReadBytes(0, dst); f != nil {
			b.Fatal(f)
		}
	}
}

// ---------------------------------------------------------------------------
// Allocation benchmarks
// ---------------------------------------------------------------------------

// BenchmarkGuestMem_Alloc64MB measures 64 MB mmap alloc + free.
// Comparable to BenchmarkLibriscv_MachineCreate (which also loads an ELF,
// so libriscv's number is necessarily larger).
func BenchmarkGuestMem_Alloc64MB(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := riscv.NewGuestMemory(riscv.Size64MB)
		if err != nil {
			b.Fatal(err)
		}
		m.Free()
	}
}

// BenchmarkGuestMem_Alloc4GB measures 4 GB MAP_NORESERVE alloc + free.
// Should be nearly identical to 64 MB — no physical pages are committed.
func BenchmarkGuestMem_Alloc4GB(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := riscv.NewGuestMemory(riscv.Size4GB)
		if err != nil {
			b.Fatal(err)
		}
		m.Free()
	}
}

// BenchmarkGuestMem_Alloc512GB measures 512 GB MAP_NORESERVE alloc + free.
// Validates that our upper limit is practically free to reserve.
func BenchmarkGuestMem_Alloc512GB(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := riscv.NewGuestMemory(riscv.Size512GB)
		if err != nil {
			b.Skipf("OS declined 512GB reservation (acceptable in constrained env): %v", err)
		}
		m.Free()
	}
}
