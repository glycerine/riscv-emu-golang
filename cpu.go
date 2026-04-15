package riscv

import (
	"errors"
	"math"
	"math/bits"
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
	f     [32]uint64  // f0-f31: uint64 holds NaN-boxed float32 or raw float64 bits
	fcsr  uint32      // floating-point control and status register (fflags + frm)
	Notes NoteChain   // exception delivery chain; handlers installed by OS layer
	// LR/SC reservation — single address, invalidated by any SC or context switch.
	resvAddr  uint64
	resvValid bool
}

func NewCPU(mem GuestMemory) *CPU { return &CPU{mem: mem} }

func (c *CPU) SetPC(addr uint64)        { c.pc = addr }
func (c *CPU) PC() uint64               { return c.pc }
func (c *CPU) SetReg(r uint8, v uint64) { if r != 0 { c.x[r] = v } }
func (c *CPU) Reg(r uint8) uint64       { if r == 0 { return 0 }; return c.x[r] }
func (c *CPU) SetFReg(r uint8, v uint64) { c.f[r] = v }
func (c *CPU) FReg(r uint8) uint64       { return c.f[r] }
func (c *CPU) FCSR() uint32              { return c.fcsr }
func (c *CPU) SetFCSR(v uint32)          { c.fcsr = v }

