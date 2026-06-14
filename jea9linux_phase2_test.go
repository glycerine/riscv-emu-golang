package riscv

import (
	"os"
	"testing"
	"time"
)

const (
	jea9TestSysNanosleep    = uint64(101)
	jea9TestSysClockGettime = uint64(113)
	jea9TestSysGettimeofday = uint64(169)

	jea9TestClockRealtime  = uint64(0)
	jea9TestClockMonotonic = uint64(1)
)

func newJea9LinuxSyscallCPU(t *testing.T, j *Jea9Linux) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	InstallJea9Linux(cpu, j)
	return cpu, mem
}

func invokeJea9LinuxSyscall(cpu *CPU, num uint64, args ...uint64) NoteDisposition {
	cpu.SetReg(17, num)
	for i := 0; i < len(args) && i < 6; i++ {
		cpu.SetReg(uint8(10+i), args[i])
	}
	return cpu.Notes.handlers[len(cpu.Notes.handlers)-1](cpu, Note{Cause: CauseEcallU})
}

func requireTimespec(t *testing.T, mem *GuestMemory, addr uint64, sec, nsec uint64) {
	t.Helper()
	gotSec, f := mem.Load64(addr)
	if f != nil {
		t.Fatalf("Load64(sec): %v", f)
	}
	gotNSec, f := mem.Load64(addr + 8)
	if f != nil {
		t.Fatalf("Load64(nsec): %v", f)
	}
	if gotSec != sec || gotNSec != nsec {
		t.Fatalf("timespec at 0x%x = {%d,%d}, want {%d,%d}", addr, gotSec, gotNSec, sec, nsec)
	}
}

func TestJea9Linux_ClockGettimeMonotonicSyscall(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 1_234_567_890})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	ts := uint64(0x2000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClockGettime, jea9TestClockMonotonic, ts); d != NoteHandled {
		t.Fatalf("disposition = %v, want NoteHandled", d)
	}
	if got := int64(cpu.Reg(10)); got != 0 {
		t.Fatalf("clock_gettime return = %d, want 0", got)
	}
	requireTimespec(t, mem, ts, 1, 234_567_890)
}

func TestJea9Linux_ClockGettimeRealtimeOffsetSyscall(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		MonotonicStartNS: 1_000_000_000,
		RealtimeOffsetNS: 2_000_000_005,
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	ts := uint64(0x2000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClockGettime, jea9TestClockRealtime, ts); d != NoteHandled {
		t.Fatalf("disposition = %v, want NoteHandled", d)
	}
	requireTimespec(t, mem, ts, 3, 5)
}

func TestJea9Linux_ClockGettimeInvalidClockSyscall(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysClockGettime, 999, 0x2000); d != NoteHandled {
		t.Fatalf("disposition = %v, want NoteHandled", d)
	}
	if got := int64(cpu.Reg(10)); got != -22 {
		t.Fatalf("clock_gettime invalid return = %d, want -EINVAL", got)
	}
}

func TestJea9Linux_GettimeofdaySyscall(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		MonotonicStartNS: 1_234_567_890,
		RealtimeOffsetNS: 2_000_000_000,
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	tv := uint64(0x3000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGettimeofday, tv, 0); d != NoteHandled {
		t.Fatalf("disposition = %v, want NoteHandled", d)
	}
	gotSec, _ := mem.Load64(tv)
	gotUSec, _ := mem.Load64(tv + 8)
	if gotSec != 3 || gotUSec != 234_567 {
		t.Fatalf("timeval = {%d,%d}, want {3,234567}", gotSec, gotUSec)
	}
}

func TestJea9Linux_NanosleepIdleJumpSyscall(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{ClockMode: Jea9ClockIdleJump})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	req := uint64(0x4000)
	if f := mem.Store64(req, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(req+8, 10_000_000); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysNanosleep, req, 0); d != NoteHandled {
		t.Fatalf("disposition = %v, want NoteHandled", d)
	}
	if got := int64(cpu.Reg(10)); got != 0 {
		t.Fatalf("nanosleep return = %d, want 0", got)
	}
	if got := j.MonotonicNS(); got != 10_000_000 {
		t.Fatalf("MonotonicNS() = %d, want 10000000", got)
	}
}

func TestJea9Linux_NanosleepInvalidTimespecSyscall(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	req := uint64(0x4000)
	if f := mem.Store64(req, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(req+8, 1_000_000_000); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysNanosleep, req, 0); d != NoteHandled {
		t.Fatalf("disposition = %v, want NoteHandled", d)
	}
	if got := int64(cpu.Reg(10)); got != -22 {
		t.Fatalf("nanosleep invalid return = %d, want -EINVAL", got)
	}
}

func TestJea9Linux_NanosleepManualClockBlocksUntilAdvance(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{ClockMode: Jea9ClockManual})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	req := uint64(0x4000)
	if f := mem.Store64(req, 0); f != nil {
		t.Fatal(f)
	}
	if f := mem.Store64(req+8, 10_000_000); f != nil {
		t.Fatal(f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysNanosleep, req, 0); d != NoteExit {
		t.Fatalf("disposition = %v, want NoteExit for blocked manual sleep", d)
	}
	if !j.Blocked() {
		t.Fatal("manual nanosleep should mark jea9linux blocked")
	}
	if got := j.MonotonicNS(); got != 0 {
		t.Fatalf("MonotonicNS() = %d, want 0 before explicit advance", got)
	}
	j.AdvanceTime(5 * time.Millisecond)
	if !j.Blocked() {
		t.Fatal("manual nanosleep unblocked before deadline")
	}
	j.AdvanceTime(5 * time.Millisecond)
	if j.Blocked() {
		t.Fatal("manual nanosleep still blocked after deadline")
	}
}

func TestJea9Linux_Phase2ClockELFFixtures(t *testing.T) {
	for _, path := range []string{
		"testvectors/jea9linux/elf/clock_gettime_basic.elf",
		"testvectors/jea9linux/elf/nanosleep_idle_jump.elf",
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
