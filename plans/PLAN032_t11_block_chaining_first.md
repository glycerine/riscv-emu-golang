# T1.1 — Finish Block Chaining, Foundation-First

## Context

Baseline: Go JIT **Fixed Static Mapping** at ~4174 MIPS (Linux) / ~3322 MIPS (macOS) on `bench_guest`. Native ceiling is ~22954 / ~18035 MIPS. The biggest single per-block-exit cost is the `jitcall.Call` round-trip back to Go. The block-chaining infrastructure exists — except `emitChainableReturn` in `jit_emit_ir.go:207` is stubbed to emit a plain `IRRet`:

```go
// emitChainableReturn emits a chain exit for jitOK exits.
// These can be patched by Go to jump directly to the target block.
// TODO: re-enable chaining after fixing MOVABS offset calculation.
func (e *emitter) emitChainableReturn(pc uint64) {
    e.emitReturn(pc, jitOK)
}
```

Call sites: `jit_emit_ir.go` L454, L461, L468, L2106 — all "successor PC statically known" returns.

## What the "MOVABS offset bug" actually refers to

A decoded look at the pipeline:

- `ir/emit.go:246` exposes `Emitter.ChainExit(targetPC, exitIdx)` that emits `IRChainExit`.
- `ir/lower_amd64.go:371` `lowerChainExit` lowers that to:
  ```
  (if frameSize>0) ADDQ $frameSize, RSP
  MOVABS R10, 0x7BADC0DE7BADC0DE    ; 10 bytes: 49 BA <8-byte imm64>
  JMP    R10                        ; 3 bytes
  ```
  and records `chainExitInfo{targetPC, movProg}`.
- `ir/lower_amd64.go:309–316` after lowering, for each chain exit, appends a slow-exit stub to the block and records its first Prog as `StubProg`.
- `jit_native.go:89–106` after assembly:
  ```go
  // The MOVABS R10, imm64 encoding is: 49 BA <8 bytes imm64>.
  // The imm64 starts at byte offset +2 from the instruction start.
  patchOff := int(ce.MovProg.Pc) + 2
  stubAddr := codeBase + uintptr(ce.StubProg.Pc)
  binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(stubAddr))
  ```
- `jit.go:422–425` `patchChainTarget` overwrites those same 8 bytes at `codeBase+patchOffset` to point to another block's `chainEntry`.

**The "offset" in the TODO refers to `MovProg.Pc + 2` — the byte position of the imm64 inside the assembled MOVABS encoding.** This is only correct if:

1. The assembler actually picks the **10-byte MOVABS form** (REX.W+B | 0xBA | imm64). It will as long as the immediate doesn't fit in sign-extended int32. The sentinel `0x7BADC0DE7BADC0DE` is > int32 max, so MOVABS is forced.
2. The REX prefix is `0x49` and opcode is `0xBA` for R10 (so `+2` lands exactly on the imm64 first byte).
3. `movProg.Pc` is the byte offset of the *start* of the MOVABS (REX byte), not of the opcode byte or the imm. This is the Go assembler's convention but it's worth verifying rather than trusting.
4. Nothing the assembler inserts between MOVABS and JMP (e.g., alignment padding) misaligns anything. For `MOVABS`+`JMP` specifically there's no reason for padding — but if the ATEXT/prologue inserts padding, `movProg.Pc` is still accurate (it's an absolute offset, not relative).

All four assumptions can be checked by emitting a block containing `IRChainExit` and reading the bytes at `MovProg.Pc` and `MovProg.Pc+2` directly. That's what "start from a green foundation" means here — we prove assumptions 1–4 with a test before changing any behavior.

## Part A — Prove the foundation (byte-level tests, likely already green)

These tests go in `ir/lower_amd64_chain_test.go` (new, package `ir`) and `jit_chain_foundation_test.go` (new, package `riscv`). They exercise the chain-exit machinery *without* touching `emitChainableReturn` — we directly build an IR block with `Emitter.ChainExit`, lower it, assemble it, inspect bytes.

