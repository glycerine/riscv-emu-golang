#include "textflag.h"

// func SwitchStack(newSP uintptr) (oldSP uintptr)
//
// Switches RSP to newSP and returns the previous RSP value.
// Caller is responsible for updating g.stack bounds before calling.
//
// Flags: NOSPLIT — must not trigger stack growth check (we're mid-switch).
//        NOFRAME — we manage the frame ourselves; don't save BP.
//
// ABI: Go register ABI (1.17+)
//   args:    AX = newSP
//   returns: AX = oldSP
TEXT ·SwitchStack(SB), NOSPLIT|NOFRAME, $0-16
    MOVQ    newSP+0(FP), AX     // AX = newSP (argument)
    MOVQ    SP, BX              // BX = current RSP (save it)
    MOVQ    AX, SP              // RSP = newSP  ← the switch
    MOVQ    BX, ret+8(FP)       // return old RSP
    RET

// func GetG() uintptr
//
// Returns the address of the current goroutine's g struct.
// On amd64 Go 1.17+, g is kept in R14.
TEXT ·GetG(SB), NOSPLIT|NOFRAME, $0-8
    MOVQ    R14, ret+0(FP)
    RET

// func CPURelax()
//
// Emits the appropriate spin-wait hint for the current architecture.
// On x86: PAUSE.  Prevents memory order violations in spin loops and
// reduces power consumption.
TEXT ·CPURelax(SB), NOSPLIT|NOFRAME, $0-0
    PAUSE
    RET

// func StoreFenceRelease()
//
// Full store-release fence. Used after writing ring buffer slots
// before bumping the head pointer.
TEXT ·StoreFenceRelease(SB), NOSPLIT|NOFRAME, $0-0
    // On x86, all stores are already TSO-ordered; MFENCE is only needed
    // for store-load reordering, not store-store. SFENCE is sufficient here.
    SFENCE
    RET

// func LoadFenceAcquire()
//
// Full load-acquire fence. Used when polling the ring tail/state.
TEXT ·LoadFenceAcquire(SB), NOSPLIT|NOFRAME, $0-0
    // On x86, loads are not reordered with other loads (TSO), so this
    // is effectively a compiler barrier only. Still correct and cheap.
    LFENCE
    RET
