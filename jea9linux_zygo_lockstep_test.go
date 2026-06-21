//go:build zygo_lockstep

package riscv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	zygoLockstepCoarseBudget = uint64(1000)
	zygoLockstepMidBudget    = uint64(100)
	zygoLockstepFineBudget   = uint64(3)
	zygoLockstepMidAtIC      = uint64(208_000)
	zygoLockstepFineAtIC     = uint64(208_600)
	zygoLockstepFineUntilIC  = uint64(210_000)
	zygoLockstepProgressIC   = uint64(10_000)
	zygoLockstepProgramArg   = "(defn fib [x] (cond (== x 0) 0 (== x 1) 1 (+ (fib (- x 1)) (fib (- x 2))))) (println (fib 10))"
)

type zygoLockstepBudgetPlan struct {
	coarse  uint64
	mid     uint64
	fine    uint64
	windows []zygoLockstepBudgetWindow
}

type zygoLockstepBudgetWindow struct {
	midAt     uint64
	fineAt    uint64
	fineUntil uint64
}

type zygoLockstepSide struct {
	name    string
	cpu     *CPU
	mem     *GuestMemory
	os      *Jea9Linux
	jit     *JIT
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	cleanup func()
}

func TestJea9Linux_ZygoFib10_LazyJITLockstepAdaptive(t *testing.T) {
	data, err := os.ReadFile("bench/zygo.elf")
	if err != nil {
		t.Skipf("bench/zygo.elf not found: %v", err)
	}

	budgetPlan := zygoLockstepInstructionBudgetPlan(t)
	budgetPlan.coarse = 2_330_000

	maxIC := zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_MAX_IC", 0)
	interp := newZygoLockstepSide(t, "interp", data, false, budgetPlan.coarse)
	defer interp.close()
	jit := newZygoLockstepSide(t, "jit", data, true, budgetPlan.coarse)
	defer jit.close()

	currentBudget := budgetPlan.coarse
	nextProgressIC := uint64(1) // zygoLockstepProgressIC
	for quantum := 0; ; quantum++ {

		nextBudget := budgetPlan.budgetForIC(jit.cpu.RiscvInstrBegun())

		if quantum > 0 {
			nextBudget = 10
		}
		if nextBudget != currentBudget {
			jit.os.instructionBudget = nextBudget
			interp.os.instructionBudget = nextBudget
			fmt.Fprintf(os.Stderr, "zygo lockstep: switching budget %d -> %d at ic=%d quantum=%d\n",
				currentBudget, nextBudget, jit.cpu.RiscvInstrBegun(), quantum)
			currentBudget = nextBudget
		}

		jitBeforeIC := jit.cpu.RiscvInstrBegun()
		interpBeforeIC := interp.cpu.RiscvInstrBegun()
		jitErr := jit.os.RunJIT(jit.cpu, jit.jit)
		interpErr := interp.os.Run(interp.cpu)
		jitDelta := jit.cpu.RiscvInstrBegun() - jitBeforeIC
		interpDelta := interp.cpu.RiscvInstrBegun() - interpBeforeIC

		jitKind := zygoLockstepErrKind(jitErr)
		interpKind := zygoLockstepErrKind(interpErr)
		if jitKind != interpKind {
			t.Fatalf("quantum %d error mismatch:\n%s\njit err=%s (%v)\ninterp err=%s (%v)",
				quantum, zygoLockstepSummary(jit, interp, jitDelta, interpDelta),
				jitKind, jitErr, interpKind, interpErr)
		}
		if diff := zygoLockstepCompare(jit, interp); diff != "" {
			t.Fatalf("quantum %d state mismatch after %s:\n%s\n%s",
				quantum, jitKind, zygoLockstepSummary(jit, interp, jitDelta, interpDelta), diff)
		}
		if nextProgressIC != 0 && jit.cpu.RiscvInstrBegun() >= nextProgressIC {
			fmt.Fprintf(os.Stderr, "zygo lockstep: ic=%d quantum=%d budget=%d pc=0x%x tid=%d syscalls=%d nanosleep=%d\n",
				jit.cpu.RiscvInstrBegun(), quantum, currentBudget, jit.cpu.pc, jit.os.currentTID, jit.os.SyscallCount(), jit.os.nanosleepCount)
			for nextProgressIC != 0 && jit.cpu.RiscvInstrBegun() >= nextProgressIC {
				nextProgressIC += zygoLockstepProgressIC
			}
		}
		if maxIC != 0 && jit.cpu.RiscvInstrBegun() >= maxIC {
			t.Logf("lockstep stopped at requested max ic=%d quantum=%d syscalls=%d budget_yields=%d",
				jit.cpu.RiscvInstrBegun(), quantum, jit.os.SyscallCount(), jit.os.BudgetYields())
			return
		}

		switch {
		case strings.HasPrefix(jitKind, "exit:"):
			if jit.stdout.String() != "55\n" {
				t.Fatalf("zygo stdout = %q, want %q", jit.stdout.String(), "55\n")
			}
			fmt.Printf("%v\n", jit.stdout.String())
			t.Logf("lockstep completed at quantum=%d ic=%d syscalls=%d budget_yields=%d",
				quantum, jit.cpu.RiscvInstrBegun(), jit.os.SyscallCount(), jit.os.BudgetYields())
			return
		case jitKind == "budget":
			continue
		default:
			t.Fatalf("quantum %d unexpected terminal result %s:\n%s",
				quantum, jitKind, zygoLockstepSummary(jit, interp, jitDelta, interpDelta))
		}
	}
}

