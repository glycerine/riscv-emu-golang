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
//   ret+48(FP)      Result      24 bytes (PC, Status, FaultAddr)
//
// System V AMD64 ABI — struct return > 16 bytes uses hidden sret pointer:
//   RDI = hidden pointer to caller-allocated Result
//   RSI = x  (register file)
//   RDX = f  (float regs)
//   RCX = fcsr
//   R8  = memBase
//   R9  = memMask
//
// Local frame layout (the callee-visible sret buffer is RDI = &frame[0]):
//   [0, 24)   Result struct (PC, Status, FaultAddr)
//   [32, 80)  trampoline's own callee-save stashes (BX, BP, R12-R15)
//   [80, 88)  fcsr pointer — written here once per Call so JIT code can
//             access it via [RBX+80] even from chained blocks (where RCX
//             is unavailable, having been released to the regalloc pool).
//             Must agree with ir.amd64SretFcsrOffset.
//   [88, …)   unused / TCC/JIT code may spill below its own RSP after
//             the callee's prologue runs SubQ.
// NOSPLIT frame size 65536 is well above any reasonable callee usage.
TEXT ·Call(SB), $65536-72

	// Save callee-saved registers that JIT/TCC code may clobber.
	MOVQ	BX,  32(SP)
	MOVQ	BP,  40(SP)
	MOVQ	R12, 48(SP)
	MOVQ	R13, 56(SP)
	MOVQ	R14, 64(SP)
	MOVQ	R15, 72(SP)

	// Zero the entire sret metadata region [80..143] before publishing
	// known values. JIT code reads from various offsets in this range
	// (fcsr, decoder_cache params, memBase/memMask). Zeroing everything
	// prevents any offset from containing stack garbage, eliminating a
	// class of non-deterministic crashes. CallAOT overwrites 88-119
	// with real decoder_cache values; the JIT prologue overwrites
	// 128-143 with memBase/memMask from R8/R9.
	MOVQ	$0,  80(SP)
	MOVQ	$0,  88(SP)
	MOVQ	$0,  96(SP)
	MOVQ	$0, 104(SP)
	MOVQ	$0, 112(SP)
	MOVQ	$0, 120(SP)
	MOVQ	$0, 128(SP)
	MOVQ	$0, 136(SP)
	MOVQ	$0, 144(SP)

	// Publish fcsr pointer into the sret buffer at [SP+80].
	MOVQ	fcsr+24(FP), AX
	MOVQ	AX,  80(SP)

	// Set up System V calling convention arguments.
	LEAQ	0(SP), DI           // RDI = hidden sret pointer (local Result buffer)
	MOVQ	x+8(FP), SI        // RSI = x
	MOVQ	f+16(FP), DX       // RDX = f
	MOVQ	fcsr+24(FP), CX    // RCX = fcsr (legacy: also available as arg; newer lowerers read from [RBX+80])
	MOVQ	memBase+32(FP), R8  // R8  = memBase
	MOVQ	memMask+40(FP), R9  // R9  = memMask

	// Call the JIT'd native function.
	MOVQ	fn+0(FP), AX
	CALL	AX

	// Copy Result from local buffer at 0(SP) to Go return area.
	// Result is 24 bytes = 3 quadwords.
	MOVQ	0(SP),  AX
	MOVQ	8(SP),  CX
	MOVQ	16(SP), DX
	MOVQ	AX, ret_PC+48(FP)
	MOVQ	CX, ret_Status+56(FP)
	MOVQ	DX, ret_FaultAddr+64(FP)

	// Restore callee-saved registers.
	MOVQ	32(SP), BX
	MOVQ	40(SP), BP
	MOVQ	48(SP), R12
	MOVQ	56(SP), R13
	MOVQ	64(SP), R14
	MOVQ	72(SP), R15

	RET

// func CallAOT(fn, x, f, fcsr, memBase, memMask, dcBase, dcMask, vBegin, segSize) Result
//
// Same as Call but also publishes four AOT-related values into the
// sret buffer at [SP+88..112] so the JIT's JALR decoder_cache
// lookup sequence can read them as [RBX+88..112].
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
//   ret+80(FP)       Result         24
TEXT ·CallAOT(SB), $65536-104

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
	MOVQ	AX, ret_PC+80(FP)
	MOVQ	CX, ret_Status+88(FP)
	MOVQ	DX, ret_FaultAddr+96(FP)

	// Restore callee-saved registers.
	MOVQ	32(SP), BX
	MOVQ	40(SP), BP
	MOVQ	48(SP), R12
	MOVQ	56(SP), R13
	MOVQ	64(SP), R14
	MOVQ	72(SP), R15

	RET
