package riscv

// float.go — IEEE-754 constants and helpers for RV64F+D (single + double
// precision floating-point). The f-register file is 32 × uint64 regardless
// of whether a value was written by an F or D instruction.
//
// NaN-boxing rule (RISC-V spec §11.2):
//   When an F instruction writes to an f-register on a machine that also
//   implements D, it must NaN-box: the upper 32 bits are set to all 1s.
//   When a D instruction (or FMV.W.X) reads an f-register as single-precision,
//   it checks bits[63:32] == 0xFFFFFFFF; if not, it substitutes the canonical
//   quiet NaN (f32CanonNaN / f64CanonNaN) instead of the stored value.

import "math"

// ── Register file widths ──────────────────────────────────────────────────

// nanBoxUpper is the value of bits[63:32] in a correctly NaN-boxed f-register.
// An F instruction always ORs this into its result before writing.
const nanBoxUpper = uint64(0xFFFFFFFF00000000)

// ── IEEE-754 single-precision (float32) bit-pattern constants ────────────

const (
	f32CanonNaN = uint32(0x7FC00000) // canonical quiet NaN
	f32PosInf   = uint32(0x7F800000) // +infinity
	f32NegInf   = uint32(0xFF800000) // -infinity
	f32PosZero  = uint32(0x00000000) // +0.0
	f32NegZero  = uint32(0x80000000) // -0.0
	f32SignBit  = uint32(0x80000000) // sign bit mask
	f32ExpMask  = uint32(0x7F800000) // exponent bits
	f32ManMask  = uint32(0x007FFFFF) // mantissa bits
)

// ── IEEE-754 double-precision (float64) bit-pattern constants ────────────

const (
	f64CanonNaN = uint64(0x7FF8000000000000) // canonical quiet NaN
	f64PosInf   = uint64(0x7FF0000000000000) // +infinity
	f64NegInf   = uint64(0xFFF0000000000000) // -infinity
	f64PosZero  = uint64(0x0000000000000000) // +0.0
	f64NegZero  = uint64(0x8000000000000000) // -0.0
	f64SignBit  = uint64(0x8000000000000000) // sign bit mask
	f64ExpMask  = uint64(0x7FF0000000000000) // exponent bits
	f64ManMask  = uint64(0x000FFFFFFFFFFFFF) // mantissa bits
)

// ── FCSR fflags bits ─────────────────────────────────────────────────────
// Written by floating-point instructions to signal exceptions.
// Accumulated in CPU.fcsr[4:0].

const (
	fflagNV = uint32(1 << 4) // invalid operation
	fflagDZ = uint32(1 << 3) // divide by zero
	fflagOF = uint32(1 << 2) // overflow
	fflagUF = uint32(1 << 1) // underflow
	fflagNX = uint32(1 << 0) // inexact
)

// ── FCLASS result bits ────────────────────────────────────────────────────
// FCLASS.S and FCLASS.D write exactly one of these bits to an integer rd.

const (
	fclassNegInf  = uint64(1 << 0) // negative infinity
	fclassNegNorm = uint64(1 << 1) // negative normal number
	fclassNegSub  = uint64(1 << 2) // negative subnormal
	fclassNegZero = uint64(1 << 3) // -0
	fclassPosZero = uint64(1 << 4) // +0
	fclassPosSub  = uint64(1 << 5) // positive subnormal
	fclassPosNorm = uint64(1 << 6) // positive normal number
	fclassPosInf  = uint64(1 << 7) // positive infinity
	fclassSNaN    = uint64(1 << 8) // signaling NaN
	fclassQNaN    = uint64(1 << 9) // quiet NaN
)

// ── NaN-box read/write helpers ────────────────────────────────────────────

// boxF32 NaN-boxes a float32 bit-pattern for storage in an f-register.
// All F instructions call this before SetFReg.
func boxF32(bits uint32) uint64 { return nanBoxUpper | uint64(bits) }

// unboxF32 extracts a float32 from an f-register, enforcing NaN-boxing.
// If the upper 32 bits are not all 1s, returns the canonical quiet NaN.
func unboxF32(freg uint64) uint32 {
	if freg>>32 != 0xFFFFFFFF {
		return f32CanonNaN
	}
	return uint32(freg)
}

// boxF64 stores a float64 bit-pattern directly — no NaN-boxing for doubles.
func boxF64(bits uint64) uint64 { return bits }

// unboxF64 reads a float64 bit-pattern directly from an f-register.
func unboxF64(freg uint64) uint64 { return freg }

// ── float32 ↔ uint32 bit conversion (inlined by Go compiler) ─────────────

func f32bits(v float32) uint32  { return math.Float32bits(v) }
func f32frombits(b uint32) float32 { return math.Float32frombits(b) }
func f64bits(v float64) uint64  { return math.Float64bits(v) }
func f64frombits(b uint64) float64 { return math.Float64frombits(b) }

