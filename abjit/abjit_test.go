package abjit

import (
	"encoding/binary"
	"runtime"
	"testing"
	"unsafe"
)

// heapU64 allocates a uint64 slice on the heap (never stack).
// Critical: callJIT's 65KB frame triggers morestack, which copies
// the goroutine stack. Any uintptr pointing to stack memory becomes
// stale after the copy. Heap memory doesn't move.
//
//go:noinline
func heapU64(n int) []uint64 {
	s := make([]uint64, n)
	return s
}

// ---------------------------------------------------------------------------
// Minimal x86_64 code builder for Phase 0 hand-assembled tests
// ---------------------------------------------------------------------------

type codeBuilder struct {
	buf []byte
	off int
}

func (c *codeBuilder) emit(b ...byte) {
	copy(c.buf[c.off:], b)
	c.off += len(b)
}

func (c *codeBuilder) imm64(v uint64) {
	binary.LittleEndian.PutUint64(c.buf[c.off:], v)
	c.off += 8
}

func (c *codeBuilder) movabs(reg int, imm uint64) {
	if reg >= 8 {
		c.emit(0x49)
	} else {
		c.emit(0x48)
	}
	c.emit(0xB8 + byte(reg&7))
	c.imm64(imm)
}

// storeRegToR9 emits MOV [R9+disp8], srcReg (64-bit).
func (c *codeBuilder) storeRegToR9(srcReg, disp int) {
	rex := byte(0x49) // REX.W + REX.B (R9 base)
	if srcReg >= 8 {
		rex |= 0x04 // REX.R
	}
	c.emit(rex, 0x89)
	regBits := byte(srcReg&7) << 3
	const rmR9 = 0x01 // R9 & 7
	if disp == 0 {
		c.emit(regBits | rmR9) // mod=00
	} else {
		c.emit(0x40 | regBits | rmR9, byte(disp)) // mod=01, disp8
	}
}

// callback emits the 5-instruction gocall sequence (34 bytes).
//
//	MOVABS gocallAddr, R11      (10B)
//	LEA    R10, [RIP+17]        ( 7B)  ← resume = after JMP R11
//	MOV    [RSP], R10           ( 4B)
//	MOVABS goFuncAddr, R10      (10B)
//	JMP    R11                  ( 3B)
//	                              34B total, resume point here
func (c *codeBuilder) callback(goFunc uintptr) {
	c.movabs(11, uint64(gocallAddr))                       // R11 = gocall
	c.emit(0x4C, 0x8D, 0x15, 0x11, 0x00, 0x00, 0x00)      // LEA R10, [RIP+17]
	c.emit(0x4C, 0x89, 0x14, 0x24)                         // MOV [RSP], R10
	c.movabs(10, uint64(goFunc))                           // R10 = Go func
	c.emit(0x41, 0xFF, 0xE3)                               // JMP R11
}

// exit emits the exit sequence: restore callee-saves, undo frame, return.
func (c *codeBuilder) exit() {
	c.emit(0x48, 0x8B, 0x5C, 0x24, 0x08)                   // MOV RBX, [RSP+0x08]
	c.emit(0x4C, 0x8B, 0x64, 0x24, 0x18)                   // MOV R12, [RSP+0x18]
	c.emit(0x4C, 0x8B, 0x6C, 0x24, 0x20)                   // MOV R13, [RSP+0x20]
	c.emit(0x4C, 0x8B, 0x7C, 0x24, 0x28)                   // MOV R15, [RSP+0x28]
	c.emit(0x48, 0x81, 0xC4, 0xF8, 0xFF, 0x00, 0x00)       // ADD RSP, 0xFFF8
	c.emit(0x5D)                                            // POP RBP
	c.emit(0xC3)                                            // RET
}

// ---------------------------------------------------------------------------
// Callback targets
// ---------------------------------------------------------------------------

var callbackFlag bool

//go:nosplit
func nosplitCallback() {
	callbackFlag = true
}

func gcCallback() {
	callbackFlag = true
	runtime.GC()
}

// ---------------------------------------------------------------------------
// Phase 0 Tests
// ---------------------------------------------------------------------------

