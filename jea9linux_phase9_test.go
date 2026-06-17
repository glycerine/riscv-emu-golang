package riscv

import (
	"errors"
	"os"
	"testing"
)

const (
	jea9TestSysSetTidAddress    = uint64(96)
	jea9TestSysFutex            = uint64(98)
	jea9TestSysSetRobustList    = uint64(99)
	jea9TestSysSchedGetAffinity = uint64(123)
	jea9TestSysSchedYield       = uint64(124)
	jea9TestSysClone            = uint64(220)
	jea9TestSysFutexTime64      = uint64(422)

	jea9TestFutexWait        = uint64(0)
	jea9TestFutexWake        = uint64(1)
	jea9TestFutexPrivateFlag = uint64(128)

	jea9TestCloneVM            = uint64(0x00000100)
	jea9TestCloneFS            = uint64(0x00000200)
	jea9TestCloneFiles         = uint64(0x00000400)
	jea9TestCloneSighand       = uint64(0x00000800)
	jea9TestCloneThread        = uint64(0x00010000)
	jea9TestCloneSysvsem       = uint64(0x00040000)
	jea9TestCloneSetTLS        = uint64(0x00080000)
	jea9TestCloneParentSetTID  = uint64(0x00100000)
	jea9TestCloneChildClearTID = uint64(0x00200000)
	jea9TestCloneChildSetTID   = uint64(0x01000000)
)

const jea9TestCloneThreadFlags = jea9TestCloneVM |
	jea9TestCloneFS |
	jea9TestCloneFiles |
	jea9TestCloneSighand |
	jea9TestCloneThread |
	jea9TestCloneSysvsem

func requireGuest32(t *testing.T, mem *GuestMemory, addr uint64, want uint32) {
	t.Helper()
	got, f := mem.Load32(addr)
	if f != nil {
		t.Fatalf("Load32(0x%x): %v", addr, f)
	}
	if got != want {
		t.Fatalf("guest32[0x%x] = %d, want %d", addr, got, want)
	}
}

func requireCurrentTID(t *testing.T, j *Jea9Linux, want uint64) {
	t.Helper()
	if got := j.currentTID; got != want {
		t.Fatalf("current tid = %d, want %d", got, want)
	}
	if got := j.tid; got != want {
		t.Fatalf("visible tid = %d, want %d", got, want)
	}
}

func cloneJea9LinuxThread(t *testing.T, cpu *CPU, j *Jea9Linux, stack, tls, ptid, ctid uint64, flags uint64) uint64 {
	t.Helper()
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClone, flags, stack, ptid, tls, ctid); d != NoteHandled {
		t.Fatalf("clone disposition = %v, want NoteHandled", d)
	}
	tid := cpu.Reg(10)
	if tid <= j.pid {
		t.Fatalf("clone returned tid %d, want > pid %d", tid, j.pid)
	}
	return tid
}

func TestJea9Linux_CloneParentAndChildContext(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	parentPC := uint64(0x1234)
	childStack := uint64(0x880000)
	tls := uint64(0x770000)
	ptid := uint64(0x5000)
	ctid := uint64(0x5008)
	flags := jea9TestCloneThreadFlags |
		jea9TestCloneSetTLS |
		jea9TestCloneParentSetTID |
		jea9TestCloneChildSetTID |
		jea9TestCloneChildClearTID

	cpu.SetPC(parentPC)
	cpu.SetReg(2, 0x660000)
	cpu.SetReg(4, 0x550000)
	cpu.SetReg(5, 0xfeedface)
	childTID := cloneJea9LinuxThread(t, cpu, j, childStack, tls, ptid, ctid, flags)

	requireCurrentTID(t, j, j.pid)
	if got := cpu.PC(); got != parentPC {
		t.Fatalf("parent PC = 0x%x, want post-ecall PC 0x%x", got, parentPC)
	}
	if got := cpu.Reg(10); got != childTID {
		t.Fatalf("parent a0 = %d, want child tid %d", got, childTID)
	}
	requireGuest32(t, mem, ptid, uint32(childTID))
	requireGuest32(t, mem, ctid, uint32(childTID))

	if got := len(j.contexts); got != 2 {
		t.Fatalf("context count = %d, want 2", got)
	}
	child := j.contexts[childTID]
	if child == nil {
		t.Fatalf("missing child context for tid %d", childTID)
	}
	if child.state != jea9LinuxContextRunnable {
		t.Fatalf("child state = %v, want runnable", child.state)
	}
	if got := child.snapshot.pc; got != parentPC {
		t.Fatalf("child PC = 0x%x, want post-ecall PC 0x%x", got, parentPC)
	}
	if got := child.snapshot.x[10]; got != 0 {
		t.Fatalf("child a0 = %d, want 0", got)
	}
	if got := child.snapshot.x[2]; got != childStack {
		t.Fatalf("child sp = 0x%x, want 0x%x", got, childStack)
	}
	if got := child.snapshot.x[4]; got != tls {
		t.Fatalf("child tp = 0x%x, want tls 0x%x", got, tls)
	}
	if got := child.snapshot.x[5]; got != 0xfeedface {
		t.Fatalf("child copied x5 = 0x%x, want 0xfeedface", got)
	}
	if got := child.clearChildTID; got != ctid {
		t.Fatalf("child clearChildTID = 0x%x, want 0x%x", got, ctid)
	}
}

