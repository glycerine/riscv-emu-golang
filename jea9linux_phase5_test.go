package riscv

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

const (
	jea9TestSysIoctl     = uint64(29)
	jea9TestSysFcntl     = uint64(25)
	jea9TestSysLseek     = uint64(62)
	jea9TestSysWrite     = uint64(64)
	jea9TestSysPread64   = uint64(67)
	jea9TestSysUname     = uint64(160)
	jea9TestSysGetrlimit = uint64(163)
	jea9TestSysPrctl     = uint64(167)
	jea9TestSysGetpid    = uint64(172)
	jea9TestSysGettid    = uint64(178)
	jea9TestSysSysinfo   = uint64(179)
	jea9TestSysPrlimit64 = uint64(261)

	jea9TestFGetFL = uint64(3)

	jea9TestTCGETS     = uint64(0x5401)
	jea9TestTCSETS     = uint64(0x5402)
	jea9TestTIOCGWINSZ = uint64(0x5413)
	jea9TestTIOCSWINSZ = uint64(0x5414)

	jea9TestSeekSet = uint64(0)
	jea9TestSeekCur = uint64(1)

	jea9TestRLimitStack  = uint64(3)
	jea9TestRLimitNOFile = uint64(7)

	jea9TestPRSetName = uint64(15)
	jea9TestPRGetName = uint64(16)
)

type limitedWriter struct {
	limit int
	buf   bytes.Buffer
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.limit {
		n = w.limit
	}
	w.buf.Write(p[:n])
	return n, nil
}

func writeGuestCString(t *testing.T, mem *GuestMemory, addr uint64, s string) {
	t.Helper()
	if f := mem.WriteBytes(addr, []byte(s+"\x00")); f != nil {
		t.Fatalf("WriteBytes string: %v", f)
	}
}

func requireSyscallReturn(t *testing.T, cpu *CPU, want int64) {
	t.Helper()
	if got := int64(cpu.Reg(10)); got != want {
		t.Fatalf("syscall return = %d, want %d", got, want)
	}
}

func unameField(t *testing.T, mem *GuestMemory, base uint64, index int) string {
	t.Helper()
	data := readGuestBytes(t, mem, base+uint64(index*65), 65)
	if i := bytes.IndexByte(data, 0); i >= 0 {
		data = data[:i]
	}
	return string(data)
}

