package bench

import (
	"testing"
	"time"

	"riscv"
)

// TestJIT_ChainReference runs bench_guest.elf under the Fixed Static
// Mapping JIT (production path) and reports dispatch/chain counters plus
// MIPS. Non-asserting — this is a before/after measurement harness.
//
// Expected shape:
//   - Before flipping emitChainableReturn (Part C):
//       ChainPatched == 0
//       DispatchOK scales with retired insns / avg-block-size
//       insns/DispatchOK ≈ block size (tens)
//
//   - After Part C:
//       ChainPatched > 0
//       DispatchOK drops by ≥ 100× (most back-edges chained)
//       insns/DispatchOK rises by ≥ 100×
//       MIPS rises meaningfully (target: +50%)
//
// Run: go test -run TestJIT_ChainReference -v ./bench/
func TestJIT_ChainReference(t *testing.T) {
	elfData := loadCPUELF(t)
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	jit := riscv.NewJIT()
	jit.SetAllocStrategy("fixed") // production path

	t0 := time.Now()
	exitCode, insns := runJITBenchGuestWith(cpu, jit)
	elapsed := time.Since(t0)

	if exitCode != 0 {
		t.Fatalf("guest exited non-zero: %d", exitCode)
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

	t.Logf("─── Chain reference (Fixed Static Mapping) ───")
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