func TestJea9Linux_CloneUnsupportedFlags(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClone, jea9TestCloneVM, 0x800000, 0, 0, 0); d != NoteHandled {
		t.Fatalf("clone disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
	if len(j.contexts) > 1 {
		t.Fatalf("unsupported clone created contexts: %d", len(j.contexts))
	}
}

func TestJea9Linux_SchedYieldRoundRobinAndSingleHart(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	cpu.SetPC(0x2000)
	cpu.SetReg(5, 11)
	first := cloneJea9LinuxThread(t, cpu, j, 0x810000, 0, 0, 0, jea9TestCloneThreadFlags)
	j.contexts[first].snapshot.x[5] = 22
	second := cloneJea9LinuxThread(t, cpu, j, 0x820000, 0, 0, 0, jea9TestCloneThreadFlags)
	j.contexts[second].snapshot.x[5] = 33

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("first yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, first)
	if got := cpu.Reg(5); got != 22 {
		t.Fatalf("after first yield x5 = %d, want first child marker 22", got)
	}
	if got := j.loadedGuestContexts; got != 1 {
		t.Fatalf("loaded guest contexts = %d, want single hart invariant 1", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("second yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, second)
	if got := cpu.Reg(5); got != 33 {
		t.Fatalf("after second yield x5 = %d, want second child marker 33", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("third yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, j.pid)
	if got := cpu.Reg(5); got != 11 {
		t.Fatalf("after third yield x5 = %d, want parent marker 11", got)
	}
}

func TestJea9Linux_SchedYieldSingleContextNoop(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	cpu.SetPC(0x3000)
	cpu.SetReg(5, 44)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, j.pid)
	requireSyscallReturn(t, cpu, 0)
	if got := cpu.Reg(5); got != 44 {
		t.Fatalf("single-context yield changed x5 to %d", got)
	}
	if got := cpu.PC(); got != 0x3000 {
		t.Fatalf("single-context yield changed PC to 0x%x", got)
	}
}

func TestJea9Linux_SchedulerQuantumRotatesRunnableContexts(t *testing.T) {
	cpu, mem, j := testLoopCPU(t, 100)
	defer mem.Free()
	j.schedulerConfig.MinQuantumRetired = 2
	j.schedulerConfig.MaxQuantumRetired = 2
	j.normalizeSchedulerConfig()
	cleanup := InstallJea9Linux(cpu, j)
	defer cleanup()

	parent := j.pid
	child := cloneJea9LinuxThread(t, cpu, j, 0x870000, 0, 0, 0, jea9TestCloneThreadFlags)

	if err := j.Run(cpu); !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("first Run error = %v, want ErrJea9LinuxBudget", err)
	}
	requireCurrentTID(t, j, child)
	if got := j.contexts[parent].snapshot.x[1]; got != 1 {
		t.Fatalf("parent saved x1 after first scheduler quantum = %d, want 1", got)
	}
	if got := cpu.Reg(1); got != 0 {
		t.Fatalf("loaded child x1 = %d, want child snapshot 0", got)
	}

	if err := j.Run(cpu); !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("second Run error = %v, want ErrJea9LinuxBudget", err)
	}
	requireCurrentTID(t, j, parent)
	if got := j.contexts[child].snapshot.x[1]; got != 1 {
		t.Fatalf("child saved x1 after second scheduler quantum = %d, want 1", got)
	}
	if got := cpu.Reg(1); got != 1 {
		t.Fatalf("reloaded parent x1 = %d, want 1", got)
	}
}

func TestJea9Linux_SchedGetAffinityOneCPU(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	mask := uint64(0x5000)
	if f := mem.WriteBytes(mask, []byte{0xff, 0xff, 0xff, 0xff}); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedGetAffinity, 0, 4, mask); d != NoteHandled {
		t.Fatalf("sched_getaffinity disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	got := readGuestBytes(t, mem, mask, 4)
	want := []byte{1, 0, 0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("affinity mask = %v, want %v", got, want)
		}
	}
}

func TestJea9Linux_SchedGetAffinityFaultsAndRejectsZeroSize(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedGetAffinity, 0, 0, 0x5000); d != NoteHandled {
		t.Fatalf("zero-size sched_getaffinity disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedGetAffinity, 0, 1, 0); d != NoteHandled {
		t.Fatalf("faulting sched_getaffinity disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEFAULT)
}

func TestJea9Linux_SetTidAddressAndRobustList(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	clearAddr := uint64(0x6000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSetTidAddress, clearAddr); d != NoteHandled {
		t.Fatalf("set_tid_address disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, int64(j.pid))
	if got := j.contexts[j.pid].clearChildTID; got != clearAddr {
		t.Fatalf("clearChildTID = 0x%x, want 0x%x", got, clearAddr)
	}

	robust := uint64(0x7000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSetRobustList, robust, 24); d != NoteHandled {
		t.Fatalf("set_robust_list disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	ctx := j.contexts[j.pid]
	if ctx.robustList != robust || ctx.robustListLen != 24 {
		t.Fatalf("robust list = {0x%x,%d}, want {0x%x,24}", ctx.robustList, ctx.robustListLen, robust)
	}
}

func TestJea9Linux_FutexWaitEAGAINWhenValueDiffers(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	addr := uint64(0x8000)
	if f := mem.Store32(addr, 1); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWait|jea9TestFutexPrivateFlag, 2, 0); d != NoteHandled {
		t.Fatalf("futex wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, -11)
	if got := j.contexts[j.pid].state; got != jea9LinuxContextRunnable {
		t.Fatalf("context state = %v, want runnable", got)
	}
	if got := len(j.futexWaiters[addr]); got != 0 {
		t.Fatalf("waiter count = %d, want 0", got)
	}
}

func TestJea9Linux_FutexWakeNoWaiters(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, 0x8800, jea9TestFutexWake, 5, 0); d != NoteHandled {
		t.Fatalf("futex wake disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := len(j.futexWaiters[0x8800]); got != 0 {
		t.Fatalf("waiters after empty wake = %d, want 0", got)
	}
}

func TestJea9Linux_FutexWaitFaultAndAlignment(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, 0x8801, jea9TestFutexWait, 0, 0); d != NoteHandled {
		t.Fatalf("unaligned futex wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, 0, jea9TestFutexWait, 0, 0); d != NoteHandled {
		t.Fatalf("faulting futex wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEFAULT)
}

func TestJea9Linux_FutexWaitBlocksAndWakeResumes(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	addr := uint64(0x9000)
	if f := mem.Store32(addr, 1); f != nil {
		t.Fatal(f)
	}
	child := cloneJea9LinuxThread(t, cpu, j, 0x830000, 0, 0, 0, jea9TestCloneThreadFlags)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWait, 1, 0); d != NoteHandled {
		t.Fatalf("futex wait disposition = %v, want NoteHandled after switching to child", d)
	}
	requireCurrentTID(t, j, child)
	if got := j.contexts[j.pid].state; got != jea9LinuxContextWaiting {
		t.Fatalf("parent state = %v, want waiting", got)
	}
	if got := len(j.futexWaiters[addr]); got != 1 || j.futexWaiters[addr][0] != j.pid {
		t.Fatalf("waiters = %v, want parent tid", j.futexWaiters[addr])
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutexTime64, addr, jea9TestFutexWake|jea9TestFutexPrivateFlag, 1, 0); d != NoteHandled {
		t.Fatalf("futex wake disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	if got := j.contexts[j.pid].state; got != jea9LinuxContextRunnable {
		t.Fatalf("parent state after wake = %v, want runnable", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, j.pid)
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_FutexWakeFIFO(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	addr := uint64(0xa000)
	if f := mem.Store32(addr, 1); f != nil {
		t.Fatal(f)
	}
	parent := j.pid
	first := cloneJea9LinuxThread(t, cpu, j, 0x840000, 0, 0, 0, jea9TestCloneThreadFlags)
	second := cloneJea9LinuxThread(t, cpu, j, 0x850000, 0, 0, 0, jea9TestCloneThreadFlags)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWait, 1, 0); d != NoteHandled {
		t.Fatalf("parent wait disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, first)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWait, 1, 0); d != NoteHandled {
		t.Fatalf("first child wait disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, second)
	if got := j.futexWaiters[addr]; len(got) != 2 || got[0] != parent || got[1] != first {
		t.Fatalf("waiters = %v, want [%d %d]", got, parent, first)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWake, 1, 0); d != NoteHandled {
		t.Fatalf("wake one disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	if j.contexts[parent].state != jea9LinuxContextRunnable || j.contexts[first].state != jea9LinuxContextWaiting {
		t.Fatalf("after wake one states parent=%v first=%v", j.contexts[parent].state, j.contexts[first].state)
	}
	if got := j.futexWaiters[addr]; len(got) != 1 || got[0] != first {
		t.Fatalf("remaining waiters = %v, want [%d]", got, first)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield to parent disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, parent)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWake, 1, 0); d != NoteHandled {
		t.Fatalf("wake second disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	if j.contexts[first].state != jea9LinuxContextRunnable {
		t.Fatalf("first state after second wake = %v, want runnable", j.contexts[first].state)
	}
}

func TestJea9Linux_FutexTimeoutIdleJump(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 10})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	addr := uint64(0xb000)
	timeout := uint64(0xb100)
	if f := mem.Store32(addr, 1); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(timeout, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(timeout+8, 90); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFutex, addr, jea9TestFutexWait, 1, timeout); d != NoteHandled {
		t.Fatalf("futex timeout disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, -110)
	if got := j.MonotonicNS(); got != 100 {
		t.Fatalf("monotonic ns = %d, want exact idle jump to 100", got)
	}
	if got := j.contexts[j.pid].state; got != jea9LinuxContextRunnable {
		t.Fatalf("context state = %v, want runnable after timeout", got)
	}
}

func TestJea9Linux_SetTidAddressClearOnThreadExit(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	ctid := uint64(0xd000)
	child := cloneJea9LinuxThread(t, cpu, j, 0x860000, 0, 0, ctid, jea9TestCloneThreadFlags|jea9TestCloneChildSetTID|jea9TestCloneChildClearTID)
	requireGuest32(t, mem, ctid, uint32(child))

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, child)

	if d := invokeJea9LinuxSyscall(cpu, jea9LinuxSysExit, 0); d != NoteHandled {
		t.Fatalf("child exit disposition = %v, want NoteHandled after switching back to parent", d)
	}
	requireCurrentTID(t, j, j.pid)
	requireGuest32(t, mem, ctid, 0)
	if got := j.contexts[child].state; got != jea9LinuxContextExited {
		t.Fatalf("child state = %v, want exited", got)
	}
}

func TestJea9Linux_Phase9ThreadingFutexELFFixtures(t *testing.T) {
	for _, path := range []string{
		"testvectors/jea9linux/elf/sched_affinity.elf",
		"testvectors/jea9linux/elf/clone_child_stack.elf",
		"testvectors/jea9linux/elf/yield_pingpong.elf",
		"testvectors/jea9linux/elf/futex_wait_wake.elf",
		"testvectors/jea9linux/elf/futex_timeout.elf",
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
			code, err := RunWithJea9Linux(cpu, j)
			if err != nil {
				t.Fatalf("RunWithJea9Linux: %v", err)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
		})
	}
}
