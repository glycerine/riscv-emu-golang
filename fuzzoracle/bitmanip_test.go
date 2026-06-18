package fuzzoracle

// bitmanip_test.go — RED unit tests for Zicsr, Zba, Zbb, Zbs extensions.
// All use runOne() comparing our CPU against libriscv.
//
// Encoding conventions (all R-type unless noted):
//   opcode 0x33 = OP (64-bit)
//   opcode 0x3B = OP-32 (32-bit, sign-extended)
//   opcode 0x13 = OP-IMM
//   opcode 0x1B = OP-IMM-32
//   opcode 0x73 = SYSTEM (CSR)

import (
	"testing"

	riscv "riscv"
)

// ── Encoding helpers ──────────────────────────────────────────────────────

func brt(funct7, funct3, rd, rs1, rs2 int, opcode ...int) uint32 {
	op := 0x33
	if len(opcode) > 0 {
		op = opcode[0]
	}
	return uint32(funct7<<25 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | op)
}

func bimm(funct7hi, funct3, rd, rs1, shamt int, opcode ...int) uint32 {
	op := 0x13
	if len(opcode) > 0 {
		op = opcode[0]
	}
	return uint32(funct7hi<<25 | shamt<<20 | rs1<<15 | funct3<<12 | rd<<7 | op)
}

func csr(csr12, rs1, funct3, rd int) uint32 {
	return uint32(csr12<<20 | rs1<<15 | funct3<<12 | rd<<7 | 0x73)
}

func csri(csr12, uimm5, funct3, rd int) uint32 {
	return uint32(csr12<<20 | uimm5<<15 | funct3<<12 | rd<<7 | 0x73)
}

// ── Zicsr ─────────────────────────────────────────────────────────────────
// CSR instructions: CSRRW, CSRRS, CSRRC, CSRRWI, CSRRSI, CSRRCI

func TestCSRRW_fcsr(t *testing.T) {
	// CSRRW x1, fcsr, x2 — write x2 to fcsr, rd=old fcsr value
	runOne(t, csr(0x003, 2, 1, 1), regs(2, 0x5), nil)
}
func TestCSRRS_fcsr(t *testing.T) {
	// CSRRS x1, fcsr, x2 — set bits in fcsr, rd=old value
	runOne(t, csr(0x003, 2, 2, 1), regs(2, 0x1), nil)
}
func TestCSRRC_fcsr(t *testing.T) {
	// CSRRC x1, fcsr, x2 — clear bits in fcsr, rd=old value
	runOne(t, csr(0x003, 2, 3, 1), regs(2, 0x1F), nil)
}
func TestCSRRWI_fcsr(t *testing.T) {
	// CSRRWI x1, fcsr, 3 — write imm=3 to fcsr
	runOne(t, csri(0x003, 3, 5, 1), regs(), nil)
}
func TestCSRRSI_fcsr(t *testing.T) {
	// CSRRSI x1, fcsr, 1
	runOne(t, csri(0x003, 1, 6, 1), regs(), nil)
}
func TestCSRRCI_fcsr(t *testing.T) {
	// CSRRCI x1, fcsr, 0x1F
	runOne(t, csri(0x003, 0x1F, 7, 1), regs(), nil)
}
func TestCSRRW_fflags(t *testing.T) {
	// CSRRW x1, fflags, x2
	runOne(t, csr(0x001, 2, 1, 1), regs(2, 0x1F), nil)
}
func TestCSRRW_frm(t *testing.T) {
	// CSRRW x1, frm, x2
	runOne(t, csr(0x002, 2, 1, 1), regs(2, 0x4), nil)
}
func TestCSRRS_rd0(t *testing.T) {
	// CSRRS x0, fcsr, x2 — write-only (no read side effect when rd=0)
	runOne(t, csr(0x003, 2, 2, 0), regs(2, 0x7), nil)
}
func TestCSRRW_rs1_0(t *testing.T) {
	// CSRRS x1, fcsr, x0 — read-only (no write when rs1=x0)
	runOne(t, csr(0x003, 0, 2, 1), regs(), nil)
}

// ── Zba — address generation ──────────────────────────────────────────────

