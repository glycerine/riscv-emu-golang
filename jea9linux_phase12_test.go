package riscv

import (
	"os"
	"testing"
)

const (
	jea9Phase12SysSetitimer    = uint64(103)
	jea9Phase12SysTimerCreate  = uint64(107)
	jea9Phase12SysTimerSettime = uint64(110)
	jea9Phase12SysTimerDelete  = uint64(111)
	jea9Phase12SysRiscvHwprobe = uint64(258)
)

func TestJea9Linux_RiscvHwprobeENOSYS(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9Phase12SysRiscvHwprobe, 0x5000, 2, 0, 0, 0, 0); d != NoteHandled {
		t.Fatalf("riscv_hwprobe disposition = %v, want NoteHandled", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrENOSYS)
}

func TestJea9Linux_RiscvHwprobeNoHostDependency(t *testing.T) {
	run := func(seed []byte, monotonic int64) (int64, uint64, uint64) {
		j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed, MonotonicStartNS: monotonic})
		cpu, mem := newJea9LinuxSyscallCPU(t, j)
		defer mem.Free()

		pairs := uint64(0x5000)
		writeGuest64(t, mem, pairs, 0x1111222233334444)
		writeGuest64(t, mem, pairs+8, 0x5555666677778888)
		if d := invokeJea9LinuxSyscall(cpu, jea9Phase12SysRiscvHwprobe, pairs, 1, 0, 0, 0, 0); d != NoteHandled {
			t.Fatalf("riscv_hwprobe disposition = %v", d)
		}
		return int64(cpu.Reg(10)), readGuest64(t, mem, pairs), readGuest64(t, mem, pairs+8)
	}
	retA, firstA, secondA := run([]byte("a"), 123)
	retB, firstB, secondB := run([]byte("different"), 987654321)
	if retA != jea9LinuxErrENOSYS || retB != jea9LinuxErrENOSYS {
		t.Fatalf("riscv_hwprobe returns = {%d,%d}, want -ENOSYS", retA, retB)
	}
	if firstA != firstB || secondA != secondB {
		t.Fatalf("riscv_hwprobe mutated pairs differently: {0x%x,0x%x} vs {0x%x,0x%x}", firstA, secondA, firstB, secondB)
	}
}

func TestJea9Linux_TimerCompatibilitySyscallsENOSYS(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	for _, tc := range []struct {
		name string
		num  uint64
	}{
		{"setitimer", jea9Phase12SysSetitimer},
		{"timer_create", jea9Phase12SysTimerCreate},
		{"timer_settime", jea9Phase12SysTimerSettime},
		{"timer_delete", jea9Phase12SysTimerDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d := invokeJea9LinuxSyscall(cpu, tc.num, 0, 0, 0, 0, 0, 0); d != NoteHandled {
				t.Fatalf("%s disposition = %v", tc.name, d)
			}
			requireSyscallReturn(t, cpu, jea9LinuxErrENOSYS)
		})
	}
}

func TestJea9Linux_ResourceSyscallErrorEdges(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysGetrlimit, 99, 0x5000); d != NoteHandled {
		t.Fatalf("getrlimit invalid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrlimit64, j.pid+1, jea9TestRLimitNOFile, 0, 0x6000); d != NoteHandled {
		t.Fatalf("prlimit64 bad pid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrESRCH)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrlimit64, 0, jea9TestRLimitNOFile, 0x7000, 0); d != NoteHandled {
		t.Fatalf("prlimit64 set disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEPERM)
}

func TestJea9Linux_PrctlAndSysinfoDeterministicEdges(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 42_000_000_001})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrctl, jea9LinuxPRSetVMA, 0, 0, 0, 0); d != NoteHandled {
		t.Fatalf("prctl PR_SET_VMA disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysPrctl, 999, 0, 0, 0, 0); d != NoteHandled {
		t.Fatalf("prctl invalid disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, jea9LinuxErrEINVAL)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysSysinfo, 0x8000); d != NoteHandled {
		t.Fatalf("sysinfo disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if uptime := readGuest64(t, mem, 0x8000); uptime != 42 {
		t.Fatalf("sysinfo uptime = %d, want 42", uptime)
	}
	if totalRAM := readGuest64(t, mem, 0x8020); totalRAM != mem.Size() {
		t.Fatalf("sysinfo totalram = %d, want %d", totalRAM, mem.Size())
	}
	requireGuest32(t, mem, 0x8068, 1)
}

func TestJea9Linux_Phase12CapabilityMiscELFFixtures(t *testing.T) {
	for _, path := range []string{
		"testvectors/jea9linux/elf/riscv_hwprobe.elf",
		"testvectors/jea9linux/elf/resource_limits.elf",
		"testvectors/jea9linux/elf/sysinfo_basic.elf",
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
			j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 42_000_000_001})
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
