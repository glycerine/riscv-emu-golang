package riscv

import "testing"

type testMachineTimer struct {
	pending bool
	ticks   uint64
}

func (t *testMachineTimer) Load(addr, width uint64) (uint64, bool, *MemFault) {
	return 0, false, nil
}

func (t *testMachineTimer) Store(addr, width, value uint64) (bool, *MemFault) {
	return false, nil
}

func (t *testMachineTimer) AdvanceMachineTimer(delta uint64) {
	t.ticks += delta
	t.pending = true
}

func (t *testMachineTimer) MachineTimerValue() uint64 {
	return t.ticks
}

func (t *testMachineTimer) MachineTimerPending() bool {
	return t.pending
}

func TestRunMachineBudget_DoesNotAdvanceBiosMachineTimer(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const (
		pc      = uint64(0x1000)
		handler = uint64(0x2000)
	)
	if fault := mem.Store32(pc, 0x00000013); fault != nil { // addi x0,x0,0
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.mtvec = handler
	cpu.mie = mipMTIP

	res, err := RunMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if timer.ticks != 0 {
		t.Fatalf("generic RunMachineBudget advanced timer by %d", timer.ticks)
	}
	if cpu.PC() != pc+4 {
		t.Fatalf("PC = 0x%x, want executed instruction at 0x%x", cpu.PC(), pc+4)
	}
}

func TestRunBiosMachineBudget_DeliversMachineTimerInterrupt(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const (
		pc      = uint64(0x1000)
		handler = uint64(0x2000)
	)
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.mtvec = handler
	cpu.mie = mipMTIP

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if cpu.PC() != handler {
		t.Fatalf("PC = 0x%x, want interrupt handler 0x%x", cpu.PC(), handler)
	}
	if cpu.PrivilegeMode() != PrivMachine {
		t.Fatalf("privilege = %v, want machine", cpu.PrivilegeMode())
	}
	if cpu.mcause != InterruptCauseFlag|InterruptMTIP || cpu.mepc != pc {
		t.Fatalf("machine trap mcause=0x%x mepc=0x%x", cpu.mcause, cpu.mepc)
	}
	if cpu.RiscvInstrBegun() != 0 {
		t.Fatalf("interrupt delivery began %d instructions, want 0", cpu.RiscvInstrBegun())
	}
}

func TestRunBiosMachineBudget_MachineTimerInterruptUsesVectoredMtvec(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const (
		pc   = uint64(0x1000)
		base = uint64(0x2000)
	)
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.mtvec = base | 1
	cpu.mie = mipMTIP

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunBiosMachineBudget result = %v, want expired", res)
	}
	if want := base + 4*InterruptMTIP; cpu.PC() != want {
		t.Fatalf("PC = 0x%x, want vectored mtvec target 0x%x", cpu.PC(), want)
	}
}

func TestRunBiosMachineBudget_DeliversDelegatedSupervisorTimerInterrupt(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const (
		pc      = uint64(0x1000)
		handler = uint64(0x3000)
	)
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.stvec = handler
	cpu.mip = mipSTIP
	cpu.mideleg = mipSTIP
	cpu.sie = mipSTIP
	cpu.mstatus = statusSIE

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if cpu.PC() != handler {
		t.Fatalf("PC = 0x%x, want supervisor handler 0x%x", cpu.PC(), handler)
	}
	if cpu.PrivilegeMode() != PrivSupervisor {
		t.Fatalf("privilege = %v, want supervisor", cpu.PrivilegeMode())
	}
	if cpu.scause != InterruptCauseFlag|InterruptSTIP || cpu.sepc != pc {
		t.Fatalf("supervisor trap scause=0x%x sepc=0x%x", cpu.scause, cpu.sepc)
	}
}

func TestRunBiosMachineBudget_SupervisorTimerInterruptUsesVectoredStvec(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const (
		pc   = uint64(0x1000)
		base = uint64(0x3000)
	)
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.stvec = base | 1
	cpu.mip = mipSTIP
	cpu.mideleg = mipSTIP
	cpu.sie = mipSTIP
	cpu.mstatus = statusSIE

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunBiosMachineBudget result = %v, want expired", res)
	}
	if want := base + 4*InterruptSTIP; cpu.PC() != want {
		t.Fatalf("PC = 0x%x, want vectored stvec target 0x%x", cpu.PC(), want)
	}
}

func TestRunBiosMachineBudget_DeliversSupervisorTimerCompareInterrupt(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const (
		pc      = uint64(0x1000)
		handler = uint64(0x3000)
	)
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.stvec = handler
	cpu.stimecmp = 500
	cpu.mideleg = mipSTIP
	cpu.sie = mipSTIP
	cpu.mstatus = statusSIE

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if cpu.PC() != handler {
		t.Fatalf("PC = 0x%x, want supervisor timer handler 0x%x", cpu.PC(), handler)
	}
	if cpu.scause != InterruptCauseFlag|InterruptSTIP || cpu.sepc != pc {
		t.Fatalf("supervisor timer trap scause=0x%x sepc=0x%x", cpu.scause, cpu.sepc)
	}
}

func TestRunMachineBudget_DoesNotAdvanceSupervisorTimerCompare(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const pc = uint64(0x1000)
	if fault := mem.Store32(pc, 0x00000013); fault != nil { // addi x0,x0,0
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.stimecmp = 500
	cpu.mideleg = mipSTIP
	cpu.sie = mipSTIP
	cpu.mstatus = statusSIE

	res, err := RunMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if timer.ticks != 0 {
		t.Fatalf("generic RunMachineBudget advanced timer by %d", timer.ticks)
	}
	if cpu.PC() != pc+4 {
		t.Fatalf("PC = 0x%x, want executed instruction at 0x%x", cpu.PC(), pc+4)
	}
	if cpu.mip&mipSTIP != 0 {
		t.Fatalf("generic RunMachineBudget set STIP: mip=0x%x", cpu.mip)
	}
}

func TestCPU_StrictUnknownCSRTraps(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const pc = uint64(0x1000)
	if fault := mem.Store32(pc, 0x7c002073); fault != nil { // csrr x0, 0x7c0
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.EnableStrictCSR()
	cpu.SetPC(pc)

	if err := cpu.Step(); err != ErrIllegalInstruction {
		t.Fatalf("strict unknown CSR step err = %v, want ErrIllegalInstruction", err)
	}
}

func TestCPU_STimecmpCSR(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const pc = uint64(0x1000)
	// csrrw x0, stimecmp, x1
	if fault := mem.Store32(pc, 0x14d09073); fault != nil {
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.EnableStrictCSR()
	cpu.SetPC(pc)
	cpu.SetReg(1, 500)

	if err := cpu.Step(); err != nil {
		t.Fatalf("stimecmp write step: %v", err)
	}
	if cpu.stimecmp != 500 {
		t.Fatalf("stimecmp = %d, want 500", cpu.stimecmp)
	}
}
