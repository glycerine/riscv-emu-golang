# Batched IC budget at back-edges for lockstep slicing

## Context

The lockstep tests (`TestRISCVTests_Lockstep_UI`, etc.) compare JIT vs interpreter state after each `StepBlock` call. They depend on `StepBlock` returning an instruction count so the interpreter can run the same number of steps. After removing per-instruction IC counting (2x overhead), `StepBlock` returns 0 and the lockstep tests break.

The solution: when `DebugOneBlockLockstepMode` is true on the JIT, emit a batched IC budget check at each backward branch. The check uses three x86 instructions that modify only EFLAGS (no GP registers): `ADD QWORD [RBP+ic_offset], N; CMP QWORD [RBP+ic_offset], budget; JGE exit`. N is the static instruction count of the loop body, known at compile time. Heavy loops hit the budget faster than tight loops, so both approximate the same total instruction count before exiting.

When `DebugOneBlockLockstepMode` is false (production), no budget check is emitted. Zero overhead.

The existing `StopperLoad + Jump` pattern at backward branches is preserved unchanged. The budget check is emitted **before** the StopperLoad when lockstep mode is active.

## New JIT fields (`jit.go`)

```go
DebugOneBlockLockstepMode bool    // emit budget checks at back-edges
LockstepModeBudget        int64   // max IC before forced exit (default 65536)
```

`NewJIT()` sets `LockstepModeBudget = 65536`.

## State struct — add IC field (`abjit/abjit.go`)

Add `IC uint64` after `Cycles`. Record its byte offset. Update `TestStateLayout`.

## Emitter changes (`jit_emit_ir.go`)

Add to the emitter struct:
```go
lockstepMode   bool
lockstepBudget int64
backEdgeIC     int   // instructions since last back-edge or block entry
```

Passed from JIT via `emitBlockRange`:
```go
lockstepMode:   j.DebugOneBlockLockstepMode,
lockstepBudget: int64(j.LockstepModeBudget),
```

In `advancePC()`, also increment `e.backEdgeIC++`.

At each backward branch site (in `emitJAL` and `emitBranch`), **before** the existing `StopperLoad + Jump`, when `e.lockstepMode` is true:

```go
if e.lockstepMode {
    exitLabel := e.irEm.NewLabel()
    e.irEm.MemBudget(IC_OFFSET, int64(e.backEdgeIC), e.lockstepBudget, exitLabel)
    e.backEdgeIC = 0
    // exitLabel path: writeback + return
    // (emitted after the normal backward branch code, before finalize)
}
// existing StopperLoad + Jump unchanged
```

The exit path at `exitLabel`: `WriteBackAll()` + `Ret(targetPC, jitOK, VRegZero)`.

## New IR op: `IRMemBudget` (`ir.go`)

```go
IRMemBudget // ADD [RBP+Imm], Imm2; CMP [RBP+Imm], Imm3; JGE Label(Dst)
```

- `Imm` = State struct offset of IC
- `Imm2` = instruction delta to add
- `Imm3` = budget limit
- `Dst` = overflow label (cast to VReg, reinterpreted as Label by lowerer)

## Lowerer (`lower_amd64_ops.go`)

```go
func (lc *lowerOps) opsMemBudget(ins *IRInstr) {
    off := ins.Imm  // IC offset in State

    // ADD QWORD [RBP + off], ins.Imm2
    p1 := lc.c.NewProg()
    p1.As = x86.AADDQ
    p1.From.Type = obj.TYPE_CONST
    p1.From.Offset = ins.Imm2
    p1.To.Type = obj.TYPE_MEM
    p1.To.Reg = goasm.REG_AMD64_BP
    p1.To.Offset = off
    lc.c.Append(p1)

    // CMP QWORD [RBP + off], ins.Imm3
    p2 := lc.c.NewProg()
    p2.As = x86.ACMPQ
    p2.From.Type = obj.TYPE_MEM
    p2.From.Reg = goasm.REG_AMD64_BP
    p2.From.Offset = off
    p2.To.Type = obj.TYPE_CONST
    p2.To.Offset = ins.Imm3
    lc.c.Append(p2)

    // JGE overflow label
    p3 := lc.c.NewProg()
    p3.As = x86.AJGE
    p3.To.Type = obj.TYPE_BRANCH
    lc.c.Append(p3)
    lc.bindLabel(Label(ins.Dst), p3)
}
```

## Dispatch — zero IC and read it back

**`jit_abjit.go` (`abjitDispatch`):** Set `s.IC = 0` before `abjit.CallJIT`. Read `s.IC` into result after.

**`jit.go` (`StepBlock`):** Return IC from result. For straight-line blocks (no back-edge hit, IC stays 0), fall back to `blk.numInsns`:

```go
case jitOK:
    ic := res.IC
    if ic == 0 {
        ic = uint64(blk.numInsns)
    }
    return ic, nil
```

## compiledBlock — add numInsns (`jit.go`)

Add `numInsns int` to `compiledBlock`. Set from `res.numInsns` during `jitCompile`.

## Lockstep test updates (`riscv_test.go`)

In `runLockstep`:
```go
jit := NewJIT()
jit.DebugOneBlockLockstepMode = true
```

Interpreter loop uses returned IC as before:
```go
jitIC, jitErr := jit.StepBlock(jitCPU)
for i := uint64(0); i < jitIC; i++ {
    interpErr = interpCPU.step()
    ...
}
```

## Files to modify

- `ir.go` — add `IRMemBudget` op
- `highlevel.go` — add `MemBudget()` emit helper
- `lower_amd64_ops.go` — add `opsMemBudget()` lowering, wire in dispatch
- `jit_emit_ir.go` — add `lockstepMode`/`lockstepBudget`/`backEdgeIC` to emitter; emit budget before StopperLoad at backward branches
- `jit.go` — add `DebugOneBlockLockstepMode`/`LockstepModeBudget` fields; read IC in StepBlock; add `numInsns` to compiledBlock
- `abjit/abjit.go` — add IC field to State
- `abjit/abjit_test.go` — update TestStateLayout
- `jit_abjit.go` — zero IC before block entry, read IC into result
- `riscv_test.go` — set DebugOneBlockLockstepMode in lockstep tests, restore IC-based interpreter loop

## Verification

```bash
go build ./...
go test -v -run TestRISCVTests_Lockstep_UI/add -timeout 30s .
go test -v -run TestRISCVTests_Lockstep_UI -timeout 300s .
go test -v -run TestABJIT_RISCVTests_UI -timeout 30s .
go test -v ./...
```
