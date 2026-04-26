package riscv

/*
#include <sys/mman.h>
#include <stdint.h>

static void* stopper_alloc() {
	void* p = mmap(NULL, 4096, PROT_READ | PROT_WRITE,
	               MAP_PRIVATE | MAP_ANON, -1, 0);
	return (p == MAP_FAILED) ? NULL : p;
}

static void stopper_free(void* p) {
	munmap(p, 4096);
}

// stopper_arm makes the page unreadable. Any load from it will SIGSEGV.
static int stopper_arm(void* p) {
	return mprotect(p, 4096, PROT_NONE);
}

// stopper_disarm restores read/write access.
static int stopper_disarm(void* p) {
	return mprotect(p, 4096, PROT_READ | PROT_WRITE);
}
*/
import "C"

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
	p := C.stopper_alloc()
	if p == nil {
		return fmt.Errorf("stopper_alloc: mmap failed")
	}
	j.stopperPage = uintptr(p)
	return nil
}

// freeStopperPage releases the stopper page.
func (j *JIT) freeStopperPage() {
	if j.stopperPage != 0 {
		C.stopper_free(unsafe.Pointer(j.stopperPage))
		j.stopperPage = 0
	}
}

// RequestPreemption arms the stopper page (PROT_NONE). The next JIT
// backward branch will fault, causing RunJIT to return via recover.
// Safe to call from any goroutine.
func (j *JIT) RequestPreemption() {
	if j.stopperPage != 0 {
		C.stopper_arm(unsafe.Pointer(j.stopperPage))
	}
}

// ClearPreemption disarms the stopper page (PROT_READ|PROT_WRITE).
// Must be called before re-entering JIT after a preemption.
func (j *JIT) ClearPreemption() {
	if j.stopperPage != 0 {
		C.stopper_disarm(unsafe.Pointer(j.stopperPage))
	}
}

// StopperPageAddr returns the address of the stopper page for embedding
// as an immediate in generated native code.
func (j *JIT) StopperPageAddr() uintptr {
	return j.stopperPage
}
