# Eliminate dead-code fall-through in terminated IR blocks

## Context

After landing Phase 5-B's inline ECALL (Steps 0–6 of the prior plan, now
all committed), the hellobench `GoCPU direct callback` metric is ~50 ns
vs libriscv's ~21 ns. A VizJit dump of one ECALL block
(`pc_0x0000101c.asm`, 185 bytes) shows an unreachable tail appended
after the cold-path `RET`:

```
== Host (tail of ECALL block) ==
...
RET                        ; cold path ends here (branch-not-taken path)

; ↓↓↓ everything below is unreachable ↓↓↓
MOVQ   AX, 80(R12)         ; WriteBackAll store for x10
MOVQ   $sentinel, R10      ; MOVABS for fall-through chain exit
JMP    R10
MOVQ   $0x1022, R10        ; slow-exit stub for the chain exit above
MOVQ   R10, (BX)
MOVQ   BP, 8(BX)
MOVQ   $0, 16(BX)
MOVQ   $0, 24(BX)
RET
```

These ~47 bytes per ECALL block come from `finalize()` in
`jit_emit_ir.go:637-675` which unconditionally calls
`e.emitChainableReturn(e.pc)` at line 642 — emitting `WriteBackAll() +
ChainExit()` — regardless of whether `e.terminated` is already true.
The lowerer then appends a slow-exit stub for each registered chain
exit, bloating the block further.

For ECALL blocks under Option D the whole tail is unreachable (both
the hot-path `JMP R10` and the cold-path `RET` have already left the
block). For a hot-loop ECALL site this wastes I-cache and hurts the
30ish-ns-per-call budget we're trying to close.

However the fall-through is **not** dead for every block — some
terminators exit by just setting `e.terminated = true` without emitting
any IR (CSR/unknown SYSTEM at jit_emit_ir.go:1085, unknown opcode at
:1090, several RVC-unknown-quad branches at :1397/2502/2512/2553/2569/
2623). Today those blocks rely on `finalize()`'s fall-through as their
only return path. Any fix must preserve that.

Goal: remove the dead tail from ECALL/EBREAK/JAL/JALR/branch blocks
while keeping CSR/unknown fallback behaviour intact.

## Approach

In `finalize()`, skip the initial `emitChainableReturn(e.pc)` call
whenever the last emitted IR op is already a terminator. Detect that
via the `ir.Block.Instrs` tail — no per-site audit of `e.terminated =
true` writers, which scales better and handles future terminator kinds
automatically.

Terminator IR ops are the ones whose lowered x86 unconditionally
leaves the block (either directly or via the dispatcher returning to
Go). Based on `ir/ir.go` IROp values and `ir/lower_amd64.go` lowerers,
the set is:
- `IRRet` — plain block return with status.
- `IRRetDyn` — dynamic-PC block return (used by JALR fallback).
- `IRSyscall` — under Option D this emits either a
  chain-exit-to-resumePC (hot) or a sret+RET (cold); both leave.
- `IRChainExit` — MOVABS+JMP R10 to the target's chainEntry.
- `IRJalrIC` — 2-way JALR inline cache; every branch of the lowered
  sequence ends in JMP R10 or RET.

If the last IR in `Block.Instrs` is one of these, the block has
already emitted its exit and `finalize`'s fall-through is dead. Skip
it. If not (CSR/unknown/empty-block cases), emit it as today.

The `deferredExits`, `gotoTargets`, and `deferredFaults` loops in
`finalize()` (lines 644-666) continue to run unconditionally — each
places its own label and emits its own `WriteBackAll+ChainExit` or
`WriteBackAll+Ret`. They are already self-contained per the
exploration; no coupling to the initial fall-through.

## Implementation

### File: `jit_emit_ir.go`

Add one helper on `emitter`:

```go
// lastIRWasTerminator reports whether the final IR instruction
// emitted into the current block is a terminator op whose lowered
// x86 unconditionally leaves the block. When true, finalize()'s
// fall-through emitChainableReturn is dead code and is skipped.
//
// Recognised terminators: IRRet, IRRetDyn, IRSyscall, IRChainExit,
// IRJalrIC. Other ops (loads, stores, ALU, labels, CSR/unknown
// no-op terminators that only set e.terminated = true) return false
// and the fall-through remains the block's exit path.
func (e *emitter) lastIRWasTerminator() bool {
    ins := e.irEm.Block.Instrs
    if len(ins) == 0 {
        return false
    }
    switch ins[len(ins)-1].Op {
    case ir.IRRet, ir.IRRetDyn, ir.IRSyscall, ir.IRChainExit, ir.IRJalrIC:
        return true
    }
    return false
}
```