func TestJea9Linux_ZygoLockstep_SchedulerIndependentOfBudget(t *testing.T) {
	data, err := os.ReadFile("bench/zygo.elf")
	if err != nil {
		t.Skipf("bench/zygo.elf not found: %v", err)
	}
	events := zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_SCHED_EVENTS", 4)
	if events == 0 {
		t.Fatalf("invalid ZYGO_LOCKSTEP_SCHED_EVENTS=0")
	}
	cfg := Jea9LinuxSchedulerConfig{
		MinQuantumRetired: 1000,
		MaxQuantumRetired: 1000,
	}

	interpWant := zygoLockstepScheduleTrace(t, data, false, 5000, events, cfg)
	for _, budget := range []uint64{3, 97, 1000} {
		got := zygoLockstepScheduleTrace(t, data, false, budget, events, cfg)
		if !reflect.DeepEqual(got, interpWant) {
			t.Fatalf("zygo interpreter schedule trace differs for host budget %d:\ngot  %+v\nwant %+v", budget, got, interpWant)
		}
	}

	jitWant := zygoLockstepScheduleTrace(t, data, true, 5000, events, cfg)
	for _, budget := range []uint64{3, 97, 1000} {
		got := zygoLockstepScheduleTrace(t, data, true, budget, events, cfg)
		if !reflect.DeepEqual(got, jitWant) {
			t.Fatalf("zygo lazy JIT schedule trace differs for host budget %d:\ngot  %+v\nwant %+v", budget, got, jitWant)
		}
	}
	if !reflect.DeepEqual(jitWant, interpWant) {
		t.Fatalf("zygo lazy JIT schedule trace differs from interpreter:\njit    %+v\ninterp %+v", jitWant, interpWant)
	}
}

func TestJea9Linux_ZygoLockstep_ChaosReplay(t *testing.T) {
	data, err := os.ReadFile("bench/zygo.elf")
	if err != nil {
		t.Skipf("bench/zygo.elf not found: %v", err)
	}
	events := zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_CHAOS_EVENTS", 4)
	if events == 0 {
		t.Fatalf("invalid ZYGO_LOCKSTEP_CHAOS_EVENTS=0")
	}
	cfg := Jea9LinuxSchedulerConfig{
		Mode:                       Jea9SchedulerChaos,
		MinQuantumRetired:          1000,
		MaxQuantumRetired:          1000,
		LowPriorityNumerator:       0,
		LowPriorityDenominator:     10,
		PriorityShuffleMinRetired:  2000,
		PriorityShuffleMaxRetired:  2000,
		ChaosWindowProbNumerator:   1,
		ChaosWindowProbDenominator: 1,
		ChaosWindowMaxNS:           50_000,
		ChaosBudgetNumerator:       1,
		ChaosBudgetDenominator:     5,
	}
	first := zygoLockstepScheduleTrace(t, data, true, 5000, events, cfg)
	second := zygoLockstepScheduleTrace(t, data, true, 97, events, cfg)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("zygo chaos replay schedule traces differ:\nfirst  %+v\nsecond %+v", first, second)
	}
}

