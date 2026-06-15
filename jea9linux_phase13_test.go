package riscv

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type jea9LinuxELFRunResult struct {
	code   int
	stdout string
	stderr string
	trace  Jea9LinuxTraceSnapshot
}

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
	jit := NewJIT()
	defer jit.Close()
	cleanup := InstallJea9LinuxJIT(cpu, jit, j)
	defer cleanup()
	err := jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		return ex.Code, nil
	}
	return cpu.ExitCode, err
}

func runJea9LinuxELFFixture(t *testing.T, path string, useJIT bool, opts Jea9LinuxOptions) jea9LinuxELFRunResult {
	t.Helper()
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
	const stackTop = uint64(0x03F00000)
	cpu.SetReg(2, stackTop)
	var stdout, stderr bytes.Buffer
	opts.Stdout = &stdout
	opts.Stderr = &stderr
	j := NewJea9Linux(opts)
	if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{StackTop: stackTop}); err != nil {
		t.Fatalf("InitELFStack: %v", err)
	}
	var code int
	if useJIT {
		jit := NewJIT()
		defer jit.Close()
		cleanup := InstallJea9LinuxJIT(cpu, jit, j)
		defer cleanup()
		err = jit.RunJIT(cpu)
		if ex, ok := err.(*ExitError); ok {
			code = ex.Code
			err = nil
		} else {
			code = cpu.ExitCode
		}
	} else {
		code, err = RunWithJea9Linux(cpu, j)
	}
	if err != nil {
		t.Fatalf("run useJIT=%v: %v", useJIT, err)
	}
	return jea9LinuxELFRunResult{
		code:   code,
		stdout: stdout.String(),
		stderr: stderr.String(),
		trace:  j.TraceSnapshot(),
	}
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

func TestJea9Linux_JITBudgetReturnPreservesState(t *testing.T) {
	cpu, mem, _ := testLoopCPU(t, 5)
	defer mem.Free()

	jit := NewJIT()
	defer jit.Close()

	res, err := jit.StepBlockBudget(cpu, 5)
	if err != nil {
		t.Fatalf("StepBlockBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("StepBlockBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.RiscvInstrBegun(); got != 5 {
		t.Fatalf("RiscvInstrBegun() = %d, want 5", got)
	}
	if got := cpu.Reg(1); got != 3 {
		t.Fatalf("x1 after budget return = %d, want 3", got)
	}
	if got := cpu.PC(); got != 0x1004 {
		t.Fatalf("PC after budget return = 0x%x, want 0x1004", got)
	}

	if got := cpu.Reg(0); got != 0 {
		t.Fatalf("x0 after budget return = %d, want 0", got)
	}
}

func TestJea9Linux_JITFutexWaitWake(t *testing.T) {
	path := "testvectors/jea9linux/elf/futex_wait_wake.elf"
	interp := runJea9LinuxELFFixture(t, path, false, Jea9LinuxOptions{})
	jit := runJea9LinuxELFFixture(t, path, true, Jea9LinuxOptions{})
	if interp.code != 0 || jit.code != 0 {
		t.Fatalf("exit codes interp=%d jit=%d, want both 0", interp.code, jit.code)
	}
	if interp.stdout != jit.stdout || interp.stderr != jit.stderr {
		t.Fatalf("fixture output mismatch:\ninterp stdout=%q stderr=%q\njit stdout=%q stderr=%q",
			interp.stdout, interp.stderr, jit.stdout, jit.stderr)
	}
	if !reflect.DeepEqual(interp.trace.Syscalls, jit.trace.Syscalls) {
		t.Fatalf("futex syscall trace mismatch:\ninterp=%+v\njit=%+v", interp.trace.Syscalls, jit.trace.Syscalls)
	}
	if !reflect.DeepEqual(interp.trace.Schedule, jit.trace.Schedule) {
		t.Fatalf("futex schedule trace mismatch:\ninterp=%+v\njit=%+v", interp.trace.Schedule, jit.trace.Schedule)
	}
}

func TestJea9Linux_JITAllCheckedInELFFixtures(t *testing.T) {
	paths, err := filepath.Glob("testvectors/jea9linux/elf/*.elf")
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no jea9linux ELF fixtures found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			opts := Jea9LinuxOptions{
				Stdin: bytes.NewBufferString("fixture input\n"),
			}
			if filepath.Base(path) == "sysinfo_basic.elf" {
				opts.MonotonicStartNS = 42_000_000_001
			}
			result := runJea9LinuxELFFixture(t, path, true, opts)
			if result.code != 0 {
				t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q",
					result.code, result.stdout, result.stderr)
			}
		})
	}
}

func TestJea9Linux_JITReplayMatchesInterpreterTrace(t *testing.T) {
	path := "testvectors/jea9linux/elf/getrandom_repeat.elf"
	opts := Jea9LinuxOptions{EntropySeed: []byte("jit replay trace")}
	interp := runJea9LinuxELFFixture(t, path, false, opts)
	jit := runJea9LinuxELFFixture(t, path, true, opts)
	if !reflect.DeepEqual(interp, jit) {
		t.Fatalf("interpreter/JIT replay mismatch:\ninterp=%+v\njit=%+v", interp, jit)
	}
}

