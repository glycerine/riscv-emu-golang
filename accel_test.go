package riscv

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

var _ Accelerator = (*noopAccelerator)(nil)
var _ DualBudgetAccelerator = (*dualNoopAccelerator)(nil)

type noopAccelerator struct {
	closed     bool
	runCalls   int
	lastCPU    *CPU
	lastNotes  *NoteChain
	lastBudget uint64
	lastMode   AccelRunMode
}

func (n *noopAccelerator) CompileMachine(context.Context, *Machine, AccelOptions) error {
	return nil
}

func (n *noopAccelerator) RunMachineBudget(cpu *CPU, nc *NoteChain, budget uint64, mode AccelRunMode) (RunBudgetResult, error) {
	n.runCalls++
	n.lastCPU = cpu
	n.lastNotes = nc
	n.lastBudget = budget
	n.lastMode = mode
	return RunBudgetExpired, nil
}

type dualNoopAccelerator struct {
	noopAccelerator
	dualCalls       int
	lastAttempt     uint64
	lastRetired     uint64
	lastDualMode    AccelRunMode
	lastDualCPU     *CPU
	lastDualNotePtr *NoteChain
}

func (d *dualNoopAccelerator) RunMachineDualBudget(cpu *CPU, nc *NoteChain, attemptBudget, retiredBudget uint64, mode AccelRunMode) (RunBudgetResult, RunBudgetLimit, error) {
	d.dualCalls++
	d.lastAttempt = attemptBudget
	d.lastRetired = retiredBudget
	d.lastDualMode = mode
	d.lastDualCPU = cpu
	d.lastDualNotePtr = nc
	return RunBudgetExpired, RunBudgetLimitRetired, nil
}

type exitAccelerator struct {
	code         int
	compileCalls int
	runCalls     int
	closed       bool
}

func (e *exitAccelerator) CompileMachine(_ context.Context, m *Machine, _ AccelOptions) error {
	e.compileCalls++
	m.Accel = e
	return nil
}

func (e *exitAccelerator) RunMachineBudget(cpu *CPU, _ *NoteChain, _ uint64, _ AccelRunMode) (RunBudgetResult, error) {
	e.runCalls++
	cpu.ExitCode = e.code
	return RunBudgetExit, nil
}

func (e *exitAccelerator) RunMachineDualBudget(cpu *CPU, _ *NoteChain, _, _ uint64, _ AccelRunMode) (RunBudgetResult, RunBudgetLimit, error) {
	e.runCalls++
	cpu.ExitCode = e.code
	return RunBudgetExit, RunBudgetLimitNone, nil
}

func (e *exitAccelerator) Invalidate(uint64, uint64, InvalidateReason) {}

func (e *exitAccelerator) Close() error {
	e.closed = true
	return nil
}

type recordingAccelerator struct {
	code           int
	compileCalls   int
	runCalls       int
	closed         bool
	compileMachine *Machine
	compilePC      uint64
	compileOptions AccelOptions
	lastMode       AccelRunMode
}

func (r *recordingAccelerator) CompileMachine(_ context.Context, m *Machine, opts AccelOptions) error {
	r.compileCalls++
	r.compileMachine = m
	r.compileOptions = opts
	if m != nil && m.CPU != nil {
		r.compilePC = m.CPU.PC()
		m.Accel = r
	}
	return nil
}

func (r *recordingAccelerator) RunMachineBudget(cpu *CPU, _ *NoteChain, _ uint64, mode AccelRunMode) (RunBudgetResult, error) {
	r.runCalls++
	r.lastMode = mode
	cpu.ExitCode = r.code
	return RunBudgetExit, nil
}

func (r *recordingAccelerator) Invalidate(uint64, uint64, InvalidateReason) {}

func (r *recordingAccelerator) Close() error {
	r.closed = true
	return nil
}

type recordedInvalidation struct {
	addr   uint64
	length uint64
	reason InvalidateReason
}

type invalidationRecorderAccelerator struct {
	noopAccelerator
	invalidations []recordedInvalidation
}

