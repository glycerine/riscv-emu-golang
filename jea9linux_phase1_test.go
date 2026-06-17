package riscv

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func testLoopCPU(t *testing.T, budget uint64) (*CPU, *GuestMemory, *Jea9Linux) {
	t.Helper()
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1
		jenc(0, -4),               // JAL x0, 0x1000
	})
	os := NewJea9Linux(Jea9LinuxOptions{InstructionBudget: budget})
	return cpu, mem, os
}

func TestJea9Linux_DefaultOptions(t *testing.T) {
	os := NewJea9Linux(Jea9LinuxOptions{})

	if os.ClockMode() != Jea9ClockIdleJump {
		t.Fatalf("ClockMode() = %v, want %v", os.ClockMode(), Jea9ClockIdleJump)
	}
	if os.InstructionBudget() == 0 {
		t.Fatal("InstructionBudget() = 0, want nonzero default")
	}

	var a, b [32]byte
	NewJea9Linux(Jea9LinuxOptions{}).fillRandom(a[:])
	NewJea9Linux(Jea9LinuxOptions{}).fillRandom(b[:])
	if a != b {
		t.Fatalf("default entropy should be deterministic: %x != %x", a, b)
	}
	if a == ([32]byte{}) {
		t.Fatal("default entropy stream returned all zero bytes")
	}
}

func TestJea9Linux_EntropySeedCopied(t *testing.T) {
	seed := []byte("seed before mutation")
	os := NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed})
	seed[0] = 'S'

	var got, want [32]byte
	os.fillRandom(got[:])
	NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("seed before mutation")}).fillRandom(want[:])
	if got != want {
		t.Fatalf("mutating seed after construction changed stream: got %x want %x", got, want)
	}
}

func TestJea9Linux_PRNGRepeatableAndDifferentSeedsDiffer(t *testing.T) {
	var a, b, c [64]byte
	NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("alpha")}).fillRandom(a[:])
	NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("alpha")}).fillRandom(b[:])
	NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("beta")}).fillRandom(c[:])

	if a != b {
		t.Fatalf("same seed streams differ: %x != %x", a, b)
	}
	if a == c {
		t.Fatalf("different seed streams matched: %x", a)
	}
}

