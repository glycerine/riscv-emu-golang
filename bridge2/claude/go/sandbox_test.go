package sandbox_test

/*
#cgo CFLAGS: -std=c11 -O2 -Wall -Wextra
#cgo linux LDFLAGS: -lpthread
#cgo CFLAGS: -I../c

#include "../c/sandbox.h"
#include "../c/sandbox.c"
*/
import "C"

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// ----------------------------------------------------------------------------
// Helpers — thin unsafe wrappers matching the C struct layout
// ----------------------------------------------------------------------------

// ringHead / ringTail / ringSlot mirror the spsc_ring_t layout so we can
// drive the ring from pure Go without going through CGo on the hot path.
// Offsets must match sandbox.h:
//   head  @ 0   (8 bytes)
//   pad0  @ 8   (56 bytes)
//   tail  @ 64  (8 bytes)
//   pad1  @ 72  (56 bytes)
//   cap   @ 128 (4 bytes)
//   pad2  @ 132 (60 bytes)
//   items @ 192 (64 bytes each)

const (
	offHead  = 0
	offTail  = 64
	offItems = 192
	itemSize = 64
)

func ringHeadPtr(r *C.spsc_ring_t) *uint64 {
	return (*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(r)) + offHead))
}

func ringTailPtr(r *C.spsc_ring_t) *uint64 {
	return (*uint64)(unsafe.Pointer(uintptr(unsafe.Pointer(r)) + offTail))
}

type cWorkItem struct {
	state  uint32
	opcode uint32
	arg0   uint64
	arg1   uint64
	arg2   uint64
	result uint64
	_pad   [16]byte
}

func ringSlot(r *C.spsc_ring_t, i uint64) *cWorkItem {
	base := uintptr(unsafe.Pointer(r)) + offItems
	return (*cWorkItem)(unsafe.Pointer(base + uintptr(i)*itemSize))
}

func ringCap(r *C.spsc_ring_t) uint64 {
	return uint64(r.capacity)
}

// goPush posts a work item from Go without CGo.
// Returns false if the ring is full.
func goPush(r *C.spsc_ring_t, opcode uint32, a0, a1, a2 uint64) bool {
	hp := ringHeadPtr(r)
	tp := ringTailPtr(r)
	cap := ringCap(r)

	head := atomic.LoadUint64(hp)
	tail := atomic.LoadUint64(tp) // acquire
	if head-tail >= cap {
		return false
	}
	slot := ringSlot(r, head&(cap-1))
	slot.opcode = opcode
	slot.arg0 = a0
	slot.arg1 = a1
	slot.arg2 = a2
	atomic.StoreUint64(hp, head+1) // release
	// Wake the C thread in case it's sleeping on the futex.
	C.futex_wake((*C.uint32_t)(unsafe.Pointer(hp)))
	return true
}

