# Plan: Fix ELS Register Allocator — Wrong Code at Block 612061

## Context

The ELS (Extended Linear Scan) allocator in `ir/regalloc.go` produces incorrect
code for the bench_guest main loop block at PC=0x1050. At block 612061, ELS exits
with IC=4086, nextPC=0x103c while Fixed exits with IC=4097, nextPC=0x107c. The ELS
version takes a wrong branch, sending execution backward into an infinite loop.
The Fixed Static allocator produces correct code for all blocks.

The test `bench/els_diag_test.go:TestELS_vs_Fixed_Long` demonstrates the failure.

## Root Cause Analysis

The ELS allocator's `computeIntervalSets()` (line 388) uses a backward scan to
compute live ranges, then extends them with `extendLoopLiveRanges()` (line 509).
The extension ONLY handles backward edges (loops). Forward branches (OOB checks,
alignment checks) are explicitly skipped at line 525-526:

```go
if !ok || targetIdx >= i {
    continue // forward edge or unknown label
}
```

This is incorrect. When a conditional forward branch exists (e.g., the alignment
check `branch.ne t70, v0 -> L5`), VRegs that are defined BEFORE the branch and
used AFTER the branch target may have their intervals split. Since guest registers
(VRegs 1-63) are extended to block-end (lines 492-498), they survive. But **temp
VRegs** used on both the fall-through path AND the taken path (or across forward
branches to shared fault handlers) can have interval gaps.

However, the exact mechanism by which this produces a wrong branch outcome needs
to be confirmed by examining the IR and allocations for the specific failing block.

## Implementation: Diagnose-then-fix

### Step 1: Write diagnostic test to dump IR and allocations

**File: `bench/els_diag_test.go`** — add `TestELS_DumpFailingBlock`

The test should:
1. Load bench_guest ELF, run 612060 blocks with Fixed allocator to reach PC=0x1050
2. Call `emitBlock(&cpu.mem, 0x1050)` to get the IR
3. Run both ELS and Fixed allocators on the same IR block
4. Log:
   - Full IR listing (all instructions)
   - ELS allocation: for each VReg, its Kind (Reg/Stack), host register, intervals
   - Fixed allocation: same
   - Diff: VRegs with different allocations
5. Compile both with `jitCompileDebug` and dump Prog listings
6. Find the first diverging instruction in the Prog listings

This will pinpoint the exact VReg with the wrong register assignment.

### Step 2: Fix the root cause

Based on diagnostic output, one of these fixes applies:

**Fix A: Extend intervals across forward branches (most likely needed)**

In `computeIntervalSets()`, after `extendLoopLiveRanges()`, add forward branch
handling. For each forward conditional branch, any VReg LIVE at the branch point
must remain live through the branch target. This ensures the register isn't reused
on the fall-through path before the branch target is reached.

The implementation: scan for forward branches and extend live VReg intervals to
cover the branch target index:

```go
// After extendLoopLiveRanges(b, result), add:
extendForwardBranchRanges(b, result)
```

Where `extendForwardBranchRanges` finds conditional branches to later labels
and extends intervals of all VRegs live at the branch point to the target.

**Fix B: Conservative interval extension for temps across branches**

Simpler alternative: for any VReg that has an interval touching a branch
instruction, extend the interval to cover the branch target. This is
conservative but safe.

**Fix C: Full dataflow-based liveness**

Replace the simple backward scan with proper iterative dataflow analysis that
handles forward and backward edges. This is the "correct" solution but much
more complex.

### Step 3: Verify

```bash
# The failing test should pass
go test -v -run TestELS_vs_Fixed_Long -timeout 30s ./bench/

# ELS benchmark should complete
go test -run='^$' -bench='^BenchmarkCPU_FullExecution_JIT$' -benchtime=1x ./bench/

# All JIT tests still pass
go test -run 'TestJIT_' .

# IR tests still pass
go test ./ir/...
```

## Files to Modify

| File | Change |
|------|--------|
| `bench/els_diag_test.go` | Add TestELS_DumpFailingBlock diagnostic |
| `ir/regalloc.go` | Fix interval computation for forward branches |

## Risks

1. **Diagnostic may reveal a different root cause**: The forward branch hypothesis
   is based on analysis, not confirmed. The diagnostic test will confirm or refute.
2. **Performance**: Extending intervals makes more VRegs compete for registers,
   potentially increasing spill count. But correctness > performance.
3. **Fix A complexity**: Forward branch handling needs to identify which VRegs are
   live at the branch point, which requires computing liveness at that point.
