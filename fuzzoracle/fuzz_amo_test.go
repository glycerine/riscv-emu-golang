package fuzzoracle

import (
	"encoding/binary"
	"testing"

	riscv "riscv"
)

// FuzzAMOVsLibriscv compares RV64A atomic memory operations against libriscv.
//
// Design:
//   - Address is always oracleDataVA (valid, 8-byte aligned)
//   - rs1=x2 (holds oracleDataVA), rs2=x3 (operand), rd=x1 (result)
//   - Initial 8 bytes of memory come from fuzz input
//   - LR/SC pairs are tested as a unit (two-instruction sequences)
//   - SC alone (without prior LR) always fails — both sides agree
//   - libriscv signals on out-of-bounds; we pin address in-bounds via x2
//
// Corpus: [op:1][width:1][mem8:8][rs2val:8]
//   op    — selects AMO operation (0..10)
//   width — 0=.W, 1=.D
//   mem8  — initial 8 bytes at oracleDataVA
//   rs2val — value in x3 (operand for AMO, store value for SC)

func FuzzAMOVsLibriscv(f *testing.F) {
	type seed struct {
		op     uint8
		width  uint8 // 0=W, 1=D
		mem    uint64
		rs2val uint64
	}
	seeds := []seed{
		{0, 0, 5, 10},                          // AMOADD.W  5+10=15
		{0, 1, 200, 100},                        // AMOADD.D  200+100=300
		{1, 0, 0xDEADBEEF, 0xCAFEBABE},          // AMOSWAP.W
		{1, 1, 0xDEADBEEFCAFEBABE, 0x1234},      // AMOSWAP.D
		{2, 0, 0xAA, 0x55},                      // AMOXOR.W
		{2, 1, 0xAAAAAAAAAAAAAAAA, 0x5555555555555555}, // AMOXOR.D
		{3, 0, 0xFF, 0x0F},                      // AMOAND.W
		{3, 1, 0xFFFFFFFFFFFFFFFF, 0xFF00FF00FF00FF00}, // AMOAND.D
		{4, 0, 0x0F, 0xF0},                      // AMOOR.W
		{4, 1, 0, 0xDEADBEEFCAFEBABE},           // AMOOR.D
		{5, 0, 5, 0xFFFFFFFFFFFFFFFF},           // AMOMIN.W  min(5,-1)=-1
		{5, 1, 5, 0xFFFFFFFFFFFFFFFF},           // AMOMIN.D
		{6, 0, 5, 10},                           // AMOMAX.W  max(5,10)=10
		{6, 1, 0xFFFFFFFFFFFFFFFF, 5},           // AMOMAX.D  max(-1,5)=5
		{7, 0, 0xFFFFFFFF, 5},                   // AMOMINU.W  minu(max,5)=5
		{7, 1, 0xFFFFFFFFFFFFFFFF, 5},           // AMOMINU.D
		{8, 0, 5, 0xFFFFFFFF},                   // AMOMAXU.W  maxu(5,max)=max
		{8, 1, 5, 0xFFFFFFFFFFFFFFFF},           // AMOMAXU.D
		// LR/SC pair
		{9,  0, 0xDEADBEEF, 0xCAFEBABE},        // LR.W + SC.W
		{9,  1, 0xDEADBEEFCAFEBABE, 0x1234},    // LR.D + SC.D
		// LR alone
		{10, 0, 0xDEADBEEF, 0},                 // LR.W
		{10, 1, 0xDEADBEEFCAFEBABE, 0},         // LR.D
	}
	for _, s := range seeds {
		var buf [18]byte
		buf[0] = s.op
		buf[1] = s.width
		binary.LittleEndian.PutUint64(buf[2:], s.mem)
		binary.LittleEndian.PutUint64(buf[10:], s.rs2val)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 18 {
			return
		}

		op     := data[0] % 11      // 0..10
		wideD  := data[1]&1 == 1    // false=.W, true=.D
		mem8   := data[2:10]        // initial memory
		rs2val := binary.LittleEndian.Uint64(data[10:])

		f3 := uint8(0b010) // .W
		if wideD { f3 = 0b011 } // .D

		// Build instruction(s)
		// rs1=x2 (oracleDataVA), rs2=x3 (operand), rd=x1
		var insns []uint32
		switch op {
		case 9: // LR + SC pair
			insns = []uint32{
				amo(0b00010, f3, 0, 2, 0), // LR rd=x0 (discard), rs1=x2
				amo(0b00011, f3, 1, 2, 3), // SC rd=x1 (result), rs1=x2, rs2=x3
			}
		case 10: // LR alone
			insns = []uint32{amo(0b00010, f3, 1, 2, 0)}
		default: // AMO operation
			funct5s := []uint8{
				0b00000, // 0: AMOADD
				0b00001, // 1: AMOSWAP
				0b00100, // 2: AMOXOR
				0b01100, // 3: AMOAND
				0b01000, // 4: AMOOR
				0b10000, // 5: AMOMIN
				0b10100, // 6: AMOMAX
				0b11000, // 7: AMOMINU
				0b11100, // 8: AMOMAXU
			}
			insns = []uint32{amo(funct5s[op], f3, 1, 2, 3)}
		}

		// Append ECALL for libriscv termination
		code := append(append([]uint32{}, insns...), 0x00000073)
		elf  := riscv.BuildELF(oracleCodeVA, code)

		// ── libriscv ─────────────────────────────────────────────────
		lm := NewMachine(elf)
		if lm == nil { return }
		defer lm.Close()

		initMem := make([]byte, 128)
		copy(initMem, mem8)
		lm.WriteGuest(oracleDataVA, initMem)

		var lInit [32]uint64
		lInit[2] = oracleDataVA // rs1: address
		lInit[3] = rs2val       // rs2: operand / store value
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
		cpu.SetReg(2, oracleDataVA)
		cpu.SetReg(3, rs2val)

		for range len(insns) {
			cpu.Step()
		}

		// Compare registers
		for r := 0; r < 32; r++ {
			if cpu.Reg(uint8(r)) != lRegs[r] {
				t.Fatalf("op=%d width=%v mem=0x%016X rs2=0x%016X: x%d ours=0x%016X libriscv=0x%016X",
					op, wideD, binary.LittleEndian.Uint64(mem8), rs2val,
					r, cpu.Reg(uint8(r)), lRegs[r])
			}
		}

		// Compare memory
		ourMem := make([]byte, oracleMemSize)
		if lMem != nil {
			if f := mem.ReadBytes(0, ourMem); f == nil {
				for i := range ourMem {
					if ourMem[i] != lMem[i] {
						t.Fatalf("op=%d width=%v: mem[0x%05X] ours=0x%02X libriscv=0x%02X",
							op, wideD, i, ourMem[i], lMem[i])
					}
				}
			}
		}
	})
}
