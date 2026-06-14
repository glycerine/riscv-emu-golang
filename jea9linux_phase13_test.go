package riscv

import (
	"bytes"
	"testing"
)

func withDirectEcallEnabled(t *testing.T) {
	t.Helper()
	directWasEnabled := DirectSyscallEnabled()
	inlineWasEnabled := InlineEcallEnabled()
	EnableDirectSyscall()
	SetInlineEcallEnabled(true)
	t.Cleanup(func() {
		if !directWasEnabled {
			DisableDirectSyscall()
		}
		SetInlineEcallEnabled(inlineWasEnabled)
	})
	if !DirectSyscallEnabled() {
		t.Skip("direct syscall fast path unavailable on this host")
	}
}

func runJITWithJea9Linux(cpu *CPU, j *Jea9Linux) (int, error) {
	cleanup := InstallJea9Linux(cpu, j)
	defer cleanup()
	jit := NewJIT()
	err := jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		return ex.Code, nil
	}
	return cpu.ExitCode, err
}

func TestJea9Linux_JITDoesNotUseHostWrite(t *testing.T) {
	withDirectEcallEnabled(t)

	const (
		codeVA = uint64(0x1000)
		msgVA  = uint64(0x3000)
	)
	msg := []byte("jea9linux jit stdout\n")
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),               // a0 = stdout
		uenc(opLUI, 11, uint32(msgVA)),           // a1 = msgVA
		ienc(opOPIMM, 0, 12, 0, int32(len(msg))), // a2 = len
		ienc(opOPIMM, 0, 17, 0, 64),              // a7 = write
		instrECALL,
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()
	if f := mem.WriteBytes(msgVA, msg); f != nil {
		t.Fatal(f)
	}

	var guestStdout bytes.Buffer
	j := NewJea9Linux(Jea9LinuxOptions{Stdout: &guestStdout})
	hostStdout := captureStdout(t, func() {
		code, err := runJITWithJea9Linux(cpu, j)
		if err != nil {
			t.Fatalf("runJITWithJea9Linux: %v", err)
		}
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	})
	if guestStdout.String() != string(msg) {
		t.Fatalf("jea9linux stdout = %q, want %q; host stdout captured %q", guestStdout.String(), msg, hostStdout)
	}
	if len(hostStdout) != 0 {
		t.Fatalf("host stdout captured %q, want no host write", hostStdout)
	}
}

func TestJea9Linux_JITGettidUsesVirtualTid(t *testing.T) {
	withDirectEcallEnabled(t)

	const codeVA = uint64(0x1000)
	insns := []uint32{
		ienc(opOPIMM, 0, 17, 0, 178), // a7 = gettid
		instrECALL,
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit, a0 remains gettid
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{PID: 4242, TID: 4243})
	code, err := runJITWithJea9Linux(cpu, j)
	if err != nil {
		t.Fatalf("runJITWithJea9Linux: %v", err)
	}
	if code != 4243 {
		t.Fatalf("JIT gettid exit code = %d, want virtual tid 4243", code)
	}
}

func TestJea9Linux_JITClockUsesDeterministicClock(t *testing.T) {
	withDirectEcallEnabled(t)

	const (
		codeVA = uint64(0x1000)
		tsVA   = uint64(0x5000)
	)
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),    // a0 = CLOCK_MONOTONIC
		uenc(opLUI, 11, uint32(tsVA)), // a1 = timespec
		ienc(opOPIMM, 0, 17, 0, 113),  // a7 = clock_gettime
		instrECALL,
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{MonotonicStartNS: 12_000_000_345})
	code, err := runJITWithJea9Linux(cpu, j)
	if err != nil {
		t.Fatalf("runJITWithJea9Linux: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	requireTimespec(t, mem, tsVA, 12, 345)
}

func TestJea9Linux_JITRandomMatchesInterpreter(t *testing.T) {
	withDirectEcallEnabled(t)

	const (
		codeVA = uint64(0x1000)
		bufVA  = uint64(0x6000)
	)
	insns := []uint32{
		uenc(opLUI, 10, uint32(bufVA)), // a0 = buffer
		ienc(opOPIMM, 0, 11, 0, 8),     // a1 = len
		ienc(opOPIMM, 0, 12, 0, 0),     // a2 = flags
		ienc(opOPIMM, 0, 17, 0, 278),   // a7 = getrandom
		instrECALL,
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	run := func(useJIT bool) []byte {
		t.Helper()
		cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
		defer mem.Free()
		j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("phase13")})
		var code int
		var err error
		if useJIT {
			code, err = runJITWithJea9Linux(cpu, j)
		} else {
			code, err = RunWithJea9Linux(cpu, j)
		}
		if err != nil {
			t.Fatalf("run useJIT=%v: %v", useJIT, err)
		}
		if code != 0 {
			t.Fatalf("exit code useJIT=%v = %d, want 0", useJIT, code)
		}
		return readGuestBytes(t, mem, bufVA, 8)
	}
	jitBytes := run(true)
	interpBytes := run(false)
	if !bytes.Equal(jitBytes, interpBytes) {
		t.Fatalf("JIT random = %x, interpreter random = %x", jitBytes, interpBytes)
	}
}

func TestJea9Linux_JITDirectEcallDoesNotRewind(t *testing.T) {
	withDirectEcallEnabled(t)

	const (
		codeVA = uint64(0x1000)
		cellVA = uint64(0x4000)
	)
	insns := []uint32{
		uenc(opLUI, 7, uint32(cellVA)), // x7 = cell
		ienc(opOPIMM, 0, 5, 0, 1),      // x5 = 1
		senc(opSTORE, 3, 7, 5, 0),      // sd x5, 0(x7)
		ienc(opOPIMM, 0, 17, 0, 172),   // a7 = getpid
		instrECALL,
		ienc(opOPIMM, 0, 6, 0, 2),   // x6 = 2
		senc(opSTORE, 3, 7, 6, 0),   // sd x6, 0(x7)
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{PID: 77, TID: 77})
	code, err := runJITWithJea9Linux(cpu, j)
	if err != nil {
		t.Fatalf("runJITWithJea9Linux: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got, f := mem.Load64(cellVA)
	if f != nil {
		t.Fatal(f)
	}
	if got != 2 {
		t.Fatalf("cell = %d, want 2; ECALL may have rewound or skipped post-ECALL code", got)
	}
}

func TestJea9Linux_InstallRestoresDirectSyscallPolicy(t *testing.T) {
	original := directSyscallDisabled
	t.Cleanup(func() { directSyscallDisabled = original })

	for _, startDisabled := range []bool{false, true} {
		t.Run(func() string {
			if startDisabled {
				return "initially-disabled"
			}
			return "initially-enabled"
		}(), func(t *testing.T) {
			directSyscallDisabled = startDisabled
			mem, err := NewGuestMemory(Size64MB)
			if err != nil {
				t.Fatal(err)
			}
			defer mem.Free()
			cpu := NewCPU(*mem)
			j := NewJea9Linux(Jea9LinuxOptions{})
			cleanup := InstallJea9Linux(cpu, j)
			if !directSyscallDisabled {
				t.Fatal("InstallJea9Linux left host direct syscall dispatcher enabled")
			}
			cleanup()
			if directSyscallDisabled != startDisabled {
				t.Fatalf("directSyscallDisabled after cleanup = %v, want %v", directSyscallDisabled, startDisabled)
			}
		})
	}
}
