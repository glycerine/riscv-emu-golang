package goasm_test

import (
	"testing"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

// TestRegs_AllResolve takes a reference to one constant per arch group
// and one register from each newly-added section. Any rename or
// deletion in the underlying obj/<arch>/a.out.go that breaks our
// re-export will fail compilation here, before the bug reaches a real
// caller.
func TestRegs_AllResolve(t *testing.T) {
	// AMD64
	_ = goasm.REG_AMD64_AX
	_ = goasm.REG_AMD64_R15
	_ = goasm.REG_AMD64_X0
	_ = goasm.REG_AMD64_Y15
	_ = goasm.REG_AMD64_Z31

	// ARM64
	_ = goasm.REG_ARM64_R0
	_ = goasm.REG_ARM64_ZR
	_ = goasm.REG_ARM64_RSP
	_ = goasm.REG_ARM64_G
	_ = goasm.REG_ARM64_F31
	_ = goasm.REG_ARM64_V31

	// ARM 32-bit (incl. status regs added in this audit pass).
	_ = goasm.REG_ARM_R0
	_ = goasm.REG_ARM_PC
	_ = goasm.REG_ARM_F15
	_ = goasm.REG_ARM_FPSR
	_ = goasm.REG_ARM_FPCR
	_ = goasm.REG_ARM_CPSR
	_ = goasm.REG_ARM_SPSR

	// RISC-V (Go's backend; ABI aliases added in this pass).
	_ = goasm.REG_RISCV_X0
	_ = goasm.REG_RISCV_X31
	_ = goasm.REG_RISCV_F31
	_ = goasm.REG_RISCV_ZERO
	_ = goasm.REG_RISCV_RA
	_ = goasm.REG_RISCV_SP
	_ = goasm.REG_RISCV_GP
	_ = goasm.REG_RISCV_TP
	_ = goasm.REG_RISCV_LR

	// MIPS (HI/LO/ZERO added in this pass).
	_ = goasm.REG_MIPS_R0
	_ = goasm.REG_MIPS_F31
	_ = goasm.REG_MIPS_HI
	_ = goasm.REG_MIPS_LO
	_ = goasm.REG_MIPS_ZERO

	// PPC64 (V/LR/CTR/XER/MSR/FPSCR added in this pass).
	_ = goasm.REG_PPC64_R0
	_ = goasm.REG_PPC64_F31
	_ = goasm.REG_PPC64_V0
	_ = goasm.REG_PPC64_V31
	_ = goasm.REG_PPC64_LR
	_ = goasm.REG_PPC64_CTR
	_ = goasm.REG_PPC64_XER
	_ = goasm.REG_PPC64_MSR
	_ = goasm.REG_PPC64_FPSCR

	// LoongArch64 (LSX V0..V31, LASX X0..X31 added in this pass).
	_ = goasm.REG_LOONG64_R0
	_ = goasm.REG_LOONG64_F31
	_ = goasm.REG_LOONG64_V0
	_ = goasm.REG_LOONG64_V31
	_ = goasm.REG_LOONG64_X0
	_ = goasm.REG_LOONG64_X31

	// s390x.
	_ = goasm.REG_S390X_R0
	_ = goasm.REG_S390X_F15
	_ = goasm.REG_S390X_V31

	// WASM (F16..F31 + PC_B added in this pass).
	_ = goasm.REG_WASM_SP
	_ = goasm.REG_WASM_R15
	_ = goasm.REG_WASM_F0
	_ = goasm.REG_WASM_F15
	_ = goasm.REG_WASM_F16
	_ = goasm.REG_WASM_F31
	_ = goasm.REG_WASM_PC_B
}