func zygoLockstepInstructionBudgetPlan(t *testing.T) zygoLockstepBudgetPlan {
	t.Helper()
	if raw := os.Getenv("ZYGO_LOCKSTEP_BUDGET"); raw != "" {
		budget := zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_BUDGET", 0)
		return zygoLockstepBudgetPlan{coarse: budget, fine: budget}
	}
	return zygoLockstepBudgetPlan{
		coarse:  zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_COARSE_BUDGET", zygoLockstepCoarseBudget),
		mid:     zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_MID_BUDGET", zygoLockstepMidBudget),
		fine:    zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_FINE_BUDGET", zygoLockstepFineBudget),
		windows: zygoLockstepBudgetWindows(t),
	}
}

func (p zygoLockstepBudgetPlan) budgetForIC(ic uint64) uint64 {
	for _, w := range p.windows {
		if w.fineAt != 0 && ic >= w.fineAt && (w.fineUntil == 0 || ic < w.fineUntil) {
			return p.fine
		}
	}
	for _, w := range p.windows {
		if w.midAt != 0 && ic >= w.midAt && (w.fineAt == 0 || ic < w.fineAt) {
			return p.mid
		}
	}
	return p.coarse
}

func zygoLockstepBudgetWindows(t *testing.T) []zygoLockstepBudgetWindow {
	t.Helper()
	raw := os.Getenv("ZYGO_LOCKSTEP_WINDOWS")
	if raw == "" {
		return []zygoLockstepBudgetWindow{{
			midAt:     zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_MID_AT", zygoLockstepMidAtIC),
			fineAt:    zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_FINE_AT", zygoLockstepFineAtIC),
			fineUntil: zygoLockstepEnvUint(t, "ZYGO_LOCKSTEP_FINE_UNTIL", zygoLockstepFineUntilIC),
		}}
	}
	if raw == "none" {
		return nil
	}
	parts := strings.Split(raw, ",")
	windows := make([]zygoLockstepBudgetWindow, 0, len(parts))
	for _, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if len(fields) != 3 {
			t.Fatalf("invalid ZYGO_LOCKSTEP_WINDOWS entry %q; want mid:fine:until", part)
		}
		midAt := zygoLockstepParseUint(t, "ZYGO_LOCKSTEP_WINDOWS", fields[0])
		fineAt := zygoLockstepParseUint(t, "ZYGO_LOCKSTEP_WINDOWS", fields[1])
		fineUntil := zygoLockstepParseUint(t, "ZYGO_LOCKSTEP_WINDOWS", fields[2])
		if midAt == 0 || fineAt == 0 || fineAt < midAt || fineUntil != 0 && fineUntil < fineAt {
			t.Fatalf("invalid ZYGO_LOCKSTEP_WINDOWS entry %q; want 0 < mid <= fine <= until", part)
		}
		windows = append(windows, zygoLockstepBudgetWindow{
			midAt:     midAt,
			fineAt:    fineAt,
			fineUntil: fineUntil,
		})
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].midAt < windows[j].midAt })
	return windows
}

func zygoLockstepEnvUint(t *testing.T, key string, def uint64) uint64 {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v := zygoLockstepParseUint(t, key, raw)
	if v == 0 && key != "ZYGO_LOCKSTEP_FINE_AT" {
		t.Fatalf("invalid %s=%q", key, raw)
	}
	return v
}

