package riscv

import (
	"errors"
	"reflect"
	"testing"
)

func TestJea9Linux_ScheduleTraceIndependentOfHostBudget_Interpreter(t *testing.T) {
	want := runJea9LinuxSchedulerTraceLoop(t, false, 1000, 4)
	for _, budget := range []uint64{1, 3, 17} {
		got := runJea9LinuxSchedulerTraceLoop(t, false, budget, 4)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("interpreter schedule trace differs for host budget %d:\ngot  %+v\nwant %+v", budget, got, want)
		}
	}
}

func TestJea9Linux_ScheduleTraceIndependentOfHostBudget_LazyJIT(t *testing.T) {
	want := runJea9LinuxSchedulerTraceLoop(t, true, 1000, 4)
	for _, budget := range []uint64{1, 3, 17} {
		got := runJea9LinuxSchedulerTraceLoop(t, true, budget, 4)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("lazy JIT schedule trace differs for host budget %d:\ngot  %+v\nwant %+v", budget, got, want)
		}
	}
}

func TestJea9Linux_ScheduleTraceInterpreterMatchesLazyJIT(t *testing.T) {
	interp := runJea9LinuxSchedulerTraceLoop(t, false, 3, 4)
	jit := runJea9LinuxSchedulerTraceLoop(t, true, 3, 4)
	if !reflect.DeepEqual(jit, interp) {
		t.Fatalf("lazy JIT schedule trace differs from interpreter:\njit    %+v\ninterp %+v", jit, interp)
	}
}

func TestJea9Linux_SchedulerQuantumUsesRetiredNotBegun_Interpreter(t *testing.T) {
	runJea9LinuxEcallRetiredQuantum(t, false)
}

func TestJea9Linux_SchedulerQuantumUsesRetiredNotBegun_LazyJIT(t *testing.T) {
	runJea9LinuxEcallRetiredQuantum(t, true)
}

func runJea9LinuxSchedulerTraceLoop(t *testing.T, useJIT bool, hostBudget uint64, events int) []Jea9LinuxScheduleTraceEntry {
	t.Helper()
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1
		jenc(0, -4),               // JAL x0, 0x1000
	})
	defer mem.Free()
	j := NewJea9Linux(Jea9LinuxOptions{
		InstructionBudget: hostBudget,
		Trace:             true,
		Scheduler: Jea9LinuxSchedulerConfig{
			MinQuantumRetired: 2,
			MaxQuantumRetired: 2,
		},
	})
	parent := j.ensureScheduler(cpu)
	childTID := parent.tid + 1
	j.contexts[childTID] = &jea9LinuxContext{
		tid:   childTID,
		state: jea9LinuxContextRunnable,
		snapshot: jea9LinuxCPUSnapshot{
			pc: 0x1000,
		},
	}
	j.contextOrder = append(j.contextOrder, childTID)

	var jit *JIT
	if useJIT {
		jit = NewSandboxJIT()
		defer jit.Close()
	}
	for len(j.TraceSnapshot().Schedule) < events {
		var err error
		if useJIT {
			err = j.RunJIT(cpu, jit)
		} else {
			err = j.Run(cpu)
		}
		if !errors.Is(err, ErrJea9LinuxBudget) {
			t.Fatalf("run useJIT=%v hostBudget=%d err=%v, want ErrJea9LinuxBudget before %d schedule events", useJIT, hostBudget, err, events)
		}
	}
	trace := j.TraceSnapshot().Schedule
	return append([]Jea9LinuxScheduleTraceEntry(nil), trace[:events]...)
}

func runJea9LinuxEcallRetiredQuantum(t *testing.T, useJIT bool) {
	t.Helper()
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1; retired
		instrECALL,                // getpid; synchronous exception, not retired
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1; retired
		jenc(0, -4),               // loop on second ADDI
	})
	defer mem.Free()
	j := NewJea9Linux(Jea9LinuxOptions{
		InstructionBudget: 100,
		Trace:             true,
		Scheduler: Jea9LinuxSchedulerConfig{
			MinQuantumRetired: 2,
			MaxQuantumRetired: 2,
		},
	})
	cpu.SetReg(17, jea9TestSysGetpid)

	var err error
	if useJIT {
		jit := NewSandboxJIT()
		defer jit.Close()
		cleanup := InstallJea9LinuxJIT(cpu, jit, j)
		defer cleanup()
		err = j.RunJIT(cpu, jit)
	} else {
		cleanup := InstallJea9Linux(cpu, j)
		defer cleanup()
		err = j.Run(cpu)
	}
	if !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("run useJIT=%v err=%v, want ErrJea9LinuxBudget at scheduler quantum", useJIT, err)
	}
	if got := j.SchedulerEventID(); got != 1 {
		t.Fatalf("SchedulerEventID = %d, want 1; scheduler must fire on retired deadline, not host poll", got)
	}
	if got := cpu.RiscvInstrRetired(); got != 2 {
		t.Fatalf("RiscvInstrRetired = %d, want 2", got)
	}
	if got := cpu.RiscvInstrBegun(); got != 3 {
		t.Fatalf("RiscvInstrBegun = %d, want 3 including non-retired ECALL", got)
	}
	if got := cpu.Reg(1); got != 2 {
		t.Fatalf("x1 = %d, want both retired ADDIs to run before scheduler quantum", got)
	}
	trace := j.TraceSnapshot()
	if got := len(trace.Schedule); got != 1 {
		t.Fatalf("schedule trace entries = %d, want 1", got)
	}
	if got := trace.Schedule[0].RiscvInstrRetired; got != 2 {
		t.Fatalf("trace retired count = %d, want 2", got)
	}
}
