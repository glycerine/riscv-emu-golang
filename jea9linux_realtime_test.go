package riscv

import (
	"bytes"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJea9Linux_RealTimeModeDefaults(t *testing.T) {
	if got := NewJea9Linux(Jea9LinuxOptions{}).TimeMode(); got != HermitTime {
		t.Fatalf("default TimeMode() = %v, want HermitTime", got)
	}
	if got := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime}).TimeMode(); got != RealTime {
		t.Fatalf("explicit TimeMode() = %v, want RealTime", got)
	}
}

func TestJea9Linux_RealTimeNanosleepDoesNotIdleJump(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime, MonotonicStartNS: 1})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	req := uint64(0x4000)
	writeGuest64(t, mem, req, 0)
	writeGuest64(t, mem, req+8, uint64(5*time.Millisecond))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysNanosleep, req, 0); d != NoteExit {
		t.Fatalf("real-time nanosleep disposition = %v, want NoteExit", d)
	}
	if got := j.contexts[j.pid].state; got != jea9LinuxContextWaiting {
		t.Fatalf("nanosleep context state = %v, want waiting before real deadline", got)
	}
	if !j.waitRealTimeBlocked(cpu) {
		t.Fatal("real-time nanosleep did not wake at deadline")
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_RealTimeEpollTimeoutDoesNotIdleJump(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime, MonotonicStartNS: 1})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	epfd := newEpoll(t, cpu)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x7000, 1, 5, 0, 0); d != NoteExit {
		t.Fatalf("real-time epoll timeout disposition = %v, want NoteExit", d)
	}
	if got := j.contexts[j.pid].state; got != jea9LinuxContextWaiting {
		t.Fatalf("epoll context state = %v, want waiting before real deadline", got)
	}
	if !j.waitRealTimeBlocked(cpu) {
		t.Fatal("real-time epoll timeout did not wake at deadline")
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_RealTimeListenerWatcherWakesEpoll(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime, AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := listenGuestTCP(t, cpu, mem)
	port := guestTCPListenPort(t, cpu, mem, server)
	epfd := newEpoll(t, cpu)
	writeEpollEvent(t, mem, 0x7000, jea9TestEpollIn, 0xcafe)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, server, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add listener disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, ^uint64(0), 0, 0); d != NoteExit {
		t.Fatalf("listener epoll wait disposition = %v, want NoteExit", d)
	}

	conn := dialHostTCP(t, port)
	defer conn.Close()
	if !j.waitRealTimeBlocked(cpu) {
		t.Fatal("listener watcher did not wake blocked epoll")
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollIn, 0xcafe)
}

