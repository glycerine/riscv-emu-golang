#include "textflag.h"

// func SwitchStack(newSP uintptr) (oldSP uintptr)
//
// ARM64 version. Go register ABI:
//   R0 = newSP (arg)
//   R0 = oldSP (return)
//
// Note: on aarch64, SP must always be 16-byte aligned at any instruction
// that accesses memory. Ensure your sandbox stack is 16-byte aligned at top.
TEXT ·SwitchStack(SB), NOSPLIT|NOFRAME, $0-16
    MOVD    newSP+0(FP), R0     // R0 = newSP
    MOVD    RSP, R1             // R1 = current SP
    MOVD    R0, RSP             // SP = newSP  ← the switch
    MOVD    R1, ret+8(FP)       // return old SP
    RET

// func GetG() uintptr
//
// On arm64, Go keeps g in R28 (dedicated g register).
TEXT ·GetG(SB), NOSPLIT|NOFRAME, $0-8
    MOVD    R28, ret+0(FP)
    RET

// func CPURelax()
//
// ARM64 spin-wait hint. YIELD tells the CPU this is a spin loop,
// allowing the core to prioritize the other hardware thread (SMT)
// or reduce power without affecting correctness.
TEXT ·CPURelax(SB), NOSPLIT|NOFRAME, $0-0
    YIELD
    RET

// func StoreFenceRelease()
//
// On ARM64, stores CAN be reordered. We need a store-release barrier
// before publishing the ring head pointer.
// DMB ISHST = Data Memory Barrier, Inner Shareable, Store-before-Store.
TEXT ·StoreFenceRelease(SB), NOSPLIT|NOFRAME, $0-0
    DMB $0x9    // ISHST
    RET

// func LoadFenceAcquire()
//
// Load-acquire barrier. Prevents loads from being speculated past this point.
// DMB ISHLD = Data Memory Barrier, Inner Shareable, Load-before-Load/Store.
TEXT ·LoadFenceAcquire(SB), NOSPLIT|NOFRAME, $0-0
    DMB $0xa    // ISHLD
    RET
