# Plan: Fix CMP Direction Bug in rv8Branch/rv8Set

## Context

The rv8 lowerer has a pre-existing CMP direction bug: `emitRM(CMPQ, ...)` and `emit2(CMPQ, ...)` compute reversed comparisons. This blocks extending CISC memory operands to comparison operations (`rv8Branch`, `rv8Set`), which currently use the narrow `spilledRegFileOff` workaround. The bug caused `1.0 + 2.0 = NaN` when `spilledMemOp` was applied to `rv8Branch` (the wrong CMP direction broke unboxF32's NaN-box checks).

## Root Cause

Go's Plan 9 assembler: `CMPQ From, To` computes **`From - To`** (reversed from SUB/ADD which compute `To - From`). This is documented: "CMP A, B computes A-B."

The three CMP emission patterns and their flag results:

| Helper | From | To | Flags = From - To | Used by |
|--------|------|----|-------------------|---------|
| `emit2(CMPQ, aHR, bHR)` | A (reg) | B (reg) | **A - B** ✓ | rv8Branch, rv8Set |
| `emitCmpRI(a, imm)` | A (reg) | Imm (const) | **A - Imm** ✓ | rv8BranchImm, rv8SetImm |
| `emitRM(CMPQ, [B], aHR)` | B (mem) | A (reg) | **B - A** ✗ | rv8Branch, rv8Set (spilled B) |

The `emitRM` path puts B in `From` and A in `To`, computing `B - A`. But IR semantics require `A - B` (since `IRBranch` / `IRSet` are defined as `A pred B`). For `SETLT`/`JLT` (SF≠OF): the emit2 path correctly tests `A < B`, but the emitRM path tests `B < A`.

**Why it's been latent**: `spilledRegFileOff` only covers VRegs 1-31 (RISC-V integer regs). In practice, branch/set operands are usually in host registers (they're "hot" values), so the emitRM path almost never fires. When extended to `spilledMemOp` (covering temps ≥ 70), it fires on NaN-box checks in `unboxF32` and produces wrong results.

**Existing tests don't catch this**: `TestRV8Lower_Branch` uses `EQ` (symmetric — direction doesn't matter). `TestRV8Lower_Set` uses `LT` but only checks `len(code) == 0`, never executing the code or verifying the result.

---

## Step 1: Red Test — `TestRV8Set_CmpDirection`

**File**: `ir/lower_amd64_test.go`

### Step 1a: Baseline register-register test

Verify emit2 CMP direction is correct by testing asymmetric predicates with execution:

```go
func TestRV8Set_CmpDirection_RegReg(t *testing.T) {
    e := NewEmitter()
    e.Const(e.XReg(10), 3)
    e.Const(e.XReg(11), 7)
    e.Set(e.XReg(12), e.XReg(10), e.XReg(11), LT) // 3 < 7 → 1
    e.Set(e.XReg(13), e.XReg(11), e.XReg(10), LT) // 7 < 3 → 0
    e.Ret(0x1000, 0, VRegZero)
    var x [32]uint64
    execBlockRV8(t, e.Block, &x)
    if x[12] != 1 {
        t.Errorf("Set(3 LT 7) = %d, want 1", x[12])
    }
    if x[13] != 0 {
        t.Errorf("Set(7 LT 3) = %d, want 0", x[13])
    }
}
```

This should PASS (emit2 path is correct). Establishes the baseline.

### Step 1b: Spilled-B test (the red test)

Force register pressure so B spills, triggering the emitRM CMP path:

```go
func TestRV8Set_CmpDirection_SpilledB(t *testing.T) {
    e := NewEmitter()
    // Load 20 RISC-V regs to exhaust the 12-GPR pool.
    // With all 20 live across the Set, ≥8 must spill.
    for i := 1; i <= 20; i++ {
        e.Const(e.XReg(i), int64(i*100))
    }
    // x10=1000, x15=1500
    e.Set(e.XReg(21), e.XReg(10), e.XReg(15), LT) // 1000 < 1500 → 1
    e.Set(e.XReg(22), e.XReg(15), e.XReg(10), LT) // 1500 < 1000 → 0
    // Keep x1-x20 live past the comparison.
    e.Mov(e.XReg(25), e.XReg(1))
    for i := 2; i <= 20; i++ {
        e.Add(e.XReg(25), e.XReg(25), e.XReg(i))
    }
    e.Ret(0x1000, 0, VRegZero)
    var x [32]uint64
    execBlockRV8(t, e.Block, &x)
    if x[21] != 1 {
        t.Errorf("Set(x10=1000 LT x15=1500) = %d, want 1", x[21])
    }
    if x[22] != 0 {
        t.Errorf("Set(x15=1500 LT x10=1000) = %d, want 0", x[22])
    }
    // Verify sum to confirm other values are intact.
    var expectedSum uint64
    for i := 1; i <= 20; i++ {
        expectedSum += uint64(i * 100)
    }
    if x[25] != expectedSum {
        t.Errorf("sum = %d, want %d", x[25], expectedSum)
    }
}
```

**Expected result**: This test should FAIL if any of {x10, x15} is spilled and the emitRM CMP path fires with the wrong direction. If both happen to be in registers (allocator-dependent), it passes — in that case, add more pressure or try different register numbers.

### Step 1c: Branch direction test

Same pattern but using `Branch` to direct control flow:

