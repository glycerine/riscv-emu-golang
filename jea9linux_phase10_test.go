package riscv

import (
	"encoding/binary"
	"os"
	"testing"
)

const (
	jea9TestSysEventfd2    = uint64(19)
	jea9TestSysEpollCreate = uint64(20)
	jea9TestSysEpollCtl    = uint64(21)
	jea9TestSysEpollPwait  = uint64(22)
	jea9TestSysPipe2       = uint64(59)
	jea9TestSysPselect6    = uint64(72)

	jea9TestEFDNonblock = uint64(0x800)

	jea9TestEpollCtlAdd = uint64(1)
	jea9TestEpollCtlDel = uint64(2)
	jea9TestEpollCtlMod = uint64(3)

	jea9TestEpollIn  = uint32(0x001)
	jea9TestEpollOut = uint32(0x004)
	jea9TestEpollET  = uint32(0x80000000)
)

func writeGuest64(t *testing.T, mem *GuestMemory, addr, value uint64) {
	t.Helper()
	if f := mem.Store64(addr, value); f != nil {
		t.Fatalf("Store64(0x%x): %v", addr, f)
	}
}

func readGuest32(t *testing.T, mem *GuestMemory, addr uint64) uint32 {
	t.Helper()
	got, f := mem.Load32(addr)
	if f != nil {
		t.Fatalf("Load32(0x%x): %v", addr, f)
	}
	return got
}

func readGuest64(t *testing.T, mem *GuestMemory, addr uint64) uint64 {
	t.Helper()
	got, f := mem.Load64(addr)
	if f != nil {
		t.Fatalf("Load64(0x%x): %v", addr, f)
	}
	return got
}

func writeEpollEvent(t *testing.T, mem *GuestMemory, addr uint64, events uint32, data uint64) {
	t.Helper()
	if f := mem.Store32(addr, events); f != nil {
		t.Fatalf("Store32(epoll events): %v", f)
	}
	if f := mem.Store32(addr+jea9LinuxEpollEventPadOffset, 0); f != nil {
		t.Fatalf("Store32(epoll pad): %v", f)
	}
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], data)
	if f := mem.WriteBytes(addr+jea9LinuxEpollEventDataOffset, raw[:]); f != nil {
		t.Fatalf("WriteBytes(epoll data): %v", f)
	}
}

func requireEpollEvent(t *testing.T, mem *GuestMemory, addr uint64, events uint32, data uint64) {
	t.Helper()
	gotEvents, f := mem.Load32(addr)
	if f != nil {
		t.Fatalf("Load32(epoll events): %v", f)
	}
	var raw [8]byte
	if f := mem.ReadBytes(addr+jea9LinuxEpollEventDataOffset, raw[:]); f != nil {
		t.Fatalf("ReadBytes(epoll data): %v", f)
	}
	gotData := binary.LittleEndian.Uint64(raw[:])
	if gotEvents != events || gotData != data {
		t.Fatalf("epoll event at 0x%x = {events=0x%x,data=0x%x}, want {0x%x,0x%x}", addr, gotEvents, gotData, events, data)
	}
}

