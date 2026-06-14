package riscv

// lower_policy.go — native register policy and backend target metadata.

import (
	"encoding/binary"
	"fmt"

	"github.com/glycerine/riscv-emu-golang/goasm"
	"github.com/glycerine/riscv-emu-golang/goasm/obj"
)

// Parameter VRegs by convention (NewEmitter allocates these first).
const (
	VRXBase   = VReg(VRegTempStart + 0) // t64
	VRFBase   = VReg(VRegTempStart + 1) // t65
	VRMemBase = VReg(VRegTempStart + 2) // t66
	VRMemMask = VReg(VRegTempStart + 3) // t67
)

// VRRegFile is the parameter VReg for the register file base pointer,
// pinned to RBP in the rv8 layout.
const VRRegFile = VReg(VRegTempStart + 4) // t68

// WARNING: Do NOT pin R15 via a VReg constant here. The Emitter's Tmp()
// allocates sequential VRegs starting at VRegTempStart+5 (after the 5
// parameter slots t64-t68). Any constant defined here collides with
// those temps — the allocator sees the pinned VReg, assigns R15 to
// the temp, and silently clobbers the budget register. Instead, reserve it
// through RegPolicy.InstructionCounterReg. The budget ops use R15 directly
// on AMD64 without going through the allocator.
//
// const VRIC = VReg(VRegTempStart + 5) // DO NOT USE — collides with first Tmp()

// PatchImm64 writes a native absolute-address patch site.
type PatchImm64 func(code []byte, prog *obj.Prog, value uint64) (int, error)

const nativePatchSentinel = uint64(0x7BADC0DE7BADC0DE)

// RegPolicy bundles register allocation choices for a target configuration.
// Pool, Pinned, Lower, and PatchImm64 must all be non-nil before the policy
// is used for compilation.
type RegPolicy struct {
	Name                  string
	Arch                  goasm.Arch
	InstructionCounterReg int16
	Pool                  func(*Block) RegPool
	Pinned                func() map[VReg]int16
	Lower                 func(*goasm.Ctx, *Block, *Allocation) (*LowerResult, error)
	PatchImm64            PatchImm64
}

func patchAMD64MovabsImm64(code []byte, prog *obj.Prog, value uint64) (int, error) {
	if prog == nil {
		return 0, fmt.Errorf("nil patch prog")
	}
	patchOff := int(prog.Pc) + 2
	if patchOff < 0 || patchOff+8 > len(code) {
		return 0, fmt.Errorf("patch offset %d outside code length %d", patchOff, len(code))
	}
	binary.LittleEndian.PutUint64(code[patchOff:], value)
	return patchOff, nil
}

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
	// X13-X15 are reserved for FP staging, including ternary FMA.
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12,
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

// PolicyRV8 is the rv8-faithful register policy: 12-register pool,
// RBP pinned to VRRegFile, LowerAMD64_RV8 lowerer.
var PolicyRV8 = RegPolicy{
	Name:                  "rv8",
	Arch:                  goasm.AMD64,
	InstructionCounterReg: goasm.REG_AMD64_R15,
	Pool:                  RV8Pool,
	Pinned:                RV8Pinned,
	Lower:                 LowerAMD64_RV8,
	PatchImm64:            patchAMD64MovabsImm64,
}

// ABJITPool returns the 11-register integer allocation pool for the abjit
// trampoline path. Excludes R14 (Go goroutine pointer, unsafe when JIT
// code can trigger Go callbacks).
func ABJITPool(_ *Block) RegPool {
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
		goasm.REG_AMD64_R15,
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

// ABJITPinned returns the pinned VReg → host register map for abjit.
// Same as RV8Pinned: only VRRegFile → RBP.
func ABJITPinned() map[VReg]int16 {
	return map[VReg]int16{
		VRRegFile: goasm.REG_AMD64_BP,
	}
}

// PolicyABJIT is the abjit register policy: 11-register pool (no R14),
// RBP pinned to VRRegFile, LowerAMD64_ABJIT lowerer.
var PolicyABJIT = RegPolicy{
	Name:                  "abjit",
	Arch:                  goasm.AMD64,
	InstructionCounterReg: goasm.REG_AMD64_R15,
	Pool:                  ABJITPool,
	Pinned:                ABJITPinned,
	Lower:                 LowerAMD64_ABJIT,
	PatchImm64:            patchAMD64MovabsImm64,
}
