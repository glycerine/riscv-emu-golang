package fuzzoracle

import (
	"encoding/binary"
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

// FuzzRVCVsLibriscv compares RVC (compressed, 16-bit) instructions against
// libriscv step-by-step, checking all registers and memory after each insn.
//
// ELF layout: pairs of 16-bit instructions packed into 32-bit words.
// Each pair is [test_insn16][C.EBREAK=0x9002] so libriscv halts after the
// test instruction. Our CPU executes one Step() (the 16-bit insn).
//
// Memory layout: same as fuzz_stores — 128KB, data at oracleDataVA.
//
// Corpus: [n:1][insns:n*2][a:8][b:8][initMem:up to 64 bytes]
//   n     — number of instructions (1..8)
//   insns — raw 16-bit words (legalised before use)
//   a     — value placed in x8 (s0, common compressed dest/src)
//   b     — value placed in x9 (s1, common compressed src)
func FuzzRVCVsLibriscv(f *testing.F) {
	seeds := []struct {
		insn uint16
		a, b uint64
	}{
		// Q0
		{cADDI4SPN(0, 4), oracleDataVA &^ 0xF, 0},
		{cLW(0, 1, 0), 0, oracleDataVA},
		{cLD(0, 1, 0), 0, oracleDataVA},
		{cSW(1, 2, 0), oracleDataVA, 0xDEADBEEF},
		{cSD(1, 2, 0), oracleDataVA, 0xDEADBEEFCAFEBABE},
		// Q1 arithmetic
		{cADDI(8, 5), 10, 0},
		{cADDIW(8, 1), 0x7FFFFFFF, 0},
		{cLI(8, 42), 0, 0},
		{cADDI16SP(-16), 256, 0},
		{cLUI(1, 1), 0, 0},
		{cSRLI(0, 4), 0xFF, 0},
		{cSRAI(0, 4), 0xFFFFFFFFFFFFFFFF, 0},
		{cANDI(0, 0xF), 0xFF, 0},
		{cSUB(0, 1), 10, 3},
		{cXOR(0, 1), 0xAA, 0x55},
		{cOR(0, 1), 0xF0, 0x0F},
		{cAND(0, 1), 0xFF, 0x0F},
		{cSUBW(0, 1), 10, 3},
		{cADDW(0, 1), 0x7FFFFFFF, 1},
		// Q2
		{cSLLI(8, 3), 1, 0},
		{cLWSP(8, 0), oracleDataVA, 0},
		{cLDSP(8, 0), oracleDataVA, 0},
		{cMV(8, 9), 0, 42},
		{cADD(8, 9), 10, 20},
		{cSWSP(9, 0), oracleDataVA, 0xDEADBEEF},
		{cSDSP(9, 0), oracleDataVA, 0xDEADBEEFCAFEBABE},
	}

	for _, s := range seeds {
		var buf [32]byte
		binary.LittleEndian.PutUint16(buf[0:], s.insn)
		binary.LittleEndian.PutUint64(buf[2:], s.a)
		binary.LittleEndian.PutUint64(buf[10:], s.b)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 18 {
			return
		}

		raw16 := binary.LittleEndian.Uint16(data[0:])
		a     := binary.LittleEndian.Uint64(data[2:])
		b     := binary.LittleEndian.Uint64(data[10:])

		insn16, rs2reg := rvcLegalise(raw16, a, b)
		if insn16 == 0 {
			return
		}

		initMem := make([]byte, 128)
		if len(data) > 18 {
			n := len(data) - 18
			if n > 64 { n = 64 }
			copy(initMem, data[18:18+n])
		}

		// Build ELF: [insn16 | C.EBREAK<<16] then full ECALL
		// insn16 at oracleCodeVA, C.EBREAK at oracleCodeVA+2,
		// ECALL at oracleCodeVA+4 (for libriscv termination).
		word0 := uint32(insn16) | (uint32(0x9002) << 16) // insn16 + C.EBREAK
		elf := riscv.BuildELF(oracleCodeVA, []uint32{word0, 0x00000073})

		// ── libriscv ─────────────────────────────────────────────────
		lm := NewMachine(elf)
		if lm == nil {
			return
		}
		defer lm.Close()

		lm.WriteGuest(oracleDataVA, initMem)

		// x2=sp, x9=oracleDataVA (base for loads/stores, always in-bounds)
		// x8=a (arithmetic src/dst), rs2reg=b (store value)
		var lInit [32]uint64
		lInit[2]  = oracleDataVA + 64 // sp in middle of data region
		lInit[8]  = a
		lInit[9]  = oracleDataVA      // rs1' base: always valid load/store address
		lInit[rs2reg] = b             // store value
		lm.SetRegsAndPC(lInit, oracleCodeVA)
		lm.RunToEcall()
		lRegs := lm.SnapshotRegs()
		lMem  := lm.SnapshotMem(0, oracleMemSize)

		// ── our CPU ───────────────────────────────────────────────────
		mem, err := riscv.NewGuestMemory(oracleMemSize)
		if err != nil { t.Fatal(err) }
		defer mem.Free()

		riscv.LoadELFBytes(mem, elf)
		mem.WriteBytes(oracleDataVA, initMem)

		cpu := riscv.NewCPU(*mem)
		cpu.SetPC(oracleCodeVA)
		cpu.SetReg(2, oracleDataVA+64)
		cpu.SetReg(8, a)
		cpu.SetReg(9, oracleDataVA)   // rs1' base: always valid
		cpu.SetReg(rs2reg, b)         // store value

		cpu.Step() // execute the 16-bit instruction

		// Compare registers
		for r := 0; r < 32; r++ {
			if cpu.Reg(uint8(r)) != lRegs[r] {
				t.Fatalf("insn16=0x%04X a=0x%016X b=0x%016X: x%d ours=0x%016X libriscv=0x%016X",
					insn16, a, b, r, cpu.Reg(uint8(r)), lRegs[r])
			}
		}

		// Compare memory
		ourMem := make([]byte, oracleMemSize)
		if lMem != nil {
			if f := mem.ReadBytes(0, ourMem); f == nil {
				for i := range ourMem {
					if ourMem[i] != lMem[i] {
						t.Fatalf("insn16=0x%04X a=0x%016X: mem[0x%05X] ours=0x%02X libriscv=0x%02X",
							insn16, a, i, ourMem[i], lMem[i])
					}
				}
			}
		}
	})
}

