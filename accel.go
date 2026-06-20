package riscv

import (
	"context"
	"unsafe"
)

// AccelABIVersion is incremented whenever AccelABI's layout contract changes.
const AccelABIVersion uintptr = 5

// Accelerator is the integration point for an external native RV64 backend.
//
// The interpreter remains the semantic oracle. Accelerators must produce the
// same CPU, memory, trap, note, budget, and virtual-hardware behavior as the
// interpreter-backed RunMachineBudget and RunBiosMachineBudget loops.
type Accelerator interface {
	CompileMachine(ctx context.Context, m *Machine, opts AccelOptions) error
	RunMachineBudget(cpu *CPU, nc *NoteChain, budget uint64, mode AccelRunMode) (RunBudgetResult, error)
	Invalidate(addr, length uint64, reason InvalidateReason)
	Close() error
}

// AccelOptions configures accelerator compilation for a Machine.
type AccelOptions struct {
	// DebugInterpreterFallback allows an accelerator under development to fall
	// back to the interpreter for unsupported blocks. Production correctness
	// modes should leave this false so missing translations fail explicitly.
	DebugInterpreterFallback bool

	// ExactInstructionAccounting requires interpreter-compatible budget,
	// interrupt, WFI, and instruction-counter points. This should stay true for
	// deterministic simulation testing.
	ExactInstructionAccounting bool

	// DisableAOTPrecompile skips eager executable-region block discovery during
	// CompileMachine. Runtime block discovery remains available for dynamic or
	// indirect targets.
	DisableAOTPrecompile bool

	// MaxAOTPrecompileBlocks caps eager block discovery. Zero means no explicit
	// cap. This protects tests and exploratory runs from pathological executable
	// metadata without changing runtime lazy translation semantics.
	MaxAOTPrecompileBlocks int
}

// AccelRunMode selects which existing budget loop semantics to preserve.
type AccelRunMode uint8

const (
	AccelRunPlain AccelRunMode = iota
	AccelRunBIOS
)

// InvalidateReason classifies why native guest-code translations are stale.
type InvalidateReason uint8

const (
	InvalidateGuestStore InvalidateReason = iota
	InvalidateFenceI
	InvalidateSFenceVMA
	InvalidateSATPWrite
	InvalidateExecRegionAdd
	InvalidateExecRegionRemove
	InvalidateUnmap
	InvalidateMachineClose
)

// AccelABI exposes the stable state layout and slow-path hooks used by native
// accelerators. Offsets are deliberately versioned so external code never has
// to infer unexported CPU or GuestMemory layout via reflection.
type AccelABI struct {
	Version uintptr

	CPUSize uintptr
	MemSize uintptr
	MMUSize uintptr

	OffMem               uintptr
	OffPC                uintptr
	OffX                 uintptr
	OffF                 uintptr
	OffFCSR              uintptr
	OffRiscvInstrBegun   uintptr
	OffRiscvInstrRetired uintptr
	OffWFI               uintptr
	OffLastTrapCause     uintptr
	OffLastTrapInsnLen   uintptr
	OffPriv              uintptr
	OffMMU               uintptr
	OffNotes             uintptr
	OffResvAddr          uintptr
	OffResvValid         uintptr
	OffWatchAddr         uintptr
	OffExitCode          uintptr

	OffMTVEC      uintptr
	OffMScratch   uintptr
	OffMEPC       uintptr
	OffMCause     uintptr
	OffMStatus    uintptr
	OffMTVal      uintptr
	OffSATP       uintptr
	OffSTVEC      uintptr
	OffSScratch   uintptr
	OffSEPC       uintptr
	OffSCause     uintptr
	OffSTVal      uintptr
	OffMEDeleg    uintptr
	OffMIDeleg    uintptr
	OffMIE        uintptr
	OffMIP        uintptr
	OffSIE        uintptr
	OffSIP        uintptr
	OffMCounterEn uintptr
	OffSCounterEn uintptr
	OffMEnvCfg    uintptr
	OffMCountInh  uintptr
	OffSTimeCmp   uintptr
	OffSTIP       uintptr
	OffStrictCSR  uintptr

	MemOffBase                uintptr
	MemOffMask                uintptr
	MemOffSize                uintptr
	MemOffExecRegions         uintptr
	MemOffExecPageGenerations uintptr
	MemOffLoadedELFSize       uintptr
	MemOffLoadedELFImageSize  uintptr
	MemOffAccessOverlay       uintptr
	MemOffMMIO                uintptr
	MemOffTohostAddr          uintptr

	SliceOffData uintptr
	SliceOffLen  uintptr

	ExecRegionSize           uintptr
	ExecRegionOffVAddrBegin  uintptr
	ExecRegionOffVAddrEnd    uintptr
	ExecRegionOffIsLikelyJIT uintptr

	MMUOffLoadTLB  uintptr
	MMUOffStoreTLB uintptr

	TLBSize uintptr
	TLBMask uint64

	TLBEntrySize         uintptr
	TLBEntryOffTag       uintptr
	TLBEntryOffMask      uintptr
	TLBEntryOffPAddrBase uintptr
	TLBEntryOffHostBase  uintptr
	TLBEntryOffPerms     uintptr
	TLBEntryOffValid     uintptr
	TLBEntryOffDirect    uintptr
	TLBEntryOffMMIO      uintptr
	TLBPermR             uint8
	TLBPermW             uint8
	TLBPermU             uint8

	GuestPageSize uint64

	Helpers AccelHelpers
}