func zygoLockstepParseUint(t *testing.T, key, raw string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatalf("invalid %s=%q", key, raw)
	}
	return v
}

func zygoLockstepScheduleTrace(t *testing.T, elfData []byte, useJIT bool, budget, events uint64, cfg Jea9LinuxSchedulerConfig) []Jea9LinuxScheduleTraceEntry {
	t.Helper()
	side := newZygoLockstepSide(t, "sched-trace", elfData, useJIT, budget)
	defer side.close()
	side.os.traceEnabled = true
	side.os.schedulerConfig = cfg
	side.os.normalizeSchedulerConfig()
	side.os.nextScheduleAtRetired = 0
	side.os.currentQuantumRetired = 0
	side.os.nextPriorityShuffleRetired = 0
	side.os.monotonicNS = 1000
	side.os.installNextSchedulerQuantum(side.cpu)

	for uint64(len(side.os.TraceSnapshot().Schedule)) < events {
		var err error
		if useJIT {
			err = side.os.RunJIT(side.cpu, side.jit)
		} else {
			err = side.os.Run(side.cpu)
		}
		switch zygoLockstepErrKind(err) {
		case "budget":
			continue
		case "exit:0":
			t.Fatalf("zygo exited before %d schedule events; got %d", events, len(side.os.TraceSnapshot().Schedule))
		default:
			t.Fatalf("zygo schedule trace run useJIT=%v budget=%d err=%v", useJIT, budget, err)
		}
	}
	trace := side.os.TraceSnapshot().Schedule
	return append([]Jea9LinuxScheduleTraceEntry(nil), trace[:events]...)
}

func newZygoLockstepSide(t *testing.T, name string, elfData []byte, useJIT bool, budget uint64) *zygoLockstepSide {
	t.Helper()
	mem, err := NewGuestMemory(Size16GB)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		t.Fatalf("%s LoadELFBytes: %v", name, err)
	}
	cpu := NewCPU(*mem)
	opts := Jea9LinuxOptions{
		ClockMode:         Jea9ClockIdleJump,
		MonotonicStartNS:  1,
		NSPerInstruction:  1,
		InstructionBudget: budget,
		PID:               1,
		TID:               1,
	}
	side := &zygoLockstepSide{
		name: name,
		cpu:  cpu,
		mem:  mem,
		os:   NewJea9Linux(opts),
	}
	side.os.stdout = &side.stdout
	side.os.stderr = &side.stderr
	args := []string{"bench/zygo.elf", "-c", zygoLockstepProgramArg}
	if err := side.os.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     args,
		ExecPath: args[0],
	}); err != nil {
		side.close()
		t.Fatalf("%s InitELFStack: %v", name, err)
	}
	if useJIT {
		side.jit = NewSandboxJIT()
		side.jit.AutoAOT = false
		side.cleanup = InstallJea9LinuxJIT(cpu, side.jit, side.os)
	} else {
		side.cleanup = InstallJea9Linux(cpu, side.os)
	}
	return side
}

func (s *zygoLockstepSide) close() {
	if s == nil {
		return
	}
	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
	if s.jit != nil {
		s.jit.Close()
		s.jit = nil
	}
	if s.mem != nil {
		s.mem.Free()
		s.mem = nil
	}
}

func zygoLockstepErrKind(err error) string {
	if err == nil {
		return "nil"
	}
	var ex *ExitError
	if errors.As(err, &ex) {
		return fmt.Sprintf("exit:%d", ex.Code)
	}
	if errors.Is(err, ErrJea9LinuxBudget) {
		return "budget"
	}
	if errors.Is(err, ErrJea9LinuxBlocked) {
		return "blocked"
	}
	return fmt.Sprintf("%T:%v", err, err)
}