func TestRunDefaultBudget_ExpiresAtExactInstructionCount(t *testing.T) {
	cpu, mem, _ := testLoopCPU(t, 5)
	defer mem.Free()

	res, err := RunDefaultBudget(cpu, &cpu.Notes, 5)
	if err != nil {
		t.Fatalf("RunDefaultBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunDefaultBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.RiscvInstrBegun(); got != 5 {
		t.Fatalf("RiscvInstrBegun() = %d, want 5", got)
	}
	if got := cpu.Reg(1); got != 3 {
		t.Fatalf("x1 = %d, want 3", got)
	}
	if got := cpu.PC(); got != 0x1004 {
		t.Fatalf("PC = 0x%x, want 0x1004", got)
	}
}

func TestRunDefaultBudget_ExpiresInsideOutOfCacheJALTarget(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const caller = uint64(0x80000)
	const callee = uint64(0x40000)
	storeInsns(mem, caller, []uint32{
		jenc(1, int32(int64(callee)-int64(caller))), // JAL ra, callee outside the initial decoder cache.
		ienc(opOPIMM, 0, 7, 0, 7),                   // ADDI x7, x0, 7; must not run before callee returns.
		instrEBREAK,
	})
	storeInsns(mem, callee, []uint32{
		ienc(opOPIMM, 0, 5, 0, 1), // ADDI x5, x0, 1
		ienc(opOPIMM, 0, 6, 0, 2), // ADDI x6, x0, 2
		ienc(opJALR, 0, 0, 1, 0),  // JALR x0, 0(ra)
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(caller)
	res, err := RunDefaultBudget(cpu, &cpu.Notes, 2)
	if err != nil {
		t.Fatalf("RunDefaultBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunDefaultBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.PC(); got != callee+4 {
		t.Fatalf("PC = 0x%x, want callee continuation 0x%x", got, callee+4)
	}
	if got := cpu.Reg(1); got != caller+4 {
		t.Fatalf("ra = 0x%x, want 0x%x", got, caller+4)
	}
	if got := cpu.Reg(5); got != 1 {
		t.Fatalf("x5 = %d, want 1", got)
	}
	if got := cpu.Reg(6); got != 0 {
		t.Fatalf("x6 = %d, want 0 before second callee instruction", got)
	}
	if got := cpu.Reg(7); got != 0 {
		t.Fatalf("x7 = %d, want 0 before caller continuation", got)
	}
}

func TestRunDefaultBudget_CSRSeesInstructionAttemptsInCurrentBatch(t *testing.T) {
	csrrsCycle := ienc(opSYSTEM, 2, 3, 0, 0xC00) // CSRRS x3, cycle, x0
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1), // ADDI x1, x0, 1
		ienc(opOPIMM, 0, 2, 0, 2), // ADDI x2, x0, 2
		csrrsCycle,                // x3 = attempt count before this instruction
		instrECALL,
	})
	defer mem.Free()

	res, err := RunDefaultBudget(cpu, &cpu.Notes, 3)
	if err != nil {
		t.Fatalf("RunDefaultBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunDefaultBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.Reg(3); got != 2 {
		t.Fatalf("cycle CSR x3 = %d, want 2 instruction attempts before CSR", got)
	}
	if got := cpu.RiscvInstrBegun(); got != 3 {
		t.Fatalf("RiscvInstrBegun() = %d, want 3", got)
	}
	if got := cpu.PC(); got != 0x100c {
		t.Fatalf("PC = 0x%x, want 0x100c", got)
	}
}

func TestRunDefaultBudget_ZeroBudgetUsesUnboundedRun(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	})
	defer mem.Free()
	cleanup := InstallLinuxOS(cpu, io.Discard)
	defer cleanup()

	res, err := RunDefaultBudget(cpu, &cpu.Notes, 0)
	var ex *ExitError
	if !errors.As(err, &ex) || ex.Code != 0 {
		t.Fatalf("RunDefaultBudget error = %v, want exit status 0", err)
	}
	if res != RunBudgetExit {
		t.Fatalf("RunDefaultBudget result = %v, want RunBudgetExit", res)
	}
}

func TestJea9Linux_RunBudgetLoopReturnsBudget(t *testing.T) {
	cpu, mem, os := testLoopCPU(t, 7)
	defer mem.Free()

	err := os.Run(cpu)
	if !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("Run error = %v, want ErrJea9LinuxBudget", err)
	}
	if got := os.BudgetYields(); got != 1 {
		t.Fatalf("BudgetYields() = %d, want 1", got)
	}
	if got := cpu.RiscvInstrBegun(); got != 7 {
		t.Fatalf("RiscvInstrBegun() = %d, want 7", got)
	}
	if got := cpu.Reg(1); got != 4 {
		t.Fatalf("x1 = %d, want 4", got)
	}
}

func TestJea9Linux_ICTickAdvancesAtBudgetBoundary(t *testing.T) {
	cpu, mem, os := testLoopCPU(t, 6)
	defer mem.Free()
	os.SetClockMode(Jea9ClockICTick)
	os.SetNSPerInstruction(7)

	err := os.Run(cpu)
	if !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("Run error = %v, want ErrJea9LinuxBudget", err)
	}
	if got, want := os.MonotonicNS(), int64(42); got != want {
		t.Fatalf("MonotonicNS() = %d, want %d", got, want)
	}
}

func TestJITStepBlockBudget_ExpiresAtExactInstructionCount(t *testing.T) {
	cpu, mem, _ := testLoopCPU(t, 5)
	defer mem.Free()

	j := NewJIT()
	defer j.Close()

	res, err := j.StepBlockBudget(cpu, 5)
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
		t.Fatalf("x1 = %d, want 3", got)
	}
	if got := cpu.PC(); got != 0x1004 {
		t.Fatalf("PC = 0x%x, want 0x1004", got)
	}
}

