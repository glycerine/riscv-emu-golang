package riscv

import (
	"os"
	"testing"
)

const (
	jea9TestSysRead      = uint64(63)
	jea9TestSysClose     = uint64(57)
	jea9TestSysOpenat    = uint64(56)
	jea9TestSysGetrandom = uint64(278)

	jea9TestATFDCWD = uint64(^uint64(99)) // -100
)

func readGuestBytes(t *testing.T, mem *GuestMemory, addr uint64, n int) []byte {
	t.Helper()
	got := make([]byte, n)
	if f := mem.ReadBytes(addr, got); f != nil {
		t.Fatalf("ReadBytes: %v", f)
	}
	return got
}

func TestJea9Linux_GetRandomRepeatableSyscall(t *testing.T) {
	seed := []byte("phase3 getrandom seed")
	var outputs [2][]byte
	for i := range outputs {
		j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed})
		cpu, mem := newJea9LinuxSyscallCPU(t, j)
		defer mem.Free()

		buf := uint64(0x5000)
		if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetrandom, buf, 32, 0); d != NoteHandled {
			t.Fatalf("disposition = %v, want NoteHandled", d)
		}
		if got := int64(cpu.Reg(10)); got != 32 {
			t.Fatalf("getrandom return = %d, want 32", got)
		}
		outputs[i] = readGuestBytes(t, mem, buf, 32)
	}
	if string(outputs[0]) != string(outputs[1]) {
		t.Fatalf("same seed getrandom mismatch: %x != %x", outputs[0], outputs[1])
	}
}

func TestJea9Linux_GetRandomChunkingSyscall(t *testing.T) {
	seed := []byte("phase3 chunk seed")
	j1 := NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed})
	cpu1, mem1 := newJea9LinuxSyscallCPU(t, j1)
	defer mem1.Free()
	j2 := NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed})
	cpu2, mem2 := newJea9LinuxSyscallCPU(t, j2)
	defer mem2.Free()

	if d := invokeJea9LinuxSyscall(cpu1, jea9TestSysGetrandom, 0x5000, 64, 0); d != NoteHandled {
		t.Fatalf("large disposition = %v", d)
	}
	for i := 0; i < 4; i++ {
		if d := invokeJea9LinuxSyscall(cpu2, jea9TestSysGetrandom, 0x6000+uint64(i*16), 16, 0); d != NoteHandled {
			t.Fatalf("chunk %d disposition = %v", i, d)
		}
	}
	large := readGuestBytes(t, mem1, 0x5000, 64)
	chunked := readGuestBytes(t, mem2, 0x6000, 64)
	if string(large) != string(chunked) {
		t.Fatalf("chunked stream mismatch: large=%x chunked=%x", large, chunked)
	}
}

func TestJea9Linux_GetRandomZeroLengthAndInvalidFlags(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("flags")})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetrandom, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("zero disposition = %v", d)
	}
	if got := int64(cpu.Reg(10)); got != 0 {
		t.Fatalf("zero getrandom return = %d, want 0", got)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetrandom, 0x5000, 8, 0x8000); d != NoteHandled {
		t.Fatalf("invalid disposition = %v", d)
	}
	if got := int64(cpu.Reg(10)); got != -22 {
		t.Fatalf("invalid getrandom return = %d, want -EINVAL", got)
	}
}

func TestJea9Linux_DevURandomOpenReadClose(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("dev urandom")})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	path := uint64(0x5000)
	if f := mem.WriteBytes(path, []byte("/dev/urandom\x00")); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, path, 0, 0); d != NoteHandled {
		t.Fatalf("openat disposition = %v", d)
	}
	fd := int64(cpu.Reg(10))
	if fd < 3 {
		t.Fatalf("openat fd = %d, want >= 3", fd)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, uint64(fd), 0x6000, 24); d != NoteHandled {
		t.Fatalf("read disposition = %v", d)
	}
	if got := int64(cpu.Reg(10)); got != 24 {
		t.Fatalf("read return = %d, want 24", got)
	}
	first := readGuestBytes(t, mem, 0x6000, 24)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClose, uint64(fd)); d != NoteHandled {
		t.Fatalf("close disposition = %v", d)
	}
	if got := int64(cpu.Reg(10)); got != 0 {
		t.Fatalf("close return = %d, want 0", got)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, path, 0, 0); d != NoteHandled {
		t.Fatalf("reopen disposition = %v", d)
	}
	fd = int64(cpu.Reg(10))
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, uint64(fd), 0x7000, 24); d != NoteHandled {
		t.Fatalf("second read disposition = %v", d)
	}
	second := readGuestBytes(t, mem, 0x7000, 24)
	if string(first) == string(second) {
		t.Fatalf("random device stream rewound after reopen: %x", first)
	}
}

func TestJea9Linux_Phase3RandomELFFixtures(t *testing.T) {
	data, err := os.ReadFile("testvectors/jea9linux/elf/getrandom_repeat.elf")
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
	code, err := RunWithJea9LinuxInterp(cpu, j)
	if err != nil {
		t.Fatalf("RunWithJea9LinuxInterp: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}
