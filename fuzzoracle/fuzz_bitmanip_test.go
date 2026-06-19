package fuzzoracle

import (
	"encoding/binary"
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

// FuzzBitmanipVsLibriscv fuzzes Zicsr, Zba, and Zbs instructions against libriscv.
// Zbb instructions that libriscv doesn't implement (CLZ/CTZ/CPOP/ORC.B/REV8/RORx)
// are omitted — those are tested directly in cpu_test.go.
//
// Corpus: [family:1][a:8][b:8]
//   family — selects instruction
//   a      — value for rs1 / x2 (source)
//   b      — value for rs2 / x3 (second source or CSR write value)

func FuzzBitmanipVsLibriscv(f *testing.F) {
	type seed struct {
		family uint8
		a, b   uint64
	}
	seeds := []seed{
		// Zicsr
		{0, 0, 0x1F}, // CSRRW fcsr
		{1, 0, 0x7},  // CSRRS fcsr
		{2, 0, 0x3},  // CSRRC fcsr
		// Zba
		{3, 10, 100},                 // SH1ADD
		{4, 10, 100},                 // SH2ADD
		{5, 10, 100},                 // SH3ADD
		{6, 0xDEADBEEFCAFEBABE, 100}, // ADD.UW
		{7, 0xDEADBEEF0000000A, 100}, // SH1ADD.UW
		// Zbb logic
		{8, 0xFF, 0x0F},     // ANDN
		{9, 0xF0, 0xF0},     // ORN
		{10, 0xAA, 0xAA},    // XNOR
		{11, ^uint64(0), 1}, // MAX
		{12, ^uint64(0), 1}, // MAXU
		{13, ^uint64(0), 1}, // MIN
		{14, ^uint64(0), 1}, // MINU
		// Zbb rotate
		{15, 1, 4},                  // ROL
		{16, 0x10, 4},               // ROR
		{17, 0xF000000000000000, 4}, // RORI imm=4
		// Zbb sign-extend
		{18, 0xFF, 0},               // SEXT.B
		{19, 0x8000, 0},             // SEXT.H
		{20, 0xDEADBEEFFFFF8000, 0}, // ZEXT.H
		// Zbs
		{21, 0, 5},    // BSET
		{22, 0xFF, 3}, // BCLR
		{23, 0xFF, 0}, // BINV
		{24, 0xF0, 7}, // BEXT
		{25, 0, 5},    // BSETI
		{26, 0xFF, 3}, // BCLRI
		// Zbc
		{27, 2, 25},                          // CLMUL
		{28, 0xDEADBEEFCAFEBABE, 0x12345678}, // CLMULR
		{29, 0xDEADBEEFCAFEBABE, 0x12345678}, // CLMULH
		// Zicond
		{30, 42, 0}, // CZERO.EQZ (rs2=0)
		{31, 42, 1}, // CZERO.NEZ (rs2!=0)
	}
	for _, s := range seeds {
		var buf [17]byte
		buf[0] = s.family
		binary.LittleEndian.PutUint64(buf[1:], s.a)
		binary.LittleEndian.PutUint64(buf[9:], s.b)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 17 {
			return
		}
		family := data[0] % 32
		a := binary.LittleEndian.Uint64(data[1:])
		b := binary.LittleEndian.Uint64(data[9:])

		var insn uint32
		initRegs := regs(2, a, 3, b)

		switch family {
		// ── Zicsr ─────────────────────────────────────────────────────────
		case 0: // CSRRW x1, fcsr, x2
			insn = csr(0x003, 2, 1, 1)
			initRegs[2] = b & 0xFF // fcsr is 8 bits
		case 1: // CSRRS x1, fcsr, x2
			insn = csr(0x003, 2, 2, 1)
			initRegs[2] = b & 0xFF
		case 2: // CSRRC x1, fcsr, x2
			insn = csr(0x003, 2, 3, 1)
			initRegs[2] = b & 0xFF
		// ── Zba ───────────────────────────────────────────────────────────
		case 3:
			insn = brt(0x10, 2, 1, 2, 3) // SH1ADD
		case 4:
			insn = brt(0x10, 4, 1, 2, 3) // SH2ADD
		case 5:
			insn = brt(0x10, 6, 1, 2, 3) // SH3ADD
		case 6:
			insn = brt(0x04, 0, 1, 2, 3, 0x3B) // ADD.UW
		case 7:
			insn = brt(0x10, 2, 1, 2, 3, 0x3B) // SH1ADD.UW
		case 8:
			insn = brt(0x10, 4, 1, 2, 3, 0x3B) // SH2ADD.UW
		case 9:
			insn = brt(0x10, 6, 1, 2, 3, 0x3B) // SH3ADD.UW
		// ── Zbb logic ─────────────────────────────────────────────────────
		case 10:
			insn = brt(0x20, 7, 1, 2, 3) // ANDN
		case 11:
			insn = brt(0x20, 6, 1, 2, 3) // ORN
		case 12:
			insn = brt(0x20, 4, 1, 2, 3) // XNOR
		case 13:
			insn = brt(0x05, 6, 1, 2, 3) // MAX
		case 14:
			insn = brt(0x05, 7, 1, 2, 3) // MAXU
		case 15:
			insn = brt(0x05, 4, 1, 2, 3) // MIN
		case 16:
			insn = brt(0x05, 5, 1, 2, 3) // MINU
		// ── Zbb rotate / sign-extend ──────────────────────────────────────
		case 17:
			insn = brt(0x30, 1, 1, 2, 3) // ROL
		case 18:
			insn = brt(0x30, 5, 1, 2, 3) // ROR
		case 19:
			insn = bimm(0x30, 5, 1, 2, int(b&63)) // RORI
		case 20:
			insn = bimm(0x30, 1, 1, 2, 4) // SEXT.B
		case 21:
			insn = bimm(0x30, 1, 1, 2, 5) // SEXT.H
		case 22:
			insn = brt(0x04, 4, 1, 2, 0, 0x3B) // ZEXT.H
		// ── Zbs ───────────────────────────────────────────────────────────
		case 23:
			insn = brt(0x14, 1, 1, 2, 3) // BSET
		case 24:
			insn = brt(0x24, 1, 1, 2, 3) // BCLR
		case 25:
			insn = brt(0x34, 1, 1, 2, 3) // BINV
		case 26:
			insn = brt(0x24, 5, 1, 2, 3) // BEXT
		// ── Zbc ───────────────────────────────────────────────────────────
		case 27:
			insn = brt(0x05, 1, 1, 2, 3) // CLMUL
		case 28:
			insn = brt(0x05, 2, 1, 2, 3) // CLMULR
		case 29:
			insn = brt(0x05, 3, 1, 2, 3) // CLMULH
		// ── Zicond ────────────────────────────────────────────────────────
		case 30:
			insn = brt(0x07, 5, 1, 2, 3) // CZERO.EQZ
		case 31:
			insn = brt(0x07, 7, 1, 2, 3) // CZERO.NEZ
		default:
			return
		}

		// Run against libriscv
		elf := riscv.BuildELF(oracleCodeVA, []uint32{insn, 0x00000073})
		lm := NewMachine(elf)
		if lm == nil {
			return
		}
		defer lm.Close()
		lm.SetRegsAndPC(initRegs, oracleCodeVA)
		lm.RunToEcall()
		lRegs := lm.SnapshotRegs()

		// Run our CPU
		mem, err := riscv.NewGuestMemory(oracleMemSize)
		if err != nil {
			t.Fatal(err)
		}
		defer mem.Free()
		riscv.LoadELFBytes(mem, elf)
		cpu := riscv.NewCPU(*mem)
		cpu.SetPC(oracleCodeVA)
		for r := uint8(1); r < 32; r++ {
			cpu.SetReg(r, initRegs[r])
		}
		cpu.Step()

		for r := 0; r < 32; r++ {
			if cpu.Reg(uint8(r)) != lRegs[r] {
				t.Fatalf("family=%d a=0x%016X b=0x%016X: x%d ours=0x%016X libriscv=0x%016X",
					family, a, b, r, cpu.Reg(uint8(r)), lRegs[r])
			}
		}
	})
}