type accelSliceHeader struct {
	Data unsafe.Pointer
	Len  uintptr
	Cap  uintptr
}

// AccelHelpers are exact Go slow paths for behavior that native code must not
// approximate. Initial accelerators should use these heavily, then graduate
// selected operations to guarded inline fast paths only with differential tests.
type AccelHelpers struct {
	RunMachineBudget         func(cpu *CPU, nc *NoteChain, budget uint64) (RunBudgetResult, error)
	RunBiosMachineBudget     func(cpu *CPU, nc *NoteChain, budget uint64) (RunBudgetResult, error)
	Step                     func(cpu *CPU) error
	TakePendingBiosInterrupt func(cpu *CPU) bool
	ServiceBiosWFI           func(cpu *CPU)
	PollWatchAddr            func(cpu *CPU) (int, bool)
	AddInstrBegun            func(cpu *CPU, n uint64)
	AddInstrRetired          func(cpu *CPU, n uint64)

	Fetch16  func(cpu *CPU, addr uint64) (uint16, *MemFault)
	Fetch32  func(cpu *CPU, addr uint64) (uint32, *MemFault)
	Fetch32U func(cpu *CPU, addr uint64) (uint32, *MemFault)

	Load8   func(cpu *CPU, addr uint64) (uint8, *MemFault)
	Load16  func(cpu *CPU, addr uint64) (uint16, *MemFault)
	Load32  func(cpu *CPU, addr uint64) (uint32, *MemFault)
	Load64  func(cpu *CPU, addr uint64) (uint64, *MemFault)
	Load16U func(cpu *CPU, addr uint64) (uint16, *MemFault)
	Load32U func(cpu *CPU, addr uint64) (uint32, *MemFault)
	Load64U func(cpu *CPU, addr uint64) (uint64, *MemFault)

	Store8   func(cpu *CPU, addr uint64, v uint8) *MemFault
	Store16  func(cpu *CPU, addr uint64, v uint16) *MemFault
	Store32  func(cpu *CPU, addr uint64, v uint32) *MemFault
	Store64  func(cpu *CPU, addr uint64, v uint64) *MemFault
	Store16U func(cpu *CPU, addr uint64, v uint16) *MemFault
	Store32U func(cpu *CPU, addr uint64, v uint32) *MemFault
	Store64U func(cpu *CPU, addr uint64, v uint64) *MemFault

	ReadCSR          func(cpu *CPU, addr uint32) (uint64, bool)
	WriteCSR         func(cpu *CPU, addr uint32, value uint64) bool
	CheckCSRAccess   func(cpu *CPU, addr uint32, write bool) bool
	FlushTLB         func(cpu *CPU)
	TrapMachineError func(cpu *CPU, err error) bool
	NoteFromCPUError func(cpu *CPU, err error) Note
	DeliverNote      func(cpu *CPU, nc *NoteChain, n Note) NoteDisposition

	AddExecRegion       func(mem *GuestMemory, begin, end uint64, isJIT bool)
	RemoveExecRegion    func(mem *GuestMemory, begin, end uint64)
	FindExecRegion      func(mem *GuestMemory, pc uint64) *ExecRegion
	ExecRegions         func(mem *GuestMemory) []ExecRegion
	BumpExecGeneration  func(mem *GuestMemory, begin, end uint64)
	ExecPageGeneration  func(mem *GuestMemory, addr uint64) uint64
	ExecPageGenerations func(mem *GuestMemory, begin, end uint64) []ExecPageGeneration
}

