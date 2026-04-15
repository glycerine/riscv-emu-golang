package riscv

import "errors"

var ErrEbreak = errors.New("ebreak")
var ErrIllegalInstruction = errors.New("illegal instruction")

// CPU is a single RV64I hart.
// mem is inline and first for cache locality — touched on every instruction.
type CPU struct {
	mem GuestMemory
	pc  uint64
	x   [32]uint64 // x[0] is hardwired zero
}

func NewCPU(mem GuestMemory) *CPU { return &CPU{mem: mem} }

func (c *CPU) SetPC(addr uint64)        { c.pc = addr }
func (c *CPU) PC() uint64               { return c.pc }
func (c *CPU) SetReg(r uint8, v uint64) { if r != 0 { c.x[r] = v } }
func (c *CPU) Reg(r uint8) uint64       { if r == 0 { return 0 }; return c.x[r] }

func (c *CPU) Run() error {
	for {
		if err := c.step(); err != nil {
			return err
		}
	}
}

func (c *CPU) step() error {
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
		c.SetReg(rd, v)

	// ── OP-32 (R-type, 32-bit, sign-extend) ─────────────────────────────
	case 0x3B:
		funct7 := insn >> 25
		a, b := uint32(c.Reg(rs1)), uint32(c.Reg(rs2))
		var v int32
		switch funct3 {
		case 0x0:
			if funct7 == 0x20 { v = int32(a - b) } else { v = int32(a + b) } // SUBW/ADDW
		case 0x1: v = int32(a << (b & 0x1F))                                  // SLLW
		case 0x5:
			if funct7 == 0x20 { v = int32(a) >> (b & 0x1F) } else { v = int32(a >> (b & 0x1F)) } // SRAW/SRLW
		default:
			return ErrIllegalInstruction
		}
		c.SetReg(rd, uint64(int64(v)))

	// ── LUI (U-type) ─────────────────────────────────────────────────────
	case 0x37:
		c.SetReg(rd, uint64(int64(int32(insn&0xFFFFF000))))

	// ── AUIPC (U-type) ───────────────────────────────────────────────────
	case 0x17:
		c.SetReg(rd, c.pc+uint64(int64(int32(insn&0xFFFFF000))))

	// ── JAL (J-type) ─────────────────────────────────────────────────────
	case 0x6F:
		jimm := int64(int32(
			((insn>>31)&1)<<20 |
			((insn>>12)&0xFF)<<12 |
			((insn>>20)&1)<<11 |
			((insn>>21)&0x3FF)<<1)) >> 11 // sign-extend 21→64
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
		case 0x000: // ECALL — not yet implemented
			return ErrIllegalInstruction
		default:
			return ErrIllegalInstruction
		}

	default:
		return ErrIllegalInstruction
	}

	c.pc = nextPC
	return nil
}
