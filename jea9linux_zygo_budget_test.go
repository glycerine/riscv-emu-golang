package riscv

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fibonacci is a classic simple driver of lots of recursive stack alloc/dealloc.
const zygoBudgetFib10Program = "(defn fib [x] (cond (== x 0) 0 (== x 1) 1 (+ (fib (- x 1)) (fib (- x 2))))) (println (fib 10))"

func TestJea9Linux_ZygoFib10_InterpreterBudget1000(t *testing.T) {
	elfData, err := os.ReadFile("bench/zygo.elf")
	if err != nil {
		t.Skipf("bench/zygo.elf not found: %v", err)
	}

	var stdout, stderr bytes.Buffer
	mem, err := NewGuestMemory(Size16GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	elf, err := LoadELFBytes(mem, elfData)
	if err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	jos := NewJea9Linux(Jea9LinuxOptions{
		ClockMode:         Jea9ClockIdleJump,
		MonotonicStartNS:  1,
		NSPerInstruction:  1,
		InstructionBudget: 1000,
		Trace:             true,
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	args := []string{"bench/zygo.elf", "-c", zygoBudgetFib10Program}
	if err := jos.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     args,
		ExecPath: args[0],
	}); err != nil {
		t.Fatal(err)
	}

	code, err := RunWithJea9Linux(cpu, jos)
	if err != nil {
		t.Fatalf("RunWithJea9Linux: %v\nstderr:\n%s\ntrace:\n%s", err, limitZygoBudgetString(stderr.String(), 2048), formatZygoBudgetTrace(cpu, jos))
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s\ntrace:\n%s", code, limitZygoBudgetString(stderr.String(), 2048), formatZygoBudgetTrace(cpu, jos))
	}
	if got := stdout.String(); got != "55\n" {
		t.Fatalf("stdout = %q, want %q\nstderr:\n%s", got, "55\n", limitZygoBudgetString(stderr.String(), 2048))
	}
}

func formatZygoBudgetTrace(cpu *CPU, jos *Jea9Linux) string {
	var b strings.Builder
	trace := jos.TraceSnapshot()
	fmt.Fprintf(&b, "currentTID=%d tid=%d contexts=%d syscalls=%d schedules=%d\n",
		jos.currentTID, jos.tid, len(jos.contexts), len(trace.Syscalls), len(trace.Schedule))
	if schedLock, f := cpu.mem.Load64(0x8510e0); f == nil {
		fmt.Fprintf(&b, "sched.lock@0x8510e0=0x%016x\n", schedLock)
	} else {
		fmt.Fprintf(&b, "sched.lock@0x8510e0 fault=%v\n", f)
	}
	for _, tid := range jos.contextOrder {
		ctx := jos.contexts[tid]
		if ctx == nil {
			continue
		}
		fmt.Fprintf(&b, "ctx tid=%d state=%s wait=%d addr=0x%x deadline=%d hasDeadline=%v pc=0x%x trap=%v trapPC=0x%x resumePC=0x%x\n",
			tid, ctx.state, ctx.waitKind, ctx.waitAddr, ctx.waitDeadlineNS, ctx.waitHasDeadline,
			ctx.snapshot.pc, ctx.syscallTrap.active, ctx.syscallTrap.trapPC, ctx.syscallTrap.resumePC)
	}
	for _, c := range jos.TopSyscallCounts(8) {
		fmt.Fprintf(&b, "syscall count num=%d count=%d\n", c.Num, c.Count)
	}
	for _, c := range jos.TopSyscallPCCounts(8) {
		fmt.Fprintf(&b, "syscall pc=0x%x count=%d\n", c.PC, c.Count)
	}
	for i, s := range trace.Schedule {
		if s.RiscvInstrBegun >= 370000 && s.RiscvInstrBegun <= 376000 {
			fmt.Fprintf(&b, "schedule-near[%d] event=%s tid=%d next=%d fromPC=0x%x nextPC=0x%x ns=%d ins=%d\n",
				i, s.Event, s.TID, s.NextTID, s.FromPC, s.NextPC, s.MonotonicNS, s.RiscvInstrBegun)
		}
	}
	start := len(trace.Schedule) - 16
	if start < 0 {
		start = 0
	}
	for i := start; i < len(trace.Schedule); i++ {
		s := trace.Schedule[i]
		fmt.Fprintf(&b, "schedule[%d] event=%s tid=%d next=%d fromPC=0x%x nextPC=0x%x ns=%d ins=%d\n",
			i, s.Event, s.TID, s.NextTID, s.FromPC, s.NextPC, s.MonotonicNS, s.RiscvInstrBegun)
	}
	start = len(trace.Syscalls) - 16
	if start < 0 {
		start = 0
	}
	for i := start; i < len(trace.Syscalls); i++ {
		s := trace.Syscalls[i]
		fmt.Fprintf(&b, "syscall[%d] tid=%d pc=0x%x num=%d ret=%d disp=%d args=%x\n",
			i, s.TID, s.PC, s.Num, s.Ret, s.Disposition, s.Args)
	}
	return b.String()
}

func limitZygoBudgetString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimRight(s[:max], "\n") + "\n..."
}
