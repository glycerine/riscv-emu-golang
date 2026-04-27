# Batched IC budget at back-edges for lockstep slicing

## Context

The lockstep tests (`TestRISCVTests_Lockstep_UI`, etc.) compare JIT vs interpreter state after each `StepBlock` call. They depend on `StepBlock` returning an instruction count so the interpreter can run the same number of steps. After removing per-instruction IC counting (2x overhead), `StepBlock` returns 0 and the lockstep tests break.

We need a mechanism that:
1. Forces JIT blocks to exit periodically (so the dispatch loop regains control)
2. Returns an accurate instruction count (so the interpreter can match)
3. Has negligible overhead in production (when disabled)
4. Adapts to loop body size (heavy loops exit sooner, tight loops later)

## Design: batched IC at back-edges

At each backward branch, emit three x86 instructions that operate on a memory-resident counter at a fixed State struct offset. No GP registers modified — only EFLAGS (which are dead at back-edges in our JIT).

```asm
L_loop_top:
    ... N guest instructions (loop body) ...
    ADD QWORD [RBP + ic_offset], N   # batched: one ADD per iteration
    CMP QWORD [RBP + ic_offset], 65536
    JGE exit_block                    # taken only when budget exhausted
    ... normal backward branch ...
```

- `N` is the static instruction count of the path from block entry (or last back-edge) to this back-edge, known at compile time from `e.numInsns`.
- The counter lives at a fixed offset in the State struct (RBP-relative), hot in L1.
- ADD/CMP modify only EFLAGS. Our JIT recomputes flags before every guest branch, so flags at back-edges are dead.
- Budget adapts: a 3-instruction loop adds 3 per iteration (exits after ~21K iterations). A 1000-instruction loop adds 1000 (exits after ~65 iterations). Both execute ~65K total instructions.

**This is only emitted when `DebugOneBlockLockstepMode` is true on the JIT.** Production code has zero overhead.

## Implementation

### 1. JIT struct — new fields (`jit.go`)

```go
DebugOneBlockLockstepMode bool   // when true, emit budget checks at back-edges
LockstepModeBudget        int64  // max IC before forced exit (default 65536)
```

In `NewJIT()`, set `LockstepModeBudget = 65536`.

### 2. State struct — add IC field (`abjit/abjit.go`)

Add `IC uint64` after `Cycles` (or replace `Cycles`). Record its offset. The dispatch loop zeroes `s.IC = 0` before each block entry in `abjitDispatch` (`jit_abjit.go`).

For the non-ABJIT (sandbox/rv8) path: add IC to the sret buffer at a known offset, or use a similar mechanism.

### 3. Emitter — track per-back-edge instruction count (`jit_emit_ir.go`)

Add `backEdgeIC int` to the emitter. It tracks instructions since the last back-edge (or block entry). Reset to 0 at block entry and after each back-edge emission.

In `advancePC()`:
```go
e.numInsns++
e.backEdgeIC++
```

### 4. New IR op: `IRBackEdgeBudget` (`ir.go`)

```go
IRBackEdgeBudget // ADD [State.IC], Imm; CMP [State.IC], Imm2; JGE exit
```

- `Imm` = instruction count to add (the `backEdgeIC` value)
- `Imm2` = budget limit (`LockstepModeBudget`)
- On overflow: writeback all dirty regs, return `(targetPC, jitOK, VRegZero)`

### 5. Emitter — emit budget check at backward branches (`jit_emit_ir.go`)

Replace the `StopperLoad + Jump` pattern at backward branches. When `e.lockstepMode` is true:

**In `emitJAL` (unconditional backward jump, ~line 2568):**
```go
if backward {
    if e.lockstepMode {
        e.irEm.BackEdgeBudget(e.backEdgeIC, e.lockstepBudget, targetLabel, target)
        e.backEdgeIC = 0
    } else {
        e.irEm.StopperLoad(e.stopperAddr)
    }
    e.irEm.Jump(targetLabel)
    e.gotoTargets.add(target)
    e.terminated = true
    return
}
```