// CurrentAccelABI returns the current accelerator ABI for this build.
func CurrentAccelABI() AccelABI {
	var c CPU
	var m GuestMemory
	var mmu MMU
	var sh accelSliceHeader
	var er ExecRegion
	var te tlbEntry
	return AccelABI{
		Version: AccelABIVersion,

		CPUSize: unsafe.Sizeof(c),
		MemSize: unsafe.Sizeof(m),
		MMUSize: unsafe.Sizeof(mmu),

		OffMem:               unsafe.Offsetof(c.mem),
		OffPC:                unsafe.Offsetof(c.pc),
		OffX:                 unsafe.Offsetof(c.x),
		OffF:                 unsafe.Offsetof(c.f),
		OffFCSR:              unsafe.Offsetof(c.fcsr),
		OffRiscvInstrBegun:   unsafe.Offsetof(c.riscvInstrBegun),
		OffRiscvInstrRetired: unsafe.Offsetof(c.riscvInstrRetired),
		OffWFI:               unsafe.Offsetof(c.wfi),
		OffLastTrapCause:     unsafe.Offsetof(c.lastTrapCause),
		OffLastTrapInsnLen:   unsafe.Offsetof(c.lastTrapInsnLen),
		OffPriv:              unsafe.Offsetof(c.priv),
		OffMMU:               unsafe.Offsetof(c.mmu),
		OffNotes:             unsafe.Offsetof(c.Notes),
		OffResvAddr:          unsafe.Offsetof(c.resvAddr),
		OffResvValid:         unsafe.Offsetof(c.resvValid),
		OffWatchAddr:         unsafe.Offsetof(c.watchAddr),
		OffExitCode:          unsafe.Offsetof(c.ExitCode),

		OffMTVEC:      unsafe.Offsetof(c.mtvec),
		OffMScratch:   unsafe.Offsetof(c.mscratch),
		OffMEPC:       unsafe.Offsetof(c.mepc),
		OffMCause:     unsafe.Offsetof(c.mcause),
		OffMStatus:    unsafe.Offsetof(c.mstatus),
		OffMTVal:      unsafe.Offsetof(c.mtval),
		OffSATP:       unsafe.Offsetof(c.satp),
		OffSTVEC:      unsafe.Offsetof(c.stvec),
		OffSScratch:   unsafe.Offsetof(c.sscratch),
		OffSEPC:       unsafe.Offsetof(c.sepc),
		OffSCause:     unsafe.Offsetof(c.scause),
		OffSTVal:      unsafe.Offsetof(c.stval),
		OffMEDeleg:    unsafe.Offsetof(c.medeleg),
		OffMIDeleg:    unsafe.Offsetof(c.mideleg),
		OffMIE:        unsafe.Offsetof(c.mie),
		OffMIP:        unsafe.Offsetof(c.mip),
		OffSIE:        unsafe.Offsetof(c.sie),
		OffSIP:        unsafe.Offsetof(c.sip),
		OffMCounterEn: unsafe.Offsetof(c.mcounteren),
		OffSCounterEn: unsafe.Offsetof(c.scounteren),
		OffMEnvCfg:    unsafe.Offsetof(c.menvcfg),
		OffMCountInh:  unsafe.Offsetof(c.mcountinh),
		OffSTimeCmp:   unsafe.Offsetof(c.stimecmp),
		OffSTIP:       unsafe.Offsetof(c.stip),
		OffStrictCSR:  unsafe.Offsetof(c.strictCSR),

		MemOffBase:                unsafe.Offsetof(m.base),
		MemOffMask:                unsafe.Offsetof(m.mask),
		MemOffSize:                unsafe.Offsetof(m.size),
		MemOffExecRegions:         unsafe.Offsetof(m.execRegions),
		MemOffExecPageGenerations: unsafe.Offsetof(m.execPageGenerations),
		MemOffLoadedELFSize:       unsafe.Offsetof(m.loadedELFSize),
		MemOffLoadedELFImageSize:  unsafe.Offsetof(m.loadedELFImageSize),
		MemOffAccessOverlay:       unsafe.Offsetof(m.accessOverlay),
		MemOffMMIO:                unsafe.Offsetof(m.mmio),
		MemOffTohostAddr:          unsafe.Offsetof(m.TohostAddr),

		SliceOffData: unsafe.Offsetof(sh.Data),
		SliceOffLen:  unsafe.Offsetof(sh.Len),

		ExecRegionSize:           unsafe.Sizeof(er),
		ExecRegionOffVAddrBegin:  unsafe.Offsetof(er.VAddrBegin),
		ExecRegionOffVAddrEnd:    unsafe.Offsetof(er.VAddrEnd),
		ExecRegionOffIsLikelyJIT: unsafe.Offsetof(er.IsLikelyJIT),

		MMUOffLoadTLB:  unsafe.Offsetof(mmu.loadTLB),
		MMUOffStoreTLB: unsafe.Offsetof(mmu.storeTLB),

		TLBSize: tlbSize,
		TLBMask: tlbMask,

		TLBEntrySize:         unsafe.Sizeof(te),
		TLBEntryOffTag:       unsafe.Offsetof(te.tag),
		TLBEntryOffMask:      unsafe.Offsetof(te.mask),
		TLBEntryOffPAddrBase: unsafe.Offsetof(te.paddrBase),
		TLBEntryOffHostBase:  unsafe.Offsetof(te.hostBase),
		TLBEntryOffPerms:     unsafe.Offsetof(te.perms),
		TLBEntryOffValid:     unsafe.Offsetof(te.valid),
		TLBEntryOffDirect:    unsafe.Offsetof(te.direct),
		TLBEntryOffMMIO:      unsafe.Offsetof(te.mmio),
		TLBPermR:             tlbR,
		TLBPermW:             tlbW,
		TLBPermU:             tlbU,

		GuestPageSize: GuestPageSize,
		Helpers:       currentAccelHelpers(),
	}
}

