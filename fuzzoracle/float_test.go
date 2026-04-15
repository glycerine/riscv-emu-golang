package fuzzoracle

// float_test.go — RED unit tests for RV64F and RV64D (F+D extension).
// All tests use runOneF() comparing our CPU against libriscv.
//
// Instruction encoding:
//   FLW/FLD:  opcode=0x07, funct3=010(F)/011(D)  I-type
//   FSW/FSD:  opcode=0x27, funct3=010(F)/011(D)  S-type
//   FMADD.S/D: opcode=0x43/0x47  R4-type
//   FMSUB.S/D: opcode=0x47/0x4B  R4-type
//   FNMSUB:    opcode=0x4B/0x4F  R4-type
//   FNMADD:    opcode=0x4F       R4-type
//   FPFUNC:   opcode=0x53, funct5 selects op
//
// Register naming: f1,f2,f3 for float operands; x1,x2 for integer

import (
	"math"
	"testing"
	"unsafe"
)

// ── Encoding helpers ──────────────────────────────────────────────────────

const rmRNE = uint32(0b000) // round to nearest, ties to even
const rmRTZ = uint32(0b001) // round toward zero
const rmDYN = uint32(0b111) // dynamic (use fcsr.frm)

// flw encodes FLW fd, imm(rs1)
func flw(rd, rs1 uint8, imm int) uint32 {
	return uint32(imm&0xFFF)<<20 | uint32(rs1)<<15 | 0b010<<12 | uint32(rd)<<7 | 0x07
}

// fld encodes FLD fd, imm(rs1)
func fld(rd, rs1 uint8, imm int) uint32 {
	return uint32(imm&0xFFF)<<20 | uint32(rs1)<<15 | 0b011<<12 | uint32(rd)<<7 | 0x07
}