Modify `finalize()` at line 642 to gate the initial fall-through:

```go
// Fall-through return. Emitted only when the last IR is not already
// a terminator. Blocks that set e.terminated = true without emitting
// a terminator IR (CSR/unknown SYSTEM, unknown opcode, unknown RVC
// quad) still get a return here, so the interpreter fallback path
// keeps working.
if !e.lastIRWasTerminator() {
    e.emitChainableReturn(e.pc)
}
```

Update the comment block at lines 638-641 to describe the new
semantics. Leave the `gotoTargets`, `deferredExits`, and
`deferredFaults` loops untouched.

No other files change. The `ir` package already exports the op
constants (`ir.IRRet`, etc.) used in the switch.

## Critical files referenced

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit_ir.go:637-675` — `finalize()` (only edit target)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit_ir.go:341-345` — `emitChainableReturn` (unchanged; still used)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/ir.go` — IROp constants (read-only dependency)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/lower_amd64.go:451-475` — `lowerChainExit` (stub bytes saved when ChainExit isn't emitted)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/lower_amd64.go:369-376` — lowerer's slow-exit stub loop (emits one stub per registered chain exit; skipping the finalize ChainExit means one fewer stub per ECALL block)

## Verification

1. **Build + unit tests** — `go build ./...`, then
   `GOCPU_VIZJIT_OFF=1 go test -timeout 120s . ./ir/`. All must pass.

2. **VizJit byte-size diff on one ECALL block** — with
   `GOCPU_VIZJIT=/tmp/vj-dedup`, run
   `go test -run TestInlineEcall_HelloEndToEnd .` and compare the
   `pc_0x0000101c.asm` file's "host code: … N bytes" header against
   the pre-fix baseline (185 bytes). Expected post-fix size: ~138
   bytes (~47 bytes saved).

3. **Fuzz suite** — `make fuzz-oracle`, `fuzz-fd`, `fuzz-rvc`,
   `fuzz-amo`, `fuzz-bitmanip`. All must pass to confirm CSR/unknown
   fallback still works under random instruction streams.

4. **Hello perf — 5 runs** — `make hello` five times, capture
   `GoCPU direct callback` mean ± stddev. Compare against the
   post-Step-6 baseline (~50 ns). Expected improvement: a few ns
   (the dead-code bytes free up one cache line per ECALL block; the
   hot path itself is unchanged).

5. **MIPS regression check** — `make bench-cpu`. Confirm
   `BenchmarkCPU_FullExecution_JIT_Fixed-8` stays ≥ 2200 MIPS.

6. **CSR/unknown-opcode regression probe** — the riscv-tests suite
   (`go test -run 'TestRISCVTests' .`) exercises instructions that
   trigger the CSR fallback path. Must pass.

## Risks

- **If `lastIRWasTerminator` misses a terminator op**: block tail emits
  an extra (dead) WriteBackAll+ChainExit — bytes wasted but no
  correctness loss. Same as today's behaviour.
- **If it reports `true` for an op that doesn't actually leave the
  block**: the block has no exit, control falls off the end of the
  lowered native code → SEGV. Catastrophic. Mitigation: the
  terminator set is small (5 ops) and each is already a terminator in
  the existing lowerer. Fuzz + full test suite catch any mislabel.
- **Future IR ops added to `ir.IROp` that are terminators**: must be
  added to the switch. Put a short doc-comment at the switch so
  future contributors know.

## Rollback

Revert the one commit. No flag is needed — the change is pure
codegen-size reduction on a path that's already correct.

## Expected outcome

Per-ECALL-block savings of ~47 bytes (one WriteBack store + MOVABS +
JMP + slow-exit stub). Smaller savings for EBREAK/JAL/JALR blocks (no
store if dirty[] is empty at exit, but the MOVABS+JMP+stub always
applies). For a large AOT binary with O(thousands) of terminator
blocks, this is ~100–200 KB of code. For the hellobench hot loop the
I-cache pressure drop may buy a few ns; if it doesn't, we still ship
the size win and look elsewhere for the remaining libriscv gap.
