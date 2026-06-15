package riscv

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestHelloGoCPU runs bench/hello_guest/hello_gocpu.elf through the
// uncached interpreter with a Linux OS personality, asserts exit 0
// and that the captured stdout is "Hello, Go CPU!\n" repeated 10000
// times.
//
// The ELF is produced by `make hello-elfs` (zig cc freestanding RV64).
//
// The interpreter routes ECALL through Go (NoteChain →
// LinuxWriteHandler → our WriteFunc), so we capture output via an
// in-process bytes.Buffer.
func TestHelloGoCPU(t *testing.T) {
	data, err := os.ReadFile("bench/hello_guest/hello_gocpu.elf")
	if err != nil {
		t.Skipf("bench/hello_guest/hello_gocpu.elf: %v "+
			"— run `make hello-elfs` to build", err)
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
	cpu.SetReg(2, 0x03F00000) // sp near top of 64 MiB

	var out bytes.Buffer
	exitCode, err := RunWithLinuxOS(cpu, &out)
	if err != nil {
		t.Fatalf("RunWithLinuxOS: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if out.Len() != len(want) {
		t.Fatalf("output length = %d, want %d", out.Len(), len(want))
	}
	if out.String() != want {
		t.Fatalf("output mismatch (same length, different content)")
	}
}

func TestHelloGoCPU_JITWithLinuxOS(t *testing.T) {
	data, err := os.ReadFile("bench/hello_guest/hello_gocpu.elf")
	if err != nil {
		t.Skipf("bench/hello_guest/hello_gocpu.elf: %v", err)
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

	var out bytes.Buffer
	cleanup := InstallLinuxOS(cpu, &out)
	defer cleanup()

	j := NewJIT()
	runErr := j.RunJIT(cpu)
	if runErr != nil {
		if _, ok := runErr.(*ExitError); !ok {
			t.Fatalf("RunJIT: %v", runErr)
		}
	}

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if out.Len() != len(want) {
		t.Fatalf("output length = %d, want %d", out.Len(), len(want))
	}
	if out.String() != want {
		t.Fatalf("output mismatch (same length, different content)")
	}
}