func (r *invalidationRecorderAccelerator) Invalidate(addr, length uint64, reason InvalidateReason) {
	r.invalidations = append(r.invalidations, recordedInvalidation{
		addr:   addr,
		length: length,
		reason: reason,
	})
}

func (n *noopAccelerator) Invalidate(uint64, uint64, InvalidateReason) {}

func (n *noopAccelerator) Close() error {
	n.closed = true
	return nil
}

func TestCurrentAccelABI_Offsets(t *testing.T) {
	var c CPU
	var m GuestMemory
	var mmu MMU
	var sh accelSliceHeader
	var er ExecRegion
	var te tlbEntry
	abi := CurrentAccelABI()

	if abi.Version != AccelABIVersion {
		t.Fatalf("Version = %d, want %d", abi.Version, AccelABIVersion)
	}
	if abi.CPUSize != unsafe.Sizeof(c) {
		t.Fatalf("CPUSize = %d, want %d", abi.CPUSize, unsafe.Sizeof(c))
	}
	if abi.MemSize != unsafe.Sizeof(m) {
		t.Fatalf("MemSize = %d, want %d", abi.MemSize, unsafe.Sizeof(m))
	}
	if abi.MMUSize != unsafe.Sizeof(mmu) {
		t.Fatalf("MMUSize = %d, want %d", abi.MMUSize, unsafe.Sizeof(mmu))
	}

	checks := map[string][2]uintptr{
		"OffMem":                    abiPair(abi.OffMem, unsafe.Offsetof(c.mem)),
		"OffPC":                     abiPair(abi.OffPC, unsafe.Offsetof(c.pc)),
		"OffX":                      abiPair(abi.OffX, unsafe.Offsetof(c.x)),
		"OffF":                      abiPair(abi.OffF, unsafe.Offsetof(c.f)),
		"OffFCSR":                   abiPair(abi.OffFCSR, unsafe.Offsetof(c.fcsr)),
		"OffRiscvInstrBegun":        abiPair(abi.OffRiscvInstrBegun, unsafe.Offsetof(c.riscvInstrBegun)),
		"OffRiscvInstrRetired":      abiPair(abi.OffRiscvInstrRetired, unsafe.Offsetof(c.riscvInstrRetired)),
		"OffWFI":                    abiPair(abi.OffWFI, unsafe.Offsetof(c.wfi)),
		"OffLastTrapCause":          abiPair(abi.OffLastTrapCause, unsafe.Offsetof(c.lastTrapCause)),
		"OffLastTrapInsnLen":        abiPair(abi.OffLastTrapInsnLen, unsafe.Offsetof(c.lastTrapInsnLen)),
		"OffPriv":                   abiPair(abi.OffPriv, unsafe.Offsetof(c.priv)),
		"OffMMU":                    abiPair(abi.OffMMU, unsafe.Offsetof(c.mmu)),
		"OffNotes":                  abiPair(abi.OffNotes, unsafe.Offsetof(c.Notes)),
		"OffResvAddr":               abiPair(abi.OffResvAddr, unsafe.Offsetof(c.resvAddr)),
		"OffResvValid":              abiPair(abi.OffResvValid, unsafe.Offsetof(c.resvValid)),
		"OffWatchAddr":              abiPair(abi.OffWatchAddr, unsafe.Offsetof(c.watchAddr)),
		"OffExitCode":               abiPair(abi.OffExitCode, unsafe.Offsetof(c.ExitCode)),
		"OffMTVEC":                  abiPair(abi.OffMTVEC, unsafe.Offsetof(c.mtvec)),
		"OffMScratch":               abiPair(abi.OffMScratch, unsafe.Offsetof(c.mscratch)),
		"OffMEPC":                   abiPair(abi.OffMEPC, unsafe.Offsetof(c.mepc)),
		"OffMCause":                 abiPair(abi.OffMCause, unsafe.Offsetof(c.mcause)),
		"OffMStatus":                abiPair(abi.OffMStatus, unsafe.Offsetof(c.mstatus)),
		"OffMTVal":                  abiPair(abi.OffMTVal, unsafe.Offsetof(c.mtval)),
		"OffSATP":                   abiPair(abi.OffSATP, unsafe.Offsetof(c.satp)),
		"OffSTVEC":                  abiPair(abi.OffSTVEC, unsafe.Offsetof(c.stvec)),
		"OffSScratch":               abiPair(abi.OffSScratch, unsafe.Offsetof(c.sscratch)),
		"OffSEPC":                   abiPair(abi.OffSEPC, unsafe.Offsetof(c.sepc)),
		"OffSCause":                 abiPair(abi.OffSCause, unsafe.Offsetof(c.scause)),
		"OffSTVal":                  abiPair(abi.OffSTVal, unsafe.Offsetof(c.stval)),
		"OffMEDeleg":                abiPair(abi.OffMEDeleg, unsafe.Offsetof(c.medeleg)),
		"OffMIDeleg":                abiPair(abi.OffMIDeleg, unsafe.Offsetof(c.mideleg)),
		"OffMIE":                    abiPair(abi.OffMIE, unsafe.Offsetof(c.mie)),
		"OffMIP":                    abiPair(abi.OffMIP, unsafe.Offsetof(c.mip)),
		"OffSIE":                    abiPair(abi.OffSIE, unsafe.Offsetof(c.sie)),
		"OffSIP":                    abiPair(abi.OffSIP, unsafe.Offsetof(c.sip)),
		"OffMCounterEn":             abiPair(abi.OffMCounterEn, unsafe.Offsetof(c.mcounteren)),
		"OffSCounterEn":             abiPair(abi.OffSCounterEn, unsafe.Offsetof(c.scounteren)),
		"OffMEnvCfg":                abiPair(abi.OffMEnvCfg, unsafe.Offsetof(c.menvcfg)),
		"OffMCountInh":              abiPair(abi.OffMCountInh, unsafe.Offsetof(c.mcountinh)),
		"OffSTimeCmp":               abiPair(abi.OffSTimeCmp, unsafe.Offsetof(c.stimecmp)),
		"OffSTIP":                   abiPair(abi.OffSTIP, unsafe.Offsetof(c.stip)),
		"OffStrictCSR":              abiPair(abi.OffStrictCSR, unsafe.Offsetof(c.strictCSR)),
		"MemOffBase":                abiPair(abi.MemOffBase, unsafe.Offsetof(m.base)),
		"MemOffMask":                abiPair(abi.MemOffMask, unsafe.Offsetof(m.mask)),
		"MemOffSize":                abiPair(abi.MemOffSize, unsafe.Offsetof(m.size)),
		"MemOffExecRegions":         abiPair(abi.MemOffExecRegions, unsafe.Offsetof(m.execRegions)),
		"MemOffExecPageGenerations": abiPair(abi.MemOffExecPageGenerations, unsafe.Offsetof(m.execPageGenerations)),
		"MemOffLoadedELFSize":       abiPair(abi.MemOffLoadedELFSize, unsafe.Offsetof(m.loadedELFSize)),
		"MemOffLoadedELFImageSize":  abiPair(abi.MemOffLoadedELFImageSize, unsafe.Offsetof(m.loadedELFImageSize)),
		"MemOffAccessOverlay":       abiPair(abi.MemOffAccessOverlay, unsafe.Offsetof(m.accessOverlay)),
		"MemOffMMIO":                abiPair(abi.MemOffMMIO, unsafe.Offsetof(m.mmio)),
		"MemOffTohostAddr":          abiPair(abi.MemOffTohostAddr, unsafe.Offsetof(m.TohostAddr)),
		"SliceOffData":              abiPair(abi.SliceOffData, unsafe.Offsetof(sh.Data)),
		"SliceOffLen":               abiPair(abi.SliceOffLen, unsafe.Offsetof(sh.Len)),
		"ExecRegionOffVAddrBegin":   abiPair(abi.ExecRegionOffVAddrBegin, unsafe.Offsetof(er.VAddrBegin)),
		"ExecRegionOffVAddrEnd":     abiPair(abi.ExecRegionOffVAddrEnd, unsafe.Offsetof(er.VAddrEnd)),
		"ExecRegionOffIsLikelyJIT":  abiPair(abi.ExecRegionOffIsLikelyJIT, unsafe.Offsetof(er.IsLikelyJIT)),
		"MMUOffLoadTLB":             abiPair(abi.MMUOffLoadTLB, unsafe.Offsetof(mmu.loadTLB)),
		"MMUOffStoreTLB":            abiPair(abi.MMUOffStoreTLB, unsafe.Offsetof(mmu.storeTLB)),
		"TLBEntryOffTag":            abiPair(abi.TLBEntryOffTag, unsafe.Offsetof(te.tag)),
		"TLBEntryOffMask":           abiPair(abi.TLBEntryOffMask, unsafe.Offsetof(te.mask)),
		"TLBEntryOffPAddrBase":      abiPair(abi.TLBEntryOffPAddrBase, unsafe.Offsetof(te.paddrBase)),
		"TLBEntryOffHostBase":       abiPair(abi.TLBEntryOffHostBase, unsafe.Offsetof(te.hostBase)),
		"TLBEntryOffPerms":          abiPair(abi.TLBEntryOffPerms, unsafe.Offsetof(te.perms)),
		"TLBEntryOffValid":          abiPair(abi.TLBEntryOffValid, unsafe.Offsetof(te.valid)),
		"TLBEntryOffDirect":         abiPair(abi.TLBEntryOffDirect, unsafe.Offsetof(te.direct)),
		"TLBEntryOffMMIO":           abiPair(abi.TLBEntryOffMMIO, unsafe.Offsetof(te.mmio)),
	}
	for name, pair := range checks {
		if pair[0] != pair[1] {
			t.Fatalf("%s = %d, want %d", name, pair[0], pair[1])
		}
	}
	if abi.ExecRegionSize != unsafe.Sizeof(er) {
		t.Fatalf("ExecRegionSize = %d, want %d", abi.ExecRegionSize, unsafe.Sizeof(er))
	}
	if abi.TLBEntrySize != unsafe.Sizeof(te) {
		t.Fatalf("TLBEntrySize = %d, want %d", abi.TLBEntrySize, unsafe.Sizeof(te))
	}
	if abi.TLBSize != tlbSize {
		t.Fatalf("TLBSize = %d, want %d", abi.TLBSize, tlbSize)
	}
	if abi.TLBMask != tlbMask {
		t.Fatalf("TLBMask = %d, want %d", abi.TLBMask, tlbMask)
	}
	if abi.TLBPermR != tlbR || abi.TLBPermW != tlbW || abi.TLBPermU != tlbU {
		t.Fatalf("TLB perms R/W/U = %#x/%#x/%#x, want %#x/%#x/%#x", abi.TLBPermR, abi.TLBPermW, abi.TLBPermU, tlbR, tlbW, tlbU)
	}
}

