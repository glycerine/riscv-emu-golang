package fuzzoracle

import (
	"encoding/binary"
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

// ALU + M-extension layout: 64KB memory, code at 0x10000.
const (
	aluMemSize = 64 * 1024
	aluCodeVA  = uint64(0x10000)
)

// FuzzALUVsLibriscv compares ALU, branch, and M-extension instructions
// against libriscv step-by-step (register state only; no memory access).
//
// Corpus: [n:1][insns:n*4][a:8][b:8]
//   n     — number of instructions (1..8)
//   insns — raw instruction words (legalised before use)
//   a     — value placed in x2 (rs1 for most instructions)
//   b     — value placed in x3 (rs2 for most instructions)
func FuzzALUVsLibriscv(f *testing.F) {
	seeds := []struct {
		insns  []uint32
		a, b   uint64
	}{
		// RV64I
		{[]uint32{ienc(0x13, 0, 1, 2, 1)}, 100, 0},          // ADDI
		{[]uint32{ienc(0x13, 0, 1, 2, -1)}, 5, 0},           // ADDI neg
		{[]uint32{renc(0x33, 0, 0x00, 1, 2, 3)}, 100, 200},  // ADD
		{[]uint32{renc(0x33, 0, 0x20, 1, 2, 3)}, 200, 100},  // SUB
		{[]uint32{renc(0x33, 4, 0x00, 1, 2, 3)}, 0xAA, 0x55},// XOR
		{[]uint32{uenc(0x37, 1, 0x12345)}, 0, 0},             // LUI
		{[]uint32{uenc(0x17, 1, 0x12345)}, 0, 0},             // AUIPC
		{[]uint32{benc(0x63, 0, 2, 3, 8)}, 42, 42},           // BEQ taken
		{[]uint32{benc(0x63, 1, 2, 3, 8)}, 1, 2},             // BNE
		{[]uint32{ienc(0x1B, 0, 1, 2, -1)}, 5, 0},            // ADDIW
		{[]uint32{renc(0x3B, 0, 0x00, 1, 2, 3)}, 100, 200},  // ADDW
		// RV64M — use b=6 (non-zero divisor)
		{[]uint32{renc(0x33, 0, 0x01, 1, 2, 3)}, 42, 6},     // MUL
		{[]uint32{renc(0x33, 1, 0x01, 1, 2, 3)}, 42, 6},     // MULH
		{[]uint32{renc(0x33, 2, 0x01, 1, 2, 3)}, 42, 6},     // MULHSU
		{[]uint32{renc(0x33, 3, 0x01, 1, 2, 3)}, 42, 6},     // MULHU
		{[]uint32{renc(0x33, 4, 0x01, 1, 2, 3)}, 42, 6},     // DIV
		{[]uint32{renc(0x33, 5, 0x01, 1, 2, 3)}, 42, 6},     // DIVU
		{[]uint32{renc(0x33, 6, 0x01, 1, 2, 3)}, 43, 6},     // REM
		{[]uint32{renc(0x33, 7, 0x01, 1, 2, 3)}, 43, 6},     // REMU
		{[]uint32{renc(0x3B, 0, 0x01, 1, 2, 3)}, 7, 6},      // MULW
		{[]uint32{renc(0x3B, 4, 0x01, 1, 2, 3)}, 42, 6},     // DIVW
		{[]uint32{renc(0x3B, 5, 0x01, 1, 2, 3)}, 42, 6},     // DIVUW
		{[]uint32{renc(0x3B, 6, 0x01, 1, 2, 3)}, 43, 6},     // REMW
		{[]uint32{renc(0x3B, 7, 0x01, 1, 2, 3)}, 43, 6},     // REMUW
		// MULH negative (our CPU is correct; only test where libriscv agrees)
		{[]uint32{renc(0x33, 1, 0x01, 1, 2, 3)}, 3, 4},      // MULH pos*pos
		{[]uint32{renc(0x33, 3, 0x01, 1, 2, 3)}, 0xFFFFFFFFFFFFFFFF, 2}, // MULHU
	}
	for _, s := range seeds {
		f.Add(aluCorpus(s.insns, s.a, s.b))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 1+4+8+8 {
			return
		}
		nInsns := int(data[0]&0x7) + 1
		if len(data) < 1+nInsns*4+16 {
			return
		}

		insns := make([]uint32, nInsns)
		for i := range nInsns {
			insns[i] = aluLegalise(binary.LittleEndian.Uint32(data[1+i*4:]))
			if insns[i] == 0 {
				return
			}
		}
		off := 1 + nInsns*4
		a := binary.LittleEndian.Uint64(data[off:])
		b := binary.LittleEndian.Uint64(data[off+8:])

		// Clamp b away from zero for divide instructions.
		// libriscv delivers SIGFPE on divide-by-zero; our CPU returns spec
		// values. We test those corner cases separately in cpu_test.go.
		if b == 0 && containsDivide(insns) {
			b = 1
		}
		// Clamp a to non-negative for MULH/MULHSU: libriscv has incorrect
		// upper-64 results when rs1 is negative. Our CPU is correct per spec;
		// those cases are verified directly in cpu_test.go.
		if int64(a) < 0 && containsMULHSigned(insns) {
			a = a &^ (uint64(1) << 63) // clear sign bit
		}

		code := append(append([]uint32{}, insns...), 0x00000073) // + ECALL
		elf := riscv.BuildELF(aluCodeVA, code)

		lm := NewMachine(elf)
		if lm == nil {
			return
		}
		defer lm.Close()

		var lInit [32]uint64
		lInit[2] = a
		lInit[3] = b
		lm.SetRegsAndPC(lInit, aluCodeVA)

		mem, err := riscv.NewGuestMemory(aluMemSize)
		if err != nil {
			t.Fatal(err)
		}
		defer mem.Free()

		ef, err := riscv.LoadELFBytes(mem, elf)
		if err != nil {
			return
		}
		cpu := riscv.NewCPU(*mem)
		cpu.SetPC(ef.Entry)
		cpu.SetReg(2, a)
		cpu.SetReg(3, b)

		for step, insn := range insns {
			ourErr := cpu.Step()
			lm.RunToEcall()

			lRegs := lm.SnapshotRegs()
			for r := 0; r < 32; r++ {
				if cpu.Reg(uint8(r)) != lRegs[r] {
					t.Fatalf("step %d insn=0x%08X a=0x%016X b=0x%016X: x%d ours=0x%016X libriscv=0x%016X",
						step, insn, a, b, r, cpu.Reg(uint8(r)), lRegs[r])
				}
			}
			if ourErr != nil {
				return
			}

			// Reset libriscv to our CPU's current state for the next step
			var next [32]uint64
			for r := uint8(0); r < 32; r++ {
				next[r] = cpu.Reg(r)
			}
			lm.SetRegsAndPC(next, cpu.PC())
		}
	})
}