// fsw encodes FSW fs2, imm(rs1)
func fsw(rs1, rs2 uint8, imm int) uint32 {
	return uint32((imm>>5)&0x7F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		0b010<<12 | uint32(imm&0x1F)<<7 | 0x27
}

// fsd encodes FSD fs2, imm(rs1)
func fsd(rs1, rs2 uint8, imm int) uint32 {
	return uint32((imm>>5)&0x7F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		0b011<<12 | uint32(imm&0x1F)<<7 | 0x27
}

// fpf encodes FPFUNC (opcode=0x53): funct5, rm, rd, rs1, rs2
func fpf(funct5, rm, rd, rs1, rs2 uint32) uint32 {
	return funct5<<27 | rs2<<20 | rs1<<15 | rm<<12 | rd<<7 | 0x53
}

// r4 encodes R4-type (FMADD etc): opcode, fmt(00=S,01=D), rm, rd, rs1, rs2, rs3
func r4(opcode, fmt, rm, rd, rs1, rs2, rs3 uint32) uint32 {
	return rs3<<27 | fmt<<25 | rs2<<20 | rs1<<15 | rm<<12 | rd<<7 | opcode
}

// bit-pattern helpers
func bits32(f float32) uint32 { return math.Float32bits(f) }
func bits64(f float64) uint64 { return math.Float64bits(f) }

// nb32 NaN-boxes a float32 bit pattern for an f-register
func nb32(b uint32) uint64 { return 0xFFFFFFFF00000000 | uint64(b) }

// fxregs builds initX with x2=oracleDataVA and optional extra pairs
func fxregs(extra ...uint64) [32]uint64 {
	var x [32]uint64
	x[2] = oracleDataVA
	for i := 0; i+1 < len(extra); i += 2 {
		x[extra[i]] = extra[i+1]
	}
	return x
}

// ffregs builds initF with f-register pairs: index, raw-uint64
func ffregs(pairs ...uint64) [32]uint64 {
	var f [32]uint64
	// default: NaN-boxed 0.0
	for i := range f { f[i] = nb32(0) }
	for i := 0; i+1 < len(pairs); i += 2 {
		f[pairs[i]] = pairs[i+1]
	}
	return f
}

// ── FLW / FLD ─────────────────────────────────────────────────────────────

func TestFLW_Basic(t *testing.T) {
	// FLW f1, 0(x2)  — load 3.14 from memory into f1
	mem := make([]byte, 8)
	b := bits32(3.14)
	mem[0],mem[1],mem[2],mem[3] = byte(b),byte(b>>8),byte(b>>16),byte(b>>24)
	runOneF(t, flw(1,2,0), fxregs(), ffregs(), mem)
}

func TestFLW_NegVal(t *testing.T) {
	mem := make([]byte, 8)
	b := bits32(-1.5)
	mem[0],mem[1],mem[2],mem[3] = byte(b),byte(b>>8),byte(b>>16),byte(b>>24)
	runOneF(t, flw(1,2,0), fxregs(), ffregs(), mem)
}

func TestFLD_Basic(t *testing.T) {
	// FLD f1, 0(x2)
	mem := make([]byte, 8)
	b := bits64(3.141592653589793)
	for i := 0; i < 8; i++ { mem[i] = byte(b >> (i*8)) }
	runOneF(t, fld(1,2,0), fxregs(), ffregs(), mem)
}

// ── FSW / FSD ─────────────────────────────────────────────────────────────

func TestFSW_Basic(t *testing.T) {
	// FSW f1, 0(x2)
	runOneF(t, fsw(2,1,0), fxregs(), ffregs(1, nb32(bits32(2.71))), make([]byte,8))
}

func TestFSD_Basic(t *testing.T) {
	// FSD f1, 0(x2)
	runOneF(t, fsd(2,1,0), fxregs(), ffregs(1, bits64(2.718281828)), make([]byte,8))
}

// ── FADD / FSUB / FMUL / FDIV ────────────────────────────────────────────

func TestFADD_S(t *testing.T) {
	// FADD.S f1, f2, f3: funct5=0x00, fmt suffix S -> rs2 upper bits 00
	runOneF(t, fpf(0x00,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.5)), 3,nb32(bits32(2.5))),
		nil)
}
func TestFADD_D(t *testing.T) {
	// FADD.D f1, f2, f3: funct5=0x00 | fmt=01 (D) encoded in rs2 upper? No —
	// for FPFUNC the format is in bit 25:26 via the fmt field.
	// Actually: FADD.S = funct7=0x00, FADD.D = funct7=0x02 (funct5=0x00, bit25=1 for D... )
	// Wait — let me re-examine:
	// funct7 = funct5[4:0] | fmt[1:0]  where fmt: 00=S, 01=D, 10=H, 11=Q
	// So FADD.D: funct7 = 0b0000001 = 0x01, not 0x02
	// fpf encodes: funct5<<27 | rs2<<20... but funct5 is bits[31:27] = 5 bits
	// The actual layout: [31:27]=funct5, [26:25]=fmt, [24:20]=rs2
	// So for FADD.D: bits[31:25] = 0b0000001, i.e. funct5=0b00000, fmt=0b01
	// fpf helper needs to encode fmt separately:
	runOneF(t, fpfD(0x00,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,bits64(1.5), 3,bits64(2.5)),
		nil)
}

func TestFSUB_S(t *testing.T) {
	runOneF(t, fpf(0x01,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(5.0)), 3,nb32(bits32(3.0))),
		nil)
}
func TestFSUB_D(t *testing.T) {
	runOneF(t, fpfD(0x01,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,bits64(5.0), 3,bits64(3.0)),
		nil)
}

func TestFMUL_S(t *testing.T) {
	runOneF(t, fpf(0x02,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(2.0)), 3,nb32(bits32(3.0))),
		nil)
}
func TestFMUL_D(t *testing.T) {
	runOneF(t, fpfD(0x02,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,bits64(2.0), 3,bits64(3.0)),
		nil)
}

