package riscv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runABJITWithOS(cpu *CPU) (exitCode int, err error) {
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	defer cpu.Notes.Pop()

	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*ExitError); ok {
				exitCode = ex.Code
				err = nil
				return
			}
			panic(r)
		}
	}()

	jit := NewJIT()
	jit.SetRegPolicy(PolicyABJIT)
	err = jit.RunJIT(cpu)
	return
}

func runABJITRISCVTest(t *testing.T, elfPath string) {
	t.Helper()
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s", elfPath)
		return
	}
	mem, merr := NewGuestMemory(Size4GB)
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	elf, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)

	exitCode, err := runABJITWithOS(cpu)
	if err != nil {
		t.Fatalf("RunJIT(abjit): %v", err)
	}
	if exitCode != 0 {
		testNum := exitCode >> 1
		t.Errorf("FAILED: test number %d (exit code %d)", testNum, exitCode)
	}
}

func TestABJIT_SingleBlock_ADD(t *testing.T) {
	insns := []uint32{
		0x00700093, // addi x1, x0, 7
		0x02300113, // addi x2, x0, 35
		0x002081b3, // add x3, x1, x2
		0x00000073, // ecall
	}
	mem, merr := NewGuestMemory(Size1MB)
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	codeVA := uint64(0x1000)
	storeInsns(mem, codeVA, insns)

	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.Notes.Push(ecallStop)
	defer cpu.Notes.Pop()

	jit := NewJIT()
	jit.SetRegPolicy(PolicyABJIT)
	_ = jit.RunJIT(cpu)

	if cpu.x[3] != 42 {
		t.Errorf("x[3] = %d, want 42", cpu.x[3])
	}
}

func TestABJIT_RISCVTests_UI(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}

func TestABJIT_RISCVTests_UM(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}

func TestABJIT_RISCVTests_UA(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}

func TestABJIT_RISCVTests_UC(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}

func BenchmarkABJIT_RISCVTest_add(b *testing.B) {
	data, err := os.ReadFile(filepath.Join(rvTestsDir, "rv64ui-p-add"))
	if err != nil {
		b.Skip("rv64ui-p-add not found")
	}
	for i := 0; i < b.N; i++ {
		mem, merr := NewGuestMemory(Size4GB)
		if merr != nil {
			b.Fatal(merr)
		}
		elf, lerr := LoadELFBytes(mem, data)
		if lerr != nil {
			mem.Free()
			b.Fatal(lerr)
		}
		cpu := NewCPU(*mem)
		cpu.SetPC(elf.Entry)
		cpu.SetWatchAddr(elf.TohostAddr)
		_, _ = runABJITWithOS(cpu)
		mem.Free()
	}
}
