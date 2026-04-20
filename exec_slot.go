package riscv

// execRVCSlot executes a pre-decoded RVC instruction. Semantically identical
// to stepRVC(uint16(slot.insn)), but reads pre-computed fields from slot
// instead of re-extracting them from the raw bits on every visit.
//
// Returns errFallback if slot.op == opFallback (meaning decodeRVC did not
// produce a dispatchable class — the caller should invoke stepRVC as a
// fallback for full coverage).
//
//go:nosplit
func (c *CPU) execRVCSlot(slot *DecodedInsn) error {
	nextPC := c.pc + 2
	switch slot.op {

	case opC_ADDI: // C.ADDI rd = rd + imm (rd=0 is C.NOP — write is squashed by the x0=0 trick)
		c.x[slot.rd] = c.x[slot.rd] + uint64(int64(slot.imm))
		c.x[0] = 0

	case opC_ADDIW: // C.ADDIW: rd != 0 by ISA; rd = sign_ext32(rd + imm)
		if slot.rd == 0 {
			return ErrIllegalInstruction
		}
		c.x[slot.rd] = uint64(int64(int32(c.x[slot.rd]) + slot.imm))

	case opC_LI: // C.LI rd = imm; rd=0 is HINT — we still write then zero x0
		c.x[slot.rd] = uint64(int64(slot.imm))
		c.x[0] = 0

	case opC_LUI_OR_ADDI16SP:
		if slot.rd == 2 { // C.ADDI16SP
			if slot.imm == 0 {
				return ErrIllegalInstruction
			}
			c.x[2] = c.x[2] + uint64(int64(slot.imm))
		} else { // C.LUI
			if slot.rd == 0 || slot.imm == 0 {
				return ErrIllegalInstruction
			}
			c.x[slot.rd] = uint64(int64(slot.imm))
		}

	case opC_ADDI4SPN: // C.ADDI4SPN rd' = sp + nzuimm*4
		if slot.imm == 0 {
			return ErrIllegalInstruction
		}
		c.x[slot.rd] = c.x[2] + uint64(slot.imm)

	case opC_LW:
		v, f := (&c.mem).Load32(c.x[slot.rs1] + uint64(slot.imm))
		if f != nil {
			return f
		}
		c.x[slot.rd] = uint64(int64(int32(v)))

	case opC_LD:
		v, f := (&c.mem).Load64(c.x[slot.rs1] + uint64(slot.imm))
		if f != nil {
			return f
		}
		c.x[slot.rd] = v

	case opC_SW:
		if f := (&c.mem).Store32(c.x[slot.rs1]+uint64(slot.imm), uint32(c.x[slot.rs2])); f != nil {
			return f
		}

	case opC_SD:
		if f := (&c.mem).Store64(c.x[slot.rs1]+uint64(slot.imm), c.x[slot.rs2]); f != nil {
			return f
		}

	case opC_LWSP:
		if slot.rd == 0 {
			return ErrIllegalInstruction
		}
		v, f := (&c.mem).Load32(c.x[2] + uint64(slot.imm))
		if f != nil {
			return f
		}
		c.x[slot.rd] = uint64(int64(int32(v)))

	case opC_LDSP:
		if slot.rd == 0 {
			return ErrIllegalInstruction
		}
		v, f := (&c.mem).Load64(c.x[2] + uint64(slot.imm))
		if f != nil {
			return f
		}
		c.x[slot.rd] = v

	case opC_SWSP:
		if f := (&c.mem).Store32(c.x[2]+uint64(slot.imm), uint32(c.x[slot.rs2])); f != nil {
			return f
		}

	case opC_SDSP:
		if f := (&c.mem).Store64(c.x[2]+uint64(slot.imm), c.x[slot.rs2]); f != nil {
			return f
		}

	case opC_SLLI:
		if slot.rd == 0 {
			return ErrIllegalInstruction
		}
		c.x[slot.rd] = c.x[slot.rd] << uint(slot.imm)

	case opC_J:
		c.pc = c.pc + uint64(int64(slot.imm))
		return nil

	case opC_BEQZ:
		if c.x[slot.rs1] == 0 {
			c.pc = c.pc + uint64(int64(slot.imm))
			return nil
		}

	case opC_BNEZ:
		if c.x[slot.rs1] != 0 {
			c.pc = c.pc + uint64(int64(slot.imm))
			return nil
		}

	case opC_CR_MISC:
		// funct3=100 q2: dispatch on bit12 | rs2==0.
		insn := uint16(slot.insn)
		bit12 := (insn >> 12) & 1
		rs2 := uint8((insn >> 2) & 0x1F)
		rd := uint8((insn >> 7) & 0x1F)
		if bit12 == 0 {
			if rs2 == 0 { // C.JR
				if rd == 0 {
					return ErrIllegalInstruction
				}
				c.pc = c.x[rd] &^ 1
				return nil
			}
			// C.MV
			c.x[rd] = c.x[rs2]
			c.x[0] = 0
		} else {
			if rd == 0 && rs2 == 0 { // C.EBREAK
				c.pc = nextPC
				return ErrEbreak
			}
			if rs2 == 0 { // C.JALR
				ret := nextPC
				c.pc = c.x[rd] &^ 1
				c.x[1] = ret
				return nil
			}
			// C.ADD
			c.x[rd] = c.x[rd] + c.x[rs2]
			c.x[0] = 0
		}

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
					return ErrIllegalInstruction
				}
			}
		}
		c.x[0] = 0

	default:
		// Complex/FP/uncommon paths — defer to the canonical stepRVC body.
		return c.stepRVC(uint16(slot.insn))
	}

	c.pc = nextPC
	return nil
}
