package bench

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/glycerine/riscv-emu-golang"
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

func brkHandler(_ *riscv.CPU, _ riscv.SyscallArgs) (int64, bool, bool, error) {
	return 0, true, false, nil
}

func tidHandler(_ *riscv.CPU, _ riscv.SyscallArgs) (int64, bool, bool, error) {
	return 1, true, false, nil
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
	err := jit.RunJIT(cpu)
	insns = cpu.RiscvInstrBegun()
	if ex, ok := err.(*riscv.ExitError); ok {
		exitCode = ex.Code
		return
	}
	if err != nil {
		panic(err)
	}
	return
}

// runCachedBenchGuest uses the decoder cache (RunCached) instead of the
// un-cached RunWithChain. The cache is sized to cover typical executable
// segments (~256 KB) based at the ELF entry.
func runCachedBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	err := riscv.RunDefault(cpu, &cpu.Notes)
	insns = cpu.RiscvInstrBegun()
	if ex, ok := err.(*riscv.ExitError); ok {
		exitCode = ex.Code
	}
	return
}

// runBenchGuest runs the guest via the default interpreter path — cpu.Run(),
// which internally uses the decoder-cached RunCached driver. Measures what
// a typical CLI user would get by default.
func runBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	err := cpu.Run()
	insns = cpu.RiscvInstrBegun()
	if ex, ok := err.(*riscv.ExitError); ok {
		exitCode = ex.Code
	}
	return
}

// runUncachedBenchGuest bypasses the decoder cache and uses the reference
// uncached driver (RunWithChain). Kept for head-to-head comparison with the
// default path; not what a typical user would run.
func runUncachedBenchGuest(cpu *riscv.CPU) (exitCode int, insns uint64) {
	err := riscv.RunWithChain(cpu, &cpu.Notes)
	insns = cpu.RiscvInstrBegun()
	if ex, ok := err.(*riscv.ExitError); ok {
		exitCode = ex.Code
	}
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
		t.Fatal("attempted 0 instructions — guest did not run")
	}
	t.Logf("Go CPU smoke: attempted %d instructions (exit code %d)", insns, code)
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

const zygoFib10Program = "(defn fib [x] (cond (== x 0) 0 (== x 1) 1 (+ (fib (- x 1)) (fib (- x 2))))) (println (fib 10))"

func zygoClockMode(tb testing.TB) riscv.Jea9LinuxClockMode {
	tb.Helper()
	switch raw := os.Getenv("ZYGO_CLOCK_MODE"); raw {
	case "", "idle", "idlejump", "idle-jump":
		return riscv.Jea9ClockIdleJump
	case "ictick", "ic-tick", "tick":
		return riscv.Jea9ClockICTick
	default:
		tb.Fatalf("invalid ZYGO_CLOCK_MODE %q; want idle-jump or ic-tick", raw)
		return riscv.Jea9ClockIdleJump
	}
}

func zygoNSPerInstruction(tb testing.TB) int64 {
	tb.Helper()
	raw := os.Getenv("ZYGO_NS_PER_INSN")
	if raw == "" {
		return 1
	}
	parsed, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		tb.Fatalf("invalid ZYGO_NS_PER_INSN %q: %v", raw, err)
	}
	return parsed
}

func zygoNanosleepAdvance(tb testing.TB) (riscv.Jea9LinuxNanosleepAdvanceMode, int64) {
	tb.Helper()
	raw := os.Getenv("ZYGO_NANOSLEEP_ADVANCE_NS")
	switch raw {
	case "", "requested", "default":
		return riscv.Jea9NanosleepAdvanceRequested, 0
	case "none":
		return riscv.Jea9NanosleepAdvanceFixed, 0
	}
	parsed, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		tb.Fatalf("invalid ZYGO_NANOSLEEP_ADVANCE_NS %q: %v", raw, err)
	}
	return riscv.Jea9NanosleepAdvanceFixed, parsed
}

func newZygoJea9LinuxCPU(tb testing.TB, elfData []byte) (*riscv.CPU, *riscv.GuestMemory, *riscv.Jea9Linux) {
	tb.Helper()
	mem, err := riscv.NewGuestMemory(riscv.Size16GB)
	if err != nil {
		tb.Fatal(err)
	}
	elf, err := riscv.LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		tb.Fatal(err)
	}
	cpu := riscv.NewCPU(*mem)
	budget := uint64(1 << 20)
	if raw := os.Getenv("ZYGO_BUDGET"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 0, 64)
		if err != nil {
			mem.Free()
			tb.Fatalf("invalid ZYGO_BUDGET %q: %v", raw, err)
		}
		budget = parsed
	}
	nanosleepMode, nanosleepFixedNS := zygoNanosleepAdvance(tb)
	jlinux := riscv.NewJea9Linux(riscv.Jea9LinuxOptions{
		ClockMode:         zygoClockMode(tb),
		MonotonicStartNS:  1,
		NSPerInstruction:  zygoNSPerInstruction(tb),
		NanosleepMode:     nanosleepMode,
		NanosleepFixedNS:  nanosleepFixedNS,
		InstructionBudget: budget,
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
	})
	args := []string{"bench/zygo.elf", "-c", zygoFib10Program}
	if err := jlinux.InitELFStack(cpu, elf, riscv.Jea9LinuxStartOptions{
		Args:     args,
		ExecPath: args[0],
	}); err != nil {
		mem.Free()
		tb.Fatal(err)
	}
	return cpu, mem, jlinux
}

