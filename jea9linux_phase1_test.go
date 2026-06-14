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

func TestRunDefaultBudget_ZeroBudgetUsesUnboundedRun(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	})
	defer mem.Free()
	cleanup := InstallLinuxOS(cpu, io.Discard)
	defer cleanup()

	res, err := RunDefaultBudget(cpu, &cpu.Notes, 0)
	if err != nil {
		t.Fatalf("RunDefaultBudget: %v", err)
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
	j.DisableAutoAOT = true

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
