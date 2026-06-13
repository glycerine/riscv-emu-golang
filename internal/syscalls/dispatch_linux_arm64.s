#include "textflag.h"

// JIT-facing syscall dispatcher for linux/arm64.
//
// jit_dispatch<> ABI:
//   R0 = &cpu.x[0]
//   R1 = memBase
//   R2 = memMask
// Return R0: 0 = handled, 1 = fallback.
TEXT jit_dispatch<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVD	R0, R9              // stash xptr
	MOVD	136(R9), R10        // a7 (x[17])

	CMP	$64, R10            // Linux RV SYS_write
	BEQ	jit_write

	MOVD	$1, R0
	RET

jit_write:
	MOVD	88(R9), R10         // guest buf VA
	AND	R2, R10, R10        // bounds-mask
	ADD	R1, R10, R10        // + memBase = host ptr

	MOVD	·jitDispatchWriteCallback(SB), R12
	CMP	$0, R12
	BNE	jit_write_callback

	MOVD	80(R9), R0          // fd
	MOVD	R10, R1             // host buf
	MOVD	96(R9), R2          // count
	MOVD	$64, R8             // Linux arm64 SYS_write
	SVC

	MOVD	R0, 80(R9)
	MOVD	$0, R0
	RET

jit_write_callback:
	MOVD	80(R9), R0          // fd
	MOVD	R10, R1             // host buf
	MOVD	96(R9), R2          // count
	CALL	(R12)
	MOVD	R0, 80(R9)
	MOVD	$0, R0
	RET

TEXT nullWriteCallback<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVD	R2, R0
	RET

TEXT ·CallDispatch(SB), NOSPLIT, $0-32
	MOVD	xptr+0(FP), R0
	MOVD	memBase+8(FP), R1
	MOVD	memMask+16(FP), R2
	CALL	jit_dispatch<>(SB)
	MOVD	R0, ret+24(FP)
	RET

TEXT ·DispatchAddr(SB), NOSPLIT, $0-8
	MOVD	$jit_dispatch<>(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·NullWriteCallbackAddr(SB), NOSPLIT, $0-8
	MOVD	$nullWriteCallback<>(SB), R0
	MOVD	R0, ret+0(FP)
	RET
