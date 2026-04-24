# Plan: Vestibule Optimization — Eliminate Per-Block Reg Spill/Reload

## Context

We're at 2963 MIPS after the direct-destination optimization (Steps 0–2a done).
Target is ≥3644 MIPS. The user proposes a "vestibule" pattern: do frame
setup and register loads **once** when entering JIT world from Go, and
teardown **once** when exiting — not on every block-to-block chain.

## Current Chain Transition Cost (the problem)

When block A chains to block B, the current code does:

**Block A exit** (`rv8ChainExit`, lower_amd64_rv8.go:1427):
1. `storeRegsBack()` — write all 12 allocated int regs + FP regs to `[RBP+vr*8]`
2. Load sret from stack, write IC to sret
3. `ADDQ frameSize, RSP` — deallocate A's frame
4. `MOVABS RCX, target` + `JMP RCX` — jump to B's chain entry

**Block B entry** (chain entry NOP, lower_amd64_rv8.go:172):
1. `SUBQ frameSize, RSP` — allocate B's frame
2. `MOV RAX → [RSP+sretOffset]` — stash sret
3. Load all 12 allocated int regs from `[RBP+vr*8]` back into host regs
4. Load IC from sret, load memBase/memMask from sret

**Total: ~24 MOVs to memory + ~6 overhead instructions — just to transfer
control from one block to the next, when ALL registers stay in the SAME
host registers thanks to FixedStaticAllocator.**

This is pure waste. Block A writes R8 (a0) → `[RBP+80]`. Block B reads
`[RBP+80]` → R8. The value never changed registers. ~30 instructions of
round-trip through L1 cache for zero semantic work.

## Why This Is Safe

**FixedStaticAllocator** (`ir/regalloc_fixed.go`) gives every block the
identical register mapping: ra→RDX, sp→RBX, t0→RSI, t1→RDI, a0-a7→R8-R15.
Block A and block B agree on what every host register holds. No permutation
shuffle is needed.

**RBP is pinned** to the register file base across all blocks. It never
changes.

**The trampoline** (`internal/jitcall/call_amd64.s`) already saves/restores
callee-saved registers (RBX, RBP, R12-R15) and sets up the sret buffer.
It's the natural vestibule boundary.

## Proposed Design

### Phase 1: Lightweight chain exit (no storeRegsBack)

**File:** `ir/lower_amd64_rv8.go`, `rv8ChainExit`

Change the chain exit to skip the register spill/reload cycle entirely:

```
Current chain exit:                    New chain exit:
  storeRegsBack()   [12+ MOVs]          (nothing — regs stay in place)
  load sret                              load sret from stack
  write IC to sret                       write IC to sret
  ADDQ RSP                               ADDQ RSP
  MOVABS + JMP                           MOVABS + JMP
```

The new chain exit only does:
1. Load sret from stack → RAX
2. Write IC to sret.IC (preserve instruction counter)
3. `ADDQ frameSize, RSP` — deallocate frame
4. `MOVABS RCX, target` + `JMP RCX` — jump to B

This saves **~12 MOV instructions** (all of storeRegsBack).

### Phase 2: Lightweight chain entry (no reg reload)

**File:** `ir/lower_amd64_rv8.go`, `emitPrologue`

Add a **second chain entry point** that skips the register loads:

```
[first-entry path: MOV RSI→RBP, publish mem, zero IC, MOV RDI→RAX]
                ↓ fall through
[chain-entry-full]: NOP  ← existing chainEntryProg (slow stubs land here)
  SUBQ frameSize, RSP
  MOV RAX → [RSP+sretOffset]
  load regs from [RBP+vr*8]          ← ONLY for slow-stub / first entry
  load IC, memBase, memMask from sret
                ↓ fall through
[chain-entry-fast]: NOP  ← NEW: fast chain entry (patched chains land here)
  SUBQ frameSize, RSP
  MOV RAX → [RSP+sretOffset]
  load IC from sret                   ← regs already in place, just reload IC
  (memBase/memMask already in host regs)
```

Wait — this won't work cleanly because the frame allocation (SUBQ) has
to happen in both paths. Better approach:

