package bench

import (
	"testing"
	"time"

	"riscv"
)

// runChainReference runs a guest ELF under the Fixed Static Mapping JIT
// (production path) and reports dispatch/chain counters plus MIPS.
// Non-asserting — this is a measurement harness.
//
// Expected interpretation of the output:
//   - ChainPatched > 0              → chaining is firing
//   - insns/DispatchOK ≫ block size → most back-edges are chained
//   - insns/DispatchOK ≈ MaxIC      → BudgetCheck (GC safepoint) is the
//                                     dominant exit, not a chainable one
func runChainReference(t *testing.T, elfData []byte, workload string) {
	t.Helper()
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	jit := riscv.NewJIT()
	jit.SetAllocStrategy("fixed") // production path

	t0 := time.Now()
	exitCode, insns := runJITBenchGuestWith(cpu, jit)
	elapsed := time.Since(t0)

	if exitCode != 0 {
		t.Fatalf("%s guest exited non-zero: %d", workload, exitCode)
	}

	totalDispatches := jit.DispatchOK + jit.DispatchOther + jit.DispatchInterp
	insnsPerDispatchOK := 0.0
	if jit.DispatchOK > 0 {
		insnsPerDispatchOK = float64(insns) / float64(jit.DispatchOK)
	}
	insnsPerTotalDispatch := 0.0
	if totalDispatches > 0 {
		insnsPerTotalDispatch = float64(insns) / float64(totalDispatches)
	}
	mips := 0.0
	if elapsed > 0 {
		mips = float64(insns) / elapsed.Seconds() / 1e6
	}

	t.Logf("─── Chain reference (%s, Fixed Static Mapping) ───", workload)
	t.Logf("  elapsed           : %v", elapsed)
	t.Logf("  retired insns     : %d", insns)
	t.Logf("  DispatchOK        : %d   (jitOK returns to Go)", jit.DispatchOK)
	t.Logf("  DispatchOther     : %d   (ecall/fault/etc returns)", jit.DispatchOther)
	t.Logf("  DispatchInterp    : %d   (interpreter fallback)", jit.DispatchInterp)
	t.Logf("  DispatchCompile   : %d   (block compilations)", jit.DispatchCompile)
	t.Logf("  ChainPatched      : %d   (patches of MOVABS sentinel → target chainEntry)",
		jit.ChainPatched)
	t.Logf("  insns/DispatchOK  : %.1f", insnsPerDispatchOK)
	t.Logf("  insns/all-disp    : %.1f", insnsPerTotalDispatch)
	t.Logf("  MIPS              : %.1f", mips)
}

// TestJIT_ChainReference runs bench_guest.elf under Fixed Static Mapping
// and prints chain/dispatch counters.
//
// Run: go test -run TestJIT_ChainReference -v ./bench/
func TestJIT_ChainReference(t *testing.T) {
	runChainReference(t, loadCPUELF(t), "bench_guest")
}

// TestJIT_CoreMark_ChainReference runs coremark.elf under Fixed Static
// Mapping and prints chain/dispatch counters.
//
// Run: go test -run TestJIT_CoreMark_ChainReference -v ./bench/
func TestJIT_CoreMark_ChainReference(t *testing.T) {
	runChainReference(t, loadELFFrom(t, "CM_ELF", "coremark.elf"), "coremark")
}

// TestJIT_Dhrystone_ChainReference runs dhrystone.elf under Fixed Static
// Mapping and prints chain/dispatch counters.
//
// Run: go test -run TestJIT_Dhrystone_ChainReference -v ./bench/
func TestJIT_Dhrystone_ChainReference(t *testing.T) {
	runChainReference(t, loadELFFrom(t, "DHRY_ELF", "dhrystone.elf"), "dhrystone")
}