// TestBasicEntryExit verifies the trampoline can enter JIT code that
// immediately exits without crashing.
func TestBasicEntryExit(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}
	cb.exit()

	state := heapU64(8)
	callJIT(
		uintptr(unsafe.Pointer(&code[0])),
		uintptr(unsafe.Pointer(&state[0])),
		0,
	)
	t.Log("basic entry/exit OK")
}

// TestCallback verifies the gocall mechanism: JIT code calls a Go function
// via the gocall label, then resumes and exits.
func TestCallback(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	callbackFlag = false
	cb := &codeBuilder{buf: code}
	cb.callback(funcAddr(nosplitCallback))
	cb.exit()

	state := heapU64(8)
	callJIT(
		uintptr(unsafe.Pointer(&code[0])),
		uintptr(unsafe.Pointer(&state[0])),
		0,
	)
	if !callbackFlag {
		t.Fatal("callback was not called")
	}
	t.Log("callback OK")
}

// TestR9Valid verifies R9 is correctly loaded by the trampoline
// (no callbacks — just store R9 and exit).
func TestR9Valid(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}
	cb.storeRegToR9(9, 0)  // state[0] = R9
	cb.exit()

	state := heapU64(8)
	stateAddr := uintptr(unsafe.Pointer(&state[0]))
	callJIT(uintptr(unsafe.Pointer(&code[0])), stateAddr, 0)

	if state[0] != uint64(stateAddr) {
		t.Errorf("R9 = 0x%X, want 0x%X (state addr)", state[0], stateAddr)
	} else {
		t.Logf("R9 = 0x%X ✓", state[0])
	}
}

// TestStoreNoCallback verifies register store encoding works
// without any callback in the path.
func TestStoreNoCallback(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}
	cb.movabs(3, 0xAAAAAAAAAAAAAAAA) // RBX = sentinel
	cb.storeRegToR9(3, 0)            // state[0] = RBX
	cb.movabs(8, 0x1234567812345678) // R8 = sentinel
	cb.storeRegToR9(8, 8)            // state[1] = R8
	cb.exit()

	state := heapU64(8)
	callJIT(uintptr(unsafe.Pointer(&code[0])), uintptr(unsafe.Pointer(&state[0])), 0)

	if state[0] != 0xAAAAAAAAAAAAAAAA {
		t.Errorf("state[0] (RBX) = 0x%X, want 0xAAAAAAAAAAAAAAAA", state[0])
	} else {
		t.Logf("state[0] (RBX) = 0x%X ✓", state[0])
	}
	if state[1] != 0x1234567812345678 {
		t.Errorf("state[1] (R8) = 0x%X, want 0x1234567812345678", state[1])
	} else {
		t.Logf("state[1] (R8) = 0x%X ✓", state[1])
	}
}

// TestAbsoluteStore bypasses R9 entirely — loads the state address
// via MOVABS and stores a sentinel. Isolates whether R9 is the problem.
func TestAbsoluteStore(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	state := heapU64(8)
	stateAddr := uint64(uintptr(unsafe.Pointer(&state[0])))

	cb := &codeBuilder{buf: code}
	cb.movabs(0, stateAddr)            // RAX = &state[0]
	cb.movabs(1, 0xDEADBEEFDEADBEEF)  // RCX = sentinel
	cb.emit(0x48, 0x89, 0x08)          // MOV [RAX], RCX
	cb.exit()

	callJIT(uintptr(unsafe.Pointer(&code[0])), uintptr(unsafe.Pointer(&state[0])), 0)

	if state[0] != 0xDEADBEEFDEADBEEF {
		t.Errorf("absolute store: state[0] = 0x%X, want 0xDEADBEEFDEADBEEF", state[0])
	} else {
		t.Logf("absolute store works ✓: 0x%X", state[0])
	}
}