func TestJea9Linux_EpollEventLayoutRISCV64(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	eventfd := newEventfd(t, cpu, 1, 0)
	epfd := newEpoll(t, cpu)
	if f := mem.Store32(0x7000, jea9TestEpollIn); f != nil {
		t.Fatalf("Store32(epoll events): %v", f)
	}
	if f := mem.Store32(0x7004, 0xdeadbeef); f != nil {
		t.Fatalf("Store32(epoll pad): %v", f)
	}
	const wantData = uint64(0x1122334455667788)
	if f := mem.Store64(0x7000+jea9LinuxEpollEventDataOffset, wantData); f != nil {
		t.Fatalf("Store64(epoll data): %v", f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("epoll wait disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollIn, wantData)
	if got := readGuest32(t, mem, 0x8004); got != 0 {
		t.Fatalf("epoll output pad = 0x%x, want 0", got)
	}
}

func newEventfd(t *testing.T, cpu *CPU, init, flags uint64) uint64 {
	t.Helper()
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEventfd2, init, flags); d != NoteHandled {
		t.Fatalf("eventfd2 disposition = %v, want NoteHandled", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("eventfd fd = %d, want >= 3", fd)
	}
	return fd
}

func newEpoll(t *testing.T, cpu *CPU) uint64 {
	t.Helper()
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCreate, 0); d != NoteHandled {
		t.Fatalf("epoll_create1 disposition = %v, want NoteHandled", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("epoll fd = %d, want >= 3", fd)
	}
	return fd
}

func TestJea9Linux_EventfdInitialValue(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	fd := newEventfd(t, cpu, 7, 0)
	buf := uint64(0x5000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, buf, 8); d != NoteHandled {
		t.Fatalf("eventfd read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 8)
	if got := readGuest64(t, mem, buf); got != 7 {
		t.Fatalf("eventfd initial read = %d, want 7", got)
	}
}

func TestJea9Linux_EventfdReadWrite(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	fd := newEventfd(t, cpu, 0, 0)
	buf := uint64(0x5000)
	writeGuest64(t, mem, buf, 5)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, fd, buf, 8); d != NoteHandled {
		t.Fatalf("eventfd write disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 8)
	writeGuest64(t, mem, buf, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, buf, 8); d != NoteHandled {
		t.Fatalf("eventfd read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 8)
	if got := readGuest64(t, mem, buf); got != 5 {
		t.Fatalf("eventfd read value = %d, want 5", got)
	}
}

func TestJea9Linux_EventfdNonblockEmptyRead(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	fd := newEventfd(t, cpu, 0, jea9TestEFDNonblock)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x5000, 8); d != NoteHandled {
		t.Fatalf("eventfd empty read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEAGAIN)
}

func TestJea9Linux_EventfdRejectsShortAndOverflowWrites(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	fd := newEventfd(t, cpu, ^uint64(0)-2, 0)
	buf := uint64(0x5000)
	writeGuest64(t, mem, buf, 2)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, fd, buf, 8); d != NoteHandled {
		t.Fatalf("eventfd overflow write disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEAGAIN)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, buf, 4); d != NoteHandled {
		t.Fatalf("eventfd short read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
}

func TestJea9Linux_EpollCreateClose(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	epfd := newEpoll(t, cpu)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, epfd); d != NoteHandled {
		t.Fatalf("epoll close disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, 0, 0); d != NoteHandled {
		t.Fatalf("epoll ctl closed disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEBADF)
}

func TestJea9Linux_EpollCtlAddModDel(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	eventfd := newEventfd(t, cpu, 0, 0)
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 0xabc)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll add disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := len(j.fds[int(epfd)].epoll.registrations); got != 1 {
		t.Fatalf("epoll registration count = %d, want 1", got)
	}

	writeEpollEvent(t, mem, event, jea9TestEpollIn|jea9TestEpollOut, 0xdef)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlMod, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll mod disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	reg := j.fds[int(epfd)].epoll.registrations[int(eventfd)]
	if reg.events != jea9TestEpollIn|jea9TestEpollOut || reg.data != 0xdef {
		t.Fatalf("modified registration = {0x%x,0x%x}, want {0x%x,0xdef}", reg.events, reg.data, jea9TestEpollIn|jea9TestEpollOut)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlDel, eventfd, 0); d != NoteHandled {
		t.Fatalf("epoll del disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := len(j.fds[int(epfd)].epoll.registrations); got != 0 {
		t.Fatalf("epoll registration count after del = %d, want 0", got)
	}
}

func TestJea9Linux_EpollCtlErrors(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	eventfd := newEventfd(t, cpu, 0, 0)
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 1)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll add disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll duplicate add disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEEXIST)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlMod, eventfd+100, event); d != NoteHandled {
		t.Fatalf("epoll missing mod disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEBADF)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, 999, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll bad epfd disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEBADF)
}

func TestJea9Linux_EpollPwaitReadyImmediate(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	eventfd := newEventfd(t, cpu, 2, 0)
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	out := uint64(0x7000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 0x1234)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll add disposition = %v, want NoteHandled", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 4, ^uint64(0), 0, 0); d != NoteHandled {
		t.Fatalf("epoll wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, out, jea9TestEpollIn, 0x1234)
	if got := j.MonotonicNS(); got != 0 {
		t.Fatalf("monotonic advanced to %d for immediate readiness", got)
	}
}

func TestJea9Linux_EpollEdgeTriggeredReportsReadinessOnce(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	eventfd := newEventfd(t, cpu, 1, 0)
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	out := uint64(0x7000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn|jea9TestEpollET, 0x1234)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll add disposition = %v, want NoteHandled", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 4, 0, 0, 0); d != NoteHandled {
		t.Fatalf("first epoll wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, out, jea9TestEpollIn, 0x1234)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 4, 0, 0, 0); d != NoteHandled {
		t.Fatalf("second epoll wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, eventfd, 0x8000, 8); d != NoteHandled {
		t.Fatalf("eventfd read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 8)
	writeGuest64(t, mem, 0x8000, 1)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, eventfd, 0x8000, 8); d != NoteHandled {
		t.Fatalf("eventfd write disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 8)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 4, 0, 0, 0); d != NoteHandled {
		t.Fatalf("third epoll wait disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, out, jea9TestEpollIn, 0x1234)
}

func TestJea9Linux_EpollPwaitBlocksUntilEvent(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	eventfd := newEventfd(t, cpu, 0, 0)
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	out := uint64(0x7000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 0xbeef)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, eventfd, event); d != NoteHandled {
		t.Fatalf("epoll add disposition = %v, want NoteHandled", d)
	}
	child := cloneJea9LinuxThread(t, cpu, j, 0x890000, 0, 0, 0, jea9TestCloneThreadFlags)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 1, ^uint64(0), 0, 0); d != NoteHandled {
		t.Fatalf("blocking epoll disposition = %v, want NoteHandled after switch", d)
	}
	requireCurrentTID(t, j, child)
	if got := j.contexts[j.pid].state; got != jea9LinuxContextWaiting {
		t.Fatalf("parent state = %v, want waiting", got)
	}

	value := uint64(0x8000)
	writeGuest64(t, mem, value, 1)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, eventfd, value, 8); d != NoteHandled {
		t.Fatalf("child eventfd write disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 8)
	if got := j.contexts[j.pid].state; got != jea9LinuxContextRunnable {
		t.Fatalf("parent state after eventfd write = %v, want runnable", got)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield to parent disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, j.pid)
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, out, jea9TestEpollIn, 0xbeef)
}

func TestJea9Linux_EpollPwaitTimeoutIdleJump(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 10})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	epfd := newEpoll(t, cpu)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x7000, 1, 7, 0, 0); d != NoteHandled {
		t.Fatalf("epoll timeout disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := j.MonotonicNS(); got != 7_000_010 {
		t.Fatalf("monotonic ns = %d, want 7000010", got)
	}
}

func TestJea9Linux_EpollPwaitIndefiniteHonorsOtherDeadline(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		ClockMode:        Jea9ClockIdleJump,
		MonotonicStartNS: 10,
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	child := cloneJea9LinuxThread(t, cpu, j, 0x890000, 0, 0, 0, jea9TestCloneThreadFlags)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSchedYield); d != NoteHandled {
		t.Fatalf("yield to child disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, child)

	req := uint64(0x5000)
	writeGuest64(t, mem, req, 0)
	writeGuest64(t, mem, req+8, 90)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysNanosleep, req, 0); d != NoteHandled {
		t.Fatalf("child nanosleep disposition = %v, want NoteHandled", d)
	}
	requireCurrentTID(t, j, j.pid)

	epfd := newEpoll(t, cpu)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x7000, 1, ^uint64(0), 0, 0); d != NoteHandled {
		t.Fatalf("indefinite epoll disposition = %v, want NoteHandled after deadline advance", d)
	}
	requireCurrentTID(t, j, child)
	if got := j.MonotonicNS(); got != 100 {
		t.Fatalf("monotonic ns = %d, want child nanosleep deadline 100", got)
	}
	if got := j.contexts[j.pid].state; got != jea9LinuxContextWaiting {
		t.Fatalf("parent state = %v, want waiting in epoll", got)
	}
	if got := j.contexts[child].state; got != jea9LinuxContextRunnable {
		t.Fatalf("child state = %v, want runnable after nanosleep deadline", got)
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_EpollPwaitReadyOrder(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	firstFD := newEventfd(t, cpu, 1, 0)
	secondFD := newEventfd(t, cpu, 1, 0)
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	out := uint64(0x7000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 0x2222)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, secondFD, event); d != NoteHandled {
		t.Fatalf("epoll add second disposition = %v, want NoteHandled", d)
	}
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 0x1111)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, firstFD, event); d != NoteHandled {
		t.Fatalf("epoll add first disposition = %v, want NoteHandled", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 2, 0, 0, 0); d != NoteHandled {
		t.Fatalf("epoll ready order disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 2)
	requireEpollEvent(t, mem, out, jea9TestEpollIn, 0x2222)
	requireEpollEvent(t, mem, out+jea9LinuxEpollEventSize, jea9TestEpollIn, 0x1111)
}

func TestJea9Linux_PipeReadinessThroughEpoll(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	pipefd := uint64(0x5000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPipe2, pipefd, 0); d != NoteHandled {
		t.Fatalf("pipe2 disposition = %v, want NoteHandled", d)
	}
	readFD, f := mem.Load32(pipefd)
	if f != nil {
		t.Fatal(f)
	}
	writeFD, f := mem.Load32(pipefd + 4)
	if f != nil {
		t.Fatal(f)
	}
	epfd := newEpoll(t, cpu)
	event := uint64(0x6000)
	out := uint64(0x7000)
	writeEpollEvent(t, mem, event, jea9TestEpollIn, 0xaaaa)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, uint64(readFD), event); d != NoteHandled {
		t.Fatalf("epoll add pipe read disposition = %v, want NoteHandled", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("empty pipe epoll disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)

	if f := mem.WriteBytes(0x8000, []byte("x")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, uint64(writeFD), 0x8000, 1); d != NoteHandled {
		t.Fatalf("pipe write disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, out, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("ready pipe epoll disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, out, jea9TestEpollIn, 0xaaaa)
}

func TestJea9Linux_Pipe2ReadWrite(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	pipefd := uint64(0x5000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPipe2, pipefd, 0); d != NoteHandled {
		t.Fatalf("pipe2 disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	readFD, f := mem.Load32(pipefd)
	if f != nil {
		t.Fatal(f)
	}
	writeFD, f := mem.Load32(pipefd + 4)
	if f != nil {
		t.Fatal(f)
	}
	if f := mem.WriteBytes(0x6000, []byte("abc")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, uint64(writeFD), 0x6000, 3); d != NoteHandled {
		t.Fatalf("pipe write disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 3)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, uint64(readFD), 0x7000, 3); d != NoteHandled {
		t.Fatalf("pipe read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 3)
	if got := string(readGuestBytes(t, mem, 0x7000, 3)); got != "abc" {
		t.Fatalf("pipe read bytes = %q, want abc", got)
	}
}

func TestJea9Linux_Pipe2NonblockEmptyRead(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	pipefd := uint64(0x5000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPipe2, pipefd, jea9TestEFDNonblock); d != NoteHandled {
		t.Fatalf("pipe2 nonblock disposition = %v, want NoteHandled", d)
	}
	readFD, f := mem.Load32(pipefd)
	if f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, uint64(readFD), 0x6000, 1); d != NoteHandled {
		t.Fatalf("pipe empty read disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEAGAIN)
}

func TestJea9Linux_Pselect6Timeout(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 5})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	timeout := uint64(0x5000)
	if f := mem.Store64(timeout, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(timeout+8, 1234); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPselect6, 0, 0, 0, 0, timeout, 0); d != NoteHandled {
		t.Fatalf("pselect6 timeout disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := j.MonotonicNS(); got != 1239 {
		t.Fatalf("monotonic ns = %d, want 1239", got)
	}
}

func TestJea9Linux_Pselect6TimeoutStopsAtChaosBoundary(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		MonotonicStartNS: 100,
		Trace:            true,
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode: Jea9SchedulerChaos,
		},
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	parent := installLowPriorityChaosPeer(t, j, cpu)

	timeout := uint64(0x5100)
	writeGuest64(t, mem, timeout, 0)
	writeGuest64(t, mem, timeout+8, 100)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPselect6, 0, 0, 0, 0, timeout, 0); d != NoteHandled {
		t.Fatalf("pselect6 timeout disposition = %v, want NoteHandled after scheduling low-priority context", d)
	}
	requireTimeoutStoppedAtChaosBoundary(t, j, parent, jea9LinuxWaitNanosleep, 200, "pselect-timeout")
}

func TestJea9Linux_EpollPwaitTimeoutStopsAtChaosBoundary(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		MonotonicStartNS: 100,
		Trace:            true,
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode: Jea9SchedulerChaos,
		},
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	parent := installLowPriorityChaosPeer(t, j, cpu)
	epfd := newEpoll(t, cpu)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x7000, 1, 100, 0, 0); d != NoteHandled {
		t.Fatalf("epoll_pwait timeout disposition = %v, want NoteHandled after scheduling low-priority context", d)
	}
	requireTimeoutStoppedAtChaosBoundary(t, j, parent, jea9LinuxWaitEpoll, 100_000_100, "epoll-timeout")
}

func installLowPriorityChaosPeer(t *testing.T, j *Jea9Linux, cpu *CPU) *jea9LinuxContext {
	t.Helper()
	parent := j.ensureScheduler(cpu)
	lowTID := parent.tid + 1
	j.contexts[lowTID] = &jea9LinuxContext{
		tid:           lowTID,
		state:         jea9LinuxContextRunnable,
		schedPriority: jea9LinuxSchedLow,
		snapshot: jea9LinuxCPUSnapshot{
			pc: 0x2000,
		},
	}
	j.contextOrder = append(j.contextOrder, lowTID)
	j.chaosActive = true
	j.chaosStartNS = 100
	j.chaosUntilNS = 150
	j.clockPolicy = ClockPolicyChaos
	j.chaosPolicyPhase = jea9LinuxChaosPolicyStarvation
	return parent
}

func requireTimeoutStoppedAtChaosBoundary(t *testing.T, j *Jea9Linux, parent *jea9LinuxContext, waitKind jea9LinuxWaitKind, deadline int64, source string) {
	t.Helper()
	if got := j.MonotonicNS(); got != 150 {
		t.Fatalf("monotonic ns = %d, want chaos boundary 150", got)
	}
	if j.chaosActive {
		t.Fatal("chaos window remained active after reaching chaos boundary")
	}
	requireCurrentTID(t, j, parent.tid+1)
	if parent.state != jea9LinuxContextWaiting || parent.waitKind != waitKind {
		t.Fatalf("parent wait state = (%v, %v), want wait kind %v", parent.state, parent.waitKind, waitKind)
	}
	if parent.waitDeadlineNS != deadline {
		t.Fatalf("parent wait deadline = %d, want %d", parent.waitDeadlineNS, deadline)
	}
	trace := j.TraceSnapshot()
	if len(trace.Clock) != 1 {
		t.Fatalf("clock trace entries = %d, want 1: %+v", len(trace.Clock), trace.Clock)
	}
	entry := trace.Clock[0]
	if entry.Source != source || entry.BeforeNS != 100 || entry.NS != 150 ||
		entry.AdvanceNS != 50 || entry.DeadlineNS != deadline || entry.ReachedDeadline {
		t.Fatalf("clock trace entry = %+v, want %s 100->150 toward deadline %d without reaching it", entry, source, deadline)
	}
}

func TestJea9Linux_Phase10EventPollingELFFixtures(t *testing.T) {
	for _, path := range []string{
		"testvectors/jea9linux/elf/eventfd_basic.elf",
		"testvectors/jea9linux/elf/epoll_eventfd.elf",
		"testvectors/jea9linux/elf/epoll_timeout.elf",
		"testvectors/jea9linux/elf/pipe2_basic.elf",
		"testvectors/jea9linux/elf/pselect_timeout.elf",
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