func abiPair(got, want uintptr) [2]uintptr {
	return [2]uintptr{got, want}
}

func TestCurrentAccelABI_RegisterFileLayout(t *testing.T) {
	abi := CurrentAccelABI()
	if abi.OffF-abi.OffX != 256 {
		t.Fatalf("F register file offset delta = %d, want 256", abi.OffF-abi.OffX)
	}
	if abi.OffFCSR-abi.OffX != 512 {
		t.Fatalf("FCSR offset delta = %d, want 512", abi.OffFCSR-abi.OffX)
	}
	if abi.GuestPageSize != GuestPageSize {
		t.Fatalf("GuestPageSize = %d, want %d", abi.GuestPageSize, GuestPageSize)
	}
}

func TestCurrentAccelABI_Helpers(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	abi := CurrentAccelABI()
	h := abi.Helpers

	if f := h.Store32(cpu, 0x1000, 0x12345678); f != nil {
		t.Fatalf("Store32: %v", f)
	}
	if got, f := h.Load32(cpu, 0x1000); f != nil || got != 0x12345678 {
		t.Fatalf("Load32 = 0x%x, %v; want 0x12345678, nil", got, f)
	}
	if f := h.Store16U(cpu, 0x1003, 0xabcd); f != nil {
		t.Fatalf("Store16U: %v", f)
	}
	if got, f := h.Load16U(cpu, 0x1003); f != nil || got != 0xabcd {
		t.Fatalf("Load16U = 0x%x, %v; want 0xabcd, nil", got, f)
	}
	if f := h.Store64(cpu, 0x1800, 1); f != nil {
		t.Fatalf("Store64 watch: %v", f)
	}
	cpu.SetWatchAddr(0x1800)
	if code, ok := h.PollWatchAddr(cpu); !ok || code != 0 {
		t.Fatalf("PollWatchAddr = %d, %v; want 0, true", code, ok)
	}
	if f := h.Store64(cpu, 0x1800, 7); f != nil {
		t.Fatalf("Store64 watch fail code: %v", f)
	}
	if code, ok := h.PollWatchAddr(cpu); !ok || code != 7 {
		t.Fatalf("PollWatchAddr = %d, %v; want 7, true", code, ok)
	}
	if f := h.Store64(cpu, 0x1800, 0); f != nil {
		t.Fatalf("Store64 clear watch: %v", f)
	}
	if code, ok := h.PollWatchAddr(cpu); ok || code != 0 {
		t.Fatalf("PollWatchAddr clear = %d, %v; want 0, false", code, ok)
	}
	cpu.SetWatchAddr(0)

	if f := h.Store32(cpu, 0x2000, 0x00100073); f != nil {
		t.Fatalf("store EBREAK: %v", f)
	}
	cpu.SetPC(0x2000)
	if got, f := h.Fetch32(cpu, 0x2000); f != nil || got != 0x00100073 {
		t.Fatalf("Fetch32 = 0x%x, %v; want EBREAK, nil", got, f)
	}
	if err := h.Step(cpu); !errors.Is(err, ErrEbreak) {
		t.Fatalf("Step = %v, want ErrEbreak", err)
	}
	if h.TakePendingBiosInterrupt(cpu) {
		t.Fatal("TakePendingBiosInterrupt on idle CPU = true, want false")
	}
	h.ServiceBiosWFI(cpu)
	h.AddInstrBegun(cpu, 3)
	h.AddInstrRetired(cpu, 2)
	if got := cpu.RiscvInstrBegun(); got != 3 {
		t.Fatalf("RiscvInstrBegun after helper = %d, want 3", got)
	}
	if got := cpu.RiscvInstrRetired(); got != 2 {
		t.Fatalf("RiscvInstrRetired after helper = %d, want 2", got)
	}

	h.AddExecRegion(mem, 0x2000, 0x3000, true)
	if got := h.FindExecRegion(mem, 0x2000); got == nil || !got.IsLikelyJIT {
		t.Fatalf("FindExecRegion = %+v, want JIT region", got)
	}
	if got := h.ExecPageGeneration(mem, 0x2000); got != 1 {
		t.Fatalf("ExecPageGeneration after AddExecRegion = %d, want 1", got)
	}
	if regs := h.ExecRegions(mem); len(regs) != 1 || regs[0].VAddrBegin != 0x2000 || regs[0].VAddrEnd != 0x3000 {
		t.Fatalf("ExecRegions = %+v, want [0x2000,0x3000)", regs)
	}
	h.BumpExecGeneration(mem, 0x2fff, 0x3001)
	if gens := h.ExecPageGenerations(mem, 0x2000, 0x4000); len(gens) != 2 || gens[0].Generation != 2 || gens[1].Generation != 1 {
		t.Fatalf("ExecPageGenerations after cross-page bump = %+v, want generations [2,1]", gens)
	}
	h.RemoveExecRegion(mem, 0x2000, 0x3000)
	if got := h.FindExecRegion(mem, 0x2000); got != nil {
		t.Fatalf("FindExecRegion after remove = %+v, want nil", got)
	}
	if got := h.ExecPageGeneration(mem, 0x2000); got != 3 {
		t.Fatalf("ExecPageGeneration after RemoveExecRegion = %d, want 3", got)
	}
}

