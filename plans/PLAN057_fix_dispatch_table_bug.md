# Plan: Dispatch Table Unit Test + Root Cause Fix

## Context

The PC dispatch table was implemented (Steps 1-7) but bench_guest still hangs at pc=0x100c with ic=4100. The dispatch table CMP/JEQ chain in the lowerer's prologue appears correct, but runtime behavior is wrong.

**Root cause found during this analysis**: The dispatch table is emitted in `emitPrologue()` (`ir/lower_amd64.go:441-457`), which runs BEFORE the lowerer processes IR instructions. Register loads (IRLoad from x[] array) are IR instructions prepended at `jit_emit_ir.go:1016-1017`. The dispatch JEQ targets jump to labels placed at guest instruction positions — which are AFTER the register loads in the lowered native code. Result: mid-function dispatch entry skips register loads, leaving physical registers uninitialized.

The native code layout is:
```
[prologue: pinned reg setup, IC=0, chain NOP, frame alloc]
[dispatch: MOVQ [RBX+120], R10; for each PC: MOVABS R11, PC; CMP; JEQ label]
[register loads from x[] -> physical regs]          <-- dispatch SKIPS these
[guest insn 0x1000]
[guest insn 0x1004]
[LABEL_0x1008:]                                     <-- dispatch lands HERE
[guest insn 0x1008]
```

libriscv's approach: `load regs; switch(pc) { case ...: goto ... }` — register loads come FIRST.

## Step 1: Write dispatch table unit test

**File:** `jit_aot_test.go` (new)

### 1a. Test helper: `jitcallCallAOT`

Wraps `jitcall.CallAOT` with dummy decoder_cache params (no JALR in test, so decoder_cache unused):

```go
func jitcallCallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
    memBase uintptr, memMask uint64, pc uint64) jitcall.Result {
    return jitcall.CallAOT(fn, x, f, fcsr,
        memBase, memMask,
        0, 0,  // decoderCacheBase, decoderCacheMask (no JALR in test)
        0, 0,  // vaddrBegin, segSize (unused without JALR)
        pc)
}
```

### 1b. Test helper: `compileAOTBlock`

Extracts the AOT compilation pipeline (Pass 1 of `jitCompileAOTSegment`, `jit_aot.go:47-95`) into a reusable test helper:

```go
func compileAOTBlock(t *testing.T, mem *GuestMemory, startPC, endPC uint64) (
    fn uintptr, cleanup func(),
) {
    t.Helper()
    res := emitBlockLinear(mem, startPC, endPC)
    // require res != nil, res.block has DispatchPCs

    j := NewJIT()
    pool := ir.AMD64Pool(res.block)
    pinned := ir.AMD64Pinned()
    alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

    ctx := goasm.New(goasm.AMD64)
    ctx.Append(ctx.NewATEXT())
    _, err := ir.LowerAMD64AOT(ctx, res.block, alloc)
    // require err == nil

    code, err := ctx.Assemble()
    // require err == nil, len(code) > 0

    execMem, err := allocExec(len(code))
    // require err == nil
    copy(execMem, code)

    fn = uintptr(unsafe.Pointer(&execMem[0]))
    cleanup = func() { syscall.Munmap(execMem) }
    return
}
```

### 1c. Test case: `TestAOT_DispatchTable`

**Guest instructions** (4 instructions, range [0x1000, 0x1010)):

| PC     | Instruction           | Encoding                                    |
|--------|-----------------------|---------------------------------------------|
| 0x1000 | `ADDI x1, x0, 42`    | `ienc(opOPIMM, 0, 1, 0, 42)`               |
| 0x1004 | `ADDI x2, x0, 1`     | `ienc(opOPIMM, 0, 2, 0, 1)`                |
| 0x1008 | `ADDI x3, x0, 77`    | `ienc(opOPIMM, 0, 3, 0, 77)`               |
| 0x100c | `BNE x10, x11, -4`   | `benc(opBRANCH, 1, 10, 11, -4)` target=0x1008 |

BNE x10, x11: both are 0 (never written) so not taken at runtime. But `collectInternalTargets` sees the backward branch and adds 0x1008 to DispatchPCs.

The block ends at 0x1010 (implicit return after BNE falls through, no ECALL needed).