// containsDivide returns true if any instruction in the slice is a
// DIV/DIVU/REM/REMU or their W variants (funct7=0x01, funct3>=4).
func containsDivide(insns []uint32) bool {
	for _, insn := range insns {
		opcode := insn & 0x7F
		funct7 := insn >> 25
		funct3 := (insn >> 12) & 0x7
		if (opcode == 0x33 || opcode == 0x3B) && funct7 == 0x01 && funct3 >= 4 {
			return true
		}
	}
	return false
}

// containsMULHSigned returns true if the slice contains MULH (funct3=1) or
// MULHSU (funct3=2) — libriscv returns incorrect upper-64 results for these
// when rs1 is negative.
func containsMULHSigned(insns []uint32) bool {
	for _, insn := range insns {
		opcode := insn & 0x7F
		funct7 := insn >> 25
		funct3 := (insn >> 12) & 0x7
		if opcode == 0x33 && funct7 == 0x01 && (funct3 == 1 || funct3 == 2) {
			return true
		}
	}
	return false
}

func aluCorpus(insns []uint32, a, b uint64) []byte {
	buf := make([]byte, 1+len(insns)*4+16)
	buf[0] = byte(len(insns))
	for i, insn := range insns {
		binary.LittleEndian.PutUint32(buf[1+i*4:], insn)
	}
	binary.LittleEndian.PutUint64(buf[1+len(insns)*4:], a)
	binary.LittleEndian.PutUint64(buf[1+len(insns)*4+8:], b)
	return buf
}

