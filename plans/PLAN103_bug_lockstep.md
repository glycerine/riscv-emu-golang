# Fix: Lockstep budget IC inflation for non-emitted terminators

## Context

With `jit.LockstepModeBudget = 2`, the lockstep test fails at **block 16** (starting at PC=0x84 in the `rv64ui-p-add` test). The JIT reports `IC=2` and `PC=0x86`, but only 1 real instruction was executed (the `c.li t6, 0` at 0x84). The interpreter, told to run 2 steps, executes the extra `csrr a0, mhartid` at 0x86, overshoots to 0x8a, and diverges from there.

With `budget=1`, the block at 0x84 has only 1 instruction before the CSR terminator, so the budget gate fires immediately at the CSR's check and IC stays correct at 1. That's why budget=1 passes.

## Root cause

In `emit32()` and `emitRVC()`, `emitBudgetCheck()` is called **before** the opcode switch. It emits both the budget gate (`CMP R15, budget; JGE cold`) and `IncIC` (`INC R15`). When the instruction turns out to be a CSR or unknown opcode, the emitter sets `e.terminated = true` **without** calling `advancePC()` — no instruction code is emitted, but the `IncIC` already ran. This inflates IC by 1 for every block that terminates at a non-emittable instruction.

The IncIC must stay **before** the instruction code (not moved to `advancePC`) because taken branches jump past any code emitted after the branch IR. The current pre-instruction placement correctly counts branches regardless of taken/not-taken.

### Affected code paths

Every case where `e.terminated = true` is set without a prior `advancePC()` call:
- `emit32` line 1314: CSR/unknown SYSTEM (`case 0x73: default`)
- `emit32` line 1319: unknown opcode (`default`)
- `emitRVC` line 2799: unknown quad
- `emitRVC_Q1` lines 2868, 2878: illegal C.ADDI16SP/C.LUI (nzimm=0)
- `emitRVC_Q1` line 2919, 2935: unknown MISC-ALU/funct3
- `emitRVC_Q2` line 2961: C.JR rd=0 (illegal)
- `emitRVC_Q2` line 2989: unknown funct3
- Sub-functions (`emitOpImm`, `emitOp`, `emitLoad`, etc.) that set `terminated=true` for unsupported sub-operations — the caller checks `if !e.terminated { e.advancePC(4) }`

All are handled generically by the fix below.

## Fix

Add `IRDecIC` (DEC R15) and emit it whenever an instruction terminates without advancing.

### Step 1: Add `IRDecIC` to the IR

**`ir.go`** — add after `IRIncIC` (line 268):
```go
IRDecIC      // DEC R15 — undo IncIC for non-emitted terminators
```
Add string at line 364: `IRDecIC: "dec_ic",`
Add format case at line 437: `case IRDecIC: return "dec_ic"`

### Step 2: Add helper method

**`highlevel.go`** — add after `IncIC()` (line 188):
```go
func (e *Emitter) DecIC() { e.emit(IRInstr{Op: IRDecIC}) }
```

### Step 3: Add x86-64 lowering

**`lower_amd64_ops.go`** — add dispatch case after `IRIncIC` (line 1537):
```go
case IRDecIC:
    lc.opsDecIC()
```

Add lowering function after `opsIncIC()` (line 1639):
```go
func (lc *lowerOps) opsDecIC() {
    p := lc.c.NewProg()
    p.As = x86.ADECQ
    p.To.Type = obj.TYPE_REG
    p.To.Reg = goasm.REG_AMD64_R15
    lc.c.Append(p)
}
```

### Step 4: Emit DecIC for non-emitted terminators

**`jit_emit_ir.go`** — in `emit32()` (around line 1105), save numInsns before processing and check after:

```go
func (e *emitter) emit32(insn uint32) {
    ...
    savedNumInsns := e.numInsns     // ADD THIS

    e.emitLabel()
    e.emitBudgetCheck()

    switch opcode {
    ...
    }
    
    // ADD THIS: undo IncIC for instructions that terminated without emitting
    if e.terminated && e.numInsns == savedNumInsns && e.lockstepMode {
        e.irEm.DecIC()
    }
}
```

**`jit_emit_ir.go`** — in `emitRVC()` (around line 2785), same pattern:

```go
func (e *emitter) emitRVC(insn uint16) {
    savedNumInsns := e.numInsns     // ADD THIS

    e.emitLabel()
    e.emitBudgetCheck()
    ...
    switch quad {
    ...
    }
    if !e.terminated {
        e.advancePC(2)
    }
    
    // ADD THIS
    if e.terminated && e.numInsns == savedNumInsns && e.lockstepMode {
        e.irEm.DecIC()
    }
}
```

### Why this works for all cases

| Case | advancePC called? | numInsns changed? | DecIC emitted? | Correct? |
|------|------------------|-------------------|---------------|----------|
| Normal insn (LUI, ADD...) | Yes | Yes | No | IC counts it |
| Branch (taken or not) | Yes | Yes | No | IncIC is before branch IR, counts it |
| ECALL | Yes (before emitSyscall) | Yes | No | IC counts it |
| EBREAK | Yes (before emitReturn) | Yes | No | IC counts it |
| JAL/JALR | Yes (inside emitJAL/emitJALR) | Yes | No | IC counts it |
| **CSR (the bug)** | **No** | **No** | **Yes** | **IncIC undone** |
| Unknown opcode | No | No | Yes | IncIC undone |
| Sub-fn unsupported | No (caller skips advancePC) | No | Yes | IncIC undone |

## Files to modify

1. `ir.go` — ~3 lines (enum + string + format)
2. `highlevel.go` — ~2 lines (helper method)
3. `lower_amd64_ops.go` — ~10 lines (dispatch + lowering function)
4. `jit_emit_ir.go` — ~8 lines (savedNumInsns + DecIC check in emit32 + emitRVC)

## Verification

```bash
# Primary test — must pass with budget=2 (the failing case)
go test -v -run TestRISCVTests_Lockstep_UI/add .

# Full lockstep suite
go test -v -run TestRISCVTests_Lockstep_UI .

# Regression — all existing tests
go test -v .
```
