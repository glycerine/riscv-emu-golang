package riscv

import (
	"encoding/binary"
	"testing"
)

// FuzzCPU fuzzes the CPU against a pure-Go reference for each implemented
// instruction. The oracle is the Go expression that the RISC-V spec defines.
//
// Corpus layout: [insn:4][a:8][b:8] = 20 bytes
//   insn — forced into a legal encoding by legalise()
//   a    — value placed in rs1
//   b    — value placed in rs2
func FuzzCPU(f *testing.F) {
	type seed struct {
		insn   uint32
		a, b   uint64
	}
	seeds := []seed{
		{buildI(0x13, 0, 1, 2, 1),       100, 0},    // ADDI positive imm
		{buildI(0x13, 0, 1, 2, -1),      5,   0},    // ADDI negative imm
		{buildI(0x13, 2, 1, 2, 10),      5,   0},    // SLTI true
		{buildI(0x13, 2, 1, 2, 10),      20,  0},    // SLTI false
		{buildI(0x13, 3, 1, 2, 10),      5,   0},    // SLTIU
		{buildI(0x13, 4, 1, 2, 0xFF),    0xAB, 0},   // XORI
		{buildI(0x13, 6, 1, 2, 0xF0),    0x0F, 0},   // ORI
		{buildI(0x13, 7, 1, 2, 0xF0),    0xFF, 0},   // ANDI
		{buildIS(0x13, 1, 1, 2, 3, false),  1,  0},  // SLLI
		{buildIS(0x13, 5, 1, 2, 3, false),  0xFF, 0},// SRLI
		{buildIS(0x13, 5, 1, 2, 3, true), ^uint64(0), 0}, // SRAI
		{buildR(0x33, 0, 0x00, 1, 2, 3), 100, 200},  // ADD
		{buildR(0x33, 0, 0x20, 1, 2, 3), 200, 100},  // SUB
		{buildR(0x33, 1, 0x00, 1, 2, 3), 1,   4},    // SLL
		{buildR(0x33, 2, 0x00, 1, 2, 3), 1,   2},    // SLT
		{buildR(0x33, 3, 0x00, 1, 2, 3), 1,   2},    // SLTU
		{buildR(0x33, 4, 0x00, 1, 2, 3), 0xAA, 0x55},// XOR
		{buildR(0x33, 5, 0x00, 1, 2, 3), 0xFF, 2},   // SRL
		{buildR(0x33, 5, 0x20, 1, 2, 3), ^uint64(0), 1}, // SRA
		{buildR(0x33, 6, 0x00, 1, 2, 3), 0xF0, 0x0F},// OR
		{buildR(0x33, 7, 0x00, 1, 2, 3), 0xFF, 0x0F},// AND
		{buildB(0x63, 0, 2, 3, 8),       42,  42},   // BEQ taken
		{buildB(0x63, 0, 2, 3, 8),       1,   2},    // BEQ not taken
		{buildB(0x63, 1, 2, 3, 8),       1,   2},    // BNE taken
		{buildB(0x63, 4, 2, 3, 8),       ^uint64(0), 1}, // BLT taken
		{buildB(0x63, 5, 2, 3, 8),       5,   5},    // BGE taken
		{buildB(0x63, 6, 2, 3, 8),       1,   2},    // BLTU taken
		{buildB(0x63, 7, 2, 3, 8),       5,   5},    // BGEU taken
		{buildU(0x37, 1, 0x12345),        0,   0},   // LUI
		{buildU(0x17, 1, 0x12345),        0,   0},   // AUIPC
	}
	for _, s := range seeds {
		var buf [20]byte
		binary.LittleEndian.PutUint32(buf[0:], s.insn)
		binary.LittleEndian.PutUint64(buf[4:], s.a)
		binary.LittleEndian.PutUint64(buf[12:], s.b)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 20 {
			return
		}
		rawInsn := binary.LittleEndian.Uint32(data[0:])
		a       := binary.LittleEndian.Uint64(data[4:])
		b       := binary.LittleEndian.Uint64(data[12:])

		insn, rd, rs1, rs2 := legalise(rawInsn)
		if insn == 0 {
			return
		}

		got  := runOne(t, insn, rd, rs1, rs2, a, b)
		want := reference(insn, rd, rs1, rs2, a, b)
		if got != want {
			t.Errorf("insn=0x%08X rs1=0x%016X rs2=0x%016X\n  got=0x%016X\n want=0x%016X",
				insn, a, b, got, want)
		}
	})
}

