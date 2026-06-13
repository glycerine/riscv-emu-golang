//go:build arm64 && !cgo

#include "textflag.h"

// func icacheFlush(start, end unsafe.Pointer)
TEXT ·icacheFlush(SB),NOSPLIT|NOFRAME,$0-16
	MOVD	start+0(FP), R0
	MOVD	end+8(FP), R1
	CMP	R1, R0
	BHS	done

	BIC	$15, R0, R2
dclean:
	DC	CVAU, R2
	ADD	$16, R2, R2
	CMP	R1, R2
	BCC	dclean

	DSB	$0xb

	BIC	$15, R0, R2
iclean:
	WORD	$0xd50b7522 // IC IVAU, R2
	ADD	$16, R2, R2
	CMP	R1, R2
	BCC	iclean

	DSB	$0xb
	ISB	$0xf
done:
	RET
