# Plan: PC Dispatch Table for Function-Level Re-entry

## Context

Steps 1-7 of function-level compilation are done. The emitter, lowerer, and AOT driver produce one compiled block per function. The hello program works. But the bench_guest hangs because when a BudgetCheck exits at a mid-function PC (e.g., backward branch target), the Go dispatch loop re-enters the block at its prologue — which re-executes initialization code, creating an infinite loop.

libriscv solves this with a `switch(pc)` dispatch table at the top of each compiled function:

```c
static ReturnValues f_1000(CPU* cpu, uint64_t ic, uint64_t max_ic, addr_t pc) {
    // load regs from cpu->r[]
    switch (pc) {
    case 0x1000: goto f_1000_1000;
    case 0x101c: goto f_1000_101c;
    default: store regs; return;
    }
    f_1000_1000:
    // function body...
}
```

We need the same: pass `cpu.pc` to the JIT block, and emit a dispatch table in the prologue that jumps to the right mid-function label.

## Approach

### 1. Pass PC via sret buffer

**Files:** `internal/jitcall/call_amd64.s`, `internal/jitcall/call.go`

The sret buffer layout has a free slot at offset 120. Add a `pc` parameter to `CallAOT`:

```
CallAOT(fn, x, f, fcsr, memBase, memMask, dcBase, dcMask, vBegin, segSize, pc)
```

In the trampoline, write `pc` to `[SP+120]` before the CALL. The JIT prologue reads `[RBX+120]` after saving args to pinned regs.

Define `amd64SretPCOffset = 120` in `ir/lower_amd64.go`.

### 2. Emit dispatch table in IR

**Files:** `jit_emit_ir.go`, `ir/highlevel.go`

After register loads in `emitBlockRange`, emit an IR dispatch sequence. The emitter needs the set of re-entry PCs (from `collectInternalTargets`). For each re-entry PC, emit:

```
load.i64 dispatchPC = [sret + 120]
branch.eq dispatchPC, <re-entry-PC> -> label_at_that_PC
```

The re-entry PCs are:
- Backward branch targets (BudgetCheck exit PCs)
- ECALL continuation PCs (fallback exit PCs)

The function entry PC (startPC) doesn't need a dispatch case — it's the fall-through default.

The dispatch IR goes right after the register loads, before any guest instruction IR. For the hello function this adds 3 comparisons (0x101c, 0x1022, 0x1030). For the bench_guest main function it adds comparisons for each loop target.

### 3. Lower the dispatch in the prologue

**File:** `ir/lower_amd64.go`

The dispatch uses existing IR ops (IRLoad + IRBranch) which the lowerer already handles. No new lowerer code needed — the dispatch table is pure IR.

The one new thing: reading from `[RBX + 120]` in the prologue. This is an IRLoad from the sret base (RBX). The sret base is available as `amd64RegSret` (pinned to RBX) after the prologue's `MOVQ RDI, RBX`.

### 4. Register ALL re-entry PCs in decoder_cache and blocks map

**File:** `jit_aot.go`

With the dispatch table, re-entering the function at chainEntry for ANY registered PC is correct — the dispatch table routes to the right label. So register the function entry + all re-entry PCs (branch targets + ECALL continuations) in both decoder_cache and blocks map. Revert the "entry-only" change.

### 5. Update RunJIT callers to pass cpu.pc

**File:** `jit.go`

All `CallAOT` call sites need the extra `cpu.pc` argument. Three sites in RunJIT (sole segment, multi-segment, no-segment paths).

## Critical Files
1. `internal/jitcall/call_amd64.s` — add `pc` param at sret[120]
2. `internal/jitcall/call.go` — update `CallAOT` signature
3. `ir/lower_amd64.go` — define `amd64SretPCOffset = 120`
4. `jit_emit_ir.go` — emit dispatch table after register loads
5. `jit_aot.go` — register re-entry PCs in decoder_cache + blocks
6. `jit.go` — pass `cpu.pc` in all `CallAOT` calls

## Verification
1. `go test -run TestJIT_BenchGuest_Smoke ./bench/` — no hang
2. `go test -run 'TestJIT_|TestAOT_|TestBloat' .` — no regressions
3. `cd ~/ris && make hello-lib` — still 1 asm file
4. `make bench-quick` — performance check
