package fuzzoracle

// Instruction encoding helpers shared across all files in this package.

func ienc(opcode, funct3, rd, rs1 uint8, imm int32) uint32 {
	return uint32(imm)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}
func senc(opcode, funct3, rd, rs1, shamt uint8, srai bool) uint32 {
	var f7 uint32
	if srai {
		f7 = 0x20
	}
	return f7<<25 | uint32(shamt)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}
func renc(opcode, funct3, funct7, rd, rs1, rs2 uint8) uint32 {
	return uint32(funct7)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}
func benc(opcode, funct3, rs1, rs2 uint8, offset int16) uint32 {
	o := uint32(offset)
	return ((o>>12)&1)<<31 | ((o>>5)&0x3F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(funct3)<<12 | ((o>>1)&0xF)<<8 | ((o>>11)&1)<<7 | uint32(opcode)
}
func uenc(opcode, rd uint8, imm20 uint32) uint32 {
	return (imm20&0xFFFFF)<<12 | uint32(rd)<<7 | uint32(opcode)
}

// lenc encodes LOAD (I-type, opcode=0x03) or STORE (S-type, opcode=0x23).
// For LOAD:  r1=rd, r2=rs1. For STORE: r1=rs1, r2=rs2.
func lenc(opcode, funct3, r1, r2 uint8, imm int32) uint32 {
	if opcode == 0x03 {
		return uint32(imm)<<20 | uint32(r2)<<15 | uint32(funct3)<<12 | uint32(r1)<<7 | uint32(opcode)
	}
	u := uint32(imm)
	return ((u>>5)&0x7F)<<25 | uint32(r2)<<20 | uint32(r1)<<15 | uint32(funct3)<<12 | (u&0x1F)<<7 | uint32(opcode)
}

// jalenc encodes a JAL instruction (J-type).
// rd=destination, offset=PC-relative byte offset (must be even).
func jalenc(rd uint8, offset int32) uint32 {
	o := uint32(offset)
	return ((o>>20)&1)<<31 |
		((o>>1)&0x3FF)<<21 |
		((o>>11)&1)<<20 |
		((o>>12)&0xFF)<<12 |
		uint32(rd)<<7 |
		0x6F
}

// jalrenc encodes a JALR instruction (I-type).
// rd=destination, rs1=base register, imm=signed 12-bit offset.
func jalrenc(rd, rs1 uint8, imm int32) uint32 {
	return uint32(imm)<<20 | uint32(rs1)<<15 | uint32(rd)<<7 | 0x67
}