func zygoLockstepCompare(jit, interp *zygoLockstepSide) string {
	var out strings.Builder
	zygoLockstepCompareCPU(&out, "current CPU", jit.cpu, interp.cpu)
	zygoLockstepCompareOS(&out, jit.os, interp.os)
	if os.Getenv("ZYGO_LOCKSTEP_MEM") != "" {
		zygoLockstepCompareMemory(&out, jit, interp)
	}
	if jit.stdout.String() != interp.stdout.String() {
		fmt.Fprintf(&out, "stdout mismatch: jit=%q interp=%q\n", jit.stdout.String(), interp.stdout.String())
	}
	if jit.stderr.String() != interp.stderr.String() {
		fmt.Fprintf(&out, "stderr mismatch: jit=%q interp=%q\n", jit.stderr.String(), interp.stderr.String())
	}
	return out.String()
}

func zygoLockstepCompareCPU(out *strings.Builder, label string, jit, interp *CPU) {
	if jit.pc != interp.pc {
		fmt.Fprintf(out, "%s pc mismatch: jit=0x%x (%s) interp=0x%x (%s)\n",
			label, jit.pc, zygoLockstepDisasm(&jit.mem, jit.pc), interp.pc, zygoLockstepDisasm(&interp.mem, interp.pc))
	}
	if jit.riscvInstrBegun != interp.riscvInstrBegun {
		fmt.Fprintf(out, "%s IC mismatch: jit=%d interp=%d\n", label, jit.riscvInstrBegun, interp.riscvInstrBegun)
	}
	for i := 0; i < 32; i++ {
		if jit.x[i] != interp.x[i] {
			fmt.Fprintf(out, "%s x[%d] mismatch: jit=0x%x interp=0x%x\n", label, i, jit.x[i], interp.x[i])
			break
		}
	}
	for i := 0; i < 32; i++ {
		if jit.f[i] != interp.f[i] {
			fmt.Fprintf(out, "%s f[%d] mismatch: jit=0x%x interp=0x%x\n", label, i, jit.f[i], interp.f[i])
			break
		}
	}
	// Native FP currently does not propagate sticky fflags into fcsr. This
	// diagnostic is chasing instruction-accounting and scheduler divergence, so
	// ignore fcsr-only differences here.
	if jit.resvAddr != interp.resvAddr || jit.resvValid != interp.resvValid {
		fmt.Fprintf(out, "%s reservation mismatch: jit=(0x%x,%v) interp=(0x%x,%v)\n",
			label, jit.resvAddr, jit.resvValid, interp.resvAddr, interp.resvValid)
	}
	if jit.mtvec != interp.mtvec || jit.mepc != interp.mepc || jit.mcause != interp.mcause ||
		jit.mstatus != interp.mstatus || jit.mtval != interp.mtval {
		fmt.Fprintf(out, "%s trap CSR mismatch: jit=(mtvec=0x%x mepc=0x%x mcause=0x%x mstatus=0x%x mtval=0x%x) interp=(mtvec=0x%x mepc=0x%x mcause=0x%x mstatus=0x%x mtval=0x%x)\n",
			label, jit.mtvec, jit.mepc, jit.mcause, jit.mstatus, jit.mtval,
			interp.mtvec, interp.mepc, interp.mcause, interp.mstatus, interp.mtval)
	}
	if jit.ExitCode != interp.ExitCode {
		fmt.Fprintf(out, "%s exit code mismatch: jit=%d interp=%d\n", label, jit.ExitCode, interp.ExitCode)
	}
}

