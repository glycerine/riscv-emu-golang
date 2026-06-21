package riscv

import (
	"os"
	"testing"
)

const (
	jea9TestSysKill          = uint64(129)
	jea9TestSysTkill         = uint64(130)
	jea9TestSysTgkill        = uint64(131)
	jea9TestSysSigaltstack   = uint64(132)
	jea9TestSysRtSigaction   = uint64(134)
	jea9TestSysRtSigprocmask = uint64(135)
	jea9TestSysRtSigreturn   = uint64(139)

	jea9TestSIGSEGV = uint64(11)
	jea9TestSIGUSR1 = uint64(10)
	jea9TestSIGURG  = uint64(23)

	jea9TestSIGBlock   = uint64(0)
	jea9TestSIGUnblock = uint64(1)
	jea9TestSIGSetmask = uint64(2)

	jea9TestSAOnstack = uint64(0x08000000)
)

func writeSignalAction(t *testing.T, mem *GuestMemory, addr, handler, flags, restorer, mask uint64) {
	t.Helper()
	writeGuest64(t, mem, addr, handler)
	writeGuest64(t, mem, addr+8, flags)
	writeGuest64(t, mem, addr+16, restorer)
	writeGuest64(t, mem, addr+24, mask)
}

func requireSignalAction(t *testing.T, mem *GuestMemory, addr, handler, flags, restorer, mask uint64) {
	t.Helper()
	if got := readGuest64(t, mem, addr); got != handler {
		t.Fatalf("signal action handler = 0x%x, want 0x%x", got, handler)
	}
	if got := readGuest64(t, mem, addr+8); got != flags {
		t.Fatalf("signal action flags = 0x%x, want 0x%x", got, flags)
	}
	if got := readGuest64(t, mem, addr+16); got != restorer {
		t.Fatalf("signal action restorer = 0x%x, want 0x%x", got, restorer)
	}
	if got := readGuest64(t, mem, addr+24); got != mask {
		t.Fatalf("signal action mask = 0x%x, want 0x%x", got, mask)
	}
}

func writeSignalSet(t *testing.T, mem *GuestMemory, addr, mask uint64) {
	t.Helper()
	writeGuest64(t, mem, addr, mask)
}

func signalBit(sig uint64) uint64 {
	return uint64(1) << (sig - 1)
}