// rvcLegalise maps a raw 16-bit fuzz word to a legal RVC instruction.
// Returns (insn16, rs2reg) where rs2reg is the register that should hold b
// for store instructions. Returns (0, 0) if it can't be mapped.
func rvcLegalise(raw uint16, a, b uint64) (insn16 uint16, rs2reg uint8) {
	// Extract fields
	quad   := raw & 0x3
	funct3 := raw >> 13
	rd3    := int((raw >> 2) & 7) // compressed rd' index (0..7 -> x8..x15)
	rs13   := int((raw >> 7) & 7) // compressed rs1' index
	rs23   := int((raw >> 2) & 7) // compressed rs2' index (Q0 stores)
	rdf    := int((raw >> 7) & 31) // full rd (Q2)
	rs2f   := int((raw >> 2) & 31) // full rs2 (Q2)

	// Clamp addresses to data region
	// baseAddr: oracleDataVA + offset, 8-byte aligned, fits 8-byte access
	baseOff := (a >> 3) & 0xF // 0..15 * 8 = 0..120, all fit in 128-byte initMem
	baseAddr := oracleDataVA + baseOff*8

	// sp-relative: sp = oracleDataVA+64, offset 0 => address = oracleDataVA+64
	// all LWSP/LDSP/SWSP/SDSP use offset 0 for simplicity

	// Which family to pick: use low 5 bits of raw
	family := int(raw>>2) & 0x1F

	switch quad {
	case 0x0: // Quadrant 0
		switch funct3 {
		case 0b000: // C.ADDI4SPN — nzuimm must be nonzero, multiple of 4
			nzuimm := ((int(raw>>11)&3)<<4 | (int(raw>>7)&0xF)<<6 |
				(int(raw>>6)&1)<<2 | (int(raw>>5)&1)<<3)
			if nzuimm == 0 { nzuimm = 4 }
			nzuimm &^= 3 // align to 4
			if nzuimm == 0 { nzuimm = 4 }
			return cADDI4SPN(rd3, nzuimm), 0
		case 0b010: // C.LW — rs1'=x9 (holds baseAddr set by harness)
			uimm := int(int(raw>>5)&0x7C | int(raw>>6)&4 | int(raw>>5)&0x40) & 0x7C
			return cLW(rd3, 1, uimm&^3), 0 // rs1'=1=x9
		case 0b011: // C.LD
			uimm := int(raw>>5) & 0xF8
			return cLD(rd3, 1, uimm&^7), 0
		case 0b110: // C.SW — rs1'=x9
			uimm := int(raw>>5) & 0x7C
			return cSW(1, rs23, uimm&^3), uint8(8 + rs23) // rs1'=x9
		case 0b111: // C.SD
			uimm := int(raw>>5) & 0xF8
			return cSD(1, rs23, uimm&^7), uint8(8 + rs23)
		}
		return 0, 0

	case 0x1: // Quadrant 1
		switch funct3 {
		case 0b000: // C.NOP/C.ADDI
			rd := rdf; if rd == 0 { rd = 8 }
			imm6 := int(raw>>2) & 0x1F
			if (raw>>12)&1 != 0 { imm6 |= -32 }
			return cADDI(rd, imm6), 0
		case 0b001: // C.ADDIW
			rd := rdf; if rd == 0 { rd = 8 }
			imm6 := int(raw>>2) & 0x1F
			if (raw>>12)&1 != 0 { imm6 |= -32 }
			return cADDIW(rd, imm6), 0
		case 0b010: // C.LI
			rd := rdf; if rd == 0 { rd = 8 }
			imm6 := int(raw>>2) & 0x1F
			if (raw>>12)&1 != 0 { imm6 |= -32 }
			return cLI(rd, imm6), 0
		case 0b011:
			rd := rdf
			if rd == 2 { // C.ADDI16SP
				nzimm := int(raw>>2)&0x3E0 | int(raw>>6)&0x10 |
					int(raw>>5)&0x40 | int(raw>>3)&0x180 | int(raw>>2)&0x20
				_ = nzimm
				// Just use -16 always (safe, valid)
				return cADDI16SP(-16), 0
			}
			// C.LUI — rd!=0,2; nzimm!=0
			if rd == 0 || rd == 2 { rd = 1 }
			nzimm := int(raw>>2) & 0x1F
			if nzimm == 0 { nzimm = 1 }
			if (raw>>12)&1 != 0 { nzimm |= -32 }
			return cLUI(rd, nzimm), 0
		case 0b100: // C.MISC-ALU
			funct2 := (raw >> 10) & 3
			rs1 := rs13 // compressed
			rs2 := rs23
			bit12 := (raw >> 12) & 1
			switch funct2 {
			case 0b00:
				shamt := int(raw>>2) & 0x3F
				if shamt == 0 { shamt = 1 }
				return cSRLI(rs1, shamt), 0
			case 0b01:
				shamt := int(raw>>2) & 0x3F
				if shamt == 0 { shamt = 1 }
				return cSRAI(rs1, shamt), 0
			case 0b10:
				imm6 := int(raw>>2) & 0x1F
				if (raw>>12)&1 != 0 { imm6 |= -32 }
				return cANDI(rs1, imm6), 0
			case 0b11:
				if bit12 == 0 {
					switch (raw >> 5) & 3 {
					case 0: return cSUB(rs1, rs2), 0
					case 1: return cXOR(rs1, rs2), 0
					case 2: return cOR(rs1, rs2), 0
					case 3: return cAND(rs1, rs2), 0
					}
				} else {
					switch (raw >> 5) & 3 {
					case 0: return cSUBW(rs1, rs2), 0
					case 1: return cADDW(rs1, rs2), 0
					default: return 0, 0
					}
				}
			}
		case 0b101: // C.J — fixed +4 to skip C.EBREAK, land on ECALL
			return cJ(4), 0
		case 0b110: // C.BEQZ — fixed +4
			return cBEQZ(rs13, 4), 0
		case 0b111: // C.BNEZ — fixed +4
			return cBNEZ(rs13, 4), 0
		}
		return 0, 0

	case 0x2: // Quadrant 2
		_ = family
		switch funct3 {
		case 0b000: // C.SLLI
			rd := rdf; if rd == 0 { rd = 8 }
			shamt := int(raw>>2) & 0x3F
			if shamt == 0 { shamt = 1 }
			return cSLLI(rd, shamt), 0
		case 0b010: // C.LWSP — rd!=0
			rd := rdf; if rd == 0 { rd = 8 }
			return cLWSP(rd, 0), 0 // sp=oracleDataVA+64, offset=0
		case 0b011: // C.LDSP
			rd := rdf; if rd == 0 { rd = 8 }
			return cLDSP(rd, 0), 0
		case 0b100:
			bit12 := (raw >> 12) & 1
			if bit12 == 0 {
				if rs2f == 0 { // C.JR — rs1!=0
					rd := rdf; if rd == 0 { rd = 8 }
					return cJR(rd), 0
				}
				// C.MV — rd!=0
				rd := rdf; if rd == 0 { rd = 8 }
				rs2 := rs2f; if rs2 == 0 { rs2 = 9 }
				return cMV(rd, rs2), 0
			}
			if rdf == 0 && rs2f == 0 { // C.EBREAK — skip, use C.ADD
				return cADD(8, 9), 0
			}
			if rs2f == 0 { // C.JALR
				rs1 := rdf; if rs1 == 0 { rs1 = 8 }
				return cJALR(rs1), 0
			}
			// C.ADD — rd!=0
			rd := rdf; if rd == 0 { rd = 8 }
			rs2 := rs2f; if rs2 == 0 { rs2 = 9 }
			return cADD(rd, rs2), 0
		case 0b110: // C.SWSP — rs2=x9 (holds b via harness)
			return cSWSP(9, 0), 9 // sp+0 = oracleDataVA+64
		case 0b111: // C.SDSP
			return cSDSP(9, 0), 9
		}
		return 0, 0
	}
	_ = baseAddr
	return 0, 0
}
