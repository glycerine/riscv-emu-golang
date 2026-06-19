package fuzzoracle

// fence_rvc_test.go — RED unit tests for FENCE, FENCE.I, and all RVC
// (compressed) instructions. All tests compare our CPU against libriscv.
//
// RVC instructions are 16-bit. The CPU must fetch them via Fetch16 at
// unaligned-to-32-bit addresses. They expand to their 32-bit equivalents
// before execution.
//
// Register conventions in tests:
//   x8 (s0) is the first compressed-only register (maps to reg' = 0)
//   x9..x15 map to reg' = 1..7
//   x2 = sp, used for stack-relative loads/stores

import (
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

// ── FENCE / Zifencei ─────────────────────────────────────────────────────
// Both are no-ops in our single-threaded emulator but must not fault.

func TestFENCE_NoFault(t *testing.T) {
	// FENCE iorw,iorw = 0x0FF0000F must execute as nop, not fault.
	// We verify PC advanced by 4 (proving execution, not ErrIllegalInstruction).
	runFENCE(t, 0x0FF0000F)
}

func TestFENCE_I_NoFault(t *testing.T) {
	// FENCE.I = 0x0000100F must execute as nop, not fault.
	runFENCE(t, 0x0000100F)
}

func runFENCE(t *testing.T, insn uint32) {
	t.Helper()
	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	elf := riscv.BuildELF(oracleCodeVA, []uint32{insn, 0x00000073})
	riscv.LoadELFBytes(mem, elf)
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	if err := cpu.Step(); err != nil {
		t.Errorf("FENCE 0x%08X faulted: %v (want nop)", insn, err)
	}
	if cpu.PC() != oracleCodeVA+4 {
		t.Errorf("FENCE 0x%08X: PC=0x%X want 0x%X", insn, cpu.PC(), oracleCodeVA+4)
	}
}

// ── RVC helpers ───────────────────────────────────────────────────────────
// runRVC wraps a 16-bit compressed instruction alongside a 32-bit ECALL
// for libriscv termination. Both emulators execute the compressed insn
// and we compare register + memory state.

// runRVC runs a 16-bit compressed instruction against both our CPU and libriscv.
func runRVC(t *testing.T, insn16 uint16, initRegs [32]uint64, initMem []byte) {
	t.Helper()
	runOneRVC(t, insn16, initRegs, initMem)
}

// runOneRVC is the oracle runner for compressed instructions.
func runOneRVC(t *testing.T, insn16 uint16, initRegs [32]uint64, initMem []byte) {
	t.Helper()

	// Pack: [insn16][C.EBREAK=0x9002] into first 32-bit word so both fit
	// at oracleCodeVA. The second word is a full ECALL for libriscv.
	word0 := uint32(insn16) | (uint32(0x9002) << 16)
	elf := riscv.BuildELF(oracleCodeVA, []uint32{word0, 0x00000073})

	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed")
	}
	defer lm.Close()

	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		lm.WriteGuest(oracleDataVA, padded)
	}
	lm.SetRegsAndPC(initRegs, oracleCodeVA)
	lm.RunToEcall()
	lRegs := lm.SnapshotRegs()

	// Our CPU — needs to handle 16-bit fetch and decode
	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	riscv.LoadELFBytes(mem, elf)
	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		mem.WriteBytes(oracleDataVA, padded)
	}

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	for r := uint8(1); r < 32; r++ {
		cpu.SetReg(r, initRegs[r])
	}
	cpu.Step() // execute the compressed instruction

	// Compare registers
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lRegs[r])
		}
	}

	// Compare memory
	lMem := lm.SnapshotMem(0, oracleMemSize)
	ourMem := make([]byte, oracleMemSize)
	if lMem != nil {
		if f := mem.ReadBytes(0, ourMem); f == nil {
			for i := range ourMem {
				if ourMem[i] != lMem[i] {
					t.Errorf("mem[0x%05X]: ours=0x%02X libriscv=0x%02X", i, ourMem[i], lMem[i])
					break
				}
			}
		}
	}
}

// ── Quadrant 0 ────────────────────────────────────────────────────────────

