package riscv

import (
	"errors"
	"math/bits"

	"riscv/internal/fenv"
)

var ErrEcall  = errors.New("ecall")
var ErrEbreak = errors.New("ebreak")
var ErrIllegalInstruction = errors.New("illegal instruction")

// CPU is a single RV64I hart.
// mem is inline and first for cache locality — touched on every instruction.
type CPU struct {
	mem   GuestMemory
	pc    uint64
	x     [32]uint64  // x[0] is hardwired zero
	f     [32]uint64  // f0-f31: NaN-boxed float32 or raw float64 bits
	fcsr  uint32      // FP control/status: fflags[4:0] + frm[7:5]
	cycle uint64      // instruction-retired counter (read via cycle/instret CSRs)
	Notes NoteChain   // exception delivery chain; handlers installed by OS layer
	// LR/SC reservation
	resvAddr  uint64
	resvValid bool
	// tohost watch: if non-zero, dispatch loops poll this guest address
	// for a non-zero value and exit when detected. Standard riscv-tests
	// exit mechanism: tohost==1 means PASS, other values mean FAIL.
	watchAddr uint64
	// M-mode trap CSRs — minimal support for riscv-tests.
	// When mtvec != 0, ECALL traps through the guest's own handler
	// instead of returning ErrEcall to the NoteChain.
	mtvec   uint64 // 0x305: trap vector base address
	mepc    uint64 // 0x341: exception program counter
	mcause  uint64 // 0x342: trap cause code
	mstatus uint64 // 0x300: machine status
	mtval   uint64 // 0x343: trap value
	// cache is the decoder cache used by Run(). Lazily allocated on first
	// Run() call when no explicit cache has been set via SetDecoderCache.
	// Persisting it across Run() invocations preserves decoded slots so
	// subsequent runs skip the one-time decode cost.
	cache *DecoderCache
}

func NewCPU(mem GuestMemory) *CPU { return &CPU{mem: mem} }

func (c *CPU) SetPC(addr uint64)        { c.pc = addr }
func (c *CPU) PC() uint64               { return c.pc }
// SetReg writes a GPR. Uses the "always write, always zero x[0]" trick to
// avoid a conditional branch. The trailing `c.x[0] = 0` preserves the RISC-V
// invariant that x0 reads as zero; when r != 0, it's a dead store the CPU
// store buffer absorbs cheaply.
func (c *CPU) SetReg(r uint8, v uint64) { c.x[r] = v; c.x[0] = 0 }

// Reg reads a GPR. Relies on the invariant that x[0] is always 0, maintained
// by SetReg above.
func (c *CPU) Reg(r uint8) uint64 { return c.x[r] }
func (c *CPU) SetFReg(r uint8, v uint64) { c.f[r] = v }
func (c *CPU) FReg(r uint8) uint64       { return c.f[r] }
func (c *CPU) FCSR() uint32              { return c.fcsr }
func (c *CPU) SetFCSR(v uint32)          { c.fcsr = v }
func (c *CPU) Cycle() uint64             { return c.cycle }
func (c *CPU) ResetCycle()               { c.cycle = 0 }
func (c *CPU) SetWatchAddr(addr uint64)  { c.watchAddr = addr }
func (c *CPU) WatchAddr() uint64         { return c.watchAddr }

// Run executes instructions until an unhandled note or fatal exception.
// Uses the decoder-cached fast path (RunCached); exceptions are delivered
// through cpu.Notes. For the reference uncached path (fetch + decode every
// instruction), call RunWithChain directly.
//
// The cache auto-sizes to 256 KB of coverage centered on the current PC on
// first call. To preconfigure size or base, call SetDecoderCache before Run.
func (c *CPU) Run() error {
	return RunCached(c, c.DecoderCache(), &c.Notes)
}

// SetDecoderCache attaches a pre-allocated decoder cache to the CPU so Run
// uses it instead of auto-allocating. Call this before Run() when you need
// control over the cache's base address or size (e.g., a large .text that
// exceeds the default 256 KB coverage).
func (c *CPU) SetDecoderCache(cache *DecoderCache) { c.cache = cache }

// DecoderCache returns the CPU's decoder cache, lazily allocating a default
// one if none has been set. Default coverage: 256 KB based at (pc &^ 0xFFF)
// minus 4 KB of guard, so typical executable segments fit entirely inside.
// PCs outside the cache range fall through to the sentinel-slot path and
// execute via cpu.step().
func (c *CPU) DecoderCache() *DecoderCache {
	if c.cache == nil {
		base := c.pc &^ uint64(0xFFF)
		if base > 0x1000 {
			base -= 0x1000
		}
		c.cache = NewDecoderCache(base, 256<<10)
	}
	return c.cache
}

// Step executes a single instruction. Returns ErrEbreak, ErrEcall, or ErrIllegalInstruction on halt/fault.
func (c *CPU) Step() error { return c.step() }