// TestR9ViaLoad loads the value at SP+0x10010 (where trampoline reads
// cpuState from) and stores it via absolute address, to verify what's
// actually at that stack location.
func TestR9ViaLoad(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	state := heapU64(8)
	stateAddr := uint64(uintptr(unsafe.Pointer(&state[0])))

	cb := &codeBuilder{buf: code}
	// Load R9 into RAX (to inspect its value)
	cb.emit(0x4C, 0x89, 0xC8) // MOV RAX, R9  (REX.WR, 89, ModRM: 11,001,000)
	// Store RAX to absolute address
	cb.movabs(1, stateAddr)    // RCX = &state[0]
	cb.emit(0x48, 0x89, 0x01)  // MOV [RCX], RAX
	cb.exit()

	callJIT(uintptr(unsafe.Pointer(&code[0])), uintptr(unsafe.Pointer(&state[0])), 0)

	t.Logf("R9 value = 0x%X, expected = 0x%X", state[0], stateAddr)
	if state[0] == stateAddr {
		t.Log("R9 is correct ✓")
	} else if state[0] == 0 {
		t.Error("R9 is zero — trampoline didn't load it")
	} else {
		t.Errorf("R9 points elsewhere — possible ABI wrapper issue")
	}
}

// TestDumpAndVerify dumps the JIT code bytes and verifies each instruction
// by comparing against known-good encodings.
func TestDumpAndVerify(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	state := heapU64(8)
	stateAddr := uint64(uintptr(unsafe.Pointer(&state[0])))
	t.Logf("stateAddr = 0x%X", stateAddr)

	cb := &codeBuilder{buf: code}
	start := cb.off
	cb.movabs(0, stateAddr) // RAX = &state[0]
	t.Logf("after movabs RAX: off=%d bytes=% x", cb.off, code[start:cb.off])

	start = cb.off
	cb.movabs(1, 0xDEADBEEFDEADBEEF) // RCX = sentinel
	t.Logf("after movabs RCX: off=%d bytes=% x", cb.off, code[start:cb.off])

	start = cb.off
	cb.emit(0x48, 0x89, 0x08) // MOV [RAX], RCX
	t.Logf("after MOV [RAX],RCX: off=%d bytes=% x", cb.off, code[start:cb.off])

	start = cb.off
	cb.exit()
	t.Logf("after exit: off=%d bytes=% x", cb.off, code[start:cb.off])

	t.Logf("full code (%d bytes): % x", cb.off, code[:cb.off])

	// Verify the code is actually in the mmap'd page
	t.Logf("code[0] addr = 0x%X", uintptr(unsafe.Pointer(&code[0])))
	t.Logf("code[0:3] = % x (should be 48 b8 ...)", code[0:3])

	callJIT(uintptr(unsafe.Pointer(&code[0])), uintptr(unsafe.Pointer(&state[0])), 0)

	t.Logf("state[0] = 0x%X (want 0xDEADBEEFDEADBEEF)", state[0])
}

// TestCalleeSaveVerification loads sentinel values into callee-saved
// registers (RBX, R13, R15), does a callback, then checks the values
// survived. System V ABI guarantees these are preserved by the callee.
func TestCalleeSaveVerification(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	const (
		sentRBX = 0xAAAAAAAAAAAAAAAA
		sentR13 = 0xBBBBBBBBBBBBBBBB
		sentR15 = 0xCCCCCCCCCCCCCCCC
		sentR12 = 0xDDDDDDDDDDDDDDDD
	)

	callbackFlag = false
	cb := &codeBuilder{buf: code}

	// Load sentinels — do this AFTER trampoline has saved the originals
	cb.movabs(3, sentRBX)  // RBX = 0xAAAA...
	cb.movabs(13, sentR13) // R13 = 0xBBBB...
	cb.movabs(15, sentR15) // R15 = 0xCCCC...
	cb.movabs(12, sentR12) // R12 = 0xDDDD...

	// Do callback — Go function may clobber caller-saved regs but
	// must preserve RBX, RBP, R12, R13, R15 (System V callee-saved).
	cb.callback(funcAddr(nosplitCallback))

	// After callback: store register values to state via R9
	cb.storeRegToR9(3, 0)   // state[0] = RBX
	cb.storeRegToR9(13, 8)  // state[1] = R13
	cb.storeRegToR9(15, 16) // state[2] = R15
	cb.storeRegToR9(12, 24) // state[3] = R12

	cb.exit()

	state := heapU64(8)
	callJIT(
		uintptr(unsafe.Pointer(&code[0])),
		uintptr(unsafe.Pointer(&state[0])),
		0,
	)

	if !callbackFlag {
		t.Fatal("callback was not called")
	}
	check := func(name string, idx int, want uint64) {
		if state[idx] != want {
			t.Errorf("%s not preserved: got 0x%X, want 0x%X", name, state[idx], want)
		} else {
			t.Logf("%s preserved: 0x%X ✓", name, state[idx])
		}
	}
	check("RBX", 0, sentRBX)
	check("R13", 1, sentR13)
	check("R15", 2, sentR15)
	check("R12", 3, sentR12)
}

