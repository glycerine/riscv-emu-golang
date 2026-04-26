package fuzzoracle

import (
	"encoding/binary"
	"testing"

	riscv "riscv"
)

// Store fuzz layout: 256-byte tiny memory.
//
//	0x00..0x7F  code  (ELF PT_LOAD segment)
//	0x80..0xFF  data  (loads/stores land here)
//
// Address clamping: baseAddr = 0x80 + (fuzz_byte & 0x78)
// 0x78 = 0111_1000 — keeps offset 8-byte aligned, max 0x78, so
// baseAddr+8 ≤ 0x80+0x78+8 = 0x100 = tinyMemSize exactly.
const (
	tinyMemSize = 256
	tinyCodeVA  = uint64(0x00)
	tinyDataVA  = uint64(0x80)
)

// FuzzStoresVsLibriscv fuzzes load/store instructions comparing our CPU
// against libriscv, with full 256-byte memory comparison after each step.
//
// Corpus: [insn:4][dataOff:1][rs2:8][initMem:up to 128 bytes]
func FuzzStoresVsLibriscv(f *testing.F) {
	seeds := []struct {
		insn    uint32
		dataOff uint8
		rs2val  uint64
		init    []byte
	}{
		{lenc(0x23, 0, 2, 3, 0), 0x00, 0xAB, []byte{0}},                                              // SB
		{lenc(0x23, 1, 2, 3, 0), 0x00, 0xABCD, []byte{0, 0}},                                         // SH
		{lenc(0x23, 2, 2, 3, 0), 0x00, 0xDEADBEEF, []byte{0, 0, 0, 0}},                               // SW
		{lenc(0x23, 3, 2, 3, 0), 0x00, 0xDEADBEEFCAFEBABE, make([]byte, 8)},                          // SD
		{lenc(0x03, 0, 1, 2, 0), 0x00, 0, []byte{0xFF}},                                              // LB
		{lenc(0x03, 4, 1, 2, 0), 0x00, 0, []byte{0xFF}},                                              // LBU
		{lenc(0x03, 1, 1, 2, 0), 0x00, 0, []byte{0x00, 0x80}},                                        // LH
		{lenc(0x03, 5, 1, 2, 0), 0x00, 0, []byte{0x00, 0x80}},                                        // LHU
		{lenc(0x03, 2, 1, 2, 0), 0x00, 0, []byte{0xFF, 0xFF, 0xFF, 0x80}},                            // LW
		{lenc(0x03, 6, 1, 2, 0), 0x00, 0, []byte{0xFF, 0xFF, 0xFF, 0x80}},                            // LWU
		{lenc(0x03, 3, 1, 2, 0), 0x00, 0, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE}},  // LD
	}
	for _, s := range seeds {
		var buf [32]byte
		binary.LittleEndian.PutUint32(buf[0:], s.insn)
		buf[4] = s.dataOff
		binary.LittleEndian.PutUint64(buf[5:], s.rs2val)
		copy(buf[13:], s.init)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 13 {
			return
		}

		insn := lsLegalise(binary.LittleEndian.Uint32(data[0:]))
		if insn == 0 {
			return
		}

		// Clamp base into data window, 8-byte aligned
		baseAddr := tinyDataVA + (uint64(data[4]) & 0x78)
		rs2val := binary.LittleEndian.Uint64(data[5:])

		initData := make([]byte, 128)
		if len(data) > 13 {
			copy(initData, data[13:])
		}

		// ELF: insn + ECALL, code at tinyCodeVA=0x00
		elf := riscv.BuildELF(tinyCodeVA, []uint32{insn, 0x00000073})

		// ── libriscv ─────────────────────────────────────────────────
		lm := NewMachine(elf)
		if lm == nil {
			return
		}
		defer lm.Close()

		if !lm.WriteGuest(tinyDataVA, initData) {
			return
		}
		var lInit [32]uint64
		lInit[2] = baseAddr
		lInit[3] = rs2val
		lm.SetRegsAndPC(lInit, tinyCodeVA)

		// ── our CPU ───────────────────────────────────────────────────
		mem, err := riscv.NewGuestMemory(tinyMemSize)
		if err != nil {
			t.Fatal(err)
		}
		defer mem.Free()

		ef, err := riscv.LoadELFBytes(mem, elf)
		if err != nil {
			return
		}
		if f := mem.WriteBytes(tinyDataVA, initData); f != nil {
			return
		}
		cpu := riscv.NewCPU(*mem)
		cpu.SetPC(ef.Entry)
		cpu.SetReg(2, baseAddr)
		cpu.SetReg(3, rs2val)

		// ── execute one instruction, compare everything ───────────────
		cpu.Step()
		lm.RunToEcall()

		lRegs := lm.SnapshotRegs()
		for r := 0; r < 32; r++ {
			if cpu.Reg(uint8(r)) != lRegs[r] {
				t.Fatalf("insn=0x%08X base=0x%02X rs2=0x%016X: x%d ours=0x%016X libriscv=0x%016X",
					insn, baseAddr, rs2val, r, cpu.Reg(uint8(r)), lRegs[r])
			}
		}

		// Full 256-byte memory comparison — cheap, catches any store bug
		lMem := lm.SnapshotMem(0, tinyMemSize)
		if lMem == nil {
			return
		}
		ourMem := make([]byte, tinyMemSize)
		if f := mem.ReadBytes(0, ourMem); f != nil {
			return
		}
		for i := range ourMem {
			if ourMem[i] != lMem[i] {
				t.Fatalf("insn=0x%08X base=0x%02X: mem[0x%02X] ours=0x%02X libriscv=0x%02X",
					insn, baseAddr, i, ourMem[i], lMem[i])
			}
		}
	})
}

// lsLegalise maps raw bytes to a load or store with imm=0, rs1=x2, rs2=x3.
func lsLegalise(raw uint32) uint32 {
	funct3 := uint8((raw >> 12) & 0x7)
	rd     := uint8((raw >> 7) & 0x1F)
	const rs1, rs2 = uint8(2), uint8(3)
	if rd == 0 || rd == 2 || rd == 3 { rd = 1 }
	switch raw & 1 {
	case 0: // LOAD
		if funct3 == 7 { funct3 = 6 }
		return lenc(0x03, funct3, rd, rs1, 0)
	default: // STORE
		if funct3 > 3 { funct3 &= 3 }
		return lenc(0x23, funct3, rs1, rs2, 0)
	}
}

