//go:build libriscv

package libriscv_bench

import (
	"os"
	"testing"
)

// ── ELF loading ────────────────────────────────────────────────────────────

var elfCache []byte

func loadELF(tb testing.TB) []byte {
	tb.Helper()
	if elfCache != nil {
		return elfCache
	}
	path := os.Getenv("BENCH_ELF")
	if path == "" {
		path = "../libriscv_guest/bench_guest.elf"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("guest ELF not found at %q — run `make bench-setup` first: %v", path, err)
	}
	elfCache = data
	return data
}

func newMachine(tb testing.TB, memBytes uint64) *Machine {
	tb.Helper()
	elf := loadELF(tb)
	m := NewMachine(elf, memBytes)
	if m == nil {
		tb.Fatal("NewMachine returned nil — ELF invalid or memory too small")
	}
	tb.Cleanup(m.Close)
	return m
}

// ── smoke test ─────────────────────────────────────────────────────────────

// TestLibriscvSmokeTest verifies the guest ELF runs to completion.
func TestLibriscvSmokeTest(t *testing.T) {
	m := newMachine(t, 64<<20)
	n := m.RunToCompletion(10_000_000_000)
	if n == 0 {
		t.Fatal("machine retired 0 instructions — possible ELF loading failure")
	}
	t.Logf("libriscv smoke: retired %d instructions", n)
}

// ── calibration benchmarks ─────────────────────────────────────────────────

// BenchmarkLibriscv_MachineCreate measures ELF loading + machine init cost.
//
// Compare with: BenchmarkGuestMem_Alloc64MB (../bench/).
// The delta between them is ELF parsing overhead — a lower bound for our
// future ELF loader.
func BenchmarkLibriscv_MachineCreate(b *testing.B) {
	elf := loadELF(b)
	const memSize = 64 << 20

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := NewMachine(elf, memSize)
		if m == nil {
			b.Fatal("NewMachine returned nil")
		}
		m.Close()
	}
}

// BenchmarkLibriscv_MemWriteRead64 measures libriscv's host↔guest memory
// transfer cost: copy_to_guest + copy_from_guest for a uint64.
//
// Direct comparison target: BenchmarkGuestMem_Store64Load64Pair (../bench/).
//
// Methodology: 10,000 pairs are executed inside C per b.N iteration to
// amortize the cgo call boundary. The ns/pair metric reflects per-pair cost.
func BenchmarkLibriscv_MemWriteRead64(b *testing.B) {
	m := newMachine(b, 64<<20)

	// Target a guest address in the heap region, past ELF segments.
	const guestAddr = uint64(0x0200_0000) // 32 MB offset
	const pairsPerIter = 10_000

	b.ReportAllocs()
	b.ResetTimer()

	totalNs := int64(0)
	totalPairs := int64(0)
	for i := 0; i < b.N; i++ {
		ns := m.MemWriteReadPairs(guestAddr, pairsPerIter)
		if ns <= 0 {
			b.Fatal("MemWriteReadPairs returned non-positive duration")
		}
		totalNs += ns
		totalPairs += pairsPerIter
	}

	b.StopTimer()
	if totalPairs > 0 {
		nsPerPair := float64(totalNs) / float64(totalPairs)
		b.ReportMetric(nsPerPair, "ns/pair")
	}
}

// BenchmarkLibriscv_FullExecution runs bench_guest.elf to completion and
// reports MIPS. This is the end-to-end emulator throughput number —
// the ultimate target for our instruction loop once it exists.
//
// Each b.N iteration creates a fresh machine (includes ELF load overhead).
// See BenchmarkLibriscv_FullExecution_Steady for hot-path throughput only.
func BenchmarkLibriscv_FullExecution(b *testing.B) {
	elf := loadELF(b)
	const memSize = 64 << 20
	const insnLimit = uint64(10_000_000_000)

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		m := NewMachine(elf, memSize)
		if m == nil {
			b.Fatal("NewMachine returned nil")
		}
		totalInsns += m.RunToCompletion(insnLimit)
		m.Close()
	}

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		mips := float64(totalInsns) / elapsed / 1e6
		b.ReportMetric(mips, "MIPS")
	}
}

// BenchmarkLibriscv_FullExecution_Steady reuses a single machine across
// iterations to measure steady-state throughput, excluding ELF load cost.
func BenchmarkLibriscv_FullExecution_Steady(b *testing.B) {
	m := newMachine(b, 64<<20)
	const insnLimit = uint64(10_000_000_000)

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		totalInsns += m.RunToCompletion(insnLimit)
	}

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		mips := float64(totalInsns) / elapsed / 1e6
		b.ReportMetric(mips, "MIPS")
	}
}
