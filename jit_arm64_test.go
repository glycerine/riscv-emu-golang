//go:build arm64

package riscv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestARM64_RV8IC_MatchesInterpreter(t *testing.T) {
	elfPath := filepath.Join(rvTestsDir, "rv64ui-p-add")
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skip("rv64ui-p-add not found")
	}

	interpCPU, interpMem := newARM64ICTestCPU(t, data)
	defer interpMem.Free()
	interpCode, interpErr := RunWithOS(interpCPU)
	if interpErr != nil {
		t.Fatalf("interpreter: %v", interpErr)
	}
	if interpCode != 0 {
		t.Fatalf("interpreter failed: exit %d", interpCode)
	}

	jitCPU, jitMem := newARM64ICTestCPU(t, data)
	defer jitMem.Free()
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	jitCPU.Notes.Push(o.Handle)

	jit := NewSandboxJIT()
	jit.SetRegPolicy(PolicyRV8)

	err = jit.RunJIT(jitCPU)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 0 {
			t.Fatalf("RV8 JIT failed: exit %d", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RV8 JIT: %v", err)
	}

	interpIC := interpCPU.RiscvInstrBegun()
	jitIC := jitCPU.RiscvInstrBegun()
	t.Logf("interpreter IC=%d, ARM64 RV8 JIT IC=%d", interpIC, jitIC)
	if interpIC == 0 || jitIC == 0 {
		t.Fatalf("zero IC: interpreter=%d jit=%d", interpIC, jitIC)
	}
	if interpIC != jitIC {
		t.Fatalf("IC mismatch: interpreter=%d jit=%d", interpIC, jitIC)
	}
}

func newARM64ICTestCPU(t *testing.T, elfData []byte) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)
	return cpu, mem
}
