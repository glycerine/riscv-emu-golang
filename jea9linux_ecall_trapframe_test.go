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
	j := NewJea9Linux(Jea9LinuxOptions{ClockMode: Jea9ClockIdleJump})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const (
		trapPC  = uint64(0x6000)
		reqAddr = uint64(0x7000)
	)
	parent := j.ensureScheduler(cpu)
	childTID := parent.tid + 1
	j.contexts[childTID] = &jea9LinuxContext{
		tid:   childTID,
		state: jea9LinuxContextRunnable,
		snapshot: jea9LinuxCPUSnapshot{
			pc: 0x8000,
		},
	}
	j.contextOrder = append(j.contextOrder, childTID)

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
	if d != NoteHandled {
		t.Fatalf("Handle disposition = %v, want NoteHandled after switching to runnable child", d)
	}
	if j.currentTID != childTID {
		t.Fatalf("current tid = %d, want child tid %d", j.currentTID, childTID)
	}
	ctx := parent
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

func TestJea9Linux_LoadContextDoesNotReplayStaleEcallTrapframe(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{PID: 123, TID: 123})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	ctx := j.ensureScheduler(cpu)
	ctx.state = jea9LinuxContextRunnable
	ctx.snapshot.pc = 0x2000
	ctx.syscallTrap = jea9LinuxEcallTrapFrame{
		active:   true,
		trapPC:   0x1000,
		resumePC: 0x1004,
		cause:    CauseEcallU,
		insnLen:  4,
	}
	cpu.SetPC(0xdead)

	if !j.loadContext(cpu, ctx.tid) {
		t.Fatal("loadContext failed")
	}
	if got := cpu.PC(); got != 0x2000 {
		t.Fatalf("loaded CPU pc = 0x%x, want saved normal PC 0x2000", got)
	}
	if ctx.syscallTrap.active {
		t.Fatal("stale ECALL trapframe was not cleared")
	}
}