func sameUint64Slice(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestMachineAcceleratorLifecycle(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	m := NewMachine(NewCPU(*mem), nil)
	accel := &noopAccelerator{}
	m.Accel = accel

	child, err := m.Clone()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if child.Accel != nil {
		t.Fatalf("cloned Machine.Accel = %#v, want nil until clone-safe accelerator policy exists", child.Accel)
	}

	m.Close()
	if !accel.closed {
		t.Fatal("Machine.Close did not close accelerator")
	}
	if m.Accel != nil {
		t.Fatalf("Machine.Accel after Close = %#v, want nil", m.Accel)
	}
}

func TestCompileConfiguredAcceleratorMergesLoaderAOTSeeds(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	accel := &recordingAccelerator{}
	m := NewMachine(NewCPU(*mem), nil)
	installed, err := compileConfiguredAccelerator(context.Background(), EmuConfig{
		AccelFactory: func() (Accelerator, error) {
			return accel, nil
		},
		AccelOptions: AccelOptions{
			AOTSeedPCs: []uint64{0x1000, 0x2000},
		},
	}, m, 0x2000, 0x3000, 0x3000)
	if err != nil {
		t.Fatalf("compileConfiguredAccelerator: %v", err)
	}
	if !installed {
		t.Fatal("compileConfiguredAccelerator installed = false, want true")
	}
	if accel.compileCalls != 1 {
		t.Fatalf("compile calls = %d, want 1", accel.compileCalls)
	}
	want := []uint64{0x1000, 0x2000, 0x3000}
	if !sameUint64Slice(accel.compileOptions.AOTSeedPCs, want) {
		t.Fatalf("AOTSeedPCs = %#x, want %#x", accel.compileOptions.AOTSeedPCs, want)
	}
}

func TestJea9LinuxExecMetadataNotifiesMachineAccelerator(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)
	accel := &invalidationRecorderAccelerator{}
	m.Accel = accel
	jlinux := NewJea9Linux(Jea9LinuxOptions{})
	jlinux.activeMachine = m

	mapped := jlinux.sysMmap(cpu, 0, GuestPageSize, jea9LinuxProtRead|jea9LinuxProtExec, jea9LinuxMapAnonymous, 0, 0)
	if mapped < 0 {
		t.Fatalf("sysMmap executable = %d, want guest address", mapped)
	}
	addr := uint64(mapped)
	cpuMem := &cpu.mem
	if got := cpuMem.FindExecRegion(addr); got == nil || !got.Contains(addr) || got.IsLikelyJIT {
		t.Fatalf("exec region after mmap = %+v, want non-JIT region containing %#x", got, addr)
	}
	if rc := jlinux.sysMprotect(cpu, addr, GuestPageSize, jea9LinuxProtRead|jea9LinuxProtWrite); rc != 0 {
		t.Fatalf("sysMprotect remove exec = %d, want 0", rc)
	}
	if got := cpuMem.FindExecRegion(addr); got != nil {
		t.Fatalf("exec region after mprotect RW = %+v, want nil", got)
	}
	if rc := jlinux.sysMprotect(cpu, addr, GuestPageSize, jea9LinuxProtRead|jea9LinuxProtExec); rc != 0 {
		t.Fatalf("sysMprotect add exec = %d, want 0", rc)
	}
	if rc := jlinux.sysMunmap(cpu, addr, GuestPageSize); rc != 0 {
		t.Fatalf("sysMunmap = %d, want 0", rc)
	}

	want := []recordedInvalidation{
		{addr: addr, length: GuestPageSize, reason: InvalidateExecRegionAdd},
		{addr: addr, length: GuestPageSize, reason: InvalidateExecRegionRemove},
		{addr: addr, length: GuestPageSize, reason: InvalidateExecRegionAdd},
		{addr: addr, length: GuestPageSize, reason: InvalidateExecRegionRemove},
	}
	if len(accel.invalidations) != len(want) {
		t.Fatalf("invalidations = %+v, want %+v", accel.invalidations, want)
	}
	for i := range want {
		if accel.invalidations[i] != want[i] {
			t.Fatalf("invalidation[%d] = %+v, want %+v", i, accel.invalidations[i], want[i])
		}
	}
}

