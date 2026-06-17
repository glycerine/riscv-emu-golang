package riscv

import (
	"encoding/binary"
	"testing"
	"time"
)

const (
	jea9TestSysSocket      = uint64(198)
	jea9TestSysBind        = uint64(200)
	jea9TestSysListen      = uint64(201)
	jea9TestSysConnect     = uint64(203)
	jea9TestSysGetsockname = uint64(204)
	jea9TestSysSetsockopt  = uint64(208)
	jea9TestSysGetsockopt  = uint64(209)
	jea9TestSysAccept4     = uint64(242)

	jea9TestAFInet       = uint64(2)
	jea9TestSockStream   = uint64(1)
	jea9TestSockNonblock = uint64(0x800)
	jea9TestSockCloexec  = uint64(0x80000)
	jea9TestSolSocket    = uint64(1)
	jea9TestSoReuseAddr  = uint64(2)
	jea9TestSoType       = uint64(3)
)

func TestJea9Linux_SocketPassthroughDisabledByDefault(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSocket, jea9TestAFInet, jea9TestSockStream, 0); d != NoteHandled {
		t.Fatalf("socket disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEACCES)
}

func TestJea9Linux_TCPSocketListenGetsockname(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	fd := newGuestTCPSocket(t, cpu)
	storeGuestU32(t, mem, 0x4000, 1)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSetsockopt, fd, jea9TestSolSocket, jea9TestSoReuseAddr, 0x4000, 4); d != NoteHandled {
		t.Fatalf("setsockopt disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	writeGuestSockaddrInet4(t, mem, 0x5000, [4]byte{127, 0, 0, 1}, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBind, fd, 0x5000, 16); d != NoteHandled {
		t.Fatalf("bind disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysListen, fd, 128); d != NoteHandled {
		t.Fatalf("listen disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	port := guestTCPListenPort(t, cpu, mem, fd)
	if port == 0 {
		t.Fatal("getsockname returned port 0 after listen")
	}

	storeGuestU32(t, mem, 0x7100, 4)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetsockopt, fd, jea9TestSolSocket, jea9TestSoType, 0x7000, 0x7100); d != NoteHandled {
		t.Fatalf("getsockopt SO_TYPE disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	gotType, fault := mem.Load32(0x7000)
	if fault != nil {
		t.Fatalf("Load32 SO_TYPE: %v", fault)
	}
	if gotType != uint32(jea9TestSockStream) {
		t.Fatalf("SO_TYPE = %d, want SOCK_STREAM", gotType)
	}
	if gotLen := loadGuestU32(t, mem, 0x7100); gotLen != 4 {
		t.Fatalf("SO_TYPE optlen = %d, want 4", gotLen)
	}
}

func TestJea9Linux_TCPSocketConnectAcceptReadWrite(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := newGuestTCPSocket(t, cpu)
	writeGuestSockaddrInet4(t, mem, 0x5000, [4]byte{127, 0, 0, 1}, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBind, server, 0x5000, 16); d != NoteHandled {
		t.Fatalf("server bind disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysListen, server, 16); d != NoteHandled {
		t.Fatalf("server listen disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	port := guestTCPListenPort(t, cpu, mem, server)

	client := newGuestTCPSocket(t, cpu)
	writeGuestSockaddrInet4(t, mem, 0x6000, [4]byte{127, 0, 0, 1}, port)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysConnect, client, 0x6000, 16); d != NoteHandled {
		t.Fatalf("client connect disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	accepted := acceptGuestTCP(t, cpu, mem, server)
	writeGuestSocket(t, cpu, mem, client, 0x8000, []byte("ping"))
	readGuestSocketEventually(t, cpu, mem, accepted, 0x8100, []byte("ping"))

	writeGuestSocket(t, cpu, mem, accepted, 0x8200, []byte("pong"))
	readGuestSocketEventually(t, cpu, mem, client, 0x8300, []byte("pong"))
}

func TestJea9Linux_TCPSocketEpollNoDataDoesNotBlock(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := newGuestTCPSocket(t, cpu)
	writeGuestSockaddrInet4(t, mem, 0x5000, [4]byte{127, 0, 0, 1}, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBind, server, 0x5000, 16); d != NoteHandled {
		t.Fatalf("server bind disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysListen, server, 16); d != NoteHandled {
		t.Fatalf("server listen disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	port := guestTCPListenPort(t, cpu, mem, server)

	client := newGuestTCPSocket(t, cpu)
	writeGuestSockaddrInet4(t, mem, 0x6000, [4]byte{127, 0, 0, 1}, port)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysConnect, client, 0x6000, 16); d != NoteHandled {
		t.Fatalf("client connect disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	accepted := acceptGuestTCP(t, cpu, mem, server)

	epfd := newEpoll(t, cpu)
	writeEpollEvent(t, mem, 0x7000, jea9TestEpollIn, 0xcafe)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, accepted, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add accepted socket disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	type epollResult struct {
		disposition NoteDisposition
		ret         int64
	}
	done := make(chan epollResult, 1)
	go func() {
		d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0)
		done <- epollResult{disposition: d, ret: int64(cpu.Reg(10))}
	}()
	select {
	case got := <-done:
		if got.disposition != NoteHandled {
			t.Fatalf("empty socket epoll disposition = %v, want NoteHandled", got.disposition)
		}
		if got.ret != 0 {
			t.Fatalf("empty socket epoll returned %d events, want 0", got.ret)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("epoll_pwait on idle connected socket blocked in readiness probe")
	}
}

func TestJea9Linux_TCPSocketEpollOutEdgeDoesNotSpin(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := newGuestTCPSocket(t, cpu)
	writeGuestSockaddrInet4(t, mem, 0x5000, [4]byte{127, 0, 0, 1}, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBind, server, 0x5000, 16); d != NoteHandled {
		t.Fatalf("server bind disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysListen, server, 16); d != NoteHandled {
		t.Fatalf("server listen disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	port := guestTCPListenPort(t, cpu, mem, server)

	client := newGuestTCPSocket(t, cpu)
	writeGuestSockaddrInet4(t, mem, 0x6000, [4]byte{127, 0, 0, 1}, port)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysConnect, client, 0x6000, 16); d != NoteHandled {
		t.Fatalf("client connect disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	_ = acceptGuestTCP(t, cpu, mem, server)

	epfd := newEpoll(t, cpu)
	writeEpollEvent(t, mem, 0x7000, jea9TestEpollOut|jea9TestEpollET, 0xbeef)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, client, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add client socket disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("first epoll wait disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollOut, 0xbeef)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("second epoll wait disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_TCPSocketHermitRefreshWakesBlockedListenerEpoll(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := listenGuestTCP(t, cpu, mem)
	port := guestTCPListenPort(t, cpu, mem, server)
	epfd := newEpoll(t, cpu)
	writeEpollEvent(t, mem, 0x7000, jea9TestEpollIn|jea9TestEpollET, 0xcafe)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, server, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add listener disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	child := cloneJea9LinuxThread(t, cpu, j, 0x890000, 0, 0, 0, jea9TestCloneThreadFlags)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, ^uint64(0), 0, 0); d != NoteHandled {
		t.Fatalf("blocking listener epoll disposition = %v", d)
	}
	requireCurrentTID(t, j, child)
	if got := j.contexts[j.pid].state; got != jea9LinuxContextWaiting {
		t.Fatalf("parent state = %v, want waiting", got)
	}

	conn := dialHostTCP(t, port)
	defer conn.Close()
	deadline := time.Now().Add(250 * time.Millisecond)
	for j.contexts[j.pid].state != jea9LinuxContextRunnable && time.Now().Before(deadline) {
		j.refreshEpollReadiness(cpu)
	}
	if got := j.contexts[j.pid].state; got != jea9LinuxContextRunnable {
		t.Fatalf("parent state after host connect = %v, want runnable", got)
	}
	if got := int64(j.contexts[j.pid].snapshot.x[10]); got != 1 {
		t.Fatalf("epoll return = %d, want 1", got)
	}
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollIn, 0xcafe)
}

func newGuestTCPSocket(t *testing.T, cpu *CPU) uint64 {
	t.Helper()
	flags := jea9TestSockStream | jea9TestSockNonblock | jea9TestSockCloexec
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSocket, jea9TestAFInet, flags, 0); d != NoteHandled {
		t.Fatalf("socket disposition = %v", d)
	}
	fd := int64(cpu.Reg(10))
	if fd < 3 {
		t.Fatalf("socket fd = %d, want >= 3", fd)
	}
	return uint64(fd)
}

func guestTCPListenPort(t *testing.T, cpu *CPU, mem *GuestMemory, fd uint64) uint16 {
	t.Helper()
	storeGuestU32(t, mem, 0x6100, 16)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetsockname, fd, 0x6200, 0x6100); d != NoteHandled {
		t.Fatalf("getsockname disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if gotLen := loadGuestU32(t, mem, 0x6100); gotLen != 16 {
		t.Fatalf("getsockname addrlen = %d, want 16", gotLen)
	}
	return guestSockaddrInet4Port(t, mem, 0x6200)
}

func acceptGuestTCP(t *testing.T, cpu *CPU, mem *GuestMemory, server uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		storeGuestU32(t, mem, 0x9100, 16)
		if d := invokeJea9LinuxSyscall(cpu, jea9TestSysAccept4, server, 0x9000, 0x9100, jea9TestSockNonblock|jea9TestSockCloexec); d != NoteHandled {
			t.Fatalf("accept4 disposition = %v", d)
		}
		fd := int64(cpu.Reg(10))
		if fd >= 3 {
			return uint64(fd)
		}
		if fd != jea9LinuxErrEAGAIN {
			t.Fatalf("accept4 return = %d, want fd or EAGAIN", fd)
		}
		if time.Now().After(deadline) {
			t.Fatal("accept4 kept returning EAGAIN")
		}
		time.Sleep(time.Millisecond)
	}
}

func writeGuestSocket(t *testing.T, cpu *CPU, mem *GuestMemory, fd, addr uint64, payload []byte) {
	t.Helper()
	if fault := mem.WriteBytes(addr, payload); fault != nil {
		t.Fatalf("WriteBytes socket payload: %v", fault)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, fd, addr, uint64(len(payload))); d != NoteHandled {
		t.Fatalf("write socket disposition = %v", d)
	}
	if got := int64(cpu.Reg(10)); got != int64(len(payload)) {
		t.Fatalf("write socket = %d, want %d", got, len(payload))
	}
}

func readGuestSocketEventually(t *testing.T, cpu *CPU, mem *GuestMemory, fd, addr uint64, want []byte) {
	t.Helper()
	got := make([]byte, 0, len(want))
	deadline := time.Now().Add(time.Second)
	for len(got) < len(want) {
		buf := addr + uint64(len(got))
		remain := len(want) - len(got)
		if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, buf, uint64(remain)); d != NoteHandled {
			t.Fatalf("read socket disposition = %v", d)
		}
		n := int64(cpu.Reg(10))
		if n > 0 {
			got = append(got, readGuestBytes(t, mem, buf, int(n))...)
			continue
		}
		if n != jea9LinuxErrEAGAIN {
			t.Fatalf("read socket = %d, want bytes or EAGAIN", n)
		}
		if time.Now().After(deadline) {
			t.Fatalf("read socket got %q before timeout, want %q", string(got), string(want))
		}
		time.Sleep(time.Millisecond)
	}
	if string(got) != string(want) {
		t.Fatalf("read socket = %q, want %q", string(got), string(want))
	}
}

func writeGuestSockaddrInet4(t *testing.T, mem *GuestMemory, addr uint64, ip [4]byte, port uint16) {
	t.Helper()
	var raw [16]byte
	binary.LittleEndian.PutUint16(raw[0:2], uint16(jea9TestAFInet))
	binary.BigEndian.PutUint16(raw[2:4], port)
	copy(raw[4:8], ip[:])
	if fault := mem.WriteBytes(addr, raw[:]); fault != nil {
		t.Fatalf("WriteBytes sockaddr_in: %v", fault)
	}
}

func guestSockaddrInet4Port(t *testing.T, mem *GuestMemory, addr uint64) uint16 {
	t.Helper()
	raw := readGuestBytes(t, mem, addr, 16)
	if family := binary.LittleEndian.Uint16(raw[0:2]); family != uint16(jea9TestAFInet) {
		t.Fatalf("sockaddr family = %d, want AF_INET", family)
	}
	return binary.BigEndian.Uint16(raw[2:4])
}

func storeGuestU32(t *testing.T, mem *GuestMemory, addr uint64, value uint32) {
	t.Helper()
	if fault := mem.Store32(addr, value); fault != nil {
		t.Fatalf("Store32: %v", fault)
	}
}

func loadGuestU32(t *testing.T, mem *GuestMemory, addr uint64) uint32 {
	t.Helper()
	value, fault := mem.Load32(addr)
	if fault != nil {
		t.Fatalf("Load32: %v", fault)
	}
	return value
}
