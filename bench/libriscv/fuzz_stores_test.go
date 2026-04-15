//go:build libriscv

package libriscv_bench

import (
	"encoding/binary"
	"testing"

	riscv "riscv"
)

// FuzzStoresVsLibriscv fuzzes load/store instructions comparing our CPU
// against libriscv step-by-step, including a full memory snapshot after
// each instruction.
//
// Address clamping: rs1 is set to (fuzz_byte & memMask) before each run.
// memMask = memSize-1. Since memSize is a power of two, the AND is one
// instruction and the result is always a valid guest address. The store
// immediate is forced to 0 so the final address equals rs1 exactly.
// libriscv gets the same clamped address via SetRegsAndPC.
//
// We compare the entire guest memory after each instruction — same size
// for both emulators (fuzzMemSize), so the memcmp is an exact oracle.
func FuzzStoresVsLibriscv(f *testing.F) {
	// memMask clamps any uint64 into [0, fuzzMemSize).
	// This is exactly what GuestMemory.Mask() returns for a fuzzMemSize allocation.
	const memMask = uint64(fuzzMemSize - 1)

	seeds := []struct {
		insn   uint32
		rs1off uint8 // base address = rs1off & memMask (aligned down for width)
		rs2val uint64
		init   []byte // initial memory at address rs1off&memMask
	}{
		{fencS(0x23, 0, 2, 3, 0), 0x10, 0xAB, []byte{0}},                                               // SB
		{fencS(0x23, 1, 2, 3, 0), 0x10, 0xABCD, []byte{0, 0}},                                          // SH
		{fencS(0x23, 2, 2, 3, 0), 0x10, 0xDEADBEEF, []byte{0, 0, 0, 0}},                                // SW
		{fencS(0x23, 3, 2, 3, 0), 0x10, 0xDEADBEEFCAFEBABE, make([]byte, 8)},                           // SD
		{fencL(0x03, 0, 1, 2, 0), 0x10, 0, []byte{0xFF}},                                               // LB
		{fencL(0x03, 4, 1, 2, 0), 0x10, 0, []byte{0xFF}},                                               // LBU
		{fencL(0x03, 1, 1, 2, 0), 0x10, 0, []byte{0x00, 0x80}},                                         // LH
		{fencL(0x03, 5, 1, 2, 0), 0x10, 0, []byte{0x00, 0x80}},                                         // LHU
		{fencL(0x03, 2, 1, 2, 0), 0x10, 0, []byte{0xFF, 0xFF, 0xFF, 0x80}},                             // LW
		{fencL(0x03, 6, 1, 2, 0), 0x10, 0, []byte{0xFF, 0xFF, 0xFF, 0x80}},                             // LWU
		{fencL(0x03, 3, 1, 2, 0), 0x10, 0, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE}},   // LD
	}
	for _, s := range seeds {
		var buf [20]byte
		binary.LittleEndian.PutUint32(buf[0:], s.insn)
		buf[4] = s.rs1off
		binary.LittleEndian.PutUint64(buf[5:], s.rs2val)
		copy(buf[13:], s.init)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 13 {
			return
		}

		// Decode fuzz input
		rawInsn := binary.LittleEndian.Uint32(data[0:])
		rs1off  := uint64(data[4])
		rs2val  := binary.LittleEndian.Uint64(data[5:])

		// Legalise to a load or store with imm=0, rs1=x2, rs2=x3 (for stores)
		insn := legaliseLoadStore(rawInsn)
		if insn == 0 {
			return
		}

		// Clamp base address into guest memory using power-of-two mask.
		// Align down by 8 so that even a 64-bit access fits within memSize.
		baseAddr := (rs1off & memMask) &^ 7

		// Initial memory at baseAddr from fuzz data (up to 8 bytes)
		initMem := make([]byte, 8)
		if len(data) > 13 {
			n := len(data) - 13
			if n > 8 { n = 8 }
			copy(initMem, data[13:13+n])
		}

		// Build single-instruction ELF + ECALL
		elf := buildFuzzELF([]uint32{insn, 0x00000073})

		// ── libriscv ──────────────────────────────────────────────────
		lm := NewMachine(elf, fuzzMemSize*8)
		if lm == nil {
			return
		}
		defer lm.Close()

		if !lm.WriteGuest(baseAddr, initMem) {
			return
		}
		var lRegsInit [32]uint64
		lRegsInit[2] = baseAddr // x2 = base address
		lRegsInit[3] = rs2val  // x3 = store value
		lm.SetRegsAndPC(lRegsInit, fuzzCodeVA)

		// ── our CPU ───────────────────────────────────────────────────
		mem, err := riscv.NewGuestMemory(riscv.Size64MB)
		if err != nil {
			t.Fatal(err)
		}
		defer mem.Free()

		entry, err := riscv.LoadELFBytes(mem, elf)
		if err != nil {
			return
		}
		if f := mem.WriteBytes(baseAddr, initMem); f != nil {
			return
		}

		cpu := riscv.NewCPU(*mem)
		cpu.SetPC(entry)
		cpu.SetReg(2, baseAddr)
		cpu.SetReg(3, rs2val)

		// ── execute one instruction in both ───────────────────────────
		cpu.Step()
		lm.Step1()

		// Compare all registers
		lRegs := lm.SnapshotRegs()
		for r := 0; r < 32; r++ {
			ours := cpu.Reg(uint8(r))
			if ours != lRegs[r] {
				t.Fatalf("insn=0x%08X base=0x%016X rs2=0x%016X: x%d ours=0x%016X libriscv=0x%016X",
					insn, baseAddr, rs2val, r, ours, lRegs[r])
			}
		}
		if cpu.PC() != lRegs[32] {
			t.Fatalf("insn=0x%08X: PC ours=0x%016X libriscv=0x%016X",
				insn, cpu.PC(), lRegs[32])
		}

		// Compare the 8 bytes around baseAddr — where any load/store landed
		ourMem := make([]byte, 8)
		lMem := lm.SnapshotMem(baseAddr, 8)
		if lMem == nil {
			return
		}
		if f := mem.ReadBytes(baseAddr, ourMem); f != nil {
			return
		}
		for i := range ourMem {
			if ourMem[i] != lMem[i] {
				t.Fatalf("insn=0x%08X base=0x%016X: mem[%d] ours=0x%02X libriscv=0x%02X",
					insn, baseAddr, i, ourMem[i], lMem[i])
			}
		}
	})
}

