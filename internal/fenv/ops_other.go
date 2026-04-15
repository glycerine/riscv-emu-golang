//go:build !amd64

package fenv

import "math"

func AddF32(a, b float32) (float32, uint32) { return a + b, 0 }
func SubF32(a, b float32) (float32, uint32) { return a - b, 0 }
func MulF32(a, b float32) (float32, uint32) { return a * b, 0 }
func DivF32(a, b float32) (float32, uint32) { return a / b, 0 }
func SqrtF32(a float32) (float32, uint32)   { return float32(math.Sqrt(float64(a))), 0 }

func AddF64(a, b float64) (float64, uint32) { return a + b, 0 }
func SubF64(a, b float64) (float64, uint32) { return a - b, 0 }
func MulF64(a, b float64) (float64, uint32) { return a * b, 0 }
func DivF64(a, b float64) (float64, uint32) { return a / b, 0 }
func SqrtF64(a float64) (float64, uint32)   { return math.Sqrt(a), 0 }

func MAddF32(a, b, c float32) (float32, uint32)  { return a*b + c, 0 }
func MSubF32(a, b, c float32) (float32, uint32)  { return a*b - c, 0 }
func NMAddF32(a, b, c float32) (float32, uint32) { return -(a*b) - c, 0 }
func NMSubF32(a, b, c float32) (float32, uint32) { return -(a*b) + c, 0 }

func MAddF64(a, b, c float64) (float64, uint32)  { return a*b + c, 0 }
func MSubF64(a, b, c float64) (float64, uint32)  { return a*b - c, 0 }
func NMAddF64(a, b, c float64) (float64, uint32) { return -(a*b) - c, 0 }
func NMSubF64(a, b, c float64) (float64, uint32) { return -(a*b) + c, 0 }
