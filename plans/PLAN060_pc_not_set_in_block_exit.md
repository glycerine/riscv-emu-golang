# Fix: TestJIT_BenchGuest_Smoke Hang — Chain Exits Don't Update Dispatch PC

## Context

`TestJIT_BenchGuest_Smoke` hangs because a JIT-compiled function-level block enters an infinite native loop. The goroutine is stuck in native code (`[running]`, stack unavailable). The test should complete in <1s but ran 52s before being killed.

**Root cause**: When a chain exit (or JALR IC hit, or decoder_cache hit) jumps directly to a target block's `chainEntry`, it bypasses `CallAOT` and therefore does NOT update `sret[120]` — the dispatch PC slot. The target block's dispatch table reads a stale `sret[120]` value. If it doesn't match any entry, execution falls through to the function body start, executing from the wrong point and entering an infinite loop.

The dispatch table (emitted at `IRDispatchBarrier`) routes re-entry to the correct label based on the guest PC read from `sret[120]`. This works when entering via `CallAOT` (which publishes the correct PC), but fails on chained entries because nobody updates `sret[120]`.

## Fix

### Step 1: Write targetPC to sret[120] in `lowerChainExit`

**File**: `ir/lower_amd64.go`, function `lowerChainExit` (~line 486)

Before the frame dealloc and JMP, add:
```go
// Write target guest PC to sret[120] so the target block's
// dispatch table routes to the correct label.
lc.loadImm64(ins.Imm, amd64Scratch2)                           // MOVABS R11, targetPC
lc.emitMR(x86.AMOVQ, amd64Scratch2, amd64RegSret, SretPCOffset) // MOVQ R11, 120(RBX)
```

Insert before the existing `if lc.frameSize > 0 { ... }` block at line 488. The targetPC is available as `ins.Imm`.

### Step 2: Write targetPC to sret[120] in `lowerJalrIC` hit paths

**File**: `ir/lower_amd64.go`, function `lowerJalrIC` (~line 537)

In both hit-0 (line 603) and hit-1 (line 592) paths, before the frame dealloc, add:
```go
lc.emitMR(x86.AMOVQ, amd64Scratch2, amd64RegSret, SretPCOffset) // MOVQ R11, 120(RBX)
```

`tgt` is already stashed in R11 (amd64Scratch2) at line 572, and is untouched by the hit path.

### Step 3: Write targetPC to sret[120] in `emitDecoderCacheLookup` hit path

**File**: `ir/lower_amd64.go`, function `emitDecoderCacheLookup` (~line 644)

Before the frame dealloc at line 718, add:
```go
lc.emitMR(x86.AMOVQ, tgt, amd64RegSret, SretPCOffset)  // MOVQ tgt, 120(RBX)
```

The `tgt` register is intentionally untouched by the decoder_cache sequence (R10 is the scratch; R11/tgt is preserved). Note: this line must go before the frame dealloc but after the TESTQ/JZ null-check at line 715. So insert it between lines 716 and 717 (after the JZ but before the ADDQ).

### Step 4: Add dispatch table fall-through handler (defense-in-depth)

**File**: `ir/lower_amd64.go`, function `emitDispatchTable` (~line 441)

After the CMP/JEQ chain for all dispatch entries, add a fall-through handler that returns to Go instead of silently executing from the wrong point:

```go
// Fall-through: no dispatch PC matched. Return to Go for re-dispatch.
// R10 still holds sret[120] (the dispatch PC loaded at the top).
lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0)          // sret.PC = R10
lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)             // sret.IC = RBP
lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 16)                     // sret.Status = jitOK
lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)                     // sret.FaultAddr = 0
lc.emitEpilogue()
```

However, this must NOT fire on first entry when `sret[120] == startPC` (which has no dispatch entry by default). To handle first entry correctly:

**Also in**: `jit_emit_ir.go`, function `emitBlockRange` (~line 1003), add the block's `startPC` to `DispatchPCs` so the dispatch table routes first entry correctly:

```go
dpcs[pc] = e.getOrCreateLabel(pc)  // startPC always routes to body start
```

This ensures that all entry paths (first entry, chain exit, budget check re-entry) go through the dispatch table and get routed to the correct label.

## Critical Files

- `ir/lower_amd64.go` — `lowerChainExit` (Step 1), `lowerJalrIC` (Step 2), `emitDecoderCacheLookup` (Step 3), `emitDispatchTable` (Step 4)
- `jit_emit_ir.go` — `emitBlockRange` (Step 4, add startPC to DispatchPCs)

## Verification

1. `cd ~/ris && go test -v -run TestJIT_BenchGuest_Smoke ./bench/` — should complete in <1s
2. `go test -v -run 'TestJIT_|TestAOT_|TestBloat' .` — no regressions
3. `go test -v -run 'TestRISCVTests_UI_JIT' .` — all 55 pass
4. `go test -v -run 'TestRISCVTests_Lockstep_UI' .` — all lockstep tests pass
5. `go test -v -run 'TestChaining_' .` — chain patching tests pass
6. `go test ./ir/` — ir package tests pass
7. `go test -v -run TestJIT_DispatchStats ./bench/` — verify reasonable dispatch stats
