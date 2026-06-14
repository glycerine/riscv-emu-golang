package riscv

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const jea9LinuxGoMemorySize = Size16GB

type jea9LinuxGoRunConfig struct {
	Name    string
	Args    []string
	Env     []string
	Options Jea9LinuxOptions
}

type jea9LinuxGoRunResult struct {
	code   int
	stdout string
	stderr string
}

type jea9LinuxGoMachine struct {
	cpu    *CPU
	mem    *GuestMemory
	os     *Jea9Linux
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func TestJea9Linux_GoHello(t *testing.T) {
	result := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{Name: "hello"})
	requireJea9LinuxGoExit(t, result, 0)
	if got, want := result.stdout, "hello jea9linux go\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestJea9Linux_GoSchedAffinityOneP(t *testing.T) {
	result := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{Name: "gomaxprocs"})
	requireJea9LinuxGoExit(t, result, 0)
	if got, want := result.stdout, "1\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestJea9Linux_GoTimeNowDeterministic(t *testing.T) {
	cfg := jea9LinuxGoRunConfig{
		Name: "timenow",
		Options: Jea9LinuxOptions{
			MonotonicStartNS: 1_234_567_890,
			RealtimeOffsetNS: 10_000_000_000,
		},
	}
	result := runJea9LinuxGoFixture(t, cfg)
	requireJea9LinuxGoExit(t, result, 0)
	replay := runJea9LinuxGoFixture(t, cfg)
	requireJea9LinuxGoExit(t, replay, 0)
	if result.stdout != replay.stdout {
		t.Fatalf("time.Now output is not replay deterministic: %q != %q", result.stdout, replay.stdout)
	}
	if strings.TrimSpace(result.stdout) == "0" {
		t.Fatalf("time.Now output = %q, want nonzero deterministic time", result.stdout)
	}
}

func TestJea9Linux_GoCryptoRandDeterministic(t *testing.T) {
	first := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{
		Name:    "cryptorand",
		Options: Jea9LinuxOptions{EntropySeed: []byte("go crypto seed")},
	})
	requireJea9LinuxGoExit(t, first, 0)
	second := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{
		Name:    "cryptorand",
		Options: Jea9LinuxOptions{EntropySeed: []byte("go crypto seed")},
	})
	requireJea9LinuxGoExit(t, second, 0)
	if first.stdout != second.stdout {
		t.Fatalf("same seed crypto/rand output differs: %q != %q", first.stdout, second.stdout)
	}
	third := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{
		Name:    "cryptorand",
		Options: Jea9LinuxOptions{EntropySeed: []byte("different go crypto seed")},
	})
	requireJea9LinuxGoExit(t, third, 0)
	if first.stdout == third.stdout {
		t.Fatalf("different seeds produced matching crypto/rand output: %q", first.stdout)
	}
}

func TestJea9Linux_GoGoroutineFutexWake(t *testing.T) {
	result := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{Name: "goroutine"})
	requireJea9LinuxGoExit(t, result, 0)
	if got, want := result.stdout, "42\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestJea9Linux_GoTimerSelectIdleJump(t *testing.T) {
	result := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{
		Name:    "timerselect",
		Options: Jea9LinuxOptions{MonotonicStartNS: 1_000_000},
	})
	requireJea9LinuxGoExit(t, result, 0)
	if !strings.HasPrefix(result.stdout, "elapsed_ms=") {
		t.Fatalf("stdout = %q, want elapsed_ms prefix", result.stdout)
	}
}

func TestJea9Linux_GoNilPointerPanic(t *testing.T) {
	result := runJea9LinuxGoFixture(t, jea9LinuxGoRunConfig{Name: "nilpanic"})
	requireJea9LinuxGoExit(t, result, 2)
	if !strings.Contains(result.stderr, "panic: runtime error") ||
		!strings.Contains(result.stderr, "invalid memory address") {
		t.Fatalf("stderr = %q, want Go nil pointer panic", result.stderr)
	}
}

func TestJea9Linux_GoMathRandStartupUnaffectedByHost(t *testing.T) {
	cfg := jea9LinuxGoRunConfig{
		Name:    "mathrand",
		Options: Jea9LinuxOptions{EntropySeed: []byte("math rand replay seed")},
	}
	first := runJea9LinuxGoFixture(t, cfg)
	requireJea9LinuxGoExit(t, first, 0)
	second := runJea9LinuxGoFixture(t, cfg)
	requireJea9LinuxGoExit(t, second, 0)
	if first.stdout != second.stdout {
		t.Fatalf("math/rand startup output is not replay deterministic: %q != %q", first.stdout, second.stdout)
	}
}

