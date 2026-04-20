package riscv

// decodeInsn32 pre-decodes a 32-bit RV64 instruction into a slot.
// Populates slot.op (= opcode), rd/rs1/rs2/rs3, funct3/funct7, and imm
// (sign-extended per the instruction format: I / S / B / U / J).
// Sets flagBlockEnd on control-transfer and system opcodes.
//
//go:nosplit
func decodeInsn32(slot *DecodedInsn, insn uint32) {
	slot.len = 4
	slot.insn = insn
	slot.op = uint8(insn & 0x7F)
	slot.rd = uint8((insn >> 7) & 0x1F)
	slot.funct3 = uint8((insn >> 12) & 0x07)
	slot.rs1 = uint8((insn >> 15) & 0x1F)
	slot.rs2 = uint8((insn >> 20) & 0x1F)
	slot.rs3 = uint8((insn >> 27) & 0x1F)
	slot.funct7 = uint8((insn >> 25) & 0x7F)

	switch slot.op {
	case 0x03, 0x13, 0x1B, 0x67, 0x73, 0x07, 0x0F:
		// I-type immediate: bits[31:20] sign-extended.
		slot.imm = int32(insn) >> 20
	case 0x23, 0x27:
		// S-type immediate: bits[31:25]|bits[11:7].
		slot.imm = (int32(insn&0xFE000000) >> 20) | int32((insn>>7)&0x1F)
	case 0x63:
		// B-type immediate: bits[31|7|30:25|11:8]|0.
		bit12 := int32(insn>>31) & 1
		bit11 := int32(insn>>7) & 1
		bits10_5 := int32(insn>>25) & 0x3F
		bits4_1 := int32(insn>>8) & 0xF
		imm := (bit12 << 12) | (bit11 << 11) | (bits10_5 << 5) | (bits4_1 << 1)
		if bit12 != 0 {
			imm |= ^0x1FFF
		}
		slot.imm = imm
		slot.flags |= flagBlockEnd
	case 0x6F:
		// J-type immediate: bits[31|19:12|20|30:21]|0.
		bit20 := int32(insn>>31) & 1
		bits10_1 := int32(insn>>21) & 0x3FF
		bit11 := int32(insn>>20) & 1
		bits19_12 := int32(insn>>12) & 0xFF
		imm := (bit20 << 20) | (bits19_12 << 12) | (bit11 << 11) | (bits10_1 << 1)
		if bit20 != 0 {
			imm |= ^0x1FFFFF
		}
		slot.imm = imm
		slot.flags |= flagBlockEnd
	case 0x37, 0x17:
		// U-type immediate: bits[31:12] << 12, sign-extended.
		slot.imm = int32(insn & 0xFFFFF000)
	default:
		// R-type / AMO / F-ops don't carry an immediate we need to pre-decode.
		slot.imm = 0
	}

	// Also flag JALR (0x67) and SYSTEM (0x73) as block-ending — they always
	// redirect control flow (jump / ecall / ebreak / mret).
	if slot.op == 0x67 || slot.op == 0x73 {
		slot.flags |= flagBlockEnd
	}
}

// Synthetic opcode classes for RVC instructions. Real RV32 opcodes occupy
// 0x00..0x7F; RVC classes start at 0x80 so they don't collide.
//
// Each RVC class corresponds to one funct3 value in one quadrant.
// Some classes (opC_LUI_OR_ADDI16SP, opC_MISC_ALU, opC_CR_MISC) require
// additional dispatch inside the executor on secondary fields.
const (
	opC_ADDI4SPN = 0x80 + iota
	opC_FLD
	opC_LW
	opC_LD
	opC_RESV_Q0_100
	opC_FSD
	opC_SW
	opC_SD
	opC_ADDI
	opC_ADDIW
	opC_LI
	opC_LUI_OR_ADDI16SP
	opC_MISC_ALU
	opC_J
	opC_BEQZ
	opC_BNEZ
	opC_SLLI
	opC_FLDSP
	opC_LWSP
	opC_LDSP
	opC_JR     // funct3=100 q2: bit12=0, rs2=0, rd!=0
	opC_MV     // funct3=100 q2: bit12=0, rs2!=0
	opC_EBREAK // funct3=100 q2: bit12=1, rd=0, rs2=0
	opC_JALR   // funct3=100 q2: bit12=1, rs2=0, rd!=0
	opC_ADD    // funct3=100 q2: bit12=1, rs2!=0
	opC_FSDSP
	opC_SWSP
	opC_SDSP
	// opFallback is set when we don't have a specialized slot-based executor
	// for this instruction. RunCached falls back to stepRVC/stepFromInsn.
	opFallback
)

