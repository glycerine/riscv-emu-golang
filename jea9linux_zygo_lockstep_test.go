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
	zygoLockstepBudget     = uint64(1000)
	zygoLockstepProgramArg = "(defn fib [x] (cond (== x 0) 0 (== x 1) 1 (+ (fib (- x 1)) (fib (- x 2))))) (println (fib 10))"
)

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

func TestJea9Linux_ZygoFib10_LazyJITLockstepBudget1000(t *testing.T) {
	data, err := os.ReadFile("bench/zygo.elf")
	if err != nil {
		t.Skipf("bench/zygo.elf not found: %v", err)
	}

	budget := zygoLockstepInstructionBudget(t)
	interp := newZygoLockstepSide(t, "interp", data, false, budget)
	defer interp.close()
	jit := newZygoLockstepSide(t, "jit", data, true, budget)
	defer jit.close()

	for quantum := 0; ; quantum++ {
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

		switch {
		case strings.HasPrefix(jitKind, "exit:"):
			if jit.stdout.String() != "55\n" {
				t.Fatalf("zygo stdout = %q, want %q", jit.stdout.String(), "55\n")
			}
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

func zygoLockstepInstructionBudget(t *testing.T) uint64 {
	t.Helper()
	raw := os.Getenv("ZYGO_LOCKSTEP_BUDGET")
	if raw == "" {
		return zygoLockstepBudget
	}
	budget, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || budget == 0 {
		t.Fatalf("invalid ZYGO_LOCKSTEP_BUDGET=%q", raw)
	}
	return budget
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
		side.jit = NewJIT()
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
		jit.sigaltSP != interp.sigaltSP || jit.sigaltSize != interp.sigaltSize || jit.sigaltFlags != interp.sigaltFlags {
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
	if jit.resvAddr != interp.resvAddr || jit.resvValid != interp.resvValid ||
		jit.mtvec != interp.mtvec || jit.mepc != interp.mepc || jit.mcause != interp.mcause ||
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
