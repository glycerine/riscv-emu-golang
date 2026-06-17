package riscv

import "unsafe"

// execRVCSlot executes a pre-decoded RVC instruction. Semantically identical
// to stepRVC(uint16(slot.insn)), but reads pre-computed fields from slot
// instead of re-extracting them from the raw bits on every visit.
//
// pc is passed in rather than read from c.pc — the driver already has it in a
// register. Returns the new pc (either pc+2 for non-branches or the branch
// target) so the driver never has to reload c.pc in the hot path. c.pc is
// only written by the driver when it exits the inner loop (for watchAddr /
// fault delivery) or by the stepRVC fallback for uncommon classes.
//
// Returns errFallback flow: when slot.op == opFallback or an uncommon class
// lands in default, we set c.pc = pc, invoke c.stepRVC, and propagate its
// updated c.pc back as the new pc.
//
//go:nosplit
func (c *CPU) execRVCSlot(slot *DecodedInsn, pc uint64) (uint64, error) {
	nextPC := pc + 2
	switch slot.op {

	case opC_ADDI: // C.ADDI rd = rd + imm (rd=0 is C.NOP — write is squashed by the x0=0 trick)
		c.x[slot.rd] = c.x[slot.rd] + uint64(int64(slot.imm))
		c.x[0] = 0

	case opC_ADDIW: // C.ADDIW: rd != 0 by ISA; rd = sign_ext32(rd + imm)
		if slot.rd == 0 {
			return pc, ErrIllegalInstruction
		}
		c.x[slot.rd] = uint64(int64(int32(c.x[slot.rd]) + slot.imm))

	case opC_LI: // C.LI rd = imm; rd=0 is HINT — we still write then zero x0
		c.x[slot.rd] = uint64(int64(slot.imm))
		c.x[0] = 0

	case opC_LUI_OR_ADDI16SP:
		if slot.rd == 2 { // C.ADDI16SP
			if slot.imm == 0 {
				return pc, ErrIllegalInstruction
			}
			c.x[2] = c.x[2] + uint64(int64(slot.imm))
		} else { // C.LUI
			if slot.rd == 0 || slot.imm == 0 {
				return pc, ErrIllegalInstruction
			}
			c.x[slot.rd] = uint64(int64(slot.imm))
		}

	case opC_ADDI4SPN: // C.ADDI4SPN rd' = sp + nzuimm*4
		if slot.imm == 0 {
			return pc, ErrIllegalInstruction
		}
		c.x[slot.rd] = c.x[2] + uint64(slot.imm)

	case opC_LW:
		v, f := (&c.mem).Load32(c.x[slot.rs1] + uint64(slot.imm))
		if f != nil {
			return pc, f
		}
		c.x[slot.rd] = uint64(int64(int32(v)))

	case opC_LD:
		// C.LD immediate is already a multiple of 8, so the base address
		// is naturally aligned as long as rs1 is. Inline fast path.
		addr := c.x[slot.rs1] + uint64(slot.imm)
		if c.mem.accessOverlay == nil && addr&7 == 0 && (addr|(addr+7))&^c.mem.mask == 0 {
			c.x[slot.rd] = *(*uint64)(unsafe.Add(c.mem.base, addr&c.mem.mask))
		} else {
			v, f := (&c.mem).Load64U(addr)
			if f != nil {
				return pc, f
			}
			c.x[slot.rd] = v
		}

	case opC_SW:
		if f := (&c.mem).Store32(c.x[slot.rs1]+uint64(slot.imm), uint32(c.x[slot.rs2])); f != nil {
			return pc, f
		}

	case opC_SD:
		addr := c.x[slot.rs1] + uint64(slot.imm)
		if c.mem.accessOverlay == nil && addr&7 == 0 && (addr|(addr+7))&^c.mem.mask == 0 {
			*(*uint64)(unsafe.Add(c.mem.base, addr&c.mem.mask)) = c.x[slot.rs2]
		} else {
			if f := (&c.mem).Store64U(addr, c.x[slot.rs2]); f != nil {
				return pc, f
			}
		}

	case opC_LWSP:
		if slot.rd == 0 {
			return pc, ErrIllegalInstruction
		}
		v, f := (&c.mem).Load32(c.x[2] + uint64(slot.imm))
		if f != nil {
			return pc, f
		}
		c.x[slot.rd] = uint64(int64(int32(v)))

	case opC_LDSP:
		if slot.rd == 0 {
			return pc, ErrIllegalInstruction
		}
		addr := c.x[2] + uint64(slot.imm)
		if c.mem.accessOverlay == nil && addr&7 == 0 && (addr|(addr+7))&^c.mem.mask == 0 {
			c.x[slot.rd] = *(*uint64)(unsafe.Add(c.mem.base, addr&c.mem.mask))
		} else {
			v, f := (&c.mem).Load64U(addr)
			if f != nil {
				return pc, f
			}
			c.x[slot.rd] = v
		}

	case opC_SWSP:
		if f := (&c.mem).Store32(c.x[2]+uint64(slot.imm), uint32(c.x[slot.rs2])); f != nil {
			return pc, f
		}

	case opC_SDSP:
		addr := c.x[2] + uint64(slot.imm)
		if c.mem.accessOverlay == nil && addr&7 == 0 && (addr|(addr+7))&^c.mem.mask == 0 {
			*(*uint64)(unsafe.Add(c.mem.base, addr&c.mem.mask)) = c.x[slot.rs2]
		} else {
			if f := (&c.mem).Store64U(addr, c.x[slot.rs2]); f != nil {
				return pc, f
			}
		}

	case opC_SLLI:
		if slot.rd == 0 {
			return pc, ErrIllegalInstruction
		}
		c.x[slot.rd] = c.x[slot.rd] << uint(slot.imm)

	case opC_J:
		return pc + uint64(int64(slot.imm)), nil

	case opC_BEQZ:
		if c.x[slot.rs1] == 0 {
			return pc + uint64(int64(slot.imm)), nil
		}

	case opC_BNEZ:
		if c.x[slot.rs1] != 0 {
			return pc + uint64(int64(slot.imm)), nil
		}

	case opC_MV: // C.MV: rd = rs2. By ISA, rd != 0 and rs2 != 0 for this
		// encoding (rs2 == 0 would be C.JR, handled separately), so no
		// x[0]=0 fixup needed.
		c.x[slot.rd] = c.x[slot.rs2]

	case opC_ADD: // C.ADD: rd = rd + rs2. Same rd != 0 / rs2 != 0 invariant.
		c.x[slot.rd] = c.x[slot.rd] + c.x[slot.rs2]

	case opC_JR: // C.JR: pc = rd & ~1; rd != 0 by decode
		return c.x[slot.rd] &^ 1, nil

	case opC_JALR: // C.JALR: x1 = nextPC; pc = rd & ~1; rd != 0 by decode
		target := c.x[slot.rd] &^ 1
		c.x[1] = nextPC
		return target, nil

	case opC_EBREAK:
		c.setTrap(CauseBreakpoint, 2)
		return pc, ErrEbreak

	case opC_MISC_ALU: // funct3=100 q1: SRLI / SRAI / ANDI / SUB / XOR / OR / AND / SUBW / ADDW
		insn := uint16(slot.insn)
		funct2 := (insn >> 10) & 3
		rs1p := 8 + uint8((insn>>7)&7)
		rs2p := 8 + uint8((insn>>2)&7)
		bit12 := (insn >> 12) & 1
		switch funct2 {
		case 0b00: // C.SRLI
			shamt := uint8(bit12<<5 | (insn>>2)&0x1F)
			c.x[rs1p] = c.x[rs1p] >> shamt
		case 0b01: // C.SRAI
			shamt := uint8(bit12<<5 | (insn>>2)&0x1F)
			c.x[rs1p] = uint64(int64(c.x[rs1p]) >> shamt)
		case 0b10: // C.ANDI
			imm6 := int32((insn >> 2) & 0x1F)
			if bit12 != 0 {
				imm6 |= ^0x1F
			}
			c.x[rs1p] = c.x[rs1p] & uint64(int64(imm6))
		case 0b11:
			op := (insn >> 5) & 3
			if bit12 == 0 {
				switch op {
				case 0b00:
					c.x[rs1p] = c.x[rs1p] - c.x[rs2p]
				case 0b01:
					c.x[rs1p] = c.x[rs1p] ^ c.x[rs2p]
				case 0b10:
					c.x[rs1p] = c.x[rs1p] | c.x[rs2p]
				case 0b11:
					c.x[rs1p] = c.x[rs1p] & c.x[rs2p]
				}
			} else {
				switch op {
				case 0b00:
					c.x[rs1p] = uint64(int64(int32(c.x[rs1p]) - int32(c.x[rs2p])))
				case 0b01:
					c.x[rs1p] = uint64(int64(int32(c.x[rs1p]) + int32(c.x[rs2p])))
				default:
					return pc, ErrIllegalInstruction
				}
			}
		}
		// rs1p is always in x8..x15 (decoded from 3-bit rs1' + 8), so no
		// x[0]=0 fixup is needed.

	default:
		// Complex/FP/uncommon paths — defer to the canonical stepRVC body.
		// stepRVC reads/writes c.pc, so synchronize around the call.
		c.pc = pc
		err := c.stepRVC(uint16(slot.insn))
		return c.pc, err
	}

	return nextPC, nil
}
