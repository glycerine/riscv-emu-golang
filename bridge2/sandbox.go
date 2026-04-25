// Package sandbox provides a zero-CGo-per-call IPC bridge between Go and a
// permanently-parked C interpreter thread, communicating via a lock-free SPSC
// ring buffer backed by mmap.
//
// The CGo tax is paid exactly once, at Init() time. All subsequent
// dispatches use atomic operations on shared memory with no CGo involvement.
package bridge2

/*
#cgo CFLAGS:  -std=c11 -O2 -Wall -Wextra
#cgo linux   LDFLAGS: -lpthread

#include "sandbox.h"
*/
import "C"

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Exported C types so the test file can use them without importing "C" itself.
// (CGo types cannot cross package boundaries, so we wrap them in Go types.)

// Ring is the Go handle to a live spsc_ring_t.
type Ring struct {
	r   *C.spsc_ring_t
	cap uint64
}

// SandboxMem is the Go handle to the three guard-page-protected regions.
type SandboxMem struct {
	m C.sandbox_mem_t
}

// ----------------------------------------------------------------------------
// Lifecycle
// ----------------------------------------------------------------------------

// NewRing allocates a ring buffer via mmap and starts the C interpreter
// thread, paying the CGo tax exactly once.
func NewRing() *Ring {
	cr := C.ring_create()
	if cr == nil {
		panic("sandbox: ring_create failed")
	}
	ring := &Ring{r: cr, cap: uint64(cr.capacity)}

	go func() {
		runtime.LockOSThread() // pin OS thread; never returns to Go scheduler
		C.interpreter_thread_main(cr)
	}()

	return ring
}

// Destroy sends a shutdown command to the C thread, waits for it to exit,
// then releases the mmap'd ring. Do not use the Ring after this.
func (ring *Ring) Destroy() {
	idx, ok := ring.Push(0xFFFFFFFF, 0, 0, 0) // OPCODE_SHUTDOWN
	if ok {
		ring.WaitResult(idx)
	}
	C.ring_destroy(ring.r)
}

// NewSandboxMem allocates guard-page-protected code, data, and stack regions.
func NewSandboxMem(codeSz, dataSz, stackSz uintptr) *SandboxMem {
	m := &SandboxMem{}
	m.m = C.sandbox_mem_create(C.size_t(codeSz), C.size_t(dataSz), C.size_t(stackSz))
	return m
}

// SealCode flips the code region from PROT_READ|PROT_WRITE to
// PROT_READ|PROT_EXEC, enforcing W^X. Call after JIT compilation is done.
func (m *SandboxMem) SealCode() { C.sandbox_seal_code(&m.m) }

// Code returns a slice over the writable code region (before sealing).
func (m *SandboxMem) Code() []byte {
	return unsafe.Slice((*byte)(m.m.code), m.m.code_size)
}

// Data returns a slice over the data region.
func (m *SandboxMem) Data() []byte {
	return unsafe.Slice((*byte)(m.m.data), m.m.data_size)
}

// StackTop returns the initial RSP value (stack grows down from here).
func (m *SandboxMem) StackTop() uintptr { return uintptr(m.m.stack_top) }

// Destroy releases all mmap'd regions.
func (m *SandboxMem) Destroy() { C.sandbox_mem_destroy(&m.m) }

// ----------------------------------------------------------------------------
// Zero-CGo fast path — direct atomic ops into the shared mmap region
// ----------------------------------------------------------------------------

// cWorkItem mirrors work_item_t exactly (64 bytes, same field offsets).
// Used for unsafe pointer arithmetic into the ring's items array.
type cWorkItem struct {
	state  uint32
	opcode uint32
	arg0   uint64
	arg1   uint64
	arg2   uint64
	result uint64
	_pad   [24]byte
}

// Byte offsets into spsc_ring_t (must match sandbox.h):
//
//	head     @   0  (8 bytes)
//	_pad0    @   8  (56 bytes)
//	tail     @  64  (8 bytes)
//	sleeping @  72  (4 bytes)
//	_pad1    @  76  (52 bytes)
//	capacity @ 128  (4 bytes)
//	_pad2    @ 132  (60 bytes)
//	items    @ 192  (64 bytes each)
const (
	ringOffHead     = 0
	ringOffTail     = 64
	ringOffSleeping = 72
	ringOffItems    = 192
)

func (ring *Ring) headPtr() *uint64 {
	return (*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(ring.r)) + ringOffHead))
}

func (ring *Ring) tailPtr() *uint64 {
	return (*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(ring.r)) + ringOffTail))
}

func (ring *Ring) sleepingPtr() *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(ring.r)) + ringOffSleeping))
}

func (ring *Ring) slot(i uint64) *cWorkItem {
	base := uintptr(unsafe.Pointer(ring.r)) + ringOffItems
	return (*cWorkItem)(unsafe.Pointer(base + uintptr(i%ring.cap)*64))
}

// Push posts a work item onto the ring without CGo.
// Returns false if the ring is full — caller should back off and retry.
func (ring *Ring) Push(opcode uint32, arg0, arg1, arg2 uint64) (slotIdx uint64, ok bool) {
	hp := ring.headPtr()
	tp := ring.tailPtr()

	head := atomic.LoadUint64(hp)
	tail := atomic.LoadUint64(tp)
	if head-tail >= ring.cap {
		return 0, false
	}

	s := ring.slot(head)
	s.state = 0
	s.opcode = opcode
	s.arg0 = arg0
	s.arg1 = arg1
	s.arg2 = arg2

	atomic.StoreUint64(hp, head+1) // release — makes item visible to C thread

	if atomic.LoadUint32(ring.sleepingPtr()) != 0 {
		C.futex_wake((*C.uint32_t)(unsafe.Pointer(hp)))
	}

	return head, true
}

// WaitResult spins until the slot at slotIdx reports WORK_STATE_DONE,
// then returns the result.
func (ring *Ring) WaitResult(slotIdx uint64) uint64 {
	s := ring.slot(slotIdx)
	for atomic.LoadUint32(&s.state) != 2 {} // WORK_STATE_DONE
	return s.result
}

// ClearSlot resets a slot's state to EMPTY before reuse.
// Must be called before Push() if the slot has been used before,
// to prevent WaitResult() from seeing a stale DONE from a prior round.
func (ring *Ring) ClearSlot(slotIdx uint64) {
	ring.slot(slotIdx).state = 0
}

// CgoAdd calls a trivial C function through CGO, for benchmarking CGO overhead.
func CgoAdd(a, b uint64) uint64 {
	return uint64(C.cgo_add(C.uint64_t(a), C.uint64_t(b)))
}
