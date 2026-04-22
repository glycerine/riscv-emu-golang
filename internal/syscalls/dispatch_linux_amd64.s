#include "textflag.h"

// JIT-facing fast-path SYSCALL dispatcher for linux/amd64.
//
// This file defines three symbols:
//
//   jit_dispatch<>(SB)      file-local asm label, System V AMD64 ABI
//                           (RDI=xptr, RSI=memBase, RDX=memMask, ret in RAX).
//                           The JIT calls this by absolute address.
//
//   ·CallDispatch(SB)       Go-ABI wrapper (for tests). Takes stack args
//                           in Go ABI0 convention and trampolines into
//                           jit_dispatch<>.
//
//   ·DispatchAddr(SB)       Go-callable helper that returns the code
//                           address of jit_dispatch<> so the JIT can
//                           emit it as a MOVABS immediate.
//
// See doc comments in dispatch.go for the overall contract.

// ──────────────────────────────────────────────────────────────────────
// jit_dispatch<> — SysV-ABI dispatcher. The hot path. Uses only
// caller-saved regs (RAX/RCX/RDX/RSI/RDI/R10/R11) so the JIT's
// pinned callee-saves (RBX, R12-R15, RBP) survive naturally.
// ──────────────────────────────────────────────────────────────────────
TEXT jit_dispatch<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	DI, R10              // stash xptr — syscalls clobber RDI
	MOVQ	136(R10), R11        // R11 = a7 (x[17], byte offset 17*8)

	CMPQ	R11, $64             // Linux RV SYS_write
	JE	jit_write

	// Unknown / complex syscall — return 1 so JIT emits jitEcall.
	MOVQ	$1, AX
	RET

jit_write:
	// a0=fd @ x[10]=80(R10), a1=buf @ 88(R10), a2=count @ 96(R10).
	MOVQ	88(R10), R11         // R11 = guest buf VA
	ANDQ	DX, R11              // mask into [0, memSize)
	ADDQ	SI, R11              // + memBase = host pointer
	MOVQ	96(R10), DX          // DX = count   (Linux syscall arg 3)
	MOVQ	R11, SI              // SI = hostbuf (Linux syscall arg 2)
	MOVQ	80(R10), DI          // DI = fd      (Linux syscall arg 1)
	MOVQ	$1, AX               // RAX = Linux SYS_write
	SYSCALL

	MOVQ	AX, 80(R10)          // x[10] = result (or -errno)
	XORQ	AX, AX               // status = 0 (handled)
	RET

// ──────────────────────────────────────────────────────────────────────
// func CallDispatch(xptr unsafe.Pointer, memBase uintptr, memMask uint64) uint64
// Go-ABI0 wrapper — shuffles stack args into SysV regs and tail-calls
// the dispatcher. Only used by tests and debugging; the JIT hot path
// bypasses this by calling jit_dispatch<> directly via DispatchAddr.
// ──────────────────────────────────────────────────────────────────────
TEXT ·CallDispatch(SB), NOSPLIT, $0-32
	MOVQ	xptr+0(FP), DI
	MOVQ	memBase+8(FP), SI
	MOVQ	memMask+16(FP), DX
	CALL	jit_dispatch<>(SB)
	MOVQ	AX, ret+24(FP)
	RET

// ──────────────────────────────────────────────────────────────────────
// func DispatchAddr() uintptr
// Returns the runtime code address of jit_dispatch<> for emission
// into JIT-generated code as an absolute immediate.
// ──────────────────────────────────────────────────────────────────────
TEXT ·DispatchAddr(SB), NOSPLIT, $0-8
	LEAQ	jit_dispatch<>(SB), AX
	MOVQ	AX, ret+0(FP)
	RET