func currentAccelHelpers() AccelHelpers {
	return AccelHelpers{
		RunMachineBudget:     RunMachineBudget,
		RunBiosMachineBudget: RunBiosMachineBudget,
		Step:                 func(cpu *CPU) error { return cpu.Step() },
		TakePendingBiosInterrupt: func(cpu *CPU) bool {
			return cpu.takePendingBiosInterrupt()
		},
		ServiceBiosWFI: func(cpu *CPU) { cpu.serviceBiosWFI() },
		PollWatchAddr: func(cpu *CPU) (int, bool) {
			if cpu.watchAddr == 0 {
				return 0, false
			}
			if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
				return tohostExitCode(v), true
			}
			return 0, false
		},
		AddInstrBegun:   func(cpu *CPU, n uint64) { cpu.riscvInstrBegun += n },
		AddInstrRetired: func(cpu *CPU, n uint64) { cpu.riscvInstrRetired += n },

		Fetch16:  func(cpu *CPU, addr uint64) (uint16, *MemFault) { return cpu.fetch16(addr) },
		Fetch32:  func(cpu *CPU, addr uint64) (uint32, *MemFault) { return cpu.fetch32(addr) },
		Fetch32U: func(cpu *CPU, addr uint64) (uint32, *MemFault) { return cpu.fetch32U(addr) },

		Load8:   func(cpu *CPU, addr uint64) (uint8, *MemFault) { return cpu.load8(addr) },
		Load16:  func(cpu *CPU, addr uint64) (uint16, *MemFault) { return cpu.load16(addr) },
		Load32:  func(cpu *CPU, addr uint64) (uint32, *MemFault) { return cpu.load32(addr) },
		Load64:  func(cpu *CPU, addr uint64) (uint64, *MemFault) { return cpu.load64(addr) },
		Load16U: func(cpu *CPU, addr uint64) (uint16, *MemFault) { return cpu.load16U(addr) },
		Load32U: func(cpu *CPU, addr uint64) (uint32, *MemFault) { return cpu.load32U(addr) },
		Load64U: func(cpu *CPU, addr uint64) (uint64, *MemFault) { return cpu.load64U(addr) },

		Store8:   func(cpu *CPU, addr uint64, v uint8) *MemFault { return cpu.store8(addr, v) },
		Store16:  func(cpu *CPU, addr uint64, v uint16) *MemFault { return cpu.store16(addr, v) },
		Store32:  func(cpu *CPU, addr uint64, v uint32) *MemFault { return cpu.store32(addr, v) },
		Store64:  func(cpu *CPU, addr uint64, v uint64) *MemFault { return cpu.store64(addr, v) },
		Store16U: func(cpu *CPU, addr uint64, v uint16) *MemFault { return cpu.store16U(addr, v) },
		Store32U: func(cpu *CPU, addr uint64, v uint32) *MemFault { return cpu.store32U(addr, v) },
		Store64U: func(cpu *CPU, addr uint64, v uint64) *MemFault { return cpu.store64U(addr, v) },

		ReadCSR:          func(cpu *CPU, addr uint32) (uint64, bool) { return cpu.readCSR(addr) },
		WriteCSR:         func(cpu *CPU, addr uint32, value uint64) bool { return cpu.writeCSR(addr, value) },
		CheckCSRAccess:   func(cpu *CPU, addr uint32, write bool) bool { return cpu.checkCSRAccess(addr, write) },
		FlushTLB:         func(cpu *CPU) { cpu.flushTLB() },
		TrapMachineError: func(cpu *CPU, err error) bool { return cpu.trapMachineError(err) },
		NoteFromCPUError: noteFromCPUError,
		DeliverNote:      func(cpu *CPU, nc *NoteChain, n Note) NoteDisposition { return nc.Deliver(cpu, n) },

		AddExecRegion: func(mem *GuestMemory, begin, end uint64, isJIT bool) {
			mem.AddExecRegion(begin, end, isJIT)
		},
		RemoveExecRegion: func(mem *GuestMemory, begin, end uint64) {
			mem.RemoveExecRegion(begin, end)
		},
		FindExecRegion: func(mem *GuestMemory, pc uint64) *ExecRegion {
			return mem.FindExecRegion(pc)
		},
		ExecRegions: func(mem *GuestMemory) []ExecRegion {
			return mem.ExecRegions()
		},
		BumpExecGeneration: func(mem *GuestMemory, begin, end uint64) {
			mem.BumpExecGeneration(begin, end)
		},
		ExecPageGeneration: func(mem *GuestMemory, addr uint64) uint64 {
			return mem.ExecPageGeneration(addr)
		},
		ExecPageGenerations: func(mem *GuestMemory, begin, end uint64) []ExecPageGeneration {
			return mem.ExecPageGenerations(begin, end)
		},
	}
}