func TestSH1ADD(t *testing.T) {
	// SH1ADD x1, x2, x3: x1 = x3 + (x2 << 1)
	runOne(t, brt(0x10, 2, 1, 2, 3), regs(2, 10, 3, 100), nil)
}
func TestSH2ADD(t *testing.T) {
	// SH2ADD x1, x2, x3: x1 = x3 + (x2 << 2)
	runOne(t, brt(0x10, 4, 1, 2, 3), regs(2, 10, 3, 100), nil)
}
func TestSH3ADD(t *testing.T) {
	// SH3ADD x1, x2, x3: x1 = x3 + (x2 << 3)
	runOne(t, brt(0x10, 6, 1, 2, 3), regs(2, 10, 3, 100), nil)
}
func TestADD_UW(t *testing.T) {
	// ADD.UW x1, x2, x3: x1 = x3 + uint64(uint32(x2))  zero-extends low 32 bits
	runOne(t, brt(0x04, 0, 1, 2, 3, 0x3B), regs(2, 0xDEADBEEFCAFEBABE, 3, 100), nil)
}
func TestSH1ADD_UW(t *testing.T) {
	runOne(t, brt(0x10, 2, 1, 2, 3, 0x3B), regs(2, 0xDEADBEEF0000000A, 3, 100), nil)
}
func TestSH2ADD_UW(t *testing.T) {
	runOne(t, brt(0x10, 4, 1, 2, 3, 0x3B), regs(2, 0xDEADBEEF0000000A, 3, 100), nil)
}
func TestSH3ADD_UW(t *testing.T) {
	runOne(t, brt(0x10, 6, 1, 2, 3, 0x3B), regs(2, 0xDEADBEEF0000000A, 3, 100), nil)
}
func TestSLLI_UW(t *testing.T) {
	// SLLI.UW x1, x2, 4: x1 = uint64(uint32(x2)) << 4
	runOne(t, bimm(0x04, 1, 1, 2, 4, 0x1B), regs(2, 0xDEADBEEFCAFEBABE), nil)
}

// ── Zbb — basic bit manipulation ─────────────────────────────────────────

func TestANDN(t *testing.T) {
	// ANDN x1, x2, x3: x1 = x2 & ~x3
	runOne(t, brt(0x20, 7, 1, 2, 3), regs(2, 0xFF, 3, 0x0F), nil)
}
func TestORN(t *testing.T) {
	// ORN x1, x2, x3: x1 = x2 | ~x3
	runOne(t, brt(0x20, 6, 1, 2, 3), regs(2, 0xF0, 3, 0xF0), nil)
}
func TestXNOR(t *testing.T) {
	// XNOR x1, x2, x3: x1 = ~(x2 ^ x3)
	runOne(t, brt(0x20, 4, 1, 2, 3), regs(2, 0xAA, 3, 0xAA), nil)
}
func TestMAX(t *testing.T) {
	// MAX x1, x2, x3: signed max
	runOne(t, brt(0x05, 6, 1, 2, 3), regs(2, ^uint64(0), 3, 1), nil) // max(-1,1)=1
}
func TestMAXU(t *testing.T) {
	runOne(t, brt(0x05, 7, 1, 2, 3), regs(2, ^uint64(0), 3, 1), nil) // maxu(2^64-1,1)=2^64-1
}
func TestMIN(t *testing.T) {
	runOne(t, brt(0x05, 4, 1, 2, 3), regs(2, ^uint64(0), 3, 1), nil) // min(-1,1)=-1
}
func TestMINU(t *testing.T) {
	runOne(t, brt(0x05, 5, 1, 2, 3), regs(2, ^uint64(0), 3, 1), nil) // minu(2^64-1,1)=1
}
func TestROL(t *testing.T) {
	// ROL x1, x2, x3: rotate left by x3[5:0]
	runOne(t, brt(0x30, 1, 1, 2, 3), regs(2, 1, 3, 4), nil) // 1<<4=16
}
func TestROR(t *testing.T) {
	// ROR x1, x2, x3: rotate right
	runOne(t, brt(0x30, 5, 1, 2, 3), regs(2, 0x10, 3, 4), nil) // 0x10>>4=1
}
func TestRORI(t *testing.T) {
	// RORI x1, x2, 4
	runOne(t, bimm(0x30, 5, 1, 2, 4), regs(2, 0xF000000000000000), nil)
}
func TestROLW(t *testing.T) {
	runOne(t, brt(0x30, 1, 1, 2, 3, 0x3B), regs(2, 1, 3, 4), nil)
}
func TestSEXT_B(t *testing.T) {
	// SEXT.B x1, x2: sign-extend byte
	runOne(t, bimm(0x30, 1, 1, 2, 4), regs(2, 0xFF), nil) // -1
}
func TestSEXT_H(t *testing.T) {
	// SEXT.H x1, x2: sign-extend halfword
	runOne(t, bimm(0x30, 1, 1, 2, 5), regs(2, 0x8000), nil) // -32768
}
func TestZEXT_H(t *testing.T) {
	// ZEXT.H x1, x2: zero-extend halfword
	runOne(t, brt(0x04, 4, 1, 2, 0, 0x3B), regs(2, 0xDEADBEEFFFFF8000), nil) // 0x8000
}

