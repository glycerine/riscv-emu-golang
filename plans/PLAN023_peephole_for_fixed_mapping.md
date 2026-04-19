# Plan: Peephole Spill/Reload Elision for Fixed Static Mapping

## Context

The fixed static allocator maps ~7 RISC-V registers to native x86-64 registers and spills the rest. Each spilled VReg generates a `spillLoad` on every read and `spillStore` on every write — even when consecutive instructions use the same spilled VReg. For example:

```asm
mov R10, [RSP+slot_t3]   ; load t3 into scratch
add R10, RAX             ; t3 + a0
mov [RSP+slot_t3], R10   ; store t3 back
mov R10, [RSP+slot_t3]   ; REDUNDANT: t3 is already in R10!
add R10, RCX
```

The CPU's rename engine handles false dependencies on R10 automatically, but it **cannot** eliminate the redundant memory traffic. That's our job.

## Where the optimization lives

The optimization is a **scratch register cache in the lowerer** (`lower_amd64.go`). Rationale:

- The allocator produces a static mapping (VReg → register or spill slot). It has no concept of "R10 currently holds VReg X" — that's runtime emission state.
- The lowerer's `use()`/`defCommit()` are where spill loads/stores are emitted. The cache sits right there, eliding redundant loads with zero architectural changes.
- Both allocators (ELS and Fixed) benefit, though Fixed benefits far more due to its higher spill rate.

## Plan

### Step 1: Add scratch cache to `lowerCtx` (`ir/lower_amd64.go`)

```go
// scratchEntry tracks which spilled VReg's value is resident in a scratch register.
type scratchEntry struct {
    vr    VReg
    valid bool
}

type lowerCtx struct {
    // ... existing fields ...

    // Scratch cache: elides redundant spill loads when consecutive instructions
    // use the same spilled VReg. scratchCache[0] tracks R10, scratchCache[1] tracks R11.
    scratchCache [2]scratchEntry
}
```

### Step 2: Cache check in `use()` (~5 lines, line 510)

Before the `spillLoad` call at line 530, check the cache:

```go
if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
    if lc.isVRegFP(v) {
        scr := lc.fpScratch(scratchIdx)
        lc.fpSpillLoad(lc.alloc.SpillSlot[v], scr)
        return scr
    }
    scr := lc.scratch(scratchIdx)
    // Peephole: skip load if this VReg is already in this scratch register.
    if lc.scratchCache[scratchIdx].valid && lc.scratchCache[scratchIdx].vr == v {
        return scr
    }
    lc.spillLoad(lc.alloc.SpillSlot[v], scr)
    lc.scratchCache[scratchIdx] = scratchEntry{v, true}
    return scr
}
```

### Step 3: Cache update in `defCommit()` (~3 lines, line 557)

After the `spillStore`, record that the scratch register now holds this VReg's value:

```go
func (lc *lowerCtx) defCommit(v VReg, hostReg int16) {
    if v == VRegZero { return }
    if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
        if isXMMReg(hostReg) {
            lc.fpSpillStore(hostReg, lc.alloc.SpillSlot[v])
        } else {
            lc.spillStore(hostReg, lc.alloc.SpillSlot[v])
            // Update scratch cache: this scratch register now holds v's value.
            if hostReg == amd64Scratch1 {
                lc.scratchCache[0] = scratchEntry{v, true}
                // Invalidate other scratch if it held the same VReg (stale value).
                if lc.scratchCache[1].vr == v { lc.scratchCache[1].valid = false }
            } else if hostReg == amd64Scratch2 {
                lc.scratchCache[1] = scratchEntry{v, true}
                if lc.scratchCache[0].vr == v { lc.scratchCache[0].valid = false }
            }
        }
    }
}
```

### Step 4: Cache invalidation

Add `invalidateScratchCache()` method:

```go
func (lc *lowerCtx) invalidateScratchCache() {
    lc.scratchCache[0].valid = false
    lc.scratchCache[1].valid = false
}
```

Invalidate at control flow boundaries and instructions that clobber scratch registers outside `use()`/`defCommit()`:

1. **`IRLabel`** — join point, scratch may hold wrong value if jumped to
2. **`IRBranch` / `IRBranchImm`** — after branch emission (fall-through cache may not match branch target)
3. **`IRJump`** — unconditional jump
4. **`IRRet` / `IRRetDyn`** — block exit
5. **`IRCall`** — external call may clobber everything
6. **Div/Mul lowering** (lines 831-973) — explicitly uses R10/R11 for operand shuffling
7. **Shift lowering** (lines 990-1060) — may save/restore CX to R11
8. **`IRStoreX` lowering** (line 1215+) — uses R10/R11 for address computation
9. **`IRMulHS` / `IRMulHSU`** (lines 950-973) — uses R10 for sign correction

The simplest safe approach: in the main `lowerInsn()` switch, call `invalidateScratchCache()` for all cases EXCEPT simple ALU ops, loads, and stores that go through the standard `use()`/`def()`/`defCommit()` path. This means the cache is conservative — it only elides reloads across consecutive simple instructions.

Concretely, **keep the cache valid** only for these ops:
- `IRAdd`, `IRSub`, `IRAnd`, `IROr`, `IRXor`, `IRSextW`, `IRNot`
- `IRAddImm`, `IRAndImm`, `IROrImm`, `IRXorImm`
- `IRMov`, `IRMovImm`
- `IRClz`, `IRCtz`, `IRPopcount`, `IRBswap`
- `IRSet`, `IRSetImm`
- `IRLoad`, `IRStore` (standard memory ops)

**Invalidate** for everything else (shifts, div/mul, branches, labels, calls, FP ops, indexed loads/stores, etc.).

## Files to modify

| File | Change |
|------|--------|
| `ir/lower_amd64.go` | Add `scratchEntry` type, `scratchCache` field, `invalidateScratchCache()`, modify `use()` and `defCommit()`, add invalidation calls in `lowerInsn()` |

Single file, ~30 lines of changes.

## Verification

```bash
# Correctness — lockstep tests compare JIT vs interpreter register-by-register + full memory
go test -v -run TestRISCVTests_Lockstep_UI .
go test -v -run TestRISCVTests_Lockstep_UM .

# Full riscv-tests suite
go test -v -run TestRISCVTests .

# JIT unit tests
go test -v -run TestJIT .

# Benchmark (requires make bench-setup)
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=3x ./bench/
```