func TestMachineRunBudgetFallsBackToInterpreter(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)

	if f := mem.Store32(0, 0x00000013); f != nil { // ADDI x0, x0, 0
		t.Fatalf("Store32: %v", f)
	}
	res, err := m.RunMachineBudget(&cpu.Notes, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want expired", res)
	}
	if got := cpu.PC(); got != 4 {
		t.Fatalf("PC = %d, want 4", got)
	}
}

func TestMachineRunBudgetUsesAccelerator(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)
	accel := &noopAccelerator{}
	m.Accel = accel

	res, err := m.RunBiosMachineBudget(&cpu.Notes, 17)
	if err != nil {
		t.Fatal(err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunBiosMachineBudget result = %v, want expired", res)
	}
	if accel.runCalls != 1 {
		t.Fatalf("accelerator run calls = %d, want 1", accel.runCalls)
	}
	if accel.lastCPU != cpu {
		t.Fatalf("accelerator CPU = %#v, want test CPU", accel.lastCPU)
	}
	if accel.lastNotes != &cpu.Notes {
		t.Fatalf("accelerator NoteChain = %#v, want CPU notes", accel.lastNotes)
	}
	if accel.lastBudget != 17 {
		t.Fatalf("accelerator budget = %d, want 17", accel.lastBudget)
	}
	if accel.lastMode != AccelRunBIOS {
		t.Fatalf("accelerator mode = %v, want BIOS", accel.lastMode)
	}
}

