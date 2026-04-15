package fuzzoracle

// mul_test.go — RED tests for RV64M multiply/divide instructions.
// All tests use runOne() which compares our CPU against libriscv.
// These will fail until cpu.go implements opcode 0x33/0x3B with funct7=0x01.
//
// RV64M instructions (all R-type, funct7=0x01):
//
//   OP (0x33):   MUL MULH MULHSU MULHU DIV DIVU REM REMU
//   OP-32 (0x3B): MULW DIVW DIVUW REMW REMUW
//
// Division semantics (from spec):
//   DIV/REM:  signed;   DIV(x,-1) = -x (no overflow trap); DIV(x,0) = -1; REM(x,0) = x
//   DIVU/REMU: unsigned; DIVU(x,0) = 2^64-1;               REMU(x,0) = x
//   *W variants: operate on low 32 bits, sign-extend result to 64 bits

import "testing"

// ── MUL ──────────────────────────────────────────────────────────────────
// MUL: rd = (rs1 * rs2)[63:0]  (lower 64 bits, same for signed/unsigned)

func TestMUL_Basic(t *testing.T)        { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,7,3,6), nil) }
func TestMUL_Zero(t *testing.T)         { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,0,3,42), nil) }
func TestMUL_One(t *testing.T)          { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,1,3,42), nil) }
func TestMUL_NegPos(t *testing.T)       { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,^uint64(0),3,2), nil) } // -1*2=-2
func TestMUL_NegNeg(t *testing.T)       { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,^uint64(0),3,^uint64(0)), nil) } // -1*-1=1
func TestMUL_Overflow(t *testing.T)     { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,0x8000000000000000,3,2), nil) }
func TestMUL_Large(t *testing.T)        { runOne(t, renc(0x33,0,0x01,1,2,3), regs(2,0xDEADBEEF,3,0xCAFEBABE), nil) }

// ── MULH ─────────────────────────────────────────────────────────────────
// MULH: rd = (signed(rs1) * signed(rs2))[127:64]  (upper 64 bits, signed*signed)

func TestMULH_Basic(t *testing.T)       { runOne(t, renc(0x33,1,0x01,1,2,3), regs(2,3,3,4), nil) }
func TestMULH_MinMin(t *testing.T)      { runOne(t, renc(0x33,1,0x01,1,2,3), regs(2,0x8000000000000000,3,0x8000000000000000), nil) }
func TestMULH_Large(t *testing.T)       { runOne(t, renc(0x33,1,0x01,1,2,3), regs(2,0x7FFFFFFFFFFFFFFF,3,0x7FFFFFFFFFFFFFFF), nil) }

// ── MULHSU ───────────────────────────────────────────────────────────────
// MULHSU: rd = (signed(rs1) * unsigned(rs2))[127:64]

func TestMULHSU_Basic(t *testing.T)     { runOne(t, renc(0x33,2,0x01,1,2,3), regs(2,3,3,4), nil) }

// ── MULHU ────────────────────────────────────────────────────────────────
// MULHU: rd = (unsigned(rs1) * unsigned(rs2))[127:64]

func TestMULHU_Basic(t *testing.T)      { runOne(t, renc(0x33,3,0x01,1,2,3), regs(2,3,3,4), nil) }
func TestMULHU_Large(t *testing.T)      { runOne(t, renc(0x33,3,0x01,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,0xFFFFFFFFFFFFFFFF), nil) }
func TestMULHU_Max(t *testing.T)        { runOne(t, renc(0x33,3,0x01,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,2), nil) }

// ── DIV ──────────────────────────────────────────────────────────────────
// DIV: signed division rd = signed(rs1) / signed(rs2)
// Special cases: rs2=0 -> -1;  INT_MIN/-1 -> INT_MIN (no trap)

func TestDIV_Basic(t *testing.T)        { runOne(t, renc(0x33,4,0x01,1,2,3), regs(2,42,3,6), nil) }
func TestDIV_Negative(t *testing.T)     { runOne(t, renc(0x33,4,0x01,1,2,3), regs(2,^uint64(6)+1,3,2), nil) } // -6/2=-3
func TestDIV_NegNeg(t *testing.T)       { runOne(t, renc(0x33,4,0x01,1,2,3), regs(2,^uint64(6)+1,3,^uint64(2)+1), nil) } // -6/-2=3
func TestDIV_Truncate(t *testing.T)     { runOne(t, renc(0x33,4,0x01,1,2,3), regs(2,7,3,2), nil) } // 7/2=3 (truncate toward zero)
func TestDIV_NegTruncate(t *testing.T)  { runOne(t, renc(0x33,4,0x01,1,2,3), regs(2,^uint64(7)+1,3,2), nil) } // -7/2=-3