func zygoLockstepCompareOS(out *strings.Builder, jit, interp *Jea9Linux) {
	if jit.currentTID != interp.currentTID || jit.tid != interp.tid || jit.nextTID != interp.nextTID {
		fmt.Fprintf(out, "scheduler tid mismatch: jit=(current=%d tid=%d next=%d) interp=(current=%d tid=%d next=%d)\n",
			jit.currentTID, jit.tid, jit.nextTID, interp.currentTID, interp.tid, interp.nextTID)
	}
	if jit.vm.brk != interp.vm.brk || jit.vm.minBrk != interp.vm.minBrk || jit.vm.mmapNext != interp.vm.mmapNext {
		fmt.Fprintf(out, "vm range mismatch: jit=(brk=0x%x min=0x%x next=0x%x) interp=(brk=0x%x min=0x%x next=0x%x)\n",
			jit.vm.brk, jit.vm.minBrk, jit.vm.mmapNext, interp.vm.brk, interp.vm.minBrk, interp.vm.mmapNext)
	}
	if jit.monotonicNS != interp.monotonicNS {
		fmt.Fprintf(out, "monotonicNS mismatch: jit=%d interp=%d\n", jit.monotonicNS, interp.monotonicNS)
	}
	if jit.clockPolicy != interp.clockPolicy ||
		jit.clockFixedAdvanceNS != interp.clockFixedAdvanceNS ||
		jit.clockPRNGMinNS != interp.clockPRNGMinNS ||
		jit.clockPRNGMaxNS != interp.clockPRNGMaxNS {
		fmt.Fprintf(out, "clock policy mismatch: jit=(policy=%s fixed=%d min=%d max=%d) interp=(policy=%s fixed=%d min=%d max=%d)\n",
			jit.clockPolicy, jit.clockFixedAdvanceNS, jit.clockPRNGMinNS, jit.clockPRNGMaxNS,
			interp.clockPolicy, interp.clockFixedAdvanceNS, interp.clockPRNGMinNS, interp.clockPRNGMaxNS)
	}
	if jit.schedulerConfig != interp.schedulerConfig ||
		jit.currentQuantumRetired != interp.currentQuantumRetired ||
		jit.nextScheduleAtRetired != interp.nextScheduleAtRetired ||
		jit.nextPriorityShuffleRetired != interp.nextPriorityShuffleRetired {
		fmt.Fprintf(out, "scheduler quantum/config mismatch: jit=(cfg=%+v q=%d next=%d shuffle=%d) interp=(cfg=%+v q=%d next=%d shuffle=%d)\n",
			jit.schedulerConfig, jit.currentQuantumRetired, jit.nextScheduleAtRetired, jit.nextPriorityShuffleRetired,
			interp.schedulerConfig, interp.currentQuantumRetired, interp.nextScheduleAtRetired, interp.nextPriorityShuffleRetired)
	}
	if jit.schedDraws != interp.schedDraws || jit.schedEventID != interp.schedEventID ||
		!bytes.Equal(jit.schedPRNGSnapshot, interp.schedPRNGSnapshot) {
		fmt.Fprintf(out, "scheduler PRNG mismatch: jit=(events=%d draws=%d state=%x) interp=(events=%d draws=%d state=%x)\n",
			jit.schedEventID, jit.schedDraws, jit.schedPRNGSnapshot,
			interp.schedEventID, interp.schedDraws, interp.schedPRNGSnapshot)
	}
	if jit.chaosActive != interp.chaosActive ||
		jit.chaosStartNS != interp.chaosStartNS ||
		jit.chaosUntilNS != interp.chaosUntilNS ||
		jit.chaosBlockedNS != interp.chaosBlockedNS {
		fmt.Fprintf(out, "chaos scheduler mismatch: jit=(active=%v start=%d until=%d blocked=%d) interp=(active=%v start=%d until=%d blocked=%d)\n",
			jit.chaosActive, jit.chaosStartNS, jit.chaosUntilNS, jit.chaosBlockedNS,
			interp.chaosActive, interp.chaosStartNS, interp.chaosUntilNS, interp.chaosBlockedNS)
	}
	if jit.budgetYields != interp.budgetYields {
		fmt.Fprintf(out, "budgetYields mismatch: jit=%d interp=%d\n", jit.budgetYields, interp.budgetYields)
	}
	if jit.syscallCount != interp.syscallCount || jit.syscallCounts != interp.syscallCounts ||
		jit.nanosleepCount != interp.nanosleepCount || jit.nanosleepTotalNS != interp.nanosleepTotalNS ||
		jit.nanosleepMaxNS != interp.nanosleepMaxNS {
		fmt.Fprintf(out, "syscall counters mismatch: jit=(total=%d nanosleep=%d ns=%d max=%d) interp=(total=%d nanosleep=%d ns=%d max=%d)\n",
			jit.syscallCount, jit.nanosleepCount, jit.nanosleepTotalNS, jit.nanosleepMaxNS,
			interp.syscallCount, interp.nanosleepCount, interp.nanosleepTotalNS, interp.nanosleepMaxNS)
	}
	if !reflect.DeepEqual(jit.contextOrder, interp.contextOrder) {
		fmt.Fprintf(out, "contextOrder mismatch: jit=%v interp=%v\n", jit.contextOrder, interp.contextOrder)
	}
	for _, tid := range zygoLockstepContextIDs(jit, interp) {
		jctx := jit.contexts[tid]
		ictx := interp.contexts[tid]
		if jctx == nil || ictx == nil {
			fmt.Fprintf(out, "context %d presence mismatch: jit=%v interp=%v\n", tid, jctx != nil, ictx != nil)
			continue
		}
		zygoLockstepCompareContext(out, tid, jctx, ictx)
	}
}

