# Phase 2: abjit package — trampoline, CodeBuilder, State, tests

## Context

Phase 1 (complete) added pluggable `RegPolicy` to `ir/`. Phase 2 makes
the `abjit` package a working standalone JIT execution engine:

1. Update trampoline to 2-arg signature, RBP as register file base
2. Simplify gocall handler to 2 instructions
3. Create exported `CodeBuilder` (promoted from test's private helper)
4. Create `State` struct matching shadow register file layout
5. Update all Phase 0 tests, add new tests exercising RBP-based access

**Scope**: 5 files modified, 2 created. Zero changes outside `~/ris/abjit/`.

---

## Step 1: Update trampoline

### 1a. trampoline_amd64.s — new content (complete replacement)

```asm
#include "funcdata.h"
#include "textflag.h"

// func callJIT(code, regFileBase uintptr)
//
// Frame layout (after Go's prologue):
//   SP+0      resume address slot (JIT writes here for gocall)
//   SP+8      saved RBX
//   SP+16     saved RBP (Go frame pointer — not explicitly restored)
//   SP+24     saved R12
//   SP+32     saved R13
//   SP+40     saved R15
//   SP+48..   available stack for Go callbacks (~65KB)
//
TEXT ·callJIT(SB), 0, $65528-16
	NO_LOCAL_POINTERS
	MOVQ BX,  8(SP)
	MOVQ BP,  16(SP)
	MOVQ R12, 24(SP)
	MOVQ R13, 32(SP)
	MOVQ R15, 40(SP)

	MOVQ regFileBase+8(FP), BP
	MOVQ code+0(FP), AX
	JMP AX
gocall:
	CALL R10
	JMP (SP)

// func callJITImplAddr() uintptr
TEXT ·callJITImplAddr(SB), 0, $0-8
	NO_LOCAL_POINTERS
	MOVQ $·callJIT(SB), AX
	MOVQ AX, ret+0(FP)
	RET
```

**Changes from old trampoline**:
- Frame declaration: `$65528-16` (was `$65528-24`, 2 args instead of 3)
- Removed: `MOVQ cpuState+8(FP), R9` — R9 is no longer pinned
- Removed: `MOVQ sandboxSP+16(FP), R12` — R12 is no longer pinned
- Added: `MOVQ regFileBase+8(FP), BP` — RBP = register file base
- gocall handler: removed `MOVQ cpuState+8(FP), R9` restore line

**Why RBP survives callbacks**: Go's frame pointer protocol (`PUSH RBP;
MOV RBP,RSP` in prologue, `POP RBP` in epilogue) saves and restores our
regFileBase value around every Go function call.

### 1b. trampoline.go — update Go stubs

Old:
```go
func callJIT(code, cpuState, sandboxSP uintptr)
```

New:
```go
func callJIT(code, regFileBase uintptr)
```

`callJITImplAddr()` unchanged.

---

## Step 2: Update callfunc.go

### getCallAddr() — unchanged logic

`getCallAddr()` scans the `callJIT` function body for the byte pattern
`{0x41, 0xFF, 0xD2}` (CALL R10). This pattern exists in the new
trampoline at the `gocall:` label, same as before. The scan still works.

**No code changes needed** — just re-run tests to verify the pattern is
found at the correct offset.

### funcAddr() — unchanged

---

## Step 3: Create emit.go — exported CodeBuilder

**File**: `~/ris/abjit/emit.go` (NEW)

```go
package abjit

import (
	"encoding/binary"
	"unsafe"
)

// x86_64 register numbers.
const (
	RAX = 0
	RCX = 1
	RDX = 2
	RBX = 3
	RSP = 4
	RBP = 5
	RSI = 6
	RDI = 7
	R8  = 8
	R9  = 9
	R10 = 10
	R11 = 11
	R12 = 12
	R13 = 13
	R14 = 14
	R15 = 15
)

// CodeBuilder emits x86_64 machine code into an mmap'd executable page.
type CodeBuilder struct {
	buf []byte
	off int
}

// NewCodeBuilder allocates a 4096-byte executable page and returns a
// CodeBuilder that emits into it. Call Free when done.
func NewCodeBuilder() (*CodeBuilder, error) {
	buf, err := mmapExec(4096)
	if err != nil {
		return nil, err
	}
	return &CodeBuilder{buf: buf}, nil
}

// Free releases the executable page.
func (c *CodeBuilder) Free() error {
	return munmapExec(c.buf)
}

// Addr returns the base address of the code page.
func (c *CodeBuilder) Addr() uintptr {
	return uintptr(unsafe.Pointer(&c.buf[0]))
}

// Len returns the number of bytes emitted so far.
func (c *CodeBuilder) Len() int { return c.off }

// Reset resets the write offset to 0 (reuse the page).
func (c *CodeBuilder) Reset() { c.off = 0 }

func (c *CodeBuilder) emit(b ...byte) {
	copy(c.buf[c.off:], b)
	c.off += len(b)
}

func (c *CodeBuilder) imm32(v int32) {
	binary.LittleEndian.PutUint32(c.buf[c.off:], uint32(v))
	c.off += 4
}

func (c *CodeBuilder) imm64(v uint64) {
	binary.LittleEndian.PutUint64(c.buf[c.off:], v)
	c.off += 8
}

// Movabs emits MOVABS reg, imm64 (REX.W + B8+rd).
func (c *CodeBuilder) Movabs(reg int, imm uint64) {
	if reg >= 8 {
		c.emit(0x49)
	} else {
		c.emit(0x48)
	}
	c.emit(0xB8 + byte(reg&7))
	c.imm64(imm)
}

// StoreToRBP emits MOV [RBP+disp], srcReg (64-bit).
// Uses disp8 when disp fits in [-128,127], disp32 otherwise.
// RBP with mod=00 is RIP-relative, so we always use mod=01 or mod=10.
func (c *CodeBuilder) StoreToRBP(srcReg, disp int) {
	rex := byte(0x48) // REX.W
	if srcReg >= 8 {
		rex |= 0x04 // REX.R
	}
	c.emit(rex, 0x89)
	regBits := byte(srcReg&7) << 3
	if disp >= -128 && disp <= 127 {
		c.emit(0x45|regBits, byte(int8(disp)))
	} else {
		c.emit(0x85 | regBits)
		c.imm32(int32(disp))
	}
}

// LoadFromRBP emits MOV dstReg, [RBP+disp] (64-bit).
func (c *CodeBuilder) LoadFromRBP(dstReg, disp int) {
	rex := byte(0x48) // REX.W
	if dstReg >= 8 {
		rex |= 0x04 // REX.R
	}
	c.emit(rex, 0x8B)
	regBits := byte(dstReg&7) << 3
	if disp >= -128 && disp <= 127 {
		c.emit(0x45|regBits, byte(int8(disp)))
	} else {
		c.emit(0x85 | regBits)
		c.imm32(int32(disp))
	}
}

// AddReg emits ADD dst, src (64-bit register-register).
func (c *CodeBuilder) AddReg(dst, src int) {
	rex := byte(0x48)
	if src >= 8 {
		rex |= 0x04 // REX.R
	}
	if dst >= 8 {
		rex |= 0x01 // REX.B
	}
	c.emit(rex, 0x01, 0xC0|byte(src&7)<<3|byte(dst&7))
}

// SubReg emits SUB dst, src (64-bit register-register).
func (c *CodeBuilder) SubReg(dst, src int) {
	rex := byte(0x48)
	if src >= 8 {
		rex |= 0x04
	}
	if dst >= 8 {
		rex |= 0x01
	}
	c.emit(rex, 0x29, 0xC0|byte(src&7)<<3|byte(dst&7))
}

// Callback emits the 34-byte gocall sequence.
//
//	MOVABS gocallAddr, R11     (10B)
//	LEA    R10, [RIP+17]       ( 7B)
//	MOV    [RSP], R10          ( 4B)
//	MOVABS goFuncAddr, R10     (10B)
//	JMP    R11                 ( 3B)
func (c *CodeBuilder) Callback(goFunc uintptr) {
	c.Movabs(R11, uint64(gocallAddr))
	c.emit(0x4C, 0x8D, 0x15, 0x11, 0x00, 0x00, 0x00) // LEA R10,[RIP+17]
	c.emit(0x4C, 0x89, 0x14, 0x24)                     // MOV [RSP],R10
	c.Movabs(R10, uint64(goFunc))
	c.emit(0x41, 0xFF, 0xE3)                            // JMP R11
}

// Exit emits the exit sequence: restore callee-saves, undo frame, RET.
func (c *CodeBuilder) Exit() {
	c.emit(0x48, 0x8B, 0x5C, 0x24, 0x08) // MOV RBX, [RSP+0x08]
	c.emit(0x4C, 0x8B, 0x64, 0x24, 0x18) // MOV R12, [RSP+0x18]
	c.emit(0x4C, 0x8B, 0x6C, 0x24, 0x20) // MOV R13, [RSP+0x20]
	c.emit(0x4C, 0x8B, 0x7C, 0x24, 0x28) // MOV R15, [RSP+0x28]
	c.emit(0x48, 0x81, 0xC4, 0xF8, 0xFF, 0x00, 0x00) // ADD RSP, 0xFFF8
	c.emit(0x5D) // POP RBP
	c.emit(0xC3) // RET
}

// MovRegReg emits MOV dst, src (64-bit register-register).
func (c *CodeBuilder) MovRegReg(dst, src int) {
	rex := byte(0x48)
	if src >= 8 {
		rex |= 0x04
	}
	if dst >= 8 {
		rex |= 0x01
	}
	c.emit(rex, 0x89, 0xC0|byte(src&7)<<3|byte(dst&7))
}
```

**Encoding notes for StoreToRBP / LoadFromRBP**:

x86_64 ModRM with base=RBP (register 5):
- mod=00, rm=101 is **reinterpreted as [RIP+disp32]**, NOT [RBP]
- Therefore, even for disp=0, we must use mod=01 with disp8=0x00
- For disp in [-128, 127]: mod=01, 1-byte displacement
- For disp outside that range (e.g., f[0] at offset 256): mod=10, 4-byte displacement

Verification: `MOV [RBP+0], RAX` = `48 89 45 00` ✓
`MOV [RBP+8], RCX` = `48 89 4D 08` ✓
`MOV [RBP+256], RAX` = `48 89 85 00 01 00 00` ✓
`MOV R8, [RBP+0]` = `4C 8B 45 00` ✓

---

## Step 4: Create abjit.go — State struct + public API

**File**: `~/ris/abjit/abjit.go` (NEW)

```go
package abjit

import "unsafe"

// State mirrors the shadow register file layout used by the JIT.
// Must be heap-allocated (callJIT's 65KB frame triggers morestack;
// stack-allocated State would be invalidated by the stack copy).
//
// Layout matches guestmem.go's RegFileBase() page:
//
//	Offset 0:   x[0..31]  — 32 × 8 = 256 bytes
//	Offset 256: f[0..31]  — 32 × 8 = 256 bytes
//	Offset 512: fcsr      — 4 bytes
//	Offset 516: (padding) — 4 bytes
//	Offset 520: memBase   — 8 bytes
//	Offset 528: memMask   — 8 bytes
type State struct {
	X       [32]uint64
	F       [32]uint64
	FCSR    uint32
	_       uint32
	MemBase uintptr
	MemMask uint64
}

// NewState allocates a State on the heap.
//
//go:noinline
func NewState() *State {
	return new(State)
}

// RegFileBase returns the address of the State as a uintptr,
// suitable for passing to callJIT as the regFileBase argument.
func (s *State) RegFileBase() uintptr {
	return uintptr(unsafe.Pointer(s))
}

// Run executes JIT code with the given state.
func Run(cb *CodeBuilder, s *State) {
	callJIT(cb.Addr(), s.RegFileBase())
}
```

**Key design decisions**:

- `NewState()` is `//go:noinline` to prevent the compiler from inlining
  and stack-allocating the State. Heap allocation is mandatory because
  `callJIT` receives the address as `uintptr`, and morestack would
  invalidate a stack pointer.

- `State` layout verified by `unsafe.Offsetof` in tests (Step 5).

- `Run()` is a free function, not a method, because the CodeBuilder and
  State are independent — the same State can run different code blocks,
  and the same code block can run with different States.

---

## Step 5: Update abjit_test.go

### 5a. Remove private codeBuilder — use exported CodeBuilder

The private `codeBuilder` struct, its methods (`emit`, `imm64`, `movabs`,
`storeRegToR9`, `callback`, `exit`), and `heapU64` are all replaced by
the exported `CodeBuilder` and `State`.

Remove: lines 10-91 (everything from `heapU64` through `exit()`).
Keep: `callbackFlag`, `nosplitCallback()`, `gcCallback()`.

### 5b. Remove R9-specific tests

Remove entirely:
- `TestR9Valid` (lines 162-182)
- `TestR9ViaLoad` (lines 245-273)
- `TestR9Restoration` (lines 377-421)

### 5c. Update remaining Phase 0 tests

Every test changes:
1. `codeBuilder{buf: code}` → `NewCodeBuilder()` (allocates its own page)
2. `heapU64(8)` → `NewState()`
3. `callJIT(codeAddr, stateAddr, 0)` → `callJIT(cb.Addr(), state.RegFileBase())`
4. `storeRegToR9(reg, disp)` → `cb.StoreToRBP(reg, disp)`
5. `movabs(reg, imm)` → `cb.Movabs(reg, imm)`
6. `callback(fn)` → `cb.Callback(fn)`
7. `exit()` → `cb.Exit()`
8. `state[0]` → `state.X[0]` (first uint64 in register file)

**TestBasicEntryExit**:
```go
func TestBasicEntryExit(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())
	t.Log("basic entry/exit OK")
}
```

**TestCallback**:
```go
func TestCallback(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	callbackFlag = false
	cb.Callback(funcAddr(nosplitCallback))
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())
	if !callbackFlag {
		t.Fatal("callback was not called")
	}
}
```

**TestStoreNoCallback** — uses StoreToRBP. The test stores sentinel
values via registers to the State, verifying RBP-relative addressing:
```go
func TestStoreNoCallback(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.Movabs(RBX, 0xAAAAAAAAAAAAAAAA)
	cb.StoreToRBP(RBX, 0)      // state.X[0] = RBX
	cb.Movabs(R8, 0x1234567812345678)
	cb.StoreToRBP(R8, 8)       // state.X[1] = R8
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())

	if state.X[0] != 0xAAAAAAAAAAAAAAAA {
		t.Errorf("X[0] = 0x%X, want 0xAAAAAAAAAAAAAAAA", state.X[0])
	}
	if state.X[1] != 0x1234567812345678 {
		t.Errorf("X[1] = 0x%X, want 0x1234567812345678", state.X[1])
	}
}
```

**TestAbsoluteStore** — unchanged except 2-arg callJIT:
```go
func TestAbsoluteStore(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	state := NewState()
	stateAddr := uint64(state.RegFileBase())

	cb.Movabs(RAX, stateAddr)
	cb.Movabs(RCX, 0xDEADBEEFDEADBEEF)
	cb.emit(0x48, 0x89, 0x08) // MOV [RAX], RCX
	cb.Exit()

	callJIT(cb.Addr(), state.RegFileBase())
	if state.X[0] != 0xDEADBEEFDEADBEEF {
		t.Errorf("X[0] = 0x%X, want 0xDEADBEEFDEADBEEF", state.X[0])
	}
}
```

**TestCalleeSaveVerification** — stores via RBP after callback:
```go
func TestCalleeSaveVerification(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	const (
		sentRBX = 0xAAAAAAAAAAAAAAAA
		sentR13 = 0xBBBBBBBBBBBBBBBB
		sentR15 = 0xCCCCCCCCCCCCCCCC
		sentR12 = 0xDDDDDDDDDDDDDDDD
	)

	callbackFlag = false
	cb.Movabs(RBX, sentRBX)
	cb.Movabs(R13, sentR13)
	cb.Movabs(R15, sentR15)
	cb.Movabs(R12, sentR12)

	cb.Callback(funcAddr(nosplitCallback))

	// After callback: RBP still = regFileBase (preserved by Go's
	// frame pointer protocol). Store sentinel values to verify.
	cb.StoreToRBP(RBX, 0)  // X[0]
	cb.StoreToRBP(R13, 8)  // X[1]
	cb.StoreToRBP(R15, 16) // X[2]
	cb.StoreToRBP(R12, 24) // X[3]
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())

	if !callbackFlag {
		t.Fatal("callback was not called")
	}
	check := func(name string, got, want uint64) {
		if got != want {
			t.Errorf("%s not preserved: got 0x%X, want 0x%X", name, got, want)
		}
	}
	check("RBX", state.X[0], sentRBX)
	check("R13", state.X[1], sentR13)
	check("R15", state.X[2], sentR15)
	check("R12", state.X[3], sentR12)
}
```

**TestGCSafety**:
```go
func TestGCSafety(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	callbackFlag = false
	cb.Callback(funcAddr(gcCallback))
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())
	if !callbackFlag {
		t.Fatal("callback was not called")
	}
}
```

**TestGCSafetyStress**:
```go
func TestGCSafetyStress(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.Callback(funcAddr(gcCallback))
	cb.Exit()

	state := NewState()
	for i := 0; i < 100; i++ {
		callbackFlag = false
		callJIT(cb.Addr(), state.RegFileBase())
		if !callbackFlag {
			t.Fatalf("iteration %d: callback not called", i)
		}
	}
}
```

**BenchmarkTrampolineOverhead**:
```go
func BenchmarkTrampolineOverhead(b *testing.B) {
	cb, err := NewCodeBuilder()
	if err != nil {
		b.Fatal(err)
	}
	defer cb.Free()

	cb.Exit()

	state := NewState()
	codeAddr := cb.Addr()
	rfBase := state.RegFileBase()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callJIT(codeAddr, rfBase)
	}
}
```

**BenchmarkCallbackRoundTrip**:
```go
func BenchmarkCallbackRoundTrip(b *testing.B) {
	cb, err := NewCodeBuilder()
	if err != nil {
		b.Fatal(err)
	}
	defer cb.Free()

	cb.Callback(funcAddr(nosplitCallback))
	cb.Exit()

	state := NewState()
	codeAddr := cb.Addr()
	rfBase := state.RegFileBase()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callJIT(codeAddr, rfBase)
	}
}
```

### 5d. New tests

**TestRBPValid** — verify RBP is loaded with regFileBase:
```go
func TestRBPValid(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	// Store RBP itself to [RBP+0] via MOV RAX,RBP; MOV [RBP+0],RAX
	cb.MovRegReg(RAX, RBP)    // RAX = RBP
	cb.StoreToRBP(RAX, 0)     // X[0] = RAX (= RBP = regFileBase)
	cb.Exit()

	state := NewState()
	rfBase := state.RegFileBase()
	callJIT(cb.Addr(), rfBase)

	if state.X[0] != uint64(rfBase) {
		t.Errorf("RBP = 0x%X, want 0x%X", state.X[0], rfBase)
	}
}
```

**TestRBPPreservedAcrossCallback** — verify RBP survives Go callback:
```go
func TestRBPPreservedAcrossCallback(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	// Store RBP before callback
	cb.MovRegReg(RAX, RBP)
	cb.StoreToRBP(RAX, 0)  // X[0] = RBP before

	callbackFlag = false
	cb.Callback(funcAddr(nosplitCallback))

	// Store RBP after callback
	cb.MovRegReg(RAX, RBP)
	cb.StoreToRBP(RAX, 8)  // X[1] = RBP after

	cb.Exit()

	state := NewState()
	rfBase := state.RegFileBase()
	callJIT(cb.Addr(), rfBase)

	if !callbackFlag {
		t.Fatal("callback not called")
	}
	if state.X[0] != uint64(rfBase) {
		t.Errorf("RBP before callback: 0x%X, want 0x%X", state.X[0], rfBase)
	}
	if state.X[1] != uint64(rfBase) {
		t.Errorf("RBP after callback: 0x%X, want 0x%X", state.X[1], rfBase)
	}
}
```

**TestStateLayout** — verify offsets match shadow register file:
```go
func TestStateLayout(t *testing.T) {
	var s State
	checks := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"X", unsafe.Offsetof(s.X), 0},
		{"F", unsafe.Offsetof(s.F), 256},
		{"FCSR", unsafe.Offsetof(s.FCSR), 512},
		{"MemBase", unsafe.Offsetof(s.MemBase), 520},
		{"MemMask", unsafe.Offsetof(s.MemMask), 528},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s offset = %d, want %d", c.name, c.got, c.want)
		}
	}
}
```

**TestLoadFromRBP** — load a pre-set register value:
```go
func TestLoadFromRBP(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	// Load X[5] into RAX, store to X[0]
	cb.LoadFromRBP(RAX, 5*8) // RAX = X[5]
	cb.StoreToRBP(RAX, 0)    // X[0] = RAX
	cb.Exit()

	state := NewState()
	state.X[5] = 0x42
	callJIT(cb.Addr(), state.RegFileBase())

	if state.X[0] != 0x42 {
		t.Errorf("X[0] = 0x%X, want 0x42", state.X[0])
	}
}
```

**TestAddReg** — emit ADD between two loaded registers:
```go
func TestAddReg(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	// X[0] = X[10] + X[11]
	cb.LoadFromRBP(RAX, 10*8) // RAX = X[10]
	cb.LoadFromRBP(RCX, 11*8) // RCX = X[11]
	cb.AddReg(RAX, RCX)       // RAX += RCX
	cb.StoreToRBP(RAX, 0)     // X[0] = RAX
	cb.Exit()

	state := NewState()
	state.X[10] = 100
	state.X[11] = 200
	callJIT(cb.Addr(), state.RegFileBase())

	if state.X[0] != 300 {
		t.Errorf("X[0] = %d, want 300", state.X[0])
	}
}
```

**TestRunAPI** — use the public `Run` function:
```go
func TestRunAPI(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	// X[0] = X[1] + X[2]
	cb.LoadFromRBP(RAX, 1*8)
	cb.LoadFromRBP(RCX, 2*8)
	cb.AddReg(RAX, RCX)
	cb.StoreToRBP(RAX, 0)
	cb.Exit()

	state := NewState()
	state.X[1] = 7
	state.X[2] = 35
	Run(cb, state)

	if state.X[0] != 42 {
		t.Errorf("X[0] = %d, want 42", state.X[0])
	}
}
```

**TestDumpAndVerify** — simplified, uses new API:
```go
func TestDumpAndVerify(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	state := NewState()
	stateAddr := uint64(state.RegFileBase())

	cb.Movabs(RAX, stateAddr)
	cb.Movabs(RCX, 0xDEADBEEFDEADBEEF)
	cb.emit(0x48, 0x89, 0x08) // MOV [RAX], RCX
	cb.Exit()

	t.Logf("code (%d bytes): % x", cb.Len(), cb.buf[:cb.Len()])

	callJIT(cb.Addr(), state.RegFileBase())
	if state.X[0] != 0xDEADBEEFDEADBEEF {
		t.Errorf("X[0] = 0x%X, want 0xDEADBEEFDEADBEEF", state.X[0])
	}
}
```

### 5e. Test file structure (final)

The test file `abjit_test.go` should contain, in order:
1. Imports (`testing`, `unsafe`)
2. Callback targets (`callbackFlag`, `nosplitCallback`, `gcCallback`)
3. Phase 0 tests (updated): TestBasicEntryExit, TestCallback,
   TestStoreNoCallback, TestAbsoluteStore, TestDumpAndVerify,
   TestCalleeSaveVerification, TestGCSafety, TestGCSafetyStress
4. Phase 2 new tests: TestRBPValid, TestRBPPreservedAcrossCallback,
   TestStateLayout, TestLoadFromRBP, TestAddReg, TestRunAPI
5. Benchmarks: BenchmarkTrampolineOverhead, BenchmarkCallbackRoundTrip

The `heapU64` function is REMOVED (replaced by `NewState()`).
The private `codeBuilder` struct is REMOVED (replaced by exported
`CodeBuilder` in emit.go).

---

## Step 6: Verification

### 6a. Build

```bash
cd ~/ris/abjit && go build .
```

### 6b. Run all tests

```bash
cd ~/ris/abjit && go test -v
```

Expected: all tests pass. The CALL R10 pattern scan in `init()` finds the
pattern in the new trampoline. RBP is correctly loaded as regFileBase.

### 6c. Benchmarks

```bash
cd ~/ris/abjit && go test -run='^$' -bench=. -benchtime=3s
```

Expected: BenchmarkTrampolineOverhead ≈ 2-3ns (possibly faster than
Phase 0 since we removed R9/R12 setup instructions).
BenchmarkCallbackRoundTrip ≈ 3-4ns (faster since gocall handler is now
2 instructions instead of 3).

### 6d. Root package regression (abjit is not imported by root)

```bash
cd ~/ris && go test -count=1 -timeout 300s .
```

Must still pass — Phase 2 changes only `~/ris/abjit/`, which is not
imported by the root package.

---

## Files summary

| File | Action | Description |
|------|--------|-------------|
| `~/ris/abjit/trampoline_amd64.s` | Rewrite | 2-arg, RBP=regFileBase, minimal gocall |
| `~/ris/abjit/trampoline.go` | Modify | `callJIT(code, regFileBase uintptr)` |
| `~/ris/abjit/callfunc.go` | No change | CALL R10 scan still works |
| `~/ris/abjit/mmap_unix.go` | No change | Used by CodeBuilder |
| `~/ris/abjit/emit.go` | Create | Exported CodeBuilder |
| `~/ris/abjit/abjit.go` | Create | State struct, NewState, Run |
| `~/ris/abjit/abjit_test.go` | Rewrite | All tests updated + new tests |

## What this does NOT do

- Does not create `LowerAMD64_ABJIT` lowerer (Phase 3)
- Does not wire `PolicyABJIT.Lower` (Phase 3)
- Does not change anything in `ir/` or root package
- Does not add inline mask-based memory access (Phase 3, in the lowerer)
