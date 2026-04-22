#include "textflag.h"

// JIT-facing fast-path SYSCALL dispatcher for darwin/amd64.
// See dispatch_linux_amd64.s for the contract and exported symbols.
// Only differences from linux: syscall numbers (BSD class prefix 0x02000000).

TEXT jit_dispatch<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	DI, R10              // stash xptr
	MOVQ	136(R10), R11        // R11 = a7 (x[17])

	CMPQ	R11, $64             // Linux RV SYS_write — unchanged guest ABI
	JE	jit_write

	MOVQ	$1, AX
	RET

jit_write:
	MOVQ	88(R10), R11         // guest buf
	ANDQ	DX, R11              // bounds-mask
	ADDQ	SI, R11              // + memBase = host ptr
	MOVQ	96(R10), DX          // count
	MOVQ	R11, SI              // host buf
	MOVQ	80(R10), DI          // fd
	MOVQ	$0x02000004, AX      // darwin BSD SYS_write
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
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
