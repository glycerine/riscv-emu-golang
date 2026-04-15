package fuzzoracle

// amo_test.go — RED unit tests for RV64A atomic instructions.
// All use runOne() comparing our CPU against libriscv.
//
// AMO encoding: opcode=0x2F, funct3=010(W)/011(D)
// | funct5[31:27] | aq[26] | rl[25] | rs2[24:20] | rs1[19:15] | funct3[14:12] | rd[11:7] | opcode[6:0] |
//
// Register convention: x2=address (oracleDataVA), x3=operand, x1=result

import "testing"

// ── AMO encoding helper ───────────────────────────────────────────────────

func amo(funct5, funct3, rd, rs1, rs2 uint8) uint32 {
	return uint32(funct5)<<27 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(funct3)<<12 | uint32(rd)<<7 | 0x2F
}

const (
	amoW = uint8(0b010) // .W suffix — 32-bit word
	amoD = uint8(0b011) // .D suffix — 64-bit doubleword
)

// ── LR / SC ───────────────────────────────────────────────────────────────
// LR.W/D: rd = mem[rs1]; register reservation
// SC.W/D: if reservation held: mem[rs1]=rs2, rd=0; else rd=1

func TestLR_W(t *testing.T) {
	// LR.W x1, (x2)  — load-reserved word, sign-extended
	runOne(t, amo(0b00010, amoW, 1, 2, 0),
		regs(2, oracleDataVA), []byte{0xEF, 0xBE, 0xAD, 0xDE})
}
func TestLR_D(t *testing.T) {
	// LR.D x1, (x2)  — load-reserved doubleword
	runOne(t, amo(0b00010, amoD, 1, 2, 0),
		regs(2, oracleDataVA), []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE})
}
func TestSC_W_Succeeds(t *testing.T) {
	// LR.W then SC.W on the same address — SC must succeed (rd=0)
	// runTwo executes both instructions sequentially
	runTwo(t,
		amo(0b00010, amoW, 0, 2, 0), // LR.W x0, (x2)  — establish reservation
		amo(0b00011, amoW, 1, 2, 3), // SC.W x1, x3, (x2) — should succeed: rd=0
		regs(2, oracleDataVA, 3, 0xCAFEBABE),
		make([]byte, 8),
	)
}
func TestSC_D_Succeeds(t *testing.T) {
	runTwo(t,
		amo(0b00010, amoD, 0, 2, 0), // LR.D x0, (x2)
		amo(0b00011, amoD, 1, 2, 3), // SC.D x1, x3, (x2)
		regs(2, oracleDataVA, 3, 0xDEADBEEFCAFEBABE),
		make([]byte, 8),
	)
}

// ── AMOSWAP ───────────────────────────────────────────────────────────────
// rd = mem[rs1]; mem[rs1] = rs2

func TestAMOSWAP_W(t *testing.T) {
	runOne(t, amo(0b00001, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xCAFEBABE), []byte{0xDE, 0xAD, 0xBE, 0xEF})
}
func TestAMOSWAP_D(t *testing.T) {
	runOne(t, amo(0b00001, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0x1234567890ABCDEF),
		[]byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE})
}

// ── AMOADD ────────────────────────────────────────────────────────────────
// rd = mem[rs1]; mem[rs1] = rd + rs2

func TestAMOADD_W(t *testing.T) {
	runOne(t, amo(0b00000, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 10), []byte{5, 0, 0, 0})
}
func TestAMOADD_D(t *testing.T) {
	runOne(t, amo(0b00000, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 100), []byte{200, 0, 0, 0, 0, 0, 0, 0})
}
func TestAMOADD_W_Overflow(t *testing.T) {
	runOne(t, amo(0b00000, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 1), []byte{0xFF, 0xFF, 0xFF, 0x7F}) // 0x7FFFFFFF+1
}

// ── AMOXOR ────────────────────────────────────────────────────────────────

func TestAMOXOR_W(t *testing.T) {
	runOne(t, amo(0b00100, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFF), []byte{0xAA, 0, 0, 0})
}
func TestAMOXOR_D(t *testing.T) {
	runOne(t, amo(0b00100, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFFFFFFFFFFFFFFFF),
		[]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22})
}

// ── AMOAND ────────────────────────────────────────────────────────────────

func TestAMOAND_W(t *testing.T) {
	runOne(t, amo(0b01100, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0x0F), []byte{0xFF, 0, 0, 0})
}
func TestAMOAND_D(t *testing.T) {
	runOne(t, amo(0b01100, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFF00FF00FF00FF00),
		[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
}

// ── AMOOR ─────────────────────────────────────────────────────────────────

func TestAMOOR_W(t *testing.T) {
	runOne(t, amo(0b01000, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xF0), []byte{0x0F, 0, 0, 0})
}
func TestAMOOR_D(t *testing.T) {
	runOne(t, amo(0b01000, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFF00FF00FF00FF00),
		[]byte{0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF})
}

// ── AMOMIN / AMOMAX (signed) ──────────────────────────────────────────────

func TestAMOMIN_W_NegWins(t *testing.T) {
	// mem=-1 (0xFFFFFFFF as signed int32), rs2=5 -> min=-1
	runOne(t, amo(0b10000, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 5), []byte{0xFF, 0xFF, 0xFF, 0xFF})
}
func TestAMOMIN_W_PosWins(t *testing.T) {
	// mem=5, rs2=-1 (sign-extended) -> min=-1 written to mem
	runOne(t, amo(0b10000, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFFFFFFFFFFFFFFFF), []byte{5, 0, 0, 0})
}
func TestAMOMIN_D(t *testing.T) {
	runOne(t, amo(0b10000, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFFFFFFFFFFFFFFFF), // -1
		[]byte{5, 0, 0, 0, 0, 0, 0, 0}) // mem=5, min(-1,5)=-1
}
func TestAMOMAX_W(t *testing.T) {
	runOne(t, amo(0b10100, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 10), []byte{5, 0, 0, 0}) // max(5,10)=10
}
func TestAMOMAX_D(t *testing.T) {
	runOne(t, amo(0b10100, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFFFFFFFFFFFFFFFF), // -1 signed
		[]byte{5, 0, 0, 0, 0, 0, 0, 0}) // max(5,-1)=5: mem unchanged
}

// ── AMOMINU / AMOMAXU (unsigned) ─────────────────────────────────────────

func TestAMOMINU_W(t *testing.T) {
	// mem=0xFFFFFFFF unsigned=4294967295, rs2=5 -> minu=5
	runOne(t, amo(0b11000, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 5), []byte{0xFF, 0xFF, 0xFF, 0xFF})
}
func TestAMOMINU_D(t *testing.T) {
	runOne(t, amo(0b11000, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 5),
		[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) // minu(max,5)=5
}
func TestAMOMAXU_W(t *testing.T) {
	// mem=5, rs2=0xFFFFFFFF -> maxu=0xFFFFFFFF
	runOne(t, amo(0b11100, amoW, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFFFFFFFF), []byte{5, 0, 0, 0})
}
func TestAMOMAXU_D(t *testing.T) {
	runOne(t, amo(0b11100, amoD, 1, 2, 3),
		regs(2, oracleDataVA, 3, 0xFFFFFFFFFFFFFFFF),
		[]byte{5, 0, 0, 0, 0, 0, 0, 0}) // maxu(5, max)=max
}
