# Phase 1: Pluggable RegPolicy — Detailed Implementation Plan

## Goal

Add `RegPolicy` struct to `ir/` that bundles Pool + Pinned + Lower into
one switchable unit. Wire it into the JIT struct so all compilation goes
through `j.regPolicy` instead of hardcoded `RV8Pool`/`RV8Pinned`/
`LowerAMD64_RV8` calls. Default to `PolicyRV8`. Zero behavior change —
all existing tests must pass unchanged.

Also add `ABJITPool()`, `ABJITPinned()`, and `PolicyABJIT` (with nil
Lower — the lowerer is a later phase).

**Scope**: 5 files modified, 0 created. ~50 lines of new code, ~15 lines
of changed code, ~60 lines of new tests.

---

## Step 1: Add RegPolicy type + ABJITPool + ABJITPinned to ir/lower_amd64.go

**File**: `~/ris/ir/lower_amd64.go` (currently 55 lines)

### 1a. Add RegPolicy struct after the VRRegFile const (after line 18)

Insert between line 18 (`const VRRegFile = ...`) and line 20 (`// RV8Pool`):

```go
// RegPolicy bundles register allocation choices for a target configuration.
// Pool, Pinned, and Lower must all be non-nil before the policy is used
// for compilation.
type RegPolicy struct {
	Name   string
	Pool   func(*Block) RegPool
	Pinned func() map[VReg]int16
	Lower  func(*goasm.Ctx, *Block, *Allocation) (*LowerResult, error)
}
```

**Why these fields**:
- `Pool`: returns which host registers are available for guest mapping.
  Takes `*Block` because some policies may vary pool based on block
  contents (e.g., shrink pool if block uses MUL/DIV).
- `Pinned`: returns which VRegs are locked to specific host registers.
  No arguments because pinning is configuration-level, not block-level.
- `Lower`: the code generator. Consumes allocation output, emits native
  code into a goasm.Ctx. Different policies have different prologue/
  epilogue sequences, memory access patterns, and callback support.

### 1b. Add PolicyRV8 variable after RV8Pinned function (after line 55)

```go
// PolicyRV8 is the rv8-faithful register policy: 12-register pool,
// RBP pinned to VRRegFile, LowerAMD64_RV8 lowerer.
var PolicyRV8 = RegPolicy{
	Name:   "rv8",
	Pool:   RV8Pool,
	Pinned: RV8Pinned,
	Lower:  LowerAMD64_RV8,
}
```

### 1c. Add ABJITPool function after PolicyRV8

```go
// ABJITPool returns the 11-register integer allocation pool for the abjit
// trampoline path. Excludes R14 (Go goroutine pointer, unsafe when JIT
// code can trigger Go callbacks). All other differences from RV8Pool are
// the same: RAX/RCX staging, RBP pinned, RSP reserved.
func ABJITPool(_ *Block) RegPool {
	intRegs := []int16{
		goasm.REG_AMD64_DX,
		goasm.REG_AMD64_BX,
		goasm.REG_AMD64_SI,
		goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8,
		goasm.REG_AMD64_R9,
		goasm.REG_AMD64_R10,
		goasm.REG_AMD64_R11,
		goasm.REG_AMD64_R12,
		goasm.REG_AMD64_R13,
		goasm.REG_AMD64_R15,
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}
```

### 1d. Add ABJITPinned function after ABJITPool

```go
// ABJITPinned returns the pinned VReg → host register map for abjit.
// Same as RV8Pinned: only VRRegFile → RBP.
func ABJITPinned() map[VReg]int16 {
	return map[VReg]int16{
		VRRegFile: goasm.REG_AMD64_BP,
	}
}
```

### 1e. Add PolicyABJIT variable after ABJITPinned

```go
// PolicyABJIT is the abjit register policy: 11-register pool (no R14),
// RBP pinned to VRRegFile. Lower is nil until lower_amd64_abjit.go exists.
var PolicyABJIT = RegPolicy{
	Name:   "abjit",
	Pool:   ABJITPool,
	Pinned: ABJITPinned,
	// Lower: nil — set when lower_amd64_abjit.go is implemented.
}
```

### Exact edit operations for Step 1

**Edit 1**: Insert RegPolicy type. Find this text in the file:

```
const VRRegFile = VReg(VRegTempStart + 5) // t69

// RV8Pool returns the 12-register integer allocation pool
```

Replace with:

