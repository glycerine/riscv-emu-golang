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
	CMPQ	R11, $63             // Linux RV SYS_read
	JE	jit_read
	CMPQ	R11, $57             // Linux RV SYS_close
	JE	jit_close
	CMPQ	R11, $62             // Linux RV SYS_lseek
	JE	jit_lseek
	CMPQ	R11, $96             // Linux RV SYS_set_tid_address
	JE	jit_set_tid_address
	CMPQ	R11, $172            // Linux RV SYS_getpid
	JE	jit_getpid
	CMPQ	R11, $178            // Linux RV SYS_gettid
	JE	jit_gettid
	CMPQ	R11, $214            // Linux RV SYS_brk
	JE	jit_brk

	MOVQ	$1, AX
	RET

jit_read:
	MOVQ	88(R10), R11         // guest buf VA
	ANDQ	DX, R11              // bounds-mask
	ADDQ	SI, R11              // + memBase = host ptr

	MOVQ	96(R10), DX          // count
	MOVQ	R11, SI              // host buf
	MOVQ	80(R10), DI          // fd
	XORQ	AX, AX               // Linux amd64 SYS_read
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
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

jit_close:
	MOVQ	80(R10), DI          // fd
	MOVQ	$3, AX               // Linux amd64 SYS_close
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
	RET

jit_lseek:
	MOVQ	96(R10), DX          // whence
	MOVQ	88(R10), SI          // offset
	MOVQ	80(R10), DI          // fd
	MOVQ	$8, AX               // Linux amd64 SYS_lseek
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
	RET

jit_set_tid_address:
	MOVQ	$1, R11              // lightweight benchmark/libc stub
	MOVQ	R11, 80(R10)
	XORQ	AX, AX
	RET

jit_getpid:
	MOVQ	$39, AX              // Linux amd64 SYS_getpid
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
	RET

jit_gettid:
	MOVQ	$186, AX             // Linux amd64 SYS_gettid
	SYSCALL

	MOVQ	AX, 80(R10)
	XORQ	AX, AX
	RET

jit_brk:
	XORQ	R11, R11             // match the repo's minimal brk handler
	MOVQ	R11, 80(R10)
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
