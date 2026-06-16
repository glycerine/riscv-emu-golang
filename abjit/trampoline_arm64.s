#include "funcdata.h"
#include "textflag.h"

// func callJIT(code, regFileBase uintptr)
//
// ARM64 abjit convention used by the conservative lowerer:
//   R20 = abjit.State / register-file base
//   R15 = remaining guest-instruction budget, loaded from State.IC
//
// The generated code runs with SP moved into the middle of this large
// no-pointer Go frame. That lets JIT stack slots and helper-call spill space
// stay inside a Go-known frame if async stack scanning happens while native
// code is running. Generated code exits by jumping to the instruction after
// the first BLR below.
//
// R19 is also used by the gocall thunk as the generated-code resume address.
// Go/ARM64 preserves R19 across the helper call, and the trampoline restores
// the caller's original R19 before returning to Go.
#define JIT_WORKSPACE 65536

TEXT ·callJIT(SB), 0, $131072-16
	NO_LOCAL_POINTERS
	MOVD R19, 8(RSP)
	MOVD R20, 16(RSP)
	MOVD R21, 24(RSP)
	MOVD R22, 32(RSP)
	MOVD R23, 40(RSP)
	MOVD R24, 48(RSP)
	MOVD R25, 56(RSP)
	MOVD R26, 64(RSP)

	MOVD regFileBase+8(FP), R20
	MOVD 600(R20), R15
	MOVD code+0(FP), R16
	MOVD $JIT_WORKSPACE, R11
	ADD R11, RSP, RSP
	BL (R16)
	MOVD $JIT_WORKSPACE, R11
	SUB R11, RSP, RSP

	MOVD 8(RSP), R19
	MOVD 16(RSP), R20
	MOVD 24(RSP), R21
	MOVD 32(RSP), R22
	MOVD 40(RSP), R23
	MOVD 48(RSP), R24
	MOVD 56(RSP), R25
	MOVD 64(RSP), R26
	RET
gocall:
	BL (R16)
	JMP (R19)

// func callJITImplAddr() uintptr
TEXT ·callJITImplAddr(SB), NOSPLIT, $0-8
	NO_LOCAL_POINTERS
	MOVD $·callJIT(SB), R0
	MOVD R0, ret+0(FP)
	RET
