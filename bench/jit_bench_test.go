package bench

import (
	"testing"

	"riscv"
)

func runJITBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	return runJITBenchGuestWith(cpu, riscv.NewJIT())
}

func TestJIT_BenchGuest_Smoke(t *testing.T) {
	elfData := loadCPUELF(t)
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	exitCode, insns := runJITBenchGuest(cpu)
	t.Logf("JIT smoke: retired %d instructions, exit code %d, PC=0x%x",
		insns, exitCode, cpu.PC())
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
}

func TestJIT_DispatchStats(t *testing.T) {
	elfData := loadCPUELF(t)
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	jit := riscv.NewJIT()
	riscv.SetDebugJIT(true) // enable emitBlock diagnostic logging
	defer riscv.SetDebugJIT(false)
	exitCode, insns := runJITBenchGuestWith(cpu, jit)
	t.Logf("retired %d instructions, exit code %d", insns, exitCode)
	t.Logf("Dispatch stats:")
	t.Logf("  jitOK returns:   %d", jit.DispatchOK)
	t.Logf("  other returns:   %d", jit.DispatchOther)
	t.Logf("  interp fallback: %d", jit.DispatchInterp)
	t.Logf("  compilations:    %d", jit.DispatchCompile)
	t.Logf("  chains patched:  %d", jit.ChainPatched)
	t.Logf("  insns/dispatch:  %.1f", float64(insns)/float64(jit.DispatchOK+jit.DispatchOther+jit.DispatchInterp))
	t.Logf("  noJIT set size:  %d", jit.NoJITSize())
}

func BenchmarkCPU_FullExecution_JIT_Fixed(b *testing.B) {
	benchJITWith(b, "fixed")
}

func BenchmarkCPU_FullExecution_JIT_TCC(b *testing.B) {
	elfData := loadCPUELF(b)
	b.ReportAllocs()
	b.ResetTimer()
	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		_, insns := runTccJITBenchGuestWith(cpu, jit)
		totalInsns += insns
		mem.Free()
	}
	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		mips := float64(totalInsns) / elapsed / 1e6
		b.ReportMetric(mips, "MIPS")
	}
}

func benchJITWith(b *testing.B, strategy string) {
	b.Helper()
	benchJITELF(b, loadCPUELF(b), strategy)
}

// ── CoreMark JIT benchmarks ───────────────────────────────────────────────

func BenchmarkJIT_CoreMark_Fixed(b *testing.B) {
	benchJITELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf"), "fixed")
}

// ── Dhrystone JIT benchmarks ──────────────────────────────────────────────

func BenchmarkJIT_Dhrystone_Fixed(b *testing.B) {
	benchJITELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf"), "fixed")
}

// benchJITELF runs the JIT benchmark loop against an arbitrary guest
// ELF. Used by the bench_guest, CoreMark, and Dhrystone JIT benchmarks.
func benchJITELF(b *testing.B, elfData []byte, strategy string) {
	b.Helper()

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.SetAllocStrategy(strategy)
		_, insns := runJITBenchGuestWith(cpu, jit)
		totalInsns += insns
		mem.Free()
	}

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		mips := float64(totalInsns) / elapsed / 1e6
		b.ReportMetric(mips, "MIPS")
	}
}