func TestC_ADDI4SPN(t *testing.T) {
	// C.ADDI4SPN x8, sp, 4  — rd'=0 (x8), nzuimm=4
	// sp=oracleDataVA so result is in bounds
	runRVC(t, cADDI4SPN(0, 4), regs(2, oracleDataVA), nil)
}
func TestC_LW(t *testing.T) {
	// C.LW x8, 0(x9)  — rd'=0(x8), rs1'=1(x9), uimm=0
	runRVC(t, cLW(0, 1, 0), regs(9, oracleDataVA), []byte{0xEF, 0xBE, 0xAD, 0xDE})
}
func TestC_LD(t *testing.T) {
	// C.LD x8, 0(x9)
	runRVC(t, cLD(0, 1, 0), regs(9, oracleDataVA), []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE})
}
func TestC_SW(t *testing.T) {
	// C.SW x10, 0(x9)  — rs2'=2(x10), rs1'=1(x9), uimm=0
	runRVC(t, cSW(1, 2, 0), regs(9, oracleDataVA, 10, 0xDEADBEEF), make([]byte, 8))
}
func TestC_SD(t *testing.T) {
	// C.SD x10, 0(x9)
	runRVC(t, cSD(1, 2, 0), regs(9, oracleDataVA, 10, 0xDEADBEEFCAFEBABE), make([]byte, 8))
}

// ── Quadrant 1 ────────────────────────────────────────────────────────────

func TestC_NOP(t *testing.T)           { runRVC(t, 0x0001, regs(), nil) }
func TestC_ADDI_Pos(t *testing.T)      { runRVC(t, cADDI(1, 5), regs(1, 10), nil) }
func TestC_ADDI_Neg(t *testing.T)      { runRVC(t, cADDI(1, -1), regs(1, 0), nil) }
func TestC_ADDIW(t *testing.T)         { runRVC(t, cADDIW(1, 1), regs(1, 0x7FFFFFFF), nil) }
func TestC_LI(t *testing.T)            { runRVC(t, cLI(1, 42), regs(), nil) }
func TestC_LI_Neg(t *testing.T)        { runRVC(t, cLI(1, -1), regs(), nil) }
func TestC_ADDI16SP(t *testing.T)      { runRVC(t, cADDI16SP(-16), regs(2, 256), nil) }
func TestC_LUI(t *testing.T)           { runRVC(t, cLUI(1, 1), regs(), nil) }
func TestC_SRLI(t *testing.T)          { runRVC(t, cSRLI(0, 4), regs(8, 0xFF), nil) }
func TestC_SRAI(t *testing.T)          { runRVC(t, cSRAI(0, 4), regs(8, 0xFFFFFFFFFFFFFFFF), nil) }
func TestC_ANDI(t *testing.T)          { runRVC(t, cANDI(0, 0xF), regs(8, 0xFF), nil) }
func TestC_SUB(t *testing.T)           { runRVC(t, cSUB(0, 1), regs(8, 10, 9, 3), nil) }
func TestC_XOR(t *testing.T)           { runRVC(t, cXOR(0, 1), regs(8, 0xAA, 9, 0x55), nil) }
func TestC_OR(t *testing.T)            { runRVC(t, cOR(0, 1), regs(8, 0xF0, 9, 0x0F), nil) }
func TestC_AND(t *testing.T)           { runRVC(t, cAND(0, 1), regs(8, 0xFF, 9, 0x0F), nil) }
func TestC_SUBW(t *testing.T)          { runRVC(t, cSUBW(0, 1), regs(8, 10, 9, 3), nil) }
func TestC_ADDW(t *testing.T)          { runRVC(t, cADDW(0, 1), regs(8, 0x7FFFFFFF, 9, 1), nil) }
func TestC_J(t *testing.T)             { runRVC(t, cJ(4), regs(), nil) } // +4: skip C.EBREAK, hit ECALL
func TestC_BEQZ_Taken(t *testing.T)    { runRVC(t, cBEQZ(0, 4), regs(8, 0), nil) }
func TestC_BEQZ_NotTaken(t *testing.T) { runRVC(t, cBEQZ(0, 4), regs(8, 1), nil) }
func TestC_BNEZ_Taken(t *testing.T)    { runRVC(t, cBNEZ(0, 4), regs(8, 1), nil) }
func TestC_BNEZ_NotTaken(t *testing.T) { runRVC(t, cBNEZ(0, 4), regs(8, 0), nil) }

// ── Quadrant 2 ────────────────────────────────────────────────────────────

