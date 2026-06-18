//go:build amd64

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

func loadMXCSR(v uint32)

const mxcsrRoundingMask = uint32(0x6000)

// SetRoundingMode changes MXCSR rounding control for the current OS thread.
// RISC-V rm encodings 0..3 map to MXCSR; rm=4 (RMM) has no MXCSR equivalent.
func SetRoundingMode(rm uint8) (old uint32, ok bool) {
	old = RawMXCSR()
	var mx uint32
	switch rm {
	case 0: // RNE
		mx = 0x0000
	case 1: // RTZ
		mx = 0x6000
	case 2: // RDN
		mx = 0x2000
	case 3: // RUP
		mx = 0x4000
	default:
		return old, false
	}
	loadMXCSR((old &^ mxcsrRoundingMask) | mx)
	return old, true
}

// RestoreRoundingMode restores only MXCSR rounding control, preserving the
// exception flags accumulated by the operation that just ran.
func RestoreRoundingMode(old uint32) {
	cur := RawMXCSR()
	loadMXCSR((cur &^ mxcsrRoundingMask) | (old & mxcsrRoundingMask))
}
