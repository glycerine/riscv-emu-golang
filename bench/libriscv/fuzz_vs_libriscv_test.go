//go:build libriscv

package libriscv_bench

import (
	"encoding/binary"
	"testing"

	riscv "riscv"
)

// Memory layout — kept small for fast full-region comparison.
const (
	fuzzMemSize   = 64 * 1024       // 64 KB total guest address space
	fuzzCodeVA    = uint64(0x10000) // code lives here
	fuzzScratchVA = uint64(0x11000) // stores target this region
	fuzzScratchSz = uint(0x1000)    // 4 KB scratch — compared after every step
)

// FuzzVsLibriscv feeds the same instruction sequence to both our CPU and
// libriscv, comparing all 32 registers, PC, and the full scratch memory
// region after every single instruction. Any divergence is a bug in our
// implementation.
//
// Corpus layout:
//
//	byte 0:         number of instructions (masked to 1..8)
//	bytes 1..N*4:   N raw instruction words (legalised before execution)
//	bytes N*4+1..:  initial scratch memory (up to 256 bytes, zero-padded)
func FuzzVsLibriscv(f *testing.F) {
	type seed struct {
		insns []uint32
	}
	seeds := []seed{
		{[]uint32{fenc(0x13, 0, 1, 2, 1)}},             // ADDI
		{[]uint32{fencR(0x33, 0, 0x00, 1, 2, 3)}},      // ADD
		{[]uint32{fencR(0x33, 0, 0x20, 1, 2, 3)}},      // SUB
		{[]uint32{fencR(0x33, 4, 0x00, 1, 2, 3)}},      // XOR
		{[]uint32{fencU(0x37, 1, 0x12345)}},             // LUI
		{[]uint32{fencU(0x17, 1, 0x12345)}},             // AUIPC
		{[]uint32{fencB(0x63, 0, 2, 3, 8)}},             // BEQ taken
		{[]uint32{fencB(0x63, 1, 2, 3, 8)}},             // BNE
		{[]uint32{fenc(0x1B, 0, 1, 2, -1)}},             // ADDIW
		{[]uint32{fencR(0x3B, 0, 0x00, 1, 2, 3)}},      // ADDW
		{[]uint32{fenc(0x13, 0, 1, 2, 1), fencR(0x33, 0, 0, 1, 1, 2)}}, // ADDI then ADD
	}
	for _, s := range seeds {
		buf := makeCorpus(s.insns, nil)
		f.Add(buf)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		nInsns := int(data[0]&0x7) + 1 // 1..8
		if len(data) < 1+nInsns*4 {
			return
		}

		// Decode and legalise instructions
		insns := make([]uint32, nInsns)
		for i := range nInsns {
			raw := binary.LittleEndian.Uint32(data[1+i*4:])
			insns[i] = fuzzLegalise(raw)
			if insns[i] == 0 {
				return
			}
		}

		// Initial scratch memory (up to 256 bytes)
		initScratch := make([]byte, fuzzScratchSz)
		if off := 1 + nInsns*4; off < len(data) {
			n := len(data) - off
			if n > 256 {
				n = 256
			}
			copy(initScratch, data[off:off+n])
		}

		// Build ELF: instructions + ECALL terminator
		code := append(append([]uint32{}, insns...), 0x00000073)
		elf := buildFuzzELF(code)

		// ── libriscv side ─────────────────────────────────────────────
		lm := NewMachine(elf, fuzzMemSize*8)
		if lm == nil {
			return
		}
		defer lm.Close()

		if !lm.WriteGuest(fuzzScratchVA, initScratch) {
			return
		}

		// Set x2 (sp) into scratch so stores have a valid target
		var initRegs [32]uint64
		initRegs[2] = fuzzScratchVA + uint64(fuzzScratchSz)/2
		lm.SetRegsAndPC(initRegs, fuzzCodeVA)

		// ── our CPU side ──────────────────────────────────────────────
		ourMem, err := riscv.NewGuestMemory(riscv.Size64MB)
		if err != nil {
			t.Fatal(err)
		}
		defer ourMem.Free()

		entry, err := riscv.LoadELFBytes(ourMem, elf)
		if err != nil {
			return
		}
		if f := ourMem.WriteBytes(fuzzScratchVA, initScratch); f != nil {
			return
		}

		cpu := riscv.NewCPU(*ourMem)
		cpu.SetPC(entry)
		cpu.SetReg(2, initRegs[2])

		// ── step and compare ──────────────────────────────────────────
		for step, insn := range insns {
			ourErr := cpu.Step()
			lm.Step1()

			// Snapshot libriscv state
			lRegs := lm.SnapshotRegs()
			lScratch := lm.SnapshotMem(fuzzScratchVA, fuzzScratchSz)

			// Compare registers
			for r := 0; r < 32; r++ {
				ours := cpu.Reg(uint8(r))
				theirs := lRegs[r]
				if ours != theirs {
					t.Fatalf("step %d insn=0x%08X: x%d ours=0x%016X libriscv=0x%016X",
						step, insn, r, ours, theirs)
				}
			}

			// Compare PC
			if cpu.PC() != lRegs[32] {
				t.Fatalf("step %d insn=0x%08X: PC ours=0x%016X libriscv=0x%016X",
					step, insn, cpu.PC(), lRegs[32])
			}

			// Compare scratch memory
			if lScratch != nil {
				ourScratch := make([]byte, fuzzScratchSz)
				if f := ourMem.ReadBytes(fuzzScratchVA, ourScratch); f == nil {
					for i := range ourScratch {
						if ourScratch[i] != lScratch[i] {
							t.Fatalf("step %d insn=0x%08X: scratch[0x%04x] ours=0x%02X libriscv=0x%02X",
								step, insn, i, ourScratch[i], lScratch[i])
						}
					}
				}
			}

			// Stop if our CPU halted (ECALL/EBREAK/fault)
			if ourErr != nil {
				return
			}
		}
	})
}