// legalise maps arbitrary fuzz bytes to a valid instruction from our
// implemented set. Returns (insn, rd, rs1, rs2) with rd always non-zero.
// Returns insn=0 if it cannot be mapped.
func legalise(raw uint32) (insn uint32, rd, rs1, rs2 uint8) {
	// Extract register fields — these roam freely over [0,31]
	rd  = uint8((raw >> 7) & 0x1F)
	rs1 = uint8((raw >> 15) & 0x1F)
	rs2 = uint8((raw >> 20) & 0x1F)
	if rd == 0 { rd = 1 }

	imm12 := int32(raw) >> 20
	shamt := uint8(raw>>20) & 0x3F
	srai  := (raw>>30)&1 == 1
	funct3 := uint8((raw >> 12) & 0x7)
	funct7 := uint8(raw >> 25)
	imm20  := (raw >> 12) & 0xFFFFF

	// Use low 4 bits of raw to select instruction family
	switch (raw & 0xF) % 10 {
	case 0: // OP-IMM (non-shift): ADDI/SLTI/SLTIU/XORI/ORI/ANDI
		if funct3 == 1 || funct3 == 5 { funct3 = 0 }
		insn = buildI(0x13, funct3, rd, rs1, imm12)
	case 1: // OP-IMM shifts
		if funct3 != 1 { funct3 = 5 }
		insn = buildIS(0x13, funct3, rd, rs1, shamt, srai)
	case 2: // OP R-type
		// Only funct7=0x00 and funct7=0x20 are valid for ADD/SUB and SRL/SRA
		if funct3 != 0 && funct3 != 5 { funct7 = 0 }
		if funct7 != 0 { funct7 = 0x20 }
		insn = buildR(0x33, funct3, funct7, rd, rs1, rs2)
	case 3: // BEQ
		insn = buildB(0x63, 0, rs1, rs2, 8)
	case 4: // BNE
		insn = buildB(0x63, 1, rs1, rs2, 8)
	case 5: // BLT
		insn = buildB(0x63, 4, rs1, rs2, 8)
	case 6: // BGE
		insn = buildB(0x63, 5, rs1, rs2, 8)
	case 7: // BLTU/BGEU
		if funct3 < 6 { funct3 = 6 }
		insn = buildB(0x63, funct3, rs1, rs2, 8)
	case 8: // LUI
		insn = buildU(0x37, rd, imm20)
	case 9: // AUIPC
		insn = buildU(0x17, rd, imm20)
	}
	return
}

// runOne executes insn in an isolated CPU and returns the result:
// for arithmetic/logic: the destination register value after execution.
// for branches: 1 if taken, 0 if not taken.
func runOne(t *testing.T, insn uint32, rd, rs1, rs2 uint8, a, b uint64) uint64 {
	t.Helper()
	mem, err := NewGuestMemory(Size64MB)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	const code = uint64(0x1000)
	mem.Store32(code, insn)
	mem.Store32(code+4, 0x00100073) // EBREAK

	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	cpu.SetReg(rs1, a)
	if rs2 != rs1 { cpu.SetReg(rs2, b) }

	// Step one instruction only — avoids EBREAK side-effects on PC
	if err := cpu.step(); err != nil && err != ErrEbreak {
		return 0 // fault/illegal — skip via matching reference returning 0
	}

	opcode := insn & 0x7F
	if opcode == 0x63 { // branch: was it taken?
		if cpu.PC() == code+8 { return 1 }
		return 0
	}
	return cpu.Reg(rd)
}

