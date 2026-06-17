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

func TestJea9Linux_JITWriteUsesPersonalityStdout(t *testing.T) {

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
	code, err := runJITWithJea9Linux(cpu, j)
	if err != nil {
		t.Fatalf("runJITWithJea9Linux: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if guestStdout.String() != string(msg) {
		t.Fatalf("jea9linux stdout = %q, want %q", guestStdout.String(), msg)
	}
}

func TestJea9Linux_JITGettidUsesVirtualTid(t *testing.T) {

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
	opts := Jea9LinuxOptions{Trace: true}
	interp := runJea9LinuxELFFixture(t, path, false, opts)
	jit := runJea9LinuxELFFixture(t, path, true, opts)
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
	integrationOnly := map[string]string{
		"tcp_socket_client.elf": "requires a peer server and port arguments",
		"tcp_socket_server.elf": "requires client peers and port arguments",
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if reason := integrationOnly[filepath.Base(path)]; reason != "" {
				t.Skip(reason)
			}
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
	opts := Jea9LinuxOptions{EntropySeed: []byte("jit replay trace"), Trace: true}
	interp := runJea9LinuxELFFixture(t, path, false, opts)
	jit := runJea9LinuxELFFixture(t, path, true, opts)
	if !reflect.DeepEqual(interp, jit) {
		t.Fatalf("interpreter/JIT replay mismatch:\ninterp=%+v\njit=%+v", interp, jit)
	}
}

func TestJea9Linux_JITEcallTrapBoundaryRunsThroughOS(t *testing.T) {

	const codeVA = uint64(0x1000)
	insns := []uint32{
		ienc(opOPIMM, 0, 17, 0, 172), // a7 = getpid
		instrECALL,
		ienc(opOPIMM, 0, 10, 10, 1), // a0++
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
		instrEBREAK,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()

	jit := NewJIT()
	j := NewJea9Linux(Jea9LinuxOptions{PID: 19, TID: 19})
	cleanup := InstallJea9LinuxJIT(cpu, jit, j)
	defer cleanup()

	res := jit.emitBlock(&cpu.mem, codeVA)
	if res == nil {
		t.Fatalf("emitBlock returned nil")
	}
	if res.numInsns != 2 {
		t.Fatalf("lazy block decoded %d instructions, want 2 ending at ECALL trap",
			res.numInsns)
	}

	err := jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 20 {
			t.Fatalf("exit code = %d, want virtual pid+1 = 20", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RunJIT: %v", err)
	} else if cpu.ExitCode != 20 {
		t.Fatalf("cpu.ExitCode = %d, want virtual pid+1 = 20", cpu.ExitCode)
	}
}