// makeCorpus encodes a seed into the fuzz corpus byte format.
func makeCorpus(insns []uint32, scratch []byte) []byte {
	buf := make([]byte, 1+len(insns)*4+len(scratch))
	buf[0] = byte(len(insns))
	for i, insn := range insns {
		binary.LittleEndian.PutUint32(buf[1+i*4:], insn)
	}
	copy(buf[1+len(insns)*4:], scratch)
	return buf
}

// buildFuzzELF constructs a minimal RV64 executable ELF with one PT_LOAD
// segment containing the given instructions at fuzzCodeVA.
func buildFuzzELF(code []uint32) []byte {
	const codeOff = 64 + 56 // ELF header + 1 program header
	codeBytes := make([]byte, len(code)*4)
	for i, insn := range code {
		binary.LittleEndian.PutUint32(codeBytes[i*4:], insn)
	}
	buf := make([]byte, codeOff+len(codeBytes))
	le := binary.LittleEndian

	// ELF header (64 bytes)
	copy(buf[0:], "\x7fELF")
	buf[4], buf[5], buf[6] = 2, 1, 1          // class64, LE, v1
	le.PutUint16(buf[16:], 2)                  // ET_EXEC
	le.PutUint16(buf[18:], 0xF3)               // EM_RISCV
	le.PutUint32(buf[20:], 1)                  // e_version
	le.PutUint64(buf[24:], fuzzCodeVA)         // e_entry
	le.PutUint64(buf[32:], 64)                 // e_phoff
	le.PutUint16(buf[52:], 64)                 // e_ehsize
	le.PutUint16(buf[54:], 56)                 // e_phentsize
	le.PutUint16(buf[56:], 1)                  // e_phnum

	// Program header (56 bytes at offset 64)
	ph := buf[64:]
	le.PutUint32(ph[0:], 1)                            // PT_LOAD
	le.PutUint32(ph[4:], 5)                            // PF_R|PF_X
	le.PutUint64(ph[8:], uint64(codeOff))              // p_offset
	le.PutUint64(ph[16:], fuzzCodeVA)                  // p_vaddr
	le.PutUint64(ph[24:], fuzzCodeVA)                  // p_paddr
	le.PutUint64(ph[32:], uint64(len(codeBytes)))      // p_filesz
	le.PutUint64(ph[40:], uint64(len(codeBytes)))      // p_memsz
	le.PutUint64(ph[48:], 0x1000)                      // p_align

	copy(buf[codeOff:], codeBytes)
	return buf
}

