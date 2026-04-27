package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"riscv"
)

// ── AOT JIT benchmarks ───────────────────────────────────────────────────

func benchAotJITELF(b *testing.B, elfData []byte) {
	b.Helper()
	b.ReportAllocs()
	b.StopTimer()

	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.SetAllocStrategy("fixed")
		if err := jit.InstallAOT(mem, elfData); err != nil {
			b.Fatalf("InstallAOT: %v", err)
		}
		b.StartTimer()
		runJITBenchGuestWith(cpu, jit)
		b.StopTimer()
		mem.Free()
	}
}

func BenchmarkAotJIT_CoreMark(b *testing.B) {
	benchAotJITELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf"))
}

func BenchmarkAotJIT_Dhrystone(b *testing.B) {
	benchAotJITELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf"))
}

func BenchmarkAotJIT_BenchGuest(b *testing.B) {
	benchAotJITELF(b, loadCPUELF(b))
}

// ── Lazy JIT benchmarks ──────────────────────────────────────────────────

func benchLazyJITELF(b *testing.B, elfData []byte) {
	b.Helper()
	b.ReportAllocs()
	b.StopTimer()

	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.DisableAutoAOT = true
		b.StartTimer()
		runJITBenchGuestWith(cpu, jit)
		b.StopTimer()
		mem.Free()
	}
}

func BenchmarkLazyJIT_CoreMark(b *testing.B) {
	benchLazyJITELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf"))
}

func BenchmarkLazyJIT_Dhrystone(b *testing.B) {
	benchLazyJITELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf"))
}

func BenchmarkLazyJIT_BenchGuest(b *testing.B) {
	benchLazyJITELF(b, loadCPUELF(b))
}

// ── RISC-V test ELF benchmarks ───────────────────────────────────────────

const rvTestsDir = "../riscv-elf-tests"

type rvTestELF struct {
	name string
	data []byte
}

func loadRVTestELFs(tb testing.TB) []rvTestELF {
	tb.Helper()
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		tb.Skip("rv64ui ELFs not found — run make riscv-tests")
	}
	var elfs []rvTestELF
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Fatalf("read %s: %v", path, err)
		}
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		elfs = append(elfs, rvTestELF{name: name, data: data})
	}
	return elfs
}

func newRVTestCPU(tb testing.TB, elfData []byte) (*riscv.CPU, *riscv.GuestMemory) {
	tb.Helper()
	mem, err := riscv.NewGuestMemory(riscv.Size1MB)
	if err != nil {
		tb.Fatal(err)
	}
	elf, err := riscv.LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		tb.Fatal(err)
	}
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)

	o := riscv.NewOS()
	o.HandleSyscall(93, riscv.LinuxExit)
	o.HandleSyscall(94, riscv.LinuxExit)
	o.HandleEcall(riscv.RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	return cpu, mem
}

func BenchmarkRVTests_UI_AotJIT(b *testing.B) {
	elfs := loadRVTestELFs(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for j, e := range elfs {
			t0 := time.Now()
			cpu, mem := newRVTestCPU(b, e.data)
			jit := riscv.NewJIT()
			if err := jit.InstallAOT(mem, e.data); err != nil {
				b.Fatalf("InstallAOT: %v", err)
			}
			runJITBenchGuestWith(cpu, jit)
			mem.Free()
			fmt.Fprintf(os.Stderr, "  AotJIT  [%2d/%d] %-12s %v\n", j+1, len(elfs), e.name, time.Since(t0))
		}
	}
}

func BenchmarkRVTests_UI_LazyJIT(b *testing.B) {
	elfs := loadRVTestELFs(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for j, e := range elfs {
			t0 := time.Now()
			cpu, mem := newRVTestCPU(b, e.data)
			jit := riscv.NewJIT()
			jit.DisableAutoAOT = true
			runJITBenchGuestWith(cpu, jit)
			mem.Free()
			fmt.Fprintf(os.Stderr, "  LazyJIT [%2d/%d] %-12s %v\n", j+1, len(elfs), e.name, time.Since(t0))
		}
	}
}