// ── FCLASS helpers ────────────────────────────────────────────────────────

// fclassF32 returns the FCLASS result for a single-precision value.
func fclassF32(bits uint32) uint64 {
	sign := bits>>31 != 0
	exp  := (bits & f32ExpMask) >> 23
	man  := bits & f32ManMask
	switch {
	case exp == 0xFF && man != 0:
		if man&0x00400000 != 0 { return fclassQNaN }
		return fclassSNaN
	case exp == 0xFF:
		if sign { return fclassNegInf }
		return fclassPosInf
	case exp == 0 && man != 0:
		if sign { return fclassNegSub }
		return fclassPosSub
	case exp == 0:
		if sign { return fclassNegZero }
		return fclassPosZero
	default:
		if sign { return fclassNegNorm }
		return fclassPosNorm
	}
}

// fclassF64 returns the FCLASS result for a double-precision value.
func fclassF64(bits uint64) uint64 {
	sign := bits>>63 != 0
	exp  := (bits & f64ExpMask) >> 52
	man  := bits & f64ManMask
	switch {
	case exp == 0x7FF && man != 0:
		if man&0x0008000000000000 != 0 { return fclassQNaN }
		return fclassSNaN
	case exp == 0x7FF:
		if sign { return fclassNegInf }
		return fclassPosInf
	case exp == 0 && man != 0:
		if sign { return fclassNegSub }
		return fclassPosSub
	case exp == 0:
		if sign { return fclassNegZero }
		return fclassPosZero
	default:
		if sign { return fclassNegNorm }
		return fclassPosNorm
	}
}

// ── FMIN/FMAX NaN-aware comparison ───────────────────────────────────────
// RISC-V FMIN/FMAX follow IEEE 754-2008 minNum/maxNum:
// if exactly one operand is NaN, return the non-NaN; if both NaN, return
// canonical NaN. -0.0 < +0.0 for FMIN.

func fminF32(a, b uint32) uint32 {
	aNaN := (a&f32ExpMask) == f32ExpMask && (a&f32ManMask) != 0
	bNaN := (b&f32ExpMask) == f32ExpMask && (b&f32ManMask) != 0
	if aNaN && bNaN { return f32CanonNaN }
	if aNaN { return b }
	if bNaN { return a }
	// -0.0 < +0.0
	if a == f32NegZero && b == f32PosZero { return a }
	if b == f32NegZero && a == f32PosZero { return b }
	if f32frombits(a) < f32frombits(b) { return a }
	return b
}

func fmaxF32(a, b uint32) uint32 {
	aNaN := (a&f32ExpMask) == f32ExpMask && (a&f32ManMask) != 0
	bNaN := (b&f32ExpMask) == f32ExpMask && (b&f32ManMask) != 0
	if aNaN && bNaN { return f32CanonNaN }
	if aNaN { return b }
	if bNaN { return a }
	if a == f32PosZero && b == f32NegZero { return a }
	if b == f32PosZero && a == f32NegZero { return b }
	if f32frombits(a) > f32frombits(b) { return a }
	return b
}

func fminF64(a, b uint64) uint64 {
	aNaN := (a&f64ExpMask) == f64ExpMask && (a&f64ManMask) != 0
	bNaN := (b&f64ExpMask) == f64ExpMask && (b&f64ManMask) != 0
	if aNaN && bNaN { return f64CanonNaN }
	if aNaN { return b }
	if bNaN { return a }
	if a == f64NegZero && b == f64PosZero { return a }
	if b == f64NegZero && a == f64PosZero { return b }
	if f64frombits(a) < f64frombits(b) { return a }
	return b
}

func fmaxF64(a, b uint64) uint64 {
	aNaN := (a&f64ExpMask) == f64ExpMask && (a&f64ManMask) != 0
	bNaN := (b&f64ExpMask) == f64ExpMask && (b&f64ManMask) != 0
	if aNaN && bNaN { return f64CanonNaN }
	if aNaN { return b }
	if bNaN { return a }
	if a == f64PosZero && b == f64NegZero { return a }
	if b == f64PosZero && a == f64NegZero { return b }
	if f64frombits(a) > f64frombits(b) { return a }
	return b
}

// ── FSGNJ helpers ─────────────────────────────────────────────────────────

func fsgnjF32(a, b uint32) uint32  { return (a &^ f32SignBit) | (b & f32SignBit) }
func fsgnjnF32(a, b uint32) uint32 { return (a &^ f32SignBit) | (^b & f32SignBit) }
func fsgnjxF32(a, b uint32) uint32 { return a ^ (b & f32SignBit) }

func fsgnjF64(a, b uint64) uint64  { return (a &^ f64SignBit) | (b & f64SignBit) }
func fsgnjnF64(a, b uint64) uint64 { return (a &^ f64SignBit) | (^b & f64SignBit) }
func fsgnjxF64(a, b uint64) uint64 { return a ^ (b & f64SignBit) }