func TestMachineRunDualBudgetFallsBackToInterpreter(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)
	if f := mem.Store32(0, 0x00000013); f != nil { // ADDI x0, x0, 0
		t.Fatalf("Store32: %v", f)
	}

	res, limit, err := m.RunMachineDualBudget(&cpu.Notes, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res != RunBudgetExpired || limit != RunBudgetLimitRetired {
		t.Fatalf("RunMachineDualBudget result = (%v, %v), want (%v, %v)", res, limit, RunBudgetExpired, RunBudgetLimitRetired)
	}
	if got := cpu.PC(); got != 4 {
		t.Fatalf("PC = %d, want 4", got)
	}
}

func TestMachineRunDualBudgetUsesAccelerator(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)
	accel := &dualNoopAccelerator{}
	m.Accel = accel

	res, limit, err := m.RunMachineDualBudget(&cpu.Notes, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res != RunBudgetExpired || limit != RunBudgetLimitRetired {
		t.Fatalf("RunMachineDualBudget result = (%v, %v), want (%v, %v)", res, limit, RunBudgetExpired, RunBudgetLimitRetired)
	}
	if accel.dualCalls != 1 {
		t.Fatalf("dual accelerator calls = %d, want 1", accel.dualCalls)
	}
	if accel.lastDualCPU != cpu {
		t.Fatalf("dual accelerator CPU = %#v, want test CPU", accel.lastDualCPU)
	}
	if accel.lastDualNotePtr != &cpu.Notes {
		t.Fatalf("dual accelerator NoteChain = %#v, want CPU notes", accel.lastDualNotePtr)
	}
	if accel.lastAttempt != 3 || accel.lastRetired != 5 {
		t.Fatalf("dual budgets = (%d, %d), want (3, 5)", accel.lastAttempt, accel.lastRetired)
	}
	if accel.lastDualMode != AccelRunPlain {
		t.Fatalf("dual accelerator mode = %v, want plain", accel.lastDualMode)
	}
}

