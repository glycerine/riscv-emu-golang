package riscv

import (
	"errors"
	"math/bits"
	"runtime"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/internal/fenv"
)

func init() {
	var c CPU
	xOff := unsafe.Offsetof(c.x)
	fOff := unsafe.Offsetof(c.f)
	fcsrOff := unsafe.Offsetof(c.fcsr)
	if fOff-xOff != 256 {
		panic("riscv: CPU.f must be at CPU.x + 256 for rv8 register file layout")
	}
	if fcsrOff-xOff != 512 {
		panic("riscv: CPU.fcsr must be at CPU.x + 512 for rv8 register file layout")
	}
}

var ErrEcall = errors.New("ecall")
var ErrEbreak = errors.New("ebreak")
var ErrIllegalInstruction = errors.New("illegal instruction")

type PrivilegeMode uint8

const (
	PrivUser       PrivilegeMode = 0
	PrivSupervisor PrivilegeMode = 1
	PrivMachine    PrivilegeMode = 3
)

const (
	statusSIE  = uint64(1) << 1
	statusMIE  = uint64(1) << 3
	statusSPIE = uint64(1) << 5
	statusMPIE = uint64(1) << 7
	statusSPP  = uint64(1) << 8
	statusMPP  = uint64(3) << 11
	statusFS   = uint64(3) << 13
	statusXS   = uint64(3) << 15
	statusMPRV = uint64(1) << 17
	statusSUM  = uint64(1) << 18
	statusMXR  = uint64(1) << 19
	statusTVM  = uint64(1) << 20
	statusTW   = uint64(1) << 21
	statusTSR  = uint64(1) << 22
	statusUXL  = uint64(3) << 32
	statusSD   = uint64(1) << 63

	sstatusMask        = statusSIE | statusSPIE | statusSPP | statusFS | statusXS | statusSUM | statusMXR | statusUXL
	sstatusReadable    = sstatusMask | statusSD
	mstatusWritable    = sstatusMask | statusMIE | statusMPIE | statusMPRV | statusMPP | statusTVM | statusTW | statusTSR
	supervisorIPMask   = mipSSIP | mipSTIP | mipSEIP
	machineIPMask      = supervisorIPMask | mipMSIP | mipMTIP | mipMEIP
	counterCycleBit    = uint64(1) << 0
	counterTimeBit     = uint64(1) << 1
	counterInstretBit  = uint64(1) << 2
	counterCSRMask     = counterCycleBit | counterTimeBit | counterInstretBit
	implementedMideleg = supervisorIPMask
	implementedMedeleg = uint64(0x000000000004b109)
	implementedMieMask = machineIPMask
	implementedSieMask = supervisorIPMask
	implementedMipMask = mipSSIP | mipMSIP | mipSEIP | mipMEIP
	implementedSipMask = mipSSIP | mipSEIP
)

const (
	rmRNE = uint8(0)
	rmRTZ = uint8(1)
	rmRDN = uint8(2)
	rmRUP = uint8(3)
	rmRMM = uint8(4)
	rmDYN = uint8(7)
)

// CPU is a single RV64I hart.
// mem is inline and first for cache locality — touched on every instruction.
type CPU struct {
	mem               GuestMemory
	pc                uint64
	x                 [32]uint64 // x[0] is hardwired zero
	f                 [32]uint64 // f0-f31: NaN-boxed float32 or raw float64 bits
	fcsr              uint32     // FP control/status: fflags[4:0] + frm[7:5]
	riscvInstrBegun   uint64     // guest instruction attempts begun, including faulting attempts
	riscvInstrRetired uint64     // RISC-V instret-style retired instructions
	wfi               bool       // last retired instruction was WFI; consumed by BIOS machine loop
	lastTrapCause     uint64
	lastTrapInsnLen   uint8
	priv              PrivilegeMode
	mmu               *MMU
	Notes             NoteChain // exception delivery chain; handlers installed by OS layer
	// LR/SC reservation
	resvAddr  uint64
	resvValid bool
	// tohost watch: if non-zero, dispatch loops poll this guest address
	// for a non-zero value and exit when detected. Standard riscv-tests
	// exit mechanism: tohost==1 means PASS, other values mean FAIL.
	watchAddr uint64
	ExitCode  int // set by OS handler on guest exit; read after RunJIT/RunWithChain returns *ExitError
	// M-mode trap CSRs — minimal support for riscv-tests.
	// Privileged guests may trap through mtvec, including mtvec==0. Plain
	// process-mode user CPUs keep mtvec==0 as a host NoteChain escape hatch.
	mtvec      uint64 // 0x305: trap vector base address
	mscratch   uint64 // 0x340: machine scratch
	mepc       uint64 // 0x341: exception program counter
	mcause     uint64 // 0x342: trap cause code
	mstatus    uint64 // 0x300: machine status
	mtval      uint64 // 0x343: trap value
	satp       uint64 // 0x180: supervisor address translation/protection
	stvec      uint64 // 0x105: supervisor trap vector
	sscratch   uint64 // 0x140: supervisor scratch
	sepc       uint64 // 0x141: supervisor exception program counter
	scause     uint64 // 0x142: supervisor trap cause
	stval      uint64 // 0x143: supervisor trap value
	medeleg    uint64 // 0x302: machine exception delegation
	mideleg    uint64 // 0x303: machine interrupt delegation
	mie        uint64 // 0x304: machine interrupt enable
	mip        uint64 // 0x344: machine interrupt pending
	sie        uint64 // 0x104: supervisor interrupt enable
	sip        uint64 // 0x144: supervisor interrupt pending
	mcounteren uint64 // 0x306: machine counter enable
	scounteren uint64 // 0x106: supervisor counter enable
	menvcfg    uint64 // 0x30a: machine environment configuration
	mcountinh  uint64 // 0x320: machine counter inhibit
	stimecmp   uint64 // 0x14d: supervisor timer compare (Sstc)
	stip       bool   // STIP asserted by stimecmp.
	strictCSR  bool
}

// ── Performance footgun: don't call runCached from this file ─────────────
//
// The interpreter's hot path is the megaswitch inside runCached (see
// run_cached.go). It's a very large function and its generated machine code
// is unusually sensitive to compilation context.
//
// Specifically: when cpu.Run() (in this file) is written as a direct call
// like `return runCached(c, NewDecoderCache(…), &c.Notes)`, the Go compiler
// emits visibly worse code for runCached itself — ~25% slower on
// bench_guest (measured 419 MIPS → 314 MIPS). The slowdown is spread across
// the hottest RVC cases (C.ADDI / C.MV / C.ADD / C.BNEZ) and the top-level
// `switch slot.op` dispatch, a fingerprint consistent with altered register
// allocation in the megaswitch. The regression appears *even on benchmarks
// that never invoke cpu.Run()* (e.g. BenchmarkCPU_FullExecution_Cached,
// which calls riscv.runCached directly from the bench package) — runCached's
// machine code itself is what changes, not its call site.
//
// The root cause is likely Go's package-level inliner/cost-model treating
// runCached differently when it has an in-package caller. We haven't fully
// characterized it, and the threshold isn't documented anywhere in the Go
// toolchain.
//
// The workaround: cpu.Run() routes through RunDefault (defined in
// run_cached.go) instead of calling runCached directly. Keeping *all*
// runCached callsites confined to run_cached.go preserves the fast codegen.
//
// Rules to keep performance:
//   - Do NOT add `runCached(...)` call sites to cpu.go or any other file
//     in the riscv package outside run_cached.go.
//   - If you need a new cpu.go entry point that runs the guest, have it
//     call RunDefault (or another helper defined in run_cached.go).
//   - When in doubt, re-run:
// go test -bench='^BenchmarkCPU_FullExecution_Cached$' -benchtime=10s ./bench/
//     you want to target ≥ 400 MIPS on this host class.
//
// Adding fields to CPU is fine. An earlier investigation blamed a new
// `cache *DecoderCache` field; bisection proved the field is innocent and
// the direct in-file runCached call is the actual cause.

func NewCPU(mem GuestMemory) *CPU {
	return &CPU{
		mem:        mem,
		mstatus:    sanitizeMstatus(0, statusFS),
		mcounteren: counterCSRMask,
		scounteren: counterCSRMask,
		stimecmp:   ^uint64(0),
	}
}

func (c *CPU) fpEnabled() bool {
	return c.mstatus&statusFS != 0
}

func (c *CPU) requireFP() error {
	if !c.fpEnabled() {
		return ErrIllegalInstruction
	}
	return nil
}

func (c *CPU) markFPDirty() {
	c.mstatus = sanitizeMstatus(c.mstatus, c.mstatus|statusFS)
}

func (c *CPU) resolveRoundingMode(encoded uint8) (uint8, error) {
	rm := encoded
	if rm == rmDYN {
		rm = uint8((c.fcsr >> 5) & 0x7)
	}
	if rm > rmRMM {
		return 0, ErrIllegalInstruction
	}
	return rm, nil
}

func (c *CPU) withRoundingMode(rm uint8, fn func()) {
	if rm == rmRNE {
		fn()
		return
	}
	runtime.LockOSThread()
	old, ok := fenv.SetRoundingMode(rm)
	if ok {
		fn()
		fenv.RestoreRoundingMode(old)
	} else {
		fn()
	}
	runtime.UnlockOSThread()
}

func sanitizeMstatus(old, val uint64) uint64 {
	next := (old &^ (mstatusWritable | statusSD)) | (val & mstatusWritable)
	if next&statusMPP == uint64(2)<<11 {
		next &^= statusMPP
	}
	if next&statusFS == statusFS || next&statusXS == statusXS {
		next |= statusSD
	}
	return next
}

func csrRequiredPrivilege(addr uint32) PrivilegeMode {
	return PrivilegeMode((addr >> 8) & 3)
}

func csrIsReadOnly(addr uint32) bool {
	return addr&0xC00 == 0xC00
}