```go
func TestRV8Branch_CmpDirection_SpilledB(t *testing.T) {
    e := NewEmitter()
    for i := 1; i <= 20; i++ {
        e.Const(e.XReg(i), int64(i*100))
    }
    trueLabel := e.NewLabel()
    endLabel := e.NewLabel()
    // x10=1000 < x15=1500 should branch to trueLabel
    e.Branch(e.XReg(10), e.XReg(15), LT, trueLabel)
    e.Const(e.XReg(21), 0) // false path
    e.Jump(endLabel)
    e.PlaceLabel(trueLabel)
    e.Const(e.XReg(21), 1) // true path
    e.PlaceLabel(endLabel)
    // Keep x1-x20 live
    e.Mov(e.XReg(25), e.XReg(1))
    for i := 2; i <= 20; i++ {
        e.Add(e.XReg(25), e.XReg(25), e.XReg(i))
    }
    e.Ret(0x1000, 0, VRegZero)
    var x [32]uint64
    execBlockRV8(t, e.Block, &x)
    if x[21] != 1 {
        t.Errorf("Branch(1000 LT 1500) took false path, x21=%d want 1", x[21])
    }
}
```

---

## Step 2: Fix — Swap CMP Operands in emitRM Sites

**File**: `ir/lower_amd64_rv8.go`

The fix is a one-line swap at each CMP emitRM call site. Replace `emitRM(CMPQ, [B], A)` with `emitMR(CMPQ, A, [B])`:

### rv8Set (lines 1170-1171, 1180-1181)

**Before**:
```go
lc.emitRM(x86.ACMPQ, goasm.REG_AMD64_BP, bOff, aHR)
```

**After**:
```go
lc.emitMR(x86.ACMPQ, aHR, goasm.REG_AMD64_BP, bOff)
```

This changes:
- From=A(reg), To=[B](mem) → flags = A - B ✓

There are **two** `emitRM(ACMPQ, ...)` calls in rv8Set:
1. Line 1171: `aHR >= 0` branch, B spilled
2. Line 1181: A staged to rv8StgA, B spilled

Both get the same swap.

### rv8Branch (lines 1308-1309, 1318-1319)

Same fix, same two call sites:
1. Line 1309: `aHR >= 0`, B spilled
2. Line 1319: A staged, B spilled

**Before**: `lc.emitRM(x86.ACMPQ, goasm.REG_AMD64_BP, bOff, aHR)`
**After**: `lc.emitMR(x86.ACMPQ, aHR, goasm.REG_AMD64_BP, bOff)`

### Verification that emitMR works for CMPQ

`emitMR(op, src, base, off)` sets `From=REG(src), To=MEM(base+off)`. For CMPQ:
- Go asm encodes as x86 opcode 39 (`CMP r/m64, r64`)
- Go's CMP semantics: `From - To` = `src - [base+off]`

This is the same encoding `emitRM` uses for loads (opcode 3B), but with swapped From/To. Go's assembler handles both forms correctly — `emitMR` with CMPQ is valid because CMP has both reg,r/m and r/m,reg encodings.

---

## Step 3: Extend rv8Branch/rv8Set to Use spilledMemOp

After the CMP direction fix, it's safe to extend the spill check from `spilledRegFileOff` (VRegs 1-31 only) to `spilledMemOp` (also covers temps ≥ 70 via `[RSP+slot*8]`).

**File**: `ir/lower_amd64_rv8.go`

In rv8Set and rv8Branch, replace:
```go
if bOff := lc.spilledRegFileOff(ins.B); bOff >= 0 {
    lc.emitMR(x86.ACMPQ, aHR, goasm.REG_AMD64_BP, bOff)
```
With:
```go
if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
    lc.emitMR(x86.ACMPQ, aHR, bBase, bOff)
```

This covers both `[RBP+r*8]` for RISC-V regs and `[RSP+slot*8]` for spilled temps.

**Note**: FP VRegs are already excluded by `spilledMemOp` (returns false for VRegs 32-63 and any VReg with an XMM host register), so FP comparisons are unaffected.

---

## Verification

```bash
# Step 1: Run the new red tests — should FAIL before fix
cd ~/ris && go test -run 'TestRV8Set_CmpDirection|TestRV8Branch_CmpDirection' -v ./ir/

# Step 2: Apply the emitMR fix, re-run — should PASS
cd ~/ris && go test -run 'TestRV8Set_CmpDirection|TestRV8Branch_CmpDirection' -v ./ir/

# Step 3: Extend to spilledMemOp, run full suite
cd ~/ris && go test ./...

# Step 4: Run bloat test to measure code size improvement
cd ~/ris && go test -run TestBloat_BenchGuest_0x10de -v .

# Step 5: Run benchmarks
cd ~/ris && make bench
```

## Key Files

| File | Lines | Change |
|------|-------|--------|
| `ir/lower_amd64_rv8.go` | 1170-1171, 1180-1181 | rv8Set: emitRM→emitMR for CMPQ |
| `ir/lower_amd64_rv8.go` | 1308-1309, 1318-1319 | rv8Branch: emitRM→emitMR for CMPQ |
| `ir/lower_amd64_rv8.go` | (same sites) | spilledRegFileOff→spilledMemOp |
| `ir/lower_amd64_test.go` | (new tests) | Red tests for CMP direction |

## Helpers Referenced

| Helper | Location | Purpose |
|--------|----------|---------|
| `emitRM(op, base, off, dst)` | line 721 | `From=[base+off], To=dst` — WRONG for CMP |
| `emitMR(op, src, base, off)` | line 732 | `From=src, To=[base+off]` — CORRECT for CMP |
| `spilledMemOp(v)` | line 495 | Returns (base, off, ok) for spilled integer VRegs |
| `spilledRegFileOff(v)` | line 478 | Narrow: only VRegs 1-31 in [RBP+r*8] |

## Commit Order

1. Add red tests (Step 1a, 1b, 1c) — verify 1b/1c fail
2. Fix CMP direction (Step 2) — verify all tests pass
3. Extend to spilledMemOp (Step 3) — verify all tests pass, measure code size
