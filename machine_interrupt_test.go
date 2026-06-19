package riscv

import (
	"testing"
	"time"
)

type testMachineTimer struct {
	ticks uint64
}

func (t *testMachineTimer) Load(addr, width uint64) (uint64, bool, *MemFault) {
	return 0, false, nil
}

func (t *testMachineTimer) Store(addr, width, value uint64) (bool, *MemFault) {
	return false, nil
}

func (t *testMachineTimer) AdvanceMachineTimer(delta uint64) {
	t.ticks += delta
}

func (t *testMachineTimer) MachineTimerValue() uint64 {
	return t.ticks
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

func TestRunBiosMachineBudget_DoesNotDeliverCLINTMachineTimerInterrupt(t *testing.T) {
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

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if cpu.PC() != pc+4 {
		t.Fatalf("PC = 0x%x, want executed instruction at 0x%x", cpu.PC(), pc+4)
	}
	if cpu.PrivilegeMode() != PrivSupervisor {
		t.Fatalf("privilege = %v, want supervisor", cpu.PrivilegeMode())
	}
	if timer.ticks != biosTimerTicksPerInstruction {
		t.Fatalf("BIOS timer ticks = %d, want %d", timer.ticks, biosTimerTicksPerInstruction)
	}
}

func TestRunBiosMachineBudget_WFISleepsUntilSupervisorTimerCompare(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const pc = uint64(0x1000)
	if fault := mem.Store32(pc, 0x10500073); fault != nil { // wfi
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.stimecmp = 6
	cpu.mideleg = mipSTIP
	cpu.sie = mipSTIP
	cpu.mstatus = statusSIE

	var sleeps []time.Duration
	withFakeBiosWFISleep(t, time.Millisecond, func(d time.Duration) {
		sleeps = append(sleeps, d)
	})

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunBiosMachineBudget result = %v, want expired", res)
	}
	if len(sleeps) != 1 {
		t.Fatalf("WFI sleeps = %d, want 1", len(sleeps))
	}
	if sleeps[0] != 500*time.Nanosecond {
		t.Fatalf("WFI sleep = %s, want 500ns", sleeps[0])
	}
	if timer.ticks != 6 {
		t.Fatalf("BIOS timer ticks = %d, want stimecmp 6", timer.ticks)
	}
	if cpu.mipValue()&mipSTIP == 0 {
		t.Fatalf("WFI did not assert STIP at stimecmp: mip=0x%x", cpu.mipValue())
	}
	if cpu.PC() != pc+4 {
		t.Fatalf("PC = 0x%x, want WFI retired to 0x%x", cpu.PC(), pc+4)
	}
}

func TestRunBiosMachineBudget_WFISleepIsCappedWithoutTimerDeadline(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const pc = uint64(0x1000)
	if fault := mem.Store32(pc, 0x10500073); fault != nil { // wfi
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)

	var sleeps []time.Duration
	withFakeBiosWFISleep(t, 2*time.Millisecond, func(d time.Duration) {
		sleeps = append(sleeps, d)
	})

	res, err := RunBiosMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunBiosMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunBiosMachineBudget result = %v, want expired", res)
	}
	if len(sleeps) != 1 || sleeps[0] != 2*time.Millisecond {
		t.Fatalf("WFI sleeps = %v, want [2ms]", sleeps)
	}
	const wantTicks = biosTimerTicksPerInstruction + 20000
	if timer.ticks != wantTicks {
		t.Fatalf("BIOS timer ticks = %d, want %d", timer.ticks, wantTicks)
	}
}

func TestRunBiosMachineBudget_WFIDoesNotSleepWithPendingInterrupt(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.wfi = true
	cpu.mip = mipSSIP
	cpu.mideleg = mipSSIP
	cpu.sie = mipSSIP
	cpu.mstatus = statusSIE

	withFakeBiosWFISleep(t, time.Millisecond, func(time.Duration) {
		t.Fatal("WFI slept despite a pending supervisor interrupt")
	})
	cpu.serviceBiosWFI()
	if cpu.wfi {
		t.Fatal("WFI flag was not consumed")
	}
	if timer.ticks != 0 {
		t.Fatalf("BIOS timer ticks = %d, want no WFI advance", timer.ticks)
	}
}