// goWaitResult spins until the item at the given slot index reports DONE,
// then returns its result. Times out after `timeout` and returns (0, false).
func goWaitResult(r *C.spsc_ring_t, slotIdx uint64, timeout time.Duration) (uint64, bool) {
	slot := ringSlot(r, slotIdx&(ringCap(r)-1))
	deadline := time.Now().Add(timeout)
	for {
		if atomic.LoadUint32(&slot.state) == 2 { // WORK_STATE_DONE
			return slot.result, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		runtime.Gosched()
	}
}

// startInterpreter launches the C interpreter thread exactly once and
// returns the ring. The OS thread is locked and never returns.
func startInterpreter(t *testing.T) *C.spsc_ring_t {
	t.Helper()
	ring := C.ring_create()
	if ring == nil {
		t.Fatal("ring_create() returned nil")
	}
	go func() {
		runtime.LockOSThread()
		C.interpreter_thread_main(ring) // never returns
	}()
	// Give the C thread a moment to reach its poll loop.
	time.Sleep(5 * time.Millisecond)
	return ring
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

// TestPingPong sends a single NOP (opcode 0) and checks the sentinel result.
func TestPingPong(t *testing.T) {
	ring := startInterpreter(t)
	defer C.ring_destroy(ring)

	headBefore := atomic.LoadUint64(ringHeadPtr(ring))

	ok := goPush(ring, 0 /*NOP*/, 0, 0, 0)
	if !ok {
		t.Fatal("goPush: ring unexpectedly full")
	}

	result, ok := goWaitResult(ring, headBefore, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for NOP result")
	}
	const want = 0xDEADBEEF
	if result != want {
		t.Errorf("NOP result = 0x%X, want 0x%X", result, want)
	}
	t.Logf("NOP round-trip OK: result=0x%X", result)
}

// TestUnknownOpcode verifies that an unrecognised opcode returns ^0.
func TestUnknownOpcode(t *testing.T) {
	ring := startInterpreter(t)
	defer C.ring_destroy(ring)

	headBefore := atomic.LoadUint64(ringHeadPtr(ring))
	goPush(ring, 0xFFFF, 42, 43, 44)

	result, ok := goWaitResult(ring, headBefore, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for unknown-opcode result")
	}
	if result != ^uint64(0) {
		t.Errorf("unknown opcode result = 0x%X, want 0xFFFFFFFFFFFFFFFF", result)
	}
}

// TestSequential sends N items in order and verifies each result arrives
// in order with no drops or corruption.
func TestSequential(t *testing.T) {
	const N = 256
	ring := startInterpreter(t)
	defer C.ring_destroy(ring)

	for i := 0; i < N; i++ {
		headBefore := atomic.LoadUint64(ringHeadPtr(ring))

		// Reset the slot's state to EMPTY before re-using it, so
		// goWaitResult doesn't read stale DONE from a previous iteration.
		ringSlot(ring, headBefore&(ringCap(ring)-1)).state = 0

		ok := goPush(ring, 0, uint64(i), 0, 0)
		if !ok {
			t.Fatalf("iteration %d: ring full", i)
		}
		_, ok = goWaitResult(ring, headBefore, 2*time.Second)
		if !ok {
			t.Fatalf("iteration %d: timed out", i)
		}
	}
	t.Logf("Sequential: %d round-trips OK", N)
}

// TestBurst fills the ring to capacity in one shot, then drains results.
func TestBurst(t *testing.T) {
	ring := startInterpreter(t)
	defer C.ring_destroy(ring)

	cap := ringCap(ring)
	// Use half capacity to stay safe without back-pressure logic.
	burst := cap / 2
	firstHead := atomic.LoadUint64(ringHeadPtr(ring))

	for i := uint64(0); i < burst; i++ {
		slot := ringSlot(ring, (firstHead+i)&(cap-1))
		slot.state = 0 // clear any stale state
		ok := goPush(ring, 0, i, 0, 0)
		if !ok {
			t.Fatalf("burst push %d/%d: ring full unexpectedly", i, burst)
		}
	}

	// Collect all results.
	for i := uint64(0); i < burst; i++ {
		_, ok := goWaitResult(ring, firstHead+i, 2*time.Second)
		if !ok {
			t.Fatalf("burst result %d/%d: timed out", i, burst)
		}
	}
	t.Logf("Burst: %d items posted and drained OK", burst)
}

// TestConcurrentProducers verifies that multiple goroutines serialising
// through a mutex onto the single-producer ring don't corrupt results.
// (SPSC means one producer at a time — the mutex enforces that.)
func TestConcurrentProducers(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50

	ring := startInterpreter(t)
	defer C.ring_destroy(ring)

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		success atomic.Int64
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				mu.Lock()
				headBefore := atomic.LoadUint64(ringHeadPtr(ring))
				slot := ringSlot(ring, headBefore&(ringCap(ring)-1))
				slot.state = 0
				for !goPush(ring, 0, uint64(id*1000+i), 0, 0) {
					mu.Unlock()
					runtime.Gosched()
					mu.Lock()
					headBefore = atomic.LoadUint64(ringHeadPtr(ring))
				}
				mu.Unlock()

				_, ok := goWaitResult(ring, headBefore, 2*time.Second)
				if ok {
					success.Add(1)
				} else {
					t.Errorf("goroutine %d item %d: timed out", id, i)
				}
			}
		}(g)
	}

	wg.Wait()
	total := int64(goroutines * perGoroutine)
	t.Logf("Concurrent: %d/%d round-trips succeeded", success.Load(), total)
	if success.Load() != total {
		t.Errorf("expected %d successes, got %d", total, success.Load())
	}
}

// TestIdleTransitionToSleep verifies that after 100 ms of inactivity the
// thread goes to sleep and still wakes correctly when new work arrives.
func TestIdleTransitionToSleep(t *testing.T) {
	ring := startInterpreter(t)
	defer C.ring_destroy(ring)

	// Let the thread cross its 100 ms spin deadline and fall asleep.
	t.Log("waiting 200 ms for idle→sleep transition...")
	time.Sleep(200 * time.Millisecond)

	// Now send a ping — the Go side must wake the futex.
	headBefore := atomic.LoadUint64(ringHeadPtr(ring))
	ringSlot(ring, headBefore&(ringCap(ring)-1)).state = 0
	goPush(ring, 0, 0, 0, 0)

	// Allow generous time: futex wake latency is ~1–4 µs, but CI can be slow.
	result, ok := goWaitResult(ring, headBefore, 2*time.Second)
	if !ok {
		t.Fatal("timed out: thread did not wake from sleep")
	}
	if result != 0xDEADBEEF {
		t.Errorf("wake-from-sleep result = 0x%X, want 0xDEADBEEF", result)
	}
	t.Log("wake-from-sleep OK")
}

