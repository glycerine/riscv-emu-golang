# Fix: Move Dispatch Table After Register Loads via IRDispatchBarrier

## Context

The PC dispatch table (CMP/JEQ chain for mid-function re-entry) is emitted in `emitPrologue()` **before** register load instructions are lowered. When re-entering at a mid-function PC (e.g., 0x1008), the JEQ jumps to a label placed after the register loads, skipping them entirely. Physical registers hold junk values, causing crashes.

The asm output from the test confirms this ordering bug:
```
MOVQ  SI, R12          ← pinned reg setup
...
XORQ  BP, BP           ← IC = 0
NOP                     ← chain entry
MOVQ  120(BX), R10     ← DISPATCH TABLE (before reg loads!)
MOVL  $4104, R11
CMPQ  R10, R11
JEQ   0
MOVQ  8(R12), SI       ← REGISTER LOADS (dispatch skips these)
MOVQ  16(R12), DX
...
```

**Target ordering:** register loads FIRST, then dispatch table, then guest code.

## Changes

### 1. `ir/ir.go` — Add `IRDispatchBarrier` op

Add to the pseudo-ops section of the `IROp` enum (after `IRWriteback`, before `irOpCount`):
```go
IRDispatchBarrier  // emit PC dispatch table here (after reg loads)
```

Add to `irOpNames` array:
```go
IRDispatchBarrier: "dispatch_barrier",
```

Add case in `IRInstr.String()` to return `"dispatch_barrier"` (avoids ugly default formatting).

### 2. `jit_emit_ir.go` — Insert barrier after register loads

Replace lines 1016-1023. After prepending register load IRInstrs, insert an `IRDispatchBarrier` instruction if `Block.DispatchPCs` is non-empty. Shift label indices by `len(loads) + 1` (for barrier) instead of just `len(loads)`.

```go
needBarrier := len(irEm.Block.DispatchPCs) > 0
prepended := 0
if len(loads) > 0 {
    e.irEm.Block.Instrs = append(loads, e.irEm.Block.Instrs...)
    prepended += len(loads)
}
if needBarrier {
    barrier := ir.IRInstr{Op: ir.IRDispatchBarrier}
    e.irEm.Block.Instrs = slices.Insert(e.irEm.Block.Instrs, prepended, barrier)
    prepended++
}
if prepended > 0 {
    for lab, idx := range e.irEm.Block.Labels {
        e.irEm.Block.Labels[lab] = idx + prepended
    }
    ir.MaxVReg(e.irEm.Block)
}
```

### 3. `ir/lower_amd64.go` — Move dispatch from prologue to barrier handler

**3a.** Extract dispatch table emission into `emitDispatchTable()` method (same CMP/JEQ code, guarded by `len(lc.blk.DispatchPCs) > 0`).

**3b.** Delete lines 435-457 from `emitPrologue()` (the dispatch table section).

**3c.** Add `IRDispatchBarrier` to the scratch-cache invalidation switch (line 830, uses R10/R11).

**3d.** Add case in `lowerInstr()`:
```go
case IRDispatchBarrier:
    lc.emitDispatchTable()
```

### 4. `ir/lower_amd64_v2.go` — Handle barrier as no-op

Add `IRDispatchBarrier` to the pseudo-ops case to avoid `unhandled op` error:
```go
case IRMarkLive, IRMarkDead, IRWriteback, IRDispatchBarrier:
    // no-op
```

### 5. Test files — Add new op to existing test infrastructure

- `ir/ir_test.go`: op name table
- `ir/lower_amd64_test.go`: handled-ops list  
- `ir/fuzz_test.go`: VRegZero allowlists (2 locations)

## Resulting native code layout

```
[prologue: pinned reg setup, IC=0, chain NOP, frame alloc]
[register loads from x[] → physical regs]           ← ALWAYS execute
[IRDispatchBarrier → dispatch CMP/JEQ chain]         ← AFTER loads
[guest insn 0x1000]                                  ← fall-through (startPC)
[guest insn 0x1004]
[LABEL_0x1008:]                                      ← dispatch lands HERE
[guest insn 0x1008]                                     (regs initialized!)
```

## Critical files

- `/Users/jaten/ris/ir/ir.go`
- `/Users/jaten/ris/jit_emit_ir.go`
- `/Users/jaten/ris/ir/lower_amd64.go`
- `/Users/jaten/ris/ir/lower_amd64_v2.go`
- `/Users/jaten/ris/ir/ir_test.go`
- `/Users/jaten/ris/ir/lower_amd64_test.go`
- `/Users/jaten/ris/ir/fuzz_test.go`

## Verification

1. `cd ~/ris && go test -v -run TestAOT_DispatchTable .` — both sub-tests pass
2. `go test -v -run 'TestJIT_|TestAOT_|TestBloat' .` — no regressions
3. VizJit dump shows `dispatch_barrier` in IR section and dispatch CMP/JEQ chain after register loads in Host section
4. `go test -v -run TestJIT_BenchGuest_Smoke ./bench/` — no hang