func installSignalHandler(t *testing.T, cpu *CPU, mem *GuestMemory, sig, handler, flags uint64) {
	t.Helper()
	action := uint64(0x5000 + sig*0x40)
	writeSignalAction(t, mem, action, handler, flags, 0xfeed0000+sig, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, sig, action, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_RtSigactionInstallAndReadBack(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	action := uint64(0x5000)
	old := uint64(0x5100)
	writeSignalAction(t, mem, action, 0x123456, jea9TestSAOnstack, 0x7777, signalBit(jea9TestSIGURG))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, jea9TestSIGUSR1, action, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction install disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	got := j.signalActions[jea9TestSIGUSR1]
	if got.handler != 0x123456 || got.flags != jea9TestSAOnstack || got.restorer != 0x7777 || got.mask != signalBit(jea9TestSIGURG) {
		t.Fatalf("stored action = %+v", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, jea9TestSIGUSR1, 0, old, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction readback disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	requireSignalAction(t, mem, old, 0x123456, jea9TestSAOnstack, 0x7777, signalBit(jea9TestSIGURG))
}

func TestJea9Linux_RtSigactionAcceptsLinuxRiscv64Layout(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const (
		action  = uint64(0x5000)
		handler = uint64(0x12345678)
		flags   = uint64(0x4) | jea9TestSAOnstack
		mask    = uint64(0xfffffffffffffff7)
	)
	writeGuest64(t, mem, action, handler)
	writeGuest64(t, mem, action+8, flags)
	writeGuest64(t, mem, action+16, mask)
	writeGuest64(t, mem, action+24, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, jea9TestSIGUSR1, action, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction linux layout disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	got := j.signalActions[jea9TestSIGUSR1]
	if got.handler != handler || got.flags != flags || got.mask != mask || got.restorer != 0 {
		t.Fatalf("linux layout action = %+v, want handler=0x%x flags=0x%x mask=0x%x restorer=0", got, handler, flags, mask)
	}
}

func TestJea9Linux_RtSigactionErrorsAndDefaultReadback(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	old := uint64(0x5200)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, jea9TestSIGURG, 0, old, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction default readback disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	requireSignalAction(t, mem, old, 0, 0, 0, 0)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, 0, 0, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction bad signal disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, jea9TestSIGUSR1, 0, 0, 16); d != NoteHandled {
		t.Fatalf("rt_sigaction bad sigset size disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
}

func TestJea9Linux_RtSigprocmaskBlockUnblockPerThread(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	maskAddr := uint64(0x6000)
	writeSignalSet(t, mem, maskAddr, signalBit(jea9TestSIGUSR1))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGBlock, maskAddr, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigprocmask block disposition = %v, want NoteHandled", d)
	}
	parent := j.contexts[j.pid]
	if parent.signalMask != signalBit(jea9TestSIGUSR1) {
		t.Fatalf("parent signal mask = 0x%x", parent.signalMask)
	}

	childTID := cloneJea9LinuxThread(t, cpu, j, 0x8a0000, 0, 0, 0, jea9TestCloneThreadFlags)
	if j.contexts[childTID].signalMask != parent.signalMask {
		t.Fatalf("child did not inherit signal mask: 0x%x != 0x%x", j.contexts[childTID].signalMask, parent.signalMask)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, childTID)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGUnblock, maskAddr, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigprocmask child unblock disposition = %v, want NoteHandled", d)
	}
	if j.contexts[childTID].signalMask != 0 {
		t.Fatalf("child signal mask = 0x%x, want 0", j.contexts[childTID].signalMask)
	}
	if parent.signalMask != signalBit(jea9TestSIGUSR1) {
		t.Fatalf("parent signal mask changed to 0x%x", parent.signalMask)
	}
}

func TestJea9Linux_RtSigprocmaskOldMaskAndErrors(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	maskAddr := uint64(0x6000)
	oldAddr := uint64(0x6010)
	writeSignalSet(t, mem, maskAddr, signalBit(jea9TestSIGUSR1)|signalBit(jea9TestSIGURG))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGSetmask, maskAddr, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigprocmask setmask disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGBlock, 0, oldAddr, 8); d != NoteHandled {
		t.Fatalf("rt_sigprocmask old mask disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := readGuest64(t, mem, oldAddr); got != signalBit(jea9TestSIGUSR1)|signalBit(jea9TestSIGURG) {
		t.Fatalf("old mask = 0x%x", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, 99, maskAddr, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigprocmask invalid how disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGBlock, maskAddr, 0, 16); d != NoteHandled {
		t.Fatalf("rt_sigprocmask invalid size disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
}

func TestJea9Linux_SignalPendingWhileMasked(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGUSR1, handler, 0)
	maskAddr := uint64(0x6000)
	writeSignalSet(t, mem, maskAddr, signalBit(jea9TestSIGUSR1))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGBlock, maskAddr, 0, 8); d != NoteHandled {
		t.Fatalf("block disposition = %v", d)
	}
	cpu.SetPC(0x2222)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if cpu.PC() != 0x2222 {
		t.Fatalf("masked signal changed PC to 0x%x", cpu.PC())
	}
	if got := len(j.contexts[j.pid].pendingSignals); got != 1 {
		t.Fatalf("pending signal count = %d, want 1", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigprocmask, jea9TestSIGUnblock, maskAddr, 0, 8); d != NoteHandled {
		t.Fatalf("unblock disposition = %v", d)
	}
	if cpu.PC() != handler || cpu.Reg(10) != jea9TestSIGUSR1 {
		t.Fatalf("unmasked delivery PC=0x%x a0=%d, want handler 0x%x sig %d", cpu.PC(), cpu.Reg(10), handler, jea9TestSIGUSR1)
	}
}

func TestJea9Linux_SigaltstackInstallAndUse(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	stack := uint64(0x7000)
	writeGuest64(t, mem, stack, 0x9000)
	writeGuest64(t, mem, stack+8, 0)
	writeGuest64(t, mem, stack+16, 0x2000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, stack, 0); d != NoteHandled {
		t.Fatalf("sigaltstack disposition = %v, want NoteHandled", d)
	}
	ctx := j.contexts[j.pid]
	if ctx.sigaltSP != 0x9000 || ctx.sigaltSize != 0x2000 || ctx.sigaltFlags != 0 {
		t.Fatalf("altstack = {0x%x,0x%x,0x%x}", ctx.sigaltSP, ctx.sigaltFlags, ctx.sigaltSize)
	}

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGUSR1, handler, jea9TestSAOnstack)
	cpu.SetReg(2, 0x30000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	sp := cpu.Reg(2)
	if sp < 0x9000 || sp >= 0x9000+0x2000 {
		t.Fatalf("handler SP = 0x%x, want on altstack [0x9000,0xb000)", sp)
	}
	if cpu.PC() != handler {
		t.Fatalf("handler PC = 0x%x, want 0x%x", cpu.PC(), handler)
	}
}

func TestJea9Linux_SigaltstackReadbackAndInvalidFlags(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	stack := uint64(0x7000)
	old := uint64(0x7040)
	writeGuest64(t, mem, stack, 0x9000)
	writeGuest64(t, mem, stack+8, 0)
	writeGuest64(t, mem, stack+16, 0x2000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, stack, 0); d != NoteHandled {
		t.Fatalf("sigaltstack install disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, 0, old); d != NoteHandled {
		t.Fatalf("sigaltstack readback disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := readGuest64(t, mem, old); got != 0x9000 {
		t.Fatalf("old altstack sp = 0x%x", got)
	}
	if got := readGuest64(t, mem, old+8); got != 0 {
		t.Fatalf("old altstack flags = 0x%x", got)
	}
	if got := readGuest64(t, mem, old+16); got != 0x2000 {
		t.Fatalf("old altstack size = 0x%x", got)
	}

	writeGuest64(t, mem, stack+8, 0x40)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, stack, 0); d != NoteHandled {
		t.Fatalf("sigaltstack invalid flags disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
}

func TestJea9Linux_SigaltstackAcceptsAutoDisarm(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	stack := uint64(0x7000)
	old := uint64(0x7040)
	writeGuest64(t, mem, stack, 0x9000)
	writeGuest64(t, mem, stack+8, jea9LinuxSSAutoDisarm)
	writeGuest64(t, mem, stack+16, 0x2000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, stack, 0); d != NoteHandled {
		t.Fatalf("sigaltstack autodisarm disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	ctx := j.contexts[j.pid]
	if ctx.sigaltSP != 0x9000 || ctx.sigaltSize != 0x2000 || ctx.sigaltFlags != jea9LinuxSSAutoDisarm {
		t.Fatalf("altstack = {0x%x,0x%x,0x%x}", ctx.sigaltSP, ctx.sigaltFlags, ctx.sigaltSize)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, 0, old); d != NoteHandled {
		t.Fatalf("sigaltstack autodisarm readback disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := readGuest64(t, mem, old+8); got != jea9LinuxSSAutoDisarm {
		t.Fatalf("old altstack flags = 0x%x, want SS_AUTODISARM", got)
	}
}

func TestJea9Linux_SigaltstackIgnoresPadding(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	stack := uint64(0x7000)
	writeGuest64(t, mem, stack, 0x9000)
	if f := mem.Store32(stack+8, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store32(stack+12, 0xdeadbeef); f != nil {
		t.Fatal(f)
	}
	writeGuest64(t, mem, stack+16, 0x2000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSigaltstack, stack, 0); d != NoteHandled {
		t.Fatalf("sigaltstack dirty padding disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_TgkillTargetsTidAndKillTargetsProcess(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4444)
	installSignalHandler(t, cpu, mem, jea9TestSIGURG, handler, 0)
	childTID := cloneJea9LinuxThread(t, cpu, j, 0x8b0000, 0, 0, 0, jea9TestCloneThreadFlags)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, childTID, jea9TestSIGURG); d != NoteHandled {
		t.Fatalf("tgkill child disposition = %v", d)
	}
	child := j.contexts[childTID]
	if child.snapshot.pc != handler || child.snapshot.x[10] != jea9TestSIGURG {
		t.Fatalf("child signal snapshot pc=0x%x a0=%d", child.snapshot.pc, child.snapshot.x[10])
	}

	cpu.SetPC(0x3333)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysKill, j.pid, jea9TestSIGURG); d != NoteHandled {
		t.Fatalf("kill disposition = %v", d)
	}
	if cpu.PC() != handler {
		t.Fatalf("process signal PC = 0x%x, want handler 0x%x", cpu.PC(), handler)
	}
}

func TestJea9Linux_RtSigreturnRestoresRegisters(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGUSR1, handler, 0)
	cpu.SetPC(0x2222)
	cpu.SetReg(2, 0x30000)
	cpu.SetReg(5, 0x1234)
	cpu.SetFReg(3, 0x9988776655443322)
	cpu.SetFCSR(0x1f)
	j.contexts[j.pid].signalMask = signalBit(jea9TestSIGURG)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	frame := cpu.Reg(2)
	cpu.SetPC(0x9999)
	cpu.SetReg(5, 0xabcd)
	cpu.SetFReg(3, 0)
	cpu.SetFCSR(0)
	j.contexts[j.pid].signalMask = 0
	cpu.SetReg(2, frame)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigreturn); d != NoteHandled {
		t.Fatalf("rt_sigreturn disposition = %v", d)
	}
	if cpu.PC() != 0x2222 || cpu.Reg(2) != 0x30000 || cpu.Reg(5) != 0x1234 {
		t.Fatalf("restored pc=0x%x sp=0x%x x5=0x%x", cpu.PC(), cpu.Reg(2), cpu.Reg(5))
	}
	if cpu.FReg(3) != 0x9988776655443322 || cpu.FCSR() != 0x1f {
		t.Fatalf("restored f3=0x%x fcsr=0x%x", cpu.FReg(3), cpu.FCSR())
	}
	if j.contexts[j.pid].signalMask != signalBit(jea9TestSIGURG) {
		t.Fatalf("restored mask = 0x%x", j.contexts[j.pid].signalMask)
	}
}

func TestJea9Linux_RtSigreturnFindsFrameAboveCurrentSP(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGUSR1, handler, 0)
	cpu.SetPC(0x2222)
	cpu.SetReg(2, 0x30000)
	cpu.SetReg(5, 0x1234)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	frame := cpu.Reg(2)
	cpu.SetPC(0x9999)
	cpu.SetReg(5, 0xabcd)
	cpu.SetReg(2, frame-32)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigreturn); d != NoteHandled {
		t.Fatalf("rt_sigreturn disposition = %v", d)
	}
	if cpu.PC() != 0x2222 || cpu.Reg(2) != 0x30000 || cpu.Reg(5) != 0x1234 {
		t.Fatalf("restored pc=0x%x sp=0x%x x5=0x%x", cpu.PC(), cpu.Reg(2), cpu.Reg(5))
	}
}

func TestJea9Linux_RtSigreturnRestoresModifiedLinuxUContext(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGUSR1, handler, 0)
	cpu.SetPC(0x2222)
	cpu.SetReg(2, 0x30000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	ucontext := cpu.Reg(12)
	const regsOff = jea9LinuxUContextMContextOff
	writeGuest64(t, mem, ucontext+regsOff, 0x7777)
	writeGuest64(t, mem, ucontext+regsOff+16, 0x8888)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigreturn); d != NoteHandled {
		t.Fatalf("rt_sigreturn disposition = %v", d)
	}
	if cpu.PC() != 0x7777 || cpu.Reg(2) != 0x8888 {
		t.Fatalf("rt_sigreturn restored pc=0x%x sp=0x%x, want modified ucontext", cpu.PC(), cpu.Reg(2))
	}
}

func TestJea9Linux_SignalWithoutRestorerUsesSyntheticRtSigreturn(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	action := uint64(0x5000)
	handler := uint64(0x4000)
	writeGuest64(t, mem, action, handler)
	writeGuest64(t, mem, action+8, jea9LinuxSASiginfo)
	writeGuest64(t, mem, action+16, signalBit(jea9TestSIGURG))
	writeGuest64(t, mem, action+24, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRtSigaction, jea9TestSIGUSR1, action, 0, 8); d != NoteHandled {
		t.Fatalf("rt_sigaction disposition = %v", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	restorer := cpu.Reg(1)
	if restorer == 0 || restorer != j.signalRestorer {
		t.Fatalf("restorer ra=0x%x cached=0x%x, want synthetic restorer", restorer, j.signalRestorer)
	}
	insn, f := mem.Fetch32(restorer)
	if f != nil {
		t.Fatalf("fetch restorer addi: %v", f)
	}
	if want := encodeJea9LinuxADDI(17, 0, int32(jea9LinuxSysRtSigreturn)); insn != want {
		t.Fatalf("restorer first insn = 0x%x, want 0x%x", insn, want)
	}
	ecall, f := mem.Fetch32(restorer + 4)
	if f != nil {
		t.Fatalf("fetch restorer ecall: %v", f)
	}
	if ecall != 0x00000073 {
		t.Fatalf("restorer second insn = 0x%x, want ecall", ecall)
	}
	if r := (&cpu.mem).FindExecRegion(restorer); r == nil || !r.Contains(restorer) {
		t.Fatalf("synthetic restorer 0x%x is not executable metadata", restorer)
	}
}

func TestJea9Linux_DefaultSignalIgnoredAndFaultForwarded(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	cpu.SetPC(0x3333)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("default tgkill disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if cpu.PC() != 0x3333 {
		t.Fatalf("default signal changed PC to 0x%x", cpu.PC())
	}
	if d := j.Handle(cpu, Note{Cause: CauseLoadFault, Tval: 0x44, PC: 0x3333}); d != NoteForward {
		t.Fatalf("fault without handler disposition = %v, want NoteForward", d)
	}
}

func TestJea9Linux_SiginfoForUserSignalAndSegv(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGUSR1, handler, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysTgkill, j.pid, j.tid, jea9TestSIGUSR1); d != NoteHandled {
		t.Fatalf("tgkill disposition = %v", d)
	}
	siginfo := cpu.Reg(11)
	requireGuest32(t, mem, siginfo, uint32(jea9TestSIGUSR1))
	requireGuest32(t, mem, siginfo+8, uint32(jea9LinuxSignalCodeUser))
	requireGuest32(t, mem, siginfo+16, uint32(j.pid))
	requireGuest32(t, mem, siginfo+20, 0)

	installSignalHandler(t, cpu, mem, jea9TestSIGSEGV, handler, 0)
	cpu.SetPC(0x5555)
	if d := j.Handle(cpu, Note{Cause: CauseLoadFault, Tval: 0xdeadbeef, PC: 0x5555}); d != NoteHandled {
		t.Fatalf("fault signal disposition = %v, want NoteHandled", d)
	}
	if cpu.PC() != handler || cpu.Reg(10) != jea9TestSIGSEGV {
		t.Fatalf("segv delivery pc=0x%x sig=%d", cpu.PC(), cpu.Reg(10))
	}
	segvInfo := cpu.Reg(11)
	requireGuest32(t, mem, segvInfo, uint32(jea9TestSIGSEGV))
	if got := readGuest64(t, mem, segvInfo+24); got != 0xdeadbeef {
		t.Fatalf("segv fault addr = 0x%x, want 0xdeadbeef", got)
	}
}

func TestJea9Linux_SignalFrameHasLinuxRiscv64UContext(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	handler := uint64(0x4000)
	installSignalHandler(t, cpu, mem, jea9TestSIGSEGV, handler, 0)
	cpu.SetPC(0x5555)
	cpu.SetReg(1, 0x1111)
	cpu.SetReg(2, 0x30000)
	cpu.SetReg(5, 0x5550)
	cpu.SetReg(6, 0x6660)
	cpu.SetReg(10, 0xaaaa)
	if d := j.Handle(cpu, Note{Cause: CauseLoadFault, Tval: 0xdeadbeef, PC: 0x5555}); d != NoteHandled {
		t.Fatalf("fault signal disposition = %v, want NoteHandled", d)
	}

	siginfo := cpu.Reg(11)
	if got := readGuest64(t, mem, siginfo+16); got != 0xdeadbeef {
		t.Fatalf("linux siginfo si_addr = 0x%x, want 0xdeadbeef", got)
	}

	const linuxRiscv64UContextMContextOff = uint64(176)
	regs := cpu.Reg(12) + linuxRiscv64UContextMContextOff
	for _, tc := range []struct {
		name string
		off  uint64
		want uint64
	}{
		{name: "pc", off: 0, want: 0x5555},
		{name: "ra", off: 8, want: 0x1111},
		{name: "sp", off: 16, want: 0x30000},
		{name: "t0", off: 40, want: 0x5550},
		{name: "t1", off: 48, want: 0x6660},
		{name: "a0", off: 80, want: 0xaaaa},
	} {
		if got := readGuest64(t, mem, regs+tc.off); got != tc.want {
			t.Fatalf("ucontext %s = 0x%x, want 0x%x", tc.name, got, tc.want)
		}
	}
}

func TestJea9Linux_Phase11SignalELFFixtures(t *testing.T) {
	for _, path := range []string{
		"testvectors/jea9linux/elf/sigaction_basic.elf",
		"testvectors/jea9linux/elf/sigmask_pending.elf",
		"testvectors/jea9linux/elf/sigaltstack_frame.elf",
		"testvectors/jea9linux/elf/tgkill_self.elf",
		"testvectors/jea9linux/elf/sigsegv_null.elf",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			mem, err := NewGuestMemory(Size64MB)
			if err != nil {
				t.Fatal(err)
			}
			defer mem.Free()
			elf, err := LoadELFBytes(mem, data)
			if err != nil {
				t.Fatalf("LoadELFBytes: %v", err)
			}
			cpu := NewCPU(*mem)
			cpu.SetPC(elf.Entry)
			cpu.SetReg(2, 0x03F00000)
			j := NewJea9Linux(Jea9LinuxOptions{})
			if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{StackTop: 0x03F00000}); err != nil {
				t.Fatalf("InitELFStack: %v", err)
			}
			code, err := RunWithJea9LinuxInterp(cpu, j)
			if err != nil {
				t.Fatalf("RunWithJea9LinuxInterp: %v", err)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
		})
	}
}
