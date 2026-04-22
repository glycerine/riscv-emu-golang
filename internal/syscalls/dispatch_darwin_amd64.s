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

	// Phase 4: consult the optional write-callback. If non-nil, call
	// it instead of issuing a kernel SYSCALL. Used for dispatch-only
	// benchmarking (match libriscv's null_stdout cost model).
	MOVQ	·jitDispatchWriteCallback(SB), AX
	TESTQ	AX, AX
	JNZ	jit_write_callback

	// Fast path: inline kernel SYSCALL.
	MOVQ	96(R10), DX          // count
	MOVQ	R11, SI              // host buf
	MOVQ	80(R10), DI          // fd
	MOVQ	$0x02000004, AX      // darwin BSD SYS_write
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
	RET

jit_write_callback:
	// Callback ABI: RDI=fd, RSI=hostBuf, RDX=count; return RAX.
	MOVQ	96(R10), DX          // count
	MOVQ	R11, SI              // host buf
	MOVQ	80(R10), DI          // fd
	CALL	AX                   // callback pointer in AX from load above
	MOVQ	AX, 80(R10)          // x[10] = bytes "written"
	XORQ	AX, AX               // status = 0 (handled)
	RET

// Built-in null callback — SysV ABI, returns RDX (count) as RAX.
// Mirrors libriscv's null_stdout: does no work, pretends success.
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
