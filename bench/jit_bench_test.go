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

func BenchmarkCPU_FullExecution_JIT(b *testing.B) {
	benchJITWith(b, "els")
}

func BenchmarkCPU_FullExecution_JIT_Fixed(b *testing.B) {
	benchJITWith(b, "fixed")
}

func benchJITWith(b *testing.B, strategy string) {
	b.Helper()
	elfData := loadCPUELF(b)

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
