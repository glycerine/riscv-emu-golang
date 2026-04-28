package riscv

import (
	"bytes"
	"io"
	"os"
	"strings"
	"syscall"
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
// in-process bytes.Buffer. The Phase-2 direct-SYSCALL path needs a
// different capture strategy — see TestHelloGoCPU_JIT_DirectSyscall.
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

// TestHelloGoCPU_JIT_DirectSyscall runs the same ELF through the JIT
// with the native SYSCALL fast path active. Since the fast path
// bypasses Go entirely (it issues a real kernel write(2)), we
// capture by redirecting the host process's fd=1 to a temp file
// with syscall.Dup2.
//
// The exit syscall still falls back to the Go path (our Phase-2
// dispatcher only handles write); that's what raises *ExitError,
// which RunWithLinuxOS-equivalent logic catches here.
func TestHelloGoCPU_JIT_DirectSyscall(t *testing.T) {
	if !DirectSyscallEnabled() {
		t.Skip("direct syscall fast path disabled")
	}

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

	// The Go OS layer is still installed for exit(93/94) — the
	// native dispatcher returns 1 on unknown syscalls so exit
	// falls through to NoteChain.Deliver → LinuxExit → panic.
	cleanup := InstallLinuxOS(cpu, io.Discard)
	defer cleanup()

	j := NewJIT()

	captured := captureStdout(t, func() {
		runErr := j.RunJIT(cpu)
		if runErr != nil {
			if _, ok := runErr.(*ExitError); !ok {
				t.Fatalf("RunJIT: %v", runErr)
			}
		}
	})

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if len(captured) != len(want) {
		t.Fatalf("captured length = %d, want %d", len(captured), len(want))
	}
	if string(captured) != want {
		t.Fatalf("captured mismatch (same length, different content)")
	}
}

// captureStdout runs fn with the process's fd=1 redirected to a
// temporary file, returning the captured bytes. Intended for
// verifying output produced by direct-SYSCALL writes in tests.
//
// Restores the original fd=1 before returning. Fails the test via
// t.Fatal on any redirection error.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	tmpf, err := os.CreateTemp("", "hellocap-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpf.Name())
	defer tmpf.Close()

	saved, err := syscall.Dup(1)
	if err != nil {
		t.Fatalf("dup(1): %v", err)
	}
	defer syscall.Close(saved)

	if err := syscall.Dup2(int(tmpf.Fd()), 1); err != nil {
		t.Fatalf("dup2(tmpf, 1): %v", err)
	}
	restoreDone := false
	defer func() {
		if !restoreDone {
			syscall.Dup2(saved, 1)
		}
	}()

	fn()

	// Restore fd=1 before reading — some readers may flush via fmt.
	if err := syscall.Dup2(saved, 1); err != nil {
		t.Fatalf("dup2(restore): %v", err)
	}
	restoreDone = true

	// Seek to 0 and read everything.
	if _, err := tmpf.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tmpf)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
