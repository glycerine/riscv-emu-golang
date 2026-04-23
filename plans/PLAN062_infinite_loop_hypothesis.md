# Debug: TestJIT_BenchGuest_Smoke hang

## Context
Test ran in 0.81s at 71f53b0, now hangs indefinitely. The diff introduces function-level JIT compilation with:
- Non-terminal ECALL (inline syscall, block continues past ECALL)
- Dispatch table (IRDispatchBarrier) for mid-function re-entry
- All `jitcall.Call` → `jitcall.CallAOT` with new `pc` parameter at sret[120]

## Primary Hypothesis: Dispatch table fallback → infinite loop

The dispatch table fallback path returns `{status=jitOK, IC=RBP}`. But RBP (the IC register) hasn't been initialized by JIT code at that point — it contains Go's callee-saved RBP (a stack frame pointer, definitely non-zero).

In RunJIT (jit.go:776-783):
```go
case jitOK:
    if res.IC == 0 { break }  // bail label detection
    continue                   // <-- takes this path because IC != 0
```

So RunJIT sees IC != 0, does `continue`, enters the same block with the same PC, dispatch table fails again → infinite loop with no progress.

This happens whenever a block is entered at a PC that IS in the block map (so lookupBlock finds it) but is NOT in DispatchPCs (so the dispatch table doesn't match).

## Secondary Hypothesis: ECALL continuation dispatch mismatch
If the ECALL continuation PCs are in the block map but the dispatch table routes incorrectly, similar infinite dispatch could occur.

## Debug Plan
1. Add `vv()` prints in RunJIT dispatch loop to observe PC, IC, status on each iteration
2. Run test with 30s timeout to capture the loop behavior
3. Confirm which PC is looping and whether IC is non-zero garbage
4. If confirmed, fix by initializing IC register before dispatch table, or by making dispatch fallback set IC=0

## Key Files
- `/Users/jaten/ris/jit.go:710-913` — RunJIT dispatch loop
- `/Users/jaten/ris/ir/lower_amd64.go:434-468` — emitDispatchTable
- `/Users/jaten/ris/jit_emit_ir.go:666-720` — finalize + dispatch setup
- `/Users/jaten/ris/debug_vizjit_dir/` — vizjit dumps of generated code