func TestJea9Linux_RealTimeReadWatcherWakesEpoll(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime, AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := listenGuestTCP(t, cpu, mem)
	port := guestTCPListenPort(t, cpu, mem, server)
	conn := dialHostTCP(t, port)
	defer conn.Close()
	waitForExternalEvents(t, j, cpu, func() bool {
		return len(j.fds[int(server)].socketPending) > 0
	})
	accepted := acceptGuestTCP(t, cpu, mem, server)

	epfd := newEpoll(t, cpu)
	writeEpollEvent(t, mem, 0x7000, jea9TestEpollIn, 0xbeef)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, accepted, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add accepted disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, ^uint64(0), 0, 0); d != NoteExit {
		t.Fatalf("read epoll wait disposition = %v, want NoteExit", d)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("host write: %v", err)
	}
	if !j.waitRealTimeBlocked(cpu) {
		t.Fatal("read watcher did not wake blocked epoll")
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollIn, 0xbeef)

	if d := invokeJea9LinuxSyscall(cpu, jea9LinuxSysRead, accepted, 0x9000, 4); d != NoteHandled {
		t.Fatalf("read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4)
	if got := string(readGuestBytes(t, mem, 0x9000, 4)); got != "ping" {
		t.Fatalf("guest read = %q, want ping", got)
	}
}

func TestJea9Linux_RealTimeStaleSocketEventIgnored(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const fd = 9
	j.fds[fd] = jea9LinuxFD{kind: jea9LinuxFDSocket, socketGen: 2}
	j.applyExternalEvent(cpu, externalEvent{
		kind: eventRead,
		fd:   fd,
		gen:  1,
		data: []byte("stale"),
	})
	if got := len(j.fds[fd].socketReadBuf); got != 0 {
		t.Fatalf("stale event appended %d bytes, want 0", got)
	}
}

func TestJea9Linux_RealTimeEdgeTriggeredSocketNoDuplicateBeforeDrain(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{TimeMode: RealTime, AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	defer j.closeAllFDs()

	server := listenGuestTCP(t, cpu, mem)
	port := guestTCPListenPort(t, cpu, mem, server)
	conn := dialHostTCP(t, port)
	defer conn.Close()
	waitForExternalEvents(t, j, cpu, func() bool {
		return len(j.fds[int(server)].socketPending) > 0
	})
	accepted := acceptGuestTCP(t, cpu, mem, server)

	epfd := newEpoll(t, cpu)
	writeEpollEvent(t, mem, 0x7000, jea9TestEpollIn|jea9TestEpollET, 0x1234)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollCtl, epfd, jea9TestEpollCtlAdd, accepted, 0x7000); d != NoteHandled {
		t.Fatalf("epoll add accepted disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	fd := int(accepted)
	gen := j.fds[fd].socketGen
	j.applyExternalEvent(cpu, externalEvent{kind: eventRead, fd: fd, gen: gen, data: []byte("ping")})
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("first edge epoll disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollIn, 0x1234)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("second edge epoll disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	if d := invokeJea9LinuxSyscall(cpu, jea9LinuxSysRead, accepted, 0x9000, 4); d != NoteHandled {
		t.Fatalf("drain read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4)
	j.applyExternalEvent(cpu, externalEvent{kind: eventRead, fd: fd, gen: gen, data: []byte("pong")})
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysEpollPwait, epfd, 0x8000, 1, 0, 0, 0); d != NoteHandled {
		t.Fatalf("rearmed edge epoll disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 1)
	requireEpollEvent(t, mem, 0x8000, jea9TestEpollIn, 0x1234)
}

func TestJea9Linux_RealTimeCGuestSocketClientServer(t *testing.T) {
	t.Run("client0SecondReplyFirst", func(t *testing.T) {
		runRealtimeCGuestSocketClientServer(t, "0")
	})
	t.Run("client1SecondReplyFirstAfterClient0Sends", func(t *testing.T) {
		runRealtimeCGuestSocketClientServer(t, "1")
	})
}

func runRealtimeCGuestSocketClientServer(t *testing.T, interleaving string) {
	t.Helper()
	port := reserveTCPPort(t)
	portArg := strconv.Itoa(port)

	serverOut := newSignalBuffer("ready\n")
	server, serverDone := startRealtimeCGuest(t, "testvectors/jea9linux/elf/tcp_socket_server.elf", []string{portArg, interleaving}, serverOut)
	defer server.closeAllFDs()

	select {
	case <-serverOut.signal:
	case res := <-serverDone:
		t.Fatalf("server exited before ready: code=%d err=%v stdout=%q stderr=%q", res.code, res.err, res.stdout, res.stderr)
	case <-time.After(2 * time.Second):
		server.closeAllFDs()
		t.Fatalf("server did not report ready; stdout=%q", serverOut.String())
	}

	client0Out := newSignalBuffer("c0gate\n")
	client0StdinR, client0StdinW := io.Pipe()
	defer client0StdinR.Close()
	defer client0StdinW.Close()
	client0, client0Done := startRealtimeCGuestWithStdin(t, "testvectors/jea9linux/elf/tcp_socket_client.elf", []string{portArg, "0", "gate"}, client0Out, client0StdinR)
	defer client0.closeAllFDs()

	client1Out := newSignalBuffer("c1gate\n")
	client1StdinR, client1StdinW := io.Pipe()
	defer client1StdinR.Close()
	defer client1StdinW.Close()
	client1, client1Done := startRealtimeCGuestWithStdin(t, "testvectors/jea9linux/elf/tcp_socket_client.elf", []string{portArg, "1", "gate"}, client1Out, client1StdinR)
	defer client1.closeAllFDs()

	waitSignalOrGuestExit(t, "client_0 gate", client0Out, client0Done, namedRealtimeCGuestDone{name: "server", done: serverDone})
	waitSignalOrGuestExit(t, "client_1 gate", client1Out, client1Done, namedRealtimeCGuestDone{name: "server", done: serverDone})

	var client0Res realtimeCGuestResult
	var client1Res realtimeCGuestResult
	if interleaving == "0" {
		releaseRealtimeCGuest(t, "client_0", client0StdinW)
		client0Res = waitRealtimeCGuest(t, "client_0", client0, client0Done)
		releaseRealtimeCGuest(t, "client_1", client1StdinW)
		client1Res = waitRealtimeCGuest(t, "client_1", client1, client1Done)
	} else {
		releaseRealtimeCGuest(t, "client_0", client0StdinW)
		waitOutputContainsOrGuestExit(t, "server deferred client_0", serverOut, serverDone, "c0defer\n")
		releaseRealtimeCGuest(t, "client_1", client1StdinW)
		client0Res = waitRealtimeCGuest(t, "client_0", client0, client0Done)
		client1Res = waitRealtimeCGuest(t, "client_1", client1, client1Done)
	}
	if client0Res.err != nil || client0Res.code != 0 || client1Res.err != nil || client1Res.code != 0 {
		server.closeAllFDs()
		serverRes := waitRealtimeCGuest(t, "server", server, serverDone)
		t.Fatalf("client_0 result: code=%d err=%v stdout=%q stderr=%q; client_1 result: code=%d err=%v stdout=%q stderr=%q; server result: code=%d err=%v stdout=%q stderr=%q",
			client0Res.code, client0Res.err, client0Res.stdout, client0Res.stderr,
			client1Res.code, client1Res.err, client1Res.stdout, client1Res.stderr,
			serverRes.code, serverRes.err, serverRes.stdout, serverRes.stderr)
	}
	serverRes := waitRealtimeCGuest(t, "server", server, serverDone)
	if serverRes.err != nil || serverRes.code != 0 {
		t.Fatalf("server result: code=%d err=%v stdout=%q stderr=%q", serverRes.code, serverRes.err, serverRes.stdout, serverRes.stderr)
	}
}

func listenGuestTCP(t *testing.T, cpu *CPU, mem *GuestMemory) uint64 {
	t.Helper()
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
	return server
}

func dialHostTCP(t *testing.T, port uint16) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTCP("tcp4", nil, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	if err != nil {
		t.Fatalf("host dial tcp port %d: %v", port, err)
	}
	return conn
}

func waitForExternalEvents(t *testing.T, j *Jea9Linux, cpu *CPU, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		j.drainExternalEvents(cpu)
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for external event")
		}
		time.Sleep(time.Millisecond)
	}
}

type signalBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	want    string
	signal  chan struct{}
	signals sync.Once
	changed chan struct{}
}

func newSignalBuffer(want string) *signalBuffer {
	w := &signalBuffer{
		want:    want,
		signal:  make(chan struct{}),
		changed: make(chan struct{}),
	}
	if want == "" {
		close(w.signal)
	}
	return w
}

func (w *signalBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if w.want != "" && strings.Contains(w.buf.String(), w.want) {
		w.signals.Do(func() {
			close(w.signal)
		})
	}
	close(w.changed)
	w.changed = make(chan struct{})
	return n, err
}

func (w *signalBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *signalBuffer) SnapshotAndChanged() (string, <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.changed
}

type realtimeCGuestResult struct {
	code   int
	err    error
	stdout string
	stderr string
}

type namedRealtimeCGuestDone struct {
	name string
	done <-chan realtimeCGuestResult
}

func startRealtimeCGuest(t *testing.T, path string, args []string, stdout interface {
	io.Writer
	String() string
}) (*Jea9Linux, <-chan realtimeCGuestResult) {
	return startRealtimeCGuestWithStdin(t, path, args, stdout, nil)
}

func startRealtimeCGuestWithStdin(t *testing.T, path string, args []string, stdout interface {
	io.Writer
	String() string
}, stdin io.Reader) (*Jea9Linux, <-chan realtimeCGuestResult) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, data)
	if err != nil {
		mem.Free()
		t.Fatalf("LoadELFBytes(%s): %v", path, err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	const stackTop = uint64(0x03F00000)
	cpu.SetReg(2, stackTop)

	var stderr bytes.Buffer
	jos := NewJea9Linux(Jea9LinuxOptions{
		TimeMode:          RealTime,
		MonotonicStartNS:  1,
		RealtimeOffsetNS:  946684800000000000 - 1,
		InstructionBudget: 100_000,
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:              Jea9SchedulerRoundRobin,
			MinQuantumRetired: 100_000,
			MaxQuantumRetired: 100_000,
		},
		Stdout:            stdout,
		Stderr:            &stderr,
		Stdin:             stdin,
		AllowAllHostFiles: true,
	})
	guestArgs := append([]string{path}, args...)
	if err := jos.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     guestArgs,
		ExecPath: guestArgs[0],
		StackTop: stackTop,
	}); err != nil {
		mem.Free()
		t.Fatalf("InitELFStack(%s): %v", path, err)
	}

	done := make(chan realtimeCGuestResult, 1)
	go func() {
		defer mem.Free()
		defer jos.closeAllFDs()
		code, err := RunWithJea9Linux(cpu, jos)
		done <- realtimeCGuestResult{
			code:   code,
			err:    err,
			stdout: stdout.String(),
			stderr: stderr.String(),
		}
	}()
	return jos, done
}

func waitSignalOrGuestExit(t *testing.T, name string, out *signalBuffer, done <-chan realtimeCGuestResult, peers ...namedRealtimeCGuestDone) {
	t.Helper()
	select {
	case <-out.signal:
		return
	case res := <-done:
		t.Fatalf("%s guest exited before signal: code=%d err=%v stdout=%q stderr=%q%s",
			name, res.code, res.err, res.stdout, res.stderr, realtimePeerExitSummary(peers))
	case <-time.After(2 * time.Second):
		t.Fatalf("%s timed out; stdout=%q", name, out.String())
	}
}

func realtimePeerExitSummary(peers []namedRealtimeCGuestDone) string {
	var b strings.Builder
	for _, peer := range peers {
		select {
		case res := <-peer.done:
			b.WriteString("; ")
			b.WriteString(peer.name)
			b.WriteString(" result: code=")
			b.WriteString(strconv.Itoa(res.code))
			b.WriteString(" err=")
			if res.err == nil {
				b.WriteString("<nil>")
			} else {
				b.WriteString(res.err.Error())
			}
			b.WriteString(" stdout=")
			b.WriteString(strconv.Quote(res.stdout))
			b.WriteString(" stderr=")
			b.WriteString(strconv.Quote(res.stderr))
		default:
			b.WriteString("; ")
			b.WriteString(peer.name)
			b.WriteString(" still running")
		}
	}
	return b.String()
}

func waitOutputContainsOrGuestExit(t *testing.T, name string, out *signalBuffer, done <-chan realtimeCGuestResult, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		output, changed := out.SnapshotAndChanged()
		if strings.Contains(output, want) {
			return
		}
		select {
		case res := <-done:
			t.Fatalf("%s guest exited before %q: code=%d err=%v stdout=%q stderr=%q", name, want, res.code, res.err, res.stdout, res.stderr)
		case <-deadline:
			t.Fatalf("%s timed out waiting for %q; stdout=%q", name, want, out.String())
		case <-changed:
		}
	}
}

func releaseRealtimeCGuest(t *testing.T, name string, w *io.PipeWriter) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte{'\n'})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			return
		}
		t.Fatalf("release %s: %v", name, err)
	case <-time.After(2 * time.Second):
		_ = w.CloseWithError(io.ErrClosedPipe)
		t.Fatalf("release %s timed out", name)
	}
}

func waitRealtimeCGuest(t *testing.T, name string, jos *Jea9Linux, done <-chan realtimeCGuestResult) realtimeCGuestResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(2 * time.Second):
		jos.closeAllFDs()
		t.Fatalf("%s guest timed out", name)
		return realtimeCGuestResult{}
	}
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("reserve tcp port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
