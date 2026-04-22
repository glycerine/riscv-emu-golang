#include "textflag.h"

// JIT-facing fast-path SYSCALL dispatcher for linux/amd64.
//
// This file defines four symbols:
//
//   jit_dispatch<>(SB)           file-local dispatcher, SysV AMD64 ABI.
//                                 The JIT calls this by absolute address.
//
//   ·CallDispatch(SB)            Go-ABI wrapper (tests only).
//
//   ·DispatchAddr(SB)            Returns jit_dispatch<> code address.
//
//   ·NullWriteCallbackAddr(SB)   Returns the built-in no-op callback
//                                 address (for dispatch-only benchmarks).
//
// See dispatch.go for the full contract.
//
// jit_dispatch<>: On entry RDI=xptr, RSI=memBase, RDX=memMask.
// Return RAX: 0 = handled (JIT continues at pc+4), 1 = fallback
// (JIT emits jitEcall).
TEXT jit_dispatch<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	DI, R10              // stash xptr
	MOVQ	136(R10), R11        // a7 (x[17])

	CMPQ	R11, $64             // Linux RV SYS_write
	JE	jit_write

	MOVQ	$1, AX
	RET

jit_write:
	MOVQ	88(R10), R11         // guest buf VA
	ANDQ	DX, R11              // bounds-mask
	ADDQ	SI, R11              // + memBase = host ptr

	// Phase 4: optional write-callback. If a C-ABI function pointer
	// is registered, CALL it; else fall through to kernel SYSCALL.
	MOVQ	·jitDispatchWriteCallback(SB), AX
	TESTQ	AX, AX
	JNZ	jit_write_callback

	// Fast path: inline kernel SYSCALL.
	MOVQ	96(R10), DX          // count
	MOVQ	R11, SI              // host buf
	MOVQ	80(R10), DI          // fd
	MOVQ	$1, AX               // Linux SYS_write
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
	RET

jit_write_callback:
	// Callback ABI: RDI=fd, RSI=hostBuf, RDX=count; return RAX.
	MOVQ	96(R10), DX          // count
	MOVQ	R11, SI              // host buf
	MOVQ	80(R10), DI          // fd
	CALL	AX                   // callback pointer (in AX from load)
	MOVQ	AX, 80(R10)          // x[10] = bytes "written"
	XORQ	AX, AX               // status = 0 (handled)
	RET

// Built-in null callback — SysV ABI. Mirrors libriscv's null_stdout:
// does no work, returns count as "bytes written".
TEXT nullWriteCallback<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	DX, AX
	RET

TEXT ·CallDispatch(SB), NOSPLIT, $0-32
	MOVQ	xptr+0(FP), DI
	MOVQ	memBase+8(FP), SI
	MOVQ	memMask+16(FP), DX
	CALL	jit_dispatch<>(SB)
	MOVQ	AX, ret+24(FP)
	RET

TEXT ·DispatchAddr(SB), NOSPLIT, $0-8
	LEAQ	jit_dispatch<>(SB), AX
	MOVQ	AX, ret+0(FP)
	RET

TEXT ·NullWriteCallbackAddr(SB), NOSPLIT, $0-8
	LEAQ	nullWriteCallback<>(SB), AX
	MOVQ	AX, ret+0(FP)
	RET