func TestFDIV_S(t *testing.T) {
	runOneF(t, fpf(0x03,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(7.0)), 3,nb32(bits32(2.0))),
		nil)
}
func TestFDIV_D(t *testing.T) {
	runOneF(t, fpfD(0x03,rmRNE,1,2,3),
		fxregs(),
		ffregs(2,bits64(7.0), 3,bits64(2.0)),
		nil)
}

// ── FSQRT ─────────────────────────────────────────────────────────────────

func TestFSQRT_S(t *testing.T) {
	// FSQRT.S f1, f2   funct5=0x0B, rs2=0
	runOneF(t, fpf(0x0B,rmRNE,1,2,0),
		fxregs(),
		ffregs(2,nb32(bits32(4.0))),
		nil)
}
func TestFSQRT_D(t *testing.T) {
	runOneF(t, fpfD(0x0B,rmRNE,1,2,0),
		fxregs(),
		ffregs(2,bits64(4.0)),
		nil)
}

// ── FSGNJ / FSGNJN / FSGNJX ──────────────────────────────────────────────

func TestFSGNJ_S(t *testing.T) {
	// FSGNJ.S: rd = |rs1| with sign of rs2
	runOneF(t, fpf(0x04,0b000,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(-3.0)), 3,nb32(bits32(1.0))),
		nil)
}
func TestFSGNJN_S(t *testing.T) {
	runOneF(t, fpf(0x04,0b001,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(3.0)), 3,nb32(bits32(1.0))),
		nil)
}
func TestFSGNJX_S(t *testing.T) {
	runOneF(t, fpf(0x04,0b010,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(3.0)), 3,nb32(bits32(-1.0))),
		nil)
}
func TestFSGNJ_D(t *testing.T) {
	runOneF(t, fpfD(0x04,0b000,1,2,3),
		fxregs(),
		ffregs(2,bits64(-3.0), 3,bits64(1.0)),
		nil)
}

// ── FMIN / FMAX ───────────────────────────────────────────────────────────

func TestFMIN_S(t *testing.T) {
	runOneF(t, fpf(0x05,0b000,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.0)), 3,nb32(bits32(2.0))),
		nil)
}
func TestFMAX_S(t *testing.T) {
	runOneF(t, fpf(0x05,0b001,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.0)), 3,nb32(bits32(2.0))),
		nil)
}
func TestFMIN_D(t *testing.T) {
	runOneF(t, fpfD(0x05,0b000,1,2,3),
		fxregs(),
		ffregs(2,bits64(1.0), 3,bits64(2.0)),
		nil)
}
func TestFMAX_D(t *testing.T) {
	runOneF(t, fpfD(0x05,0b001,1,2,3),
		fxregs(),
		ffregs(2,bits64(1.0), 3,bits64(2.0)),
		nil)
}

// ── FCVT (float ↔ integer) ────────────────────────────────────────────────
// rs2 field selects destination integer type: 0=W 1=WU 2=L 3=LU

