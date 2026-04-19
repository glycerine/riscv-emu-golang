# Plan: Fix Fault Label Placement — Unblock 84 Failing JIT PCs

## Context

The native IR JIT compiles only 2 out of 86 attempted blocks. The other 84 fail with
`"ir.LowerAMD64: N unresolved forward labels"` because `loadFaultLabel` and
`storeFaultLabel` are pre-allocated but never placed in `finalize()`. ANY block
containing a load or store fails to compile, falling back to the interpreter for
2.5 billion instructions (99.99% of execution).

### Root Cause (confirmed via instrumentation)

`MaskedLoad` and `GuestStore` (in `ir/highlevel.go:26,53`) emit forward branches
`Branch(tmp1, VRegZero, NE, faultLabel)` targeting the fault labels. These branches
are registered as pending forward references in the lowerer. Since `PlaceLabel` is
never called for the fault labels, the pending map is non-empty at line 301 of
`ir/lower_amd64.go`, causing the error return.

### Previous Crash (resolved)

A prior attempt to place fault labels crashed at ~memMask (SIGSEGV at
0xFFFFFFFFFC000000). That crash occurred while chain exit infrastructure, fcsr
prologue changes, and fault labels were all being modified simultaneously. Those
concurrent changes have since been reverted — the prologue is back to the original
form, `emitChainableReturn` is a pass-through to `emitReturn`, and chain entry is
disabled. The crash is expected to not reproduce.

## Implementation

### Step 1: Set hasLoadFault/hasStoreFault flags

**File: `jit_emit_ir.go`**

The flags `hasLoadFault` and `hasStoreFault` (line 67-68) are declared but never
set. Set them in every code path that calls `MaskedLoad` or `GuestStore`:

- After `emitLoad` calls to `MaskedLoad` (~lines 1389, 1397): `e.hasLoadFault = true`
- After `emitStore` calls to `GuestStore` (~lines 1419, 1427): `e.hasStoreFault = true`
- After `emitFPLoad` calls to `MaskedLoad` (~lines 1451, 1455, 1458): `e.hasLoadFault = true`
- After `emitFPStore` calls to `GuestStore` (~lines 1481, 1486, 1488): `e.hasStoreFault = true`
- After alignment-check `Branch` to fault labels (~lines 1451, 1481): already
  covered since the MaskedLoad/GuestStore that follows also sets the flag.

### Step 2: Place fault labels in finalize()

**File: `jit_emit_ir.go`, function `finalize()` (line 419)**

After the deferred exits loop (line 439) and before the `return &emitResult{}`
(line 441), add:

```go
// Fault handlers: place labels only if the block contains loads/stores.
if e.hasLoadFault {
    e.irEm.PlaceLabel(e.loadFaultLabel)
    e.irEm.WriteBackAll()
    e.irEm.Ret(e.startPC, jitLoadFault, ir.VRegZero)
}
if e.hasStoreFault {
    e.irEm.PlaceLabel(e.storeFaultLabel)
    e.irEm.WriteBackAll()
    e.irEm.Ret(e.startPC, jitStoreFault, ir.VRegZero)
}
```

Design decisions:
- **PC = `e.startPC`**: The fault handler is shared across all loads/stores in the
  block. We return the block start PC so the interpreter replays from there and
  finds the exact faulting instruction.
- **FaultAddr = VRegZero (0)**: Avoids register pressure in the handler. The
  interpreter replay will produce the correct fault address.
- **WriteBackAll()**: Essential — dirty registers must be flushed before returning
  to Go, just like every other exit path.
- **Conditional placement**: Only emit the handler if the block actually references
  the label. Unreferenced labels are harmless to the lowerer (not in pending map)
  but conditional placement avoids dead code bloat.

### Step 3: No other files need changes

The lowerer, assembler, register allocator, and dispatch loop all work correctly
already. The ONLY missing piece was the PlaceLabel calls.

## Verification

```bash
# 1. IR unit tests (labels, lowering, highlevel)
go test -v -count=1 ./ir/...

# 2. JIT unit tests (load/store, faults, register state)
go test -v -count=1 -run 'TestJIT_' .

# 3. Full RISC-V ISA test suite
go test -v -count=1 -run 'TestRISCV' .

# 4. Dispatch stats — expect noJIT ≈ 0, interp fallback ≪ 2.5B
go test -v -run TestJIT_DispatchStats -timeout 120s ./bench/

# 5. Benchmark — expect significant MIPS improvement
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT_Fixed' -benchtime=1x ./bench/
```

If step 2 or 3 crashes with SIGSEGV, escalate to diagnostic mode: use
`jitCompileDebug` to dump the Prog listing and assembled bytes for the crashing
block, disassemble, and find the exact instruction causing the crash. A diagnostic
test template exists in `jit_test.go` and `jit_native.go:134-164`.

## Files to Modify

| File | Change |
|------|--------|
| `jit_emit_ir.go` | Set hasLoadFault/hasStoreFault flags; place fault labels in finalize() |

## Risks

1. **Previous crash recurrence**: Low risk. The concurrent prologue/chain changes
   that caused the crash are reverted. If it recurs, the crash address (~memMask)
   provides a strong diagnostic signal.
2. **Fault address = 0**: Acceptable. The interpreter replay from startPC produces
   the correct fault. Can be improved later with per-check fault address capture.
3. **WriteBackAll overhead**: Negligible. The fault path is cold (OOB accesses are
   rare in correct programs).
