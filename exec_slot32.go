package riscv

import (
	"math/bits"
	"unsafe"
)

// exec32Slot executes a pre-decoded 32-bit RV64 instruction from slot fields.
// Covers the common RV64I / RV64M opcodes — LOAD, STORE, OP-IMM, OP,
// OP-IMM-32, OP-32, BRANCH, JAL, JALR, LUI, AUIPC, FENCE. Complex cases
// (SYSTEM, AMO, FP, Zb* extensions with non-zero funct7) fall back to
// stepFromInsn which does the full decode+execute from raw bits.
//
// pc is passed in rather than read from c.pc. Returns the new pc — pc+4 for
// non-branches, the target for taken branches / JAL / JALR. Callers (RunCached)
// keep pc in a local and only write cpu.pc when the driver's inner loop exits.
// Fault cases return (pc, err) so the driver can record the faulting PC.
//
//go:nosplit
func (c *CPU) exec32Slot(slot *DecodedInsn, pc uint64) (uint64, error) {
	nextPC := pc + 4

	switch slot.op {

	case 0x03: // LOAD (I-type)
		addr := c.x[slot.rs1] + uint64(int64(slot.imm))
		var v uint64
		switch slot.funct3 {
		case 0x0: // LB
			u, f := (&c.mem).Load8(addr)
			if f != nil {
				return pc, f
			}
			v = uint64(int64(int8(u)))
		case 0x1: // LH
			u, f := (&c.mem).Load16(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = (&c.mem).Load16U(addr)
			}
			if f != nil {
				return pc, f
			}
			v = uint64(int64(int16(u)))
		case 0x2: // LW
			u, f := (&c.mem).Load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = (&c.mem).Load32U(addr)
			}
			if f != nil {
				return pc, f
			}
			v = uint64(int64(int32(u)))
		case 0x3: // LD — inline aligned fast path (Phase D)
			if addr&7 == 0 && (addr|(addr+7))&^c.mem.mask == 0 {
				v = *(*uint64)(unsafe.Add(c.mem.base, addr&c.mem.mask))
			} else {
				u, f := (&c.mem).Load64U(addr)
				if f != nil {
					return pc, f
				}
				v = u
			}
		case 0x4: // LBU
			u, f := (&c.mem).Load8(addr)
			if f != nil {
				return pc, f
			}
			v = uint64(u)
		case 0x5: // LHU
			u, f := (&c.mem).Load16(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = (&c.mem).Load16U(addr)
			}
			if f != nil {
				return pc, f
			}
			v = uint64(u)
		case 0x6: // LWU
			u, f := (&c.mem).Load32(addr)
			if f != nil && f.Kind == FaultMisalign {
				u, f = (&c.mem).Load32U(addr)
			}
			if f != nil {
				return pc, f
			}
			v = uint64(u)
		default:
			return pc, ErrIllegalInstruction
		}
		c.x[slot.rd] = v
		c.x[0] = 0

	case 0x23: // STORE (S-type)
		addr := c.x[slot.rs1] + uint64(int64(slot.imm))
		switch slot.funct3 {
		case 0x0: // SB
			if f := (&c.mem).Store8(addr, uint8(c.x[slot.rs2])); f != nil {
				return pc, f
			}
		case 0x1: // SH
			f := (&c.mem).Store16(addr, uint16(c.x[slot.rs2]))
			if f != nil && f.Kind == FaultMisalign {
				f = (&c.mem).Store16U(addr, uint16(c.x[slot.rs2]))
			}
			if f != nil {
				return pc, f
			}
		case 0x2: // SW
			f := (&c.mem).Store32(addr, uint32(c.x[slot.rs2]))
			if f != nil && f.Kind == FaultMisalign {
				f = (&c.mem).Store32U(addr, uint32(c.x[slot.rs2]))
			}
			if f != nil {
				return pc, f
			}
		case 0x3: // SD — inline aligned fast path (Phase D)
			if addr&7 == 0 && (addr|(addr+7))&^c.mem.mask == 0 {
				*(*uint64)(unsafe.Add(c.mem.base, addr&c.mem.mask)) = c.x[slot.rs2]
			} else {
				if f := (&c.mem).Store64U(addr, c.x[slot.rs2]); f != nil {
					return pc, f
				}
			}
		default:
			return pc, ErrIllegalInstruction
		}

	case 0x13: // OP-IMM (I-type)
		// Fast path covers the ADDI/SLTI/XORI/ORI/ANDI and simple shifts.
		// Zbs/Zbb variants (funct7 != 0/0x20 for shifts, or sub-funct3=0x5)
		// fall back to stepFromInsn for full decoding.
		a := c.x[slot.rs1]
		imm := uint64(int64(slot.imm))
		switch slot.funct3 {
		case 0x0: // ADDI
			c.x[slot.rd] = a + imm
		case 0x2: // SLTI
			if int64(a) < int64(slot.imm) {
				c.x[slot.rd] = 1
			} else {
				c.x[slot.rd] = 0
			}
		case 0x3: // SLTIU
			if a < imm {
				c.x[slot.rd] = 1
			} else {
				c.x[slot.rd] = 0
			}
		case 0x4: // XORI
			c.x[slot.rd] = a ^ imm
		case 0x6: // ORI
			c.x[slot.rd] = a | imm
		case 0x7: // ANDI
			c.x[slot.rd] = a & imm
		case 0x1: // SLLI (funct7 == 0 fast path)
			if slot.funct7&^1 == 0 {
				shamt := uint(slot.insn>>20) & 0x3F
				c.x[slot.rd] = a << shamt
			} else {
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		case 0x5: // SRLI/SRAI fast path
			shamt := uint(slot.insn>>20) & 0x3F
			switch slot.funct7 &^ 1 {
			case 0x00: // SRLI
				c.x[slot.rd] = a >> shamt
			case 0x20: // SRAI
				c.x[slot.rd] = uint64(int64(a) >> shamt)
			default:
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		default:
			c.pc = pc
			err := c.stepFromInsn(slot.insn)
			return c.pc, err
		}
		c.x[0] = 0

	case 0x1B: // OP-IMM-32
		a := uint32(c.x[slot.rs1])
		switch slot.funct3 {
		case 0x0: // ADDIW
			v := int32(a) + slot.imm
			c.x[slot.rd] = uint64(int64(v))
		case 0x1: // SLLIW (funct7 == 0)
			if slot.funct7 == 0 {
				shamt := uint(slot.insn>>20) & 0x1F
				c.x[slot.rd] = uint64(int64(int32(a << shamt)))
			} else {
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		case 0x5: // SRLIW/SRAIW fast path
			shamt := uint(slot.insn>>20) & 0x1F
			switch slot.funct7 {
			case 0x00: // SRLIW
				c.x[slot.rd] = uint64(int64(int32(a >> shamt)))
			case 0x20: // SRAIW
				c.x[slot.rd] = uint64(int64(int32(a) >> shamt))
			default:
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		default:
			c.pc = pc
			err := c.stepFromInsn(slot.insn)
			return c.pc, err
		}
		c.x[0] = 0

	case 0x33: // OP (R-type)
		a := c.x[slot.rs1]
		b := c.x[slot.rs2]
		switch slot.funct7 {
		case 0x00: // ADD / SLL / SLT / SLTU / XOR / SRL / OR / AND
			switch slot.funct3 {
			case 0x0:
				c.x[slot.rd] = a + b
			case 0x1:
				c.x[slot.rd] = a << (b & 0x3F)
			case 0x2:
				if int64(a) < int64(b) {
					c.x[slot.rd] = 1
				} else {
					c.x[slot.rd] = 0
				}
			case 0x3:
				if a < b {
					c.x[slot.rd] = 1
				} else {
					c.x[slot.rd] = 0
				}
			case 0x4:
				c.x[slot.rd] = a ^ b
			case 0x5:
				c.x[slot.rd] = a >> (b & 0x3F)
			case 0x6:
				c.x[slot.rd] = a | b
			case 0x7:
				c.x[slot.rd] = a & b
			}
		case 0x20: // SUB / SRA
			switch slot.funct3 {
			case 0x0:
				c.x[slot.rd] = a - b
			case 0x5:
				c.x[slot.rd] = uint64(int64(a) >> (b & 0x3F))
			default:
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		case 0x01: // RV64M
			switch slot.funct3 {
			case 0x0:
				c.x[slot.rd] = a * b
			case 0x1:
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 {
					hi -= b
				}
				if int64(b) < 0 {
					hi -= a
				}
				c.x[slot.rd] = hi
			case 0x2:
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 {
					hi -= b
				}
				c.x[slot.rd] = hi
			case 0x3:
				hi, _ := bits.Mul64(a, b)
				c.x[slot.rd] = hi
			case 0x4:
				if b == 0 {
					c.x[slot.rd] = ^uint64(0)
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					c.x[slot.rd] = a
				} else {
					c.x[slot.rd] = uint64(int64(a) / int64(b))
				}
			case 0x5:
				if b == 0 {
					c.x[slot.rd] = ^uint64(0)
				} else {
					c.x[slot.rd] = a / b
				}
			case 0x6:
				if b == 0 {
					c.x[slot.rd] = a
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					c.x[slot.rd] = 0
				} else {
					c.x[slot.rd] = uint64(int64(a) % int64(b))
				}
			case 0x7:
				if b == 0 {
					c.x[slot.rd] = a
				} else {
					c.x[slot.rd] = a % b
				}
			}
		default:
			c.pc = pc
			err := c.stepFromInsn(slot.insn)
			return c.pc, err
		}
		c.x[0] = 0

	case 0x3B: // OP-32 (RV64I word ops + RV64M word ops)
		a32 := uint32(c.x[slot.rs1])
		b32 := uint32(c.x[slot.rs2])
		switch slot.funct7 {
		case 0x00:
			switch slot.funct3 {
			case 0x0: // ADDW
				c.x[slot.rd] = uint64(int64(int32(a32 + b32)))
			case 0x1: // SLLW
				c.x[slot.rd] = uint64(int64(int32(a32 << (b32 & 0x1F))))
			case 0x5: // SRLW
				c.x[slot.rd] = uint64(int64(int32(a32 >> (b32 & 0x1F))))
			default:
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		case 0x20:
			switch slot.funct3 {
			case 0x0: // SUBW
				c.x[slot.rd] = uint64(int64(int32(a32 - b32)))
			case 0x5: // SRAW
				c.x[slot.rd] = uint64(int64(int32(a32) >> (b32 & 0x1F)))
			default:
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		case 0x01: // RV64M word ops
			switch slot.funct3 {
			case 0x0: // MULW
				c.x[slot.rd] = uint64(int64(int32(a32 * b32)))
			case 0x4: // DIVW
				if b32 == 0 {
					c.x[slot.rd] = ^uint64(0)
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					c.x[slot.rd] = uint64(int64(int32(a32)))
				} else {
					c.x[slot.rd] = uint64(int64(int32(a32) / int32(b32)))
				}
			case 0x5: // DIVUW
				if b32 == 0 {
					c.x[slot.rd] = ^uint64(0)
				} else {
					c.x[slot.rd] = uint64(int64(int32(a32 / b32)))
				}
			case 0x6: // REMW
				if b32 == 0 {
					c.x[slot.rd] = uint64(int64(int32(a32)))
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					c.x[slot.rd] = 0
				} else {
					c.x[slot.rd] = uint64(int64(int32(a32) % int32(b32)))
				}
			case 0x7: // REMUW
				if b32 == 0 {
					c.x[slot.rd] = uint64(int64(int32(a32)))
				} else {
					c.x[slot.rd] = uint64(int64(int32(a32 % b32)))
				}
			default:
				c.pc = pc
				err := c.stepFromInsn(slot.insn)
				return c.pc, err
			}
		default:
			c.pc = pc
			err := c.stepFromInsn(slot.insn)
			return c.pc, err
		}
		c.x[0] = 0

	case 0x63: // BRANCH (B-type)
		a := c.x[slot.rs1]
		b := c.x[slot.rs2]
		taken := false
		switch slot.funct3 {
		case 0x0: // BEQ
			taken = a == b
		case 0x1: // BNE
			taken = a != b
		case 0x4: // BLT
			taken = int64(a) < int64(b)
		case 0x5: // BGE
			taken = int64(a) >= int64(b)
		case 0x6: // BLTU
			taken = a < b
		case 0x7: // BGEU
			taken = a >= b
		default:
			return pc, ErrIllegalInstruction
		}
		if taken {
			return pc + uint64(int64(slot.imm)), nil
		}

	case 0x6F: // JAL
		c.x[slot.rd] = nextPC
		c.x[0] = 0
		return pc + uint64(int64(slot.imm)), nil

	case 0x67: // JALR
		target := (c.x[slot.rs1] + uint64(int64(slot.imm))) &^ 1
		c.x[slot.rd] = nextPC
		c.x[0] = 0
		return target, nil

	case 0x37: // LUI
		c.x[slot.rd] = uint64(int64(slot.imm))
		c.x[0] = 0

	case 0x17: // AUIPC
		c.x[slot.rd] = pc + uint64(int64(slot.imm))
		c.x[0] = 0

	case 0x0F: // FENCE / FENCE.I — no-op for single-threaded emulator
		// nothing to do

	default:
		// SYSTEM (0x73), AMO (0x2F), LOAD-FP (0x07), STORE-FP (0x27),
		// FMA family (0x43..0x4F), OP-FP (0x53), and any Zb* corner cases
		// with unusual funct7 fall through to the full interpreter.
		c.pc = pc
		err := c.stepFromInsn(slot.insn)
		return c.pc, err
	}

	return nextPC, nil
}
