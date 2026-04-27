# Hybrid Block-Size Cap Following the libriscv Model

## Context

Large JIT blocks are expensive to compile. The `rv64ui-p-ld_st` test ELF produced a 24K-instruction IR block with 65K+ VRegs, taking 2+ minutes to compile. The VReg uint64 fix prevents the infinite-loop hang, but compilation time remains proportional to block size. Large blocks also thrash the register file — with ~10 host registers, every instruction past ~100 guest instructions generates pure spill traffic.

libriscv solves this with a hybrid cap: accumulate guest instructions up to a threshold (`ITS_TIME_TO_SPLIT = 5000` for TCC), then stop at the next **natural break point** (JALR, EBREAK, WFI, C.JR, C.JALR). This avoids both arbitrary mid-block truncation and unbounded block growth.

We adopt this model, using our existing `classifyFlow()` function (jit_decode.go:19) as the natural-break-point oracle.

## Design

### Cap Variable

```go
// jit.go (or jit_emit_ir.go, near the top)
var PerBlockCapTimeToSplit int64 = 5000
```

Package-level variable, adjustable at runtime. Set to 0 to disable the cap entirely (restoring current behavior). Tests can lower it (e.g., 100) to exercise the split logic on small ELFs.

### Stopping Condition

After `numInsns >= PerBlockCapTimeToSplit`, the emitter checks the **next** instruction's flow class via `classifyFlow(mem, e.pc)`. If the flow class is a natural boundary, the block terminates cleanly.

Natural boundaries (anything except sequential or conditional-branch):

| `classifyFlow` result | Instructions | Stop? |
|---|---|---|
| `flowTerm` (4) | JALR, C.JR, C.JALR, ECALL, EBREAK, CSR, JAL rd!=0/1 | **Yes** |
| `flowCall` (3) | JAL rd=ra (function call) | **Yes** |
| `flowJump` (2) | JAL rd=0, C.J (unconditional jump) | **Yes** |
| `flowBranch` (1) | BEQ, BNE, BLT, etc., C.BEQZ, C.BNEZ | No |
| `flowSeq` (0) | All other instructions | No |

Helper function:

```go
func isStoppingFlow(fc flowClass) bool {
    return fc >= flowJump  // flowJump=2, flowCall=3, flowTerm=4
}
```

### Hard Cap (safety net)

If no natural boundary appears within `2 * PerBlockCapTimeToSplit` instructions, force-terminate anyway. This handles pathological code with no jumps/calls (e.g., giant unrolled arithmetic).

### Where the Cap Is Enforced

**In `emitBlockRange`** (jit_emit_ir.go, the emit loop at line 996). This is the sequential instruction walk — exactly the right place because:

1. `e.numInsns` tracks guest instruction count accurately (including fused pairs)
2. `classifyFlow` can be called on `e.pc` (the next instruction to emit)
3. When the loop `break`s, `finalize()` already emits a clean fall-through exit via `emitChainableReturn(e.pc)` (line 727-729) — no special exit code needed
4. The dispatcher subsequently compiles from `e.pc` as a fresh block — seamless continuation

**Not in `scanRegion`** — the user explicitly chose to keep scanRegion unlimited for whole-function discovery. The emitter cap is the control point. (A future optimization could add a generous scanRegion cap at `3 * PerBlockCapTimeToSplit` to avoid scanning 100K+ PCs when only 5K will be emitted, but this is not required for correctness.)

## Implementation

### File: `jit_emit_ir.go`

**1. Add the package-level variable** (near the top, after imports):

```go
var PerBlockCapTimeToSplit int64 = 5000
```

**2. Add the helper** (near `classifyFlow` usage or in jit_decode.go):

```go
func isStoppingFlow(fc flowClass) bool {
    return fc >= flowJump
}
```

**3. Modify the emit loop** (line 996). Insert the cap check as the first thing inside the loop, before the `e.visited` check:

```go
// Emit IR (populates regsUsed via xreg/xregDst calls).
for !e.terminated && e.pc < e.regionEnd {
    // ── Block size cap (libriscv hybrid model) ──
    // After exceeding the soft cap, stop at the next natural break
    // point (flowTerm, flowCall, or flowJump) as determined by
    // classifyFlow. Hard cap at 2x prevents unbounded growth if
    // no natural break appears.
    if PerBlockCapTimeToSplit > 0 && int64(e.numInsns) >= PerBlockCapTimeToSplit {
        if int64(e.numInsns) >= PerBlockCapTimeToSplit*2 {
            break // hard cap
        }
        fc, _, _ := classifyFlow(e.mem, e.pc)
        if isStoppingFlow(fc) {
            break // natural boundary after soft cap
        }
    }

    if e.visited[e.pc] {
        // ... existing visited-PC handling ...
```

When `break` fires, the loop exits with `e.terminated == false`. `finalize()` at line 727 sees `!e.lastIRWasTerminator()` and emits `emitChainableReturn(e.pc)` — a clean block exit that sets PC to the uncompiled instruction. The dispatcher compiles from there on the next dispatch cycle. Block chaining then patches the exit to jump directly.

### File: `jit_emit_ir.go` (emitter struct)

The emitter needs access to `mem` for the `classifyFlow` call. Check that `e.mem` is already stored:

```go
type emitter struct {
    mem *GuestMemory  // ← already present (line 975-976)
    ...
}
```

Yes, `e.mem` is set at line 976: `mem: mem`.

## What This Does NOT Change

- `scanRegion` — remains unlimited (whole-function discovery)
- Lockstep mode — budget checks still work independently (per-instruction IC)
- AOT path — `emitBlockLinear` calls `emitBlockRange`, so the cap applies there too (correct: AOT blocks get the same cap)
- Block chaining — split blocks get chained automatically by the dispatch loop
- `classifyFlow()` — unchanged; we only call it as a predicate

## Verification

```bash
# 1. Existing tests still pass (cap=5000 is generous, most test blocks are small)
go test -v -run TestRISCVTests .

# 2. Lockstep test is faster (smaller blocks compile faster)
go test -v -run TestRISCVTests_Lockstep_UI/ld_st .

# 3. Targeted test: lower the cap and verify splits work
#    (add a small test that sets PerBlockCapTimeToSplit=100, runs a >100-insn ELF,
#     and checks correct execution)
go test -v -run TestBlockCap .

# 4. Benchmarks still work (blocks split transparently)
make bench-quick
```

## Critical Files

| File | Change |
|---|---|
| `jit_emit_ir.go` | Add `PerBlockCapTimeToSplit`, `isStoppingFlow`, cap check in emit loop |
| `jit_decode.go` | (read-only: `classifyFlow` used as-is) |