func TestMachineRunBiosDualBudgetFallsBackToInterpreter(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)
	if f := mem.Store32(0, 0x00000013); f != nil { // ADDI x0, x0, 0
		t.Fatalf("Store32: %v", f)
	}

	res, limit, err := m.RunBiosMachineDualBudget(&cpu.Notes, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res != RunBudgetExpired || limit != RunBudgetLimitRetired {
		t.Fatalf("RunBiosMachineDualBudget result = (%v, %v), want (%v, %v)", res, limit, RunBudgetExpired, RunBudgetLimitRetired)
	}
	if got := cpu.PC(); got != 4 {
		t.Fatalf("PC = %d, want 4", got)
	}
}

func TestMachineRunBiosDualBudgetUsesAccelerator(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)
	m := NewMachine(cpu, nil)
	accel := &dualNoopAccelerator{}
	m.Accel = accel

	res, limit, err := m.RunBiosMachineDualBudget(&cpu.Notes, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res != RunBudgetExpired || limit != RunBudgetLimitRetired {
		t.Fatalf("RunBiosMachineDualBudget result = (%v, %v), want (%v, %v)", res, limit, RunBudgetExpired, RunBudgetLimitRetired)
	}
	if accel.dualCalls != 1 {
		t.Fatalf("dual accelerator calls = %d, want 1", accel.dualCalls)
	}
	if accel.lastDualMode != AccelRunBIOS {
		t.Fatalf("dual accelerator mode = %v, want BIOS", accel.lastDualMode)
	}
}