func TestJea9Linux_JITFreshAfterSyscallModeChange(t *testing.T) {
	withDirectEcallEnabled(t)

	const (
		codeVA = uint64(0x1000)
		msgVA  = uint64(0x3000)
	)
	msg := []byte("fresh jea9linux policy\n")
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

	jit := NewJIT()
	defer jit.Close()

	hostWarmup := captureStdout(t, func() {
		if _, err := jit.StepBlock(cpu); err != nil {
			t.Fatalf("initial host-policy StepBlock: %v", err)
		}
	})
	if string(hostWarmup) != string(msg) {
		t.Fatalf("host warmup stdout = %q, want %q", hostWarmup, msg)
	}
	if cpu.PC() != codeVA+20 {
		t.Fatalf("PC after initial StepBlock = 0x%x, want post-write 0x%x", cpu.PC(), codeVA+20)
	}

	cpu.SetPC(codeVA)
	var guestStdout bytes.Buffer
	j := NewJea9Linux(Jea9LinuxOptions{Stdout: &guestStdout})
	var err error
	hostAfterInstall := captureStdout(t, func() {
		cleanup := InstallJea9LinuxJIT(cpu, jit, j)
		err = jit.RunJIT(cpu)
		cleanup()
	})
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 0 {
			t.Fatalf("exit code = %d, want 0 after policy change", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RunJIT after policy change: %v", err)
	} else if cpu.ExitCode != 0 {
		t.Fatalf("cpu.ExitCode = %d, want 0 after policy change", cpu.ExitCode)
	}
	if guestStdout.String() != string(msg) {
		t.Fatalf("jea9linux stdout = %q, want %q", guestStdout.String(), msg)
	}
	if len(hostAfterInstall) != 0 {
		t.Fatalf("host stdout after jea9linux install = %q, want none", hostAfterInstall)
	}
	if got := jit.PersonalityEcallCount(); got != 2 {
		t.Fatalf("PersonalityEcallCount() = %d, want write+exit after fresh policy", got)
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

func TestJea9Linux_InstallJITDoesNotMutateGlobalDirectSyscallPolicy(t *testing.T) {
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
			jit := NewJIT()
			j := NewJea9Linux(Jea9LinuxOptions{})
			cleanup := InstallJea9LinuxJIT(cpu, jit, j)
			if directSyscallDisabled != startDisabled {
				t.Fatalf("InstallJea9LinuxJIT changed directSyscallDisabled to %v, want %v", directSyscallDisabled, startDisabled)
			}
			cleanup()
			if directSyscallDisabled != startDisabled {
				t.Fatalf("directSyscallDisabled after cleanup = %v, want %v", directSyscallDisabled, startDisabled)
			}
		})
	}
}

func TestJea9Linux_InstallJITRestoresPerJITSyscallPolicy(t *testing.T) {
	original := directSyscallDisabled
	t.Cleanup(func() { directSyscallDisabled = original })
	directSyscallDisabled = false

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	jit := NewJIT()
	before := jit.currentSyscallDispatcherAddr()
	if before == 0 && DirectSyscallEnabled() {
		t.Fatal("test requires a nonzero inherited direct syscall dispatcher")
	}

	j := NewJea9Linux(Jea9LinuxOptions{})
	cleanup := InstallJea9LinuxJIT(cpu, jit, j)
	if got := jit.currentSyscallDispatcherAddr(); got != 0 {
		t.Fatalf("installed jea9linux JIT dispatcher addr = 0x%x, want 0", got)
	}
	cleanup()
	if got := jit.currentSyscallDispatcherAddr(); got != before {
		t.Fatalf("restored JIT dispatcher addr = 0x%x, want 0x%x", got, before)
	}
}

func TestJea9Linux_JITPersonalityCalloutBypassesEcallNoteChain(t *testing.T) {
	withDirectEcallEnabled(t)

	const codeVA = uint64(0x1000)
	insns := []uint32{
		ienc(opOPIMM, 0, 17, 0, 172), // a7 = getpid
		instrECALL,
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit, a0 remains getpid
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()

	jit := NewJIT()
	j := NewJea9Linux(Jea9LinuxOptions{PID: 19, TID: 19})
	cleanup := InstallJea9LinuxJIT(cpu, jit, j)
	defer cleanup()

	ecallNotes := 0
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if IsEcall(n) {
			ecallNotes++
			return NoteFatal
		}
		return NoteForward
	})
	defer cpu.Notes.Pop()

	err := jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 19 {
			t.Fatalf("exit code = %d, want virtual pid 19", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RunJIT: %v", err)
	} else if cpu.ExitCode != 19 {
		t.Fatalf("cpu.ExitCode = %d, want virtual pid 19", cpu.ExitCode)
	}
	if ecallNotes != 0 {
		t.Fatalf("ECALL note chain saw %d ECALL notes, want direct JIT personality callout", ecallNotes)
	}
	if got := jit.PersonalityEcallCount(); got != 2 {
		t.Fatalf("PersonalityEcallCount() = %d, want getpid+exit", got)
	}
}
