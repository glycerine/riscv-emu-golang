# Plan: Fix ELS Liveness + Comprehensive Test Coverage

## Context

The `[0, n-1]` guest-reg merge is a conservative hack that defeats ELS's
performance advantage. The paper (Sarkar & Barik, Section 2.3) explicitly
allows holes in interval sets — that's the source of ELS's space efficiency
(SCF of 4.5–22.7% vs interference graphs). The paper assumes correct
liveness as input (Figure 1, input #3). Our bug is in the liveness
computation, not in the ELS algorithm itself.

### The actual bug

`computeIntervalSets()` does a linear backward scan. This correctly handles:
- Linear code (uses/defs in sequence)
- Backward branches (loops) via `extendLoopLiveRanges()`

But it does NOT handle **forward conditional branches**. When a forward branch
at instruction i targets a label at instruction t > i, any VReg live at t must
also be live at i (because the branch can transfer control from i to t). The
backward scan doesn't know this because it scans sequentially — it never
follows the forward edge from i to t.

**Concrete example from the failing block**: Guest reg x13 is loaded at
instruction 4, used at instruction 47, and used by WriteBackAll at instruction
260 (fault handler). The OOB-check branch at instruction 17 targets the fault
handler. x13 has intervals [4,4], [47,51], [141,260+] — with a gap at [5,46].
The gap allows a temp (t72) to take x13's register (R8). When the OOB branch
at instruction 17 is taken, the fault handler stores R8 (now holding t72's
value) as x13 — wrong. Even on the non-taken path, x13 is read at instruction
47 from R8, which was clobbered by t72.

The fix: x13 must be live at instruction 17 because the forward branch can
reach instruction 260 where x13 is used. Adding a forward-branch extension
produces interval [17, 260] which merges with [47,51] and [141,260] into
[17, 260+], eliminating the problematic gap.

## Part 1: Fix liveness computation

### Step 1: Replace `[0,n-1]` merge with proper forward branch extension

**File: `ir/regalloc.go`**

Revert the `[0,n-1]` merge at lines 492-506 back to the original
last-interval extension:

```go
// Guest regs (1-63): extend last interval's End to len(Instrs)-1.
for vr := 1; vr <= 63 && vr < len(result); vr++ {
    ivals := result[vr].Intervals
    if len(ivals) > 0 {
        ivals[len(ivals)-1].End = n - 1
    }
}
```

### Step 2: Add `extendForwardBranchRanges()`

**File: `ir/regalloc.go`**, after `extendLoopLiveRanges()` call (line 468)

Add a new function that finds forward conditional branches and extends
intervals of VRegs live at the branch target backward to the branch point:

```go
func extendForwardBranchRanges(b *Block, result []intervalSet) {
    for i := range b.Instrs {
        ins := &b.Instrs[i]
        // Only conditional branches create forward edges that matter.
        // Unconditional jumps don't — the code after them is unreachable.
        var targetLabel Label
        switch ins.Op {
        case IRBranch, IRBranchImm:
            targetLabel = Label(ins.Imm)
        default:
            continue
        }
        targetIdx, ok := b.Labels[targetLabel]
        if !ok || targetIdx <= i {
            continue // backward or unknown — handled by extendLoopLiveRanges
        }
        // Forward conditional branch from i to targetIdx.
        // Any VReg live at targetIdx must also be live at i.
        for vr := range result {
            if result[vr].VReg == VRegZero { continue }
            for _, iv := range result[vr].Intervals {
                if iv.Start <= targetIdx && targetIdx <= iv.End {
                    result[vr].Intervals = append(result[vr].Intervals,
                        Interval{VReg: result[vr].VReg, Start: i, End: targetIdx})
                    break
                }
            }
        }
    }
}
```

Call it in `computeIntervalSets()` right after `extendLoopLiveRanges(b, result)`:

```go
extendLoopLiveRanges(b, result)
extendForwardBranchRanges(b, result)
```

The subsequent sort+merge step (lines 470-490) will unify overlapping intervals.

### Why this is faithful to the paper

The paper (Figure 4, Section 2.3) takes I(s) — interval sets with holes —
as input. It does NOT specify how to compute them; it just assumes correct
liveness. Our backward scan + loop extension + **forward branch extension**
is a correct liveness computation for our IR's control flow patterns:

- Linear code: backward scan handles it
- Loops (backward branches): `extendLoopLiveRanges` handles it
- If/else (forward conditional branches): `extendForwardBranchRanges` handles it

Guest registers CAN have holes between redefinitions. Temps CAN have holes.
This preserves ELS's space efficiency while producing correct code.

## Part 2: Test coverage (ir/els_test.go, ir/els_fuzz_test.go)

### ir/els_test.go — Unit tests

Use existing helpers: `makeBlock()`, `testPool()`, `assertNoConflicts()`.

**Test 1: TestELS_ForwardBranch_GuestRegLive**
The bug case. Guest reg x5 loaded at [0], used at [20], forward branch at [10]
targeting label at [15] where WriteBackAll stores x5. Verify x5's interval
covers [0, 20+] with no gap at [10] (the forward branch point).

**Test 2: TestELS_ForwardBranch_TempNotExtended**
Temp t64 defined at [5], used at [6]. Forward branch at [3] targets [10].
t64 is NOT live at [10]. Verify t64's interval stays [5,6] — not extended.

**Test 3: TestELS_ForwardBranch_MultipleTargets**
Two forward branches targeting different labels. Guest reg live at both targets.
Verify intervals extended for both.

**Test 4: TestELS_GuestRegHole_BetweenRedefs**
Guest reg x5 defined at [0], used at [10], redefined at [15], used at [25].
Intervals: [0,10] + [15,n-1]. Hole at [11,14] is VALID — different values.
Verify a temp CAN use x5's register during [11,14].

**Test 5: TestELS_BackwardBranch_LoopExtension**
Backward branch (loop). VRegs in loop body extended across full loop range.

**Test 6: TestELS_MaskedLoadPattern**
Build block via Emitter with MaskedLoad + fault handler (the real-world
pattern). Run both ELS and Fixed. `assertNoConflicts()` on both.

**Test 7: TestELS_TwoMaskedLoads**
Two MaskedLoad calls in same block. Many temps + guest regs. No conflicts.

**Test 8: TestELS_SpillWithHoles**
More VRegs than pool size. Some VRegs have holes. Verify spilled VRegs have
unique stack slots. Non-spilled VRegs have no conflicts.

**Test 9: TestELS_PinnedRegsExcluded**
Pinned VRegs (t64-t68) don't appear in allocation pool. No non-pinned VReg
gets a pinned host register.

**Test 10: TestELS_MatchesFixed_Execution**
Compile same block with ELS and Fixed via `execBlock()`. Compare register
outputs. Both must produce identical results.

### ir/els_fuzz_test.go — Fuzz tests

**Fuzz 1: FuzzELS_NoConflicts**
Random IR blocks with branches/labels/loads/stores. Pool with 3-5 regs.
Invariants: no conflicts, all referenced VRegs allocated, unique spill slots.

**Fuzz 2: FuzzELS_ForwardBranchLiveness**
Random blocks with forward branches. For each forward branch from i to t,
verify every VReg live at t also has an interval covering i.

**Fuzz 3: FuzzELS_GuestRegExtension**
Random blocks with guest regs 1-8. Verify last interval of each used guest
reg extends to n-1.

**Fuzz 4: FuzzELS_MatchesExecution**
Random arithmetic blocks. Compile with ELS and Fixed, execute both,
compare register state.

**Fuzz 5: FuzzELS_SpillSlotUniqueness**
High-pressure blocks. Verify spill slots unique and in [0, StackSlots).

## Verification

```bash
# Fix
go test -v -run 'TestELS_vs_Fixed_Long' -timeout 30s ./bench/
go test -run='^$' -bench='^BenchmarkCPU_FullExecution_JIT$' -benchtime=1x ./bench/

# Unit tests
go test -v -run 'TestELS_' -count=1 ./ir/

# Fuzz tests
go test -fuzz 'FuzzELS_' -fuzztime=10s ./ir/

# Existing tests
go test -count=1 ./ir/...
go test -count=1 -run 'TestJIT_' .
```

## Files to Modify

| File | Change |
|------|--------|
| `ir/regalloc.go` | Revert [0,n-1] merge; add extendForwardBranchRanges(); restore last-interval extension |
| `ir/els_test.go` | New: unit tests for ELS edge cases |
| `ir/els_fuzz_test.go` | New: fuzz tests for ELS invariants |