func TestSetBiosIdleSleepCapRestoresPreviousValue(t *testing.T) {
	old := biosWFIHostSleepCap
	restore := SetBiosIdleSleepCap(17 * time.Millisecond)
	if biosWFIHostSleepCap != 17*time.Millisecond {
		t.Fatalf("biosWFIHostSleepCap = %s, want 17ms", biosWFIHostSleepCap)
	}
	restore()
	if biosWFIHostSleepCap != old {
		t.Fatalf("biosWFIHostSleepCap = %s, want restored %s", biosWFIHostSleepCap, old)
	}
}

func TestRunMachineBudget_WFIDoesNotSleepOrAdvanceBiosTimer(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const pc = uint64(0x1000)
	if fault := mem.Store32(pc, 0x10500073); fault != nil { // wfi
		t.Fatal(fault)
	}
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)

	withFakeBiosWFISleep(t, time.Millisecond, func(time.Duration) {
		t.Fatal("generic RunMachineBudget used BIOS WFI sleep")
	})
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
}

func TestRunBiosMachineBudget_SupervisorTimerCompareUsesVectoredStvec(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	timer := &testMachineTimer{}
	mem.SetMMIO(timer)

	const (
		pc   = uint64(0x1000)
		base = uint64(0x3000)
	)
	cpu := NewCPU(*mem)
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.stvec = base | 1
	cpu.stimecmp = 1
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

func withFakeBiosWFISleep(t *testing.T, cap time.Duration, sleep func(time.Duration)) {
	t.Helper()
	oldSleep := biosWFIHostSleep
	oldCap := biosWFIHostSleepCap
	biosWFIHostSleep = sleep
	biosWFIHostSleepCap = cap
	t.Cleanup(func() {
		biosWFIHostSleep = oldSleep
		biosWFIHostSleepCap = oldCap
	})
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
	cpu.stimecmp = 1
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
	cpu.stimecmp = 1
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
	if cpu.mipValue()&mipSTIP != 0 {
		t.Fatalf("generic RunMachineBudget set STIP: mip=0x%x", cpu.mipValue())
	}
}

func TestCPU_STimecmpOwnsSTIP(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)

	cpu.writeCSR(0x344, mipSTIP)
	if got := cpu.mipValue(); got&mipSTIP != 0 {
		t.Fatalf("mip.STIP write asserted STIP outside stimecmp: mip=0x%x", got)
	}

	cpu.stimecmp = 2000
	cpu.refreshSupervisorTimerPendingAt(1000)
	if got := cpu.mipValue(); got&mipSTIP != 0 {
		t.Fatalf("inactive stimecmp asserted STIP: mip=0x%x", got)
	}

	cpu.refreshSupervisorTimerPendingAt(2000)
	if got := cpu.mipValue(); got&mipSTIP == 0 {
		t.Fatalf("ready stimecmp did not assert STIP: mip=0x%x", got)
	}

	cpu.writeCSR(0x344, 0)
	if got := cpu.mipValue(); got&mipSTIP == 0 {
		t.Fatalf("mip write cleared comparator-owned STIP: mip=0x%x", got)
	}

	cpu.writeCSR(0x14d, ^uint64(0))
	if got := cpu.mipValue(); got&mipSTIP != 0 {
		t.Fatalf("disabled stimecmp still asserted STIP: mip=0x%x", got)
	}
}

func TestCPU_StrictFirmwareCSRsForSstcProbe(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	cpu.EnableStrictCSR()

	for _, csr := range []uint32{0x30a, 0x320, 0x14d} {
		if _, ok := cpu.readCSR(csr); !ok {
			t.Fatalf("strict CSR read %#x failed", csr)
		}
	}
	if !cpu.writeCSR(0x30a, uint64(1)<<63) {
		t.Fatal("strict menvcfg write failed")
	}
	if !cpu.writeCSR(0x320, 0xff) {
		t.Fatal("strict mcountinhibit write failed")
	}
	if cpu.menvcfg != uint64(1)<<63 || cpu.mcountinh != 0xff {
		t.Fatalf("firmware CSR values menvcfg=0x%x mcountinhibit=0x%x", cpu.menvcfg, cpu.mcountinh)
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
	cpu.SetPrivilegeMode(PrivSupervisor)
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
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.SetPC(pc)
	cpu.SetReg(1, 500)

	if err := cpu.Step(); err != nil {
		t.Fatalf("stimecmp write step: %v", err)
	}
	if cpu.stimecmp != 500 {
		t.Fatalf("stimecmp = %d, want 500", cpu.stimecmp)
	}
}
