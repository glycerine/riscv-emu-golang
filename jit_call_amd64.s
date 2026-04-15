#include "textflag.h"

// func callJIT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
//              memBase uintptr, memMask uint64) JITResult
//
// Calls a TCC-compiled native block using the System V AMD64 ABI.
// No cgo overhead — this is a plain CALL instruction.
//
// Go ABI0 stack layout (args + return):
//   fn+0(FP)        uintptr     8 bytes
//   x+8(FP)         *[32]uint64 8 bytes
//   f+16(FP)        *[32]uint64 8 bytes
//   fcsr+24(FP)     *uint32     8 bytes
//   memBase+32(FP)  uintptr     8 bytes
//   memMask+40(FP)  uint64      8 bytes
//   ret+48(FP)      JITResult   32 bytes  (pc, ic, status, pad, fault_addr)
//
// System V ABI for JITResult block_entry(x, f, fcsr, memBase, memMask):
//   JITResult is 32 bytes (> 16), so it uses hidden sret pointer:
//   RDI = hidden pointer to caller-allocated JITResult
//   RSI = x
//   RDX = f
//   RCX = fcsr
//   R8  = memBase
//   R9  = memMask
//
// Local frame: 32 (JITResult sret area) + 48 (6 callee-saved regs) = 80 bytes.
TEXT ·callJIT(SB), NOSPLIT, $80-80

	// Save Go's callee-saved registers (SysV callee-saved: RBX, RBP, R12-R15).
	// TCC-compiled code may clobber these.
	MOVQ	BX,  32(SP)
	MOVQ	BP,  40(SP)
	MOVQ	R12, 48(SP)
	MOVQ	R13, 56(SP)
	MOVQ	R14, 64(SP)
	MOVQ	R15, 72(SP)

	// Set up SysV calling convention arguments.
	// RDI = hidden sret pointer (points to 0(SP), our local JITResult buffer)
	LEAQ	0(SP), DI
	// RSI..R9 = real arguments from Go stack
	MOVQ	x+8(FP), SI
	MOVQ	f+16(FP), DX
	MOVQ	fcsr+24(FP), CX
	MOVQ	memBase+32(FP), R8
	MOVQ	memMask+40(FP), R9

	// Call the JIT'd native function.
	MOVQ	fn+0(FP), AX
	CALL	AX

	// Copy JITResult from local buffer 0(SP) to Go return value at ret+48(FP).
	// JITResult is 32 bytes = 4 QWORDs.
	MOVQ	0(SP),  R10
	MOVQ	8(SP),  R11
	MOVQ	16(SP), R12
	MOVQ	24(SP), R13
	MOVQ	R10, ret+48(FP)
	MOVQ	R11, ret+56(FP)
	MOVQ	R12, ret+64(FP)
	MOVQ	R13, ret+72(FP)

	// Restore callee-saved registers.
	MOVQ	32(SP), BX
	MOVQ	40(SP), BP
	MOVQ	48(SP), R12
	MOVQ	56(SP), R13
	MOVQ	64(SP), R14
	MOVQ	72(SP), R15

	RET