func TestFCVT_W_S(t *testing.T) {
	// FCVT.W.S x1, f2  funct5=0x18 rs2=0 (signed 32-bit)
	runOneF(t, fpf(0x18,rmRTZ,1,2,0),
		fxregs(), ffregs(2,nb32(bits32(3.7))), nil)
}
func TestFCVT_WU_S(t *testing.T) {
	runOneF(t, fpf(0x18,rmRTZ,1,2,1),
		fxregs(), ffregs(2,nb32(bits32(3.7))), nil)
}
func TestFCVT_L_S(t *testing.T) {
	runOneF(t, fpf(0x18,rmRTZ,1,2,2),
		fxregs(), ffregs(2,nb32(bits32(3.7))), nil)
}
func TestFCVT_LU_S(t *testing.T) {
	runOneF(t, fpf(0x18,rmRTZ,1,2,3),
		fxregs(), ffregs(2,nb32(bits32(3.7))), nil)
}
func TestFCVT_S_W(t *testing.T) {
	// FCVT.S.W f1, x2  funct5=0x1A rs2=0
	runOneF(t, fpf(0x1A,rmRNE,1,2,0),
		fxregs(2,42), ffregs(), nil)
}
func TestFCVT_W_D(t *testing.T) {
	runOneF(t, fpfD(0x18,rmRTZ,1,2,0),
		fxregs(), ffregs(2,bits64(3.7)), nil)
}
func TestFCVT_D_W(t *testing.T) {
	runOneF(t, fpfD(0x1A,rmRNE,1,2,0),
		fxregs(2,42), ffregs(), nil)
}
func TestFCVT_S_D(t *testing.T) {
	// FCVT.S.D f1, f2  funct5=0x08 rs2=1 (D->S)
	runOneF(t, fpf(0x08,rmRNE,1,2,1),
		fxregs(), ffregs(2,bits64(3.141592653589793)), nil)
}
func TestFCVT_D_S(t *testing.T) {
	// FCVT.D.S f1, f2  funct5=0x08 rs2=0 (S->D) but funct7 fmt=D
	// Actually: FCVT.D.S uses opcode 0x53, funct7=0x21 (funct5=0x08,fmt=D=01)
	runOneF(t, fpfD(0x08,rmRNE,1,2,0),
		fxregs(), ffregs(2,nb32(bits32(3.14))), nil)
}

// ── FMV (bit moves) ────────────────────────────────────────────────────────

func TestFMV_X_W(t *testing.T) {
	// FMV.X.W x1, f2  — move bits of float32 to integer x1 (sign-extended)
	// funct5=0x1C, funct3=0b000, rs2=0
	runOneF(t, fpf(0x1C,0b000,1,2,0),
		fxregs(), ffregs(2,nb32(bits32(1.0))), nil)
}
func TestFMV_W_X(t *testing.T) {
	// FMV.W.X f1, x2  — move integer x2 bits to f1 (NaN-boxed)
	// funct5=0x1E, funct3=0b000, rs2=0
	runOneF(t, fpf(0x1E,0b000,1,2,0),
		fxregs(2,uint64(bits32(2.71))), ffregs(), nil)
}
func TestFMV_X_D(t *testing.T) {
	// FMV.X.D x1, f2  funct5=0x1C fmt=D
	runOneF(t, fpfD(0x1C,0b000,1,2,0),
		fxregs(), ffregs(2,bits64(1.0)), nil)
}
func TestFMV_D_X(t *testing.T) {
	// FMV.D.X f1, x2  funct5=0x1E fmt=D
	runOneF(t, fpfD(0x1E,0b000,1,2,0),
		fxregs(2,bits64(2.718281828)), ffregs(), nil)
}

// ── FEQ / FLT / FLE ───────────────────────────────────────────────────────

func TestFEQ_S_True(t *testing.T) {
	// FEQ.S x1, f2, f3  funct5=0x14, funct3=0b010
	runOneF(t, fpf(0x14,0b010,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.0)), 3,nb32(bits32(1.0))),
		nil)
}
func TestFEQ_S_False(t *testing.T) {
	runOneF(t, fpf(0x14,0b010,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.0)), 3,nb32(bits32(2.0))),
		nil)
}
func TestFLT_S(t *testing.T) {
	runOneF(t, fpf(0x14,0b001,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.0)), 3,nb32(bits32(2.0))),
		nil)
}
func TestFLE_S(t *testing.T) {
	runOneF(t, fpf(0x14,0b000,1,2,3),
		fxregs(),
		ffregs(2,nb32(bits32(1.0)), 3,nb32(bits32(1.0))),
		nil)
}
func TestFEQ_D(t *testing.T) {
	runOneF(t, fpfD(0x14,0b010,1,2,3),
		fxregs(),
		ffregs(2,bits64(1.0), 3,bits64(1.0)),
		nil)
}
func TestFLT_D(t *testing.T) {
	runOneF(t, fpfD(0x14,0b001,1,2,3),
		fxregs(),
		ffregs(2,bits64(1.0), 3,bits64(2.0)),
		nil)
}
func TestFLE_D(t *testing.T) {
	runOneF(t, fpfD(0x14,0b000,1,2,3),
		fxregs(),
		ffregs(2,bits64(2.0), 3,bits64(2.0)),
		nil)
}