// decodeRVC populates slot with the pre-decoded form of a 16-bit RVC
// instruction. Register fields using the 3-bit rd'/rs1'/rs2' encoding are
// translated to their full 5-bit form (x8..x15).
//
// Immediates are pre-expanded to their final signed int32 value. The
// executor can use slot.imm directly without re-shifting.
//
//go:nosplit
func decodeRVC(slot *DecodedInsn, insn uint16) {
	slot.len = 2
	slot.insn = uint32(insn)
	slot.funct3 = uint8(insn >> 13)
	quad := uint8(insn & 0x3)

	switch quad {
	case 0x0:
		slot.rd = 8 + uint8((insn>>2)&7)
		slot.rs1 = 8 + uint8((insn>>7)&7)
		slot.rs2 = 8 + uint8((insn>>2)&7)
		switch slot.funct3 {
		case 0b000: // C.ADDI4SPN
			nzuimm := int32(((insn>>11)&3)<<4 | ((insn>>7)&0xF)<<6 |
				((insn>>6)&1)<<2 | ((insn>>5)&1)<<3)
			slot.op = opC_ADDI4SPN
			slot.imm = nzuimm
		case 0b001:
			slot.op = opC_FLD
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		case 0b010:
			slot.op = opC_LW
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
		case 0b011:
			slot.op = opC_LD
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		case 0b101:
			slot.op = opC_FSD
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		case 0b110:
			slot.op = opC_SW
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
		case 0b111:
			slot.op = opC_SD
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		default:
			slot.op = opFallback
		}
	case 0x1:
		switch slot.funct3 {
		case 0b000: // C.ADDI / C.NOP(rd=0)
			slot.rd = uint8((insn >> 7) & 0x1F)
			slot.rs1 = slot.rd
			imm6 := int32((insn >> 2) & 0x1F)
			if (insn>>12)&1 != 0 {
				imm6 |= ^0x1F
			}
			slot.op = opC_ADDI
			slot.imm = imm6
		case 0b001: // C.ADDIW
			slot.rd = uint8((insn >> 7) & 0x1F)
			slot.rs1 = slot.rd
			imm6 := int32((insn >> 2) & 0x1F)
			if (insn>>12)&1 != 0 {
				imm6 |= ^0x1F
			}
			slot.op = opC_ADDIW
			slot.imm = imm6
		case 0b010: // C.LI
			slot.rd = uint8((insn >> 7) & 0x1F)
			imm6 := int32((insn >> 2) & 0x1F)
			if (insn>>12)&1 != 0 {
				imm6 |= ^0x1F
			}
			slot.op = opC_LI
			slot.imm = imm6
		case 0b011: // C.LUI (rd != 2) or C.ADDI16SP (rd == 2)
			slot.rd = uint8((insn >> 7) & 0x1F)
			slot.rs1 = slot.rd
			if slot.rd == 2 {
				imm := int32(((insn>>12)&1)<<9 | ((insn>>6)&1)<<4 |
					((insn>>5)&1)<<6 | ((insn>>3)&3)<<7 | ((insn>>2)&1)<<5)
				if (insn>>12)&1 != 0 {
					imm |= ^0x3FF
				}
				slot.imm = imm
			} else {
				hi := int32((insn >> 12) & 1)
				lo := int32((insn >> 2) & 0x1F)
				imm := (hi << 5) | lo
				if hi != 0 {
					imm |= ^0x3F
				}
				slot.imm = imm << 12
			}
			slot.op = opC_LUI_OR_ADDI16SP
		case 0b100:
			slot.op = opC_MISC_ALU // executor dispatches on funct2 + bit12
		case 0b101:
			slot.op = opC_J
			slot.imm = int32(cjOffset(insn))
			slot.flags |= flagBlockEnd
		case 0b110:
			slot.rs1 = 8 + uint8((insn>>7)&7)
			slot.op = opC_BEQZ
			slot.imm = int32(cbOffset(insn))
			slot.flags |= flagBlockEnd
		case 0b111:
			slot.rs1 = 8 + uint8((insn>>7)&7)
			slot.op = opC_BNEZ
			slot.imm = int32(cbOffset(insn))
			slot.flags |= flagBlockEnd
		default:
			slot.op = opFallback
		}
	case 0x2:
		slot.rd = uint8((insn >> 7) & 0x1F)
		slot.rs1 = slot.rd
		slot.rs2 = uint8((insn >> 2) & 0x1F)
		switch slot.funct3 {
		case 0b000: // C.SLLI
			slot.op = opC_SLLI
			slot.imm = int32((insn>>12)&1)<<5 | int32((insn>>2)&0x1F)
		case 0b001:
			slot.op = opC_FLDSP
			slot.imm = int32(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
		case 0b010:
			slot.op = opC_LWSP
			slot.imm = int32(((insn>>12)&1)<<5 | ((insn>>4)&7)<<2 | ((insn>>2)&3)<<6)
		case 0b011:
			slot.op = opC_LDSP
			slot.imm = int32(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
		case 0b100:
			// funct3=100 q2 splits on bit12 + rs2 (rs2 is already in slot.rs2).
			bit12 := (insn >> 12) & 1
			if bit12 == 0 {
				if slot.rs2 == 0 {
					// C.JR — rd holds rs1 here
					if slot.rd == 0 {
						slot.op = opFallback // illegal; let stepRVC reject
					} else {
						slot.op = opC_JR
						slot.flags |= flagBlockEnd
					}
				} else {
					slot.op = opC_MV
				}
			} else {
				if slot.rs2 == 0 {
					if slot.rd == 0 {
						slot.op = opC_EBREAK
						slot.flags |= flagBlockEnd
					} else {
						slot.op = opC_JALR
						slot.flags |= flagBlockEnd
					}
				} else {
					slot.op = opC_ADD
				}
			}
		case 0b101:
			slot.op = opC_FSDSP
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
		case 0b110:
			slot.op = opC_SWSP
			slot.imm = int32(((insn>>9)&0xF)<<2 | ((insn>>7)&3)<<6)
		case 0b111:
			slot.op = opC_SDSP
			slot.imm = int32(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
		default:
			slot.op = opFallback
		}
	default:
		slot.op = opFallback
	}
}