**Sub-test A: entry at startPC (0x1000)**
```go
// Zero all registers, call with pc=0x1000
result := jitcallCallAOT(fn, &cpu.x, &cpu.f, &cpu.fcsr,
    cpu.mem.Base(), cpu.mem.Mask(), 0x1000)

assert x[1] == 42   // ADDI x1, x0, 42 executed
assert x[2] == 1    // ADDI x2, x0, 1 executed
assert x[3] == 77   // ADDI x3, x0, 77 executed
assert result.PC == 0x1010
```

**Sub-test B: entry at dispatch target (0x1008)**
```go
// Zero all registers, call with pc=0x1008
result := jitcallCallAOT(fn, &cpu.x, &cpu.f, &cpu.fcsr,
    cpu.mem.Base(), cpu.mem.Mask(), 0x1008)

assert x[1] == 0    // ADDI x1 SKIPPED -- dispatch bypassed it
assert x[2] == 0    // ADDI x2 SKIPPED -- dispatch bypassed it
assert x[3] == 77   // ADDI x3 executed -- dispatch routed here
assert result.PC == 0x1010
```

**How this exposes the bug**: With the current code (dispatch in prologue, before register loads), sub-test B will fail because dispatch jumps past the register loads. Physical registers hold junk, so x[3] won't be 77 — the ADDI x3, x0, 77 uses VR0 (the zero register source) which may not map to zero if the register load for it was skipped.

**Once fixed**: Sub-test B passes because register loads execute before dispatch, VR0 correctly provides 0, and ADDI x3 computes 0+77=77.

## Step 2: Fix the dispatch table ordering

**File:** `ir/lower_amd64.go`

Move the dispatch table from `emitPrologue()` to a new lowerer phase that runs AFTER lowering all register-load IR instructions but BEFORE lowering the first guest-instruction IR.

Approach: the lowerer already walks `block.Instrs[0:]` sequentially. The first `len(loads)` instructions are register loads (prepended by emitter at `jit_emit_ir.go:1016-1017`). After lowering instruction index `len(loads)-1`, emit the dispatch table, then continue with guest IR.

This requires the lowerer to know where register loads end. Two options:

**Option A** (recommended): Tag the boundary in the IR. Add a sentinel IR op `IRDispatchBarrier` after the register loads. The lowerer emits the dispatch table when it encounters this op.

**Option B**: Pass `numRegLoads` from the emitter to the lowerer via the Block struct. The lowerer emits the dispatch table after processing that many instructions.

Option A is cleaner — the lowerer doesn't need to count instructions; it just reacts to the sentinel.

Changes to `jit_emit_ir.go` (around line 1016):
```go
if len(loads) > 0 {
    e.irEm.Block.Instrs = append(loads, e.irEm.Block.Instrs...)
    // Insert dispatch barrier after loads.
    barrier := ir.IRInstr{Op: ir.IRDispatchBarrier}
    e.irEm.Block.Instrs = slices.Insert(e.irEm.Block.Instrs, len(loads), barrier)
    // Fix label indices (shifted by len(loads) + 1 for barrier).
    for lab, idx := range e.irEm.Block.Labels {
        e.irEm.Block.Labels[lab] = idx + len(loads) + 1
    }
}
```

Changes to `ir/ir.go`:
- Add `IRDispatchBarrier` to the IR op enum

Changes to `ir/lower_amd64.go`:
- Remove dispatch table from `emitPrologue()` (lines 435-457)
- Add case in the instruction lowering switch for `IRDispatchBarrier`:
  ```go
  case IRDispatchBarrier:
      lc.emitDispatchTable()  // extracted from old emitPrologue code
  ```

The dispatch table assembly stays the same — just moves from prologue to after register loads. After the register loads are lowered (physical regs populated from x[]), the dispatch CMP/JEQ chain routes to the right guest instruction label. Fall-through reaches the first guest instruction (startPC entry).

## Critical Files
1. `jit_aot_test.go` (new) — test helper + TestAOT_DispatchTable
2. `ir/ir.go` — add `IRDispatchBarrier` op constant
3. `ir/lower_amd64.go` — move dispatch from prologue to IRDispatchBarrier handler
4. `jit_emit_ir.go` — insert IRDispatchBarrier after prepended loads

## Verification
1. `go test -v -run TestAOT_DispatchTable .` — both sub-tests pass
2. `go test -v -run TestJIT_BenchGuest_Smoke ./bench/` — no hang
3. `go test -v -run 'TestJIT_|TestAOT_|TestBloat' .` — no regressions
4. `cd ~/ris && make hello-lib` — still 1 asm file
