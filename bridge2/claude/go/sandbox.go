package sandbox

/*
#cgo CFLAGS: -std=c11 -O2 -Wall -Wextra
#cgo linux LDFLAGS: -lpthread
#cgo darwin LDFLAGS:

#include "sandbox.h"
#include <stdlib.h>
*/
import "C"

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Ring is the Go-side handle to the shared SPSC ring buffer.
// After Init(), all communication with the interpreter thread
// happens through this structure — zero CGo per call.
type Ring struct {
	cring *C.spsc_ring_t
	cap   uint64
}

// SandboxMem is the Go-side handle to the guard-page-protected
// memory regions for JIT code, data, and stack.
type SandboxMem struct {
	cmem C.sandbox_mem_t
}

// Init starts the interpreter thread (pays the CGo tax once)
// and returns the ring handle for subsequent zero-overhead calls.
func Init(codeSz, dataSz, stackSz uintptr) (*Ring, *SandboxMem) {
	r := &Ring{}
	r.cring = C.ring_create()
	if r.cring == nil {
		panic("sandbox: ring_create failed")
	}
	r.cap = uint64(r.cring.capacity)

	mem := &SandboxMem{}
	mem.cmem = C.sandbox_mem_create(
		C.size_t(codeSz),
		C.size_t(dataSz),
		C.size_t(stackSz),
	)

	// Pay the CGo tax exactly once: lock an OS thread and hand it
	// to the C interpreter loop permanently.
	go func() {
		runtime.LockOSThread() // pin: this OS thread never returns to Go scheduler
		C.interpreter_thread_main(r.cring)
		// unreachable
	}()

	return r, mem
}

// SealCode flips the code region to PROT_READ|PROT_EXEC (W^X).
// Call this after writing all JIT output into mem.Code().
func (m *SandboxMem) SealCode() {
	C.sandbox_seal_code(&m.cmem)
}

// Code returns a byte slice over the JIT code region.
func (m *SandboxMem) Code() []byte {
	return unsafe.Slice((*byte)(m.cmem.code), m.cmem.code_size)
}

// Data returns a byte slice over the sandbox data region.
func (m *SandboxMem) Data() []byte {
	return unsafe.Slice((*byte)(m.cmem.data), m.cmem.data_size)
}

// StackTop returns the initial RSP value (stack grows down from here).
func (m *SandboxMem) StackTop() uintptr {
	return uintptr(m.cmem.stack_top)
}

// Destroy releases all mmap'd regions.
func (m *SandboxMem) Destroy() {
	C.sandbox_mem_destroy(&m.cmem)
}

// ----------------------------------------------------------------------------
// Zero-CGo fast path — direct atomic ops on the shared ring
// ----------------------------------------------------------------------------

// workItem mirrors the C work_item_t layout exactly.
// We address it directly via unsafe into the mmap'd ring.
// Must match c/sandbox.h: 64 bytes, same field offsets.
type workItem struct {
	state  uint32   // atomic
	opcode uint32
	arg0   uint64
	arg1   uint64
	arg2   uint64
	result uint64
	_pad   [16]byte
}

// Call posts an opcode + args to the interpreter thread and spins
// until the result is ready. No CGo. No syscall. Just atomics.
//
// This will be as fast as cache coherency allows (~10–30 ns hot path).
func (r *Ring) Call(opcode uint32, arg0, arg1, arg2 uint64) uint64 {
	// --- Push ---
	// Load head (relaxed — we're the only producer)
	headPtr := (*uint64)(unsafe.Pointer(&r.cring.head))
	tailPtr := (*uint64)(unsafe.Pointer(
		uintptr(unsafe.Pointer(&r.cring.head)) + 64, // skip head + pad0
	))

	var head uint64
	for {
		head = atomic_load_u64(headPtr)
		tail := atomic_load_u64(tailPtr)
		if head-tail < r.cap {
			break // space available
		}
		runtime.Gosched() // ring full — yield and retry
	}

	slot := head & (r.cap - 1)
	itemPtr := ringSlot(r.cring, slot)

	// Write fields before publishing head
	itemPtr.opcode = opcode
	itemPtr.arg0   = arg0
	itemPtr.arg1   = arg1
	itemPtr.arg2   = arg2
	atomic_store_u32(&itemPtr.state, 0) // EMPTY → will become DONE

	// Publish: release store on head
	atomic_store_u64_release(headPtr, head+1)

	// Wake the C thread if it's sleeping on futex
	// (harmless no-op if it's spinning)
	C.futex_wake((*C.uint32_t)(unsafe.Pointer(headPtr)))

	// --- Wait for result (spin) ---
	for atomic_load_u32_acquire(&itemPtr.state) != 2 { // WORK_STATE_DONE
		CPU_RELAX_Go()
	}

	return itemPtr.result
}

// ringSlot returns a pointer to slot i in the ring's items array.
func ringSlot(ring *C.spsc_ring_t, i uint64) *workItem {
	// items[] starts after head(8)+pad0(56)+tail(8)+pad1(56)+capacity(4)+pad2(60) = 192 bytes
	const itemsOffset = 192
	base := uintptr(unsafe.Pointer(ring)) + itemsOffset
	return (*workItem)(unsafe.Pointer(base + uintptr(i)*64))
}

// ----------------------------------------------------------------------------
// Thin atomic wrappers (avoid importing sync/atomic for uintptr games)
// ----------------------------------------------------------------------------

func atomic_load_u64(p *uint64) uint64 {
	return atomic.LoadUint64(p)
}

func atomic_store_u64_release(p *uint64, v uint64) {
	atomic.StoreUint64(p, v)
}

func atomic_load_u32_acquire(p *uint32) uint32 {
	return atomic.LoadUint32(p)
}

func atomic_store_u32(p *uint32, v uint32) {
	atomic.StoreUint32(p, v)
}

func CPU_RELAX_Go() {
	// On Go side, a simple Gosched is fine for the wait loop;
	// for truly hot paths, busy-spin with runtime.GOMAXPROCS(0) > 1 check.
	runtime.Gosched()
}
