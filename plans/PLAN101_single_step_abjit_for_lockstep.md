# Per-instruction budget checking with register IC for lockstep single-stepping

## Context

The ABJIT backend is young and likely has bugs. Without precise single-stepping we can't narrow them down. The lockstep tests compare JIT vs interpreter state, but they need the JIT to execute a controllable number of instructions and report the exact count.

**Problem with the current memory-batched approach:** IC is accumulated via `MemAdd` at labels and `MemBudget` at back-edges only. `emitLabel` places the `MemAdd` BEFORE `PlaceLabel` — on the fall-through path only. When a forward branch is taken, it skips the `MemAdd`, losing IC for all instructions since the last flush. This makes counting path-dependent and systematically wrong.

**Solution:** Per-instruction budget checking with a dedicated register.

- Dedicate R15 as the IC register (lockstep mode only)
- **Before** each RISC-V instruction: `CMP R15, budget; JGE exit_stub` then `INC R15`
- The budget check BEFORE the INC means: if budget is hit, we exit before executing the instruction, and IC reflects only instructions already completed
- With budget=1: true single-stepping — execute exactly 1 instruction per StepBlock
- With budget=N: execute exactly N instructions then exit
- Budget exit stubs are deferred to block end (cold path)
- At all natural exits: `MOV [RBP+IC_offset], R15` spills the precise count

When `DebugOneBlockLockstepMode` is false (production), none of this is emitted. Zero overhead.

## Per-instruction IR sequence

At the start of each RISC-V instruction's code (before the instruction's IR):
```
RegBudget(budget, exitLabel)   <- CMP R15, budget; JGE exitLabel
IncIC                           <- INC R15
; instruction IR follows...
```

Budget exit stubs (deferred to block end, one per instruction):
```
exitLabel:
  SpillIC                       <- MOV [RBP+IC_offset], R15
  WriteBackAll
  Ret(current_pc, jitOK)        <- exit with the unexecuted instruction's PC
```

**Trace with budget=1:**
1. R15=0 at block entry.
2. At PC=0x100: CMP 0 >= 1 -> false. INC (R15=1). Execute instruction.
3. At PC=0x104: CMP 1 >= 1 -> true. Exit with PC=0x104, IC=1.
Result: executed 1 instruction, IC=1, next_pc=0x104. Interpreter runs 1 step. Compare.

**Trace with budget=3 and a taken branch:**
1. R15=0. At 0x100: check pass. INC (1). Execute add.
2. At 0x104: check pass. INC (2). Execute beq -> taken, jump to 0x200.
3. At 0x200: check pass. INC (3). Execute or.
4. At 0x204: CMP 3 >= 3 -> true. Exit with PC=0x204, IC=3.
Result: 3 instructions executed on both taken and not-taken paths. Precise.

## Why budget check goes BEFORE the instruction, not after

A branch instruction's IR changes control flow. If the budget check + INC were placed after the instruction (as in `advancePC`), a taken branch would skip them -- losing the count for the branch instruction itself. Placing the check + INC BEFORE the instruction means:
- The INC always executes regardless of what the instruction does
- Branches land at the next instruction's label -> next instruction's budget check -> correct

## Register choice: R15

ABJIT pool has 11 GP regs (R14 excluded as Go goroutine pointer). In lockstep mode, R15 is removed from the pool and pinned to `VRIC`, leaving 10 allocatable GP regs.

## New VReg constant (`lower_amd64.go`)

```go
const VRIC = VReg(VRegTempStart + 5) // t69 -- IC register, pinned to R15 in lockstep
```

## Register pool adjustment (`jit_native.go`)

In `jitCompile` (and `jitCompileDebug`), after getting pool/pinned:

```go
if j.DebugOneBlockLockstepMode {
    pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
    pinned[VRIC] = goasm.REG_AMD64_R15
}
```

Add `removeReg` helper (filter one int16 from a slice).

## New IR ops (`ir.go`)