func zygoLockstepCompareMemory(out *strings.Builder, jit, interp *zygoLockstepSide) {
	pages := make(map[uint64]bool, len(jit.os.vm.pages)+len(interp.os.vm.pages))
	for page := range jit.os.vm.pages {
		pages[page] = true
	}
	for page := range interp.os.vm.pages {
		pages[page] = true
	}
	ids := make([]uint64, 0, len(pages))
	for page := range pages {
		ids = append(ids, page)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, page := range ids {
		jstate := jit.os.vm.pages[page]
		istate := interp.os.vm.pages[page]
		if jstate != istate {
			fmt.Fprintf(out, "vm page 0x%x state mismatch: jit=0x%x interp=0x%x\n", page, jstate, istate)
			return
		}
	}

	jraw := jit.mem.RawSlice()
	iraw := interp.mem.RawSlice()
	jlen := uint64(len(jraw))
	ilen := uint64(len(iraw))
	for _, page := range ids {
		state := jit.os.vm.pages[page] | interp.os.vm.pages[page]
		if state&jea9LinuxPageMapped == 0 {
			continue
		}
		begin := page * GuestPageSize
		end := begin + GuestPageSize
		if end < begin || end > jlen || end > ilen {
			fmt.Fprintf(out, "vm page 0x%x outside memory bounds\n", page)
			return
		}
		js := jraw[begin:end]
		is := iraw[begin:end]
		if bytes.Equal(js, is) {
			continue
		}
		for i := range js {
			if js[i] != is[i] {
				addr := begin + uint64(i)
				fmt.Fprintf(out, "memory mismatch at 0x%x page=0x%x: jit=0x%02x interp=0x%02x\n",
					addr, page, js[i], is[i])
				return
			}
		}
	}
}

func zygoLockstepCompareContext(out *strings.Builder, tid uint64, jit, interp *jea9LinuxContext) {
	if jit.state != interp.state || jit.waitKind != interp.waitKind ||
		jit.waitAddr != interp.waitAddr || jit.waitDeadlineNS != interp.waitDeadlineNS ||
		jit.waitHasDeadline != interp.waitHasDeadline || jit.waitFD != interp.waitFD ||
		jit.waitEventAddr != interp.waitEventAddr || jit.waitMaxEvents != interp.waitMaxEvents {
		fmt.Fprintf(out, "context %d wait/state mismatch: jit=(state=%v kind=%d addr=0x%x deadline=%d has=%v fd=%d event=0x%x max=%d) interp=(state=%v kind=%d addr=0x%x deadline=%d has=%v fd=%d event=0x%x max=%d)\n",
			tid, jit.state, jit.waitKind, jit.waitAddr, jit.waitDeadlineNS, jit.waitHasDeadline, jit.waitFD, jit.waitEventAddr, jit.waitMaxEvents,
			interp.state, interp.waitKind, interp.waitAddr, interp.waitDeadlineNS, interp.waitHasDeadline, interp.waitFD, interp.waitEventAddr, interp.waitMaxEvents)
	}
	if jit.clearChildTID != interp.clearChildTID || jit.robustList != interp.robustList ||
		jit.robustListLen != interp.robustListLen || jit.signalMask != interp.signalMask ||
		len(jit.pendingSignals) != len(interp.pendingSignals) ||
		jit.sigaltSP != interp.sigaltSP || jit.sigaltSize != interp.sigaltSize || jit.sigaltFlags != interp.sigaltFlags ||
		jit.syscallTrap != interp.syscallTrap || jit.schedPriority != interp.schedPriority {
		fmt.Fprintf(out, "context %d metadata mismatch\n", tid)
	}
	if diff := zygoLockstepSnapshotDiff("saved CPU", jit.snapshot, interp.snapshot); diff != "" {
		fmt.Fprintf(out, "context %d snapshot mismatch:\n%s", tid, diff)
	}
}

func zygoLockstepSnapshotDiff(label string, jit, interp jea9LinuxCPUSnapshot) string {
	var out strings.Builder
	if jit.pc != interp.pc {
		fmt.Fprintf(&out, "%s pc mismatch: jit=0x%x interp=0x%x\n", label, jit.pc, interp.pc)
	}
	for i := 0; i < 32; i++ {
		if jit.x[i] != interp.x[i] {
			fmt.Fprintf(&out, "%s x[%d] mismatch at pc=0x%x: jit=0x%x interp=0x%x\n",
				label, i, jit.pc, jit.x[i], interp.x[i])
			break
		}
	}
	for i := 0; i < 32; i++ {
		if jit.f[i] != interp.f[i] {
			fmt.Fprintf(&out, "%s f[%d] mismatch at pc=0x%x: jit=0x%x interp=0x%x\n",
				label, i, jit.pc, jit.f[i], interp.f[i])
			break
		}
	}
	if jit.mtvec != interp.mtvec || jit.mepc != interp.mepc || jit.mcause != interp.mcause ||
		jit.mstatus != interp.mstatus || jit.mtval != interp.mtval {
		fmt.Fprintf(&out, "%s control state mismatch\n", label)
	}
	return out.String()
}

func zygoLockstepContextIDs(a, b *Jea9Linux) []uint64 {
	seen := make(map[uint64]bool)
	for tid := range a.contexts {
		seen[tid] = true
	}
	for tid := range b.contexts {
		seen[tid] = true
	}
	ids := make([]uint64, 0, len(seen))
	for tid := range seen {
		ids = append(ids, tid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func zygoLockstepSummary(jit, interp *zygoLockstepSide, jitDelta, interpDelta uint64) string {
	return fmt.Sprintf(
		"jit:    pc=0x%x insn=%s tid=%d ic=%d delta=%d syscalls=%d nanosleep=%d budget_yields=%d stdout=%q\ninterp: pc=0x%x insn=%s tid=%d ic=%d delta=%d syscalls=%d nanosleep=%d budget_yields=%d stdout=%q",
		jit.cpu.pc, zygoLockstepDisasm(&jit.cpu.mem, jit.cpu.pc), jit.os.currentTID, jit.cpu.RiscvInstrBegun(), jitDelta, jit.os.SyscallCount(), jit.os.nanosleepCount, jit.os.BudgetYields(), jit.stdout.String(),
		interp.cpu.pc, zygoLockstepDisasm(&interp.cpu.mem, interp.cpu.pc), interp.os.currentTID, interp.cpu.RiscvInstrBegun(), interpDelta, interp.os.SyscallCount(), interp.os.nanosleepCount, interp.os.BudgetYields(), interp.stdout.String(),
	)
}

func zygoLockstepDisasm(mem *GuestMemory, pc uint64) string {
	half, fault := mem.Fetch16(pc)
	if fault != nil {
		return fault.Error()
	}
	if half&0x3 != 0x3 {
		return DisasmRVC(half)
	}
	word, fault := mem.Fetch32(pc)
	if fault != nil {
		return fault.Error()
	}
	return DisasmRV32(pc, word)
}
