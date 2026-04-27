package bench

import (
	"os"
	"testing"

	"riscv"
)

// ── ELF loading ────────────────────────────────────────────────────────────

var cpuELFCache []byte

func loadCPUELF(tb testing.TB) []byte {
	tb.Helper()
	if cpuELFCache != nil {
		return cpuELFCache
	}
	path := os.Getenv("BENCH_ELF")
	if path == "" {
		path = "libriscv_guest/bench_guest.elf"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("guest ELF not found at %q — run `make bench-setup` first: %v", path, err)
	}
	cpuELFCache = data
	return data
}

// ── syscall stubs for musl startup ─────────────────────────────────────────

func brkHandler(_ *riscv.CPU, _ riscv.SyscallArgs) (riscv.SyscallResult, bool) {
	return 0, true // musl falls back gracefully when brk returns 0
}

func tidHandler(_ *riscv.CPU, _ riscv.SyscallArgs) (riscv.SyscallResult, bool) {
	return 1, true // fake TID
}

// ── run helper ─────────────────────────────────────────────────────────────

func newBenchCPU(tb testing.TB, elfData []byte) (*riscv.CPU, *riscv.GuestMemory) {
	tb.Helper()
	mem, err := riscv.NewGuestMemory(riscv.Size64MB)
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
	cpu.SetReg(2, 0x03F00000) // sp — near top of 64MB, zero-filled (argc=0)

	o := riscv.NewOS()
	o.HandleSyscall(93, riscv.LinuxExit) // exit
	o.HandleSyscall(94, riscv.LinuxExit) // exit_group
	o.HandleSyscall(214, brkHandler)     // brk
	o.HandleSyscall(96, tidHandler)      // set_tid_address
	cpu.Notes.Push(o.Handle)
	return cpu, mem
}

func runJITBenchGuestWith(cpu *riscv.CPU, jit *riscv.JIT) (exitCode int, insns uint64) {
	defer func() {
		if r := recover(); r != nil {
			vv("runJITBenchGuestWith recovered: r='%v'", r)
			if ex, ok := r.(*riscv.ExitError); ok {
				exitCode = ex.Code
				insns = cpu.Cycle()
				return
			}
			panic(r)
		}
	}()
	_ = jit.RunJIT(cpu)
	insns = cpu.Cycle()
	return
}

// runCachedBenchGuest uses the decoder cache (RunCached) instead of the
// un-cached RunWithChain. The cache is sized to cover typical executable
// segments (~256 KB) based at the ELF entry.
func runCachedBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*riscv.ExitError); ok {
				exitCode = ex.Code
				insns = cpu.Cycle()
				return
			}
			panic(r)
		}
	}()

	// try to avoid the 25% performance drop footgun of calling runCached.

	// Cache covers [entry-4K, entry+256K). Anything outside falls back to step().
	//base := cpu.PC() & ^uint64(0xFFF)
	//if base > 0x1000 {
	//	base -= 0x1000
	//}
	//cache := riscv.NewDecoderCache(base, 256<<10)
	//_ = riscv.RunCached(cpu, cache, &cpu.Notes)

	err := riscv.RunDefault(cpu, &cpu.Notes)
	_ = err
	insns = cpu.Cycle()
	return
}

// runBenchGuest runs the guest via the default interpreter path — cpu.Run(),
// which internally uses the decoder-cached RunCached driver. Measures what
// a typical CLI user would get by default.
func runBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*riscv.ExitError); ok {
				exitCode = ex.Code
				insns = cpu.Cycle()
				return
			}
			panic(r)
		}
	}()
	_ = cpu.Run()
	insns = cpu.Cycle()
	return
}

// runUncachedBenchGuest bypasses the decoder cache and uses the reference
// uncached driver (RunWithChain). Kept for head-to-head comparison with the
// default path; not what a typical user would run.
func runUncachedBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*riscv.ExitError); ok {
				exitCode = ex.Code
				insns = cpu.Cycle()
				return
			}
			panic(r)
		}
	}()
	_ = riscv.RunWithChain(cpu, &cpu.Notes)
	insns = cpu.Cycle()
	return
}

// ── smoke test ─────────────────────────────────────────────────────────────

