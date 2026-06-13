//go:build darwin && arm64

#include "textflag.h"

TEXT ·libcPthreadJITWriteProtectNPTrampoline(SB),NOSPLIT,$0-0
	JMP	libc_pthread_jit_write_protect_np(SB)

TEXT ·pthreadJITWriteProtect(SB),NOSPLIT,$0-1
	MOVBU	enabled+0(FP), R0
	CALL	·libcPthreadJITWriteProtectNPTrampoline(SB)
	RET