// ── FCLASS ────────────────────────────────────────────────────────────────

func TestFCLASS_S_PosNorm(t *testing.T) {
	runOneF(t, fpf(0x1C,0b001,1,2,0),
		fxregs(), ffregs(2,nb32(bits32(1.0))), nil)
}
func TestFCLASS_D_PosNorm(t *testing.T) {
	runOneF(t, fpfD(0x1C,0b001,1,2,0),
		fxregs(), ffregs(2,bits64(1.0)), nil)
}

// ── FMADD / FMSUB / FNMADD / FNMSUB ──────────────────────────────────────

func TestFMADD_S(t *testing.T) {
	// FMADD.S f1, f2, f3, f4: opcode=0x43, fmt=00
	// result = f2*f3 + f4
	runOneF(t, r4(0x43,0,rmRNE,1,2,3,4),
		fxregs(),
		ffregs(2,nb32(bits32(2.0)), 3,nb32(bits32(3.0)), 4,nb32(bits32(1.0))),
		nil)
}
func TestFMSUB_S(t *testing.T) {
	// FMSUB.S: opcode=0x47, result = f2*f3 - f4
	runOneF(t, r4(0x47,0,rmRNE,1,2,3,4),
		fxregs(),
		ffregs(2,nb32(bits32(2.0)), 3,nb32(bits32(3.0)), 4,nb32(bits32(1.0))),
		nil)
}
func TestFNMADD_S(t *testing.T) {
	// FNMADD.S: opcode=0x4F, result = -(f2*f3) - f4
	runOneF(t, r4(0x4F,0,rmRNE,1,2,3,4),
		fxregs(),
		ffregs(2,nb32(bits32(2.0)), 3,nb32(bits32(3.0)), 4,nb32(bits32(1.0))),
		nil)
}
func TestFNMSUB_S(t *testing.T) {
	// FNMSUB.S: opcode=0x4B, result = -(f2*f3) + f4
	runOneF(t, r4(0x4B,0,rmRNE,1,2,3,4),
		fxregs(),
		ffregs(2,nb32(bits32(2.0)), 3,nb32(bits32(3.0)), 4,nb32(bits32(1.0))),
		nil)
}
func TestFMADD_D(t *testing.T) {
	// FMADD.D: opcode=0x43, fmt=01
	runOneF(t, r4(0x43,1,rmRNE,1,2,3,4),
		fxregs(),
		ffregs(2,bits64(2.0), 3,bits64(3.0), 4,bits64(1.0)),
		nil)
}

// ── fpfD helper ───────────────────────────────────────────────────────────
// fpfD encodes a D-format FPFUNC instruction (fmt bits = 01).

func fpfD(funct5, rm, rd, rs1, rs2 uint32) uint32 {
	// For D extension: the fmt field occupies bits [26:25] = 0b01
	// funct7 = funct5[4:0] | fmt[1:0], packed as:
	// bits[31:27]=funct5, bit[26]=fmt[1]=0, bit[25]=fmt[0]=1
	return funct5<<27 | 0<<26 | 1<<25 | rs2<<20 | rs1<<15 | rm<<12 | rd<<7 | 0x53
}

// ── unused import guard ───────────────────────────────────────────────────
var _ = unsafe.Sizeof(0) // ensure unsafe imported for f2bits in oracle_test.go
