At 

commit 95bbfd424fbc74712a97152af208f58e491f6a5f (HEAD -> incr15, origin/incr15)
Author: Jason E. Aten, Ph.D. <jason@devnull>
Date:   Mon Apr 27 02:06:48 2026 -0300

    with uint64 fix to avoid register alloc overflow, green TestRISCVTests_Lockstep_UI after 5 minutes


=== RUN   TestEmitter_Tmp
    emit_test.go:32: first Tmp = 69, want 70
--- FAIL: TestEmitter_Tmp (0.00s)

=== RUN   TestEndToEnd_LoopWithBudget
    highlevel_test.go:455: expected BranchImm GE for budget check
--- FAIL: TestEndToEnd_LoopWithBudget (0.00s)

=== RUN   TestIRInstrNoPointers
    ir_test.go:172: IRInstr size = 56, expected <= 48 (no slices/maps inside)
--- FAIL: TestIRInstrNoPointers (0.00s)

=== RUN   TestChaining_ICAccumulatesAcrossChainedExits
    jit_chaining_test.go:204: cpu.Cycle() = 65094, want 40001 (IC accounting across budget-check re-entries must match exactly)
--- FAIL: TestChaining_ICAccumulatesAcrossChainedExits (0.00s)

=== RUN   TestChaining_FaultExitsWritebackIC
    jit_chaining_test.go:238: cpu.Cycle() = 1102, want 1 (LUI retires, LW faults before its IC++). A value != 1 suggests IC writeback on the fault path is broken.
--- FAIL: TestChaining_FaultExitsWritebackIC (0.00s)

FAIL	riscv	502.149s


# Fix 5 Red Tests After VReg uint16 -> uint64 Change