// Run executes instructions until an unhandled note or fatal exception.
// Exceptions are delivered through cpu.Notes; see NoteChain and RunWithChain.
func (c *CPU) Run() error {
	return RunWithChain(c, &c.Notes)
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
		return f
	}

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
		case 0x0: // LB  — sign-extend 8→64
			u, f := (&c.mem).Load8(addr)
			if f != nil { return f }
			v = uint64(int64(int8(u)))
		case 0x1: // LH  — sign-extend 16→64
			u, f := (&c.mem).Load16(addr)
			if f != nil { return f }
			v = uint64(int64(int16(u)))
		case 0x2: // LW  — sign-extend 32→64
			u, f := (&c.mem).Load32(addr)
			if f != nil { return f }
			v = uint64(int64(int32(u)))
		case 0x3: // LD  — full 64-bit
			u, f := (&c.mem).Load64(addr)
			if f != nil { return f }
			v = u
		case 0x4: // LBU — zero-extend 8→64
			u, f := (&c.mem).Load8(addr)
			if f != nil { return f }
			v = uint64(u)
		case 0x5: // LHU — zero-extend 16→64
			u, f := (&c.mem).Load16(addr)
			if f != nil { return f }
			v = uint64(u)
		case 0x6: // LWU — zero-extend 32→64
			u, f := (&c.mem).Load32(addr)
			if f != nil { return f }
			v = uint64(u)
		default:
			return ErrIllegalInstruction
		}
		c.SetReg(rd, v)

	// ── STORE (S-type) ───────────────────────────────────────────────────
	case 0x23:
		simm := int64((insn&0xFE000000)>>20) | int64((insn>>7)&0x1F)
		addr := c.Reg(rs1) + uint64(simm)
		switch funct3 {
		case 0x0: // SB
			if f := (&c.mem).Store8(addr, uint8(c.Reg(rs2))); f != nil { return f }
		case 0x1: // SH
			if f := (&c.mem).Store16(addr, uint16(c.Reg(rs2))); f != nil { return f }
		case 0x2: // SW
			if f := (&c.mem).Store32(addr, uint32(c.Reg(rs2))); f != nil { return f }
		case 0x3: // SD
			if f := (&c.mem).Store64(addr, c.Reg(rs2)); f != nil { return f }
		default:
			return ErrIllegalInstruction
		}

	// ── OP-IMM (I-type) ──────────────────────────────────────────────────
	case 0x13:
		shamt := uint8(insn >> 20) & 0x3F // for shifts
		var v uint64
		switch funct3 {
		case 0x0: // ADDI
			v = c.Reg(rs1) + uint64(iimm)
		case 0x1: // SLLI
			v = c.Reg(rs1) << shamt
		case 0x2: // SLTI
			if int64(c.Reg(rs1)) < iimm { v = 1 }
		case 0x3: // SLTIU
			if c.Reg(rs1) < uint64(iimm) { v = 1 }
		case 0x4: // XORI
			v = c.Reg(rs1) ^ uint64(iimm)
		case 0x5:
			if (insn>>30)&1 == 1 { // SRAI
				v = uint64(int64(c.Reg(rs1)) >> shamt)
			} else { // SRLI
				v = c.Reg(rs1) >> shamt
			}
		case 0x6: // ORI
			v = c.Reg(rs1) | uint64(iimm)
		case 0x7: // ANDI
			v = c.Reg(rs1) & uint64(iimm)
		}
		c.SetReg(rd, v)

	// ── OP-IMM-32 (I-type, 32-bit ops, sign-extend result) ───────────────
	case 0x1B:
		shamt := uint8(insn >> 20) & 0x1F
		var v int32
		switch funct3 {
		case 0x0: // ADDIW
			v = int32(c.Reg(rs1)) + int32(iimm)
		case 0x1: // SLLIW
			v = int32(c.Reg(rs1)) << shamt
		case 0x5:
			if (insn>>30)&1 == 1 { // SRAIW
				v = int32(c.Reg(rs1)) >> shamt
			} else { // SRLIW
				v = int32(uint32(c.Reg(rs1)) >> shamt)
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
		if funct7 == 0x01 { // ── RV64M ──────────────────────────────────
			switch funct3 {
			case 0x0: v = a * b                                          // MUL
			case 0x1: // MULH: signed × signed, upper 64 bits
				hi, _ := bits.Mul64(a, b)
				// Adjust for signed: if rs1<0 subtract rs2; if rs2<0 subtract rs1
				if int64(a) < 0 { hi -= b }
				if int64(b) < 0 { hi -= a }
				v = hi
			case 0x2: // MULHSU: signed rs1 × unsigned rs2, upper 64 bits
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 { hi -= b }
				v = hi
			case 0x3: // MULHU: unsigned × unsigned, upper 64 bits
				hi, _ := bits.Mul64(a, b)
				v = hi
			case 0x4: // DIV: signed division
				if b == 0 {
					v = ^uint64(0) // -1
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					v = a // overflow: INT_MIN / -1 = INT_MIN
				} else {
					v = uint64(int64(a) / int64(b))
				}
			case 0x5: // DIVU: unsigned division
				if b == 0 { v = ^uint64(0) } else { v = a / b }
			case 0x6: // REM: signed remainder
				if b == 0 {
					v = a
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					v = 0
				} else {
					v = uint64(int64(a) % int64(b))
				}
			case 0x7: // REMU: unsigned remainder
				if b == 0 { v = a } else { v = a % b }
			}
		} else { // ── RV64I ──────────────────────────────────────────────
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
	case 0x3B:
		funct7 := insn >> 25
		a32, b32 := uint32(c.Reg(rs1)), uint32(c.Reg(rs2))
		var v int32
		if funct7 == 0x01 { // ── RV64M word ops ─────────────────────────
			switch funct3 {
			case 0x0: v = int32(a32 * b32)                          // MULW
			case 0x4: // DIVW: signed 32-bit division
				if b32 == 0 {
					v = -1
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					v = int32(a32) // overflow: INT32_MIN / -1 = INT32_MIN
				} else {
					v = int32(a32) / int32(b32)
				}
			case 0x5: // DIVUW: unsigned 32-bit division
				if b32 == 0 { v = -1 } else { v = int32(a32 / b32) }
			case 0x6: // REMW: signed 32-bit remainder
				if b32 == 0 {
					v = int32(a32)
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					v = 0
				} else {
					v = int32(a32) % int32(b32)
				}
			case 0x7: // REMUW: unsigned 32-bit remainder
				if b32 == 0 { v = int32(a32) } else { v = int32(a32 % b32) }
			default:
				return ErrIllegalInstruction
			}
		} else { // ── RV64I word ops ─────────────────────────────────────
			switch funct3 {
			case 0x0:
				if funct7 == 0x20 { v = int32(a32 - b32) } else { v = int32(a32 + b32) } // SUBW/ADDW
			case 0x1: v = int32(a32 << (b32 & 0x1F))                                  // SLLW
			case 0x5:
				if funct7 == 0x20 { v = int32(a32) >> (b32 & 0x1F) } else { v = int32(a32 >> (b32 & 0x1F)) } // SRAW/SRLW
			default:
				return ErrIllegalInstruction
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
	case 0x73:
		switch insn >> 20 {
		case 0x001: // EBREAK
			c.pc = nextPC
			return ErrEbreak
		case 0x000: // ECALL
			c.pc = nextPC
			return ErrEcall
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
			if f := (&c.mem).Store32(addr, unboxF32(c.FReg(rs2))); f != nil { return f }
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
			var v float32
			switch opcode {
			case 0x43: v = a*b + d           // FMADD.S
			case 0x47: v = a*b - d           // FMSUB.S
			case 0x4B: v = -(a*b) + d        // FNMSUB.S
			case 0x4F: v = -(a*b) - d        // FNMADD.S
			}
			c.SetFReg(rd, boxF32(f32bits(v)))
		} else if fmt == 1 { // .D double-precision
			a := f64frombits(c.FReg(rs1))
			b := f64frombits(c.FReg(rs2))
			d := f64frombits(c.FReg(rs3))
			var v float64
			switch opcode {
			case 0x43: v = a*b + d
			case 0x47: v = a*b - d
			case 0x4B: v = -(a*b) + d
			case 0x4F: v = -(a*b) - d
			}
			c.SetFReg(rd, boxF64(f64bits(v)))
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
			case 0x00: c.SetFReg(rd, boxF32(f32bits(af+bf)))          // FADD.S
			case 0x01: c.SetFReg(rd, boxF32(f32bits(af-bf)))          // FSUB.S
			case 0x02: c.SetFReg(rd, boxF32(f32bits(af*bf)))          // FMUL.S
			case 0x03: c.SetFReg(rd, boxF32(f32bits(af/bf)))          // FDIV.S
			case 0x0B: c.SetFReg(rd, boxF32(f32bits(float32(math.Sqrt(float64(af)))))) // FSQRT.S
			case 0x04: // FSGNJ.S / FSGNJN.S / FSGNJX.S
				switch funct3 {
				case 0: c.SetFReg(rd, boxF32(fsgnjF32(a,b)))
				case 1: c.SetFReg(rd, boxF32(fsgnjnF32(a,b)))
				case 2: c.SetFReg(rd, boxF32(fsgnjxF32(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x05: // FMIN.S / FMAX.S
				switch funct3 {
				case 0: c.SetFReg(rd, boxF32(fminF32(a,b)))
				case 1: c.SetFReg(rd, boxF32(fmaxF32(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x08: // FCVT.S.D  (rs2=1 = from D)
				c.SetFReg(rd, boxF32(f32bits(float32(f64frombits(c.FReg(rs1))))))
			case 0x14: // FEQ.S / FLT.S / FLE.S -> integer rd
				var v uint64
				switch funct3 {
				case 2: if af == bf { v = 1 }
				case 1: if af < bf { v = 1 }
				case 0: if af <= bf { v = 1 }
				default: return ErrIllegalInstruction
				}
				c.SetReg(rd, v)
			case 0x18: // FCVT.{W,WU,L,LU}.S -> integer rd
				switch rs2 {
				case 0: c.SetReg(rd, uint64(int64(int32(af))))           // FCVT.W.S
				case 1: c.SetReg(rd, uint64(uint32(af)))                 // FCVT.WU.S
				case 2: c.SetReg(rd, uint64(int64(af)))                  // FCVT.L.S
				case 3: c.SetReg(rd, uint64(af))                         // FCVT.LU.S
				}
			case 0x1A: // FCVT.S.{W,WU,L,LU} <- integer rs1
				switch rs2 {
				case 0: c.SetFReg(rd, boxF32(f32bits(float32(int32(c.Reg(rs1))))))  // FCVT.S.W
				case 1: c.SetFReg(rd, boxF32(f32bits(float32(uint32(c.Reg(rs1)))))) // FCVT.S.WU
				case 2: c.SetFReg(rd, boxF32(f32bits(float32(int64(c.Reg(rs1))))))  // FCVT.S.L
				case 3: c.SetFReg(rd, boxF32(f32bits(float32(c.Reg(rs1)))))          // FCVT.S.LU
				}
			case 0x1C: // FMV.X.W (funct3=0) / FCLASS.S (funct3=1)
				switch funct3 {
				case 0: c.SetReg(rd, uint64(int64(int32(a))))            // FMV.X.W sign-extend
				case 1: c.SetReg(rd, fclassF32(a))                       // FCLASS.S
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
			case 0x00: c.SetFReg(rd, boxF64(f64bits(af+bf)))          // FADD.D
			case 0x01: c.SetFReg(rd, boxF64(f64bits(af-bf)))          // FSUB.D
			case 0x02: c.SetFReg(rd, boxF64(f64bits(af*bf)))          // FMUL.D
			case 0x03: c.SetFReg(rd, boxF64(f64bits(af/bf)))          // FDIV.D
			case 0x0B: c.SetFReg(rd, boxF64(f64bits(math.Sqrt(af))))  // FSQRT.D
			case 0x04: // FSGNJ.D / FSGNJN.D / FSGNJX.D
				switch funct3 {
				case 0: c.SetFReg(rd, boxF64(fsgnjF64(a,b)))
				case 1: c.SetFReg(rd, boxF64(fsgnjnF64(a,b)))
				case 2: c.SetFReg(rd, boxF64(fsgnjxF64(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x05: // FMIN.D / FMAX.D
				switch funct3 {
				case 0: c.SetFReg(rd, boxF64(fminF64(a,b)))
				case 1: c.SetFReg(rd, boxF64(fmaxF64(a,b)))
				default: return ErrIllegalInstruction
				}
			case 0x08: // FCVT.D.S  (rs2=0 = from S)
				c.SetFReg(rd, boxF64(f64bits(float64(f32frombits(unboxF32(c.FReg(rs1)))))))
			case 0x14: // FEQ.D / FLT.D / FLE.D
				var v uint64
				switch funct3 {
				case 2: if af == bf { v = 1 }
				case 1: if af < bf { v = 1 }
				case 0: if af <= bf { v = 1 }
				default: return ErrIllegalInstruction
				}
				c.SetReg(rd, v)
			case 0x18: // FCVT.{W,WU,L,LU}.D
				switch rs2 {
				case 0: c.SetReg(rd, uint64(int64(int32(af))))
				case 1: c.SetReg(rd, uint64(uint32(af)))
				case 2: c.SetReg(rd, uint64(int64(af)))
				case 3: c.SetReg(rd, uint64(af))
				}
			case 0x1A: // FCVT.D.{W,WU,L,LU}
				switch rs2 {
				case 0: c.SetFReg(rd, boxF64(f64bits(float64(int32(c.Reg(rs1))))))
				case 1: c.SetFReg(rd, boxF64(f64bits(float64(uint32(c.Reg(rs1))))))
				case 2: c.SetFReg(rd, boxF64(f64bits(float64(int64(c.Reg(rs1))))))
				case 3: c.SetFReg(rd, boxF64(f64bits(float64(c.Reg(rs1)))))
				}
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
		case 0b011: // C.LD  rd'= mem[rs1'+uimm]
			uimm := uint64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
			v, f := (&c.mem).Load64(c.Reg(rs1) + uimm)
			if f != nil { return f }
			c.SetReg(rd, v)
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
