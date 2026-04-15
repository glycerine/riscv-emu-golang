package fenv

// ops.go — Go declarations for the float operation + flag capture asm functions.
// Each function performs one IEEE 754 operation and returns the result plus
// the RISC-V fflags bits that the operation set (NV=16 DZ=8 OF=4 UF=2 NX=1).
// The fflags value should be OR'd into cpu.fcsr after each float instruction.

// float32 arithmetic
func AddF32(a, b float32) (float32, uint32)
func SubF32(a, b float32) (float32, uint32)
func MulF32(a, b float32) (float32, uint32)
func DivF32(a, b float32) (float32, uint32)
func SqrtF32(a float32) (float32, uint32)

// float64 arithmetic
func AddF64(a, b float64) (float64, uint32)
func SubF64(a, b float64) (float64, uint32)
func MulF64(a, b float64) (float64, uint32)
func DivF64(a, b float64) (float64, uint32)
func SqrtF64(a float64) (float64, uint32)

// float32 fused multiply-add variants
func MAddF32(a, b, c float32) (float32, uint32)  // a*b + c
func MSubF32(a, b, c float32) (float32, uint32)  // a*b - c
func NMAddF32(a, b, c float32) (float32, uint32) // -(a*b) - c
func NMSubF32(a, b, c float32) (float32, uint32) // -(a*b) + c

// float64 fused multiply-add variants
func MAddF64(a, b, c float64) (float64, uint32)
func MSubF64(a, b, c float64) (float64, uint32)
func NMAddF64(a, b, c float64) (float64, uint32)
func NMSubF64(a, b, c float64) (float64, uint32)
