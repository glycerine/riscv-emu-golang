#include "textflag.h"

// func CallAOT(fn, x, f, fcsr, memBase, memMask, dcBase, dcMask, vBegin, segSize, pc) Result
//
// Same as Call but also publishes AOT-related values into the sret
// buffer so the JIT can read them as [RBX+offset]:
//   [RBX+88..112]  decoder_cache state (JALR fast path)
//   [RBX+120]      dispatch PC (function re-entry target)
//
// Go ABI0 stack layout:
//   fn+0(FP)         uintptr        8 bytes
//   x+8(FP)          *[32]uint64    8
//   f+16(FP)         *[32]uint64    8
//   fcsr+24(FP)      *uint32        8
//   memBase+32(FP)   uintptr        8
//   memMask+40(FP)   uint64         8
//   dcBase+48(FP)    uintptr        8
//   dcMask+56(FP)    uint64         8
//   vBegin+64(FP)    uint64         8
//   segSize+72(FP)   uint64         8
//   pc+80(FP)        uint64         8
//   ret+88(FP)       Result         32
TEXT ·CallAOT(SB), $65536-120

	// Save callee-saved registers that JIT/TCC code may clobber.
	MOVQ	BX,  32(SP)
	MOVQ	BP,  40(SP)
	MOVQ	R12, 48(SP)
	MOVQ	R13, 56(SP)
	MOVQ	R14, 64(SP)
	MOVQ	R15, 72(SP)

	// Publish fcsr pointer into the sret buffer at [SP+80].
	MOVQ	fcsr+24(FP), AX
	MOVQ	AX,  80(SP)

	// Publish AOT state into the sret buffer at [SP+88..112].
	MOVQ	dcBase+48(FP),  AX
	MOVQ	AX,  88(SP)
	MOVQ	dcMask+56(FP),  AX
	MOVQ	AX,  96(SP)
	MOVQ	vBegin+64(FP),  AX
	MOVQ	AX, 104(SP)
	MOVQ	segSize+72(FP), AX
	MOVQ	AX, 112(SP)

	// Publish dispatch PC at [SP+120] for function re-entry.
	MOVQ	pc+80(FP), AX
	MOVQ	AX, 120(SP)

	// Set up System V calling convention arguments.
	LEAQ	0(SP), DI           // RDI = hidden sret pointer
	MOVQ	x+8(FP), SI         // RSI = x
	MOVQ	f+16(FP), DX        // RDX = f
	MOVQ	fcsr+24(FP), CX     // RCX = fcsr (legacy)
	MOVQ	memBase+32(FP), R8  // R8  = memBase
	MOVQ	memMask+40(FP), R9  // R9  = memMask

	// Call the JIT'd native function.
	MOVQ	fn+0(FP), AX
	CALL	AX

	// Copy Result from local buffer at 0(SP) to Go return area.
	MOVQ	0(SP),  AX
	MOVQ	8(SP),  CX
	MOVQ	16(SP), DX
	MOVQ	24(SP), SI
	MOVQ	AX, ret_PC+88(FP)
	MOVQ	CX, ret_IC+96(FP)
	MOVQ	DX, ret_Status+104(FP)
	MOVQ	SI, ret_FaultAddr+112(FP)

	// Restore callee-saved registers.
	MOVQ	32(SP), BX
	MOVQ	40(SP), BP
	MOVQ	48(SP), R12
	MOVQ	56(SP), R13
	MOVQ	64(SP), R14
	MOVQ	72(SP), R15

	RET