### A1 — `TestLower_ChainExit_MOVABS_EncodedAsExpected` (in `ir/`)

Build a minimal `ir.Block` that is: single `IRChainExit{targetPC=0xDEAD0000}`. Lower via `LowerAMD64`. Assemble. Assert:

1. `result.ChainExits` has exactly 1 entry.
2. The 2 bytes at `code[MovProg.Pc : MovProg.Pc+2]` equal `[]byte{0x49, 0xBA}`.
3. The 8 bytes at `code[MovProg.Pc+2 : MovProg.Pc+10]` equal little-endian `0x7BADC0DE7BADC0DE` = `DE C0 AD 7B DE C0 AD 7B`. (Sentinel before `jit_native`'s backpatch.)
4. The 3 bytes immediately after the MOVABS equal `[]byte{0x41, 0xFF, 0xE2}` (JMP R10: REX.B + FF /4).

This directly catches any bug in the offset math, in assembler encoding choice, or in REX/opcode assumption. Expected: **GREEN** today.

### A2 — `TestLower_ChainExit_StubBackpatched` (in package `riscv`)

Drive `jitCompileWith(res, useV2=false)` on a tiny `emitResult` whose IR block ends in a single `IRChainExit{targetPC=0xDEAD0000}`. Build the block manually via `ir.Emitter` without going through `emitBlock` (so we can keep this isolated from the rest of emission). After compilation:

1. `blk.chainExits` has one entry; `patchOffset` is within `[0, codeLen)`.
2. Read the 8 bytes at `blk.fn + patchOffset`. They should equal `ce.StubProg.Pc + codeBase` — i.e., the slow-exit stub's absolute address.
3. Read the bytes at `blk.fn + patchOffset - 2` — they should still be `0x49 0xBA`. Sanity check that the backpatch didn't clobber the REX/opcode.

Expected: **GREEN** today.

### A3 — `TestLower_ChainExit_PatchTarget_Roundtrip` (in package `riscv`)

Compile as in A2. Call `patchChainTarget(blk.fn, ce.patchOffset, 0xCAFEBABE12345678)`. Read the 8 bytes back. Assert equal. This proves the Go-side patch path writes to the same bytes the assembler placed the imm64 at.

Expected: **GREEN** today.

### A4 — `TestLower_ChainExit_MultipleExitsIndependent` (in `ir/`)

Block with two `IRChainExit`s (different `targetPC`). Assert:

1. Two entries in `ChainExits`.
2. Their `MovProg.Pc` values differ by at least 10 bytes (the MOVABS+JMP sequence length, ≥13).
3. Each `MovProg.Pc+2..+10` range contains the sentinel bytes (before backpatch).
4. The two `patchOffset`s don't overlap.

Expected: **GREEN** today.

### A5 — `TestLower_ChainExit_ChainEntry_NonZero_PastPrologue` (in `ir/`)

Lower any block. Assert `result.ChainEntryProg != nil` and `result.ChainEntryProg.Pc > 0` after assembly. Note: right now `chainEntryProg` is set where the NOP is emitted — we need to verify it's positioned *after* the prologue's IC-zero (`XORQ RBP, RBP`). The intent is that an inbound chain JMP skips arg setup AND IC-zero so IC accumulates.

Expected: possibly **RED** today if `chainEntryProg` is still nil (not emitted in the V1 prologue) or set to the wrong location. If RED, that's part of the real bug surface.

### Running Part A — report format

```
go test -run 'TestLower_ChainExit|TestJITChain_Foundation' -v ./... 
```

Capture PASS/FAIL for each. If all pass, the infrastructure is correct; the TODO's concern is historical. If any fail, the failing test's assertion pinpoints the actual bug. Do not proceed to Part C until Part A is fully green.

## Part B — Fix whatever Part A turns up (only if needed)

Likely root causes we'd investigate, ranked by plausibility given what we read:

- **B-α — `chainEntryProg` not emitted / misplaced.** Most likely candidate. Fix: emit the NOP in `emitPrologue` after `XORQ RBP, RBP`, assign to `lc.chainEntryProg`. Verify A5 goes green and T1.1-E (IC accumulation, later) stays green.
- **B-β — Assembler chose shorter MOV encoding.** Unlikely given sentinel > int32, but verify by inspecting A1 bytes. Fix would be: force the encoding explicitly by using a goasm helper that picks `AMOVABSQ` directly, not AMOVQ with constant.
- **B-γ — `MovProg.Pc` isn't the start of the MOVABS.** Would be a goasm bookkeeping bug. A1 catches it; the fix is either a goasm patch or recording the offset at emission time instead of post-assembly.
- **B-δ — Backpatch clobbers the wrong bytes.** A2 catches it; verify `patchOff` calculation.

For each failing test, adjust the narrowest thing and re-run A. Do not widen scope.

## Part C — Wire `emitChainableReturn` to actually emit `IRChainExit`

Only after Part A is fully green:

Current:
```go
func (e *emitter) emitChainableReturn(pc uint64) {
    e.emitReturn(pc, jitOK)
}
```

Target:
```go
func (e *emitter) emitChainableReturn(pc uint64) {
    e.emitWriteBackAll()
    e.irEm.ChainExit(pc, e.exitIdx)
    e.exitIdx++
    e.numInsns-- // exit is not a retired insn; or confirm existing accounting
}
```

Caveats to check while writing this:
- Does `IRChainExit` implicitly do WriteBackAll, or must the caller? Look at `ir/ir.go:192`: `IRChainExit // chain exit: {targetPC=Imm, exitIdx=Imm2}. WriteBackAll must precede.` — caller must emit WriteBackAll first. Do so.
- `e.exitIdx` already exists (`jit_emit_ir.go:486` uses it as `numChainExits`). Confirm its lifecycle: incremented once per chain exit.
- Does IC accounting need a fix? The IC is accumulated in RBP throughout the block. For chained exits, RBP survives into the next block (bypassing XORQ). For non-chain exits, the epilogue writes RBP → `sret.IC`. The slow-exit stub emitted by `emitSlowExitStub` (`ir/lower_amd64.go:397–416`) **already writes `RBP → sret.IC`**. Good — unpatched chain exits still deliver the correct IC.

## Part D — Observability tests (chaining actually fires)

With Part C in place, add these tests to `jit_chaining_test.go` (package `riscv`):

### D1 — `TestChaining_HotLoop_DispatchOKBecomesSubLinear`

Assemble a RISC-V loop that runs N iterations (e.g., N=200000), using Fixed Static Mapping (`jit.SetAllocStrategy("fixed")`). After run:
- `j.DispatchOK < N/50` (fully chained → O(1) in steady state, allow generous warm-up).
- `j.ChainPatched >= 1`.
- `cpu.cycle == expected_retired_insns` (exact IC invariant).

### D2 — `TestChaining_ChainExitsPopulated_OnCompiledBlock`

Compile any block whose end falls through or branches to a static PC. Assert `len(blk.chainExits) > 0`.

### D3 — `TestChaining_PatchPointsAtImm64_OfMovABS`

Compile a block with a chain exit. Read the 2 bytes at `blk.fn + blk.chainExits[0].patchOffset - 2` — must be `0x49 0xBA`.

### D4 — `TestChaining_PatchedJumpReachesChainEntry`

Compile blocks A (ends with chain exit to pc_B) and B. Run once so A executes and returns, triggering `tryPatchChain(A, pc_B)`. Read the 8 bytes at `A.fn + A.chainExits[0].patchOffset` and assert equal to `B.chainEntry` (as uintptr).

### D5 — `TestChaining_ICAccumulatesAcrossChainedExits`

Hot loop, N iterations. Assert `cpu.cycle == N * insns_per_iter` exactly. This protects against IC being reset by an accidental `XORQ RBP, RBP` on chain entry, or IC not being written back on the final non-chain exit.

### D6 — `TestChaining_FaultExitsWritebackIC`

Block that ends in a load fault. Run under chaining. Assert `cpu.cycle` equals the insns retired before the fault. Non-chain exits must write RBP → `sret.IC`.

### D7 — `TestChaining_IndirectBranch_NoChain` (regression guard)

JALR block (dynamic target). Assert no chain exit recorded (`len(blk.chainExits) == 0` for that block's dynamic-exit end). JALR goes through IRRetDyn, not IRChainExit. This is a safety check that we didn't accidentally broaden the scope of chaining.

## Part E — Reference harness (non-asserting)

`bench/jit_chain_reference_test.go` loads `bench/libriscv_guest/bench_guest.elf`, runs under Fixed Static Mapping for a fixed cycle budget, and prints a table:

```
--- Chain reference (Fixed Static Mapping) ---
  retired insns    : <N>
  DispatchOK       : <N>
  DispatchCompile  : <N>
  DispatchInterp   : <N>
  ChainPatched     : <N>
  insns/DispatchOK : <ratio>
  MIPS             : <value>
```

Capture numbers **before Part C** (baseline) and **after Part C** (proof). Expected shape: `DispatchOK` drops by ≥ 100×, `insns/DispatchOK` rises by ≥ 100×, MIPS rises meaningfully.

## Part F — Broad regression sweep

Before declaring done:
- `go test ./...` green.
- `make fuzz-oracle`, `make fuzz-fd`, `make fuzz-rvc`, `make fuzz-amo`, `make fuzz-bitmanip` green.
- `go test -run=. ./riscv-elf-tests/...` green.
- `make bench-ours` Fixed Static Mapping: expect +50% minimum.

## Files we expect to touch

- **Tests (new):**
  - `ir/lower_amd64_chain_test.go` — Part A1, A4, A5.
  - `jit_chain_foundation_test.go` (pkg `riscv`) — A2, A3.
  - `jit_chaining_test.go` (pkg `riscv`) — D1–D7.
  - `bench/jit_chain_reference_test.go` — Part E.
- **Implementation (likely just one line, plus any Part B fixes):**
  - `jit_emit_ir.go:207` — `emitChainableReturn` body.
  - Possibly `ir/lower_amd64.go` `emitPrologue` for chainEntryProg placement (if A5 is red).
- **Unchanged:** `jit.go`, `jit_native.go`, `internal/jitcall/`.

## Recommended execution order

1. Write Part A tests (A1–A5) — they're small and self-contained. No impl changes yet.
2. Run Part A. Capture red/green status per test.
3. **If all green**: Part A proves the foundation. Proceed to Part C.
4. **If any red**: Part B — fix the narrowest thing to turn that test green; re-run Part A.
5. Capture baseline from Part E (reference harness) before Part C.
6. Do Part C (one-line change in `emitChainableReturn`).
7. Run Part D. Iterate on Part C if needed.
8. Re-run Part E. Compare to baseline.
9. Part F regression sweep.

## Non-goals for T1.1

- No changes to IC-per-insn emission (that's T1.2).
- No regalloc pool changes (T2.4).
- No new IR optimization (T2.1/T2.2).
- V2 lowering untouched unless Part B finds a bug that's shared.

## Risks and stop conditions

- **Cache eviction risk:** `blocks` is a direct-mapped cache. If A at slot S is ever overwritten by a different block, any chain jumps targeting A's chainEntry point into the new block's bytes — undefined behavior. During Part A read-through, confirm whether eviction can happen. If yes, either invalidate inbound chains on eviction (track them per block) or assert no eviction. Do not ship chaining with eviction unsolved.
- **Concurrent write of patched bytes:** RunJIT is single-goroutine; `patchChainTarget` is `//go:nosplit`. Document the single-goroutine requirement.
- **MIPS gain < +20% after Part C:** stop and re-profile. Something else is now the bottleneck.

## Success criteria

1. All Part A tests green (foundation proven).
2. All Part D tests green (chaining actually fires).
3. Part E reference harness: `DispatchOK` drops ≥ 100×, MIPS +50% min.
4. Full regression sweep green.