**Split chain entry into two labels:**

```
chainEntryFull (existing): for first entry + slow stubs
  SUBQ frameSize, RSP
  MOV RAX → [RSP+sretOffset]
  load all regs from regfile
  load IC, memBase, memMask
  JMP body

chainEntryFast (new): for patched chains
  SUBQ frameSize, RSP
  MOV RAX → [RSP+sretOffset]
  load IC from sret (2 MOVs)
  fall through to body
```

The fast path saves **~12 MOV instructions** (all register loads).

### Phase 3: Match chain exit to fast entry

When `rv8ChainExit` is paired with `chainEntryFast`, the chain targets
the fast label. The slow exit stub (for unresolvable targets) still
targets `chainEntryFull`.

**Patching:** `patchChainTarget` in `jit.go` already writes the target
address into the MOVABS. It currently points at the target block's
`chainEntry` (= chainEntryFull). We add a second field `chainEntryFast`
to the block struct, and patch to that instead.

### Total Savings Per Chain Transition

| Operation | Before | After |
|-----------|--------|-------|
| storeRegsBack (block A exit) | 12 MOVs | 0 |
| reg loads (block B entry) | 12 MOVs | 0 |
| IC transfer | 3 MOVs | 2 MOVs |
| Frame dealloc/alloc | 2 instrs | 2 instrs |
| MOVABS+JMP | 2 instrs | 2 instrs |
| sret stash | 1 MOV | 1 MOV |
| **Total** | **~32 instrs** | **~7 instrs** |

Saves ~25 instructions per chain transition. With the benchmark doing
thousands of chains per second, this should be significant.

### Complications and Safety

1. **Slow exit stubs** must still do storeRegsBack + target chainEntryFull,
   because they return to Go (where regs must be in the regfile array).
   The slow stub at lower_amd64_rv8.go:1472 already does this correctly —
   it stores regs and writes the Result struct. No change needed.

2. **Non-chain returns** (IRRetDyn, IRRet) always go back to Go. They
   still call storeRegsBack. No change.

3. **ECALL/EBREAK** returns to Go. Still calls storeRegsBack. No change.

4. **IC (instruction counter)**: Must still be transferred via sret.IC
   because it's a per-block value that accumulates. The fast chain entry
   reads it from `[RAX+8]` (sret.IC) and loads it into its host reg/spill.
   This is 2 MOVs — acceptable.

5. **memBase/memMask**: These are constant for the entire execution
   (set once by the trampoline, published to sret by first-entry prologue).
   On the fast chain entry, they're already in their host registers (R-something)
   and don't need reloading. We just skip those loads.

6. **FP registers**: Same argument as integer registers — fixed allocation,
   identical mapping across blocks. Skip reload on fast chain entry.

7. **Spilled registers** (s2-s11, fs2-fs11): These live at `[RBP+vr*8]`
   permanently and are never loaded into host registers. They're accessed
   via `spilledRegFileOff` in the lowerer. No spill/reload issue — they
   stay in memory throughout.

## Critical Files

| File | Change |
|------|--------|
| `ir/lower_amd64_rv8.go` | Add `chainEntryFastProg`, skip reg loads in fast path, skip storeRegsBack in fast chain exit |
| `ir/lower_amd64.go` | Add `ChainEntryFastProg` to `LowerResult` |
| `jit.go` | Add `chainEntryFast` to block struct, patch to fast entry |
| `jit_native.go` | Record `chainEntryFast` offset from LowerResult |

## Verification

1. `go test ./...` — all tests green (especially chaining tests)
2. `make bench` — target ≥3644 MIPS
3. `go test -run TestBloat -v` — code size should be similar (we add
   a few instructions for the fast entry label but save nothing in code
   size — the savings are at runtime, not code size)
4. `go test -run TestChaining -v` — chaining tests verify correct behavior

## Execution Order

1. Add `chainEntryFastProg` label in `emitPrologue` (after full-load path)
2. Add fast chain exit variant (skip storeRegsBack)
3. Wire up `chainEntryFast` in block struct and patching
4. Test and benchmark