func csrIsCounter(addr uint32) bool {
	return addr == 0xC00 || addr == 0xC01 || addr == 0xC02
}

func csrCounterBit(addr uint32) uint64 {
	return uint64(1) << (addr & 31)
}

func (c *CPU) counterCSRAllowed(addr uint32) bool {
	bit := csrCounterBit(addr)
	if c.priv < PrivMachine && c.mcounteren&bit == 0 {
		return false
	}
	if c.priv < PrivSupervisor && c.scounteren&bit == 0 {
		return false
	}
	return true
}

func (c *CPU) checkCSRAccess(addr uint32, write bool) bool {
	if c.strictCSR {
		req := csrRequiredPrivilege(addr)
		if req == 2 || c.priv < req {
			return false
		}
		if write && csrIsReadOnly(addr) {
			return false
		}
	}
	switch addr {
	case 0x001, 0x002, 0x003:
		if !c.fpEnabled() {
			return false
		}
	}
	if c.strictCSR && csrIsCounter(addr) && !c.counterCSRAllowed(addr) {
		return false
	}
	return true
}

func (c *CPU) SetPC(addr uint64) { c.pc = addr }
func (c *CPU) PC() uint64        { return c.pc }
func (c *CPU) SetPrivilegeMode(mode PrivilegeMode) {
	c.priv = mode
}
func (c *CPU) PrivilegeMode() PrivilegeMode { return c.priv }
func (c *CPU) EnableStrictCSR()             { c.strictCSR = true }

// SetReg writes a GPR. Uses the "always write, always zero x[0]" trick to
// avoid a conditional branch. The trailing `c.x[0] = 0` preserves the RISC-V
// invariant that x0 reads as zero; when r != 0, it's a dead store the CPU
// store buffer absorbs cheaply.
func (c *CPU) SetReg(r uint8, v uint64) { c.x[r] = v; c.x[0] = 0 }

// Reg reads a GPR. Relies on the invariant that x[0] is always 0, maintained
// by SetReg above.
func (c *CPU) Reg(r uint8) uint64        { return c.x[r] }
func (c *CPU) SetFReg(r uint8, v uint64) { c.f[r] = v }
func (c *CPU) FReg(r uint8) uint64       { return c.f[r] }
func (c *CPU) FCSR() uint32              { return c.fcsr }
func (c *CPU) SetFCSR(v uint32)          { c.fcsr = v }
func (c *CPU) RiscvInstrBegun() uint64   { return c.riscvInstrBegun }
func (c *CPU) RiscvInstrRetired() uint64 { return c.riscvInstrRetired }
func (c *CPU) SetWatchAddr(addr uint64)  { c.watchAddr = addr }
func (c *CPU) WatchAddr() uint64         { return c.watchAddr }

type CPUDebugSnapshot struct {
	PC                uint64
	Privilege         PrivilegeMode
	MStatus           uint64
	MIE               uint64
	MIP               uint64
	RawMIP            uint64
	MEnvCfg           uint64
	MCountInhibit     uint64
	SIE               uint64
	SIP               uint64
	MEDeleg           uint64
	MIDeleg           uint64
	MTVec             uint64
	STVec             uint64
	MEPC              uint64
	SEPC              uint64
	MCause            uint64
	SCause            uint64
	MTVal             uint64
	STVal             uint64
	SATP              uint64
	STimecmp          uint64
	STimecmpPending   bool
	LastTrapCause     uint64
	LastTrapInsnLen   uint8
	RiscvInstrBegun   uint64
	RiscvInstrRetired uint64
}

func (c *CPU) DebugSnapshot() CPUDebugSnapshot {
	return CPUDebugSnapshot{
		PC:                c.pc,
		Privilege:         c.priv,
		MStatus:           c.mstatus,
		MIE:               c.mie,
		MIP:               c.mipValue(),
		RawMIP:            c.mip,
		MEnvCfg:           c.menvcfg,
		MCountInhibit:     c.mcountinh,
		SIE:               c.sie,
		SIP:               c.sip,
		MEDeleg:           c.medeleg,
		MIDeleg:           c.mideleg,
		MTVec:             c.mtvec,
		STVec:             c.stvec,
		MEPC:              c.mepc,
		SEPC:              c.sepc,
		MCause:            c.mcause,
		SCause:            c.scause,
		MTVal:             c.mtval,
		STVal:             c.stval,
		SATP:              c.satp,
		STimecmp:          c.stimecmp,
		STimecmpPending:   c.stip,
		LastTrapCause:     c.lastTrapCause,
		LastTrapInsnLen:   c.lastTrapInsnLen,
		RiscvInstrBegun:   c.riscvInstrBegun,
		RiscvInstrRetired: c.riscvInstrRetired,
	}
}

func (c *CPU) setTrap(cause uint64, insnLen uint8) {
	c.lastTrapCause = cause
	c.lastTrapInsnLen = insnLen
}

func (c *CPU) trapToPrivilegedAt(pc, cause, tval uint64, insnLen uint8) bool {
	c.setTrap(cause, insnLen)
	if c.shouldDelegateException(cause) {
		c.trapToSupervisorAt(pc, cause, tval)
		return true
	}
	return c.trapToMachineAt(pc, cause, tval)
}

func (c *CPU) shouldDelegateException(cause uint64) bool {
	return c.priv != PrivMachine && cause < 64 && (c.medeleg&(uint64(1)<<cause)) != 0
}

func (c *CPU) trapToSupervisorAt(pc, cause, tval uint64) {
	sie := (c.mstatus & statusSIE) != 0
	c.mstatus &^= statusSIE | statusSPIE | statusSPP
	if sie {
		c.mstatus |= statusSPIE
	}
	if c.priv == PrivSupervisor {
		c.mstatus |= statusSPP
	}
	c.sepc = pc
	c.scause = cause
	c.stval = tval
	c.pc = trapVectorPC(c.stvec, cause)
	c.priv = PrivSupervisor
}

func (c *CPU) trapToMachineAt(pc, cause, tval uint64) bool {
	if c.mtvec == 0 && !c.strictCSR {
		// Process-mode tests historically use mtvec==0 as "return the note to
		// the host"; privileged guests may legitimately vector traps to address 0.
		return false
	}
	mie := c.mstatus & statusMIE
	c.mstatus &^= statusMIE | statusMPIE | statusMPP
	if mie != 0 {
		c.mstatus |= statusMPIE
	}
	c.mstatus |= uint64(c.priv&3) << 11
	c.mepc = pc
	c.mcause = cause
	c.mtval = tval
	c.pc = trapVectorPC(c.mtvec, cause)
	c.priv = PrivMachine
	return true
}

func trapVectorPC(tvec, cause uint64) uint64 {
	base := tvec &^ 3
	if cause&InterruptCauseFlag == 0 || tvec&3 != 1 {
		return base
	}
	return base + 4*(cause&^InterruptCauseFlag)
}

func (c *CPU) ecallCause() uint64 {
	switch c.priv {
	case PrivSupervisor:
		return CauseEcallS
	case PrivMachine:
		return CauseEcallM
	default:
		return CauseEcallU
	}
}

func (c *CPU) setEbreakTrapAtPC() {
	insnLen := uint8(4)
	if half, f := c.fetch16(c.pc); f == nil && half == 0x9002 {
		insnLen = 2
	}
	c.setTrap(CauseBreakpoint, insnLen)
}

func (c *CPU) retireInsn() {
	c.riscvInstrRetired++
}

func (c *CPU) clearReservation() {
	c.resvAddr = 0
	c.resvValid = false
}

// Run executes the guest until an unhandled note or fatal exception.
// Exceptions are delivered through cpu.Notes; see NoteChain and RunWithChain.
//
// Dispatches through RunDefault (in run_cached.go) which auto-allocates a
// 256 KB decoder cache at the current PC and invokes runCached. This one
// level of indirection is load-bearing for performance — do NOT inline the
// runCached call here. See the long-form comment above CPU for why.
//
// Callers that want the uncached reference path call RunWithChain directly.
// Callers that want a persistent or custom-sized decoder cache call
// runCached(cpu, cache, &cpu.Notes) directly.
func (c *CPU) Run() error {
	return RunDefault(c, &c.Notes)
}

// Step executes a single instruction. Returns ErrEbreak, ErrEcall, or ErrIllegalInstruction on halt/fault.
func (c *CPU) Step() error { return c.step() }

func (c *CPU) step() error {
	c.wfi = false
	// Fetch 16 bits first to detect compressed (RVC) instructions.
	// Bits[1:0] != 0b11 means 16-bit; 0b11 means 32-bit.
	half, fh := c.fetch16(c.pc)
	if fh != nil {
		return fh
	}
	if half&0x3 != 0x3 {
		return c.stepRVC(uint16(half))
	}

	insn, f := c.fetch32(c.pc)
	if f != nil {
		if f.Kind == FaultMisalign {
			// Strict Spike-style behavior would return the misaligned fetch fault.
			// This emulator intentionally allows bytewise misaligned access paths;
			// several real tests depend on that permissiveness.
			insn, f = c.fetch32U(c.pc)
		}
		if f != nil {
			return f
		}
	}
	return c.stepFromInsn(insn)
}

