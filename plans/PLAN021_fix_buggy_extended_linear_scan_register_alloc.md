# Fix JIT Register Allocator Loop Liveness + Lowerer Performance

## Context

Several riscv-tests fail under JIT: **sraw, srlw, sllw** produce wrong results (infinite loop until BudgetCheck), and **ld_st** hangs during lowering (>30s in `LowerAMD64`). Interpreter passes ALL tests — confirming the bugs are in the JIT pipeline, not instruction semantics.

Diagnostic test `TestNativeTrace_sraw` proves **V1 == V2** (both lowerers agree) but **both differ from interpreter** — the bug is in IR emission/register allocation, not the lowerer itself. Exhaustive lowerer tests (`TestExhaustive_SHR/SHL/SAR`) all pass.

## Bug 1: Loop Liveness — sraw, srlw, sllw fail

### Root Cause

`computeIntervalSets()` in `ir/regalloc.go:346` does a linear backward scan to build live ranges. It treats the IR as straight-line code and **does not extend live ranges across backward branches**.

Example from the sraw test, block 39 (PC 0x138-0x618, 402 RISC-V insns, 1649 IR insns):

```
IR idx K_top:  PlaceLabel loop_0x3ce       # loop header
...            lui x1, 0x80000000          # uses VReg(1), NOT VReg(4)
...            sraw x14, x1, x2           # writes VReg(14)
...            c.mv x6, x14               # reads VReg(14)
IR idx K_use:  c.addi x4, 1               # READS VReg(4), writes VReg(4)
...            c.li x5, 2
IR idx K_br:   Branch VReg(4),VReg(5),NE  # backward branch
               ...BudgetCheck...
               Jump loop_0x3ce             # backward edge to K_top
```

The backward scan computes VReg(4)'s live range as `[K_use, K_br]` — dead before `K_use`. Between `K_top` and `K_use`, VReg(4) is "dead" so its host register gets reused for VReg(14) (the SRAW result). When the backward branch jumps to `K_top`, VReg(4) holds 0xFFFFFFFFFF000000 (the SRAW result) instead of the loop counter.

Lockstep confirms: `x[4] = 0xffffffffff000001` (SRAW result + 1 from c.addi), interpreter `x[4] = 0x2` (correct loop counter).

### Fix

After `computeIntervalSets` builds initial intervals, add a **loop-extension pass**:

1. Scan IR for backward edges: any `IRJump` or `IRBranch`/`IRBranchImm` whose target label resolves to an earlier IR index
2. For each backward edge from source index S to target index T (T < S):
   - Collect all VRegs that have any use in `[T, S]`
   - For each such VReg, ensure its intervals cover the full range `[T, S]` by extending or merging intervals
3. This runs after the backward scan but before the merge step

This is the standard "loop liveness extension" for linear-scan register allocators. It's conservative (keeps all loop-touched VRegs alive for the entire loop body) but correct.

### Files to Modify

| File | Change |
|------|--------|
| `ir/regalloc.go` | Add `extendLoopLiveRanges(b *Block, result []intervalSet)` after backward scan, before merge |

### Algorithm Detail

```go
func extendLoopLiveRanges(b *Block, result []intervalSet) {
    for i, ins := range b.Instrs {
        // Find backward edges
        var targetLabel Label
        var isBranch bool
        switch ins.Op {
        case IRJump:
            targetLabel = Label(ins.Imm)
            isBranch = true
        case IRBranch, IRBranchImm:
            targetLabel = Label(ins.Imm)
            isBranch = true
        }
        if !isBranch { continue }

        targetIdx, ok := b.Labels[targetLabel]
        if !ok || targetIdx >= i { continue } // not backward

        // For each VReg with any use in [targetIdx, i],
        // extend intervals to cover [targetIdx, i]
        for j := targetIdx; j <= i; j++ {
            for _, vr := range instrUses(&b.Instrs[j]) {
                ensureCovered(result, vr, targetIdx, i)
            }
            def := instrDefs(&b.Instrs[j])
            if def != VRegZero {
                ensureCovered(result, def, targetIdx, i)
            }
        }
    }
}
```

Where `ensureCovered` extends or adds an interval for the VReg to cover `[T, S]`.