func TestC_SLLI(t *testing.T) { runRVC(t, cSLLI(1, 3), regs(1, 1), nil) }
func TestC_LWSP(t *testing.T) {
	runRVC(t, cLWSP(1, 0), regs(2, oracleDataVA), []byte{0xEF, 0xBE, 0xAD, 0xDE})
}
func TestC_LDSP(t *testing.T) {
	runRVC(t, cLDSP(1, 0), regs(2, oracleDataVA), []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE})
}
func TestC_JR(t *testing.T)  { runRVC(t, cJR(1), regs(1, oracleCodeVA+4), nil) } // jump to C.EBREAK then ECALL
func TestC_MV(t *testing.T)  { runRVC(t, cMV(1, 2), regs(2, 42), nil) }
func TestC_ADD(t *testing.T) { runRVC(t, cADD(1, 2), regs(1, 10, 2, 20), nil) }
func TestC_SWSP(t *testing.T) {
	runRVC(t, cSWSP(1, 0), regs(2, oracleDataVA, 1, 0xDEADBEEF), make([]byte, 8))
}
func TestC_SDSP(t *testing.T) {
	runRVC(t, cSDSP(1, 0), regs(2, oracleDataVA, 1, 0xDEADBEEFCAFEBABE), make([]byte, 8))
}
func TestC_JALR(t *testing.T) { runRVC(t, cJALR(1), regs(1, oracleCodeVA+4), nil) }

// ── RVC 16-bit instruction encoders ──────────────────────────────────────

func b(val, hi, lo int) uint16 {
	return uint16((val >> lo) & ((1 << (hi - lo + 1)) - 1))
}

// Quadrant 0
func cADDI4SPN(rd, nzuimm int) uint16 {
	return (0b000 << 13) | (b(nzuimm, 5, 4) << 11) | (b(nzuimm, 9, 6) << 7) |
		(b(nzuimm, 2, 2) << 6) | (b(nzuimm, 3, 3) << 5) | (uint16(rd&7) << 2) | 0b00
}
func cLW(rd, rs1, uimm int) uint16 {
	return (0b010 << 13) | (b(uimm, 5, 3) << 10) | (uint16(rs1&7) << 7) |
		(b(uimm, 2, 2) << 6) | (b(uimm, 6, 6) << 5) | (uint16(rd&7) << 2) | 0b00
}
func cLD(rd, rs1, uimm int) uint16 {
	return (0b011 << 13) | (b(uimm, 5, 3) << 10) | (uint16(rs1&7) << 7) |
		(b(uimm, 7, 6) << 5) | (uint16(rd&7) << 2) | 0b00
}
func cSW(rs1, rs2, uimm int) uint16 {
	return (0b110 << 13) | (b(uimm, 5, 3) << 10) | (uint16(rs1&7) << 7) |
		(b(uimm, 2, 2) << 6) | (b(uimm, 6, 6) << 5) | (uint16(rs2&7) << 2) | 0b00
}
func cSD(rs1, rs2, uimm int) uint16 {
	return (0b111 << 13) | (b(uimm, 5, 3) << 10) | (uint16(rs1&7) << 7) |
		(b(uimm, 7, 6) << 5) | (uint16(rs2&7) << 2) | 0b00
}

