package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glycerine/riscv-emu-golang"
)

// ── AOT JIT benchmarks ───────────────────────────────────────────────────

func benchAotJITELF(b *testing.B, elfData []byte) {
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
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
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

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.DisableAutoAOT = true
		b.StartTimer()
		_, insns := runJITBenchGuestWith(cpu, jit)
		b.StopTimer()
		totalInsns += insns
		mem.Free()
	}

	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
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

func loadRVTestELF(tb testing.TB, name string) rvTestELF {
	tb.Helper()
	path := filepath.Join(rvTestsDir, "rv64ui-p-"+name)
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("rv64ui-p-%s not found — run make riscv-tests: %v", name, err)
	}
	return rvTestELF{name: name, data: data}
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

func newRVTestLoaded(tb testing.TB, elfData []byte) (*riscv.GuestMemory, uint64, uint64) {
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
	return mem, elf.Entry, elf.TohostAddr
}

func newRVTestCPUFromLoaded(mem *riscv.GuestMemory, entry, tohost uint64) *riscv.CPU {
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetWatchAddr(tohost)

	o := riscv.NewOS()
	o.HandleSyscall(93, riscv.LinuxExit)
	o.HandleSyscall(94, riscv.LinuxExit)
	o.HandleEcall(riscv.RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	return cpu
}

func resetRVTestTohost(tb testing.TB, mem *riscv.GuestMemory, tohost uint64) {
	tb.Helper()
	if tohost == 0 {
		return
	}
	if f := mem.Store64(tohost, 0); f != nil {
		tb.Fatalf("reset tohost: %v", f)
	}
}

func reportRVTestMIPS(b *testing.B, totalInsns uint64) {
	b.Helper()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
	}
}

func benchRVTestUICached(b *testing.B, name string) {
	e := loadRVTestELF(b, name)
	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newRVTestCPU(b, e.data)
		code, insns := runCachedBenchGuest(cpu)
		mem.Free()
		if code != 0 {
			b.Fatalf("rv64ui-p-%s cached interpreter exit %d, want 0", name, code)
		}
		totalInsns += insns
	}

	b.StopTimer()
	reportRVTestMIPS(b, totalInsns)
}

func benchRVTestUILazyJIT(b *testing.B, name string) {
	benchRVTestUILazyJITPolicy(b, name, riscv.PolicyABJIT)
}

func benchRVTestUILazyJITPolicy(b *testing.B, name string, policy riscv.RegPolicy) {
	e := loadRVTestELF(b, name)
	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newRVTestCPU(b, e.data)
		jit := riscv.NewJIT()
		jit.SetRegPolicy(policy)
		jit.DisableAutoAOT = true
		code, insns := runJITBenchGuestWith(cpu, jit)
		mem.Free()
		if code != 0 {
			b.Fatalf("rv64ui-p-%s lazy JIT exit %d, want 0", name, code)
		}
		totalInsns += insns
	}

	b.StopTimer()
	reportRVTestMIPS(b, totalInsns)
}

func benchRVTestUILazyJITHotPolicy(b *testing.B, name string, policy riscv.RegPolicy) {
	e := loadRVTestELF(b, name)
	jit := riscv.NewJIT()
	jit.SetRegPolicy(policy)
	jit.DisableAutoAOT = true

	warmCPU, warmMem := newRVTestCPU(b, e.data)
	code, _ := runJITBenchGuestWith(warmCPU, jit)
	warmMem.Free()
	if code != 0 {
		b.Fatalf("rv64ui-p-%s warm lazy JIT exit %d, want 0", name, code)
	}

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newRVTestCPU(b, e.data)
		code, insns := runJITBenchGuestWith(cpu, jit)
		mem.Free()
		if code != 0 {
			b.Fatalf("rv64ui-p-%s hot lazy JIT exit %d, want 0", name, code)
		}
		totalInsns += insns
	}

	b.StopTimer()
	reportRVTestMIPS(b, totalInsns)
}