```
const VRRegFile = VReg(VRegTempStart + 5) // t69

// RegPolicy bundles register allocation choices for a target configuration.
// Pool, Pinned, and Lower must all be non-nil before the policy is used
// for compilation.
type RegPolicy struct {
	Name   string
	Pool   func(*Block) RegPool
	Pinned func() map[VReg]int16
	Lower  func(*goasm.Ctx, *Block, *Allocation) (*LowerResult, error)
}

// RV8Pool returns the 12-register integer allocation pool
```

**Edit 2**: Append after end of file. Find the closing brace of RV8Pinned:

```
func RV8Pinned() map[VReg]int16 {
	return map[VReg]int16{
		VRRegFile: goasm.REG_AMD64_BP,
	}
}
```

Replace with:

```
func RV8Pinned() map[VReg]int16 {
	return map[VReg]int16{
		VRRegFile: goasm.REG_AMD64_BP,
	}
}

// PolicyRV8 is the rv8-faithful register policy: 12-register pool,
// RBP pinned to VRRegFile, LowerAMD64_RV8 lowerer.
var PolicyRV8 = RegPolicy{
	Name:   "rv8",
	Pool:   RV8Pool,
	Pinned: RV8Pinned,
	Lower:  LowerAMD64_RV8,
}

// ABJITPool returns the 11-register integer allocation pool for the abjit
// trampoline path. Excludes R14 (Go goroutine pointer, unsafe when JIT
// code can trigger Go callbacks). All other differences from RV8Pool are
// the same: RAX/RCX staging, RBP pinned, RSP reserved.
func ABJITPool(_ *Block) RegPool {
	intRegs := []int16{
		goasm.REG_AMD64_DX,
		goasm.REG_AMD64_BX,
		goasm.REG_AMD64_SI,
		goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8,
		goasm.REG_AMD64_R9,
		goasm.REG_AMD64_R10,
		goasm.REG_AMD64_R11,
		goasm.REG_AMD64_R12,
		goasm.REG_AMD64_R13,
		goasm.REG_AMD64_R15,
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

// ABJITPinned returns the pinned VReg → host register map for abjit.
// Same as RV8Pinned: only VRRegFile → RBP.
func ABJITPinned() map[VReg]int16 {
	return map[VReg]int16{
		VRRegFile: goasm.REG_AMD64_BP,
	}
}

// PolicyABJIT is the abjit register policy: 11-register pool (no R14),
// RBP pinned to VRRegFile. Lower is nil until lower_amd64_abjit.go exists.
var PolicyABJIT = RegPolicy{
	Name:   "abjit",
	Pool:   ABJITPool,
	Pinned: ABJITPinned,
}
```

### Checkpoint 1

```bash
cd ~/ris && go build ./ir/
```

Must compile with zero errors. If `LowerResult` is not found, check that
`lower_amd64_shared.go` is in the same package (it is — type defined at
line 126 of that file).

---

## Step 2: Add regPolicy field to JIT struct

**File**: `~/ris/jit.go`

### 2a. Add field after irAlloc (line 225)

Find:

```
	irAlloc ir.RegAllocator

	// Dispatch counters (for diagnostics).
```

Replace with:

```
	irAlloc   ir.RegAllocator
	regPolicy ir.RegPolicy

	// Dispatch counters (for diagnostics).
```

### 2b. Initialize in NewJIT (line 239-243)

Find:

```
func NewJIT() *JIT {
	return &JIT{
		noJIT:   make(map[uint64]bool),
		irAlloc: ir.NewFixedStaticAllocator(),
	}
}
```

Replace with:

```
func NewJIT() *JIT {
	return &JIT{
		noJIT:     make(map[uint64]bool),
		irAlloc:   ir.NewFixedStaticAllocator(),
		regPolicy: ir.PolicyRV8,
	}
}
```

### 2c. Add SetRegPolicy method after SetAllocStrategy (after line 253)

Find:

```
// NoJITSize returns the number of PCs in the noJIT set (translation failures).
func (j *JIT) NoJITSize() int { return len(j.noJIT) }
```

Replace with:

```
// SetRegPolicy switches the register allocation policy and clears
// cached blocks (they were compiled with the old policy).
func (j *JIT) SetRegPolicy(p ir.RegPolicy) {
	j.regPolicy = p
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.noJIT = make(map[uint64]bool)
}

// NoJITSize returns the number of PCs in the noJIT set (translation failures).
func (j *JIT) NoJITSize() int { return len(j.noJIT) }
```