func BenchmarkCPU_ZygoFib10_LazyJIT(b *testing.B) {
	elfData := loadELFFrom(b, "ZYGO_ELF", "zygo.elf")
	traceFallbacks := os.Getenv("GOCPU_JIT_FALLBACK_TRACE") != ""

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	var totalSyscalls uint64
	var totalNanosleeps uint64
	var totalNanosleepNS uint64
	var maxNanosleepNS uint64
	var totalBudgetYields uint64
	var totalMonotonicNS int64
	var syscallCounts [512]uint64
	syscallPCCounts := make(map[uint64]uint64)
	var totalStats jitBenchStats
	for i := 0; i < b.N; i++ {
		cpu, mem, jlinux := newZygoJea9LinuxCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.AutoAOT = false
		if traceFallbacks {
			jit.EnableFallbackTrace()
		}
		code, err := riscv.RunWithJea9LinuxJIT(cpu, jit, jlinux)
		insns := cpu.RiscvInstrBegun()
		totalInsns += insns
		totalSyscalls += jlinux.SyscallCount()
		nanoCount, nanoTotal, nanoMax := jlinux.NanosleepStats()
		totalNanosleeps += nanoCount
		totalNanosleepNS += nanoTotal
		if nanoMax > maxNanosleepNS {
			maxNanosleepNS = nanoMax
		}
		totalBudgetYields += jlinux.BudgetYields()
		totalMonotonicNS += jlinux.MonotonicNS()
		addJea9SyscallCounts(&syscallCounts, jlinux)
		addJea9SyscallPCCounts(syscallPCCounts, jlinux)
		totalStats.add(jit)
		if traceFallbacks {
			dumpJITFallbackTrace(b, jit, 32)
		}
		jit.Close()
		mem.Free()
		if err != nil {
			b.Fatalf("RunWithJea9LinuxJIT: %v", err)
		}
		if code != 0 {
			b.Fatalf("zygo exited with code %d, want 0", code)
		}
	}

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
	}
	if b.N > 0 {
		b.ReportMetric(float64(totalInsns)/float64(b.N), "insns/op")
		b.ReportMetric(float64(totalSyscalls)/float64(b.N), "syscalls/op")
		b.ReportMetric(float64(totalNanosleeps)/float64(b.N), "nanosleep/op")
		b.ReportMetric(float64(totalNanosleepNS)/float64(b.N), "nanosleep_ns/op")
		b.ReportMetric(float64(maxNanosleepNS), "nanosleep_max_ns")
		b.ReportMetric(float64(totalBudgetYields)/float64(b.N), "budget_yield/op")
		b.ReportMetric(float64(totalMonotonicNS)/float64(b.N), "monotonic_ns/op")
		reportJea9SyscallCounts(b, "sys", syscallCounts)
		reportJea9SyscallPCCounts(b, "sys_pc", syscallPCCounts)
	}
	totalStats.report(b)
}

