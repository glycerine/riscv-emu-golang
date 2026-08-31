package riscv

import "testing"

func newTrapAccountingCPU(t *testing.T, codeAddr uint64, insns []uint32) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	for i, insn := range insns {
		if f := mem.Store32(codeAddr+uint64(i)*4, insn); f != nil {
			mem.Free()
			t.Fatalf("Store32 insn %d: %v", i, f)
		}
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(codeAddr)
	return cpu, mem
}

func TestCPU_ECALLTrapPCAndRetired(t *testing.T) {
	const codeAddr = uint64(0x2000)
	cpu, mem := newTrapAccountingCPU(t, codeAddr, []uint32{
		0x00100293, // addi x5, x0, 1
		0x00000073, // ecall
	})
	defer mem.Free()

	var got Note
	var seen bool
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		got = n
		seen = true
		return NoteFatal
	})

	if err := RunWithChain(cpu, &cpu.Notes); err != ErrEcall {
		t.Fatalf("RunWithChain err = %v, want ErrEcall", err)
	}
	if !seen {
		t.Fatal("ECALL note was not delivered")
	}
	if got.Cause != CauseEcallU || got.PC != codeAddr+4 || got.InsnLen != 4 {
		t.Fatalf("note = {cause:%d pc:0x%x len:%d}, want ECALL pc=0x%x len=4", got.Cause, got.PC, got.InsnLen, codeAddr+4)
	}
	if cpu.PC() != codeAddr+4 {
		t.Fatalf("cpu.PC() = 0x%x, want trap PC 0x%x", cpu.PC(), codeAddr+4)
	}
	if got := cpu.RiscvInstrBegun(); got != 2 {
		t.Fatalf("RiscvInstrBegun = %d, want 2", got)
	}
	if got := cpu.RiscvInstrRetired(); got != 1 {
		t.Fatalf("RiscvInstrRetired = %d, want 1", got)
	}
}

func TestCPU_EBREAKTrapPCAndRetired(t *testing.T) {
	const codeAddr = uint64(0x3000)
	cpu, mem := newTrapAccountingCPU(t, codeAddr, []uint32{
		0x00100073, // ebreak
	})
	defer mem.Free()

	var got Note
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		got = n
		return NoteFatal
	})

	if err := RunWithChain(cpu, &cpu.Notes); err != ErrEbreak {
		t.Fatalf("RunWithChain err = %v, want ErrEbreak", err)
	}
	if got.Cause != CauseBreakpoint || got.PC != codeAddr || got.InsnLen != 4 {
		t.Fatalf("note = {cause:%d pc:0x%x len:%d}, want EBREAK pc=0x%x len=4", got.Cause, got.PC, got.InsnLen, codeAddr)
	}
	if cpu.PC() != codeAddr {
		t.Fatalf("cpu.PC() = 0x%x, want trap PC 0x%x", cpu.PC(), codeAddr)
	}
	if got := cpu.RiscvInstrBegun(); got != 1 {
		t.Fatalf("RiscvInstrBegun = %d, want 1", got)
	}
	if got := cpu.RiscvInstrRetired(); got != 0 {
		t.Fatalf("RiscvInstrRetired = %d, want 0", got)
	}
}

func TestCPU_CEBREAKTrapPCAndRetired(t *testing.T) {
	const codeAddr = uint64(0x4000)
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if f := mem.Store16(codeAddr, 0x9002); f != nil {
		t.Fatal(f)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(codeAddr)

	var got Note
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		got = n
		return NoteFatal
	})

	if err := cpu.Run(); err != ErrEbreak {
		t.Fatalf("cpu.Run err = %v, want ErrEbreak", err)
	}
	if got.Cause != CauseBreakpoint || got.PC != codeAddr || got.InsnLen != 2 {
		t.Fatalf("note = {cause:%d pc:0x%x len:%d}, want C.EBREAK pc=0x%x len=2", got.Cause, got.PC, got.InsnLen, codeAddr)
	}
	if cpu.PC() != codeAddr {
		t.Fatalf("cpu.PC() = 0x%x, want trap PC 0x%x", cpu.PC(), codeAddr)
	}
	if got := cpu.RiscvInstrBegun(); got != 1 {
		t.Fatalf("RiscvInstrBegun = %d, want 1", got)
	}
	if got := cpu.RiscvInstrRetired(); got != 0 {
		t.Fatalf("RiscvInstrRetired = %d, want 0", got)
	}
}

func TestCPU_UserEBREAKDelegatesToSupervisor(t *testing.T) {
	const (
		codeAddr = uint64(0x5000)
		handler  = uint64(0x6000)
	)
	cpu, mem := newTrapAccountingCPU(t, codeAddr, []uint32{
		0x00100073, // ebreak
	})
	defer mem.Free()
	cpu.SetPrivilegeMode(PrivUser)
	cpu.medeleg = uint64(1) << CauseBreakpoint
	cpu.stvec = handler

	if err := cpu.Step(); err != nil {
		t.Fatalf("Step err = %v, want guest breakpoint trap", err)
	}
	if cpu.PC() != handler || cpu.PrivilegeMode() != PrivSupervisor {
		t.Fatalf("after EBREAK pc=0x%x priv=%v, want pc=0x%x priv=%v", cpu.PC(), cpu.PrivilegeMode(), handler, PrivSupervisor)
	}
	if cpu.sepc != codeAddr || cpu.scause != CauseBreakpoint || cpu.stval != 0 {
		t.Fatalf("supervisor trap sepc=0x%x scause=%d stval=0x%x, want sepc=0x%x scause=%d stval=0", cpu.sepc, cpu.scause, cpu.stval, codeAddr, CauseBreakpoint)
	}
	if cpu.lastTrapInsnLen != 4 {
		t.Fatalf("lastTrapInsnLen = %d, want 4", cpu.lastTrapInsnLen)
	}
}

func TestRunCached_UserCEBREAKDelegatesToSupervisor(t *testing.T) {
	const (
		codeAddr = uint64(0x7000)
		handler  = uint64(0x8000)
	)
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if f := mem.Store16(codeAddr, 0x9002); f != nil {
		t.Fatal(f)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(codeAddr)
	cpu.SetPrivilegeMode(PrivUser)
	cpu.medeleg = uint64(1) << CauseBreakpoint
	cpu.stvec = handler

	res, limit, err := RunDefaultDualBudget(cpu, &cpu.Notes, 1, ^uint64(0))
	if err != nil {
		t.Fatalf("RunDefaultDualBudget err = %v, want guest breakpoint trap", err)
	}
	if res != RunBudgetExpired || limit != RunBudgetLimitAttempt {
		t.Fatalf("RunDefaultDualBudget result = (%v, %v), want (%v, %v)", res, limit, RunBudgetExpired, RunBudgetLimitAttempt)
	}
	if cpu.PC() != handler || cpu.PrivilegeMode() != PrivSupervisor {
		t.Fatalf("after C.EBREAK pc=0x%x priv=%v, want pc=0x%x priv=%v", cpu.PC(), cpu.PrivilegeMode(), handler, PrivSupervisor)
	}
	if cpu.sepc != codeAddr || cpu.scause != CauseBreakpoint || cpu.stval != 0 {
		t.Fatalf("supervisor trap sepc=0x%x scause=%d stval=0x%x, want sepc=0x%x scause=%d stval=0", cpu.sepc, cpu.scause, cpu.stval, codeAddr, CauseBreakpoint)
	}
	if cpu.lastTrapInsnLen != 2 {
		t.Fatalf("lastTrapInsnLen = %d, want 2", cpu.lastTrapInsnLen)
	}
}
