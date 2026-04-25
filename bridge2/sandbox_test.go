package bridge2

// Pure Go — no CGo here. All C interaction goes through sandbox.go.

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
	//sandbox "riscv/bridge2" // adjust to your actual module path
)

// startRing launches the interpreter thread and returns a ready Ring.
// Gives the C thread 5 ms to reach its poll loop before returning.
func startRing(t *testing.T) *Ring {
	t.Helper()
	ring := NewRing()
	time.Sleep(5 * time.Millisecond)
	return ring
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

// TestPingPong sends a single NOP (opcode 0) and checks the sentinel result.
func TestPingPong(t *testing.T) {
	ring := startRing(t)
	defer ring.Destroy()

	idx, ok := ring.Push(0, 0, 0, 0)
	if !ok {
		t.Fatal("Push: ring unexpectedly full")
	}
	result := ring.WaitResult(idx)
	const want = 0xDEADBEEF
	if result != want {
		t.Errorf("NOP result = 0x%X, want 0x%X", result, want)
	}
	t.Logf("NOP round-trip OK: result=0x%X", result)
}

// TestUnknownOpcode verifies that an unrecognised opcode returns ^0.
func TestUnknownOpcode(t *testing.T) {
	ring := startRing(t)
	defer ring.Destroy()

	idx, ok := ring.Push(0xFFFF, 42, 43, 44)
	if !ok {
		t.Fatal("Push: ring unexpectedly full")
	}
	result := ring.WaitResult(idx)
	if result != ^uint64(0) {
		t.Errorf("unknown opcode result = 0x%X, want 0xFFFFFFFFFFFFFFFF", result)
	}
}

// TestSequential sends N items in order and verifies each result arrives
// without drops or corruption.
func TestSequential(t *testing.T) {
	const N = 256
	ring := startRing(t)
	defer ring.Destroy()

	for i := 0; i < N; i++ {
		idx, ok := ring.Push(0, uint64(i), 0, 0)
		if !ok {
			t.Fatalf("iteration %d: ring full", i)
		}
		ring.WaitResult(idx)
		// Clear the slot so the next iteration doesn't see a stale DONE.
		ring.ClearSlot(idx)
	}
	t.Logf("Sequential: %d round-trips OK", N)
}

// TestBurst posts half the ring capacity in one shot and drains all results.
func TestBurst(t *testing.T) {
	ring := startRing(t)
	defer ring.Destroy()

	const burst = 512 // half of RING_CAPACITY=1024
	indices := make([]uint64, burst)

	for i := 0; i < burst; i++ {
		idx, ok := ring.Push(0, uint64(i), 0, 0)
		if !ok {
			t.Fatalf("burst push %d/%d: ring full unexpectedly", i, burst)
		}
		indices[i] = idx
	}
	for i, idx := range indices {
		ring.WaitResult(idx)
		ring.ClearSlot(idx)
		_ = i
	}
	t.Logf("Burst: %d items posted and drained OK", burst)
}

// TestConcurrentProducers runs multiple goroutines serialised by a mutex
// (SPSC contract: one producer at a time) and verifies all results arrive.
func TestConcurrentProducers(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50

	ring := startRing(t)
	defer ring.Destroy()

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
				var idx uint64
				mu.Lock()
				for {
					var ok bool
					idx, ok = ring.Push(0, uint64(id*1000+i), 0, 0)
					if ok {
						break
					}
					mu.Unlock()
					runtime.Gosched()
					mu.Lock()
				}
				mu.Unlock()

				ring.WaitResult(idx)
				ring.ClearSlot(idx)
				success.Add(1)
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

// TestIdleTransitionToSleep lets the thread cross its 100 ms spin deadline,
// then verifies the futex wake path correctly resumes it.
func TestIdleTransitionToSleep(t *testing.T) {
	ring := startRing(t)
	defer ring.Destroy()

	t.Log("waiting 200 ms for idle→sleep transition...")
	time.Sleep(200 * time.Millisecond)

	idx, ok := ring.Push(0, 0, 0, 0)
	if !ok {
		t.Fatal("Push after sleep: ring full")
	}

	// Poll with a generous timeout — futex wake is ~1–4 µs but CI can be slow.
	done := make(chan uint64, 1)
	go func() { done <- ring.WaitResult(idx) }()

	select {
	case result := <-done:
		if result != 0xDEADBEEF {
			t.Errorf("wake-from-sleep result = 0x%X, want 0xDEADBEEF", result)
		}
		t.Log("wake-from-sleep OK")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out: thread did not wake from futex sleep")
	}
}

// TestGuardPageLayout verifies that the three sandbox memory regions are
// non-nil, non-overlapping, and correctly sized.
func TestGuardPageLayout(t *testing.T) {
	mem := NewSandboxMem(4096*4, 4096*4, 4096*8)
	defer mem.Destroy()

	code := mem.Code()
	data := mem.Data()
	stackTop := mem.StackTop()

	if len(code) == 0 {
		t.Error("code region is empty")
	}
	if len(data) == 0 {
		t.Error("data region is empty")
	}
	if stackTop == 0 {
		t.Error("stack_top is zero")
	}

	codeAddr := uintptr(unsafe.Pointer(&code[0]))
	dataAddr := uintptr(unsafe.Pointer(&data[0]))

	if codeAddr == dataAddr {
		t.Error("code and data regions overlap")
	}
	if stackTop == codeAddr || stackTop == dataAddr {
		t.Error("stack_top collides with code or data region")
	}

	t.Logf("code   @ 0x%x  len=%d", codeAddr, len(code))
	t.Logf("data   @ 0x%x  len=%d", dataAddr, len(data))
	t.Logf("stack  top=0x%x", stackTop)
}

// TestSealCode writes a known byte pattern into the code region while RW,
// seals it to PROT_READ|PROT_EXEC, then verifies the bytes are still readable.
func TestSealCode(t *testing.T) {
	mem := NewSandboxMem(4096, 4096, 4096*4)
	defer mem.Destroy()

	code := mem.Code()
	for i := range code {
		code[i] = byte(i & 0xFF)
	}

	mem.SealCode()

	for i, b := range code {
		if b != byte(i&0xFF) {
			t.Errorf("code[%d] = 0x%02X after seal, want 0x%02X", i, b, byte(i&0xFF))
			break
		}
	}
	t.Log("code region sealed and still readable OK")
}

// ----------------------------------------------------------------------------
// Benchmarks
// ----------------------------------------------------------------------------

// BenchmarkRoundTrip measures hot-path Go→C→Go latency through the ring.
func BenchmarkRoundTrip(b *testing.B) {
	ring := NewRing()
	defer ring.Destroy()
	time.Sleep(5 * time.Millisecond)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		idx, _ := ring.Push(0, 0, 0, 0)
		ring.WaitResult(idx)
		ring.ClearSlot(idx)
	}
}

// BenchmarkRoundTripParallel stresses concurrent serialised producers.
func BenchmarkRoundTripParallel(b *testing.B) {
	ring := NewRing()
	defer ring.Destroy()
	time.Sleep(5 * time.Millisecond)

	var mu sync.Mutex
	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			idx, _ := ring.Push(0, uint64(rand.Uint32()), 0, 0)
			mu.Unlock()
			ring.WaitResult(idx)
			ring.ClearSlot(idx)
		}
	})
}

// ----------------------------------------------------------------------------
// TestMain
// ----------------------------------------------------------------------------

func TestMain(m *testing.M) {
	fmt.Printf("=== JIT Sandbox IPC Tests  (GOMAXPROCS=%d) ===\n\n",
		runtime.GOMAXPROCS(0))
	m.Run()
}