// stepFromInsn executes one 32-bit RV64 instruction whose encoded bits have
// already been fetched into insn. Callers must verify the instruction is
// non-compressed (bits[1:0] == 0b11) before invoking.
//
// Used by runCached and other fast-path drivers to bypass the guest-memory
// fetch that step() would otherwise redo on every visit to a given PC.
func (c *CPU) stepFromInsn(insn uint32) error {
	c.wfi = false
	opcode := uint8(insn & 0x7F)
	rd := uint8((insn >> 7) & 0x1F)
	funct3 := uint8((insn >> 12) & 0x07)
	rs1 := uint8((insn >> 15) & 0x1F)
	rs2 := uint8((insn >> 20) & 0x1F)

	// I-type immediate: sign-extended bits [31:20]
	iimm := int64(int32(insn)) >> 20

	nextPC := c.pc + 4

	switch opcode {

	// ── LOAD (I-type) ────────────────────────────────────────────────────
	case 0x03:
		addr := c.Reg(rs1) + uint64(iimm)
		var v uint64
		// Strict Spike-style behavior would return FaultMisalign from the
		// aligned helpers below instead of retrying with the U bytewise helpers.
		// The permissive retry is intentional here; actual compatibility tests
		// rely on accepting misaligned scalar memory accesses.
		switch funct3 {
		case 0x0: // LB — sign-extend 8→64
			u, f := c.load8(addr)
			if f != nil {
				return f
			}
			v = uint64(int64(int8(u)))
		case 0x1: // LH — sign-extend 16→64 (misalign-capable)
			u, f := c.load16(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = c.load16U(addr)
			}
			if f != nil {
				return f
			}
			v = uint64(int64(int16(u)))
		case 0x2: // LW — sign-extend 32→64 (misalign-capable)
			u, f := c.load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = c.load32U(addr)
			}
			if f != nil {
				return f
			}
			v = uint64(int64(int32(u)))
		case 0x3: // LD — full 64-bit (misalign-capable)
			u, f := c.load64(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = c.load64U(addr)
			}
			if f != nil {
				return f
			}
			v = u
		case 0x4: // LBU — zero-extend 8→64
			u, f := c.load8(addr)
			if f != nil {
				return f
			}
			v = uint64(u)
		case 0x5: // LHU — zero-extend 16→64 (misalign-capable)
			u, f := c.load16(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = c.load16U(addr)
			}
			if f != nil {
				return f
			}
			v = uint64(u)
		case 0x6: // LWU — zero-extend 32→64 (misalign-capable)
			u, f := c.load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = c.load32U(addr)
			}
			if f != nil {
				return f
			}
			v = uint64(u)
		default:
			return ErrIllegalInstruction
		}
		c.SetReg(rd, v)

	// ── STORE (S-type) ───────────────────────────────────────────────────
	case 0x23:
		simm := int64(int32(insn&0xFE000000)>>20) | int64((insn>>7)&0x1F)
		addr := c.Reg(rs1) + uint64(simm)
		// Strict correctness would propagate FaultMisalign from store16/32/64.
		// We keep the bytewise fallback for the same compatibility reason as
		// the load path above.
		switch funct3 {
		case 0x0: // SB
			if f := c.store8(addr, uint8(c.Reg(rs2))); f != nil {
				return f
			}
		case 0x1: // SH (misalign-capable)
			f := c.store16(addr, uint16(c.Reg(rs2)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store16U(addr, uint16(c.Reg(rs2)))
			}
			if f != nil {
				return f
			}
		case 0x2: // SW (misalign-capable)
			f := c.store32(addr, uint32(c.Reg(rs2)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store32U(addr, uint32(c.Reg(rs2)))
			}
			if f != nil {
				return f
			}
		case 0x3: // SD (misalign-capable)
			f := c.store64(addr, c.Reg(rs2))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store64U(addr, c.Reg(rs2))
			}
			if f != nil {
				return f
			}
		default:
			return ErrIllegalInstruction
		}

		// ── OP-IMM (I-type) ──────────────────────────────────────────────────
	case 0x13: // ── OP-IMM ─────────────────────────────────────────────────
		imm12 := (insn >> 20) & 0xFFF
		funct6i := imm12 >> 6
		shamt := uint8(imm12 & 0x3F)
		a := c.Reg(rs1)
		var v uint64
		switch funct3 {
		case 0x0:
			v = a + uint64(iimm) // ADDI
		case 0x1: // SLLI / Zbs BSETI/BCLRI/BINVI / Zbb CLZ/CTZ/CPOP/SEXT
			switch funct6i {
			case 0x00:
				v = a << shamt // SLLI (shamt[5] may be 0 or 1)
			case 0x0A:
				v = a | (1 << uint(shamt)) // BSETI
			case 0x12:
				v = a &^ (1 << uint(shamt)) // BCLRI
			case 0x1A:
				v = a ^ (1 << uint(shamt)) // BINVI
			case 0x18: // CLZ/CTZ/CPOP/SEXT.B/SEXT.H
				switch imm12 {
				case 0x600:
					v = uint64(bits.LeadingZeros64(a)) // CLZ
				case 0x601:
					v = uint64(bits.TrailingZeros64(a)) // CTZ
				case 0x602:
					v = uint64(bits.OnesCount64(a)) // CPOP
				case 0x604:
					v = uint64(int64(int8(a))) // SEXT.B
				case 0x605:
					v = uint64(int64(int16(a))) // SEXT.H
				default:
					return ErrIllegalInstruction
				}
			default:
				return ErrIllegalInstruction
			}
		case 0x2:
			if int64(a) < iimm {
				v = 1
			} // SLTI
		case 0x3:
			if a < uint64(iimm) {
				v = 1
			} // SLTIU
		case 0x4:
			v = a ^ uint64(iimm) // XORI
		case 0x5: // SRLI/SRAI / Zbs BEXTI / Zbb RORI/ORC.B/REV8
			switch {
			case funct6i == 0x00:
				v = a >> shamt // SRLI
			case funct6i == 0x10:
				v = uint64(int64(a) >> shamt) // SRAI
			case funct6i == 0x12:
				v = (a >> uint(shamt)) & 1 // BEXTI
			case funct6i == 0x18:
				v = uint64(bits.RotateLeft64(a, -int(shamt))) // RORI
			case imm12 == 0x287:
				v = orcB(a) // ORC.B
			case imm12 == 0x6B8:
				v = rev8(a) // REV8
			default:
				return ErrIllegalInstruction
			}
		case 0x6:
			v = a | uint64(iimm) // ORI
		case 0x7:
			v = a & uint64(iimm) // ANDI
		}
		c.SetReg(rd, v)

		// ── OP-IMM-32 (I-type, 32-bit ops, sign-extend result) ───────────────
	case 0x1B: // ── OP-IMM-32 ───────────────────────────────────────────────
		imm12 := (insn >> 20) & 0xFFF
		funct7i32 := insn >> 25
		funct6i32 := imm12 >> 6
		shamt := uint8(imm12 & 0x1F)
		shamt6 := uint8(imm12 & 0x3F)
		var v int32
		switch funct3 {
		case 0x0: // ADDIW
			v = int32(c.Reg(rs1)) + int32(iimm)
		case 0x1: // SLLIW / Zba SLLI.UW / Zbb CLZW/CTZW/CPOPW
			switch {
			case funct7i32 == 0x00:
				v = int32(uint32(c.Reg(rs1)) << shamt)
			case funct6i32 == 0x02: // SLLI.UW: rd = uint64(uint32(rs1)) << shamt[5:0]
				c.SetReg(rd, uint64(uint32(c.Reg(rs1)))<<uint(shamt6))
				c.pc = nextPC
				c.retireInsn()
				return nil
			case imm12 == 0x600:
				v = int32(bits.LeadingZeros32(uint32(c.Reg(rs1)))) // CLZW
			case imm12 == 0x601:
				v = int32(bits.TrailingZeros32(uint32(c.Reg(rs1)))) // CTZW
			case imm12 == 0x602:
				v = int32(bits.OnesCount32(uint32(c.Reg(rs1)))) // CPOPW
			default:
				return ErrIllegalInstruction
			}
		case 0x5: // SRLIW / SRAIW / RORIW
			switch funct7i32 {
			case 0x00:
				v = int32(uint32(c.Reg(rs1)) >> shamt) // SRLIW
			case 0x20:
				v = int32(c.Reg(rs1)) >> shamt // SRAIW
			case 0x30: // RORIW: rotate right word immediate
				v = int32(bits.RotateLeft32(uint32(c.Reg(rs1)), -int(shamt)))
			default:
				return ErrIllegalInstruction
			}
		default:
			return ErrIllegalInstruction
		}
		c.SetReg(rd, uint64(int64(v)))

	// ── OP (R-type) ──────────────────────────────────────────────────────
	case 0x33:
		funct7 := insn >> 25
		a, b := c.Reg(rs1), c.Reg(rs2)
		var v uint64
		switch funct7 {
		case 0x01: // ── RV64M ───────────────────────────────────────────────
			switch funct3 {
			case 0x0:
				v = a * b
			case 0x1:
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 {
					hi -= b
				}
				if int64(b) < 0 {
					hi -= a
				}
				v = hi
			case 0x2:
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 {
					hi -= b
				}
				v = hi
			case 0x3:
				hi, _ := bits.Mul64(a, b)
				v = hi
			case 0x4:
				if b == 0 {
					v = ^uint64(0)
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					v = a
				} else {
					v = uint64(int64(a) / int64(b))
				}
			case 0x5:
				if b == 0 {
					v = ^uint64(0)
				} else {
					v = a / b
				}
			case 0x6:
				if b == 0 {
					v = a
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					v = 0
				} else {
					v = uint64(int64(a) % int64(b))
				}
			case 0x7:
				if b == 0 {
					v = a
				} else {
					v = a % b
				}
			}
			// ── Zbb/Zbs/Zba (OP) ────────────────────────────────────────────────
		case 0x04: // RV32-style PACK/ZEXT.H encoding; RV64 Zbb uses PACKW below.
			if funct3 != 4 || rs2 != 0 {
				return ErrIllegalInstruction
			}
			v = a & 0xFFFF
		case 0x05: // Zbb: MIN/MAX + Zbc: CLMUL/CLMULR/CLMULH
			switch funct3 {
			case 1: // CLMUL
				var r uint64
				for i := 0; i < 64; i++ {
					if (b>>i)&1 != 0 {
						r ^= a << i
					}
				}
				v = r
			case 2: // CLMULR
				var r uint64
				for i := 0; i < 64; i++ {
					if (b>>i)&1 != 0 {
						r ^= a >> (63 - i)
					}
				}
				v = r
			case 3: // CLMULH
				var r uint64
				for i := 1; i < 64; i++ {
					if (b>>i)&1 != 0 {
						r ^= a >> (64 - i)
					}
				}
				v = r
			case 4:
				if int64(a) < int64(b) {
					v = a
				} else {
					v = b
				} // MIN
			case 5:
				if a < b {
					v = a
				} else {
					v = b
				} // MINU
			case 6:
				if int64(a) > int64(b) {
					v = a
				} else {
					v = b
				} // MAX
			case 7:
				if a > b {
					v = a
				} else {
					v = b
				} // MAXU
			default:
				return ErrIllegalInstruction
			}
		case 0x07: // Zicond: CZERO.EQZ / CZERO.NEZ
			switch funct3 {
			case 5:
				if b == 0 {
					v = 0
				} else {
					v = a
				} // CZERO.EQZ
			case 7:
				if b != 0 {
					v = 0
				} else {
					v = a
				} // CZERO.NEZ
			default:
				return ErrIllegalInstruction
			}
		case 0x10: // Zba: SH1ADD/SH2ADD/SH3ADD
			switch funct3 {
			case 2:
				v = b + (a << 1)
			case 4:
				v = b + (a << 2)
			case 6:
				v = b + (a << 3)
			default:
				return ErrIllegalInstruction
			}
		case 0x14: // Zbs: BSET
			switch funct3 {
			case 1:
				v = a | (1 << (b & 63)) // BSET
			default:
				return ErrIllegalInstruction
			}
		case 0x20: // RV64I SUB/SRA + Zbb ANDN/ORN/XNOR
			switch funct3 {
			case 0:
				v = a - b // SUB
			case 4:
				v = a ^ ^b // XNOR
			case 5:
				v = uint64(int64(a) >> (b & 0x3F)) // SRA
			case 6:
				v = a | ^b // ORN
			case 7:
				v = a & ^b // ANDN
			default:
				return ErrIllegalInstruction
			}
		case 0x24: // Zbs: BCLR/BEXT
			switch funct3 {
			case 1:
				v = a &^ (1 << (b & 63)) // BCLR
			case 5:
				v = (a >> (b & 63)) & 1 // BEXT
			default:
				return ErrIllegalInstruction
			}
		case 0x30: // Zbb: ROL/ROR
			switch funct3 {
			case 1: // ROL
				v = uint64(bits.RotateLeft64(a, int(b&63)))
			case 5: // ROR
				v = uint64(bits.RotateLeft64(a, -int(b&63)))
			default:
				return ErrIllegalInstruction
			}
		case 0x34: // Zbs: BINV
			if funct3 != 1 {
				return ErrIllegalInstruction
			}
			v = a ^ (1 << (b & 63))
		default: // ── RV64I ─────────────────────────────────────────────────
			if funct7 != 0x00 {
				return ErrIllegalInstruction
			}
			switch funct3 {
			case 0x0:
				v = a + b // ADD
			case 0x1:
				v = a << (b & 0x3F) // SLL
			case 0x2:
				if int64(a) < int64(b) {
					v = 1
				} // SLT
			case 0x3:
				if a < b {
					v = 1
				} // SLTU
			case 0x4:
				v = a ^ b // XOR
			case 0x5:
				v = a >> (b & 0x3F) // SRL
			case 0x6:
				v = a | b // OR
			case 0x7:
				v = a & b // AND
			default:
				return ErrIllegalInstruction
			}
		}
		c.SetReg(rd, v)

	// ── OP-32 (R-type, 32-bit, sign-extend) ─────────────────────────────
	case 0x3B: // ── OP-32 ────────────────────────────────────────────────────
		funct7 := insn >> 25
		a32, b32 := uint32(c.Reg(rs1)), uint32(c.Reg(rs2))
		var v int32
		switch funct7 {
		case 0x01: // ── RV64M word ops ─────────────────────────────────
			switch funct3 {
			case 0x0:
				v = int32(a32 * b32)
			case 0x4:
				if b32 == 0 {
					v = -1
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					v = int32(a32)
				} else {
					v = int32(a32) / int32(b32)
				}
			case 0x5:
				if b32 == 0 {
					v = -1
				} else {
					v = int32(a32 / b32)
				}
			case 0x6:
				if b32 == 0 {
					v = int32(a32)
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					v = 0
				} else {
					v = int32(a32) % int32(b32)
				}
			case 0x7:
				if b32 == 0 {
					v = int32(a32)
				} else {
					v = int32(a32 % b32)
				}
			default:
				return ErrIllegalInstruction
			}
		case 0x04:
			switch funct3 {
			case 0: // Zba: ADD.UW  rd = rs2 + uint64(uint32(rs1))
				c.SetReg(rd, c.Reg(rs2)+uint64(uint32(c.Reg(rs1))))
			case 4: // Zbb: ZEXT.H (RV64 canonical PACKW rd, rs1, x0)
				if rs2 != 0 {
					return ErrIllegalInstruction
				}
				c.SetReg(rd, uint64(uint16(c.Reg(rs1))))
			default:
				return ErrIllegalInstruction
			}
			c.pc = nextPC
			c.retireInsn()
			return nil
		case 0x10: // Zba: SH1ADD.UW / SH2ADD.UW / SH3ADD.UW
			zext := uint64(uint32(c.Reg(rs1)))
			base := c.Reg(rs2)
			switch funct3 {
			case 2:
				c.SetReg(rd, base+(zext<<1))
			case 4:
				c.SetReg(rd, base+(zext<<2))
			case 6:
				c.SetReg(rd, base+(zext<<3))
			default:
				return ErrIllegalInstruction
			}
			c.pc = nextPC
			c.retireInsn()
			return nil
		case 0x30: // Zbb: ROLW / RORW
			sh := b32 & 0x1F
			switch funct3 {
			case 1:
				v = int32(bits.RotateLeft32(a32, int(sh)))
			case 5:
				v = int32(bits.RotateLeft32(a32, -int(sh)))
			default:
				return ErrIllegalInstruction
			}
		default: // ── RV64I word ops ─────────────────────────────────────
			switch funct3 {
			case 0x0:
				if funct7 == 0x20 {
					v = int32(a32 - b32)
				} else if funct7 == 0x00 {
					v = int32(a32 + b32)
				} else {
					return ErrIllegalInstruction
				}
			case 0x1:
				if funct7 != 0x00 {
					return ErrIllegalInstruction
				}
				v = int32(a32 << (b32 & 0x1F))
			case 0x5:
				if funct7 == 0x20 {
					v = int32(a32) >> (b32 & 0x1F)
				} else if funct7 == 0x00 {
					v = int32(a32 >> (b32 & 0x1F))
				} else {
					return ErrIllegalInstruction
				}
			default:
				return ErrIllegalInstruction
			}
		}
		c.SetReg(rd, uint64(int64(v)))

	// ── AMO — RV64A atomic memory operations ─────────────────────────────
	case 0x2F:
		funct5 := insn >> 27
		width := funct3 // 010=W, 011=D
		addr := c.Reg(rs1)

		switch funct5 {
		case 0b00010: // LR.W / LR.D
			var v uint64
			if width == 0b010 {
				u, f := c.load32(addr)
				if f != nil {
					return f
				}
				v = uint64(int64(int32(u)))
			} else {
				u, f := c.load64(addr)
				if f != nil {
					return f
				}
				v = u
			}
			c.SetReg(rd, v)
			c.resvAddr = addr
			c.resvValid = true

		case 0b00011: // SC.W / SC.D
			success := c.resvValid && c.resvAddr == addr
			if success {
				if width == 0b010 {
					if f := c.store32(addr, uint32(c.Reg(rs2))); f != nil {
						return f
					}
				} else {
					if f := c.store64(addr, c.Reg(rs2)); f != nil {
						return f
					}
				}
				c.SetReg(rd, 0) // success
			} else {
				c.SetReg(rd, 1) // failure
			}
			c.resvValid = false

		default: // AMO ops: rd=mem[rs1]; mem[rs1]=op(rd,rs2); advance PC
			if width == 0b010 { // .W — 32-bit
				old, f := c.load32(addr)
				if f != nil {
					return f
				}
				oldSE := uint64(int64(int32(old))) // sign-extended for rd
				newVal := amoOpW(funct5, old, uint32(c.Reg(rs2)))
				if f := c.store32(addr, newVal); f != nil {
					return f
				}
				c.SetReg(rd, oldSE)
			} else { // .D — 64-bit
				old, f := c.load64(addr)
				if f != nil {
					return f
				}
				newVal := amoOpD(funct5, old, c.Reg(rs2))
				if f := c.store64(addr, newVal); f != nil {
					return f
				}
				c.SetReg(rd, old)
			}
			c.resvValid = false // any AMO invalidates reservation
		}

	// ── FENCE / FENCE.I (no-op for single-threaded emulator) ────────────
	case 0x0F:
		// FENCE and FENCE.I are memory/instruction-cache ordering barriers.
		// In our single-threaded emulator with no instruction cache,
		// both are safe no-ops. We just advance PC.

	// ── LUI (U-type) ─────────────────────────────────────────────────────
	case 0x37:
		c.SetReg(rd, uint64(int64(int32(insn&0xFFFFF000))))

	// ── AUIPC (U-type) ───────────────────────────────────────────────────
	case 0x17:
		c.SetReg(rd, c.pc+uint64(int64(int32(insn&0xFFFFF000))))

		// ── JAL (J-type) ─────────────────────────────────────────────────────
	case 0x6F:
		// Reconstruct J-type immediate (21 bits, bit 0 always 0).
		// Shift left 11 so the sign bit lands at bit 31 of int32,
		// then arithmetic-right-shift 11 to sign-extend to 64 bits.
		raw := ((insn>>31)&1)<<20 |
			((insn>>12)&0xFF)<<12 |
			((insn>>20)&1)<<11 |
			((insn>>21)&0x3FF)<<1
		jimm := int64(int32(raw<<11)) >> 11 // sign-extend 21→64
		c.SetReg(rd, uint64(nextPC))
		c.pc = c.pc + uint64(jimm)
		c.retireInsn()
		return nil

	// ── JALR (I-type) ────────────────────────────────────────────────────
	case 0x67:
		target := (c.Reg(rs1) + uint64(iimm)) &^ 1
		c.SetReg(rd, uint64(nextPC))
		c.pc = target
		c.retireInsn()
		return nil

	// ── BRANCH (B-type) ──────────────────────────────────────────────────
	case 0x63:
		bimm := int64(int32(
			((insn>>31)&1)<<20|
				((insn>>7)&1)<<19|
				((insn>>25)&0x3F)<<13|
				((insn>>8)&0xF)<<9)) >> 19 // sign-extend 13→64, still need >>8 more
		// Simpler: reconstruct as 13-bit then sign-extend
		uimm := ((insn>>31)&1)<<12 |
			((insn>>7)&1)<<11 |
			((insn>>25)&0x3F)<<5 |
			((insn>>8)&0xF)<<1
		bimm = int64(int32(uimm<<19)) >> 19
		a, b := c.Reg(rs1), c.Reg(rs2)
		var taken bool
		switch funct3 {
		case 0x0:
			taken = a == b // BEQ
		case 0x1:
			taken = a != b // BNE
		case 0x4:
			taken = int64(a) < int64(b) // BLT
		case 0x5:
			taken = int64(a) >= int64(b) // BGE
		case 0x6:
			taken = a < b // BLTU
		case 0x7:
			taken = a >= b // BGEU
		default:
			return ErrIllegalInstruction
		}
		if taken {
			c.pc = c.pc + uint64(bimm)
			c.retireInsn()
			return nil
		}

		// ── SYSTEM ───────────────────────────────────────────────────────────
	case 0x73: // ── SYSTEM ───────────────────────────────────────────────────
		csrAddr := insn >> 20
		switch {
		case insn == 0x00100073: // EBREAK
			if c.priv != PrivUser && c.trapToPrivilegedAt(c.pc, CauseBreakpoint, 0, 4) {
				return nil
			}
			c.setTrap(CauseBreakpoint, 4)
			return ErrEbreak
		case insn == 0x00000073: // ECALL
			if c.trapToPrivilegedAt(c.pc, c.ecallCause(), 0, 4) {
				return nil
			}
			return ErrEcall
		case insn == 0x30200073: // MRET
			if c.strictCSR && c.priv != PrivMachine {
				return ErrIllegalInstruction
			}
			nextPriv := PrivilegeMode((c.mstatus >> 11) & 3)
			if nextPriv == 2 {
				nextPriv = PrivUser
			}
			mpie := (c.mstatus >> 7) & 1
			c.mstatus &^= (uint64(1) << 3) | (uint64(3) << 11)
			c.mstatus |= mpie << 3
			c.mstatus |= uint64(1) << 7
			if nextPriv != PrivMachine {
				c.mstatus &^= statusMPRV
			}
			c.mstatus = sanitizeMstatus(c.mstatus, c.mstatus)
			c.priv = nextPriv
			c.pc = c.mepc
			c.retireInsn()
			return nil
		case insn == 0x10200073: // SRET
			if c.strictCSR && (c.priv == PrivUser || (c.priv < PrivMachine && c.mstatus&statusTSR != 0)) {
				return ErrIllegalInstruction
			}
			nextPriv := PrivUser
			if c.mstatus&statusSPP != 0 {
				nextPriv = PrivSupervisor
			}
			spie := c.mstatus & statusSPIE
			c.mstatus &^= statusSIE | statusSPIE | statusSPP
			if spie != 0 {
				c.mstatus |= statusSIE
			}
			c.mstatus |= statusSPIE
			c.mstatus &^= statusMPRV
			c.mstatus = sanitizeMstatus(c.mstatus, c.mstatus)
			c.priv = nextPriv
			c.pc = c.sepc
			c.retireInsn()
			return nil
		case insn == 0x10500073: // WFI — no-op in user-mode emulation
			if c.strictCSR && (c.priv == PrivUser || (c.priv < PrivMachine && c.mstatus&statusTW != 0)) {
				return ErrIllegalInstruction
			}
			c.wfi = true
		case funct3 == 0 && insn>>25 == 0x09: // SFENCE.VMA
			if c.strictCSR && (c.priv == PrivUser || (c.priv < PrivMachine && c.mstatus&statusTVM != 0)) {
				return ErrIllegalInstruction
			}
			c.flushTLB()
		case funct3 >= 1 && funct3 <= 7 && funct3 != 4: // Zicsr
			var src uint64
			if funct3 >= 5 { // immediate forms (funct3=5/6/7)
				src = uint64(rs1) // rs1 field is the uimm5
			} else {
				src = c.Reg(rs1)
			}
			write := funct3 == 1 || funct3 == 5 || rs1 != 0
			if !c.checkCSRAccess(csrAddr, write) {
				return ErrIllegalInstruction
			}
			var old uint64
			if rd != 0 || funct3 != 1 && funct3 != 5 {
				var ok bool
				old, ok = c.readCSR(csrAddr)
				if !ok {
					return ErrIllegalInstruction
				}
			}
			var newVal uint64
			switch funct3 {
			case 1, 5:
				newVal = src // CSRRW/CSRRWI: write src
			case 2, 6:
				newVal = old | src // CSRRS/CSRRSI: set bits
			case 3, 7:
				newVal = old &^ src // CSRRC/CSRRCI: clear bits
			}
			if write {
				if !c.writeCSR(csrAddr, newVal) {
					return ErrIllegalInstruction
				}
			}
			c.SetReg(rd, old)
		default:
			return ErrIllegalInstruction
		}

	// ── FLW / FLD — float loads ──────────────────────────────────────────
	case 0x07:
		if err := c.requireFP(); err != nil {
			return err
		}
		addr := uint64(int64(c.Reg(rs1)) + iimm)
		// Strict Spike-style behavior would propagate FaultMisalign from the
		// aligned helpers. We intentionally preserve bytewise misaligned FP
		// loads for compatibility with existing guests/tests.
		switch funct3 {
		case 0b010: // FLW
			v, f := c.load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load32U(addr)
			}
			if f != nil {
				return f
			}
			c.SetFReg(rd, boxF32(v))
		case 0b011: // FLD
			v, f := c.load64(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load64U(addr)
			}
			if f != nil {
				return f
			}
			c.SetFReg(rd, boxF64(v))
		default:
			return ErrIllegalInstruction
		}
		c.markFPDirty()

	// ── FSW / FSD — float stores ──────────────────────────────────────────
	case 0x27:
		if err := c.requireFP(); err != nil {
			return err
		}
		simm := int64(int32(insn&0xFE000000)>>20) | int64((insn>>7)&0x1F)
		addr := uint64(int64(c.Reg(rs1)) + simm)
		// Strict Spike-style behavior would trap on FaultMisalign here. The
		// permissive bytewise retry is intentional, matching scalar stores.
		switch funct3 {
		case 0b010: // FSW
			f := c.store32(addr, uint32(c.FReg(rs2)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store32U(addr, uint32(c.FReg(rs2)))
			}
			if f != nil {
				return f
			} // FSW: raw low 32 bits
		case 0b011: // FSD
			f := c.store64(addr, c.FReg(rs2))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store64U(addr, c.FReg(rs2))
			}
			if f != nil {
				return f
			}
		default:
			return ErrIllegalInstruction
		}

	// ── FMADD/FMSUB/FNMSUB/FNMADD — fused multiply-add (R4-type) ─────────
	case 0x43, 0x47, 0x4B, 0x4F:
		if err := c.requireFP(); err != nil {
			return err
		}
		rm, err := c.resolveRoundingMode(funct3)
		if err != nil {
			return err
		}
		rs3 := uint8(insn >> 27)
		fmt := uint8((insn >> 25) & 0x3)
		if fmt == 0 { // .S single-precision
			a := f32frombits(unboxF32(c.FReg(rs1)))
			b := f32frombits(unboxF32(c.FReg(rs2)))
			d := f32frombits(unboxF32(c.FReg(rs3)))
			var v float32
			var fl uint32
			c.withRoundingMode(rm, func() {
				switch opcode {
				case 0x43:
					v, fl = fenv.MAddF32(a, b, d)
				case 0x47:
					v, fl = fenv.MSubF32(a, b, d)
				case 0x4B:
					v, fl = fenv.NMSubF32(a, b, d)
				case 0x4F:
					v, fl = fenv.NMAddF32(a, b, d)
				}
			})
			c.fcsr |= fl
			c.SetFReg(rd, boxF32(canonNaN32(f32bits(v))))
		} else if fmt == 1 { // .D double-precision
			a := f64frombits(c.FReg(rs1))
			b := f64frombits(c.FReg(rs2))
			d := f64frombits(c.FReg(rs3))
			var v float64
			var fl uint32
			c.withRoundingMode(rm, func() {
				switch opcode {
				case 0x43:
					v, fl = fenv.MAddF64(a, b, d)
				case 0x47:
					v, fl = fenv.MSubF64(a, b, d)
				case 0x4B:
					v, fl = fenv.NMSubF64(a, b, d)
				case 0x4F:
					v, fl = fenv.NMAddF64(a, b, d)
				}
			})
			c.fcsr |= fl
			c.SetFReg(rd, boxF64(canonNaN64(f64bits(v))))
		} else {
			return ErrIllegalInstruction
		}
		c.markFPDirty()

	// ── FPFUNC — all other float ops (opcode=0x53) ────────────────────────
	case 0x53:
		if err := c.requireFP(); err != nil {
			return err
		}
		funct5 := uint8(insn >> 27)
		fmt := uint8((insn >> 25) & 0x3)
		if fmt == 0 { // ── single-precision ────────────────────────────────
			a := unboxF32(c.FReg(rs1))
			b := unboxF32(c.FReg(rs2))
			af, bf := f32frombits(a), f32frombits(b)
			switch funct5 {
			case 0x00:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r32 float32
				var fl uint32
				c.withRoundingMode(rm, func() { r32, fl = fenv.AddF32(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x01:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r32 float32
				var fl uint32
				c.withRoundingMode(rm, func() { r32, fl = fenv.SubF32(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x02:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r32 float32
				var fl uint32
				c.withRoundingMode(rm, func() { r32, fl = fenv.MulF32(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x03:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r32 float32
				var fl uint32
				c.withRoundingMode(rm, func() { r32, fl = fenv.DivF32(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x0B:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r32 float32
				var fl uint32
				c.withRoundingMode(rm, func() { r32, fl = fenv.SqrtF32(af) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x04: // FSGNJ.S / FSGNJN.S / FSGNJX.S
				switch funct3 {
				case 0:
					c.SetFReg(rd, boxF32(fsgnjF32(a, b)))
				case 1:
					c.SetFReg(rd, boxF32(fsgnjnF32(a, b)))
				case 2:
					c.SetFReg(rd, boxF32(fsgnjxF32(a, b)))
				default:
					return ErrIllegalInstruction
				}
			case 0x05: // FMIN.S / FMAX.S
				if isSNaNF32(a) || isSNaNF32(b) {
					c.fcsr |= fflagNV
				}
				switch funct3 {
				case 0:
					c.SetFReg(rd, boxF32(fminF32(a, b)))
				case 1:
					c.SetFReg(rd, boxF32(fmaxF32(a, b)))
				default:
					return ErrIllegalInstruction
				}
			case 0x08: // FCVT.S.D  (rs2=1 = from D)
				if rs2 != 1 {
					return ErrIllegalInstruction
				}
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				src := c.FReg(rs1)
				var r float32
				fenv.ClearFFlags()
				c.withRoundingMode(rm, func() { r = float32(f64frombits(src)) })
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r))))
			case 0x14: // FEQ.S / FLT.S / FLE.S -> integer rd
				var v uint64
				switch funct3 {
				case 2: // FEQ.S
					if af == bf {
						v = 1
					}
					if isSNaNF32(a) || isSNaNF32(b) {
						c.fcsr |= fflagNV
					}
				case 1: // FLT.S
					if af < bf {
						v = 1
					}
					if isNaNF32(a) || isNaNF32(b) {
						c.fcsr |= fflagNV
					}
				case 0: // FLE.S
					if af <= bf {
						v = 1
					}
					if isNaNF32(a) || isNaNF32(b) {
						c.fcsr |= fflagNV
					}
				default:
					return ErrIllegalInstruction
				}
				c.SetReg(rd, v)
			case 0x18: // FCVT.{W,WU,L,LU}.S -> integer rd
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				switch rs2 {
				case 0:
					v, fl := fcvtWS(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				case 1:
					v, fl := fcvtWUS(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				case 2:
					v, fl := fcvtLS(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				case 3:
					v, fl := fcvtLUS(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				default:
					return ErrIllegalInstruction
				}
			case 0x1A: // FCVT.S.{W,WU,L,LU} <- integer rs1
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r float32
				fenv.ClearFFlags()
				c.withRoundingMode(rm, func() {
					switch rs2 {
					case 0:
						r = float32(int32(c.Reg(rs1))) // FCVT.S.W
					case 1:
						r = float32(uint32(c.Reg(rs1))) // FCVT.S.WU
					case 2:
						r = float32(int64(c.Reg(rs1))) // FCVT.S.L
					case 3:
						r = float32(c.Reg(rs1)) // FCVT.S.LU
					}
				})
				if rs2 > 3 {
					return ErrIllegalInstruction
				}
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF32(f32bits(r)))
			case 0x1C: // FMV.X.W (funct3=0) / FCLASS.S (funct3=1)
				switch funct3 {
				case 0:
					c.SetReg(rd, uint64(int64(int32(uint32(c.FReg(rs1)))))) // FMV.X.W raw bits
				case 1:
					c.SetReg(rd, fclassF32(a)) // FCLASS.S
				default:
					return ErrIllegalInstruction
				}
			case 0x1E: // FMV.W.X
				c.SetFReg(rd, boxF32(uint32(c.Reg(rs1))))
			default:
				return ErrIllegalInstruction
			}
		} else if fmt == 1 { // ── double-precision ────────────────────────
			a := c.FReg(rs1)
			b := c.FReg(rs2)
			af, bf := f64frombits(a), f64frombits(b)
			switch funct5 {
			case 0x00:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r64 float64
				var fl uint32
				c.withRoundingMode(rm, func() { r64, fl = fenv.AddF64(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x01:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r64 float64
				var fl uint32
				c.withRoundingMode(rm, func() { r64, fl = fenv.SubF64(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x02:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r64 float64
				var fl uint32
				c.withRoundingMode(rm, func() { r64, fl = fenv.MulF64(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x03:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r64 float64
				var fl uint32
				c.withRoundingMode(rm, func() { r64, fl = fenv.DivF64(af, bf) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x0B:
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r64 float64
				var fl uint32
				c.withRoundingMode(rm, func() { r64, fl = fenv.SqrtF64(af) })
				c.fcsr |= fl
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x04: // FSGNJ.D / FSGNJN.D / FSGNJX.D
				switch funct3 {
				case 0:
					c.SetFReg(rd, boxF64(fsgnjF64(a, b)))
				case 1:
					c.SetFReg(rd, boxF64(fsgnjnF64(a, b)))
				case 2:
					c.SetFReg(rd, boxF64(fsgnjxF64(a, b)))
				default:
					return ErrIllegalInstruction
				}
			case 0x05: // FMIN.D / FMAX.D
				if isSNaNF64(a) || isSNaNF64(b) {
					c.fcsr |= fflagNV
				}
				switch funct3 {
				case 0:
					c.SetFReg(rd, boxF64(fminF64(a, b)))
				case 1:
					c.SetFReg(rd, boxF64(fmaxF64(a, b)))
				default:
					return ErrIllegalInstruction
				}
			case 0x08: // FCVT.D.S  (rs2=0 = from S)
				if rs2 != 0 {
					return ErrIllegalInstruction
				}
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				src := unboxF32(c.FReg(rs1))
				if isSNaNF32(src) {
					c.fcsr |= fflagNV
				}
				var r float64
				fenv.ClearFFlags()
				c.withRoundingMode(rm, func() { r = float64(f32frombits(src)) })
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r))))
			case 0x14: // FEQ.D / FLT.D / FLE.D
				var v uint64
				switch funct3 {
				case 2: // FEQ.D
					if af == bf {
						v = 1
					}
					if isSNaNF64(a) || isSNaNF64(b) {
						c.fcsr |= fflagNV
					}
				case 1: // FLT.D
					if af < bf {
						v = 1
					}
					if isNaNF64(a) || isNaNF64(b) {
						c.fcsr |= fflagNV
					}
				case 0: // FLE.D
					if af <= bf {
						v = 1
					}
					if isNaNF64(a) || isNaNF64(b) {
						c.fcsr |= fflagNV
					}
				default:
					return ErrIllegalInstruction
				}
				c.SetReg(rd, v)
			case 0x18: // FCVT.{W,WU,L,LU}.D
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				switch rs2 {
				case 0:
					v, fl := fcvtWD(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				case 1:
					v, fl := fcvtWUD(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				case 2:
					v, fl := fcvtLD(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				case 3:
					v, fl := fcvtLUD(af, rm)
					c.fcsr |= fl
					c.SetReg(rd, v)
				default:
					return ErrIllegalInstruction
				}
			case 0x1A: // FCVT.D.{W,WU,L,LU}
				rm, err := c.resolveRoundingMode(funct3)
				if err != nil {
					return err
				}
				var r float64
				fenv.ClearFFlags()
				c.withRoundingMode(rm, func() {
					switch rs2 {
					case 0:
						r = float64(int32(c.Reg(rs1))) // FCVT.D.W
					case 1:
						r = float64(uint32(c.Reg(rs1))) // FCVT.D.WU
					case 2:
						r = float64(int64(c.Reg(rs1))) // FCVT.D.L
					case 3:
						r = float64(c.Reg(rs1)) // FCVT.D.LU
					}
				})
				if rs2 > 3 {
					return ErrIllegalInstruction
				}
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF64(f64bits(r)))
			case 0x1C: // FMV.X.D (funct3=0) / FCLASS.D (funct3=1)
				switch funct3 {
				case 0:
					c.SetReg(rd, a) // FMV.X.D
				case 1:
					c.SetReg(rd, fclassF64(a)) // FCLASS.D
				default:
					return ErrIllegalInstruction
				}
			case 0x1E: // FMV.D.X
				c.SetFReg(rd, boxF64(c.Reg(rs1)))
			default:
				return ErrIllegalInstruction
			}
		} else {
			return ErrIllegalInstruction
		}
		c.markFPDirty()

	default:
		return ErrIllegalInstruction
	}

	c.pc = nextPC
	c.retireInsn()
	return nil
}

// stepRVC decodes and executes a 16-bit RVC (compressed) instruction.
// Called from step() when bits[1:0] != 0b11.
// Advances PC by 2 on success.
func (c *CPU) stepRVC(insn uint16) error {
	// Compressed register fields:
	//   rd'/rs1'/rs2' (3-bit) map to x(8+field) — the "popular" registers x8-x15.
	// Full rd/rs1/rs2 (5-bit) map directly.
	rp := func(f uint16) uint8 { return uint8(8 + (f & 7)) } // 3-bit -> x8..x15
	rf := func(hi, lo uint) uint8 { return uint8((insn >> lo) & ((1 << (hi - lo + 1)) - 1) & 31) }

	quad := insn & 0x3
	funct3 := insn >> 13

	nextPC := c.pc + 2

	switch quad {

	// ── Quadrant 0 ────────────────────────────────────────────────────
	case 0x0:
		rd := rp((insn >> 2) & 7)
		rs1 := rp((insn >> 7) & 7)
		switch funct3 {
		case 0b000: // C.ADDI4SPN  rd'= sp + nzuimm*4
			nzuimm := uint64(((insn>>11)&3)<<4 | ((insn>>7)&0xF)<<6 |
				((insn>>6)&1)<<2 | ((insn>>5)&1)<<3)
			if nzuimm == 0 {
				return ErrIllegalInstruction
			}
			c.SetReg(rd, c.Reg(2)+nzuimm)
		case 0b010: // C.LW  rd'= mem[rs1'+uimm]
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
			addr := c.Reg(rs1) + uimm
			v, f := c.load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load32U(addr)
			}
			if f != nil {
				return f
			}
			c.SetReg(rd, uint64(int64(int32(v))))
		case 0b001: // C.FLD fd' = mem[rs1'+uimm] (RV64: double-precision float)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			addr := c.Reg(rs1) + uimm
			v, f := c.load64(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load64U(addr)
			}
			if f != nil {
				return f
			}
			c.SetFReg(rd, boxF64(v))
		case 0b011: // C.LD  rd'= mem[rs1'+uimm]
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			addr := c.Reg(rs1) + uimm
			v, f := c.load64(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load64U(addr)
			}
			if f != nil {
				return f
			}
			c.SetReg(rd, v)
		case 0b101: // C.FSD mem[rs1'+uimm] = fs2' (double-precision float)
			rs2f := rp((insn >> 2) & 7)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			addr := c.Reg(rs1) + uimm
			f := c.store64(addr, unboxF64(c.FReg(rs2f)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store64U(addr, unboxF64(c.FReg(rs2f)))
			}
			if f != nil {
				return f
			}
		case 0b110: // C.SW  mem[rs1'+uimm] = rs2'
			rs2 := rp((insn >> 2) & 7)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
			addr := c.Reg(rs1) + uimm
			f := c.store32(addr, uint32(c.Reg(rs2)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store32U(addr, uint32(c.Reg(rs2)))
			}
			if f != nil {
				return f
			}
		case 0b111: // C.SD  mem[rs1'+uimm] = rs2'
			rs2 := rp((insn >> 2) & 7)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			addr := c.Reg(rs1) + uimm
			f := c.store64(addr, c.Reg(rs2))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store64U(addr, c.Reg(rs2))
			}
			if f != nil {
				return f
			}
		default:
			return ErrIllegalInstruction
		}

	// ── Quadrant 1 ────────────────────────────────────────────────────
	case 0x1:
		switch funct3 {
		case 0b000: // C.NOP (rd=0) / C.ADDI (rd!=0)
			rd := rf(11, 7)
			imm6 := int64(insn>>2) & 0x1F
			if (insn>>12)&1 != 0 {
				imm6 |= -32
			}
			c.SetReg(rd, c.Reg(rd)+uint64(imm6))
		case 0b001: // C.ADDIW (RV64, rd!=0)
			rd := rf(11, 7)
			imm6 := int64(insn>>2) & 0x1F
			if (insn>>12)&1 != 0 {
				imm6 |= -32
			}
			c.SetReg(rd, uint64(int64(int32(c.Reg(rd))+int32(imm6))))
		case 0b010: // C.LI  rd = sign_extend(imm6)
			rd := rf(11, 7)
			imm6 := int64(insn>>2) & 0x1F
			if (insn>>12)&1 != 0 {
				imm6 |= -32
			}
			c.SetReg(rd, uint64(imm6))
		case 0b011:
			rd := rf(11, 7)
			if rd == 2 { // C.ADDI16SP
				nzimm := int64(((insn>>12)&1)<<9 | ((insn>>6)&1)<<4 |
					((insn>>5)&1)<<6 | ((insn>>3)&3)<<7 | ((insn>>2)&1)<<5)
				if (insn>>12)&1 != 0 {
					nzimm |= -512
				}
				if nzimm == 0 {
					return ErrIllegalInstruction
				}
				c.SetReg(2, c.Reg(2)+uint64(nzimm))
			} else { // C.LUI
				nzimm := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
				if (insn>>12)&1 != 0 {
					nzimm |= -32
				}
				if nzimm == 0 {
					return ErrIllegalInstruction
				}
				c.SetReg(rd, uint64(nzimm<<12))
			}
		case 0b100: // C.MISC-ALU
			rs1 := rp((insn >> 7) & 7)
			rs2 := rp((insn >> 2) & 7)
			funct2 := (insn >> 10) & 3
			switch funct2 {
			case 0b00: // C.SRLI
				shamt := uint8(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
				c.SetReg(rs1, c.Reg(rs1)>>shamt)
			case 0b01: // C.SRAI
				shamt := uint8(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
				c.SetReg(rs1, uint64(int64(c.Reg(rs1))>>shamt))
			case 0b10: // C.ANDI
				imm6 := int64(insn>>2) & 0x1F
				if (insn>>12)&1 != 0 {
					imm6 |= -32
				}
				c.SetReg(rs1, c.Reg(rs1)&uint64(imm6))
			case 0b11: // C.SUB/XOR/OR/AND/SUBW/ADDW
				bit12 := (insn >> 12) & 1
				op := (insn >> 5) & 3
				if bit12 == 0 {
					switch op {
					case 0b00:
						c.SetReg(rs1, c.Reg(rs1)-c.Reg(rs2)) // C.SUB
					case 0b01:
						c.SetReg(rs1, c.Reg(rs1)^c.Reg(rs2)) // C.XOR
					case 0b10:
						c.SetReg(rs1, c.Reg(rs1)|c.Reg(rs2)) // C.OR
					case 0b11:
						c.SetReg(rs1, c.Reg(rs1)&c.Reg(rs2)) // C.AND
					}
				} else {
					switch op {
					case 0b00:
						c.SetReg(rs1, uint64(int64(int32(c.Reg(rs1))-int32(c.Reg(rs2))))) // C.SUBW
					case 0b01:
						c.SetReg(rs1, uint64(int64(int32(c.Reg(rs1))+int32(c.Reg(rs2))))) // C.ADDW
					default:
						return ErrIllegalInstruction
					}
				}
			}
		case 0b101: // C.J  pc += offset
			off := cjOffset(insn)
			c.pc = c.pc + uint64(off)
			c.retireInsn()
			return nil
		case 0b110: // C.BEQZ
			rs1 := rp((insn >> 7) & 7)
			if c.Reg(rs1) == 0 {
				c.pc = c.pc + uint64(cbOffset(insn))
				c.retireInsn()
				return nil
			}
		case 0b111: // C.BNEZ
			rs1 := rp((insn >> 7) & 7)
			if c.Reg(rs1) != 0 {
				c.pc = c.pc + uint64(cbOffset(insn))
				c.retireInsn()
				return nil
			}
		}

	// ── Quadrant 2 ────────────────────────────────────────────────────
	case 0x2:
		rd := rf(11, 7)
		rs2 := rf(6, 2)
		switch funct3 {
		case 0b000: // C.SLLI
			shamt := uint8(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			c.SetReg(rd, c.Reg(rd)<<shamt)
		case 0b001: // C.FLDSP fd = mem[sp+uimm] (double-precision float)
			uimm := uint64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
			addr := c.Reg(2) + uimm
			v, f := c.load64(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load64U(addr)
			}
			if f != nil {
				return f
			}
			c.SetFReg(rd, boxF64(v))
		case 0b010: // C.LWSP
			uimm := uint64(((insn>>12)&1)<<5 | ((insn>>4)&7)<<2 | ((insn>>2)&3)<<6)
			addr := c.Reg(2) + uimm
			v, f := c.load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load32U(addr)
			}
			if f != nil {
				return f
			}
			c.SetReg(rd, uint64(int64(int32(v))))
		case 0b011: // C.LDSP
			uimm := uint64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
			addr := c.Reg(2) + uimm
			v, f := c.load64(addr)
			if f != nil && f.Kind == FaultMisalign {
				v, f = c.load64U(addr)
			}
			if f != nil {
				return f
			}
			c.SetReg(rd, v)
		case 0b100:
			bit12 := (insn >> 12) & 1
			if bit12 == 0 {
				if rs2 == 0 { // C.JR
					if rd == 0 {
						return ErrIllegalInstruction
					}
					c.pc = c.Reg(rd) &^ 1
					c.retireInsn()
					return nil
				}
				// C.MV
				c.SetReg(rd, c.Reg(rs2))
			} else {
				if rd == 0 && rs2 == 0 { // C.EBREAK
					if c.priv != PrivUser && c.trapToPrivilegedAt(c.pc, CauseBreakpoint, 0, 2) {
						return nil
					}
					c.setTrap(CauseBreakpoint, 2)
					return ErrEbreak
				}
				if rs2 == 0 { // C.JALR
					ret := nextPC
					c.pc = c.Reg(rd) &^ 1
					c.SetReg(1, ret)
					c.retireInsn()
					return nil
				}
				// C.ADD
				c.SetReg(rd, c.Reg(rd)+c.Reg(rs2))
			}
		case 0b101: // C.FSDSP mem[sp+uimm] = fs2 (double-precision float)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
			addr := c.Reg(2) + uimm
			f := c.store64(addr, unboxF64(c.FReg(rs2)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store64U(addr, unboxF64(c.FReg(rs2)))
			}
			if f != nil {
				return f
			}
		case 0b110: // C.SWSP
			uimm := uint64(((insn>>9)&0xF)<<2 | ((insn>>7)&3)<<6)
			addr := c.Reg(2) + uimm
			f := c.store32(addr, uint32(c.Reg(rs2)))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store32U(addr, uint32(c.Reg(rs2)))
			}
			if f != nil {
				return f
			}
		case 0b111: // C.SDSP
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
			addr := c.Reg(2) + uimm
			f := c.store64(addr, c.Reg(rs2))
			if f != nil && f.Kind == FaultMisalign {
				f = c.store64U(addr, c.Reg(rs2))
			}
			if f != nil {
				return f
			}
		default:
			return ErrIllegalInstruction
		}

	// end switch quad (Quadrant 2)

	default:
		return ErrIllegalInstruction
	}

	c.pc = nextPC
	c.retireInsn()
	return nil
}

func misaRV64IMAFDCSU() uint64 {
	const mxlRV64 = uint64(2) << 62
	const extIMAFDCSU = uint64(1<<0 | 1<<2 | 1<<3 | 1<<5 | 1<<8 | 1<<12 | 1<<18 | 1<<20)
	return mxlRV64 | extIMAFDCSU
}

// readCSR reads a CSR value. The bool is false when a strict firmware CPU
// should raise illegal-instruction for an unknown CSR probe.
func (c *CPU) readCSR(addr uint32) (uint64, bool) {
	switch addr {
	case 0x001:
		return uint64(c.fcsr & 0x1F), true // fflags
	case 0x002:
		return uint64((c.fcsr >> 5) & 0x7), true // frm
	case 0x003:
		return uint64(c.fcsr & 0xFF), true // fcsr
	case 0x100:
		return c.mstatus & sstatusReadable, true // sstatus
	case 0x104:
		return c.sie | (c.mie & c.mideleg), true // sie
	case 0x105:
		return c.stvec, true // stvec
	case 0x106:
		return c.scounteren, true // scounteren
	case 0x140:
		return c.sscratch, true // sscratch
	case 0x141:
		return c.sepc, true // sepc
	case 0x142:
		return c.scause, true // scause
	case 0x143:
		return c.stval, true // stval
	case 0x144:
		return c.sip | (c.mipValue() & c.mideleg), true // sip
	case 0x14d:
		return c.stimecmp, true // stimecmp
	case 0x180:
		return c.satp, true // satp
	case 0x300:
		return c.mstatus, true // mstatus
	case 0x301:
		return misaRV64IMAFDCSU(), true
	case 0x302:
		return c.medeleg, true // medeleg
	case 0x303:
		return c.mideleg, true // mideleg
	case 0x304:
		return c.mie, true // mie
	case 0x305:
		return c.mtvec, true // mtvec
	case 0x306:
		return c.mcounteren, true // mcounteren
	case 0x30a:
		return c.menvcfg, true // menvcfg
	case 0x320:
		return c.mcountinh, true // mcountinhibit
	case 0x340:
		return c.mscratch, true // mscratch
	case 0x341:
		return c.mepc, true // mepc
	case 0x342:
		return c.mcause, true // mcause
	case 0x343:
		return c.mtval, true // mtval
	case 0x344:
		return c.mipValue(), true // mip
	case 0xC00:
		return c.riscvInstrBegun, true // cycle approximation tracks instruction attempts
	case 0xC02:
		return c.riscvInstrRetired, true // instret/minstret-style retired instructions
	case 0xC01:
		return c.timerValue(), true
	case 0xF11:
		return 0, true // mvendorid
	case 0xF12:
		return 0, true // marchid
	case 0xF13:
		return 0, true // mimpid
	case 0xF14:
		return 0, true // mhartid = 0
	}
	return 0, !c.strictCSR
}

// writeCSR writes a CSR value. The bool is false when a strict firmware CPU
// should raise illegal-instruction for an unknown or read-only CSR write.
func (c *CPU) writeCSR(addr uint32, val uint64) bool {
	switch addr {
	case 0x001:
		c.fcsr = (c.fcsr &^ 0x1F) | uint32(val&0x1F) // fflags
		c.markFPDirty()
	case 0x002:
		c.fcsr = (c.fcsr &^ 0xE0) | uint32((val&0x7)<<5) // frm
		c.markFPDirty()
	case 0x003:
		c.fcsr = uint32(val & 0xFF) // fcsr
		c.markFPDirty()
	case 0x100:
		c.mstatus = sanitizeMstatus(c.mstatus, (c.mstatus&^sstatusMask)|(val&sstatusMask)) // sstatus
	case 0x104:
		c.sie = val & implementedSieMask // sie
	case 0x105:
		c.stvec = val // stvec
	case 0x106:
		c.scounteren = val & counterCSRMask // scounteren
	case 0x140:
		c.sscratch = val // sscratch
	case 0x141:
		c.sepc = val // sepc
	case 0x142:
		c.scause = val // scause
	case 0x143:
		c.stval = val // stval
	case 0x144:
		c.sip = val & implementedSipMask // sip; Sstc owns STIP.
	case 0x14d:
		c.stimecmp = val // stimecmp
		c.refreshSupervisorTimerPending()
	case 0x180:
		if !satpWriteSupported(val) {
			return true
		}
		if c.satp != val {
			c.satp = val // satp
			c.flushTLB()
		}
	// M-mode trap CSRs
	case 0x300:
		c.mstatus = sanitizeMstatus(c.mstatus, val) // mstatus
	case 0x302:
		c.medeleg = val & implementedMedeleg // medeleg
	case 0x303:
		c.mideleg = val & implementedMideleg // mideleg
	case 0x304:
		c.mie = val & implementedMieMask // mie
	case 0x305:
		c.mtvec = val // mtvec
	case 0x306:
		c.mcounteren = val & counterCSRMask // mcounteren
	case 0x30a:
		c.menvcfg = val // menvcfg
	case 0x320:
		c.mcountinh = val // mcountinhibit
	case 0x340:
		c.mscratch = val // mscratch
	case 0x341:
		c.mepc = val // mepc
	case 0x342:
		c.mcause = val // mcause
	case 0x343:
		c.mtval = val // mtval
	case 0x344:
		c.mip = val & implementedMipMask // mip; Sstc owns STIP.
	// CSRs written by riscv-tests reset_vector — accept silently
	case 0x3A0: // pmpcfg0
	case 0x3B0: // pmpaddr0
	case 0x744: // mnstatus (non-standard)
		// cycle/time/instret are read-only — silently ignore writes
	case 0xC00, 0xC01, 0xC02, 0xF11, 0xF12, 0xF13, 0xF14:
		return !c.strictCSR
	default:
		return !c.strictCSR
	}
	return true
}

// amoOpW applies the AMO operation to a 32-bit word value.
func amoOpW(funct5 uint32, mem, rs2 uint32) uint32 {
	switch funct5 {
	case 0b00001:
		return rs2 // AMOSWAP
	case 0b00000:
		return mem + rs2 // AMOADD
	case 0b00100:
		return mem ^ rs2 // AMOXOR
	case 0b01100:
		return mem & rs2 // AMOAND
	case 0b01000:
		return mem | rs2 // AMOOR
	case 0b10000:
		if int32(mem) < int32(rs2) {
			return mem
		}
		return rs2 // AMOMIN
	case 0b10100:
		if int32(mem) > int32(rs2) {
			return mem
		}
		return rs2 // AMOMAX
	case 0b11000:
		if mem < rs2 {
			return mem
		}
		return rs2 // AMOMINU
	case 0b11100:
		if mem > rs2 {
			return mem
		}
		return rs2 // AMOMAXU
	}
	return mem
}

// amoOpD applies the AMO operation to a 64-bit doubleword value.
func amoOpD(funct5 uint32, mem, rs2 uint64) uint64 {
	switch funct5 {
	case 0b00001:
		return rs2 // AMOSWAP
	case 0b00000:
		return mem + rs2 // AMOADD
	case 0b00100:
		return mem ^ rs2 // AMOXOR
	case 0b01100:
		return mem & rs2 // AMOAND
	case 0b01000:
		return mem | rs2 // AMOOR
	case 0b10000:
		if int64(mem) < int64(rs2) {
			return mem
		}
		return rs2 // AMOMIN
	case 0b10100:
		if int64(mem) > int64(rs2) {
			return mem
		}
		return rs2 // AMOMAX
	case 0b11000:
		if mem < rs2 {
			return mem
		}
		return rs2 // AMOMINU
	case 0b11100:
		if mem > rs2 {
			return mem
		}
		return rs2 // AMOMAXU
	}
	return mem
}

// cjOffset extracts the sign-extended 12-bit J-type offset from a C.J instruction.
func cjOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o>>12)&1)<<11 | ((o>>11)&1)<<4 | ((o>>9)&3)<<8 | ((o>>8)&1)<<10 |
		((o>>7)&1)<<6 | ((o>>6)&1)<<7 | ((o>>3)&7)<<1 | ((o>>2)&1)<<5
	if off&(1<<11) != 0 {
		off |= -1 << 12
	}
	return off
}

// cbOffset extracts the sign-extended 9-bit branch offset from C.BEQZ/C.BNEZ.
func cbOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o>>12)&1)<<8 | ((o>>10)&3)<<3 | ((o>>5)&3)<<6 | ((o>>3)&3)<<1 | ((o>>2)&1)<<5
	if off&(1<<8) != 0 {
		off |= -1 << 9
	}
	return off
}

// orcB: for each byte, if any bit set -> 0xFF, else 0x00 (Zbb ORC.B)
func orcB(x uint64) uint64 {
	const mask = uint64(0x0101010101010101)
	// For each byte: set 0x80 if non-zero, then spread to all bits of that byte.
	x |= x >> 4
	x |= x >> 2
	x |= x >> 1
	// Now bit 0 of each byte is 1 iff the original byte was non-zero.
	x &= mask
	// Spread bit 0 of each byte to all 8 bits of that byte.
	x *= 0xFF
	return x
}

// rev8: reverse byte order (Zbb REV8)
func rev8(x uint64) uint64 {
	return bits.ReverseBytes64(x)
}