func TestRunEmuUsesConfiguredAccelerator(t *testing.T) {
	elfPath := filepath.Join(t.TempDir(), "tiny.elf")
	if err := os.WriteFile(elfPath, BuildELF(0x10000, []uint32{0x00000013}), 0644); err != nil {
		t.Fatal(err)
	}
	accel := &exitAccelerator{code: 23}
	code, err := runEmu(EmuConfig{
		RunPath: elfPath,
		Memory:  "64MB",
		AccelFactory: func() (Accelerator, error) {
			return accel, nil
		},
	})
	if err != nil {
		t.Fatalf("runEmu with accelerator: %v", err)
	}
	if code != 23 {
		t.Fatalf("exit code = %d, want 23", code)
	}
	if accel.compileCalls != 1 {
		t.Fatalf("compile calls = %d, want 1", accel.compileCalls)
	}
	if accel.runCalls == 0 {
		t.Fatal("accelerator was compiled but not used")
	}
	if !accel.closed {
		t.Fatal("runEmu did not close configured accelerator")
	}
}

func TestRunEmuBiosUsesConfiguredAccelerator(t *testing.T) {
	biosPath := filepath.Join(t.TempDir(), "tiny-bios.elf")
	if err := os.WriteFile(biosPath, BuildELF(0x10000, []uint32{0x00000013}), 0644); err != nil {
		t.Fatal(err)
	}
	accel := &exitAccelerator{code: 29}
	var stdout, stderr bytes.Buffer
	code, err := runEmu(EmuConfig{
		BiosPath: biosPath,
		Memory:   "512MB",
		Budget:   "1",
		Stdin:    strings.NewReader(""),
		Stdout:   &stdout,
		Stderr:   &stderr,
		AccelFactory: func() (Accelerator, error) {
			return accel, nil
		},
	})
	if err != nil {
		t.Fatalf("runEmu BIOS with accelerator: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if code != 29 {
		t.Fatalf("exit code = %d, want 29", code)
	}
	if accel.compileCalls != 1 {
		t.Fatalf("compile calls = %d, want 1", accel.compileCalls)
	}
	if accel.runCalls == 0 {
		t.Fatal("BIOS accelerator was compiled but not used")
	}
	if !accel.closed {
		t.Fatal("runEmu BIOS did not close configured accelerator")
	}
}

func TestEmuConfigRejectsAcceleratorWithOldJIT(t *testing.T) {
	elfPath := filepath.Join(t.TempDir(), "tiny.elf")
	if err := os.WriteFile(elfPath, BuildELF(0x10000, []uint32{0x00000013}), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := EmuConfig{
		RunPath:      elfPath,
		JITLazy:      true,
		AccelFactory: func() (Accelerator, error) { return &exitAccelerator{}, nil },
	}
	if err := cfg.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted accelerator with old JIT")
	}
}