**In `emitBranch` (conditional backward branch, ~line 2651):**
```go
if backward {
    takenLabel := e.irEm.NewLabel()
    continueLabel := e.irEm.NewLabel()
    e.irEm.Branch(a, b, pred, takenLabel)
    e.irEm.Jump(continueLabel)
    e.irEm.PlaceLabel(takenLabel)
    if e.lockstepMode {
        e.irEm.BackEdgeBudget(e.backEdgeIC, e.lockstepBudget, targetLabel, target)
        e.backEdgeIC = 0
    } else {
        e.irEm.StopperLoad(e.stopperAddr)
    }
    e.irEm.Jump(targetLabel)
    e.irEm.PlaceLabel(continueLabel)
}
```

### 6. Highlevel IR helper (`highlevel.go`)

```go
func (e *Emitter) BackEdgeBudget(icDelta int, budget int64, loopLabel Label, targetPC uint64) {
    // ADD [ic_mem], icDelta  — emitted as IRBackEdgeBudget
    // CMP [ic_mem], budget
    // JGE overflow
    // Jump loopLabel
    // overflow: WriteBackAll; Ret(targetPC, jitOK, VRegZero)
    overflow := e.NewLabel()
    e.emit(IRInstr{Op: IRBackEdgeBudget, Imm: int64(icDelta), Imm2: budget, Imm3: int64(overflow)})
    // The lowerer handles the ADD/CMP/JGE sequence and the overflow exit.
}
```

Actually, simpler: keep it as a highlevel expansion like BudgetCheck was:
```go
func (e *Emitter) BackEdgeBudget(icDelta int, budget int64, loopLabel Label, targetPC uint64) {
    exitLabel := e.NewLabel()
    e.AddImmMem(IC_OFFSET, int64(icDelta))  // ADD QWORD [RBP+offset], icDelta
    e.CmpImmMem(IC_OFFSET, budget)           // CMP QWORD [RBP+offset], budget
    e.BranchGE(exitLabel)                    // JGE exit
    e.Jump(loopLabel)                        // continue loop
    e.PlaceLabel(exitLabel)
    e.WriteBackAll()
    e.Ret(targetPC, jitOK, VRegZero)
}
```

Hmm, but we don't have `AddImmMem` / `CmpImmMem` IR ops. Simpler to make `IRBackEdgeBudget` a single IR op that the lowerer expands into the 3 x86 instructions + the overflow exit path.

### 7. Lowerer — emit x86 for `IRBackEdgeBudget` (`lower_amd64_ops.go`)

```go
func (lc *lowerOps) opsBackEdgeBudget(ins *IRInstr) {
    icOffset := int64(IC_STATE_OFFSET)  // offset of IC in State struct
    
    // ADD QWORD [RBP + icOffset], ins.Imm
    p1 := lc.c.NewProg()
    p1.As = x86.AADDQ
    p1.From.Type = obj.TYPE_CONST
    p1.From.Offset = ins.Imm          // icDelta
    p1.To.Type = obj.TYPE_MEM
    p1.To.Reg = goasm.REG_AMD64_BP
    p1.To.Offset = icOffset
    lc.c.Append(p1)
    
    // CMP QWORD [RBP + icOffset], ins.Imm2
    p2 := lc.c.NewProg()
    p2.As = x86.ACMPQ
    p2.From.Type = obj.TYPE_MEM
    p2.From.Reg = goasm.REG_AMD64_BP
    p2.From.Offset = icOffset
    p2.To.Type = obj.TYPE_CONST
    p2.To.Offset = ins.Imm2           // budget
    lc.c.Append(p2)
    
    // JGE to overflow label (handled by caller via bindLabel)
    p3 := lc.c.NewProg()
    p3.As = x86.AJGE
    p3.To.Type = obj.TYPE_BRANCH
    lc.c.Append(p3)
    lc.bindLabel(Label(ins.Imm3), p3)  // Imm3 = overflow label
}
```

Wait — the overflow exit (writeback + ret) needs to be emitted too. Better to handle the full sequence in the highlevel emitter using existing IR ops, and just add one new IR op for the ADD-to-memory:

