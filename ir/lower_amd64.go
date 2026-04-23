package ir

// lower_amd64.go — AMD64 register layout constants and pool definitions.

import "riscv/goasm"

// Parameter VRegs by convention (NewEmitter allocates these first).
const (
	VRXBase   = VReg(VRegTempStart + 0) // t64
	VRFBase   = VReg(VRegTempStart + 1) // t65
	VRIC      = VReg(VRegTempStart + 2) // t66
	VRMemBase = VReg(VRegTempStart + 3) // t67
	VRMemMask = VReg(VRegTempStart + 4) // t68
)

// VRRegFile is the parameter VReg for the register file base pointer,
// pinned to RBP in the rv8 layout.
const VRRegFile = VReg(VRegTempStart + 5) // t69

// RV8Pool returns the 12-register integer allocation pool matching rv8's
// Table 1. RAX, RCX, RBP, and RSP are excluded (reserved).
// DIV/MUL is handled by local save/restore of RDX, not pool shrinking.
func RV8Pool(_ *Block) RegPool {
	intRegs := []int16{
		goasm.REG_AMD64_DX,
		goasm.REG_AMD64_BX,
		goasm.REG_AMD64_SI,
		goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8,
		goasm.REG_AMD64_R9,
		goasm.REG_AMD64_R10,
		goasm.REG_AMD64_R11,
		goasm.REG_AMD64_R12,
		goasm.REG_AMD64_R13,
		goasm.REG_AMD64_R14,
		goasm.REG_AMD64_R15,
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13, goasm.REG_AMD64_X14, goasm.REG_AMD64_X15,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

// RV8Pinned returns the pinned VReg → host register map for the rv8 layout.
// Only VRRegFile → RBP is pinned; all other parameter VRegs are loaded
// from [RBP+offset] by the prologue.
func RV8Pinned() map[VReg]int16 {
	return map[VReg]int16{
		VRRegFile: goasm.REG_AMD64_BP,
	}
}