func (c *CPU) step() error {
	// Fetch 16 bits first to detect compressed (RVC) instructions.
	// Bits[1:0] != 0b11 means 16-bit; 0b11 means 32-bit.
	half, fh := (&c.mem).Fetch16(c.pc)
	if fh != nil {
		return fh
	}
	if half&0x3 != 0x3 {
		return c.stepRVC(uint16(half))
	}

	insn, f := (&c.mem).Fetch32(c.pc)
	if f != nil {
		if f.Kind == FaultMisalign {
			insn, f = (&c.mem).Fetch32U(c.pc)
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
// Used by RunCached and other fast-path drivers to bypass the guest-memory
// fetch that step() would otherwise redo on every visit to a given PC.
func (c *CPU) stepFromInsn(insn uint32) error {
	opcode := uint8(insn & 0x7F)
	rd     := uint8((insn >> 7) & 0x1F)
	funct3 := uint8((insn >> 12) & 0x07)
	rs1    := uint8((insn >> 15) & 0x1F)
	rs2    := uint8((insn >> 20) & 0x1F)

	// I-type immediate: sign-extended bits [31:20]
	iimm := int64(int32(insn)) >> 20

	nextPC := c.pc + 4

	switch opcode {

	// ── LOAD (I-type) ────────────────────────────────────────────────────
	case 0x03:
		addr := c.Reg(rs1) + uint64(iimm)
		var v uint64
		switch funct3 {
		case 0x0: // LB — sign-extend 8→64
			u, f := (&c.mem).Load8(addr); if f != nil { return f }
			v = uint64(int64(int8(u)))
		case 0x1: // LH — sign-extend 16→64 (misalign-capable)
			u, f := (&c.mem).Load16(addr)
			if f != nil && f.Kind == FaultMisalign { u, f = (&c.mem).Load16U(addr) }
			if f != nil { return f }
			v = uint64(int64(int16(u)))
		case 0x2: // LW — sign-extend 32→64 (misalign-capable)
			u, f := (&c.mem).Load32(addr)
			if f != nil && f.Kind == FaultMisalign { u, f = (&c.mem).Load32U(addr) }
			if f != nil { return f }
			v = uint64(int64(int32(u)))
		case 0x3: // LD — full 64-bit (misalign-capable)
			u, f := (&c.mem).Load64(addr)
			if f != nil && f.Kind == FaultMisalign { u, f = (&c.mem).Load64U(addr) }
			if f != nil { return f }
			v = u
		case 0x4: // LBU — zero-extend 8→64
			u, f := (&c.mem).Load8(addr); if f != nil { return f }
			v = uint64(u)
		case 0x5: // LHU — zero-extend 16→64 (misalign-capable)
			u, f := (&c.mem).Load16(addr)
			if f != nil && f.Kind == FaultMisalign { u, f = (&c.mem).Load16U(addr) }
			if f != nil { return f }
			v = uint64(u)
		case 0x6: // LWU — zero-extend 32→64 (misalign-capable)
			u, f := (&c.mem).Load32(addr)
			if f != nil && f.Kind == FaultMisalign { u, f = (&c.mem).Load32U(addr) }
			if f != nil { return f }
			v = uint64(u)
		default:
			return ErrIllegalInstruction
		}
		c.SetReg(rd, v)

	// ── STORE (S-type) ───────────────────────────────────────────────────
	case 0x23:
		simm := int64(int32(insn&0xFE000000)>>20) | int64((insn>>7)&0x1F)
		addr := c.Reg(rs1) + uint64(simm)
		switch funct3 {
		case 0x0: // SB
			if f := (&c.mem).Store8(addr, uint8(c.Reg(rs2))); f != nil { return f }
		case 0x1: // SH (misalign-capable)
			f := (&c.mem).Store16(addr, uint16(c.Reg(rs2)))
			if f != nil && f.Kind == FaultMisalign { f = (&c.mem).Store16U(addr, uint16(c.Reg(rs2))) }
			if f != nil { return f }
		case 0x2: // SW (misalign-capable)
			f := (&c.mem).Store32(addr, uint32(c.Reg(rs2)))
			if f != nil && f.Kind == FaultMisalign { f = (&c.mem).Store32U(addr, uint32(c.Reg(rs2))) }
			if f != nil { return f }
		case 0x3: // SD (misalign-capable)
			f := (&c.mem).Store64(addr, c.Reg(rs2))
			if f != nil && f.Kind == FaultMisalign { f = (&c.mem).Store64U(addr, c.Reg(rs2)) }
			if f != nil { return f }
		default:
			return ErrIllegalInstruction
		}

	// ── OP-IMM (I-type) ──────────────────────────────────────────────────
	case 0x13: // ── OP-IMM ─────────────────────────────────────────────────
		funct7i := insn >> 25
		shamt   := uint8(insn >> 20) & 0x3F
		a       := c.Reg(rs1)
		var v uint64
		switch funct3 {
		case 0x0: v = a + uint64(iimm)                                 // ADDI
		case 0x1: // SLLI / Zbs BSETI/BCLRI/BINVI / Zbb CLZ/CTZ/CPOP/SEXT
			// Use bits[31:26] (mask out bit25=shamt[5]) to identify the operation.
			switch funct7i &^ 1 {
			case 0x00: v = a << shamt                                  // SLLI (shamt[5] may be 0 or 1)
			case 0x14: v = a | (1 << uint(shamt))                     // BSETI
			case 0x24: v = a &^ (1 << uint(shamt))                    // BCLRI
			case 0x34: v = a ^ (1 << uint(shamt))                     // BINVI
			case 0x60: // CLZ/CTZ/CPOP/SEXT.B/SEXT.H — rs2 in shamt
				switch shamt {
				case 0:  v = uint64(bits.LeadingZeros64(a))            // CLZ
				case 1:  v = uint64(bits.TrailingZeros64(a))           // CTZ
				case 2:  v = uint64(bits.OnesCount64(a))               // CPOP
				case 0x22: v = uint64(int64(int8(a)))                  // SEXT.B
				case 0x23: v = uint64(int64(int16(a)))                 // SEXT.H
				default: return ErrIllegalInstruction
				}
			default: return ErrIllegalInstruction
			}
		case 0x2: if int64(a) < iimm { v = 1 }                        // SLTI
		case 0x3: if a < uint64(iimm) { v = 1 }                       // SLTIU
		case 0x4: v = a ^ uint64(iimm)                                 // XORI
		case 0x5: // SRLI/SRAI / Zbs BEXTI / Zbb RORI/ORC.B/REV8/ZEXT.H
			switch funct7i &^ 1 {
			case 0x00: v = a >> shamt                                  // SRLI
			case 0x20: v = uint64(int64(a) >> shamt)                  // SRAI
			case 0x24: v = (a >> uint(shamt)) & 1                     // BEXTI
			case 0x30: sh := uint(shamt & 63); v = (a>>sh)|(a<<(64-sh)) // RORI
			case 0x14: v = orcB(a)                                    // ORC.B
			case 0x34: v = rev8(a)                                    // REV8
			case 0x04: v = a & 0xFFFF                                 // ZEXT.H
			default: return ErrIllegalInstruction
			}
		case 0x6: v = a | uint64(iimm)                                 // ORI
		case 0x7: v = a & uint64(iimm)                                 // ANDI
		}
		c.SetReg(rd, v)

	// ── OP-IMM-32 (I-type, 32-bit ops, sign-extend result) ───────────────
	case 0x1B: // ── OP-IMM-32 ───────────────────────────────────────────────
		funct7i32 := insn >> 25
		shamt := uint8(insn >> 20) & 0x1F
		var v int32
		switch funct3 {
		case 0x0: // ADDIW
			v = int32(c.Reg(rs1)) + int32(iimm)
		case 0x1: // SLLIW / Zba SLLI.UW
			if funct7i32 == 0x04 { // SLLI.UW: rd = uint64(uint32(rs1)) << shamt
				c.SetReg(rd, uint64(uint32(c.Reg(rs1)))<<uint(shamt))
				c.pc = nextPC; return nil
			}
			v = int32(c.Reg(rs1)) << shamt
		case 0x5: // SRLIW / SRAIW / RORIW
			switch funct7i32 >> 1 { // mask shamt[5] (always 0 for 5-bit shamt)
			case 0x00: v = int32(uint32(c.Reg(rs1)) >> shamt)           // SRLIW
			case 0x10: v = int32(c.Reg(rs1)) >> shamt                   // SRAIW
			case 0x30: // RORIW: rotate right word immediate
				a32 := uint32(c.Reg(rs1))
				sh := uint(shamt & 0x1F)
				v = int32(a32>>sh | a32<<(32-sh))
			default: return ErrIllegalInstruction
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
			case 0x0: v = a * b
			case 0x1: hi, _ := bits.Mul64(a, b); if int64(a)<0 { hi-=b }; if int64(b)<0 { hi-=a }; v=hi
			case 0x2: hi, _ := bits.Mul64(a, b); if int64(a)<0 { hi-=b }; v=hi
			case 0x3: hi, _ := bits.Mul64(a, b); v=hi
			case 0x4: if b==0 { v=^uint64(0) } else if a==0x8000000000000000&&b==^uint64(0) { v=a } else { v=uint64(int64(a)/int64(b)) }
			case 0x5: if b==0 { v=^uint64(0) } else { v=a/b }
			case 0x6: if b==0 { v=a } else if a==0x8000000000000000&&b==^uint64(0) { v=0 } else { v=uint64(int64(a)%int64(b)) }
			case 0x7: if b==0 { v=a } else { v=a%b }
			}
		// ── Zbb/Zbs/Zba (OP) ────────────────────────────────────────────────
		case 0x04: // Zbb: ZEXT.H
			v = a & 0xFFFF
		case 0x05: // Zbb: MIN/MAX + Zbc: CLMUL/CLMULR/CLMULH
			switch funct3 {
			case 1: // CLMUL
				var r uint64
				for i := 0; i < 64; i++ { if (b>>i)&1 != 0 { r ^= a << i } }
				v = r
			case 2: // CLMULR
				var r uint64
				for i := 0; i < 63; i++ { if (b>>i)&1 != 0 { r ^= a >> (63 - i) } }
				v = r
			case 3: // CLMULH
				var r uint64
				for i := 1; i < 64; i++ { if (b>>i)&1 != 0 { r ^= a >> (64 - i) } }
				v = r
			case 4: if int64(a)<int64(b) { v=a } else { v=b }  // MIN
			case 5: if a<b { v=a } else { v=b }                 // MINU
			case 6: if int64(a)>int64(b) { v=a } else { v=b }  // MAX
			case 7: if a>b { v=a } else { v=b }                 // MAXU
			}
		case 0x07: // Zicond: CZERO.EQZ / CZERO.NEZ
			switch funct3 {
			case 5: if b == 0 { v = 0 } else { v = a }  // CZERO.EQZ
			case 7: if b != 0 { v = 0 } else { v = a }  // CZERO.NEZ
			}
		case 0x10: // Zba: SH1ADD/SH2ADD/SH3ADD
			switch funct3 {
			case 2: v = b + (a << 1)
			case 4: v = b + (a << 2)
			case 6: v = b + (a << 3)
			}
		case 0x14: // Zbs: BSET / Zbb: ORC.B
			switch funct3 {
			case 1: v = a | (1 << (b & 63))            // BSET
			case 5: v = orcB(a)                         // ORC.B (rs2=7, but we check funct3)
			}
		case 0x20: // RV64I SUB/SRA + Zbb ANDN/ORN/XNOR
			switch funct3 {
			case 0: v = a - b                              // SUB
			case 4: v = a ^ ^b                             // XNOR
			case 5: v = uint64(int64(a) >> (b & 0x3F))    // SRA
			case 6: v = a | ^b                             // ORN
			case 7: v = a & ^b                             // ANDN
			}
		case 0x24: // Zbs: BCLR/BEXT
			switch funct3 {
			case 1: v = a &^ (1 << (b & 63))           // BCLR
			case 5: v = (a >> (b & 63)) & 1            // BEXT
			}
		case 0x30: // Zbb: ROL/ROR/SEXT.B/SEXT.H  (funct7=0x30, rs2 disambiguates)
			switch funct3 {
			case 1: // ROL
				sh := b & 63
				v = (a << sh) | (a >> (64 - sh))
			case 5: // ROR
				sh := b & 63
				v = (a >> sh) | (a << (64 - sh))
			}
		case 0x34: // Zbs: BINV
			v = a ^ (1 << (b & 63))
		case 0x35: // Zbb: REV8 (funct3=5, rs2=24 for RV64)
			v = rev8(a)
		case 0x60: // Zbb: CLZ/CTZ/CPOP/SEXT.B/SEXT.H (funct3=1, rs2 selects)
			switch rs2 {
			case 0: v = uint64(bits.LeadingZeros64(a))   // CLZ
			case 1: v = uint64(bits.TrailingZeros64(a))  // CTZ
			case 2: v = uint64(bits.OnesCount64(a))      // CPOP
			case 2+0x20: // SEXT.B (rs2=0x22, funct7=0x30 handled above — but gcc may emit differently)
				v = uint64(int64(int8(a)))
			case 3+0x20:
				v = uint64(int64(int16(a)))
			}
		default: // ── RV64I ─────────────────────────────────────────────────
			switch funct3 {
			case 0x0:
				if funct7 == 0x20 { v = a - b } else { v = a + b } // SUB / ADD
			case 0x1: v = a << (b & 0x3F)                          // SLL
			case 0x2: if int64(a) < int64(b) { v = 1 }            // SLT
			case 0x3: if a < b { v = 1 }                           // SLTU
			case 0x4: v = a ^ b                                    // XOR
			case 0x5:
				if funct7 == 0x20 { v = uint64(int64(a) >> (b & 0x3F)) } else { v = a >> (b & 0x3F) } // SRA/SRL
			case 0x6: v = a | b                                    // OR
			case 0x7: v = a & b                                    // AND
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
			case 0x0: v = int32(a32 * b32)
			case 0x4:
				if b32 == 0 { v = -1 } else if a32==0x80000000&&b32==0xFFFFFFFF { v=int32(a32) } else { v=int32(a32)/int32(b32) }
			case 0x5: if b32==0 { v=-1 } else { v=int32(a32/b32) }
			case 0x6:
				if b32==0 { v=int32(a32) } else if a32==0x80000000&&b32==0xFFFFFFFF { v=0 } else { v=int32(a32)%int32(b32) }
			case 0x7: if b32==0 { v=int32(a32) } else { v=int32(a32%b32) }
			default: return ErrIllegalInstruction
			}
		case 0x04: // Zba: ADD.UW  rd = x3 + uint64(uint32(x2))
			c.SetReg(rd, c.Reg(rs2)+uint64(uint32(c.Reg(rs1))))
			c.pc = nextPC; return nil
		case 0x10: // Zba: SH1ADD.UW / SH2ADD.UW / SH3ADD.UW
			zext := uint64(uint32(c.Reg(rs1)))
			base := c.Reg(rs2)
			switch funct3 {
			case 2: c.SetReg(rd, base+(zext<<1))
			case 4: c.SetReg(rd, base+(zext<<2))
			case 6: c.SetReg(rd, base+(zext<<3))
			default: return ErrIllegalInstruction
			}
			c.pc = nextPC; return nil
		case 0x30: // Zbb: ROLW / RORW
			sh := b32 & 0x1F
			switch funct3 {
			case 1: v = int32(a32<<sh | a32>>(32-sh))
			case 5: v = int32(a32>>sh | a32<<(32-sh))
			default: return ErrIllegalInstruction
			}
		case 0x60: // Zbb: CLZW / CTZW / CPOPW
			switch rs2 {
			case 0: v = int32(bits.LeadingZeros32(a32))
			case 1: v = int32(bits.TrailingZeros32(a32))
			case 2: v = int32(bits.OnesCount32(a32))
			default: return ErrIllegalInstruction
			}
		default: // ── RV64I word ops ─────────────────────────────────────
			switch funct3 {
			case 0x0:
				if funct7==0x20 { v=int32(a32-b32) } else { v=int32(a32+b32) }
			case 0x1: v = int32(a32 << (b32 & 0x1F))
			case 0x5:
				if funct7==0x20 { v=int32(a32)>>(b32&0x1F) } else { v=int32(a32>>(b32&0x1F)) }
			default: return ErrIllegalInstruction
			}
		}
		c.SetReg(rd, uint64(int64(v)))


	// ── AMO — RV64A atomic memory operations ─────────────────────────────
	case 0x2F:
		funct5 := insn >> 27
		width  := funct3 // 010=W, 011=D
		addr   := c.Reg(rs1)

		switch funct5 {
		case 0b00010: // LR.W / LR.D
			var v uint64
			if width == 0b010 {
				u, f := (&c.mem).Load32(addr)
				if f != nil { return f }
				v = uint64(int64(int32(u)))
			} else {
				u, f := (&c.mem).Load64(addr)
				if f != nil { return f }
				v = u
			}
			c.SetReg(rd, v)
			c.resvAddr  = addr
			c.resvValid = true

		case 0b00011: // SC.W / SC.D
			if c.resvValid && c.resvAddr == addr {
				if width == 0b010 {
					if f := (&c.mem).Store32(addr, uint32(c.Reg(rs2))); f != nil { return f }
				} else {
					if f := (&c.mem).Store64(addr, c.Reg(rs2)); f != nil { return f }
				}
				c.SetReg(rd, 0) // success
			} else {
				c.SetReg(rd, 1) // failure
			}
			c.resvValid = false

		default: // AMO ops: rd=mem[rs1]; mem[rs1]=op(rd,rs2); advance PC
			if width == 0b010 { // .W — 32-bit
				old, f := (&c.mem).Load32(addr)
				if f != nil { return f }
				oldSE := uint64(int64(int32(old))) // sign-extended for rd
				newVal := amoOpW(funct5, old, uint32(c.Reg(rs2)))
				if f := (&c.mem).Store32(addr, newVal); f != nil { return f }
				c.SetReg(rd, oldSE)
			} else { // .D — 64-bit
				old, f := (&c.mem).Load64(addr)
				if f != nil { return f }
				newVal := amoOpD(funct5, old, c.Reg(rs2))
				if f := (&c.mem).Store64(addr, newVal); f != nil { return f }
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
		return nil

	// ── JALR (I-type) ────────────────────────────────────────────────────
	case 0x67:
		target := (c.Reg(rs1) + uint64(iimm)) &^ 1
		c.SetReg(rd, uint64(nextPC))
		c.pc = target
		return nil

	// ── BRANCH (B-type) ──────────────────────────────────────────────────
	case 0x63:
		bimm := int64(int32(
			((insn>>31)&1)<<20 |
			((insn>>7)&1)<<19 |
			((insn>>25)&0x3F)<<13 |
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
		case 0x0: taken = a == b               // BEQ
		case 0x1: taken = a != b               // BNE
		case 0x4: taken = int64(a) < int64(b)  // BLT
		case 0x5: taken = int64(a) >= int64(b) // BGE
		case 0x6: taken = a < b                // BLTU
		case 0x7: taken = a >= b               // BGEU
		default:  return ErrIllegalInstruction
		}
		if taken {
			c.pc = c.pc + uint64(bimm)
			return nil
		}

	// ── SYSTEM ───────────────────────────────────────────────────────────
	case 0x73: // ── SYSTEM ───────────────────────────────────────────────────
		csrAddr := insn >> 20
		switch {
		case insn == 0x00100073: // EBREAK
			c.pc = nextPC; return ErrEbreak
		case insn == 0x00000073: // ECALL
			if c.mtvec != 0 {
				c.mepc = c.pc  // save PC of ECALL instruction
				c.mcause = 8   // CauseEcallU
				c.mtval = 0
				c.pc = c.mtvec // trap to handler
				return nil
			}
			c.pc = nextPC; return ErrEcall
		case insn == 0x30200073: // MRET
			c.pc = c.mepc; return nil
		case insn == 0x10500073: // WFI — no-op in user-mode emulation
		case funct3 == 0 && insn>>25 == 0x09: // SFENCE.VMA — no-op in user-mode
		case funct3 >= 1 && funct3 <= 7 && funct3 != 4: // Zicsr
			// Read old CSR value
			old := c.readCSR(csrAddr)
			c.SetReg(rd, old)
			// Compute new value and write if applicable
			var src uint64
			if funct3 >= 5 { // immediate forms (funct3=5/6/7)
				src = uint64(rs1) // rs1 field is the uimm5
			} else {
				src = c.Reg(rs1)
			}
			var newVal uint64
			switch funct3 {
			case 1, 5: newVal = src                // CSRRW/CSRRWI: write src
			case 2, 6: newVal = old | src           // CSRRS/CSRRSI: set bits
			case 3, 7: newVal = old &^ src          // CSRRC/CSRRCI: clear bits
			}
			// Only write if rs1 != x0 (or uimm5 != 0 for immediate forms)
			if src != 0 || funct3 == 1 || funct3 == 5 {
				c.writeCSR(csrAddr, newVal)
			}
		default:
			return ErrIllegalInstruction
		}

	// ── FLW / FLD — float loads ──────────────────────────────────────────
	case 0x07:
		addr := uint64(int64(c.Reg(rs1)) + iimm)
		switch funct3 {
		case 0b010: // FLW
			v, f := (&c.mem).Load32(addr); if f != nil { return f }
			c.SetFReg(rd, boxF32(v))
		case 0b011: // FLD
			v, f := (&c.mem).Load64(addr); if f != nil { return f }
			c.SetFReg(rd, boxF64(v))
		default: return ErrIllegalInstruction
		}

	// ── FSW / FSD — float stores ──────────────────────────────────────────
	case 0x27:
		simm := int64(int32(insn&0xFE000000)>>20) | int64((insn>>7)&0x1F)
		addr := uint64(int64(c.Reg(rs1)) + simm)
		switch funct3 {
		case 0b010: // FSW
			if f := (&c.mem).Store32(addr, uint32(c.FReg(rs2))); f != nil { return f }  // FSW: raw low 32 bits
		case 0b011: // FSD
			if f := (&c.mem).Store64(addr, c.FReg(rs2)); f != nil { return f }
		default: return ErrIllegalInstruction
		}

	// ── FMADD/FMSUB/FNMSUB/FNMADD — fused multiply-add (R4-type) ─────────
	case 0x43, 0x47, 0x4B, 0x4F:
		rs3 := uint8(insn >> 27)
		fmt := uint8((insn >> 25) & 0x3)
		if fmt == 0 { // .S single-precision
			a := f32frombits(unboxF32(c.FReg(rs1)))
			b := f32frombits(unboxF32(c.FReg(rs2)))
			d := f32frombits(unboxF32(c.FReg(rs3)))
			var v float32; var fl uint32
			switch opcode {
			case 0x43: v, fl = fenv.MAddF32(a, b, d)
			case 0x47: v, fl = fenv.MSubF32(a, b, d)
			case 0x4B: v, fl = fenv.NMSubF32(a, b, d)
			case 0x4F: v, fl = fenv.NMAddF32(a, b, d)
			}
			c.fcsr |= fl
			c.SetFReg(rd, boxF32(canonNaN32(f32bits(v))))
		} else if fmt == 1 { // .D double-precision
			a := f64frombits(c.FReg(rs1))
			b := f64frombits(c.FReg(rs2))
			d := f64frombits(c.FReg(rs3))
			var v float64; var fl uint32
			switch opcode {
			case 0x43: v, fl = fenv.MAddF64(a, b, d)
			case 0x47: v, fl = fenv.MSubF64(a, b, d)
			case 0x4B: v, fl = fenv.NMSubF64(a, b, d)
			case 0x4F: v, fl = fenv.NMAddF64(a, b, d)
			}
			c.fcsr |= fl
			c.SetFReg(rd, boxF64(canonNaN64(f64bits(v))))
		} else { return ErrIllegalInstruction }

	// ── FPFUNC — all other float ops (opcode=0x53) ────────────────────────
	case 0x53:
		funct5 := uint8(insn >> 27)
		fmt    := uint8((insn >> 25) & 0x3)
		if fmt == 0 { // ── single-precision ────────────────────────────────
			a := unboxF32(c.FReg(rs1))
			b := unboxF32(c.FReg(rs2))
			af, bf := f32frombits(a), f32frombits(b)
			switch funct5 {
			case 0x00: r32, fl := fenv.AddF32(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x01: r32, fl := fenv.SubF32(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x02: r32, fl := fenv.MulF32(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x03: r32, fl := fenv.DivF32(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x0B: r32, fl := fenv.SqrtF32(af);   c.fcsr|=fl; c.SetFReg(rd, boxF32(canonNaN32(f32bits(r32))))
			case 0x04: // FSGNJ.S / FSGNJN.S / FSGNJX.S
				switch funct3 {
				case 0: c.SetFReg(rd, boxF32(fsgnjF32(a,b)))
				case 1: c.SetFReg(rd, boxF32(fsgnjnF32(a,b)))
				case 2: c.SetFReg(rd, boxF32(fsgnjxF32(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x05: // FMIN.S / FMAX.S
				if isSNaNF32(a) || isSNaNF32(b) { c.fcsr |= fflagNV }
				switch funct3 {
				case 0: c.SetFReg(rd, boxF32(fminF32(a,b)))
				case 1: c.SetFReg(rd, boxF32(fmaxF32(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x08: // FCVT.S.D  (rs2=1 = from D)
				src := c.FReg(rs1)
				r := float32(f64frombits(src))
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF32(canonNaN32(f32bits(r))))
			case 0x14: // FEQ.S / FLT.S / FLE.S -> integer rd
				var v uint64
				switch funct3 {
				case 2: // FEQ.S
					if af == bf { v = 1 }
					if isSNaNF32(a) || isSNaNF32(b) { c.fcsr |= fflagNV }
				case 1: // FLT.S
					if af < bf { v = 1 }
					if isNaNF32(a) || isNaNF32(b) { c.fcsr |= fflagNV }
				case 0: // FLE.S
					if af <= bf { v = 1 }
					if isNaNF32(a) || isNaNF32(b) { c.fcsr |= fflagNV }
				default: return ErrIllegalInstruction
				}
				c.SetReg(rd, v)
			case 0x18: // FCVT.{W,WU,L,LU}.S -> integer rd
				switch rs2 {
				case 0: v, fl := fcvtWS(af);  c.fcsr |= fl; c.SetReg(rd, v)
				case 1: v, fl := fcvtWUS(af); c.fcsr |= fl; c.SetReg(rd, v)
				case 2: v, fl := fcvtLS(af);  c.fcsr |= fl; c.SetReg(rd, v)
				case 3: v, fl := fcvtLUS(af); c.fcsr |= fl; c.SetReg(rd, v)
				}
			case 0x1A: // FCVT.S.{W,WU,L,LU} <- integer rs1
				var r float32
				switch rs2 {
				case 0: r = float32(int32(c.Reg(rs1)))   // FCVT.S.W
				case 1: r = float32(uint32(c.Reg(rs1)))  // FCVT.S.WU
				case 2: r = float32(int64(c.Reg(rs1)))   // FCVT.S.L
				case 3: r = float32(c.Reg(rs1))           // FCVT.S.LU
				}
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF32(f32bits(r)))
			case 0x1C: // FMV.X.W (funct3=0) / FCLASS.S (funct3=1)
				switch funct3 {
				case 0: c.SetReg(rd, uint64(int64(int32(uint32(c.FReg(rs1))))))  // FMV.X.W raw bits
				case 1: c.SetReg(rd, fclassF32(a))                                // FCLASS.S
				default: return ErrIllegalInstruction
				}
			case 0x1E: // FMV.W.X
				c.SetFReg(rd, boxF32(uint32(c.Reg(rs1))))
			default: return ErrIllegalInstruction
			}
		} else if fmt == 1 { // ── double-precision ────────────────────────
			a := c.FReg(rs1)
			b := c.FReg(rs2)
			af, bf := f64frombits(a), f64frombits(b)
			switch funct5 {
			case 0x00: r64, fl := fenv.AddF64(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x01: r64, fl := fenv.SubF64(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x02: r64, fl := fenv.MulF64(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x03: r64, fl := fenv.DivF64(af,bf); c.fcsr|=fl; c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x0B: r64, fl := fenv.SqrtF64(af);   c.fcsr|=fl; c.SetFReg(rd, boxF64(canonNaN64(f64bits(r64))))
			case 0x04: // FSGNJ.D / FSGNJN.D / FSGNJX.D
				switch funct3 {
				case 0: c.SetFReg(rd, boxF64(fsgnjF64(a,b)))
				case 1: c.SetFReg(rd, boxF64(fsgnjnF64(a,b)))
				case 2: c.SetFReg(rd, boxF64(fsgnjxF64(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x05: // FMIN.D / FMAX.D
				if isSNaNF64(a) || isSNaNF64(b) { c.fcsr |= fflagNV }
				switch funct3 {
				case 0: c.SetFReg(rd, boxF64(fminF64(a,b)))
				case 1: c.SetFReg(rd, boxF64(fmaxF64(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x08: // FCVT.D.S  (rs2=0 = from S)
				src := unboxF32(c.FReg(rs1))
				r := float64(f32frombits(src))
				c.SetFReg(rd, boxF64(canonNaN64(f64bits(r))))
			case 0x14: // FEQ.D / FLT.D / FLE.D
				var v uint64
				switch funct3 {
				case 2: // FEQ.D
					if af == bf { v = 1 }
					if isSNaNF64(a) || isSNaNF64(b) { c.fcsr |= fflagNV }
				case 1: // FLT.D
					if af < bf { v = 1 }
					if isNaNF64(a) || isNaNF64(b) { c.fcsr |= fflagNV }
				case 0: // FLE.D
					if af <= bf { v = 1 }
					if isNaNF64(a) || isNaNF64(b) { c.fcsr |= fflagNV }
				default: return ErrIllegalInstruction
				}
				c.SetReg(rd, v)
			case 0x18: // FCVT.{W,WU,L,LU}.D
				switch rs2 {
				case 0: v, fl := fcvtWD(af);  c.fcsr |= fl; c.SetReg(rd, v)
				case 1: v, fl := fcvtWUD(af); c.fcsr |= fl; c.SetReg(rd, v)
				case 2: v, fl := fcvtLD(af);  c.fcsr |= fl; c.SetReg(rd, v)
				case 3: v, fl := fcvtLUD(af); c.fcsr |= fl; c.SetReg(rd, v)
				}
			case 0x1A: // FCVT.D.{W,WU,L,LU}
				var r float64
				switch rs2 {
				case 0: r = float64(int32(c.Reg(rs1)))   // FCVT.D.W
				case 1: r = float64(uint32(c.Reg(rs1)))  // FCVT.D.WU
				case 2: r = float64(int64(c.Reg(rs1)))   // FCVT.D.L
				case 3: r = float64(c.Reg(rs1))           // FCVT.D.LU
				}
				c.fcsr |= fenv.FFlags()
				c.SetFReg(rd, boxF64(f64bits(r)))
			case 0x1C: // FMV.X.D (funct3=0) / FCLASS.D (funct3=1)
				switch funct3 {
				case 0: c.SetReg(rd, a)                                  // FMV.X.D
				case 1: c.SetReg(rd, fclassF64(a))                       // FCLASS.D
				default: return ErrIllegalInstruction
				}
			case 0x1E: // FMV.D.X
				c.SetFReg(rd, boxF64(c.Reg(rs1)))
			default: return ErrIllegalInstruction
			}
		} else { return ErrIllegalInstruction }

	default:
		return ErrIllegalInstruction
	}

	c.pc = nextPC
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

	quad   := insn & 0x3
	funct3 := insn >> 13

	nextPC := c.pc + 2

	switch quad {

	// ── Quadrant 0 ────────────────────────────────────────────────────
	case 0x0:
		rd  := rp((insn >> 2) & 7)
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
			v, f := (&c.mem).Load32(c.Reg(rs1) + uimm)
			if f != nil { return f }
			c.SetReg(rd, uint64(int64(int32(v))))
		case 0b001: // C.FLD fd' = mem[rs1'+uimm] (RV64: double-precision float)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			v, f := (&c.mem).Load64(c.Reg(rs1) + uimm)
			if f != nil { return f }
			c.SetFReg(rd, boxF64(v))
		case 0b011: // C.LD  rd'= mem[rs1'+uimm]
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			v, f := (&c.mem).Load64(c.Reg(rs1) + uimm)
			if f != nil { return f }
			c.SetReg(rd, v)
		case 0b101: // C.FSD mem[rs1'+uimm] = fs2' (double-precision float)
			rs2f := rp((insn >> 2) & 7)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			if f := (&c.mem).Store64(c.Reg(rs1)+uimm, unboxF64(c.FReg(rs2f))); f != nil { return f }
		case 0b110: // C.SW  mem[rs1'+uimm] = rs2'
			rs2 := rp((insn >> 2) & 7)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
			if f := (&c.mem).Store32(c.Reg(rs1)+uimm, uint32(c.Reg(rs2))); f != nil { return f }
		case 0b111: // C.SD  mem[rs1'+uimm] = rs2'
			rs2 := rp((insn >> 2) & 7)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			if f := (&c.mem).Store64(c.Reg(rs1)+uimm, c.Reg(rs2)); f != nil { return f }
		default:
			return ErrIllegalInstruction
		}

	// ── Quadrant 1 ────────────────────────────────────────────────────
	case 0x1:
		switch funct3 {
		case 0b000: // C.NOP (rd=0) / C.ADDI (rd!=0)
			rd := rf(11, 7)
			imm6 := int64(insn>>2) & 0x1F
			if (insn>>12)&1 != 0 { imm6 |= -32 }
			c.SetReg(rd, c.Reg(rd)+uint64(imm6))
		case 0b001: // C.ADDIW (RV64, rd!=0)
			rd := rf(11, 7)
			imm6 := int64(insn>>2) & 0x1F
			if (insn>>12)&1 != 0 { imm6 |= -32 }
			c.SetReg(rd, uint64(int64(int32(c.Reg(rd))+int32(imm6))))
		case 0b010: // C.LI  rd = sign_extend(imm6)
			rd := rf(11, 7)
			imm6 := int64(insn>>2) & 0x1F
			if (insn>>12)&1 != 0 { imm6 |= -32 }
			c.SetReg(rd, uint64(imm6))
		case 0b011:
			rd := rf(11, 7)
			if rd == 2 { // C.ADDI16SP
				nzimm := int64(((insn>>12)&1)<<9 | ((insn>>6)&1)<<4 |
					((insn>>5)&1)<<6 | ((insn>>3)&3)<<7 | ((insn>>2)&1)<<5)
				if (insn>>12)&1 != 0 { nzimm |= -512 }
				if nzimm == 0 { return ErrIllegalInstruction }
				c.SetReg(2, c.Reg(2)+uint64(nzimm))
			} else { // C.LUI
				nzimm := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
				if (insn>>12)&1 != 0 { nzimm |= -32 }
				if nzimm == 0 { return ErrIllegalInstruction }
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
				if (insn>>12)&1 != 0 { imm6 |= -32 }
				c.SetReg(rs1, c.Reg(rs1)&uint64(imm6))
			case 0b11: // C.SUB/XOR/OR/AND/SUBW/ADDW
				bit12 := (insn >> 12) & 1
				op    := (insn >> 5) & 3
				if bit12 == 0 {
					switch op {
					case 0b00: c.SetReg(rs1, c.Reg(rs1)-c.Reg(rs2))                                      // C.SUB
					case 0b01: c.SetReg(rs1, c.Reg(rs1)^c.Reg(rs2))                                      // C.XOR
					case 0b10: c.SetReg(rs1, c.Reg(rs1)|c.Reg(rs2))                                      // C.OR
					case 0b11: c.SetReg(rs1, c.Reg(rs1)&c.Reg(rs2))                                      // C.AND
					}
				} else {
					switch op {
					case 0b00: c.SetReg(rs1, uint64(int64(int32(c.Reg(rs1))-int32(c.Reg(rs2)))))          // C.SUBW
					case 0b01: c.SetReg(rs1, uint64(int64(int32(c.Reg(rs1))+int32(c.Reg(rs2)))))          // C.ADDW
					default:   return ErrIllegalInstruction
					}
				}
			}
		case 0b101: // C.J  pc += offset
			off := cjOffset(insn)
			c.pc = c.pc + uint64(off)
			return nil
		case 0b110: // C.BEQZ
			rs1 := rp((insn >> 7) & 7)
			if c.Reg(rs1) == 0 {
				c.pc = c.pc + uint64(cbOffset(insn))
				return nil
			}
		case 0b111: // C.BNEZ
			rs1 := rp((insn >> 7) & 7)
			if c.Reg(rs1) != 0 {
				c.pc = c.pc + uint64(cbOffset(insn))
				return nil
			}
		}

	// ── Quadrant 2 ────────────────────────────────────────────────────
	case 0x2:
		rd  := rf(11, 7)
		rs2 := rf(6, 2)
		switch funct3 {
		case 0b000: // C.SLLI
			shamt := uint8(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			c.SetReg(rd, c.Reg(rd)<<shamt)
		case 0b001: // C.FLDSP fd = mem[sp+uimm] (double-precision float)
			uimm := uint64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
			v, f := (&c.mem).Load64(c.Reg(2) + uimm)
			if f != nil { return f }
			c.SetFReg(rd, boxF64(v))
		case 0b010: // C.LWSP
			uimm := uint64(((insn>>12)&1)<<5 | ((insn>>4)&7)<<2 | ((insn>>2)&3)<<6)
			v, f := (&c.mem).Load32(c.Reg(2) + uimm)
			if f != nil { return f }
			c.SetReg(rd, uint64(int64(int32(v))))
		case 0b011: // C.LDSP
			uimm := uint64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
			v, f := (&c.mem).Load64(c.Reg(2) + uimm)
			if f != nil { return f }
			c.SetReg(rd, v)
		case 0b100:
			bit12 := (insn >> 12) & 1
			if bit12 == 0 {
				if rs2 == 0 { // C.JR
					if rd == 0 { return ErrIllegalInstruction }
					c.pc = c.Reg(rd) &^ 1
					return nil
				}
				// C.MV
				c.SetReg(rd, c.Reg(rs2))
			} else {
				if rd == 0 && rs2 == 0 { // C.EBREAK
					c.pc = nextPC
					return ErrEbreak
				}
				if rs2 == 0 { // C.JALR
					ret := nextPC
					c.pc = c.Reg(rd) &^ 1
					c.SetReg(1, ret)
					return nil
				}
				// C.ADD
				c.SetReg(rd, c.Reg(rd)+c.Reg(rs2))
			}
		case 0b101: // C.FSDSP mem[sp+uimm] = fs2 (double-precision float)
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
			if f := (&c.mem).Store64(c.Reg(2)+uimm, unboxF64(c.FReg(rs2))); f != nil { return f }
		case 0b110: // C.SWSP
			uimm := uint64(((insn>>9)&0xF)<<2 | ((insn>>7)&3)<<6)
			if f := (&c.mem).Store32(c.Reg(2)+uimm, uint32(c.Reg(rs2))); f != nil { return f }
		case 0b111: // C.SDSP
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
			if f := (&c.mem).Store64(c.Reg(2)+uimm, c.Reg(rs2)); f != nil { return f }
		default:
			return ErrIllegalInstruction
		}

	// end switch quad (Quadrant 2)

	default:
		return ErrIllegalInstruction
	}

	c.pc = nextPC
	return nil
}

// readCSR reads a CSR value. Returns 0 for unknown CSRs.
func (c *CPU) readCSR(addr uint32) uint64 {
	switch addr {
	case 0x001: return uint64(c.fcsr & 0x1F)          // fflags
	case 0x002: return uint64((c.fcsr >> 5) & 0x7)    // frm
	case 0x003: return uint64(c.fcsr & 0xFF)           // fcsr
	case 0x300: return c.mstatus                       // mstatus
	case 0x305: return c.mtvec                         // mtvec
	case 0x341: return c.mepc                          // mepc
	case 0x342: return c.mcause                        // mcause
	case 0x343: return c.mtval                         // mtval
	case 0xC00, 0xC02: return c.cycle                  // cycle / instret
	case 0xC01: return c.cycle                         // time (approx)
	case 0xF14: return 0                               // mhartid = 0
	}
	return 0
}

// writeCSR writes a CSR value. Ignores writes to unknown or read-only CSRs.
func (c *CPU) writeCSR(addr uint32, val uint64) {
	switch addr {
	case 0x001: c.fcsr = (c.fcsr &^ 0x1F) | uint32(val&0x1F)  // fflags
	case 0x002: c.fcsr = (c.fcsr &^ 0xE0) | uint32((val&0x7)<<5) // frm
	case 0x003: c.fcsr = uint32(val & 0xFF)                    // fcsr
	// M-mode trap CSRs
	case 0x300: c.mstatus = val                                // mstatus
	case 0x305: c.mtvec = val                                  // mtvec
	case 0x341: c.mepc = val                                   // mepc
	case 0x342: c.mcause = val                                 // mcause
	case 0x343: c.mtval = val                                  // mtval
	// CSRs written by riscv-tests reset_vector — accept silently
	case 0x105: // stvec
	case 0x180: // satp
	case 0x302: // medeleg
	case 0x303: // mideleg
	case 0x304: // mie
	case 0x3A0: // pmpcfg0
	case 0x3B0: // pmpaddr0
	case 0x744: // mnstatus (non-standard)
	// cycle/time/instret are read-only — silently ignore writes
	}
}

// amoOpW applies the AMO operation to a 32-bit word value.
func amoOpW(funct5 uint32, mem, rs2 uint32) uint32 {
	switch funct5 {
	case 0b00001: return rs2                                                       // AMOSWAP
	case 0b00000: return mem + rs2                                                 // AMOADD
	case 0b00100: return mem ^ rs2                                                 // AMOXOR
	case 0b01100: return mem & rs2                                                 // AMOAND
	case 0b01000: return mem | rs2                                                 // AMOOR
	case 0b10000: if int32(mem) < int32(rs2) { return mem }; return rs2           // AMOMIN
	case 0b10100: if int32(mem) > int32(rs2) { return mem }; return rs2           // AMOMAX
	case 0b11000: if mem < rs2 { return mem }; return rs2                         // AMOMINU
	case 0b11100: if mem > rs2 { return mem }; return rs2                         // AMOMAXU
	}
	return mem
}

// amoOpD applies the AMO operation to a 64-bit doubleword value.
func amoOpD(funct5 uint32, mem, rs2 uint64) uint64 {
	switch funct5 {
	case 0b00001: return rs2                                                              // AMOSWAP
	case 0b00000: return mem + rs2                                                        // AMOADD
	case 0b00100: return mem ^ rs2                                                        // AMOXOR
	case 0b01100: return mem & rs2                                                        // AMOAND
	case 0b01000: return mem | rs2                                                        // AMOOR
	case 0b10000: if int64(mem) < int64(rs2) { return mem }; return rs2                  // AMOMIN
	case 0b10100: if int64(mem) > int64(rs2) { return mem }; return rs2                  // AMOMAX
	case 0b11000: if mem < rs2 { return mem }; return rs2                                // AMOMINU
	case 0b11100: if mem > rs2 { return mem }; return rs2                                // AMOMAXU
	}
	return mem
}

// cjOffset extracts the sign-extended 12-bit J-type offset from a C.J instruction.
func cjOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o>>12)&1)<<11 | ((o>>11)&1)<<4 | ((o>>9)&3)<<8 | ((o>>8)&1)<<10 |
		((o>>7)&1)<<6 | ((o>>6)&1)<<7 | ((o>>3)&7)<<1 | ((o>>2)&1)<<5
	if off&(1<<11) != 0 { off |= -1 << 12 }
	return off
}

// cbOffset extracts the sign-extended 9-bit branch offset from C.BEQZ/C.BNEZ.
func cbOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o>>12)&1)<<8 | ((o>>10)&3)<<3 | ((o>>5)&3)<<6 | ((o>>3)&3)<<1 | ((o>>2)&1)<<5
	if off&(1<<8) != 0 { off |= -1 << 9 }
	return off
}

// orcB: for each byte, if any bit set -> 0xFF, else 0x00 (Zbb ORC.B)
func orcB(x uint64) uint64 {
	const mask = uint64(0x0101010101010101)
	// For each byte: set 0x80 if non-zero, then spread to all bits of that byte.
	x |= x >> 4; x |= x >> 2; x |= x >> 1
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