// legaliseLoadStore maps raw fuzz bytes to a load or store with imm=0.
// rs1=x2 (base, set by harness), rs2=x3 (store value, set by harness).
// rd is derived from raw but kept away from x2/x3.
func legaliseLoadStore(raw uint32) uint32 {
	funct3 := uint8((raw >> 12) & 0x7)
	rd     := uint8((raw >> 7) & 0x1F)
	const rs1, rs2 = uint8(2), uint8(3)

	if rd == 0 || rd == 2 || rd == 3 { rd = 1 }

	switch (raw & 1) {
	case 0: // LOAD
		if funct3 == 7 { funct3 = 6 } // funct3=7 unused, map to LWU
		return fencL(0x03, funct3, rd, rs1, 0)
	case 1: // STORE
		if funct3 > 3 { funct3 &= 3 } // SB/SH/SW/SD only
		return fencS(0x23, funct3, rs1, rs2, 0)
	}
	return 0
}

// fencL encodes a LOAD I-type instruction.
func fencL(opcode, funct3, rd, rs1 uint8, imm int32) uint32 {
	return uint32(imm)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}

// fencS encodes a STORE S-type instruction.
func fencS(opcode, funct3, rs1, rs2 uint8, imm int32) uint32 {
	u := uint32(imm)
	return ((u>>5)&0x7F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(funct3)<<12 | (u&0x1F)<<7 | uint32(opcode)
}
// one PT_LOAD at tinyCodeVA containing the given instructions.
// The ELF is sized to fit within tinyMemSize.
func buildTinyELF(code []uint32) []byte {
	const codeOff = 64 + 56
	codeBytes := make([]byte, len(code)*4)
	for i, insn := range code {
		binary.LittleEndian.PutUint32(codeBytes[i*4:], insn)
	}
	if uint64(codeOff)+uint64(len(codeBytes)) > tinyDataVA {
		return nil // code would overlap data region
	}
	buf := make([]byte, codeOff+len(codeBytes))
	le := binary.LittleEndian

	// ELF header
	copy(buf[0:], "\x7fELF")
	buf[4], buf[5], buf[6] = 2, 1, 1
	le.PutUint16(buf[16:], 2)           // ET_EXEC
	le.PutUint16(buf[18:], 0xF3)        // EM_RISCV
	le.PutUint32(buf[20:], 1)
	le.PutUint64(buf[24:], tinyCodeVA)  // e_entry
	le.PutUint64(buf[32:], 64)          // e_phoff
	le.PutUint16(buf[52:], 64)
	le.PutUint16(buf[54:], 56)
	le.PutUint16(buf[56:], 1)

	// Program header
	ph := buf[64:]
	le.PutUint32(ph[0:], 1)                           // PT_LOAD
	le.PutUint32(ph[4:], 5)                           // PF_R|PF_X
	le.PutUint64(ph[8:], uint64(codeOff))             // p_offset
	le.PutUint64(ph[16:], tinyCodeVA)                 // p_vaddr
	le.PutUint64(ph[24:], tinyCodeVA)                 // p_paddr
	le.PutUint64(ph[32:], uint64(len(codeBytes)))     // p_filesz
	le.PutUint64(ph[40:], uint64(len(codeBytes)))     // p_memsz
	le.PutUint64(ph[48:], 0x10)                       // p_align

	copy(buf[codeOff:], codeBytes)
	return buf
}

// legaliseLoadStore maps a raw uint32 to a load or store with imm=0.
// rs1=x2 (base address set by harness), rs2=x3 (store value).
// rd avoids x2 and x3 to prevent clobbering the base/store registers.
func legaliseLoadStore(raw uint32) uint32 {
	funct3 := uint8((raw >> 12) & 0x7)
	rd     := uint8((raw >> 7) & 0x1F)
	const rs1, rs2 = uint8(2), uint8(3)

	if rd == 0 || rd == 2 || rd == 3 { rd = 1 }

	switch raw & 1 {
	case 0: // LOAD: LB/LH/LW/LD/LBU/LHU/LWU
		if funct3 == 7 { funct3 = 6 } // funct3=7 unused → LWU
		return fencL(0x03, funct3, rd, rs1, 0)
	default: // STORE: SB/SH/SW/SD
		if funct3 > 3 { funct3 &= 3 }
		return fencS(0x23, funct3, rs1, rs2, 0)
	}
}

// fencL encodes a LOAD I-type instruction (opcode=0x03).
func fencL(opcode, funct3, rd, rs1 uint8, imm int32) uint32 {
	return uint32(imm)<<20 | uint32(rs1)<<15 | uint32(funct3)<<12 | uint32(rd)<<7 | uint32(opcode)
}

// fencS encodes a STORE S-type instruction (opcode=0x23).
func fencS(opcode, funct3, rs1, rs2 uint8, imm int32) uint32 {
	u := uint32(imm)
	return ((u>>5)&0x7F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(funct3)<<12 | (u&0x1F)<<7 | uint32(opcode)
}
