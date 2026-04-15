// Package fenv provides access to the host CPU's floating-point
// exception flags for translating them into RISC-V fflags format.
package fenv

// FFlags reads the host CPU's FP exception sticky flags, translates
// them to RISC-V fflags bit layout, clears the host flags, and returns
// the result. On amd64 this reads/clears MXCSR bits 0-5 via STMXCSR/LDMXCSR.
//
// RISC-V fflags layout returned: NV(4) DZ(3) OF(2) UF(1) NX(0)
func FFlags() uint32

// ClearFFlags clears the host FP exception flag bits without reading them.
func ClearFFlags()

// RawMXCSR returns the raw MXCSR value without modification (for debugging).
func RawMXCSR() uint32