```go
IRZeroIC     // XOR R15, R15 -- block entry
IRIncIC      // INC R15 -- per-instruction count
IRSpillIC    // MOV [RBP+IC_offset], R15 -- at every exit
IRRegBudget  // CMP R15, Imm2; JGE Label(Dst) -- per-instruction budget gate
```

`IRMemAdd` and `IRMemBudget` remain but are no longer used by lockstep IC.

## Highlevel emit helpers (`highlevel.go`)

```go
func (e *Emitter) ZeroIC()  { e.emit(IRInstr{Op: IRZeroIC}) }
func (e *Emitter) IncIC()   { e.emit(IRInstr{Op: IRIncIC}) }
func (e *Emitter) SpillIC() { e.emit(IRInstr{Op: IRSpillIC}) }
func (e *Emitter) RegBudget(budget int64, overflowLabel Label) {
    e.emit(IRInstr{Op: IRRegBudget, Imm2: budget, Dst: VReg(overflowLabel)})
}
```

## Lowerer (`lower_amd64_ops.go`)

```go
func (lc *lowerOps) opsZeroIC()  { /* XOR R15, R15 */ }
func (lc *lowerOps) opsIncIC()   { /* INC R15  (3 bytes: 49 FF C7) */ }
func (lc *lowerOps) opsSpillIC() { /* MOV [RBP+abjitStateICOffset], R15 */ }
func (lc *lowerOps) opsRegBudget(ins *IRInstr) {
    /* CMP R15, ins.Imm2; JGE Label(ins.Dst) */
}
```

Wire all four into the lowerer's op dispatch.

## Emitter changes (`jit_emit_ir.go`)

### Remove (dead code from old batched approach):
- `backEdgeIC int` field
- `flushAndResetIC()` function
- `flushIC()` function
- All calls to `flushAndResetIC` and `flushIC`

### Simplify `emitLabel`:
```go
func (e *emitter) emitLabel() {
    e.irEm.PlaceLabel(e.getOrCreateLabel(e.pc))
}
```

### New `emitBudgetCheck` -- called at start of each instruction:
```go
func (e *emitter) emitBudgetCheck() {
    if !e.lockstepMode { return }
    exitLabel := e.irEm.NewLabel()
    e.irEm.RegBudget(e.lockstepBudget, exitLabel)
    e.irEm.IncIC()
    e.budgetExits = append(e.budgetExits, budgetExit{exitLabel, e.pc})
}
```

New field on emitter: `budgetExits []budgetExit` where:
```go
type budgetExit struct {
    label Label
    pc    uint64 // the instruction we did NOT execute -- resume here
}
```

### `advancePC` -- IC logic removed, just bookkeeping:
```go
func (e *emitter) advancePC(size uint64) {
    e.numInsns++
    e.pc += size
}
```

### `spillIC` helper:
```go
func (e *emitter) spillIC() {
    if e.lockstepMode { e.irEm.SpillIC() }
}
```

### Placement -- `emit32` and `emitRVC`:

In `emit32` (line 1039), right after `emitLabel()`:
```go
func (e *emitter) emit32(insn uint32) {
    // ...
    e.emitLabel()
    e.emitBudgetCheck()   // <- NEW: budget gate + INC before instruction
    switch opcode { ... }
}
```

In `emitRVC` (line 2720), right after `emitLabel()`:
```go
func (e *emitter) emitRVC(insn uint16) {
    e.emitLabel()
    e.emitBudgetCheck()   // <- NEW
    // ...
}
```

### Macro-op fusion -- extra INC for fused second instruction:

The budget check at `emit32` entry counts instruction 1 (e.g., AUIPC). The fused second instruction needs an explicit `IncIC`. In each fusion path, add `if e.lockstepMode { e.irEm.IncIC() }` before the second `advancePC`:

1. **AUIPC+ADDI** (line 1078): before 2nd `advancePC`
2. **AUIPC+JALR** (line 1087): before `emitJALR`
3. **AUIPC+LOAD** (line 1093): before 2nd `advancePC`
4. **AUIPC+STORE/tohost** (line 1111): before 2nd `advancePC`
5. **SLLI+SRLI zext** (line 1279): before 2nd `advancePC`
6. **SLLIW+SRLIW zext** (line 1419): before 2nd `advancePC`

