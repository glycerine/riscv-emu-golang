#include "funcdata.h"
#include "textflag.h"

// func callJIT(code, regFileBase uintptr)
//
// ARM64 abjit convention used by the conservative lowerer:
//   R20 = abjit.State / register-file base
//
// Generated code returns with RET. The trampoline uses BL so LR points
// back here, then restores the callee-saves it touched and returns to Go.
TEXT ·callJIT(SB), NOSPLIT, $48-16
	NO_LOCAL_POINTERS
	MOVD R20, 0(RSP)
	MOVD R21, 8(RSP)
	MOVD R22, 16(RSP)
	MOVD R23, 24(RSP)
	MOVD R30, 32(RSP)

	MOVD regFileBase+8(FP), R20
	MOVD code+0(FP), R16
	BL (R16)

	MOVD 0(RSP), R20
	MOVD 8(RSP), R21
	MOVD 16(RSP), R22
	MOVD 24(RSP), R23
	MOVD 32(RSP), R30
	RET

// func callJITImplAddr() uintptr
TEXT ·callJITImplAddr(SB), NOSPLIT, $0-8
	NO_LOCAL_POINTERS
	MOVD $·callJIT(SB), R0
	MOVD R0, ret+0(FP)
	RET
