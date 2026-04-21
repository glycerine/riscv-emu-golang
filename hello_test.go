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
	entry, err := LoadELFBytes(mem, data)
	if err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
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

// TestHelloGoCPU_JIT runs the same ELF through the JIT (with its
// existing Go-path ECALL handling). Once Phase 2 lands, this will
// exercise the direct-SYSCALL fast path.
func TestHelloGoCPU_JIT(t *testing.T) {
	data, err := os.ReadFile("bench/hello_guest/hello_gocpu.elf")
	if err != nil {
		t.Skipf("bench/hello_guest/hello_gocpu.elf: %v", err)
	}

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	entry, err := LoadELFBytes(mem, data)
	if err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetReg(2, 0x03F00000)

	var out bytes.Buffer
	cleanup := InstallLinuxOS(cpu, &out)
	defer cleanup()

	j := NewJIT()

	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(*ExitError); ok {
					return
				}
				panic(r)
			}
		}()
		runErr = j.RunJIT(cpu)
	}()
	if runErr != nil {
		t.Fatalf("RunJIT: %v", runErr)
	}

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if out.Len() != len(want) {
		t.Fatalf("output length = %d, want %d", out.Len(), len(want))
	}
	if out.String() != want {
		t.Fatalf("output mismatch (same length, different content)")
	}
}

