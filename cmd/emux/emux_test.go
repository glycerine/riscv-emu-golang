package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

func TestRunEmuxRunsGoHelloFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/hello.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "hello jea9linux go\n"; got != want {
		t.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
	}
}

func TestRunEmuxReturnsGuestExitCodeAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/nilpanic.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "panic: runtime error") {
		t.Fatalf("stderr = %q, want Go panic text", stderr.String())
	}
}

func TestRunEmuxSeedControlsGetrandom(t *testing.T) {
	first := runEmuxFixtureOutput(t, 1234)
	second := runEmuxFixtureOutput(t, 1234)
	third := runEmuxFixtureOutput(t, 5678)

	if first != second {
		t.Fatalf("same seed output differs: %q != %q", first, second)
	}
	if first == third {
		t.Fatalf("different seeds produced matching output: %q", first)
	}
}

func TestRunEmuxJITRunsJea9LinuxFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/elf/write_stdout.elf",
		MemorySize:        riscv.Size64MB,
		InstructionBudget: 1 << 20,
		JIT:               true,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "jea9linux stdout\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestEmuxConfigDefaultsPreserveExplicitZeroClock(t *testing.T) {
	cfg := EmuxConfig{}.withDefaults()
	if cfg.MemorySize != defaultEmuxMemorySize {
		t.Fatalf("MemorySize = %d, want %d", cfg.MemorySize, defaultEmuxMemorySize)
	}
	if cfg.InstructionBudget != defaultEmuxInstructionBudget {
		t.Fatalf("InstructionBudget = %d, want %d", cfg.InstructionBudget, defaultEmuxInstructionBudget)
	}
	if cfg.ClockMode != defaultEmuxClockMode {
		t.Fatalf("ClockMode = %q, want %q", cfg.ClockMode, defaultEmuxClockMode)
	}
	if cfg.MonotonicStartNS != defaultEmuxMonotonicStartNS {
		t.Fatalf("MonotonicStartNS = %d, want %d", cfg.MonotonicStartNS, defaultEmuxMonotonicStartNS)
	}

	explicitZero := EmuxConfig{MonotonicStartSet: true}.withDefaults()
	if explicitZero.MonotonicStartNS != 0 {
		t.Fatalf("explicit MonotonicStartNS = %d, want zero preserved", explicitZero.MonotonicStartNS)
	}
}

func TestParseClockModeAndSeedBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want riscv.Jea9LinuxClockMode
	}{
		{name: "idle-jump", want: riscv.Jea9ClockIdleJump},
		{name: "idlejump", want: riscv.Jea9ClockIdleJump},
		{name: "ic-tick", want: riscv.Jea9ClockICTick},
		{name: "ictick", want: riscv.Jea9ClockICTick},
		{name: "manual", want: riscv.Jea9ClockManual},
	} {
		got, err := parseClockMode(tc.name)
		if err != nil {
			t.Fatalf("parseClockMode(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("parseClockMode(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := parseClockMode("host-time"); err == nil {
		t.Fatal("parseClockMode(host-time) returned nil error")
	}

	const seed = uint64(0x0102030405060708)
	if got := binary.LittleEndian.Uint64(seedBytes(seed)); got != seed {
		t.Fatalf("seedBytes round trip = %#x, want %#x", got, seed)
	}
}

func runEmuxFixtureOutput(t *testing.T, seed uint64) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/cryptorand.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Seed:              seed,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	return stdout.String()
}