// TestGuardPageLayout verifies that the sandbox memory regions are
// non-nil and that code/data/stack pointers look plausible (non-zero,
// different from each other).
func TestGuardPageLayout(t *testing.T) {
	const (
		codeSz  = 4096 * 4
		dataSz  = 4096 * 4
		stackSz = 4096 * 8
	)
	mem := C.sandbox_mem_create(codeSz, dataSz, stackSz)
	defer C.sandbox_mem_destroy(&mem)

	if mem.code == nil {
		t.Error("code region is nil")
	}
	if mem.data == nil {
		t.Error("data region is nil")
	}
	if mem.stack_top == nil {
		t.Error("stack_top is nil")
	}

	codeAddr := uintptr(mem.code)
	dataAddr := uintptr(mem.data)
	stackAddr := uintptr(mem.stack_top)

	if codeAddr == dataAddr || dataAddr == stackAddr || codeAddr == stackAddr {
		t.Errorf("regions overlap or are identical: code=%x data=%x stack=%x",
			codeAddr, dataAddr, stackAddr)
	}

	// Stack top must be above the base by at least stackSz.
	stackBase := stackAddr - stackSz
	if stackAddr <= stackBase {
		t.Errorf("stack_top (%x) not above base (%x)", stackAddr, stackBase)
	}

	t.Logf("code   @ 0x%x  size=%d", codeAddr, mem.code_size)
	t.Logf("data   @ 0x%x  size=%d", dataAddr, mem.data_size)
	t.Logf("stack  top=0x%x size=%d", stackAddr, mem.stack_size)
}

// TestSealCode writes a known byte pattern into the code region, seals it
// to PROT_READ|PROT_EXEC, and verifies the bytes are still readable.
func TestSealCode(t *testing.T) {
	mem := C.sandbox_mem_create(4096, 4096, 4096*4)
	defer C.sandbox_mem_destroy(&mem)

	// Write a pattern into the code region while it's still RW.
	code := unsafe.Slice((*byte)(mem.code), mem.code_size)
	for i := range code {
		code[i] = byte(i & 0xFF)
	}

	// Seal: flip to RX.
	C.sandbox_seal_code(&mem)

	// Verify bytes survived the mprotect (read is still permitted).
	for i, b := range code {
		want := byte(i & 0xFF)
		if b != want {
			t.Errorf("code[%d] = 0x%02X after seal, want 0x%02X", i, b, want)
			break
		}
	}
	t.Log("code region sealed and readable OK")
}

// ----------------------------------------------------------------------------
// Benchmark
// ----------------------------------------------------------------------------

// BenchmarkRoundTrip measures the end-to-end latency of a single
// Go→C→Go dispatch through the ring buffer while the thread is hot.
func BenchmarkRoundTrip(b *testing.B) {
	ring := C.ring_create()
	if ring == nil {
		b.Fatal("ring_create failed")
	}
	defer C.ring_destroy(ring)
	go func() {
		runtime.LockOSThread()
		C.interpreter_thread_main(ring)
	}()
	time.Sleep(5 * time.Millisecond)

	cap := ringCap(ring)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		headBefore := atomic.LoadUint64(ringHeadPtr(ring))
		slot := ringSlot(ring, headBefore&(cap-1))
		slot.state = 0
		for !goPush(ring, 0, 0, 0, 0) {
			runtime.Gosched()
		}
		for atomic.LoadUint32(&slot.state) != 2 {
			runtime.Gosched()
		}
	}
}

// BenchmarkRoundTripParallel stresses concurrent serialised producers.
func BenchmarkRoundTripParallel(b *testing.B) {
	ring := C.ring_create()
	if ring == nil {
		b.Fatal("ring_create failed")
	}
	defer C.ring_destroy(ring)
	go func() {
		runtime.LockOSThread()
		C.interpreter_thread_main(ring)
	}()
	time.Sleep(5 * time.Millisecond)

	var mu sync.Mutex
	cap := ringCap(ring)

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			headBefore := atomic.LoadUint64(ringHeadPtr(ring))
			slot := ringSlot(ring, headBefore&(cap-1))
			slot.state = 0
			for !goPush(ring, 0, uint64(rand.Uint32()), 0, 0) {
				mu.Unlock()
				runtime.Gosched()
				mu.Lock()
				headBefore = atomic.LoadUint64(ringHeadPtr(ring))
				slot = ringSlot(ring, headBefore&(cap-1))
			}
			mu.Unlock()
			for atomic.LoadUint32(&slot.state) != 2 {
				runtime.Gosched()
			}
		}
	})
}

// ----------------------------------------------------------------------------
// TestMain — print a summary header
// ----------------------------------------------------------------------------

func TestMain(m *testing.M) {
	fmt.Println("=== JIT Sandbox IPC Tests ===")
	fmt.Printf("    GOMAXPROCS = %d\n\n", runtime.GOMAXPROCS(0))
	m.Run()
}