// Quadrant 1
func cADDI(rd, nzimm int) uint16 {
	u := nzimm & 0x3F
	return (0b000 << 13) | (b(u, 5, 5) << 12) | (uint16(rd&31) << 7) | (b(u, 4, 0) << 2) | 0b01
}
func cADDIW(rd, imm int) uint16 {
	u := imm & 0x3F
	return (0b001 << 13) | (b(u, 5, 5) << 12) | (uint16(rd&31) << 7) | (b(u, 4, 0) << 2) | 0b01
}
func cLI(rd, imm int) uint16 {
	u := imm & 0x3F
	return (0b010 << 13) | (b(u, 5, 5) << 12) | (uint16(rd&31) << 7) | (b(u, 4, 0) << 2) | 0b01
}
func cADDI16SP(nzimm int) uint16 {
	u := nzimm & 0x3FF
	return (0b011 << 13) | (b(u, 9, 9) << 12) | (2 << 7) |
		(b(u, 4, 4) << 6) | (b(u, 6, 6) << 5) | (b(u, 8, 7) << 3) | (b(u, 5, 5) << 2) | 0b01
}
func cLUI(rd, nzimm int) uint16 {
	u := nzimm & 0x3F
	return (0b011 << 13) | (b(u, 5, 5) << 12) | (uint16(rd&31) << 7) | (b(u, 4, 0) << 2) | 0b01
}
func cSRLI(rs1, shamt int) uint16 {
	return (0b100 << 13) | (b(shamt, 5, 5) << 12) | (0b00 << 10) | (uint16(rs1&7) << 7) | (b(shamt, 4, 0) << 2) | 0b01
}
func cSRAI(rs1, shamt int) uint16 {
	return (0b100 << 13) | (b(shamt, 5, 5) << 12) | (0b01 << 10) | (uint16(rs1&7) << 7) | (b(shamt, 4, 0) << 2) | 0b01
}
func cANDI(rs1, imm int) uint16 {
	u := imm & 0x3F
	return (0b100 << 13) | (b(u, 5, 5) << 12) | (0b10 << 10) | (uint16(rs1&7) << 7) | (b(u, 4, 0) << 2) | 0b01
}
func cSUB(rd, rs2 int) uint16 {
	return (0b100 << 13) | (0b11 << 10) | (uint16(rd&7) << 7) | (0b00 << 5) | (uint16(rs2&7) << 2) | 0b01
}
func cXOR(rd, rs2 int) uint16 {
	return (0b100 << 13) | (0b11 << 10) | (uint16(rd&7) << 7) | (0b01 << 5) | (uint16(rs2&7) << 2) | 0b01
}
func cOR(rd, rs2 int) uint16 {
	return (0b100 << 13) | (0b11 << 10) | (uint16(rd&7) << 7) | (0b10 << 5) | (uint16(rs2&7) << 2) | 0b01
}
func cAND(rd, rs2 int) uint16 {
	return (0b100 << 13) | (0b11 << 10) | (uint16(rd&7) << 7) | (0b11 << 5) | (uint16(rs2&7) << 2) | 0b01
}
func cSUBW(rd, rs2 int) uint16 {
	return (0b100 << 13) | (1 << 12) | (0b11 << 10) | (uint16(rd&7) << 7) | (0b00 << 5) | (uint16(rs2&7) << 2) | 0b01
}
func cADDW(rd, rs2 int) uint16 {
	return (0b100 << 13) | (1 << 12) | (0b11 << 10) | (uint16(rd&7) << 7) | (0b01 << 5) | (uint16(rs2&7) << 2) | 0b01
}
func cJ(offset int) uint16 {
	o := offset & 0xFFF
	return (0b101 << 13) | (b(o, 11, 11) << 12) | (b(o, 4, 4) << 11) |
		(b(o, 9, 8) << 9) | (b(o, 10, 10) << 8) | (b(o, 6, 6) << 7) |
		(b(o, 7, 7) << 6) | (b(o, 3, 1) << 3) | (b(o, 5, 5) << 2) | 0b01
}
func cBEQZ(rs1, offset int) uint16 {
	o := offset & 0x1FF
	return (0b110 << 13) | (b(o, 8, 8) << 12) | (b(o, 4, 3) << 10) | (uint16(rs1&7) << 7) |
		(b(o, 7, 6) << 5) | (b(o, 2, 1) << 3) | (b(o, 5, 5) << 2) | 0b01
}
func cBNEZ(rs1, offset int) uint16 {
	o := offset & 0x1FF
	return (0b111 << 13) | (b(o, 8, 8) << 12) | (b(o, 4, 3) << 10) | (uint16(rs1&7) << 7) |
		(b(o, 7, 6) << 5) | (b(o, 2, 1) << 3) | (b(o, 5, 5) << 2) | 0b01
}

// Quadrant 2
func cSLLI(rd, shamt int) uint16 {
	return (0b000 << 13) | (b(shamt, 5, 5) << 12) | (uint16(rd&31) << 7) | (b(shamt, 4, 0) << 2) | 0b10
}
func cLWSP(rd, uimm int) uint16 {
	return (0b010 << 13) | (b(uimm, 5, 5) << 12) | (uint16(rd&31) << 7) |
		(b(uimm, 4, 2) << 4) | (b(uimm, 7, 6) << 2) | 0b10
}
func cLDSP(rd, uimm int) uint16 {
	return (0b011 << 13) | (b(uimm, 5, 5) << 12) | (uint16(rd&31) << 7) |
		(b(uimm, 4, 3) << 5) | (b(uimm, 8, 6) << 2) | 0b10
}
func cJR(rs1 int) uint16 { return (0b100 << 13) | (uint16(rs1&31) << 7) | 0b10 }
func cMV(rd, rs2 int) uint16 {
	return (0b100 << 13) | (uint16(rd&31) << 7) | (uint16(rs2&31) << 2) | 0b10
}
func cJALR(rs1 int) uint16 {
	return (0b100 << 13) | (1 << 12) | (uint16(rs1&31) << 7) | 0b10
}
func cADD(rd, rs2 int) uint16 {
	return (0b100 << 13) | (1 << 12) | (uint16(rd&31) << 7) | (uint16(rs2&31) << 2) | 0b10
}
func cSWSP(rs2, uimm int) uint16 {
	return (0b110 << 13) | (b(uimm, 5, 2) << 9) | (b(uimm, 7, 6) << 7) | (uint16(rs2&31) << 2) | 0b10
}
func cSDSP(rs2, uimm int) uint16 {
	return (0b111 << 13) | (b(uimm, 5, 3) << 10) | (b(uimm, 8, 6) << 7) | (uint16(rs2&31) << 2) | 0b10
}
