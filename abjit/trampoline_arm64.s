#include "funcdata.h"
#include "textflag.h"

// func callJIT(code, regFileBase uintptr)
//
// ARM64 abjit convention used by the conservative lowerer:
//   R20 = abjit.State / register-file base
//   R15 = remaining guest-instruction budget, loaded from State.IC
//
// Generated code exits by jumping back to the instruction after BLR. The
// assembler-generated ARM64 prologue stores LR at 0(SP), so our save area
// starts at 8(SP) and leaves LR for the generated epilogue to restore.
TEXT ·callJIT(SB), NOSPLIT, $80-16
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
	BL (R16)

	MOVD 8(RSP), R19
	MOVD 16(RSP), R20
	MOVD 24(RSP), R21
	MOVD 32(RSP), R22
	MOVD 40(RSP), R23
	MOVD 48(RSP), R24
	MOVD 56(RSP), R25
	MOVD 64(RSP), R26
	RET

// func callJITImplAddr() uintptr
TEXT ·callJITImplAddr(SB), NOSPLIT, $0-8
	NO_LOCAL_POINTERS
	MOVD $·callJIT(SB), R0
	MOVD R0, ret+0(FP)
	RET