### Revised approach: minimal new IR

Instead of one big `IRBackEdgeBudget` op, add two small ops:

```go
IRAddMem  // [RBP + Imm] += Imm2  (no GP regs, only EFLAGS)
IRCmpMem  // cmp [RBP + Imm], Imm2; sets flags for subsequent branch
```

Wait, we already have `IRBranchImm` which compares a VReg to an immediate. The problem is IC isn't in a VReg — it's in memory.

Simplest: **one new IR op `IRMemBudget`** that the lowerer expands fully:

```go
IRMemBudget  // Imm=icDelta, Imm2=budget, Imm3=overflow_label, mem offset=IC_STATE_OFFSET (hardcoded)
```

Lowerer emits: ADD [RBP+off], Imm; CMP [RBP+off], Imm2; JGE Label(Imm3).

The overflow label and exit path (writeback + ret) are emitted by the highlevel `BackEdgeBudget` helper using existing IR ops.

### 8. Emitter plumbing (`jit_emit_ir.go`)

Pass `lockstepMode bool` and `lockstepBudget int64` from JIT to emitter:

```go
e := &emitter{
    ...
    lockstepMode:   j.DebugOneBlockLockstepMode,
    lockstepBudget: int64(j.LockstepModeBudget),
}
```

### 9. StepBlock return value (`jit.go`)

After block execution, read `State.IC` and return it as the instruction count:

```go
case jitOK:
    return s.IC, nil   // IC accumulated by back-edge budget checks
```

For blocks without loops (no back-edges hit), IC stays 0. Use `numInsns` from the compiled block as fallback:

```go
case jitOK:
    ic := s.IC
    if ic == 0 {
        ic = uint64(blk.numInsns)  // straight-line block, no back-edge
    }
    return ic, nil
```

Store `numInsns` on `compiledBlock` during compilation.

### 10. compiledBlock — add numInsns field (`jit.go`)

```go
type compiledBlock struct {
    fn         uintptr
    nativeMmap []byte
    hasFP      bool
    numInsns   int    // static instruction count from emission
    chainExits []chainPatchInfo
    chainEntry uintptr
}
```

Set during `jitCompile` from `res.numInsns`.

### 11. Lockstep test updates (`riscv_test.go`)

In `runLockstep`:
```go
jit := NewJIT()
jit.DebugOneBlockLockstepMode = true
// jit.LockstepModeBudget = 65536  // default, can override
```

The interpreter loop uses the returned IC:
```go
jitIC, jitErr := jit.StepBlock(jitCPU)
for i := uint64(0); i < jitIC; i++ {
    interpErr = interpCPU.step()
    ...
}
```

### 12. IC offset in State struct

Add IC after Cycles (or reuse the Cycles slot):

```
Offset 592: Cycles  uint64
Offset 600: IC      uint64   ← new
```

Or replace Cycles with IC at offset 592 since RDTSC can be removed from the exit thunk when lockstep mode is active.

Actually, keep both. IC at a new offset. Update `TestStateLayout` accordingly.

## Files to modify

- `ir.go` — add `IRMemBudget` op
- `highlevel.go` — add `BackEdgeBudget()` helper
- `lower_amd64_ops.go` — add `opsMemBudget()` lowering
- `jit_emit_ir.go` — add `lockstepMode`/`lockstepBudget`/`backEdgeIC` to emitter; emit budget at backward branches
- `jit.go` — add `DebugOneBlockLockstepMode`/`LockstepModeBudget` fields; read IC in StepBlock; store numInsns on compiledBlock
- `abjit/abjit.go` — add IC field to State
- `abjit/abjit_test.go` — update TestStateLayout
- `jit_abjit.go` — zero IC before block entry, read IC into result
- `riscv_test.go` — set DebugOneBlockLockstepMode in lockstep tests

## Verification

```bash
go build ./...
go test -v -run TestRISCVTests_Lockstep_UI/add -timeout 30s .
go test -v -run TestRISCVTests_Lockstep_UI -timeout 300s .
go test -v -run TestABJIT_RISCVTests_UI -timeout 30s .
go test -v ./...
```
