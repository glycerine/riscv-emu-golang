# Fix Remaining JIT Bugs: sraw/srlw/sllw Lockstep + ld_st Hang

## Context

After fixing the R11 clobber bug in `lowerShift` (XCHG fix), the JIT now passes all 64-bit shift tests and exhaustive register-pair tests. Two classes of bugs remain:

1. **sraw/srlw/sllw lockstep failures** — W-variant (32-bit) shift tests fail in lockstep (JIT vs interpreter comparison). The JIT and interpreter produce different results.
2. **ld_st hang** — `rv64ui-p-ld_st` causes `StepBlock` to never return (infinite loop in native code).

Both the W-variant shift IR emission and interpreter semantics were audited and appear correct in isolation. The bugs are in the interaction between blocks, budget checks, or a subtle lowering edge case that only manifests with specific register allocations in large blocks.

## Bug 1: sraw/srlw Lockstep Failures

### Symptoms

```
sraw block 39 (pc=0x3ce, IC=4102):
  x[1] jit=0xffffffff80000000 interp=0x400
  x[3] jit=0x19               interp=0x1    ← test case 25 vs test case 1
```

JIT has run through 25 test cases; interpreter is still on test 1 after IC=4102 steps. This means the JIT executed the loop correctly (passing 24 tests, failing on 25), but the **interpreter got stuck or diverged early**.

### Root Cause Hypothesis

The lockstep test runs the interpreter for `jitIC` steps. If the interpreter encounters an error (ECALL, etc.) it breaks early (line 422-424 of `riscv_test.go`). If the interpreter finishes the test suite and ECALL's after only ~100 steps but the JIT looped for 4102 steps, the interpreter breaks at step 100, having only completed 1 test case, while the JIT completed 25.

**OR**: the JIT's W-variant IR produces a wrong result for one specific test value (not caught by the semantic audit because it depends on register allocation), causing the JIT's branch to diverge from the interpreter's.

### Investigation Plan

**Step 1**: Create `TestNativeTrace_sraw` — a focused diagnostic test:
- Load `rv64ui-p-sraw`
- Run JIT until block 39 (pc=0x3ce)
- Snapshot register state
- Execute block with V1, capture result
- Execute block with V2, compare V1 vs V2
- Execute same IC steps with interpreter, compare
- If V1==V2 but V1!=interp → interpreter bug or IC mismatch
- If V1!=V2 → lowerer bug (use Prog dump to find divergence)

**Step 2**: If V1==V2 (both correct), the issue is the lockstep test mechanism. The test assumes the interpreter can step `IC` times without errors, but the block's backward branches cause IC>4096 which may exceed the interpreter's natural stopping point.

**Step 3**: If V1!=V2 or V1!=expected, use native-code trace (already implemented: `jitCompileDebug`) to dump V1/V2 Prog listings and find the specific instruction divergence.

### Files

- `jit_emit_ir_test.go` — add `TestNativeTrace_sraw`
- `jit_emit_ir.go` lines 1220-1226 — SRAW emission (IR looks correct)
- `cpu.go` line 378 — interpreter SRAW/SRLW (looks correct)
- `riscv_test.go` lines 395-484 — lockstep test mechanism

## Bug 2: ld_st Hang

### Symptoms

`rv64ui-p-ld_st` hangs at block 63, pc=0x1a0. `StepBlock` never returns — the native code enters an infinite loop. The interpreter passes the same test.

### Root Cause Hypothesis

The `BudgetCheck` mechanism (`ir/highlevel.go:107`) should prevent infinite loops by checking `IC >= 4096` before each backward branch. If BudgetCheck is working, the block exits after at most 4096 instruction iterations. The hang means either:

1. **No BudgetCheck on a backward path**: A backward branch or jump within the block doesn't have a BudgetCheck because the emitter miscategorizes it as forward.

2. **BudgetCheck comparison bug**: `BranchImm(ic, MaxIC, GE, tooBig)` doesn't fire because of a comparison or lowering issue.

3. **IC not incrementing**: `AddImm(ic, ic, 1)` doesn't actually increment RBP within the loop body, so IC stays at 0 and the budget check never triggers.

### Investigation Plan

**Step 1**: Create `TestDumpBlock_ld_st_0x1a0` — emit the block at pc=0x1a0 from `rv64ui-p-ld_st` with maxBlockInsns=2048 and dump:
- The full IR listing
- Whether any backward branches exist
- Whether BudgetCheck labels are present
- The V1 Prog listing (via `jitCompileDebug`)

**Step 2**: Check if the block has a backward branch path WITHOUT BudgetCheck. Search the IR for `IRJump` to labels that appear before the jump (backward without budget).

**Step 3**: If the block has an unconditional jump to an internal label (JAL rd=0 forward to a previously-emitted label), this could loop without BudgetCheck. The fix: treat ANY jump to an already-emitted label as a backward branch requiring BudgetCheck.

**Step 4**: As a safety net, add a global IC limit in the native code prologue or the BudgetCheck itself.

### Files

- `jit_emit_ir.go` lines 1843-1863 — `emitJAL` backward branch handling
- `jit_emit_ir.go` lines 1879-1911 — `emitBranch` backward branch handling
- `ir/highlevel.go` lines 103-114 — `BudgetCheck` implementation
- `jit_emit_ir_test.go` — add `TestDumpBlock_ld_st_0x1a0`

## Implementation Order

1. **Diagnostic tests first** — before fixing, confirm root causes:
   - `TestNativeTrace_sraw` — V1 vs V2 vs interpreter on the failing block
   - `TestNativeTrace_srlw` — same for SRLW
   - `TestDumpBlock_ld_st_0x1a0` — dump IR and check BudgetCheck presence

2. **Fix ld_st hang** — likely a missing BudgetCheck on a forward-jump-to-already-emitted-label path in `emitJAL`. Fix: check if the jump target label has already been placed (not just "is the target address < current PC"), and add BudgetCheck if so.

3. **Fix sraw/srlw/sllw** — depends on diagnostic results:
   - If V1!=V2: fix the lowerer (use Prog dump to find exact divergence)
   - If V1==V2!=interp: fix the interpreter or the lockstep IC comparison

## Verification

```bash
# Diagnostic tests
go test -count=1 -run 'TestNativeTrace_sraw' -timeout 30s -v .
go test -count=1 -run 'TestNativeTrace_srlw' -timeout 30s -v .
go test -count=1 -run 'TestDumpBlock_ld_st_0x1a0' -timeout 30s -v .

# After fixes:
go test -count=1 -run 'TestRISCVTests_Lockstep_UI' -timeout 120s -v .
go test -count=1 -run 'TestRISCVTests_UI_JIT' -timeout 60s -v .
go test -count=1 -run 'TestBisectBlockSize' -timeout 60s -v .
go test -count=1 -run 'TestExhaustive' -timeout 120s -v ./ir/
go test -count=1 -run 'TestMetaIterOrder_AllUI' -timeout 120s -v .
```

## Critical Files

- `jit_emit_ir_test.go` — diagnostic tests
- `jit_emit_ir.go` — emitJAL/emitBranch backward-branch handling
- `ir/highlevel.go` — BudgetCheck
- `cpu.go` — interpreter W-variant handling
- `riscv_test.go` — lockstep test mechanism
- `ir/lower_amd64.go` — lowerer (if V1!=V2)
- `jit_native.go` — `jitCompileDebug` (already implemented)
- `goasm/api.go` — `DumpProgs` (already implemented)