## Bug 2: O(N^2) Lowerer Performance — ld_st hangs

### Root Cause

`hostRegFor()` at `ir/lower_amd64.go:359` does a **linear scan** of `IntervalMap` for every `use()` and `def()` call. Similarly, `isVRegFP()` and `isCXLive()` scan `IntervalMap` linearly.

The ld_st test generates a block with **6700+ IR instructions** and a proportionally large IntervalMap. Each lowering step calls `use/def` ~2-3 times, each scanning the full IntervalMap:

- 6700 instructions × 3 calls × O(6700) scan = ~135 million iterations
- At ~5ns per iteration = ~0.7 seconds best case, much more with cache misses
- Stack trace confirms: hanging in `use()` → `hostRegFor()` at `lower_amd64.go:403`

### Fix

Build a **per-VReg lookup index** at the start of `LowerAMD64`, replacing the linear scan with O(1) lookup:

```go
type regLookup struct {
    entries []lookupEntry  // sorted by Start for binary search
}
type lookupEntry struct {
    start, end int
    host       int16
}
```

For each `hostRegFor(v, idx)` call: look up v's entries, binary search for the interval containing idx.

### Files to Modify

| File | Change |
|------|--------|
| `ir/lower_amd64.go` | Add `regLookup` type; build index in `LowerAMD64`; replace linear scan in `hostRegFor`, `isCXLive`, `isVRegFP` |

### Implementation

1. In `LowerAMD64`, after calling `Allocate`, build a `map[VReg][]lookupEntry` from `alloc.IntervalMap`
2. Sort each VReg's entries by Start
3. Replace `hostRegFor` linear scan with map lookup + binary search
4. Replace `isCXLive` with a precomputed bitset or similar O(1) check
5. Replace `isVRegFP` with a precomputed `map[VReg]bool`

## Verification

```bash
# After Bug 1 fix — sraw/srlw/sllw should pass:
go test -count=1 -run 'TestRISCVTests_Lockstep_UI/^sraw$' -timeout 30s -v .
go test -count=1 -run 'TestRISCVTests_UI_JIT/^sraw$' -timeout 30s -v .
go test -count=1 -run 'TestRISCVTests_UI_JIT/^srlw$' -timeout 30s -v .
go test -count=1 -run 'TestRISCVTests_UI_JIT/^sllw$' -timeout 30s -v .

# After Bug 2 fix — ld_st should complete:
go test -count=1 -run 'TestRISCVTests_UI_JIT/^ld_st$' -timeout 30s -v .
go test -count=1 -run 'TestRISCVTests_Lockstep_UI/^ld_st$' -timeout 30s -v .

# Full regression — all UI tests should pass:
go test -count=1 -run 'TestRISCVTests_UI_JIT' -timeout 120s -v .
go test -count=1 -run 'TestRISCVTests_Lockstep_UI' -timeout 120s -v .

# Exhaustive lowerer tests — should still pass:
go test -count=1 -run 'TestExhaustive' -timeout 60s -v ./ir/

# Diagnostic — V1==V2==interp:
go test -count=1 -run 'TestNativeTrace_sraw' -timeout 30s -v .

# Meta iteration order stress test:
go test -count=1 -run 'TestMetaIterOrder_AllUI' -timeout 120s -v .

# Benchmark — no regression:
make bench-quick
```

## Implementation Order

1. **Bug 1 first** (loop liveness) — this is the correctness fix, affects 3+ tests
2. **Bug 2 second** (performance) — this unblocks ld_st and any other large-block tests
3. Run full verification suite after each fix

## Key Files

| File | Role |
|------|------|
| `ir/regalloc.go:346` | `computeIntervalSets` — add loop extension pass |
| `ir/lower_amd64.go:359` | `hostRegFor` — replace linear scan with index lookup |
| `ir/lower_amd64.go:386` | `isVRegFP` — replace linear scan |
| `ir/lower_amd64.go:973` | `isCXLive` — replace linear scan |
| `jit_emit_ir.go:1882` | `emitBranch` — backward detection (reference, no changes needed) |
| `ir/highlevel.go:107` | `BudgetCheck` — backward branch budget check (reference, no changes) |
| `tools/disasm_riscv.py` | RISC-V disassembler utility for debugging (already saved) |
