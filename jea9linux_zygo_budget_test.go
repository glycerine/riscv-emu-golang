package riscv

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const zygoBudgetFib10Program = "(defn fib [x] (cond (== x 0) 0 (== x 1) 1 (+ (fib (- x 1)) (fib (- x 2))))) (println (fib 10))"

func TestJea9Linux_ZygoFib10_InterpreterBudget1000(t *testing.T) {
	elfData, err := os.ReadFile("bench/zygo.elf")
	if err != nil {
		t.Skipf("bench/zygo.elf not found: %v", err)
	}

	var stdout, stderr bytes.Buffer
	mem, err := NewGuestMemory(Size16GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	elf, err := LoadELFBytes(mem, elfData)
	if err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	jos := NewJea9Linux(Jea9LinuxOptions{
		ClockMode:         Jea9ClockIdleJump,
		MonotonicStartNS:  1,
		NSPerInstruction:  1,
		InstructionBudget: 1000,
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	args := []string{"bench/zygo.elf", "-c", zygoBudgetFib10Program}
	if err := jos.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     args,
		ExecPath: args[0],
	}); err != nil {
		t.Fatal(err)
	}

	code, err := RunWithJea9Linux(cpu, jos)
	if err != nil {
		t.Fatalf("RunWithJea9Linux: %v\nstderr:\n%s", err, limitZygoBudgetString(stderr.String(), 2048))
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, limitZygoBudgetString(stderr.String(), 2048))
	}
	if got := stdout.String(); got != "55\n" {
		t.Fatalf("stdout = %q, want %q\nstderr:\n%s", got, "55\n", limitZygoBudgetString(stderr.String(), 2048))
	}
}

func limitZygoBudgetString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimRight(s[:max], "\n") + "\n..."
}