func TestJea9Linux_WriteStdoutStderrAndPartial(t *testing.T) {
	var stdout, stderr bytes.Buffer
	j := NewJea9Linux(Jea9LinuxOptions{Stdout: &stdout, Stderr: &stderr})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if f := mem.WriteBytes(0x5000, []byte("hello stdout")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, 1, 0x5000, 12); d != NoteHandled {
		t.Fatalf("stdout write disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 12)
	if stdout.String() != "hello stdout" {
		t.Fatalf("stdout = %q", stdout.String())
	}

	if f := mem.WriteBytes(0x6000, []byte("err")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, 2, 0x6000, 3); d != NoteHandled {
		t.Fatalf("stderr write disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 3)
	if stderr.String() != "err" {
		t.Fatalf("stderr = %q", stderr.String())
	}

	partial := &limitedWriter{limit: 4}
	j = NewJea9Linux(Jea9LinuxOptions{Stdout: partial})
	cpu, mem = newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	if f := mem.WriteBytes(0x7000, []byte("partial")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, 1, 0x7000, 7); d != NoteHandled {
		t.Fatalf("partial write disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4)
	if partial.buf.String() != "part" {
		t.Fatalf("partial writer saw %q", partial.buf.String())
	}
}

func TestJea9Linux_WriteBadFD(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if f := mem.WriteBytes(0x5000, []byte("nope")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, 99, 0x5000, 4); d != NoteHandled {
		t.Fatalf("write bad fd disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -9)
}

func TestJea9Linux_IoctlTermiosRoundTrip(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 0, jea9TestTCGETS, 0x5000); d != NoteHandled {
		t.Fatalf("TCGETS disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	term := readGuestBytes(t, mem, 0x5000, jea9LinuxTermiosSize)
	if got := binary.LittleEndian.Uint32(term[8:]); got == 0 {
		t.Fatal("TCGETS cflag is zero, want plausible terminal mode")
	}
	if got := term[17+6]; got != 1 {
		t.Fatalf("TCGETS VMIN = %d, want 1", got)
	}

	binary.LittleEndian.PutUint32(term[12:], 0x12345678)
	term[17+6] = 7
	if f := mem.WriteBytes(0x6000, term); f != nil {
		t.Fatalf("write termios: %v", f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 0, jea9TestTCSETS, 0x6000); d != NoteHandled {
		t.Fatalf("TCSETS disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 0, jea9TestTCGETS, 0x7000); d != NoteHandled {
		t.Fatalf("second TCGETS disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	got := readGuestBytes(t, mem, 0x7000, jea9LinuxTermiosSize)
	if !bytes.Equal(got, term) {
		t.Fatalf("TCGETS after TCSETS mismatch\ngot  %x\nwant %x", got, term)
	}
}

func TestJea9Linux_IoctlWinsizeRoundTrip(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 1, jea9TestTIOCGWINSZ, 0x5000); d != NoteHandled {
		t.Fatalf("TIOCGWINSZ disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	win := readGuestBytes(t, mem, 0x5000, jea9LinuxWinsizeSize)
	if rows, cols := binary.LittleEndian.Uint16(win[0:]), binary.LittleEndian.Uint16(win[2:]); rows != 24 || cols != 80 {
		t.Fatalf("default winsize = %dx%d, want 24x80", rows, cols)
	}

	binary.LittleEndian.PutUint16(win[0:], 40)
	binary.LittleEndian.PutUint16(win[2:], 120)
	if f := mem.WriteBytes(0x6000, win); f != nil {
		t.Fatalf("write winsize: %v", f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 1, jea9TestTIOCSWINSZ, 0x6000); d != NoteHandled {
		t.Fatalf("TIOCSWINSZ disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 1, jea9TestTIOCGWINSZ, 0x7000); d != NoteHandled {
		t.Fatalf("second TIOCGWINSZ disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	got := readGuestBytes(t, mem, 0x7000, jea9LinuxWinsizeSize)
	if !bytes.Equal(got, win) {
		t.Fatalf("winsize after TIOCSWINSZ mismatch: got %x want %x", got, win)
	}
}

func TestJea9Linux_IoctlErrors(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{Files: map[string][]byte{"/file": []byte("abc")}})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 99, jea9TestTCGETS, 0x5000); d != NoteHandled {
		t.Fatalf("bad fd ioctl disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEBADF)

	writeGuestCString(t, mem, 0x5000, "/file")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("openat disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, fd, jea9TestTCGETS, 0x6000); d != NoteHandled {
		t.Fatalf("file ioctl disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrENOTTY)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 0, 0xdeadbeef, 0x6000); d != NoteHandled {
		t.Fatalf("unknown ioctl disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrENOTTY)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysIoctl, 0, jea9TestTCGETS, mem.Size()-2); d != NoteHandled {
		t.Fatalf("fault ioctl disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEFAULT)
}

func TestJea9Linux_ReadStdinEOFAndConfiguredInput(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, 0, 0x5000, 8); d != NoteHandled {
		t.Fatalf("nil stdin read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	j = NewJea9Linux(Jea9LinuxOptions{Stdin: bytes.NewBufferString("abcdef")})
	cpu, mem = newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, 0, 0x6000, 4); d != NoteHandled {
		t.Fatalf("stdin read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4)
	if got := string(readGuestBytes(t, mem, 0x6000, 4)); got != "abcd" {
		t.Fatalf("stdin first read = %q", got)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, 0, 0x7000, 4); d != NoteHandled {
		t.Fatalf("stdin second read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 2)
	if got := string(readGuestBytes(t, mem, 0x7000, 2)); got != "ef" {
		t.Fatalf("stdin second read = %q", got)
	}
}

func TestJea9Linux_CloseThenReadBadFD(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{Stdin: bytes.NewBufferString("x")})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, 0); d != NoteHandled {
		t.Fatalf("close stdin disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, 0, 0x5000, 1); d != NoteHandled {
		t.Fatalf("read closed stdin disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -9)
}

func TestJea9Linux_OpenAtUnsupportedPath(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, "/etc/passwd")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("open unsupported disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -2)
}

func TestJea9Linux_ConfiguredReadOnlyFileReadSeekPreadAndFcntl(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		Files: map[string][]byte{
			"/fixture.txt": []byte("abcdef"),
		},
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, "/fixture.txt")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("open fixture disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("fixture fd = %d, want >= 3", fd)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFcntl, fd, jea9TestFGetFL, 0); d != NoteHandled {
		t.Fatalf("fcntl disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x6000, 2); d != NoteHandled {
		t.Fatalf("read fixture disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 2)
	if got := string(readGuestBytes(t, mem, 0x6000, 2)); got != "ab" {
		t.Fatalf("fixture first read = %q", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysLseek, fd, 4, jea9TestSeekSet); d != NoteHandled {
		t.Fatalf("lseek set disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x7000, 8); d != NoteHandled {
		t.Fatalf("read after seek disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 2)
	if got := string(readGuestBytes(t, mem, 0x7000, 2)); got != "ef" {
		t.Fatalf("fixture read after seek = %q", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPread64, fd, 0x8000, 3, 1); d != NoteHandled {
		t.Fatalf("pread fixture disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 3)
	if got := string(readGuestBytes(t, mem, 0x8000, 3)); got != "bcd" {
		t.Fatalf("fixture pread = %q", got)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysLseek, fd, 0, jea9TestSeekCur); d != NoteHandled {
		t.Fatalf("lseek cur disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 6)
}

func TestJea9Linux_ConfiguredFileOptionsCopied(t *testing.T) {
	file := []byte("stable")
	j := NewJea9Linux(Jea9LinuxOptions{
		Files: map[string][]byte{"/copied.txt": file},
	})
	file[0] = 'X'
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, "/copied.txt")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("open copied fixture disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x6000, 6); d != NoteHandled {
		t.Fatalf("read copied fixture disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 6)
	if got := string(readGuestBytes(t, mem, 0x6000, 6)); got != "stable" {
		t.Fatalf("copied fixture read = %q", got)
	}
}

func TestJea9Linux_FileDescriptorErrorEdges(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		Files: map[string][]byte{"/fixture.txt": []byte("abcdef")},
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, "/fixture.txt")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 1, 0); d != NoteHandled {
		t.Fatalf("open write-only fixture disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -13)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("open fixture disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFcntl, fd, 999, 0); d != NoteHandled {
		t.Fatalf("invalid fcntl disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -22)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysLseek, fd, ^uint64(0), jea9TestSeekSet); d != NoteHandled {
		t.Fatalf("negative lseek disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -22)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPread64, fd, 0x6000, 1, ^uint64(0)); d != NoteHandled {
		t.Fatalf("negative pread disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -22)
}

func TestJea9Linux_LseekNonseekableStream(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysLseek, 0, 0, jea9TestSeekCur); d != NoteHandled {
		t.Fatalf("lseek stdin disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -29)
}

func TestJea9Linux_GetpidGettidInitialThread(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{PID: 4242, TID: 4243})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetpid); d != NoteHandled {
		t.Fatalf("getpid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4242)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGettid); d != NoteHandled {
		t.Fatalf("gettid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4243)
}

func TestJea9Linux_UnameDeterministic(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysUname, 0x5000); d != NoteHandled {
		t.Fatalf("uname disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got := unameField(t, mem, 0x5000, 0); got != "Linux" {
		t.Fatalf("sysname = %q", got)
	}
	if got := unameField(t, mem, 0x5000, 1); got != "jea9linux" {
		t.Fatalf("nodename = %q", got)
	}
	if got := unameField(t, mem, 0x5000, 4); got != "riscv64" {
		t.Fatalf("machine = %q", got)
	}
}

func TestJea9Linux_ResourceSyscallsDeterministic(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 12_345_000_000})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetrlimit, jea9TestRLimitStack, 0x5000); d != NoteHandled {
		t.Fatalf("getrlimit disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	cur, _ := mem.Load64(0x5000)
	max, _ := mem.Load64(0x5008)
	if cur != 8*1024*1024 || max != 8*1024*1024 {
		t.Fatalf("RLIMIT_STACK = {%d,%d}", cur, max)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrlimit64, 0, jea9TestRLimitNOFile, 0, 0x6000); d != NoteHandled {
		t.Fatalf("prlimit64 disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	cur, _ = mem.Load64(0x6000)
	max, _ = mem.Load64(0x6008)
	if cur != 1024 || max != 1024 {
		t.Fatalf("RLIMIT_NOFILE = {%d,%d}", cur, max)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSysinfo, 0x7000); d != NoteHandled {
		t.Fatalf("sysinfo disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	uptime, _ := mem.Load64(0x7000)
	memUnit, _ := mem.Load32(0x7068)
	if uptime != 12 || memUnit != 1 {
		t.Fatalf("sysinfo uptime/mem_unit = {%d,%d}, want {12,1}", uptime, memUnit)
	}
}

func TestJea9Linux_PrctlThreadName(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, "main-thread-name-is-long")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrctl, jea9TestPRSetName, 0x5000); d != NoteHandled {
		t.Fatalf("PR_SET_NAME disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrctl, jea9TestPRGetName, 0x6000); d != NoteHandled {
		t.Fatalf("PR_GET_NAME disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	name, errno := readLinuxCString(cpu, 0x6000, 16)
	if errno != 0 {
		t.Fatalf("thread name read errno = %d", errno)
	}
	if name != "main-thread-nam" {
		t.Fatalf("thread name = %q, want 15-byte truncation", name)
	}
}

func TestJea9Linux_Phase5ProcessFDELFFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		stdin   string
		wantOut string
	}{
		{
			name:    "write_stdout",
			path:    "testvectors/jea9linux/elf/write_stdout.elf",
			wantOut: "jea9linux stdout\n",
		},
		{
			name:    "read_stdin_echo",
			path:    "testvectors/jea9linux/elf/read_stdin_echo.elf",
			stdin:   "fixture input\n",
			wantOut: "fixture input\n",
		},
		{
			name: "pid_tid",
			path: "testvectors/jea9linux/elf/pid_tid.elf",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
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
			var out bytes.Buffer
			j := NewJea9Linux(Jea9LinuxOptions{
				Stdin:  bytes.NewBufferString(tc.stdin),
				Stdout: &out,
			})
			code, err := RunWithJea9Linux(cpu, j)
			if err != nil {
				t.Fatalf("RunWithJea9Linux: %v", err)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if out.String() != tc.wantOut {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}