func TestJITStepBlockBudget_UsesCumulativeInstructionCounter(t *testing.T) {
	cpu, mem, _ := testLoopCPU(t, 5)
	defer mem.Free()

	j := NewJIT()
	defer j.Close()

	for slice := 1; slice <= 2; slice++ {
		sliceStart := cpu.RiscvInstrBegun()
		for {
			used := cpu.RiscvInstrBegun() - sliceStart
			if used >= 5 {
				t.Fatalf("slice %d used %d instructions without budget expiry", slice, used)
			}
			res, err := j.StepBlockBudget(cpu, 5-used)
			if err != nil {
				t.Fatalf("slice %d StepBlockBudget: %v", slice, err)
			}
			if res == RunBudgetExpired {
				break
			}
			after := cpu.RiscvInstrBegun()
			if after == sliceStart+used {
				t.Fatalf("slice %d StepBlockBudget made no progress", slice)
			}
		}
	}
	if got := cpu.RiscvInstrBegun(); got != 10 {
		t.Fatalf("RiscvInstrBegun() after two slices = %d, want 10", got)
	}
	if got := cpu.Reg(1); got != 5 {
		t.Fatalf("x1 after two slices = %d, want 5", got)
	}
	if got := cpu.PC(); got != 0x1000 {
		t.Fatalf("PC after two slices = 0x%x, want 0x1000", got)
	}
}

func TestJITStepBlockBudget_ChangingBudgetDoesNotRecompile(t *testing.T) {
	cpu, mem, _ := testLoopCPU(t, 10)
	defer mem.Free()

	j := NewJIT()
	defer j.Close()

	res, err := j.StepBlockBudget(cpu, 4)
	if err != nil {
		t.Fatalf("first StepBlockBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("first StepBlockBudget result = %v, want RunBudgetExpired", res)
	}
	compiles := j.DispatchCompile
	if compiles == 0 {
		t.Fatal("first StepBlockBudget did not compile a block")
	}
	if got := cpu.PC(); got != 0x1000 {
		t.Fatalf("PC after first slice = 0x%x, want 0x1000", got)
	}

	res, err = j.StepBlockBudget(cpu, 6)
	if err != nil {
		t.Fatalf("second StepBlockBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("second StepBlockBudget result = %v, want RunBudgetExpired", res)
	}
	if got := j.DispatchCompile; got != compiles {
		t.Fatalf("DispatchCompile after budget change = %d, want unchanged %d", got, compiles)
	}
	if got := cpu.RiscvInstrBegun(); got != 10 {
		t.Fatalf("RiscvInstrBegun() after two slices = %d, want 10", got)
	}
	if got := cpu.Reg(1); got != 5 {
		t.Fatalf("x1 after two slices = %d, want 5", got)
	}
}

func TestJITStepBlockBudget_FusedPairTooSmallFallsBackOneInstruction(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		uenc(opAUIPC, 5, 0),       // x5 = current PC
		ienc(opOPIMM, 0, 5, 5, 7), // x5 += 7; AUIPC+ADDI fusion when budget allows
		instrEBREAK,
	})
	defer mem.Free()

	j := NewJIT()
	defer j.Close()

	res, err := j.StepBlockBudget(cpu, 1)
	if err != nil {
		t.Fatalf("StepBlockBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("StepBlockBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.RiscvInstrBegun(); got != 1 {
		t.Fatalf("RiscvInstrBegun() = %d, want 1", got)
	}
	if got := cpu.PC(); got != 0x1004 {
		t.Fatalf("PC = 0x%x, want 0x1004", got)
	}
	if got := cpu.Reg(5); got != 0x1000 {
		t.Fatalf("x5 = 0x%x, want 0x1000", got)
	}
}

func TestJITStepBlockBudget_FusedPairRunsWhenBudgetFits(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		uenc(opAUIPC, 5, 0),
		ienc(opOPIMM, 0, 5, 5, 7),
		instrEBREAK,
	})
	defer mem.Free()

	j := NewJIT()
	defer j.Close()

	res, err := j.StepBlockBudget(cpu, 2)
	if err != nil {
		t.Fatalf("StepBlockBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("StepBlockBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.RiscvInstrBegun(); got != 2 {
		t.Fatalf("RiscvInstrBegun() = %d, want 2", got)
	}
	if got := cpu.PC(); got != 0x1008 {
		t.Fatalf("PC = 0x%x, want 0x1008", got)
	}
	if got := cpu.Reg(5); got != 0x1007 {
		t.Fatalf("x5 = 0x%x, want 0x1007", got)
	}
}

func TestJITStepBlockBudget_FusedTripleTooSmallFallsBackOneInstruction(t *testing.T) {
	const opOPIMM32 = uint32(0x1B)
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM32, 0, 10, 10, 5), // ADDIW x10, x10, 5
		ienc(opOPIMM, 1, 10, 10, 32),  // SLLI  x10, x10, 32
		ienc(opOPIMM, 5, 10, 10, 32),  // SRLI  x10, x10, 32
		instrEBREAK,
	})
	defer mem.Free()
	cpu.SetReg(10, 1)

	j := NewJIT()
	defer j.Close()

	res, err := j.StepBlockBudget(cpu, 2)
	if err != nil {
		t.Fatalf("StepBlockBudget: %v", err)
	}
	if res != RunBudgetContinue {
		t.Fatalf("StepBlockBudget result = %v, want RunBudgetContinue", res)
	}
	if got := cpu.RiscvInstrBegun(); got != 1 {
		t.Fatalf("RiscvInstrBegun() = %d, want 1", got)
	}
	if got := cpu.PC(); got != 0x1004 {
		t.Fatalf("PC = 0x%x, want 0x1004", got)
	}
	if got := cpu.Reg(10); got != 6 {
		t.Fatalf("x10 = %d, want 6", got)
	}
}