// fuzzLegalise maps arbitrary fuzz bytes to a valid instruction from our
// implemented set that is safe to run in both emulators:
//   - no CSR / privileged instructions
//   - no ECALL / EBREAK (would terminate the run early)
//   - branches use fixed +8 offset (stay within code region)
//   - no load/store (register values aren't guaranteed in-bounds;
//     stores are added once we add address clamping)
func fuzzLegalise(raw uint32) uint32 {
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

	switch (raw & 0xF) % 10 {
	case 0: // OP-IMM non-shift: ADDI/SLTI/SLTIU/XORI/ORI/ANDI
		if funct3 == 1 || funct3 == 5 { funct3 = 0 }
		return fenc(0x13, funct3, rd, rs1, imm12)
	case 1: // OP-IMM shift: SLLI/SRLI/SRAI
		if funct3 != 1 { funct3 = 5 }
		return fencShift(0x13, funct3, rd, rs1, shamt, srai)
	case 2: // OP R-type: ADD/SUB/SLL/SLT/SLTU/XOR/SRL/SRA/OR/AND
		if funct3 != 0 && funct3 != 5 { funct7 = 0 }
		if funct7 != 0 { funct7 = 0x20 }
		return fencR(0x33, funct3, funct7, rd, rs1, rs2)
	case 3: // LUI
		return fencU(0x37, rd, imm20)
	case 4: // AUIPC
		return fencU(0x17, rd, imm20)
	case 5: // BEQ  — fixed +8 offset so branch stays within code
		return fencB(0x63, 0, rs1, rs2, 8)
	case 6: // BNE
		return fencB(0x63, 1, rs1, rs2, 8)
	case 7: // BLT/BGE/BLTU/BGEU
		if funct3 < 4 { funct3 = 4 }
		return fencB(0x63, funct3, rs1, rs2, 8)
	case 8: // OP-IMM-32: ADDIW/SLLIW/SRLIW/SRAIW
		if funct3 != 0 && funct3 != 1 && funct3 != 5 { funct3 = 0 }
		if funct3 == 0 { return fenc(0x1B, 0, rd, rs1, imm12) }
		return fencShift(0x1B, funct3, rd, rs1, shamt&0x1F, srai)
	case 9: // OP-32: ADDW/SUBW/SLLW/SRLW/SRAW
		if funct3 != 0 && funct3 != 1 && funct3 != 5 { funct3 = 0 }
		f7 := uint8(0)
		if (raw>>30)&1 == 1 && (funct3 == 0 || funct3 == 5) { f7 = 0x20 }
		return fencR(0x3B, funct3, f7, rd, rs1, rs2)
	}
	return 0
}

// ── encoding helpers ──────────────────────────────────────────────────────

func fenc(opcode, funct3, rd, rs1 uint8, imm int32) uint32 {
	return uint32(imm)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}
func fencShift(opcode, funct3, rd, rs1, shamt uint8, srai bool) uint32 {
	var f7 uint32
	if srai { f7 = 0x20 }
	return f7<<25 | uint32(shamt)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}
func fencR(opcode, funct3, funct7, rd, rs1, rs2 uint8) uint32 {
	return uint32(funct7)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}
func fencB(opcode, funct3, rs1, rs2 uint8, offset int16) uint32 {
	o := uint32(offset)
	return ((o>>12)&1)<<31 | ((o>>5)&0x3F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(funct3)<<12 | ((o>>1)&0xF)<<8 | ((o>>11)&1)<<7 | uint32(opcode)
}
func fencU(opcode, rd uint8, imm20 uint32) uint32 {
	return (imm20&0xFFFFF)<<12 | uint32(rd)<<7 | uint32(opcode)
}