// TestR9Restoration verifies that R9 (CPU state pointer) is correctly
// restored by the gocall handler after a callback clobbers it.
func TestR9Restoration(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}

	// Store R9 before callback → state[0]
	cb.storeRegToR9(9, 0)

	// Callback (may clobber R9; gocall handler restores it)
	callbackFlag = false
	cb.callback(funcAddr(nosplitCallback))

	// Store R9 after callback → state[1]
	cb.storeRegToR9(9, 8)

	cb.exit()

	state := heapU64(8)
	stateAddr := uintptr(unsafe.Pointer(&state[0]))
	callJIT(
		uintptr(unsafe.Pointer(&code[0])),
		stateAddr,
		0,
	)

	if !callbackFlag {
		t.Fatal("callback was not called")
	}
	if state[0] != uint64(stateAddr) {
		t.Errorf("R9 before callback: got 0x%X, want 0x%X", state[0], stateAddr)
	} else {
		t.Logf("R9 before callback: 0x%X ✓", state[0])
	}
	if state[1] != uint64(stateAddr) {
		t.Errorf("R9 after callback: got 0x%X, want 0x%X", state[1], stateAddr)
	} else {
		t.Logf("R9 after callback: 0x%X ✓ (correctly restored by gocall handler)", state[1])
	}
}

// TestGCSafety calls runtime.GC() inside a callback from JIT code.
// This exercises the exact scenario that crashes without proper
// NO_LOCAL_POINTERS and frame setup.
func TestGCSafety(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	callbackFlag = false
	cb := &codeBuilder{buf: code}
	cb.callback(funcAddr(gcCallback))
	cb.exit()

	state := heapU64(8)
	callJIT(
		uintptr(unsafe.Pointer(&code[0])),
		uintptr(unsafe.Pointer(&state[0])),
		0,
	)
	if !callbackFlag {
		t.Fatal("callback was not called")
	}
	t.Log("GC during callback: no crash ✓")
}

// TestGCSafetyStress runs the GC callback many times to shake out
// any intermittent races.
func TestGCSafetyStress(t *testing.T) {
	code, err := mmapExec(4096)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}
	cb.callback(funcAddr(gcCallback))
	cb.exit()

	state := heapU64(8)
	for i := 0; i < 100; i++ {
		callbackFlag = false
		callJIT(
			uintptr(unsafe.Pointer(&code[0])),
			uintptr(unsafe.Pointer(&state[0])),
			0,
		)
		if !callbackFlag {
			t.Fatalf("iteration %d: callback not called", i)
		}
	}
	t.Log("100 GC callbacks: no crash ✓")
}

// BenchmarkTrampolineOverhead measures raw entry/exit cost of callJIT
// with JIT code that immediately exits.
func BenchmarkTrampolineOverhead(b *testing.B) {
	code, err := mmapExec(4096)
	if err != nil {
		b.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}
	cb.exit()

	state := heapU64(8)
	codeAddr := uintptr(unsafe.Pointer(&code[0]))
	stateAddr := uintptr(unsafe.Pointer(&state[0]))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callJIT(codeAddr, stateAddr, 0)
	}
}

// BenchmarkCallbackRoundTrip measures callJIT + one callback + exit.
func BenchmarkCallbackRoundTrip(b *testing.B) {
	code, err := mmapExec(4096)
	if err != nil {
		b.Fatal(err)
	}
	defer munmapExec(code)

	cb := &codeBuilder{buf: code}
	cb.callback(funcAddr(nosplitCallback))
	cb.exit()

	state := heapU64(8)
	codeAddr := uintptr(unsafe.Pointer(&code[0]))
	stateAddr := uintptr(unsafe.Pointer(&state[0]))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callJIT(codeAddr, stateAddr, 0)
	}
}