func TestJITStepBlockBudget_FusedTripleRunsWhenBudgetFits(t *testing.T) {
	const opOPIMM32 = uint32(0x1B)
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM32, 0, 10, 10, 5),
		ienc(opOPIMM, 1, 10, 10, 32),
		ienc(opOPIMM, 5, 10, 10, 32),
		instrEBREAK,
	})
	defer mem.Free()
	cpu.SetReg(10, 1)

	j := NewJIT()
	defer j.Close()

	res, err := j.StepBlockBudget(cpu, 3)
	if err != nil {
		t.Fatalf("StepBlockBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("StepBlockBudget result = %v, want RunBudgetExpired", res)
	}
	if got := cpu.RiscvInstrBegun(); got != 3 {
		t.Fatalf("RiscvInstrBegun() = %d, want 3", got)
	}
	if got := cpu.PC(); got != 0x100c {
		t.Fatalf("PC = 0x%x, want 0x100c", got)
	}
	if got := cpu.Reg(10); got != 6 {
		t.Fatalf("x10 = %d, want 6", got)
	}
}

func TestJea9Linux_ManualClockAdvance(t *testing.T) {
	os := NewJea9Linux(Jea9LinuxOptions{
		ClockMode:        Jea9ClockManual,
		MonotonicStartNS: 10,
	})

	os.AdvanceTime(15 * time.Nanosecond)
	if got := os.MonotonicNS(); got != 25 {
		t.Fatalf("MonotonicNS() = %d, want 25", got)
	}
	os.SetMonotonicNS(100)
	if got := os.MonotonicNS(); got != 100 {
		t.Fatalf("MonotonicNS() after set = %d, want 100", got)
	}
}

func TestJea9Linux_InstallHandlesExitSyscall(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 10, 0, 23), // a0 = 23
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	})
	defer mem.Free()

	os := NewJea9Linux(Jea9LinuxOptions{})
	cleanup := InstallJea9Linux(cpu, os)
	defer cleanup()

	err := RunDefault(cpu, &cpu.Notes)
	var ex *ExitError
	if !errors.As(err, &ex) {
		t.Fatalf("RunDefault error = %v, want ExitError", err)
	}
	if ex.Code != 23 {
		t.Fatalf("exit code = %d, want 23", ex.Code)
	}
}

func TestJea9Linux_DefaultWritersDiscard(t *testing.T) {
	os := NewJea9Linux(Jea9LinuxOptions{})
	if os.stdout == nil || os.stderr == nil {
		t.Fatal("default stdout/stderr must be non-nil")
	}

	var out bytes.Buffer
	os = NewJea9Linux(Jea9LinuxOptions{Stdout: &out})
	if _, err := os.stdout.Write([]byte("ok")); err != nil {
		t.Fatalf("stdout write: %v", err)
	}
	if out.String() != "ok" {
		t.Fatalf("stdout = %q, want ok", out.String())
	}
}