func benchRVTestUIRunOnlyCached(b *testing.B, name string) {
	e := loadRVTestELF(b, name)
	mem, entry, tohost := newRVTestLoaded(b, e.data)
	defer mem.Free()

	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		resetRVTestTohost(b, mem, tohost)
		cpu := newRVTestCPUFromLoaded(mem, entry, tohost)
		b.StartTimer()
		code, insns := runCachedBenchGuest(cpu)
		b.StopTimer()
		if code != 0 {
			b.Fatalf("rv64ui-p-%s run-only cached interpreter exit %d, want 0", name, code)
		}
		totalInsns += insns
	}

	reportRVTestMIPS(b, totalInsns)
}

func benchRVTestUIRunOnlyHotJITPolicy(b *testing.B, name string, policy riscv.RegPolicy) {
	e := loadRVTestELF(b, name)
	mem, entry, tohost := newRVTestLoaded(b, e.data)
	defer mem.Free()

	jit := riscv.NewJIT()
	jit.SetRegPolicy(policy)
	jit.DisableAutoAOT = true

	resetRVTestTohost(b, mem, tohost)
	warmCPU := newRVTestCPUFromLoaded(mem, entry, tohost)
	code, _ := runJITBenchGuestWith(warmCPU, jit)
	if code != 0 {
		b.Fatalf("rv64ui-p-%s warm run-only JIT exit %d, want 0", name, code)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		resetRVTestTohost(b, mem, tohost)
		cpu := newRVTestCPUFromLoaded(mem, entry, tohost)
		b.StartTimer()
		code, insns := runJITBenchGuestWith(cpu, jit)
		b.StopTimer()
		if code != 0 {
			b.Fatalf("rv64ui-p-%s run-only hot JIT exit %d, want 0", name, code)
		}
		totalInsns += insns
	}

	reportRVTestMIPS(b, totalInsns)
}

func benchRVTestUIAotJIT(b *testing.B, name string) {
	e := loadRVTestELF(b, name)
	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newRVTestCPU(b, e.data)
		jit := riscv.NewJIT()
		if err := jit.InstallAOT(mem, e.data); err != nil {
			mem.Free()
			b.Fatalf("InstallAOT rv64ui-p-%s: %v", name, err)
		}
		code, insns := runJITBenchGuestWith(cpu, jit)
		mem.Free()
		if code != 0 {
			b.Fatalf("rv64ui-p-%s AOT JIT exit %d, want 0", name, code)
		}
		totalInsns += insns
	}

	b.StopTimer()
	reportRVTestMIPS(b, totalInsns)
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
			vv("jit.InstallAOT: %v", e.name)
			if err := jit.InstallAOT(mem, e.data); err != nil {
				b.Fatalf("InstallAOT: %v", err)
			}
			vv("runJITBenchGuestWith: %v", e.name)
			runJITBenchGuestWith(cpu, jit)
			vv("back from runJITBenchGuestWith: %v", e.name)
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

func BenchmarkRVTests_UI_Interp2(b *testing.B) {
	benchRVTestUICached(b, "add")
}

func BenchmarkRVTests_UI_LazyJIT2(b *testing.B) {
	benchRVTestUILazyJIT(b, "add")
}

func BenchmarkRVTests_UI_LazyJIT2_RV8(b *testing.B) {
	benchRVTestUILazyJITPolicy(b, "add", riscv.PolicyRV8)
}

func BenchmarkRVTests_UI_LazyJIT2_Hot(b *testing.B) {
	benchRVTestUILazyJITHotPolicy(b, "add", riscv.PolicyABJIT)
}

func BenchmarkRVTests_UI_LazyJIT2_Hot_RV8(b *testing.B) {
	benchRVTestUILazyJITHotPolicy(b, "add", riscv.PolicyRV8)
}

func BenchmarkRVTests_UI_RunOnlyInterp2(b *testing.B) {
	benchRVTestUIRunOnlyCached(b, "add")
}

func BenchmarkRVTests_UI_RunOnlyLazyJIT2_Hot(b *testing.B) {
	benchRVTestUIRunOnlyHotJITPolicy(b, "add", riscv.PolicyABJIT)
}

func BenchmarkRVTests_UI_RunOnlyLazyJIT2_Hot_RV8(b *testing.B) {
	benchRVTestUIRunOnlyHotJITPolicy(b, "add", riscv.PolicyRV8)
}

func BenchmarkRVTests_UI_AotJIT2(b *testing.B) {
	benchRVTestUIAotJIT(b, "add")
}
