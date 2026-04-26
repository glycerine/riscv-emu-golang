#include "funcdata.h"
#include "textflag.h"

// func callJIT(code, regFileBase uintptr)
//
// Frame layout (after Go's prologue):
//   SP+0      resume address slot (JIT writes here for gocall)
//   SP+8      saved RBX
//   SP+16     saved RBP (Go frame pointer — not explicitly restored)
//   SP+24     saved R12
//   SP+32     saved R13
//   SP+40     saved R15
//   SP+48..   available stack for Go callbacks (~65KB)
//
TEXT ·callJIT(SB), 0, $65528-16
	NO_LOCAL_POINTERS
	MOVQ BX,  8(SP)
	MOVQ BP,  16(SP)
	MOVQ R12, 24(SP)
	MOVQ R13, 32(SP)
	MOVQ R15, 40(SP)

	MOVQ regFileBase+8(FP), BP
	MOVQ code+0(FP), AX
	JMP AX
gocall:
	CALL R10
	JMP (SP)

// func callJITImplAddr() uintptr
TEXT ·callJITImplAddr(SB), 0, $0-8
	NO_LOCAL_POINTERS
	MOVQ $·callJIT(SB), AX
	MOVQ AX, ret+0(FP)
	RET