### 2d. Copy regPolicy in CloneShared (line 341-351)

Find:

```
	child := &JIT{
		aotSegments: append([]*DecodedExecuteSegment(nil), j.aotSegments...),
		noJIT:       make(map[uint64]bool),
		irAlloc:     j.irAlloc, // stateless; sharing is safe
	}
```

Replace with:

```
	child := &JIT{
		aotSegments: append([]*DecodedExecuteSegment(nil), j.aotSegments...),
		noJIT:       make(map[uint64]bool),
		irAlloc:     j.irAlloc,   // stateless; sharing is safe
		regPolicy:   j.regPolicy, // struct copy; function pointers are safe to share
	}
```

### Checkpoint 2

```bash
cd ~/ris && go build .
```

Must compile. The `regPolicy` field is used nowhere yet — that's Step 3.

---

## Step 3: Replace hardcoded RV8 calls in jit_native.go

**File**: `~/ris/jit_native.go`

### 3a. jitCompile method (lines 34-41)

Find:

```
	pool := ir.RV8Pool(res.block)
	pinned := ir.RV8Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := getJITCtx()
	ctx.Append(ctx.NewATEXT())

	lowerResult, lowerErr := ir.LowerAMD64_RV8(ctx, res.block, alloc)
```

Replace with:

```
	pool := j.regPolicy.Pool(res.block)
	pinned := j.regPolicy.Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := getJITCtx()
	ctx.Append(ctx.NewATEXT())

	lowerResult, lowerErr := j.regPolicy.Lower(ctx, res.block, alloc)
```

### 3b. jitCompileDebug method (lines 150-157)

Find:

```
	pool := ir.RV8Pool(res.block)
	pinned := ir.RV8Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	lowerResult, lowerErr := ir.LowerAMD64_RV8(ctx, res.block, alloc)
```

Replace with:

```
	pool := j.regPolicy.Pool(res.block)
	pinned := j.regPolicy.Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	lowerResult, lowerErr := j.regPolicy.Lower(ctx, res.block, alloc)
```

### 3c. Remove unused ir import if needed

After these changes, `jit_native.go` no longer calls `ir.RV8Pool`,
`ir.RV8Pinned`, or `ir.LowerAMD64_RV8` directly. But it still uses
`ir.LowerResult` (implicitly through `lowerResult`) and `ir.Block`
(through `res.block`). The `"riscv/ir"` import stays.

Check: does the file still reference any `ir.` symbol? Yes — the
`j.irAlloc` field is type `ir.RegAllocator`, the block is `*ir.Block`.
The import stays.

### Checkpoint 3

```bash
cd ~/ris && go build .
```

Must compile. Behavior is identical — `j.regPolicy` is `PolicyRV8`, which
calls the exact same functions as before.

---

## Step 4: Replace hardcoded RV8 calls in jit_aot.go

**File**: `~/ris/jit_aot.go`

### 4a. AOT compilation loop (lines 55-61)

Find:

```
		pool := ir.RV8Pool(res.block)
		pinned := ir.RV8Pinned()
		alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

		ctx := goasm.New(goasm.AMD64)
		ctx.Append(ctx.NewATEXT())
		lowerResult, lowerErr := ir.LowerAMD64_RV8(ctx, res.block, alloc)
```

Replace with:

```
		pool := j.regPolicy.Pool(res.block)
		pinned := j.regPolicy.Pinned()
		alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

		ctx := goasm.New(goasm.AMD64)
		ctx.Append(ctx.NewATEXT())
		lowerResult, lowerErr := j.regPolicy.Lower(ctx, res.block, alloc)
```

### Checkpoint 4

```bash
cd ~/ris && go build .
```

---

## Step 5: Add tests

**File**: `~/ris/ir/lower_amd64_test.go`

Add these tests after the existing `TestRV8Pinned` function (around line
600). Follow the exact style of the existing `TestRV8Pool` and
`TestRV8Pinned` tests.

### 5a. TestABJITPool