// ── Zbs — single-bit instructions ─────────────────────────────────────────

func TestBSET(t *testing.T) {
	// BSET x1, x2, x3: set bit x3[5:0] in x2
	runOne(t, brt(0x14, 1, 1, 2, 3), regs(2, 0, 3, 5), nil) // set bit 5 -> 0x20
}
func TestBCLR(t *testing.T) {
	// BCLR x1, x2, x3: clear bit
	runOne(t, brt(0x24, 1, 1, 2, 3), regs(2, 0xFF, 3, 3), nil) // clear bit 3 -> 0xF7
}
func TestBINV(t *testing.T) {
	// BINV x1, x2, x3: invert bit
	runOne(t, brt(0x34, 1, 1, 2, 3), regs(2, 0xFF, 3, 0), nil) // invert bit 0 -> 0xFE
}
func TestBEXT(t *testing.T) {
	// BEXT x1, x2, x3: extract bit -> 0 or 1
	runOne(t, brt(0x24, 5, 1, 2, 3), regs(2, 0xF0, 3, 7), nil) // bit 7 of 0xF0 -> 1
}
func TestBSETI(t *testing.T) {
	runOne(t, bimm(0x14, 1, 1, 2, 5), regs(2, 0), nil) // set bit 5
}
func TestBCLRI(t *testing.T) {
	runOne(t, bimm(0x24, 1, 1, 2, 3), regs(2, 0xFF), nil) // clear bit 3
}
func TestBINVI(t *testing.T) {
	runOne(t, bimm(0x34, 1, 1, 2, 0), regs(2, 0xFF), nil) // invert bit 0
}
func TestBEXTI(t *testing.T) {
	runOne(t, bimm(0x24, 5, 1, 2, 7), regs(2, 0xF0), nil) // bit 7 -> 1
}

// ── Zbc: Carryless Multiply ──────────────────────────────────────────────

func TestCLMUL(t *testing.T) {
	runOne(t, brt(0x05, 1, 1, 2, 3), regs(2, 2, 3, 25), nil)
}
func TestCLMUL_Large(t *testing.T) {
	runOne(t, brt(0x05, 1, 1, 2, 3), regs(2, 0xDEADBEEFCAFEBABE, 3, 0x123456789ABCDEF0), nil)
}
func TestCLMULR(t *testing.T) {
	runOne(t, brt(0x05, 2, 1, 2, 3), regs(2, 0xDEADBEEFCAFEBABE, 3, 0x123456789ABCDEF0), nil)
}
func TestCLMULH(t *testing.T) {
	runOne(t, brt(0x05, 3, 1, 2, 3), regs(2, 0xDEADBEEFCAFEBABE, 3, 0x123456789ABCDEF0), nil)
}

// ── Zicond: Conditional Zero ─────────────────────────────────────────────

func TestCZERO_EQZ_Zero(t *testing.T) {
	runOne(t, brt(0x07, 5, 1, 2, 3), regs(2, 42, 3, 0), nil)
}
func TestCZERO_EQZ_NonZero(t *testing.T) {
	runOne(t, brt(0x07, 5, 1, 2, 3), regs(2, 42, 3, 1), nil)
}
func TestCZERO_NEZ_Zero(t *testing.T) {
	runOne(t, brt(0x07, 7, 1, 2, 3), regs(2, 42, 3, 0), nil)
}
func TestCZERO_NEZ_NonZero(t *testing.T) {
	runOne(t, brt(0x07, 7, 1, 2, 3), regs(2, 42, 3, 1), nil)
}

// ── SRET: illegal instruction (matches libriscv) ────────────────────────

func TestSRET_Illegal(t *testing.T) {
	// SRET = 0x10200073: libriscv triggers ILLEGAL_OPERATION, we return ErrIllegalInstruction.
	// We can't use runOne (which calls Step then compares regs) because Step errors out.
	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	elf := riscv.BuildELF(oracleCodeVA, []uint32{0x10200073, 0x00000073})
	riscv.LoadELFBytes(mem, elf)
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	if err := cpu.Step(); err != riscv.ErrIllegalInstruction {
		t.Errorf("SRET: got %v, want ErrIllegalInstruction", err)
	}
}
