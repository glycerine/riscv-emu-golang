package riscv

import "testing"

func TestJea9Linux_HandledEcallResumesFromTrapframe(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{PID: 123, TID: 123})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const trapPC = uint64(0x5000)
	cpu.SetPC(trapPC)
	cpu.SetReg(17, jea9LinuxSysGetpid)

	d := j.Handle(cpu, Note{Cause: CauseEcallU, PC: trapPC, InsnLen: 4})
	if d != NoteHandled {
		t.Fatalf("Handle disposition = %v, want NoteHandled", d)
	}
	if got := cpu.Reg(10); got != 123 {
		t.Fatalf("a0 = %d, want pid 123", got)
	}
	if got := cpu.PC(); got != trapPC+4 {
		t.Fatalf("cpu.PC() = 0x%x, want resume PC 0x%x", got, trapPC+4)
	}
}

func TestJea9Linux_BlockingEcallKeepsTrapframeUntilWake(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{ClockMode: Jea9ClockManual})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const (
		trapPC  = uint64(0x6000)
		reqAddr = uint64(0x7000)
	)
	if f := mem.Store64(reqAddr, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(reqAddr+8, 1); f != nil {
		t.Fatal(f)
	}
	cpu.SetPC(trapPC)
	cpu.SetReg(17, jea9LinuxSysNanosleep)
	cpu.SetReg(10, reqAddr)

	d := j.Handle(cpu, Note{Cause: CauseEcallU, PC: trapPC, InsnLen: 4})
	if d != NoteExit {
		t.Fatalf("Handle disposition = %v, want NoteExit while all contexts are blocked", d)
	}
	ctx := j.contexts[j.currentTID]
	if ctx == nil {
		t.Fatal("current context was not created")
	}
	if ctx.state != jea9LinuxContextWaiting || ctx.waitKind != jea9LinuxWaitNanosleep {
		t.Fatalf("context state=%v waitKind=%d, want waiting nanosleep", ctx.state, ctx.waitKind)
	}
	if !ctx.syscallTrap.active {
		t.Fatal("waiting context did not keep an active ECALL trapframe")
	}
	if ctx.syscallTrap.trapPC != trapPC || ctx.syscallTrap.resumePC != trapPC+4 {
		t.Fatalf("trapframe = {trap:0x%x resume:0x%x}, want {trap:0x%x resume:0x%x}",
			ctx.syscallTrap.trapPC, ctx.syscallTrap.resumePC, trapPC, trapPC+4)
	}
	if ctx.snapshot.pc != trapPC {
		t.Fatalf("waiting snapshot pc = 0x%x, want trap PC 0x%x", ctx.snapshot.pc, trapPC)
	}

	j.markRunnable(ctx.tid, 0)
	if ctx.syscallTrap.active {
		t.Fatal("woken context still has active ECALL trapframe")
	}
	if ctx.snapshot.pc != trapPC+4 {
		t.Fatalf("woken snapshot pc = 0x%x, want resume PC 0x%x", ctx.snapshot.pc, trapPC+4)
	}
	if !j.loadContext(cpu, ctx.tid) {
		t.Fatal("loadContext failed for woken context")
	}
	if cpu.PC() != trapPC+4 {
		t.Fatalf("loaded CPU pc = 0x%x, want resume PC 0x%x", cpu.PC(), trapPC+4)
	}
}