```go
func TestABJITPool(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	}
	b.maxVreg = MaxVReg(b)
	pool := ABJITPool(b)

	if len(pool.IntRegs) != 11 {
		t.Fatalf("want 11 int regs, got %d", len(pool.IntRegs))
	}
	if len(pool.FPRegs) != 14 {
		t.Errorf("want 14 FP regs, got %d", len(pool.FPRegs))
	}

	// R14 must NOT be in the pool (Go goroutine pointer).
	for _, r := range pool.IntRegs {
		if r == goasm.REG_AMD64_R14 {
			t.Error("pool must not contain R14 (Go goroutine pointer)")
		}
	}

	// RAX, RCX, RBP, RSP must not be in the pool.
	excluded := map[int16]string{
		goasm.REG_AMD64_AX: "RAX",
		goasm.REG_AMD64_CX: "RCX",
		goasm.REG_AMD64_BP: "RBP",
		goasm.REG_AMD64_SP: "RSP",
	}
	for _, r := range pool.IntRegs {
		if name, bad := excluded[r]; bad {
			t.Errorf("pool must not contain %s", name)
		}
	}

	// Verify all 11 expected registers are present.
	want := map[int16]bool{
		goasm.REG_AMD64_DX:  true,
		goasm.REG_AMD64_BX:  true,
		goasm.REG_AMD64_SI:  true,
		goasm.REG_AMD64_DI:  true,
		goasm.REG_AMD64_R8:  true,
		goasm.REG_AMD64_R9:  true,
		goasm.REG_AMD64_R10: true,
		goasm.REG_AMD64_R11: true,
		goasm.REG_AMD64_R12: true,
		goasm.REG_AMD64_R13: true,
		goasm.REG_AMD64_R15: true,
	}
	for _, r := range pool.IntRegs {
		if !want[r] {
			t.Errorf("unexpected register %d in pool", r)
		}
	}
}
```

### 5b. TestABJITPinned

```go
func TestABJITPinned(t *testing.T) {
	pinned := ABJITPinned()
	if len(pinned) != 1 {
		t.Fatalf("want 1 pinned VReg, got %d", len(pinned))
	}
	got, ok := pinned[VRRegFile]
	if !ok {
		t.Fatal("VRRegFile not in pinned map")
	}
	if got != goasm.REG_AMD64_BP {
		t.Errorf("VRRegFile pinned to %d, want RBP (%d)", got, goasm.REG_AMD64_BP)
	}
}
```

### 5c. TestPolicyRV8

```go
func TestPolicyRV8(t *testing.T) {
	p := PolicyRV8
	if p.Name != "rv8" {
		t.Errorf("name = %q, want %q", p.Name, "rv8")
	}
	if p.Pool == nil {
		t.Fatal("Pool is nil")
	}
	if p.Pinned == nil {
		t.Fatal("Pinned is nil")
	}
	if p.Lower == nil {
		t.Fatal("Lower is nil")
	}

	// Verify Pool returns the same result as RV8Pool.
	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	b.maxVreg = MaxVReg(b)
	pool := p.Pool(b)
	if len(pool.IntRegs) != 12 {
		t.Errorf("Pool().IntRegs = %d, want 12", len(pool.IntRegs))
	}

	// Verify Pinned returns the same result as RV8Pinned.
	pinned := p.Pinned()
	if len(pinned) != 1 {
		t.Errorf("Pinned() = %d entries, want 1", len(pinned))
	}
}
```

### 5d. TestPolicyABJIT

```go
func TestPolicyABJIT(t *testing.T) {
	p := PolicyABJIT
	if p.Name != "abjit" {
		t.Errorf("name = %q, want %q", p.Name, "abjit")
	}
	if p.Pool == nil {
		t.Fatal("Pool is nil")
	}
	if p.Pinned == nil {
		t.Fatal("Pinned is nil")
	}
	// Lower is nil until the ABJIT lowerer is implemented.
	if p.Lower != nil {
		t.Error("Lower should be nil (not yet implemented)")
	}

	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	b.maxVreg = MaxVReg(b)
	pool := p.Pool(b)
	if len(pool.IntRegs) != 11 {
		t.Errorf("Pool().IntRegs = %d, want 11", len(pool.IntRegs))
	}
}
```

### 5e. TestABJITPool_R14Excluded — explicit R14 exclusion test