func BenchmarkCPU_ZygoFib10_Interpreter(b *testing.B) {
	elfData := loadELFFrom(b, "ZYGO_ELF", "zygo.elf")

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	var totalSyscalls uint64
	var totalNanosleeps uint64
	var totalNanosleepNS uint64
	var maxNanosleepNS uint64
	var totalBudgetYields uint64
	var totalMonotonicNS int64
	var syscallCounts [512]uint64
	syscallPCCounts := make(map[uint64]uint64)
	for i := 0; i < b.N; i++ {
		cpu, mem, jlinux := newZygoJea9LinuxCPU(b, elfData)
		code, err := riscv.RunWithJea9LinuxInterp(cpu, jlinux)
		insns := cpu.RiscvInstrBegun()
		totalInsns += insns
		totalSyscalls += jlinux.SyscallCount()
		nanoCount, nanoTotal, nanoMax := jlinux.NanosleepStats()
		totalNanosleeps += nanoCount
		totalNanosleepNS += nanoTotal
		if nanoMax > maxNanosleepNS {
			maxNanosleepNS = nanoMax
		}
		totalBudgetYields += jlinux.BudgetYields()
		totalMonotonicNS += jlinux.MonotonicNS()
		addJea9SyscallCounts(&syscallCounts, jlinux)
		addJea9SyscallPCCounts(syscallPCCounts, jlinux)
		mem.Free()
		if err != nil {
			b.Fatalf("RunWithJea9Linux: %v", err)
		}
		if code != 0 {
			b.Fatalf("zygo exited with code %d, want 0", code)
		}
	}

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
	}
	if b.N > 0 {
		b.ReportMetric(float64(totalInsns)/float64(b.N), "insns/op")
		b.ReportMetric(float64(totalSyscalls)/float64(b.N), "syscalls/op")
		b.ReportMetric(float64(totalNanosleeps)/float64(b.N), "nanosleep/op")
		b.ReportMetric(float64(totalNanosleepNS)/float64(b.N), "nanosleep_ns/op")
		b.ReportMetric(float64(maxNanosleepNS), "nanosleep_max_ns")
		b.ReportMetric(float64(totalBudgetYields)/float64(b.N), "budget_yield/op")
		b.ReportMetric(float64(totalMonotonicNS)/float64(b.N), "monotonic_ns/op")
		reportJea9SyscallCounts(b, "sys", syscallCounts)
		reportJea9SyscallPCCounts(b, "sys_pc", syscallPCCounts)
	}
}

func addJea9SyscallCounts(dst *[512]uint64, jlinux *riscv.Jea9Linux) {
	if dst == nil || jlinux == nil {
		return
	}
	for i := range dst {
		dst[i] += jlinux.SyscallCountByNumber(uint64(i))
	}
}

func reportJea9SyscallCounts(b *testing.B, prefix string, counts [512]uint64) {
	b.Helper()
	if b.N == 0 {
		return
	}
	used := make([]bool, len(counts))
	for rank := 0; rank < 5; rank++ {
		var bestNum int
		var bestCount uint64
		for num, count := range counts {
			if used[num] || count <= bestCount {
				continue
			}
			bestNum = num
			bestCount = count
		}
		if bestCount == 0 {
			return
		}
		used[bestNum] = true
		b.ReportMetric(float64(bestCount)/float64(b.N), fmt.Sprintf("%s_%d/op", prefix, bestNum))
	}
}

func addJea9SyscallPCCounts(dst map[uint64]uint64, jlinux *riscv.Jea9Linux) {
	if dst == nil || jlinux == nil {
		return
	}
	for _, ent := range jlinux.TopSyscallPCCounts(16) {
		dst[ent.PC] += ent.Count
	}
}

func reportJea9SyscallPCCounts(b *testing.B, prefix string, counts map[uint64]uint64) {
	b.Helper()
	if b.N == 0 || len(counts) == 0 {
		return
	}
	used := make(map[uint64]bool, 5)
	for rank := 0; rank < 5; rank++ {
		var bestPC uint64
		var bestCount uint64
		for pc, count := range counts {
			if used[pc] || count <= bestCount {
				continue
			}
			bestPC = pc
			bestCount = count
		}
		if bestCount == 0 {
			return
		}
		used[bestPC] = true
		b.ReportMetric(float64(bestCount)/float64(b.N), fmt.Sprintf("%s_%x/op", prefix, bestPC))
	}
}

func dumpJITFallbackTrace(tb testing.TB, jit *riscv.JIT, limit int) {
	tb.Helper()
	top := jit.FallbackTraceTop(limit)
	fmt.Fprintf(os.Stderr, "\nlazy JIT interpreter fallback trace: top=%d total_pcs=%d\n", len(top), jit.NoJITSize())
	for i, ent := range top {
		exec := "exec=<none>"
		if ent.InExec {
			exec = fmt.Sprintf("exec=[0x%x,0x%x)", ent.RegionBegin, ent.RegionEnd)
		}
		insn := ent.FetchFault
		if insn == "" && ent.IsRVC {
			insn = fmt.Sprintf("half=0x%04x %s", ent.Half, ent.Disasm)
		} else if insn == "" {
			insn = fmt.Sprintf("word=0x%08x %s", ent.Word, ent.Disasm)
		}
		fmt.Fprintf(os.Stderr, "  %2d pc=0x%x count=%d reason=%s %s %s\n",
			i+1, ent.PC, ent.Count, ent.Reason, exec, insn)
	}
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
		t.Fatal("attempted 0 instructions")
	}
	t.Logf("coremark: attempted %d instructions (exit %d)", insns, code)
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
		t.Fatal("attempted 0 instructions")
	}
	t.Logf("dhrystone: attempted %d instructions (exit %d)", insns, code)
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
