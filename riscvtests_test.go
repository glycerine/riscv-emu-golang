package riscv

// riscvtests_test.go — runs the official riscv-tests ELF binaries.
//
// The riscv-tests suite uses the following ECALL convention (machine-mode):
//   a7=93 (exit syscall), a0=0 => PASS
//   a7=93, a0=(testnum<<1)|1 => FAIL test number (testnum)
//
// Each ELF is a bare-metal binary linked at 0x80000000 with a reset vector
// that sets up minimal CSRs then falls through to the test code.
// We load it, run it, and check the exit code.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rvTestsDir = "/tmp/rvtests"

func runRISCVTest(t *testing.T, elfPath string) {
	t.Helper()

	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s (run make riscv-tests first)", elfPath)
		return
	}

	mem, merr := NewGuestMemory(Size4GB) // riscv-tests link at 0x80000000
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	entry, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)

	exitCode, err := RunWithOS(cpu)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if exitCode != 0 {
		testNum := exitCode >> 1
		t.Errorf("FAILED: test number %d (exit code %d)", testNum, exitCode)
	}
}

func TestRISCVTests_UI(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runRISCVTest(t, path)
		})
	}
}

func TestRISCVTests_UM(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) {
			runRISCVTest(t, path)
		})
	}
}

// quick sanity — runs just one test so CI doesn't need the full suite
func TestRISCVTests_Smoke(t *testing.T) {
	path := filepath.Join(rvTestsDir, "rv64ui-p-add")
	if _, err := os.Stat(path); err != nil {
		t.Skip("rv64ui-p-add not found")
	}
	runRISCVTest(t, path)
	fmt.Println("rv64ui-p-add: PASS")
}

func TestRISCVTests_UA(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

func TestRISCVTests_UF(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uf-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uf ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uf-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

func TestRISCVTests_UD(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ud-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ud ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ud-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

func TestRISCVTests_UC(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}