```go
func TestABJITPool_R14Excluded(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	b.maxVreg = MaxVReg(b)

	rv8Pool := RV8Pool(b)
	abjitPool := ABJITPool(b)

	// rv8 includes R14, abjit excludes it.
	hasR14 := func(pool RegPool) bool {
		for _, r := range pool.IntRegs {
			if r == goasm.REG_AMD64_R14 {
				return true
			}
		}
		return false
	}
	if !hasR14(rv8Pool) {
		t.Error("RV8Pool should include R14")
	}
	if hasR14(abjitPool) {
		t.Error("ABJITPool must not include R14")
	}

	// abjit has exactly 1 fewer int reg than rv8.
	if len(abjitPool.IntRegs) != len(rv8Pool.IntRegs)-1 {
		t.Errorf("abjit=%d, rv8=%d, want abjit=rv8-1",
			len(abjitPool.IntRegs), len(rv8Pool.IntRegs))
	}
}
```

### Where to insert these tests

Find the end of `TestVRRegFile_Distinct` (around line 620 in
`lower_amd64_test.go`). Insert all 5 test functions after it, before
the next test function.

### Checkpoint 5

```bash
cd ~/ris && go test -v -run 'TestABJIT|TestPolicy' ./ir/
```

Expected output: 5 tests pass (TestABJITPool, TestABJITPinned,
TestPolicyRV8, TestPolicyABJIT, TestABJITPool_R14Excluded).

---

## Step 6: Full regression test

### 6a. ir/ package tests

```bash
cd ~/ris && go test -v ./ir/ 2>&1 | tail -5
```

ALL existing tests must pass. Key tests to watch:
- `TestRV8Pool` — still 12 regs ✓
- `TestRV8Pinned` — still VRRegFile→RBP ✓
- `TestLowerRV8_EmptyBlock` — lowering works ✓
- All `TestLower*` tests — no behavior change ✓

### 6b. Root package tests (includes JIT execution)

```bash
cd ~/ris && go test -count=1 -timeout 120s . 2>&1 | tail -5
```

This runs the full RISC-V test suite via JIT. Every block is now compiled
through `j.regPolicy.Pool/Pinned/Lower` instead of hardcoded functions.
Since `regPolicy` defaults to `PolicyRV8`, behavior is identical.

### 6c. Benchmark package

```bash
cd ~/ris && go test -count=1 -timeout 120s ./bench/ 2>&1 | tail -5
```

### 6d. abjit package (Phase 0 tests still pass)

```bash
cd ~/ris/abjit && go test -v
```

All 11 Phase 0 tests must pass. The abjit package is not touched in
Phase 1 — this is a smoke test.

### 6e. Quick performance check (should be identical)

```bash
cd ~/ris && go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=1x ./bench/
```

---

## Files changed (summary)

| File | Lines changed | What |
|------|--------------|------|
| `~/ris/ir/lower_amd64.go` | +~60 | RegPolicy type, PolicyRV8, ABJITPool, ABJITPinned, PolicyABJIT |
| `~/ris/jit.go` | +~10 | regPolicy field, init, SetRegPolicy, CloneShared copy |
| `~/ris/jit_native.go` | ~6 changed | 2 call sites: RV8Pool→regPolicy.Pool, etc. |
| `~/ris/jit_aot.go` | ~3 changed | 1 call site: same pattern |
| `~/ris/ir/lower_amd64_test.go` | +~95 | 5 new test functions |

## Files NOT changed

These files call `ir.RV8Pool`/`ir.RV8Pinned`/`ir.LowerAMD64_RV8` directly
in test code. They bypass the JIT struct and test the allocator/lowerer
directly. They must NOT be changed — the standalone functions still exist
and these tests verify them independently of the policy abstraction.

- `~/ris/ir/lower_amd64_test.go` (existing tests — only new tests added)
- `~/ris/ir/lower_amd64_chain_test.go`
- `~/ris/ir/lower_amd64_dcache_test.go`
- `~/ris/ir/lower_exhaust_test.go`
- `~/ris/lockstep_v1v2_test.go`
- `~/ris/jit_emit_ir_test.go`
- `~/ris/jit_bloat_test.go`
- `~/ris/bench/lower_bench_test.go`

## What this enables

After Phase 1, switching the JIT to abjit is:
```go
j := NewJIT()
j.SetRegPolicy(ir.PolicyABJIT) // when Lower is implemented
```

A/B benchmark comparison:
```go
j.SetRegPolicy(ir.PolicyRV8)
// benchmark...
j.SetRegPolicy(ir.PolicyABJIT)
// benchmark...
```

## What this does NOT do

- Does not create the ABJIT lowerer (Phase 2)
- Does not change the abjit trampoline (Phase 2)
- Does not add callback emission support (Phase 2)
- Does not touch any existing test files
- Does not change any runtime behavior (default = PolicyRV8 = identical)
