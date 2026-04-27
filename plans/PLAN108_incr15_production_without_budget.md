# Plan: Add UseR15InstructionCounter for Lightweight Production IC

## Context

`cpu.Cycle()` currently accumulates `blk.numInsns` (static block size), which overcounts when blocks exit early via branches or traps. We need precise per-instruction counting for accurate benchmarking. The lockstep machinery already uses R15 as an IC register with `INC R15` before each guest instruction, but it's bundled with budget checks that force block exits every N instructions — too expensive for production.

New flag `JIT.UseR15InstructionCounter` enables R15 as a lightweight IC: `INC R15` per guest instruction, no budget checks, no forced exits. R15 is spilled to `State.IC` on block exit and read back by Go dispatch.

## Behavior Matrix

| UseR15InstructionCounter | DebugOneBlockLockstepMode | Effect |
|:---:|:---:|---|
| false | false | Status quo: no R15 IC, no budget checks |
| **true** | false | **New**: R15 IC counting only, no budget checks |
| true | true | Full lockstep: R15 IC + budget checks (existing) |
| false | true | Treated as (true, true) — lockstep implies IC |

## Changes

### 1. `jit.go` — Add flag, wire to emitter

**Line ~244**: Add new field:
```go
UseR15InstructionCounter bool // INC R15 per guest instruction for precise IC
```

**`NewJIT()` (~line 265)**: Default to `true` so all new JITs get precise IC.

**Implication**: `DebugOneBlockLockstepMode = true` implies `UseR15InstructionCounter = true` (lockstep needs IC). No need to check both in the emitter — just derive a single `useIC` bool.

### 2. `jit_emit_ir.go` — Decouple IncIC from budget checks

**emitter struct (~line 93)**: Add field:
```go
useIC bool // emit INC R15 per instruction (no budget checks unless lockstepMode)
```

**emitter initialization (~line 1002)**: Set from JIT flags:
```go
useIC: j.UseR15InstructionCounter || j.DebugOneBlockLockstepMode,
```

**Block init (~line 1006-1009)**: Change from:
```go
if e.lockstepMode {
    e.sharedBudgetExit = e.irEm.NewLabel()
    e.irEm.ZeroIC()
}
```
To:
```go
if e.useIC {
    e.irEm.ZeroIC()  // XOR R15, R15
}
if e.lockstepMode {
    e.sharedBudgetExit = e.irEm.NewLabel()
}
```

**`emitBudgetCheck()` (~line 302-309)**: Currently emits RegBudget + IncIC. Split:
```go
func (e *emitter) emitBudgetCheck() {
    if e.useIC {
        e.irEm.IncIC()  // INC R15 — always when IC enabled
    }
    if e.lockstepMode {
        // budget check (CMP R15, budget; JGE coldPath) — only in lockstep
        ...existing RegBudget code...
    }
}
```

**`spillIC()` (~line 367-370)**: Change guard:
```go
func (e *emitter) spillIC() {
    if e.useIC {
        e.irEm.SpillIC()  // MOV [RBP+600], R15
    }
}
```

**Fused instruction IncIC calls (~lines 1148-1191)**: These emit extra `IncIC()` for fused pairs (AUIPC+ADDI etc.). Guard changes from `e.lockstepMode` to `e.useIC`.

**DecIC guard (~lines 1323-1325, 2811-2813)**: Change from `e.lockstepMode` to `e.useIC`.

**Budget exit finalization (~lines 773-783)**: The `sharedBudgetExit` / `RetBudget` code stays guarded by `e.lockstepMode` only — no change needed (budget exits are lockstep-only).

### 3. `jit_native.go` — Exclude R15 from pool when IC enabled

**Line 35-36**: Change from:
```go
if j.DebugOneBlockLockstepMode {
    pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
}
```
To:
```go
if j.UseR15InstructionCounter || j.DebugOneBlockLockstepMode {
    pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
}
```

Same change at line 159-160 (`jitCompileDebug`).

### 4. `jit.go` — Use `res.IC` for `cpu.cycle` when available

**StepBlock (~line 593)**: Already does:
```go
ic := res.IC
if ic == 0 {
    ic = uint64(blk.numInsns)
}
cpu.cycle += ic
```
This already prefers `res.IC` when non-zero. No change needed — when UseR15InstructionCounter is on, `res.IC` will be populated via State.IC, and `cpu.cycle` will be precise.

**RunJIT (~line 785)**: Currently uses `uint64(blk.numInsns)`. Change to same pattern:
```go
ic := res.IC
if ic == 0 {
    ic = uint64(blk.numInsns)
}
cpu.cycle += ic
```

### 5. No changes needed

- **`abjit/abjit.go`**: State.IC field already exists at offset 600
- **`jit_abjit.go`**: Already copies `s.IC` to `res.IC` (line 47)
- **`lower_amd64_ops.go`**: `opsIncIC`, `opsSpillIC`, `opsZeroIC` already exist
- **`lower_amd64_abjit.go`**: Lowerer already handles IRIncIC/IRSpillIC/IRZeroIC
- **`highlevel.go`**: `IncIC()`, `SpillIC()`, `ZeroIC()` already exist
- **`ir.go`**: IR opcodes already defined

## Verification

```bash
# Basic: all tests pass with UseR15InstructionCounter=true (new default)
cd ~/ris && go test -count=1 -run 'TestRISCVTests_UI_JIT_Lazy' .

# Lockstep still works (uses both IC + budget)
cd ~/ris && go test -count=1 -run 'TestRISCVTests_Lockstep_UI' .

# Precise IC: cpu.Cycle() should now match interpreter's cycle count
# for the same program (verify via lockstep or manual comparison)

# Benchmarks: make quad should show accurate timing
cd ~/ris && make quad
```

## Critical Files

| File | Change |
|------|--------|
| `jit.go:244` | Add `UseR15InstructionCounter bool`, default true in NewJIT |
| `jit.go:785` | Use `res.IC` instead of `blk.numInsns` in RunJIT |
| `jit_emit_ir.go:93` | Add `useIC bool` to emitter |
| `jit_emit_ir.go:302` | Split emitBudgetCheck: IncIC always, RegBudget only in lockstep |
| `jit_emit_ir.go:367` | spillIC guard: `useIC` instead of `lockstepMode` |
| `jit_emit_ir.go:1006` | ZeroIC guard: `useIC` instead of `lockstepMode` |
| `jit_emit_ir.go:1148+` | Fused IncIC guard: `useIC` instead of `lockstepMode` |
| `jit_emit_ir.go:1323+` | DecIC guard: `useIC` instead of `lockstepMode` |
| `jit_native.go:35` | R15 pool exclusion: add `UseR15InstructionCounter` check |
