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
	entry, err := riscv.LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		tb.Fatal(err)
	}
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetReg(2, 0x03F00000) // sp — near top of 64MB, zero-filled (argc=0)

	o := riscv.NewOS()
	o.HandleSyscall(93, riscv.LinuxExit)  // exit
	o.HandleSyscall(94, riscv.LinuxExit)  // exit_group
	o.HandleSyscall(214, brkHandler)      // brk
	o.HandleSyscall(96, tidHandler)       // set_tid_address
	cpu.Notes.Push(o.Handle)
	return cpu, mem
}

func runJITBenchGuestWith(cpu *riscv.CPU, jit *riscv.JIT) (exitCode int, insns uint64) {
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
	_ = jit.RunJIT(cpu)
	insns = cpu.Cycle()
	return
}

func runTccJITBenchGuestWith(cpu *riscv.CPU, jit *riscv.JIT) (exitCode int, insns uint64) {
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
	_ = jit.TccRunJIT(cpu)
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
	// Cache covers [entry-4K, entry+256K). Anything outside falls back to step().
	base := cpu.PC() & ^uint64(0xFFF)
	if base > 0x1000 {
		base -= 0x1000
	}
	cache := riscv.NewDecoderCache(base, 256<<10)
	_ = riscv.RunCached(cpu, cache, &cpu.Notes)
	insns = cpu.Cycle()
	return
}

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