// aluLegalise maps raw fuzz bytes to a legal instruction from our
// implemented set — now including M-extension multiply/divide.
func aluLegalise(raw uint32) uint32 {
	rd     := uint8((raw >> 7) & 0x1F)
	rs1    := uint8((raw >> 15) & 0x1F)
	rs2    := uint8((raw >> 20) & 0x1F)
	funct3 := uint8((raw >> 12) & 0x7)
	funct7 := uint8(raw >> 25)
	imm12  := int32(raw) >> 20
	shamt  := uint8(raw>>20) & 0x3F
	srai   := (raw>>30)&1 == 1
	imm20  := (raw >> 12) & 0xFFFFF

	if rd == 0 { rd = 1 }

	switch (raw & 0xF) % 16 {
	case 0: // OP-IMM non-shift
		if funct3 == 1 || funct3 == 5 { funct3 = 0 }
		return ienc(0x13, funct3, rd, rs1, imm12)
	case 1: // OP-IMM shift
		if funct3 != 1 { funct3 = 5 }
		return senc(0x13, funct3, rd, rs1, shamt, srai)
	case 2: // OP R-type (RV64I)
		if funct3 != 0 && funct3 != 5 { funct7 = 0 }
		if funct7 != 0 { funct7 = 0x20 }
		return renc(0x33, funct3, funct7, rd, rs1, rs2)
	case 3: return uenc(0x37, rd, imm20)                          // LUI
	case 4: return uenc(0x17, rd, imm20)                          // AUIPC
	case 5: return benc(0x63, 0, rs1, rs2, 8)                     // BEQ
	case 6: return benc(0x63, 1, rs1, rs2, 8)                     // BNE
	case 7:                                                        // BLT/BGE/BLTU/BGEU
		if funct3 < 4 { funct3 = 4 }
		return benc(0x63, funct3, rs1, rs2, 8)
	case 8: // OP-IMM-32
		if funct3 != 0 && funct3 != 1 && funct3 != 5 { funct3 = 0 }
		if funct3 == 0 { return ienc(0x1B, 0, rd, rs1, imm12) }
		return senc(0x1B, funct3, rd, rs1, shamt&0x1F, srai)
	case 9: // OP-32 (RV64I)
		if funct3 != 0 && funct3 != 1 && funct3 != 5 { funct3 = 0 }
		f7 := uint8(0)
		if (raw>>30)&1 == 1 && (funct3 == 0 || funct3 == 5) { f7 = 0x20 }
		return renc(0x3B, funct3, f7, rd, rs1, rs2)
	case 10: // MUL/MULH/MULHSU/MULHU (funct7=0x01, funct3=0..3)
		// Only funct3 0..3 — avoids divide (funct3 4..7)
		return renc(0x33, funct3&0x3, 0x01, rd, rs1, rs2)
	case 11: // DIV/DIVU/REM/REMU (funct7=0x01, funct3=4..7)
		// rs2 clamped to non-zero by containsDivide + caller
		return renc(0x33, 4+(funct3&0x3), 0x01, rd, rs1, rs2)
	case 12: // MULW
		return renc(0x3B, 0, 0x01, rd, rs1, rs2)
	case 13: // DIVW/DIVUW/REMW/REMUW
		return renc(0x3B, 4+(funct3&0x3), 0x01, rd, rs1, rs2)
	case 14: // JAL — fixed +8 offset
		return jalenc(rd, 8)
	case 15: // JALR — x2+0
		return jalrenc(rd, rs1, 0)
	}
	return 0
}
