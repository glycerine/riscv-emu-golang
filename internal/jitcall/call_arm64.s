#include "textflag.h"

// func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
//           memBase uintptr, memMask uint64) Result
//
// ARM64 JIT ABI:
//   R0 = sret buffer
//   R1 = x register file
//   R2 = f register file
//   R3 = fcsr pointer
//   R4 = memBase
//   R5 = memMask
TEXT ·Call(SB), $65536-80
	MOVD R19, 32(RSP)
	MOVD R20, 40(RSP)
	MOVD R21, 48(RSP)
	MOVD R22, 56(RSP)
	MOVD R23, 64(RSP)
	MOVD R24, 72(RSP)
	MOVD R30, 80(RSP)

	MOVD $0, 0(RSP)
	MOVD $0, 8(RSP)
	MOVD $0, 16(RSP)
	MOVD $0, 24(RSP)

	MOVD $0(RSP), R0
	MOVD x+8(FP), R1
	MOVD f+16(FP), R2
	MOVD fcsr+24(FP), R3
	MOVD memBase+32(FP), R4
	MOVD memMask+40(FP), R5

	MOVD fn+0(FP), R16
	BL (R16)

	MOVD 0(RSP), R6
	MOVD 8(RSP), R7
	MOVD 16(RSP), R8
	MOVD 24(RSP), R9
	MOVD R6, ret_PC+48(FP)
	MOVD R7, ret_Status+56(FP)
	MOVD R8, ret_FaultAddr+64(FP)
	MOVD R9, ret_Cycles+72(FP)

	MOVD 32(RSP), R19
	MOVD 40(RSP), R20
	MOVD 48(RSP), R21
	MOVD 56(RSP), R22
	MOVD 64(RSP), R23
	MOVD 72(RSP), R24
	MOVD 80(RSP), R30
	RET

// func CallAOT(fn, x, f, fcsr, memBase, memMask, dcBase, dcMask, vBegin, segSize) Result
//
// Same as Call, with decoder-cache metadata published at sret[32..56].
TEXT ·CallAOT(SB), $65536-112
	MOVD R19, 64(RSP)
	MOVD R20, 72(RSP)
	MOVD R21, 80(RSP)
	MOVD R22, 88(RSP)
	MOVD R23, 96(RSP)
	MOVD R24, 104(RSP)
	MOVD R30, 112(RSP)

	MOVD $0, 0(RSP)
	MOVD $0, 8(RSP)
	MOVD $0, 16(RSP)
	MOVD $0, 24(RSP)
	MOVD dcBase+48(FP), R6
	MOVD R6, 32(RSP)
	MOVD dcMask+56(FP), R6
	MOVD R6, 40(RSP)
	MOVD vBegin+64(FP), R6
	MOVD R6, 48(RSP)
	MOVD segSize+72(FP), R6
	MOVD R6, 56(RSP)

	MOVD $0(RSP), R0
	MOVD x+8(FP), R1
	MOVD f+16(FP), R2
	MOVD fcsr+24(FP), R3
	MOVD memBase+32(FP), R4
	MOVD memMask+40(FP), R5

	MOVD fn+0(FP), R16
	BL (R16)

	MOVD 0(RSP), R6
	MOVD 8(RSP), R7
	MOVD 16(RSP), R8
	MOVD 24(RSP), R9
	MOVD R6, ret_PC+80(FP)
	MOVD R7, ret_Status+88(FP)
	MOVD R8, ret_FaultAddr+96(FP)
	MOVD R9, ret_Cycles+104(FP)

	MOVD 64(RSP), R19
	MOVD 72(RSP), R20
	MOVD 80(RSP), R21
	MOVD 88(RSP), R22
	MOVD 96(RSP), R23
	MOVD 104(RSP), R24
	MOVD 112(RSP), R30
	RET
