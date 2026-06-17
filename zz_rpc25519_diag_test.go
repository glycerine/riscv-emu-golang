package riscv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiagRPC25519HangLog5(t *testing.T) {
	root := "/Users/jaten/rpc25519"
	elfPath := filepath.Join(root, "rpc25519.test")
	if _, err := os.Stat(elfPath); err != nil {
		t.Skipf("rpc25519.test not available: %v", err)
	}
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	mem, err := NewGuestMemory(Size16GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	elf, err := LoadELF(mem, elfPath)
	if err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	jos := NewJea9Linux(Jea9LinuxOptions{
		ClockMode:         Jea9ClockIdleJump,
		ClockPolicy:       ClockPolicyOnlyDeadlockAdvances,
		MonotonicStartNS:  1,
		RealtimeOffsetNS:  946684800000000000 - 1,
		InstructionBudget: 100_000,
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:              Jea9SchedulerRoundRobin,
			MinQuantumRetired: 100_000,
			MaxQuantumRetired: 100_000,
		},
		Trace:             true,
		Stdout:            &stdout,
		Stderr:            &stderr,
		AllowAllHostFiles: true,
	})
	args := []string{"rpc25519.test"}
	if err := jos.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     args,
		Env:      os.Environ(),
		ExecPath: args[0],
	}); err != nil {
		t.Fatal(err)
	}
	cleanup := InstallJea9Linux(cpu, jos)
	defer cleanup()

	netpollHits := 0
	deadline := time.Now().Add(90 * time.Second)
	for slice := 0; slice < 20_000; slice++ {
		err := jos.Run(cpu)
		if errors.Is(err, ErrJea9LinuxBudget) {
			if cpu.PC() >= 0x533a0 && cpu.PC() < 0x53520 {
				netpollHits++
			} else if netpollHits > 0 {
				netpollHits = 0
			}
			if slice%250 == 0 || netpollHits == 8 {
				t.Logf("slice=%d pc=0x%x insn=%s ic=%d syscalls=%d budget=%d netpollHits=%d",
					slice, cpu.PC(), disasmGuestInsn(t, &cpu.mem, cpu.PC()),
					cpu.RiscvInstrBegun(), jos.SyscallCount(), jos.BudgetYields(), netpollHits)
			}
			if netpollHits >= 8 {
				t.Log("\n" + formatRPC25519Diag(cpu, jos, stdout.String(), stderr.String()))
				return
			}
			if time.Now().After(deadline) {
				t.Log("\n" + formatRPC25519Diag(cpu, jos, stdout.String(), stderr.String()))
				t.Fatal("diagnostic deadline")
			}
			continue
		}
		if err != nil {
			var ex *ExitError
			if errors.As(err, &ex) {
				t.Logf("exit code=%d", ex.Code)
				t.Log("\n" + formatRPC25519Diag(cpu, jos, stdout.String(), stderr.String()))
				return
			}
			t.Fatalf("Run: %v\n%s", err, formatRPC25519Diag(cpu, jos, stdout.String(), stderr.String()))
		}
		t.Log("\n" + formatRPC25519Diag(cpu, jos, stdout.String(), stderr.String()))
		return
	}
	t.Log("\n" + formatRPC25519Diag(cpu, jos, stdout.String(), stderr.String()))
	t.Fatal("slice limit")
}

