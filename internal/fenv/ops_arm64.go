//go:build arm64

package fenv

// FFlags reads the host ARM64 FPSR exception bits, translates them to
// RISC-V fflags layout, clears the host flags, and returns the result.
//
// RISC-V fflags layout returned: NV(4) DZ(3) OF(2) UF(1) NX(0).
func FFlags() uint32

// ClearFFlags clears the host FP exception flags.
func ClearFFlags()

// RawMXCSR is kept for package API symmetry with amd64. On ARM64 it returns
// the raw FPSR value.
func RawMXCSR() uint32

func AddF32(a, b float32) (float32, uint32)
func SubF32(a, b float32) (float32, uint32)
func MulF32(a, b float32) (float32, uint32)
func DivF32(a, b float32) (float32, uint32)
func SqrtF32(a float32) (float32, uint32)

func AddF64(a, b float64) (float64, uint32)
func SubF64(a, b float64) (float64, uint32)
func MulF64(a, b float64) (float64, uint32)
func DivF64(a, b float64) (float64, uint32)
func SqrtF64(a float64) (float64, uint32)

func MAddF32(a, b, c float32) (float32, uint32)
func MSubF32(a, b, c float32) (float32, uint32)
func NMAddF32(a, b, c float32) (float32, uint32)
func NMSubF32(a, b, c float32) (float32, uint32)

func MAddF64(a, b, c float64) (float64, uint32)
func MSubF64(a, b, c float64) (float64, uint32)
func NMAddF64(a, b, c float64) (float64, uint32)
func NMSubF64(a, b, c float64) (float64, uint32)
