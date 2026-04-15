// ops_amd64.s — Float arithmetic with flag clear + immediate MXCSR capture.
//
// Each function:
//   1. Clears MXCSR exception status bits (so only THIS op's flags are seen)
//   2. Executes the SSE instruction
//   3. Reads MXCSR immediately (red zone: -4(SP)) before any function call
//   4. Clears the flags again and permutes to RISC-V fflags
//
// NOSPLIT|NOFRAME: no prologue/epilogue, so FP offsets match what the
// ABI0 wrapper expects (verified via objdump: caller places args at
// 0(SP)..N(SP) before CALL; inside abi0 those are at 8(SP)..N+8(SP)
// which corresponds to 0(FP)..N(FP) with NOFRAME).
//
// RISC-V fflags from MXCSR:
//   MXCSR bit5=PE(inexact)   → rv NX bit0
//   MXCSR bit4=UE(underflow) → rv UF bit1
//   MXCSR bit3=OE(overflow)  → rv OF bit2
//   MXCSR bit2=ZE(divzero)   → rv DZ bit3
//   MXCSR bit0=IE(invalid)   → rv NV bit4

#include "textflag.h"

// CLEAR_FLAGS: clear MXCSR exception bits before the operation.
#define CLEAR_FLAGS \
    STMXCSR -4(SP)              \
    MOVL    -4(SP), AX          \
    ANDL    $0xFFFFFFC0, AX     \
    MOVL    AX, -4(SP)          \
    LDMXCSR -4(SP)

// CAPTURE_FLAGS: read MXCSR after the operation, clear, permute → CX.
// Must follow immediately after the SSE instruction (no intervening calls).
#define CAPTURE_FLAGS \
    STMXCSR -4(SP)              \
    MOVL    -4(SP), AX          \
    MOVL    AX, BX              \
    ANDL    $0xFFFFFFC0, BX     \
    MOVL    BX, -4(SP)          \
    LDMXCSR -4(SP)              \
    MOVL    AX, CX              \
    SHRL    $5, CX; ANDL $1, CX                             \
    MOVL    AX, DX; SHRL $3, DX; ANDL $2, DX; ORL DX, CX  \
    MOVL    AX, DX; SHRL $1, DX; ANDL $4, DX; ORL DX, CX  \
    MOVL    AX, DX; SHLL $1, DX; ANDL $8, DX; ORL DX, CX  \
    MOVL    AX, DX; SHLL $4, DX; ANDL $16, DX; ORL DX, CX

// ── FFlags / ClearFFlags ──────────────────────────────────────────────────

TEXT ·FFlags(SB),NOSPLIT|NOFRAME,$0-4
    CAPTURE_FLAGS
    MOVL CX, ret+0(FP)
    RET

TEXT ·ClearFFlags(SB),NOSPLIT|NOFRAME,$0-0
    CLEAR_FLAGS
    RET

// ── float32 arithmetic ────────────────────────────────────────────────────
// Arg layout (ABI0, NOFRAME): a+0(FP), b+4(FP), ret+8(FP), flags+12(FP)

TEXT ·AddF32(SB),NOSPLIT|NOFRAME,$0-16
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    ADDSS X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+8(FP)
    MOVL    CX, flags+12(FP)
    RET

TEXT ·SubF32(SB),NOSPLIT|NOFRAME,$0-16
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    SUBSS X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+8(FP)
    MOVL    CX, flags+12(FP)
    RET

TEXT ·MulF32(SB),NOSPLIT|NOFRAME,$0-16
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    MULSS X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+8(FP)
    MOVL    CX, flags+12(FP)
    RET

TEXT ·DivF32(SB),NOSPLIT|NOFRAME,$0-16
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    DIVSS X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+8(FP)
    MOVL    CX, flags+12(FP)
    RET

TEXT ·SqrtF32(SB),NOSPLIT|NOFRAME,$0-16
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    SQRTSS X0, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+8(FP)
    MOVL    CX, flags+12(FP)
    RET

// ── float64 arithmetic ────────────────────────────────────────────────────
// a+0(FP), b+8(FP), ret+16(FP), flags+24(FP)

TEXT ·AddF64(SB),NOSPLIT|NOFRAME,$0-28
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    ADDSD X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+16(FP)
    MOVL    CX, flags+24(FP)
    RET

TEXT ·SubF64(SB),NOSPLIT|NOFRAME,$0-28
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    SUBSD X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+16(FP)
    MOVL    CX, flags+24(FP)
    RET

TEXT ·MulF64(SB),NOSPLIT|NOFRAME,$0-28
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    MULSD X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+16(FP)
    MOVL    CX, flags+24(FP)
    RET

TEXT ·DivF64(SB),NOSPLIT|NOFRAME,$0-28
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    DIVSD X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+16(FP)
    MOVL    CX, flags+24(FP)
    RET

TEXT ·SqrtF64(SB),NOSPLIT|NOFRAME,$0-20
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    SQRTSD X0, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+8(FP)
    MOVL    CX, flags+16(FP)
    RET

// ── fused multiply-add (AVX FMA, available on Haswell+) ──────────────────
// VFMADD213SS X2, X1, X0: X0 = X0*X1 + X2
// a+0(FP), b+4(FP), c+8(FP), ret+12(FP), flags+16(FP)

TEXT ·MAddF32(SB),NOSPLIT|NOFRAME,$0-24
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    MOVSS c+8(FP), X2
    VFMADD213SS X2, X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+16(FP)
    MOVL    CX, flags+20(FP)
    RET

TEXT ·MSubF32(SB),NOSPLIT|NOFRAME,$0-24
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    MOVSS c+8(FP), X2
    VFMSUB213SS X2, X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+16(FP)
    MOVL    CX, flags+20(FP)
    RET

TEXT ·NMAddF32(SB),NOSPLIT|NOFRAME,$0-24
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    MOVSS c+8(FP), X2
    VFNMADD213SS X2, X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+16(FP)
    MOVL    CX, flags+20(FP)
    RET

TEXT ·NMSubF32(SB),NOSPLIT|NOFRAME,$0-24
    CLEAR_FLAGS
    MOVSS a+0(FP), X0
    MOVSS b+4(FP), X1
    MOVSS c+8(FP), X2
    VFNMSUB213SS X2, X1, X0
    CAPTURE_FLAGS
    MOVSS   X0, ret+16(FP)
    MOVL    CX, flags+20(FP)
    RET

// a+0(FP), b+8(FP), c+16(FP), ret+24(FP), flags+32(FP)

TEXT ·MAddF64(SB),NOSPLIT|NOFRAME,$0-36
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    MOVSD c+16(FP), X2
    VFMADD213SD X2, X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+24(FP)
    MOVL    CX, flags+32(FP)
    RET

TEXT ·MSubF64(SB),NOSPLIT|NOFRAME,$0-36
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    MOVSD c+16(FP), X2
    VFMSUB213SD X2, X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+24(FP)
    MOVL    CX, flags+32(FP)
    RET

TEXT ·NMAddF64(SB),NOSPLIT|NOFRAME,$0-36
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    MOVSD c+16(FP), X2
    VFNMADD213SD X2, X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+24(FP)
    MOVL    CX, flags+32(FP)
    RET

TEXT ·NMSubF64(SB),NOSPLIT|NOFRAME,$0-36
    CLEAR_FLAGS
    MOVSD a+0(FP), X0
    MOVSD b+8(FP), X1
    MOVSD c+16(FP), X2
    VFNMSUB213SD X2, X1, X0
    CAPTURE_FLAGS
    MOVSD   X0, ret+24(FP)
    MOVL    CX, flags+32(FP)
    RET