func TestCPU_BenchGuest_Smoke(t *testing.T) {
	elfData := loadCPUELF(t)
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	code, insns := runBenchGuest(cpu)
	if code != 0 {
		t.Fatalf("guest exited with code %d, want 0", code)
	}
	if insns == 0 {
		t.Fatal("retired 0 instructions — guest did not run")
	}
	t.Logf("Go CPU smoke: retired %d instructions (exit code %d)", insns, code)
}

// ── MIPS benchmark ─────────────────────────────────────────────────────────

// BenchmarkCPU_FullExecution measures the default interpreter path on
// bench_guest.elf — cpu.Run() with its auto-allocated decoder cache.
func BenchmarkCPU_FullExecution(b *testing.B) {
	elfData := loadCPUELF(b)

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runBenchGuest(cpu)
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

// BenchmarkCPU_FullExecution_Uncached measures the un-cached reference path
// (RunWithChain) on bench_guest.elf, for head-to-head vs the default.
func BenchmarkCPU_FullExecution_Uncached(b *testing.B) {
	elfData := loadCPUELF(b)

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runUncachedBenchGuest(cpu)
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

// loadELFFrom reads an ELF whose path is either in BENCH_ELF_<name> env var
// or defaults to the given relative path.
func loadELFFrom(tb testing.TB, envVar, defaultPath string) []byte {
	tb.Helper()
	path := os.Getenv(envVar)
	if path == "" {
		path = defaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("ELF not found at %q — build it first (see Makefile): %v", path, err)
	}
	return data
}

// ── CoreMark benchmarks ───────────────────────────────────────────────────

// TestCPU_CoreMark_Smoke verifies the CoreMark guest ELF runs to completion
// via the cached driver.
func TestCPU_CoreMark_Smoke(t *testing.T) {
	elfData := loadELFFrom(t, "CM_ELF", "coremark.elf")
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()
	code, insns := runCachedBenchGuest(cpu)
	if code != 0 {
		t.Fatalf("coremark exited with %d, want 0", code)
	}
	if insns == 0 {
		t.Fatal("retired 0 instructions")
	}
	t.Logf("coremark: retired %d instructions (exit %d)", insns, code)
}

// BenchmarkCPU_CoreMark runs CoreMark through the cached interpreter.
// Reports MIPS — directly comparable to BenchmarkCPU_FullExecution_Cached
// on the bench_guest workload.
func BenchmarkCPU_CoreMark(b *testing.B) {
	elfData := loadELFFrom(b, "CM_ELF", "coremark.elf")

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runCachedBenchGuest(cpu)
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

// BenchmarkCPU_CoreMark_Uncached runs CoreMark through the un-cached
// interpreter (RunWithChain) — direct comparison with BenchmarkCPU_CoreMark.
func BenchmarkCPU_CoreMark_Uncached(b *testing.B) {
	elfData := loadELFFrom(b, "CM_ELF", "coremark.elf")

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runUncachedBenchGuest(cpu)
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

// ── Dhrystone benchmarks ──────────────────────────────────────────────────

func TestCPU_Dhrystone_Smoke(t *testing.T) {
	elfData := loadELFFrom(t, "DHRY_ELF", "dhrystone.elf")
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()
	code, insns := runCachedBenchGuest(cpu)
	if code != 0 {
		t.Fatalf("dhrystone exited with %d, want 0", code)
	}
	if insns == 0 {
		t.Fatal("retired 0 instructions")
	}
	t.Logf("dhrystone: retired %d instructions (exit %d)", insns, code)
}

func BenchmarkCPU_Dhrystone(b *testing.B) {
	elfData := loadELFFrom(b, "DHRY_ELF", "dhrystone.elf")

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runCachedBenchGuest(cpu)
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

func BenchmarkCPU_Dhrystone_Uncached(b *testing.B) {
	elfData := loadELFFrom(b, "DHRY_ELF", "dhrystone.elf")

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runUncachedBenchGuest(cpu)
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

// BenchmarkCPU_FullExecution_Cached runs the same workload through the
// decoder-cache driver (RunCached). Used to measure the skip-fetch speedup
// vs the un-cached BenchmarkCPU_FullExecution above.
func BenchmarkCPU_FullExecution_Cached(b *testing.B) {
	elfData := loadCPUELF(b)

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		_, insns := runCachedBenchGuest(cpu)
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
