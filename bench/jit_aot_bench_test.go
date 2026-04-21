package bench

import (
	"testing"

	"riscv"
)

func benchJITAOTELF(b *testing.B, elfData []byte) {
	b.Helper()
	b.ReportAllocs()
	b.StopTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.SetAllocStrategy("fixed")
		if err := jit.InstallAOT(mem, elfData); err != nil {
			b.Fatalf("InstallAOT: %v", err)
		}
		b.StartTimer()
		_, insns := runJITBenchGuestWith(cpu, jit)
		b.StopTimer()
		totalInsns += insns
		mem.Free()
	}

	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		mips := float64(totalInsns) / elapsed / 1e6
		b.ReportMetric(mips, "MIPS")
	}
}

func BenchmarkJITAOT_CoreMark(b *testing.B) {
	benchJITAOTELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf"))
}

func BenchmarkJITAOT_Dhrystone(b *testing.B) {
	benchJITAOTELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf"))
}

func BenchmarkJITAOT_BenchGuest(b *testing.B) {
	benchJITAOTELF(b, loadCPUELF(b))
}
