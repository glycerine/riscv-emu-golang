package bench

import (
	"testing"

	"riscv"
)

func TestJIT_BenchGuest_Smoke(t *testing.T) {
	elfData := loadCPUELF(t)
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	jit := riscv.NewJIT()
	// jit.InterpOnly = true // DEBUG: test interpreter-only mode

	exitCode := -1
	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				if ex, ok := r.(*riscv.ExitError); ok {
					exitCode = ex.Code
					return
				}
				panic(r)
			}
		}()
		runErr = jit.RunJIT(cpu)
	}()

	t.Logf("JIT smoke: retired %d instructions, exit code %d, runErr=%v, PC=0x%x",
		cpu.Cycle(), exitCode, runErr, cpu.PC())
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d (runErr=%v)", exitCode, runErr)
	}
}
