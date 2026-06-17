package riscv

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

const (
	jea9TestSysGetcwd     = uint64(17)
	jea9TestSysDup3       = uint64(24)
	jea9TestSysIoctl      = uint64(29)
	jea9TestSysMkdirat    = uint64(34)
	jea9TestSysUnlinkat   = uint64(35)
	jea9TestSysStatfs     = uint64(43)
	jea9TestSysFstatfs    = uint64(44)
	jea9TestSysFtruncate  = uint64(46)
	jea9TestSysFaccessat  = uint64(48)
	jea9TestSysChdir      = uint64(49)
	jea9TestSysFcntl      = uint64(25)
	jea9TestSysGetdents   = uint64(61)
	jea9TestSysLseek      = uint64(62)
	jea9TestSysWrite      = uint64(64)
	jea9TestSysReadv      = uint64(65)
	jea9TestSysWritev     = uint64(66)
	jea9TestSysPread64    = uint64(67)
	jea9TestSysPwrite64   = uint64(68)
	jea9TestSysReadlink   = uint64(78)
	jea9TestSysFstatat    = uint64(79)
	jea9TestSysFstat      = uint64(80)
	jea9TestSysFsync      = uint64(82)
	jea9TestSysUname      = uint64(160)
	jea9TestSysGetrlimit  = uint64(163)
	jea9TestSysPrctl      = uint64(167)
	jea9TestSysGetpid     = uint64(172)
	jea9TestSysGettid     = uint64(178)
	jea9TestSysSysinfo    = uint64(179)
	jea9TestSysPrlimit64  = uint64(261)
	jea9TestSysRenameat2  = uint64(276)
	jea9TestSysStatx      = uint64(291)
	jea9TestSysFaccessat2 = uint64(439)

	jea9TestFGetFL = uint64(3)

	jea9TestOWronly    = uint64(0x1)
	jea9TestORdwr      = uint64(0x2)
	jea9TestOCreate    = uint64(0x40)
	jea9TestOTrunc     = uint64(0x200)
	jea9TestODirectory = uint64(0x10000)

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

	jea9TestATEmptyPath = uint64(0x1000)
	jea9TestATRemovedir = uint64(0x200)
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

func parseLinuxDirentNames(t *testing.T, mem *GuestMemory, addr uint64, n int) map[string]bool {
	t.Helper()
	data := readGuestBytes(t, mem, addr, n)
	names := make(map[string]bool)
	for off := 0; off < len(data); {
		if off+19 > len(data) {
			t.Fatalf("short dirent at offset %d in %d bytes", off, len(data))
		}
		reclen := int(binary.LittleEndian.Uint16(data[off+16:]))
		if reclen <= 0 || off+reclen > len(data) {
			t.Fatalf("bad dirent reclen %d at offset %d in %d bytes", reclen, off, len(data))
		}
		nameBytes := data[off+19 : off+reclen]
		if i := bytes.IndexByte(nameBytes, 0); i >= 0 {
			nameBytes = nameBytes[:i]
		}
		names[string(nameBytes)] = true
		off += reclen
	}
	return names
}

func writeGuestIovec(t *testing.T, mem *GuestMemory, addr, base, length uint64) {
	t.Helper()
	if f := mem.Store64(addr, base); f != nil {
		t.Fatalf("Store64 iov base: %v", f)
	}
	if f := mem.Store64(addr+8, length); f != nil {
		t.Fatalf("Store64 iov len: %v", f)
	}
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

func TestJea9Linux_HostFilePassthroughDisabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.txt")
	if err := os.WriteFile(path, []byte("host data"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("open host path disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrENOENT)
}

func TestJea9Linux_HostFilePassthroughReadSeekPreadAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("open host fixture disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("host fixture fd = %d, want >= 3", fd)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x6000, 2); d != NoteHandled {
		t.Fatalf("read host fixture disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 2)
	if got := string(readGuestBytes(t, mem, 0x6000, 2)); got != "ab" {
		t.Fatalf("host first read = %q", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysLseek, fd, 4, jea9TestSeekSet); d != NoteHandled {
		t.Fatalf("lseek host disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 4)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x7000, 8); d != NoteHandled {
		t.Fatalf("read after host seek disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 2)
	if got := string(readGuestBytes(t, mem, 0x7000, 2)); got != "ef" {
		t.Fatalf("host read after seek = %q", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPread64, fd, 0x8000, 3, 1); d != NoteHandled {
		t.Fatalf("pread host disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 3)
	if got := string(readGuestBytes(t, mem, 0x8000, 3)); got != "bcd" {
		t.Fatalf("host pread = %q", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, fd); d != NoteHandled {
		t.Fatalf("close host disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x9000, 1); d != NoteHandled {
		t.Fatalf("read closed host disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEBADF)
}

func TestJea9Linux_HostFilePassthroughWriteCreateTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created.txt")
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	flags := jea9TestOWronly | jea9TestOCreate | jea9TestOTrunc
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, flags, 0o644); d != NoteHandled {
		t.Fatalf("open host output disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("host output fd = %d, want >= 3", fd)
	}

	if f := mem.WriteBytes(0x6000, []byte("guest wrote host file")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, fd, 0x6000, 21); d != NoteHandled {
		t.Fatalf("write host output disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 21)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, fd); d != NoteHandled {
		t.Fatalf("close host output disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if string(got) != "guest wrote host file" {
		t.Fatalf("host output = %q", string(got))
	}
}

func TestJea9Linux_HostPassthroughDefaultCwdAllowsRelativeCreate(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetcwd, 0x7000, 4096); d != NoteHandled {
		t.Fatalf("getcwd disposition = %v", d)
	}
	wantCwd := normalizeJea9LinuxGuestPath(cwd)
	requireSyscallReturn(t, cpu, int64(len(wantCwd)+1))
	if got := string(readGuestBytes(t, mem, 0x7000, len(wantCwd))); got != wantCwd {
		t.Fatalf("getcwd = %q, want %q", got, wantCwd)
	}

	writeGuestCString(t, mem, 0x5000, "./host.cid")
	flags := jea9TestOWronly | jea9TestOCreate | jea9TestOTrunc
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, flags, 0o644); d != NoteHandled {
		t.Fatalf("openat ./host.cid disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("host.cid fd = %d, want >= 3", fd)
	}

	if f := mem.WriteBytes(0x6000, []byte("cid payload")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, fd, 0x6000, 11); d != NoteHandled {
		t.Fatalf("write host.cid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 11)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, fd); d != NoteHandled {
		t.Fatalf("close host.cid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	got, err := os.ReadFile(filepath.Join(cwd, "host.cid"))
	if err != nil {
		t.Fatalf("ReadFile host.cid: %v", err)
	}
	if string(got) != "cid payload" {
		t.Fatalf("host.cid = %q", string(got))
	}
}

func TestJea9Linux_MkdiratHostPassthroughDisabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-dir")
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMkdirat, jea9TestATFDCWD, 0x5000, 0o700); d != NoteHandled {
		t.Fatalf("mkdirat host path disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrENOENT)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled mkdirat created %q or returned unexpected stat error: %v", path, err)
	}
}

func TestJea9Linux_MkdiratHostPassthroughCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created-dir")
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMkdirat, jea9TestATFDCWD, 0x5000, 0o700); d != NoteHandled {
		t.Fatalf("mkdirat host path disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("mkdirat(%q) stat = {%v,%v}, want directory", path, info, err)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMkdirat, jea9TestATFDCWD, 0x5000, 0o700); d != NoteHandled {
		t.Fatalf("second mkdirat host path disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEEXIST)
}

func TestJea9Linux_OpenatAndMkdiratDirfd(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", parent, err)
	}
	if err := os.WriteFile(filepath.Join(parent, "data.txt"), []byte("dirfd data"), 0o644); err != nil {
		t.Fatalf("WriteFile data.txt: %v", err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, parent)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("openat parent disposition = %v", d)
	}
	dirfd := cpu.Reg(10)
	if int64(dirfd) < 3 {
		t.Fatalf("parent dirfd = %d, want >= 3", dirfd)
	}

	writeGuestCString(t, mem, 0x5000, "data.txt")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, dirfd, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("openat dirfd relative file disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if int64(fd) < 3 {
		t.Fatalf("relative file fd = %d, want >= 3", fd)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x6000, 32); d != NoteHandled {
		t.Fatalf("read relative file disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 10)
	if got := string(readGuestBytes(t, mem, 0x6000, 10)); got != "dirfd data" {
		t.Fatalf("relative file read = %q", got)
	}

	writeGuestCString(t, mem, 0x5000, "created")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMkdirat, dirfd, 0x5000, 0o700); d != NoteHandled {
		t.Fatalf("mkdirat dirfd relative disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if info, err := os.Stat(filepath.Join(parent, "created")); err != nil || !info.IsDir() {
		t.Fatalf("mkdirat dirfd child stat = {%v,%v}, want directory", info, err)
	}
}

func TestJea9Linux_MkdiratDirfdErrorEdges(t *testing.T) {
	parent := t.TempDir()
	hostFile := filepath.Join(parent, "file.txt")
	if err := os.WriteFile(hostFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", hostFile, err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, "child")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMkdirat, 99, 0x5000, 0o700); d != NoteHandled {
		t.Fatalf("mkdirat bad dirfd disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEBADF)

	writeGuestCString(t, mem, 0x5000, hostFile)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("openat host file disposition = %v", d)
	}
	fd := cpu.Reg(10)
	writeGuestCString(t, mem, 0x5000, "child")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMkdirat, fd, 0x5000, 0o700); d != NoteHandled {
		t.Fatalf("mkdirat nondir fd disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrENOTDIR)
}

func TestJea9Linux_GetcwdChdirAndRelativeOpenat(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", sub, err)
	}
	if err := os.WriteFile(filepath.Join(sub, "data.txt"), []byte("cwd data"), 0o644); err != nil {
		t.Fatalf("WriteFile data.txt: %v", err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true, Cwd: root})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetcwd, 0x5000, 4096); d != NoteHandled {
		t.Fatalf("getcwd disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, int64(len(root)+1))
	if got, errno := readLinuxCString(cpu, 0x5000, 4096); errno != 0 || got != root {
		t.Fatalf("getcwd = %q errno %d, want %q", got, errno, root)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetcwd, 0x5000, uint64(len(root))); d != NoteHandled {
		t.Fatalf("small getcwd disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrERANGE)

	writeGuestCString(t, mem, 0x5000, "sub")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysChdir, 0x5000); d != NoteHandled {
		t.Fatalf("chdir disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetcwd, 0x5000, 4096); d != NoteHandled {
		t.Fatalf("getcwd after chdir disposition = %v", d)
	}
	if got, errno := readLinuxCString(cpu, 0x5000, 4096); errno != 0 || got != sub {
		t.Fatalf("getcwd after chdir = %q errno %d, want %q", got, errno, sub)
	}

	writeGuestCString(t, mem, 0x5000, "data.txt")
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("relative openat disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, fd, 0x6000, 16); d != NoteHandled {
		t.Fatalf("relative read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 8)
	if got := string(readGuestBytes(t, mem, 0x6000, 8)); got != "cwd data" {
		t.Fatalf("relative read = %q", got)
	}
}

func TestJea9Linux_StatFstatStatxAndStatfs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat.txt")
	if err := os.WriteFile(path, []byte("stat-data"), 0o640); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFstatat, jea9TestATFDCWD, 0x5000, 0x6000, 0); d != NoteHandled {
		t.Fatalf("fstatat disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	mode, _ := mem.Load32(0x6000 + 16)
	size, _ := mem.Load64(0x6000 + 48)
	if mode&jea9LinuxModeIFMT != jea9LinuxModeIFREG || size != 9 {
		t.Fatalf("fstatat mode/size = 0%o/%d, want regular/9", mode, size)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("openat stat file disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFstat, fd, 0x6100); d != NoteHandled {
		t.Fatalf("fstat disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	size, _ = mem.Load64(0x6100 + 48)
	if size != 9 {
		t.Fatalf("fstat size = %d, want 9", size)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysStatx, jea9TestATFDCWD, 0x5000, 0, 0, 0x6200); d != NoteHandled {
		t.Fatalf("statx disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	statxMode, _ := mem.Load16(0x6200 + 28)
	statxSize, _ := mem.Load64(0x6200 + 40)
	if uint32(statxMode)&jea9LinuxModeIFMT != jea9LinuxModeIFREG || statxSize != 9 {
		t.Fatalf("statx mode/size = 0%o/%d, want regular/9", statxMode, statxSize)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysStatfs, 0x5000, 0x6400); d != NoteHandled {
		t.Fatalf("statfs disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if magic, _ := mem.Load64(0x6400); magic == 0 {
		t.Fatal("statfs magic is zero")
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFstatfs, fd, 0x6500); d != NoteHandled {
		t.Fatalf("fstatfs disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_GetdentsReadlinkAndAccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("WriteFile alpha: %v", err)
	}
	link := filepath.Join(dir, "alpha.link")
	if err := os.Symlink("alpha.txt", link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, dir)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, jea9TestODirectory, 0); d != NoteHandled {
		t.Fatalf("openat dir disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetdents, fd, 0x6000, 512); d != NoteHandled {
		t.Fatalf("getdents disposition = %v", d)
	}
	n := int(cpu.Reg(10))
	if n <= 0 {
		t.Fatalf("getdents returned %d, want entries", n)
	}
	names := parseLinuxDirentNames(t, mem, 0x6000, n)
	if !names["."] || !names[".."] || !names["alpha.txt"] || !names["alpha.link"] {
		t.Fatalf("getdents names = %v", names)
	}

	writeGuestCString(t, mem, 0x5000, link)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysReadlink, jea9TestATFDCWD, 0x5000, 0x7000, 64); d != NoteHandled {
		t.Fatalf("readlinkat disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 9)
	if got := string(readGuestBytes(t, mem, 0x7000, 9)); got != "alpha.txt" {
		t.Fatalf("readlink target = %q", got)
	}

	writeGuestCString(t, mem, 0x5000, filepath.Join(dir, "alpha.txt"))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFaccessat, jea9TestATFDCWD, 0x5000, 4, 0); d != NoteHandled {
		t.Fatalf("faccessat disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFaccessat2, jea9TestATFDCWD, 0x5000, 4, 0); d != NoteHandled {
		t.Fatalf("faccessat2 disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
}

func TestJea9Linux_VectorPwriteFtruncateRenameUnlinkAndDup3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vec.txt")
	j := NewJea9Linux(Jea9LinuxOptions{AllowAllHostFiles: true})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	flags := jea9TestORdwr | jea9TestOCreate | jea9TestOTrunc
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, flags, 0o644); d != NoteHandled {
		t.Fatalf("open vec file disposition = %v", d)
	}
	fd := cpu.Reg(10)
	if f := mem.WriteBytes(0x6000, []byte("hello ")); f != nil {
		t.Fatal(f)
	}
	if f := mem.WriteBytes(0x6100, []byte("world")); f != nil {
		t.Fatal(f)
	}
	writeGuestIovec(t, mem, 0x7000, 0x6000, 6)
	writeGuestIovec(t, mem, 0x7010, 0x6100, 5)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWritev, fd, 0x7000, 2); d != NoteHandled {
		t.Fatalf("writev disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 11)

	if f := mem.WriteBytes(0x6200, []byte("J")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPwrite64, fd, 0x6200, 1, 6); d != NoteHandled {
		t.Fatalf("pwrite64 disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 1)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFsync, fd); d != NoteHandled {
		t.Fatalf("fsync disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFtruncate, fd, 7); d != NoteHandled {
		t.Fatalf("ftruncate disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysLseek, fd, 0, jea9TestSeekSet); d != NoteHandled {
		t.Fatalf("lseek vec disposition = %v", d)
	}
	writeGuestIovec(t, mem, 0x7020, 0x6300, 4)
	writeGuestIovec(t, mem, 0x7030, 0x6400, 8)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysReadv, fd, 0x7020, 2); d != NoteHandled {
		t.Fatalf("readv disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 7)
	if got := string(readGuestBytes(t, mem, 0x6300, 4)) + string(readGuestBytes(t, mem, 0x6400, 3)); got != "hello J" {
		t.Fatalf("readv data = %q", got)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysDup3, fd, 77, 0); d != NoteHandled {
		t.Fatalf("dup3 disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 77)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, fd); d != NoteHandled {
		t.Fatalf("close original disposition = %v", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysFstat, 77, 0x7200); d != NoteHandled {
		t.Fatalf("fstat dup disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, 77); d != NoteHandled {
		t.Fatalf("close dup disposition = %v", d)
	}

	renamed := filepath.Join(dir, "renamed.txt")
	writeGuestCString(t, mem, 0x5000, path)
	writeGuestCString(t, mem, 0x5100, renamed)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRenameat2, jea9TestATFDCWD, 0x5000, jea9TestATFDCWD, 0x5100, 0); d != NoteHandled {
		t.Fatalf("renameat2 disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if got, err := os.ReadFile(renamed); err != nil || string(got) != "hello J" {
		t.Fatalf("renamed file = %q err %v, want hello J", string(got), err)
	}
	writeGuestCString(t, mem, 0x5000, renamed)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysUnlinkat, jea9TestATFDCWD, 0x5000, 0); d != NoteHandled {
		t.Fatalf("unlinkat file disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if _, err := os.Stat(renamed); !os.IsNotExist(err) {
		t.Fatalf("renamed file still exists or stat error: %v", err)
	}
	emptyDir := filepath.Join(dir, "empty")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatalf("Mkdir empty: %v", err)
	}
	writeGuestCString(t, mem, 0x5000, emptyDir)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysUnlinkat, jea9TestATFDCWD, 0x5000, jea9TestATRemovedir); d != NoteHandled {
		t.Fatalf("unlinkat dir disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
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
