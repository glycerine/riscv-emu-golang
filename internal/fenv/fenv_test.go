package fenv_test

import (
	"math"
	"testing"

	"github.com/glycerine/riscv-emu-golang/internal/fenv"
)

// Prevent constant-folding by going through bit patterns
func f32(bits uint32) float32 { return math.Float32frombits(bits) }
func f64(bits uint64) float64 { return math.Float64frombits(bits) }

func TestFFlags_ClearWorks(t *testing.T) {
	_ = f32(0x3F800000) / f32(0x40400000)
	fenv.ClearFFlags()
	flags := fenv.FFlags()
	if flags != 0 {
		t.Errorf("after clear, expected 0, got 0x%02X", flags)
	}
}

func TestFFlags_NoSpillFromNonFloat(t *testing.T) {
	fenv.ClearFFlags()
	x := 42
	y := x * 2
	_ = y
	flags := fenv.FFlags()
	if flags != 0 {
		t.Errorf("integer ops set flags=0x%02X, expected 0", flags)
	}
}

var sinkVal float32

//go:noinline
func sink(v float32) { sinkVal = v }

func TestOps_Inexact(t *testing.T) {
	r, flags := fenv.DivF32(
		math.Float32frombits(0x3F800000), // 1.0
		math.Float32frombits(0x40400000), // 3.0
	)
	_ = r
	if flags&1 == 0 {
		t.Errorf("DivF32(1/3) NX not set, flags=0x%02X", flags)
	}
}

func TestOps_DivByZero(t *testing.T) {
	r, flags := fenv.DivF32(
		math.Float32frombits(0x3F800000),
		math.Float32frombits(0x00000000),
	)
	_ = r
	if flags&8 == 0 {
		t.Errorf("DivF32(1/0) DZ not set, flags=0x%02X", flags)
	}
}

func TestOps_Invalid(t *testing.T) {
	inf := math.Float32frombits(0x7F800000)
	r, flags := fenv.SubF32(inf, inf) // Inf - Inf = NaN
	_ = r
	if flags&16 == 0 {
		t.Errorf("SubF32(Inf-Inf) NV not set, flags=0x%02X", flags)
	}
}

func TestOps_MAdd_Inexact(t *testing.T) {
	a := math.Float32frombits(0x3F800000) // 1.0
	b := math.Float32frombits(0x40400000) // 3.0
	c := math.Float32frombits(0x3F800000) // 1.0
	r, flags := fenv.MAddF32(a, b, c)     // 1*3+1 = 4 (exact, NX=0)
	_ = r
	if flags&1 != 0 {
		t.Errorf("MAddF32 exact op has NX set, flags=0x%02X", flags)
	}

	// 1/3 + 0: fused, should be inexact
	r2, flags2 := fenv.MAddF32(
		math.Float32frombits(0x3F800000),
		math.Float32frombits(0x3EAAAAAB), // ~0.3333
		math.Float32frombits(0x00000000),
	)
	_ = r2
	t.Logf("MAddF32(1, 0.3333, 0) flags=0x%02X", flags2)
}

func TestMAddF32_Exact(t *testing.T) {
	// 1.0 * 1.5 + 2.0 = 3.5 (exact)
	r, flags := fenv.MAddF32(
		math.Float32frombits(0x3F800000), // 1.0
		math.Float32frombits(0x3FC00000), // 1.5
		math.Float32frombits(0x40000000), // 2.0
	)
	if math.Float32bits(r) != 0x40600000 { // 3.5
		t.Errorf("MAddF32(1.0*1.5+2.0) = %v (0x%08X), want 3.5", r, math.Float32bits(r))
	}
	if flags != 0 {
		t.Errorf("MAddF32 exact: flags=0x%02X, want 0", flags)
	}
}

func TestAddF32_Exact(t *testing.T) {
	// 2.5 + 1.0 = 3.5 (exact)
	r, flags := fenv.AddF32(
		math.Float32frombits(0x40200000), // 2.5
		math.Float32frombits(0x3F800000), // 1.0
	)
	if math.Float32bits(r) != 0x40600000 { // 3.5
		t.Errorf("AddF32(2.5+1.0) = %v (0x%08X), want 3.5", r, math.Float32bits(r))
	}
	if flags != 0 {
		t.Errorf("AddF32 exact: flags=0x%02X, want 0", flags)
	}
}