(No extra budget check between fused instructions -- they execute atomically.)

### Block entry -- emit ZeroIC in `emitBlockRange` (line 963):
```go
if e.lockstepMode {
    e.irEm.ZeroIC()
}
// main emit loop...
```

### Block finalize -- emit budget exit stubs:

After existing deferred exit handling (after line 737):
```go
for _, be := range e.budgetExits {
    e.irEm.PlaceLabel(be.label)
    e.spillIC()
    e.irEm.WriteBackAll()
    e.irEm.Ret(be.pc, jitOK, VRegZero)
}
```

### All natural exits -- add `spillIC()`:

1. `emitReturn` (line 346): `e.spillIC()` before `WriteBackAll`
2. `emitChainableReturn` (line 393): `e.spillIC()` before `WriteBackAll`
3. JALR `DynChainableRet` (lines 2648, 2663): `e.spillIC()` before `DynChainableRet`
4. tohost exit (line 1119): `e.spillIC()` before `WriteBackAll`
5. Fault tails in finalize (line 744): `e.spillIC()` before `WriteBackAll`
6. Syscall (line 369): `e.spillIC()` before `WriteBackAll`

### Remove old back-edge budget checks:

In `emitBranch` backward (line 2687) and `emitJAL` backward (line 2592): remove the `if e.lockstepMode { ... MemBudget ... }` blocks. The per-instruction budget check already handles loop exits -- when execution jumps back to a label, the label's budget check fires.

The backward-branch code simplifies to just the production path:
```go
e.irEm.StopperLoad(e.stopperAddr)
e.irEm.Jump(targetLabel)
```

## Dispatch -- no changes needed (`jit_abjit.go`, `jit.go`)

- `s.IC = 0` before CallJIT -> zeroes memory (R15 zeroed by IRZeroIC)
- `res.IC = s.IC` after CallJIT -> reads spilled value
- `StepBlock` returns IC with `numInsns` fallback for non-lockstep blocks

## Lockstep test (`riscv_test.go`)

Set budget=1 for single-step debugging:
```go
jit.DebugOneBlockLockstepMode = true
jit.LockstepModeBudget = 1
```

With budget=1, IC is always exactly 1 (except for fused instructions where IC=2). The interpreter runs IC steps. PCs must match exactly -- no catch-up needed. Keep catch-up as safety net.

For faster runs once single-step passes: increase budget (e.g., 100). IC is still precise.

## Files to modify

- `lower_amd64.go` -- add `VRIC` constant
- `ir.go` -- add `IRZeroIC`, `IRIncIC`, `IRSpillIC`, `IRRegBudget` ops
- `highlevel.go` -- add `ZeroIC()`, `IncIC()`, `SpillIC()`, `RegBudget()` helpers
- `lower_amd64_ops.go` -- add lowering for all four ops; wire into dispatch
- `jit_emit_ir.go` -- add `emitBudgetCheck`/`budgetExits`; call at start of `emit32`/`emitRVC`; add fusion INC; remove old `backEdgeIC`/`flushAndResetIC`/`flushIC`; remove back-edge budget checks; add `spillIC` at all exits; emit `ZeroIC` at block start; emit budget exit stubs in finalize
- `jit_native.go` -- exclude R15 from pool and pin VRIC when lockstepMode; add `removeReg` helper
- `riscv_test.go` -- set `LockstepModeBudget = 1` for single-step

## Verification

```bash
go build ./...
go test -v -run TestRISCVTests_Lockstep_UI/add -timeout 60s .
go test -v -run TestRISCVTests_Lockstep_UI -timeout 600s .
go test -v -run TestABJIT_RISCVTests_UI -timeout 30s .
go test -v ./...
```

Budget=1 will be slow (one JIT block per instruction). Use longer timeouts.