func TestJea9Linux_GoManualClockTimerBlocksUntilAdvance(t *testing.T) {
	t.Skip("pending: Go manual-clock timers need resumable sleep/futex scheduling beyond the current blocked-deadline model")

	m := newJea9LinuxGoMachine(t, jea9LinuxGoRunConfig{
		Name: "timerselect",
		Options: Jea9LinuxOptions{
			ClockMode:        Jea9ClockManual,
			MonotonicStartNS: 1_000_000,
		},
	})
	defer m.mem.Free()
	cleanup := InstallJea9Linux(m.cpu, m.os)
	defer cleanup()

	blockedCount := 0
	for steps := 0; steps < 200; steps++ {
		err := m.os.Run(m.cpu)
		switch {
		case errors.Is(err, ErrJea9LinuxBudget):
			continue
		case errors.Is(err, ErrJea9LinuxBlocked):
			blockedCount++
			if blockedCount > 50 {
				t.Fatalf("guest blocked too many times under manual clock; stdout=%q stderr=%q", m.stdout.String(), m.stderr.String())
			}
			if !m.os.blockedHasDeadline {
				t.Fatalf("guest blocked without a deadline under manual clock; stdout=%q stderr=%q", m.stdout.String(), m.stderr.String())
			}
			m.os.SetMonotonicNS(m.os.blockedUntil + int64(10*time.Millisecond))
			continue
		case err != nil:
			t.Fatalf("manual clock run: %v; pc=0x%x insn=%s stdout=%q stderr=%q", err, m.cpu.PC(), disasmGuestInsn(t, &m.cpu.mem, m.cpu.PC()), m.stdout.String(), m.stderr.String())
		default:
			if blockedCount == 0 {
				t.Fatal("Go timer exited without blocking under manual clock")
			}
			result := jea9LinuxGoRunResult{code: m.cpu.ExitCode, stdout: m.stdout.String(), stderr: m.stderr.String()}
			requireJea9LinuxGoExit(t, result, 0)
			if !strings.HasPrefix(result.stdout, "elapsed_ms=") {
				t.Fatalf("stdout = %q, want elapsed_ms prefix", result.stdout)
			}
			return
		}
	}
	t.Fatalf("manual clock timer did not exit; blockedCount=%d stdout=%q stderr=%q", blockedCount, m.stdout.String(), m.stderr.String())
}

func TestJea9Linux_GoReplayIdentical(t *testing.T) {
	cfg := jea9LinuxGoRunConfig{
		Name: "goroutine",
		Options: Jea9LinuxOptions{
			EntropySeed:      []byte("go replay seed"),
			MonotonicStartNS: 123,
		},
	}
	first := runJea9LinuxGoFixture(t, cfg)
	second := runJea9LinuxGoFixture(t, cfg)
	if first != second {
		t.Fatalf("replay mismatch: first=%+v second=%+v", first, second)
	}
}

func runJea9LinuxGoFixture(t *testing.T, cfg jea9LinuxGoRunConfig) jea9LinuxGoRunResult {
	t.Helper()
	m := newJea9LinuxGoMachine(t, cfg)
	defer m.mem.Free()
	code, err := RunWithJea9Linux(m.cpu, m.os)
	if err != nil {
		t.Fatalf("RunWithJea9Linux: %v; pc=0x%x insn=%s stdout=%q stderr=%q", err, m.cpu.PC(), disasmGuestInsn(t, &m.cpu.mem, m.cpu.PC()), m.stdout.String(), m.stderr.String())
	}
	return jea9LinuxGoRunResult{
		code:   code,
		stdout: m.stdout.String(),
		stderr: m.stderr.String(),
	}
}

func newJea9LinuxGoMachine(t *testing.T, cfg jea9LinuxGoRunConfig) *jea9LinuxGoMachine {
	t.Helper()
	path := "testvectors/jea9linux/go/elf/" + cfg.Name + ".elf"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Go fixture: %v", err)
	}
	mem, err := NewGuestMemory(jea9LinuxGoMemorySize)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, data)
	if err != nil {
		mem.Free()
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	const stackTop = jea9LinuxGoMemorySize - Size1MB
	cpu.SetReg(2, stackTop)

	var stdout, stderr bytes.Buffer
	opts := cfg.Options
	if opts.MonotonicStartNS == 0 {
		opts.MonotonicStartNS = 1
	}
	if opts.InstructionBudget == 0 {
		opts.InstructionBudget = 1 << 20
	}
	opts.Stdout = &stdout
	opts.Stderr = &stderr
	j := NewJea9Linux(opts)
	args := append([]string(nil), cfg.Args...)
	if len(args) == 0 {
		args = []string{"/" + cfg.Name}
	}
	if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     args,
		Env:      append([]string(nil), cfg.Env...),
		ExecPath: args[0],
		StackTop: stackTop,
	}); err != nil {
		mem.Free()
		t.Fatalf("InitELFStack: %v", err)
	}
	return &jea9LinuxGoMachine{
		cpu:    cpu,
		mem:    mem,
		os:     j,
		stdout: &stdout,
		stderr: &stderr,
	}
}

func requireJea9LinuxGoExit(t *testing.T, result jea9LinuxGoRunResult, want int) {
	t.Helper()
	if result.code != want {
		t.Fatalf("exit code = %d, want %d; stdout=%q stderr=%q", result.code, want, result.stdout, result.stderr)
	}
}

func disasmGuestInsn(t *testing.T, mem *GuestMemory, pc uint64) string {
	t.Helper()
	half, fault := mem.Fetch16(pc)
	if fault != nil {
		return fault.Error()
	}
	if half&0x3 != 0x3 {
		return DisasmRVC(half)
	}
	word, fault := mem.Fetch32(pc)
	if fault != nil {
		return fault.Error()
	}
	return DisasmRV32(pc, word)
}
