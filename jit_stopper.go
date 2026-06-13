package riscv

import (
	"fmt"
	"unsafe"
)

// initStopperPage allocates the InfiniteLoopStopperPage — a single 4KB
// mmap'd page used to preempt JIT-compiled infinite loops. At each
// backward branch, the JIT emits a TESTQ that reads from this page.
// Normally it's PROT_READ|PROT_WRITE and the read is an L1 cache hit
// (~1 cycle). To preempt, call RequestPreemption which mprotects to
// PROT_NONE — the next backward branch faults, and RunJIT's
// defer/recover catches the panic.
func (j *JIT) initStopperPage() error {
	p, err := guestAlloc(GuestPageSize)
	if err != nil {
		return fmt.Errorf("stopper mmap: %w", err)
	}
	j.stopperPage = uintptr(p)
	return nil
}

// freeStopperPage releases the stopper page.
func (j *JIT) freeStopperPage() {
	if j.stopperPage != 0 {
		_ = guestFree(unsafe.Pointer(j.stopperPage), GuestPageSize)
		j.stopperPage = 0
	}
}

// RequestPreemption arms the stopper page (PROT_NONE). The next JIT
// backward branch will fault, causing RunJIT to return via recover.
// Safe to call from any goroutine.
func (j *JIT) RequestPreemption() {
	if j.stopperPage != 0 {
		_ = guestGuard(unsafe.Pointer(j.stopperPage), GuestPageSize)
	}
}

// ClearPreemption disarms the stopper page (PROT_READ|PROT_WRITE).
// Must be called before re-entering JIT after a preemption.
func (j *JIT) ClearPreemption() {
	if j.stopperPage != 0 {
		_ = guestUnguard(unsafe.Pointer(j.stopperPage), GuestPageSize)
	}
}

// StopperPageAddr returns the address of the stopper page for embedding
// as an immediate in generated native code.
func (j *JIT) StopperPageAddr() uintptr {
	return j.stopperPage
}