## Context
Widening `VReg` from `uint16` to `uint64` (ir.go:40) fixed an infinite-loop hang in the register allocator when blocks exceed 65535 temps. The lockstep test suite is now green (267s). However, 5 unit tests broke. Three are direct consequences of the type change; two appear to be pre-existing failures exposed by running the full suite on the incr15 branch (they fail because RunJIT's ABJIT path uses RDTSC for `cpu.cycle` but the tests expect instruction count).

---

## Test 1: TestEmitter_Tmp (emit_test.go:30)

**Symptom:** `first Tmp = 69, want 70`

**Root cause:** Test expects `VRegTempStart+6` (70) but `NewEmitter` only allocates 5 Tmp() calls:
- xBase(64), fBase(65), memBase(66), memMask(67), VRRegFile(68)

The sibling test `TestEmitter_NewEmitter` at line 13 correctly checks `nextTmp == VRegTempStart+5`. This test's expectation is stale (a 6th param VReg was removed at some point).

**Fix:** emit_test.go:30 — change `VRegTempStart+6` to `VRegTempStart+5`. Update the comment to "5 params already allocated" (4 named + VRRegFile).

---

## Test 2: TestEndToEnd_LoopWithBudget (highlevel_test.go:449)

**Symptom:** `expected BranchImm GE for budget check`

**Root cause:** The test creates IR via high-level API (`AddImm`, `StopperLoad`, `Jump`), then checks for an `IRBranchImm` with `GE` predicate. But:
- `Jump()` just emits `IRJump` — no auto budget check
- `NewEmitter(nil)` has no JIT context, so lockstep mode is off
- Budget checks are only inserted by `emitBudgetCheck()` in the RISC-V decoder (`emit32`/`emitRVC`), not the high-level API
- The current budget mechanism uses `IRRegBudget` (not `IRBranchImm`)

The test checks for a feature that either was removed or never existed in the high-level path.

**Fix:** Update the test to reflect current behavior. Two options:
- **Option A (recommended):** Remove the `IRBranchImm GE` assertion. The test still verifies that the loop IR is well-formed (label, add, stopper, jump). Budget checking is covered by the lockstep integration tests.
- **Option B:** Have the test explicitly call `e.RegBudget()` and assert `IRRegBudget` is present.

---

## Test 3: TestIRInstrNoPointers (ir_test.go:172)

**Symptom:** `IRInstr size = 56, expected <= 48`

**Root cause:** With VReg=uint64, IRInstr layout is:
```
Op(1) + T(1) + U(1) + Pred(1) + Scale(1)  = 5 bytes
padding                                     = 3 bytes  (align Dst to 8)
Dst(8) + A(8) + B(8) + C(8)               = 32 bytes
Imm(8) + Imm2(8)                           = 16 bytes
Total:                                       56 bytes
```
Previously with VReg=uint16: ~32 bytes.

**Fix:** ir_test.go — update the threshold from `48` to `64`. Update the comment to document the uint64 VReg layout. The test's purpose (detect accidental slice/map fields) is still served since 56 << 80 (minimum with one pointer field).

---

## Test 4: TestChaining_ICAccumulatesAcrossChainedExits (jit_chaining_test.go:203)

**Symptom:** `cpu.Cycle() = 65094, want 40001`

**Root cause:** This test runs a tight 4-instruction loop for 10,000 iterations via `RunJIT` and checks that `cpu.Cycle() == 4*10000+1`. But:
- `NewJIT()` defaults to ABJIT policy (`useABJIT = true`)
- ABJIT exit thunk stores **RDTSC cycles** in `State.Cycles` (lower_amd64_abjit.go:198-212)
- `RunJIT` does `cpu.cycle += res.Cycles` (jit.go:778) — adds RDTSC delta, not instruction count
- IC counting (`ZeroIC`/`IncIC`/`SpillIC`) is only emitted in lockstep mode
- In non-lockstep mode, `res.IC = 0` and `res.Cycles = RDTSC_delta`

The test expects instruction-level cycle counting which doesn't exist in normal ABJIT RunJIT mode.

**Fix:** Enable IC counting in all ABJIT blocks (not just lockstep). This requires:
1. **jit_emit_ir.go** — Add a new field `icCountingMode bool` to the emitter, set `true` for all ABJIT blocks. Emit `ZeroIC` at block start and `IncIC` after each instruction. Keep `emitBudgetCheck()` lockstep-only (budget checks are separate from IC).
2. **jit_emit_ir.go** — In `spillIC()`, spill IC unconditionally when `icCountingMode` is true (currently gated on `lockstepMode`).
3. **jit_emit_ir.go** — In `finalize()`, emit `SpillIC` before all block exits (ret, chain exit, fault exit) when `icCountingMode` is true.
4. **jit_native.go:34-36** — Always remove R15 from the pool for ABJIT blocks (not just lockstep mode), since R15 is dedicated to IC.
5. **jit.go:778** — In RunJIT, change `cpu.cycle += res.Cycles` to `cpu.cycle += res.IC` when IC > 0, falling back to `res.Cycles` otherwise.
6. **jit.go:599** — Same change in StepBlock.

**Files to modify:**
- `jit_emit_ir.go` — emitter struct, emitBlockRange, spillIC, finalize
- `jit_native.go` — R15 pool removal (lines 34-36)
- `jit.go` — RunJIT (line 778), StepBlock (line 599)

---

## Test 5: TestChaining_FaultExitsWritebackIC (jit_chaining_test.go:237)

**Symptom:** `cpu.Cycle() = 1102, want 1`

**Root cause:** Same as Test 4 — `cpu.cycle` accumulates RDTSC cycles instead of instruction count. The test expects exactly 1 (LUI retires, LW faults before its IC++).

**Fix:** Addressed by the same IC-counting fix as Test 4. Once IC is always tracked and used for cpu.cycle, the fault exit path already spills IC via the fault stub, and the test will see `cpu.Cycle() == 1`.

---

## Implementation Order

1. Fix Test 3 (ir_test.go size threshold) — trivial, no logic change
2. Fix Test 1 (emit_test.go expectation) — trivial, no logic change
3. Fix Test 2 (highlevel_test.go budget check assertion) — test-only change
4. Fix Tests 4 & 5 (enable IC counting in all ABJIT blocks) — requires emitter and dispatch changes

## Verification

```bash
go test -v -run "TestEmitter_Tmp|TestEndToEnd_LoopWithBudget|TestIRInstrNoPointers|TestChaining_ICAccumulates|TestChaining_FaultExitsWritebackIC" .
```

Then full suite:
```bash
go test -v .
```