func formatRPC25519Diag(cpu *CPU, jos *Jea9Linux, stdout, stderr string) string {
	var b strings.Builder
	trace := jos.TraceSnapshot()
	fmt.Fprintf(&b, "pc=0x%x insn=%s currentTID=%d tid=%d contexts=%d ic=%d monotonic=%d blocked=%v blockedDeadline=%v/%d syscalls=%d schedules=%d\n",
		cpu.PC(), diagDisasm(&cpu.mem, cpu.PC()), jos.currentTID, jos.tid, len(jos.contexts),
		cpu.RiscvInstrBegun(), jos.monotonicNS, jos.blocked, jos.blockedHasDeadline, jos.blockedUntil,
		jos.SyscallCount(), len(trace.Schedule))
	fmt.Fprintf(&b, "stdout=%q\nstderr=%q\n", limitDiagString(stdout, 4096), limitDiagString(stderr, 4096))
	for _, tid := range jos.contextOrder {
		ctx := jos.contexts[tid]
		if ctx == nil {
			continue
		}
		fmt.Fprintf(&b, "ctx tid=%d state=%s wait=%d fd=%d addr=0x%x eventAddr=0x%x maxEvents=%d deadline=%d hasDeadline=%v pc=0x%x trap=%v trapPC=0x%x resumePC=0x%x ret=%d\n",
			tid, ctx.state, ctx.waitKind, ctx.waitFD, ctx.waitAddr, ctx.waitEventAddr, ctx.waitMaxEvents,
			ctx.waitDeadlineNS, ctx.waitHasDeadline, ctx.snapshot.pc, ctx.syscallTrap.active,
			ctx.syscallTrap.trapPC, ctx.syscallTrap.resumePC, int64(ctx.snapshot.x[10]))
	}
	for _, c := range jos.TopSyscallCounts(16) {
		fmt.Fprintf(&b, "syscall count num=%d count=%d\n", c.Num, c.Count)
	}
	for _, c := range jos.TopSyscallPCCounts(16) {
		fmt.Fprintf(&b, "syscall pc=0x%x count=%d\n", c.PC, c.Count)
	}
	for fd, f := range jos.fds {
		switch f.kind {
		case jea9LinuxFDEpoll:
			fmt.Fprintf(&b, "fd=%d kind=epoll regs=%d order=%v\n", fd, len(f.epoll.registrations), f.epoll.order)
			for _, watched := range f.epoll.order {
				reg, ok := f.epoll.registrations[watched]
				if !ok {
					continue
				}
				fmt.Fprintf(&b, "  watch fd=%d reg.events=0x%x data=0x%x ready=0x%x kind=%s\n",
					watched, reg.events, reg.data, jos.fdReadyEvents(watched)&reg.events, fdKindString(jos.fds[watched].kind))
			}
		case jea9LinuxFDSocket:
			fmt.Fprintf(&b, "fd=%d kind=socket flags=0x%x listener=%v conn=%v pending=%d readbuf=%d eof=%v local=%v peer=%v ready=0x%x\n",
				fd, f.flags, f.tcpListener != nil, f.tcpConn != nil, len(f.socketPending),
				len(f.socketReadBuf), f.socketEOF, f.socketLocal, f.socketPeer, jos.fdReadyEvents(fd))
		case jea9LinuxFDEventfd:
			fmt.Fprintf(&b, "fd=%d kind=eventfd counter=%d ready=0x%x\n", fd, f.eventfdCounter, jos.fdReadyEvents(fd))
		}
	}
	start := len(trace.Syscalls) - 40
	if start < 0 {
		start = 0
	}
	for i := start; i < len(trace.Syscalls); i++ {
		s := trace.Syscalls[i]
		fmt.Fprintf(&b, "syscall[%d] tid=%d pc=0x%x num=%d ret=%d disp=%d args=%x\n",
			i, s.TID, s.PC, s.Num, s.Ret, s.Disposition, s.Args)
	}
	start = len(trace.Schedule) - 20
	if start < 0 {
		start = 0
	}
	for i := start; i < len(trace.Schedule); i++ {
		s := trace.Schedule[i]
		fmt.Fprintf(&b, "schedule[%d] event=%s tid=%d next=%d fromPC=0x%x nextPC=0x%x ns=%d ins=%d reason=%s\n",
			i, s.Event, s.TID, s.NextTID, s.FromPC, s.NextPC, s.MonotonicNS, s.RiscvInstrBegun, s.Reason)
	}
	return b.String()
}

func diagDisasm(mem *GuestMemory, pc uint64) string {
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

func fdKindString(kind jea9LinuxFDKind) string {
	switch kind {
	case jea9LinuxFDStdin:
		return "stdin"
	case jea9LinuxFDStdout:
		return "stdout"
	case jea9LinuxFDStderr:
		return "stderr"
	case jea9LinuxFDRandom:
		return "random"
	case jea9LinuxFDFile:
		return "file"
	case jea9LinuxFDEventfd:
		return "eventfd"
	case jea9LinuxFDEpoll:
		return "epoll"
	case jea9LinuxFDPipeRead:
		return "pipe-read"
	case jea9LinuxFDPipeWrite:
		return "pipe-write"
	case jea9LinuxFDHostFile:
		return "host-file"
	case jea9LinuxFDDir:
		return "dir"
	case jea9LinuxFDSocket:
		return "socket"
	default:
		return "unknown"
	}
}

func limitDiagString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimRight(s[:max], "\n") + "\n..."
}
