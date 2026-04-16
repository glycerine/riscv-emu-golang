#include "textflag.h"

// func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
//           memBase uintptr, memMask uint64) Result
//
// Calls a TCC-compiled native block using the System V AMD64 ABI.
// No cgo overhead — just a plain CALL instruction.
//
// Go ABI0 stack layout (args + return):
//   fn+0(FP)        uintptr     8 bytes
//   x+8(FP)         *[32]uint64 8 bytes
//   f+16(FP)        *[32]uint64 8 bytes
//   fcsr+24(FP)     *uint32     8 bytes
//   memBase+32(FP)  uintptr     8 bytes
//   memMask+40(FP)  uint64      8 bytes
//   ret+48(FP)      Result      32 bytes
//
// System V AMD64 ABI — struct return > 16 bytes uses hidden sret pointer:
//   RDI = hidden pointer to caller-allocated Result
//   RSI = x  (register file)
//   RDX = f  (float regs)
//   RCX = fcsr
//   R8  = memBase
//   R9  = memMask
//
// Local frame: 768 bytes. Must be large enough for TCC-compiled code's
// stack frame (up to 32 uint64_t locals = 256 bytes plus temporaries).
// NOSPLIT limit is 792, so 768 is the practical maximum.
// The sret buffer occupies bytes [0,32), callee-saved regs at [32,80).
// TCC code uses the remainder via RSP after its own prologue.
TEXT ·Call(SB), $65536-80

	// Save callee-saved registers that TCC-compiled code may clobber.
	MOVQ	BX,  32(SP)
	MOVQ	BP,  40(SP)
	MOVQ	R12, 48(SP)
	MOVQ	R13, 56(SP)
	MOVQ	R14, 64(SP)
	MOVQ	R15, 72(SP)

	// Set up System V calling convention arguments.
	LEAQ	0(SP), DI           // RDI = hidden sret pointer (local Result buffer)
	MOVQ	x+8(FP), SI        // RSI = x
	MOVQ	f+16(FP), DX       // RDX = f
	MOVQ	fcsr+24(FP), CX    // RCX = fcsr
	MOVQ	memBase+32(FP), R8  // R8  = memBase
	MOVQ	memMask+40(FP), R9  // R9  = memMask

	// Call the JIT'd native function.
	MOVQ	fn+0(FP), AX
	CALL	AX

	// Copy Result from local buffer at 0(SP) to Go return area.
	// Result is 32 bytes = 4 quadwords. Use named subfields for go vet.
	MOVQ	0(SP),  AX
	MOVQ	8(SP),  CX
	MOVQ	16(SP), DX
	MOVQ	24(SP), SI
	MOVQ	AX, ret_PC+48(FP)
	MOVQ	CX, ret_IC+56(FP)
	MOVQ	DX, ret_Status+64(FP)
	MOVQ	SI, ret_FaultAddr+72(FP)

	// Restore callee-saved registers.
	MOVQ	32(SP), BX
	MOVQ	40(SP), BP
	MOVQ	48(SP), R12
	MOVQ	56(SP), R13
	MOVQ	64(SP), R14
	MOVQ	72(SP), R15

	RET