// ── DIVU ─────────────────────────────────────────────────────────────────
// DIVU: unsigned division rd = rs1 / rs2
// Special case: rs2=0 -> 2^64-1

func TestDIVU_Basic(t *testing.T)       { runOne(t, renc(0x33,5,0x01,1,2,3), regs(2,42,3,6), nil) }
func TestDIVU_Large(t *testing.T)       { runOne(t, renc(0x33,5,0x01,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,2), nil) }
func TestDIVU_NegAsUnsigned(t *testing.T) { runOne(t, renc(0x33,5,0x01,1,2,3), regs(2,^uint64(0),3,^uint64(0)), nil) } // (2^64-1)/(2^64-1)=1

// ── REM ──────────────────────────────────────────────────────────────────
// REM: signed remainder rd = signed(rs1) % signed(rs2)
// Special cases: rs2=0 -> rs1;  INT_MIN/-1 -> 0

func TestREM_Basic(t *testing.T)        { runOne(t, renc(0x33,6,0x01,1,2,3), regs(2,7,3,3), nil) } // 7%3=1
func TestREM_Negative(t *testing.T)     { runOne(t, renc(0x33,6,0x01,1,2,3), regs(2,^uint64(7)+1,3,3), nil) } // -7%3=-1
func TestREM_Overflow(t *testing.T)     { runOne(t, renc(0x33,6,0x01,1,2,3), regs(2,0x8000000000000000,3,^uint64(0)), nil) } // INT_MIN%-1 -> 0
func TestREM_Exact(t *testing.T)        { runOne(t, renc(0x33,6,0x01,1,2,3), regs(2,6,3,3), nil) } // 6%3=0

// ── REMU ─────────────────────────────────────────────────────────────────
// REMU: unsigned remainder rd = rs1 % rs2
// Special case: rs2=0 -> rs1

func TestREMU_Basic(t *testing.T)       { runOne(t, renc(0x33,7,0x01,1,2,3), regs(2,7,3,3), nil) }
func TestREMU_Large(t *testing.T)       { runOne(t, renc(0x33,7,0x01,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,3), nil) }

// ── MULW ─────────────────────────────────────────────────────────────────
// MULW: rd = sign_extend((rs1[31:0] * rs2[31:0])[31:0])

func TestMULW_Basic(t *testing.T)       { runOne(t, renc(0x3B,0,0x01,1,2,3), regs(2,7,3,6), nil) }
func TestMULW_SignExtend(t *testing.T)  { runOne(t, renc(0x3B,0,0x01,1,2,3), regs(2,0x80000000,3,2), nil) } // overflows into negative
func TestMULW_Neg(t *testing.T)         { runOne(t, renc(0x3B,0,0x01,1,2,3), regs(2,^uint64(1)+1,3,2), nil) } // -2*2=-4 as 32-bit
func TestMULW_IgnoresUpper(t *testing.T){ runOne(t, renc(0x3B,0,0x01,1,2,3), regs(2,0xDEADBEEF00000007,3,6), nil) } // upper bits ignored

// ── DIVW ─────────────────────────────────────────────────────────────────
// DIVW: rd = sign_extend(signed(rs1[31:0]) / signed(rs2[31:0]))

func TestDIVW_Basic(t *testing.T)       { runOne(t, renc(0x3B,4,0x01,1,2,3), regs(2,42,3,6), nil) }
func TestDIVW_Negative(t *testing.T)    { runOne(t, renc(0x3B,4,0x01,1,2,3), regs(2,^uint64(6)+1,3,2), nil) }

// ── DIVUW ────────────────────────────────────────────────────────────────
// DIVUW: rd = sign_extend(unsigned(rs1[31:0]) / unsigned(rs2[31:0]))

func TestDIVUW_Basic(t *testing.T)      { runOne(t, renc(0x3B,5,0x01,1,2,3), regs(2,42,3,6), nil) }

// ── REMW ─────────────────────────────────────────────────────────────────
// REMW: rd = sign_extend(signed(rs1[31:0]) % signed(rs2[31:0]))

func TestREMW_Basic(t *testing.T)       { runOne(t, renc(0x3B,6,0x01,1,2,3), regs(2,7,3,3), nil) }
func TestREMW_Overflow(t *testing.T)    { runOne(t, renc(0x3B,6,0x01,1,2,3), regs(2,0x80000000,3,^uint64(0)), nil) } // -> 0

// ── REMUW ────────────────────────────────────────────────────────────────
// REMUW: rd = sign_extend(unsigned(rs1[31:0]) % unsigned(rs2[31:0]))

func TestREMUW_Basic(t *testing.T)      { runOne(t, renc(0x3B,7,0x01,1,2,3), regs(2,7,3,3), nil) }