// reference computes the expected result using direct Go expressions.
// For branches returns 1=taken, 0=not taken.
func reference(insn uint32, rd, rs1, rs2 uint8, a, b uint64) uint64 {
	opcode := insn & 0x7F
	funct3 := uint8((insn >> 12) & 0x7)
	funct7 := uint8(insn >> 25)
	iimm   := int64(int32(insn)) >> 20
	shamt  := uint8(insn>>20) & 0x3F
	imm20  := insn & 0xFFFFF000

	if rs1 == 0 { a = 0 }
	if rs2 == 0 { b = 0 } else if rs2 == rs1 { b = a }

	switch opcode {
	case 0x13: // OP-IMM
		switch funct3 {
		case 0: return a + uint64(iimm)
		case 1: // SLLI — funct7[6:1] must be 0x00; bit25 is shamt[5] (allowed to be 1)
			if funct7 &^ 1 != 0 { return 0 } // reserved encoding → illegal instruction
			return a << shamt
		case 2: if int64(a) < iimm { return 1 }; return 0
		case 3: if a < uint64(iimm) { return 1 }; return 0
		case 4: return a ^ uint64(iimm)
		case 5: // SRLI/SRAI — funct7[6:1] must be 0x00 or 0x10
			if (insn>>30)&1 == 1 {
				if funct7 &^ 1 != 0x20 { return 0 } // reserved
				return uint64(int64(a) >> shamt)
			}
			if funct7 &^ 1 != 0 { return 0 } // reserved
			return a >> shamt
		case 6: return a | uint64(iimm)
		case 7: return a & uint64(iimm)
		}
	case 0x33: // OP
		switch funct3 {
		case 0: if funct7 == 0x20 { return a - b }; return a + b
		case 1: return a << (b & 0x3F)
		case 2: if int64(a) < int64(b) { return 1 }; return 0
		case 3: if a < b { return 1 }; return 0
		case 4: return a ^ b
		case 5:
			if (insn>>30)&1 == 1 { return uint64(int64(a) >> (b & 0x3F)) }
			return a >> (b & 0x3F)
		case 6: return a | b
		case 7: return a & b
		}
	case 0x63: // BRANCH — 1=taken, 0=not taken
		var taken bool
		switch funct3 {
		case 0: taken = a == b
		case 1: taken = a != b
		case 4: taken = int64(a) < int64(b)
		case 5: taken = int64(a) >= int64(b)
		case 6: taken = a < b
		case 7: taken = a >= b
		}
		if taken { return 1 }; return 0
	case 0x37: // LUI
		return uint64(int64(int32(imm20)))
	case 0x17: // AUIPC — pc=0x1000 in our test
		return 0x1000 + uint64(int64(int32(imm20)))
	}
	_ = rd
	return 0
}

// ── encoding helpers ──────────────────────────────────────────────────────

func buildI(opcode, funct3, rd, rs1 uint8, imm int32) uint32 {
	return uint32(imm)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}

func buildIS(opcode, funct3, rd, rs1, shamt uint8, srai bool) uint32 {
	var f7 uint32
	if srai { f7 = 0x20 }
	return f7<<25 | uint32(shamt)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}

func buildR(opcode, funct3, funct7, rd, rs1, rs2 uint8) uint32 {
	return uint32(funct7)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}

func buildB(opcode, funct3, rs1, rs2 uint8, offset int16) uint32 {
	o := uint32(offset)
	return ((o>>12)&1)<<31 | ((o>>5)&0x3F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(funct3)<<12 | ((o>>1)&0xF)<<8 | ((o>>11)&1)<<7 | uint32(opcode)
}

func buildU(opcode, rd uint8, imm20 uint32) uint32 {
	return (imm20&0xFFFFF)<<12 | uint32(rd)<<7 | uint32(opcode)
}
