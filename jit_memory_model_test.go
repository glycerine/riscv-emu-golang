package riscv

import (
	"strings"
	"testing"
)

func TestJITMemoryModelMismatchRejected(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	t.Cleanup(mem.Free)
	cpu := NewCPU(*mem)

	jit := NewJIT()
	defer jit.Close()

	_, err = jit.StepBlock(cpu)
	requireMemoryModelMismatch(t, err)

	_, _, err = jit.StepBlockDualBudget(cpu, 1, 1)
	requireMemoryModelMismatch(t, err)

	err = jit.RunJIT(cpu)
	requireMemoryModelMismatch(t, err)
}

func TestJITAOTMemoryModelMismatchRejected(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	t.Cleanup(mem.Free)

	jit := NewJIT()
	defer jit.Close()

	requireMemoryModelMismatch(t, jit.InstallAOTFromMem(mem))
	requireMemoryModelMismatch(t, jit.InstallAOT(mem, nil))
}

func TestJITMemoryModelMatchAllowsEmptyAOTInstall(t *testing.T) {
	mem, err := NewLinearGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewLinearGuestMemory: %v", err)
	}
	t.Cleanup(mem.Free)

	jit := NewJIT()
	defer jit.Close()

	if err := jit.InstallAOTFromMem(mem); err != nil {
		t.Fatalf("InstallAOTFromMem matching linear memory: %v", err)
	}
	if err := jit.InstallAOT(mem, nil); err != nil {
		t.Fatalf("InstallAOT matching linear memory: %v", err)
	}
}

func requireMemoryModelMismatch(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want memory model mismatch")
	}
	if !strings.Contains(err.Error(), "memory model") {
		t.Fatalf("error = %v, want memory model mismatch", err)
	}
}
