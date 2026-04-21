# libriscv-inspired optimizations for Go JIT (Fixed Static Mapping)

## Context

Measured baseline (post-chaining, Fixed Static Mapping, macOS):

| workload   | insns  | insns/DispatchOK | ChainPatched | MIPS |
|------------|--------|------------------|--------------|------|
| bench_guest| 2.52 B | **4100**         | 2,071        | 3343 |
| coremark   | 456 M  | **50.8**         | 1,665,643    | 750  |
| dhrystone  | 282 M  | **26.9**         | 8            | 527  |

Native ceiling on this host: ~18,000 MIPS. We have 5× headroom on
bench_guest, 24× on CoreMark, 34× on Dhrystone.

- bench_guest hits `MaxIC=4096` every block (BudgetCheck-bound).
- CoreMark: heavy dispatch (9 M returns to Go) — JALR is the exit.
- Dhrystone: 10.5 M returns, only 8 chained — pure JALR workload.

So on standard benchmarks, **JALR (function returns and indirect
calls) is what's keeping us slow**. Our current JIT emits every JALR
as an `IRRetDyn` → round-trip to Go → dispatcher lookup → trampoline
→ target block (`jit_emit_ir.go:2117`, `ir/lower_amd64.go:1549`).

Study of libriscv (`xendor/libriscv/lib/libriscv/`) identified a set
of optimizations worth importing, ranked here by measured relevance
to **our** workloads, not libriscv's author's priorities.

## What libriscv already has that we don't

From the survey (Explore agent):

1. **Return-location tracking** — `tr_translate.cpp:412-424, 600-605`
   scans JAL instructions and records call/return pairs. libriscv
   collects this data but **never fully exploits it**. This is our
   opening: use it to build a JALR inline cache.
2. **FAST_JAL / FAST_CALL bytecodes** — `threaded_rewriter.cpp:243-276`
   rewrites JAL-within-segment into direct-dispatch bytecodes. Our
   block chaining already does the equivalent for static JAL targets
   (via `emitChainableReturn` at `jit_emit_ir.go:2113`), so this box
   is already checked.
3. **Bounds-check elision via arena-direct access** —
   `tr_emit.cpp:327-365` skips re-checks on recently-checked base
   registers within a block. We don't do this today.
4. **GP constant propagation** — `tr_translate.cpp:451-484` detects
   `auipc gp, X; addi gp, gp, Y` at block entry and inlines the
   constant. We don't do this.
5. **Block-scope register caching** — `tr_emit.cpp:93-180` keeps the
   first 18 GPRs as C locals for the whole block. Our Fixed Static
   Mapping already does this for the whole *function* (pinned
   regs), so we're actually stronger here than libriscv.
6. **Forward-branch skip-IC** — `threaded_rewriter.cpp:140-142` skips
   counter checks on forward branches. We already only emit
   `BudgetCheck` on backward branches (`jit_emit_ir.go:2152-2162`),
   so box checked.
7. **Syscall-clobber minimization** — `tr_emit.cpp:725-798` saves
   only syscall-clobbered registers. We save all live caller-saved.
8. **Segment translation cache (CRC32-C)** — `tr_translate.cpp:95-136`
   avoids re-translating unchanged code across runs.

## What we should do (ranked by measured ROI on our workloads)

### Priority 1 — JALR site inline cache (Phase 1)

**Why first.** Directly addresses Dhrystone (R=27) and CoreMark (R=51).
A one-entry-per-site IC converts monomorphic returns from Go
round-trips into direct block-to-block jumps. Conservative projection
(see "Expected gains" below): Dhrystone 527 → 1300–1600 MIPS,
CoreMark 750 → 1800–2200 MIPS.

#### Validated design

Three investigations nailed down the shape (details in "Validated
assumptions" below):

- Patch slots will be the `imm64` fields of two **`MOVABS R10, <imm>`**
  instructions, not RIP-relative data. This reuses the exact backpatch
  machinery already proven in chain exits (`ir/lower_amd64.go:388-410`,
  `jit_native.go:89-106`). No new data-section scaffolding.
- **WriteBackAll before the site is mandatory** — block-to-block
  register passing is flush-and-reload through `x[]`/`f[]` arrays
  (`ir/highlevel.go:64-80`, `ir/lower_amd64.go:330-358`), not through
  pinned host regs. Target block's register allocator loads its
  operands on demand from `x[]`/`f[]` via R12/R13. Skipping writeback
  on hit would read stale data.
- Chain JMPs land at `chainEntryProg` (a NOP after arg-moves, after
  `XORQ RBP`, before `SUBQ RSP, frameSize`; `ir/lower_amd64.go:351`).
  Our chain exits deallocate the current frame with `ADDQ RSP,
  frameSize` before jumping. JALR IC must match this: deallocate our
  frame, then JMP. Target's prologue re-allocates its own frame.

#### Emitted sequence (per JALR site, ~43 bytes hit-path inline)

```asm
; State on entry: r_tgt in an allocatable host reg, IC in RBP,
;                 all dirty pinned x/f regs already WriteBackAll'd
MOVQ   r_tgt, 0(RBX)             ; 4B  — write sret.PC unconditionally
                                 ;       (harmless on hit, required on miss)
MOVABS R10, 0x7BADC0DE7BADC0DE   ; 10B — imm64 = cache_pc patch slot 1
CMPQ   r_tgt, R10                ; 3B
JNE    .miss                     ; 2B short / 6B near (assembler picks)
ADDQ   $frameSize, RSP           ; 4B  (omitted if frameSize == 0)
MOVABS R10, 0x7BADC0DE7BADC0DE   ; 10B — imm64 = cache_fn patch slot 2
JMP    R10                       ; 3B  → target block's chainEntry

.miss:                           ; appended after main code, one per site
ADDQ   $frameSize, RSP           ; 4B
MOVQ   RBP, 8(RBX)               ; 4B  — sret.IC
MOVQ   $jitOKJalrMiss, 16(RBX)   ; 7B  — sret.Status (new code)
MOVQ   $siteIdx, 24(RBX)         ; 7B  — sret.FaultAddr repurposed
                                 ;       to carry the site index
RET                              ; 1B
```

Byte counts are x86-64 machine-code sizes; the assembler may
short-form JNE if `.miss` is within 127 bytes (usually yes for small
blocks, no for large). Either way the patch-slot offsets are correct
because we read them post-assembly from `MovProg.Pc + 2`, same as
chain exits (`jit_native.go:94-95`).

#### Patching flow (mirrors `jit.go:422-425`)

- **Initial state** (set by `jit_native.go` backpatch loop, symmetric
  to the existing chain-exit one):
    - `cache_pc` = `0xFFFFFFFFFFFFFFFF` — guaranteed miss (unaligned,
      can't be a real PC).
    - `cache_fn` = address of the site's `.miss` stub. First
      execution falls through CMPQ → JNE taken → miss stub → RET.
- **First execution**: `CMPQ` fails, jumps to `.miss`, writes target
  PC + IC + miss status + siteIdx to sret, returns to Go.
- **Go dispatcher** (new branch in `jit.go` dispatch loop): on
  `jitOKJalrMiss`, read `siteIdx` from `sret.FaultAddr`, resolve the
  block for `sret.PC` (same path as `tryPatchChain`), if resolved and
  `chainEntry != 0` then call `patchJalrIC(blk, siteIdx, targetPC,
  chainEntry)` — atomically writes both `cache_pc` and `cache_fn`.
- **Subsequent executions** with the same target: `CMPQ` matches,
  `ADDQ + JMP` jumps to target's `chainEntry`. Zero Go round-trips.
- **Subsequent executions** with a different target: `CMPQ` fails,
  miss path runs, dispatcher re-patches. Monomorphic after that.
  Polymorphic callers thrash (same IC gets repeatedly re-patched);
  still correct, just not sped up.

#### Cost model

| path | instructions | rough cycles |
|------|--------------|--------------|
| hit  | MOVQ mem + MOVABS + CMPQ + JNE(nt) + ADDQ + MOVABS + JMP reg | ~7 |
| miss | hit-path through JNE, then miss stub + Go round-trip + next block entry | ~150+ |
| current (no IC) | every JALR: equivalent of a miss | ~150+ |

Dhrystone: if 99% of its 10.5 M JALRs hit after warm-up, and each
hit saves ~143 cycles vs. today, that's 10.5e6 × 0.99 × 143 ≈ 1.5
billion cycles saved on a 2.3 GHz host = ~650 ms. Current dhrystone
run is 535 ms total, so we're compressing the JALR-return cost from
the dominant bill to near-zero. New MIPS floor: 282e6 / (0.535 −
0.5×0.65) = 282e6 / 0.21 ≈ 1340 MIPS. Upper bound (polymorphic fully
mitigated): 2×.

CoreMark: 9 M JALRs, hit rate likely lower (state-machine dispatch
is more polymorphic). If 90% hit, new MIPS floor around 1800.

#### Eviction and inbound-patch correctness

**Finding** (from chain-infra investigation): the `blocks` cache
(`jit.go:107-124`) is a 4096-entry direct map indexed by `(pc >> 1)
& 0xFFF`, no reverse tracking of inbound chain patches, no
invalidation on slot overwrite. Correctness today relies on eviction
being empirically rare (workloads with <4096 unique block PCs never
collide).

JALR IC inherits this exact invariant — it does **not** make the
problem qualitatively worse:

- Chain patches today are one per static-successor block exit. A
  successful patch leaves a pointer into the target block's
  `chainEntry` that becomes stale if the target is ever evicted.
- JALR IC patches are one per site per observed target. Higher
  patch rate on polymorphic sites, but each individual patch points
  at a `chainEntry` the same way — same class of stale-pointer risk.
- bench_guest/coremark/dhrystone all have well under 4096 unique
  hot block PCs, so neither chain patches nor JALR IC patches hit
  the eviction path in practice.

**Mitigation plan if this ever bites**: add `inboundPatches
[]patchRef` to `compiledBlock`, appended on every
`patchChainTarget` / `patchJalrIC`. On eviction (the `j.blocks[idx]
= new` line in `insertBlock`), walk the old block's
`inboundPatches` and reset each back to its initial sentinel +
slow-stub address. Deferred — the current zero-invalidation story is
already load-bearing and works; we track this as a known limitation
both before and after Phase 1.

#### RAS variant: deferred

A shadow Return Address Stack would predict rs1=x1, rd=x0 JALRs
(pure returns) even more cheaply (no compare — just pop + direct
jump). But: separate data structure, correctness hazards around tail
calls / setjmp / exceptions / signal-resumption, interacts with
call-not-return JALRs badly. Site IC handles all JALR flavors
uniformly and is strictly simpler. Revisit RAS only if measured
hit rate on the site IC is below 90%.

#### IR / lowering changes

- `ir/ir.go` — new op `IRJalrIC` with `{A=targetVReg,
  Imm=siteIdx}`. Treated as a block terminator (same categorization
  as `IRRetDyn` / `IRChainExit` in `ir/lower_amd64.go:445`).
- `ir/emit.go` — new `(*Emitter).JalrIC(target VReg, siteIdx int)`
  emitter (no implicit WriteBackAll — caller does it, parallel to
  how `ChainExit` works in `ir/highlevel.go:105-107`).
- `ir/highlevel.go` — new `(*Emitter).DynChainableRet(target VReg,
  siteIdx int)` convenience that does
  `WriteBackAll(); JalrIC(target, siteIdx)`, symmetric with
  `ChainableRet`.
- `ir/lower_amd64.go`:
    - New `lowerJalrIC(ins *IRInstr)` emitting the 7-instruction hit
      sequence plus recording a new `jalrICInfo{sitePC: pc, siteIdx,
      movPcProg, movFnProg, missStubProg}`.
    - New `emitJalrMissStub(siteIdx int, frameSize int64)` appended
      after the main block code (parallel to `emitSlowExitStub`).
    - `lc.jalrICs []jalrICInfo` collected on `lowerCtx`, flushed to
      `lowerResult.JalrICs` at `finalize()` time (parallel to
      `chainExits` path).
- `ir/lower_amd64_v2.go` — add the same `lowerJalrIC` for parity
  (even though V2 isn't production, keep it buildable and tested).
- `jit_emit_ir.go:2117-2128` — `emitJALR`: replace the
  `WriteBackAll; RetDyn` final sequence with
  `WriteBackAll; JalrIC(tgt, e.jalrSiteIdx); e.jalrSiteIdx++`.
  Add `jalrSiteIdx int` field on `emitter`.
- `jit.go`:
    - New `jalrICPatchInfo{sitePC, movPcPatchOff, movFnPatchOff,
      missStubOff}` on the `compiledBlock` struct (parallel to
      `chainExits []chainPatchInfo`).
    - New counters: `ChainPatchedJalr`, `JalrICHits`,
      `JalrICMisses` (all `uint64`).
    - New `patchJalrIC(blk, siteIdx, targetPC, chainEntry)`
      `//go:nosplit`, writes both 8-byte slots. Increments
      `ChainPatchedJalr`.
    - Dispatch-loop branch: when `sret.Status == jitOKJalrMiss`,
      read `siteIdx` from `sret.FaultAddr`, resolve block, patch,
      increment `JalrICMisses`. Normal chain-exit flow for next
      block continues from there.
    - New status constant `jitOKJalrMiss`. Hit path never surfaces
      to Go — increment `JalrICHits` via a separate mechanism
      (e.g., the missing counter could just be `total_jalrs -
      misses`, or sample-based; not critical). Simplest: don't
      count hits at all, rely on `insns/DispatchOK` ratio in the
      chain reference harness to measure effect.
- `jit_native.go` — extend the post-assembly backpatch loop: for
  each `jalrIC`, write `cache_pc = 0xFFFFFFFFFFFFFFFF` and `cache_fn
  = missStubAddr` into the assembled `execMem`. Record `patchOffs`
  as `MovProg.Pc + 2` for each of the two MOVABS (same math as
  chain exits).
- `internal/jitcall/call_amd64.s` — untouched; status codes are
  values, not ABI.

#### Tests

- `ir/lower_amd64_jalric_test.go` — byte-level encoding
  (MOVABS+CMP+JNE+MOVABS+JMP pattern, sentinel placement, patch-offset
  math). Mirror `ir/lower_amd64_chain_test.go`.
- `jit_jalr_ic_foundation_test.go` — drive a minimal block through
  `lowerJalrIC` via direct IR emission (no `emitJALR`), assert:
    - After compilation: `cache_pc == 0xFFFF...`, `cache_fn ==
      missStub`.
    - Running once: returns with `Status == jitOKJalrMiss`,
      `FaultAddr == siteIdx`.
    - After `patchJalrIC(blk, 0, 0xBEEF, 0xDEAD)`: both slots
      updated; next run `CMPQ` matches; the test verifies the bytes,
      not an actual jump (no destination block).
- `jit_jalr_ic_test.go` — two-block integration: A ends in a JALR to
  B's entry. Verify first pass: miss, Go dispatch, patch. Verify
  second pass: hit, no Go round-trip (check dispatcher counters).
  Verify polymorphic case: A's JALR visits two different targets in
  sequence; second visit misses+repatches.
- Extend `bench/jit_chain_reference_test.go::runChainReference` to
  print `ChainPatchedJalr`, `JalrICMisses`, `insns/JalrICMiss`.

#### Fuzz / regression gates (Phase 1)

- `make fuzz-oracle` / `-fd` / `-rvc` / `-amo` / `-bitmanip` green —
  JALR semantics unchanged, only the emit sequence differs.
- `go test ./...` green.
- `go test ./riscv-elf-tests/...` green.
- `make bench-chain-ref`: dhrystone MIPS ≥ 2×, coremark MIPS ≥ 2×,
  bench_guest MIPS within ±5% of baseline.

#### Validated assumptions (from pre-implementation investigation)

1. **goasm can emit every instruction needed.** `MOVABS R10, imm64`
   is the same form chain exits already use
   (`ir/lower_amd64.go:396-402`). `CMPQ r64, r64` uses existing
   `emitRR(x86.ACMPQ, …)`. `JNE label` uses the branch lowerer.
   `JMP R10` uses existing `emitJmpReg`
   (`ir/lower_amd64.go:378-384`). `ADDQ imm32, RSP` is existing
   `emitRI`. `MOVQ reg, memOff(RBX)` / `MOVQ imm, memOff(RBX)` are
   the existing `emitMR` / `emitMI` helpers. **No new op spellings,
   no new addressing modes, no data sections required.**
2. **Patch-slot offset math is exactly what chain exits use.**
   `MovProg.Pc + 2` points at the imm64 of MOVABS R10
   (`jit_native.go:94-95`). Two MOVABS per site → two patch
   offsets recorded; both resolved identically.
3. **WriteBackAll cannot be skipped on hit.** Fixed Static Mapping
   passes state block-to-block through `x[]` / `f[]` arrays
   (`ir/highlevel.go:64-80`), not through pinned host regs. Target
   block's register allocator will load its operands on demand.
   Skipping writeback = stale reads. The hit-path cost model above
   already counts WriteBackAll as "already paid" because today's
   `emitJALR` already issues it before `RetDyn`.
4. **`chainEntry` jump target is after arg-moves, after `XORQ RBP`,
   before `SUBQ RSP, frameSize`.** Exit sequence deallocates its own
   frame first, then JMP. Match chain-exit pattern at
   `ir/lower_amd64.go:388-410`.
5. **Eviction invalidation is absent and inherited, not newly
   introduced.** Plan respects the existing load-bearing assumption
   that 4096-slot direct-mapped cache is large enough. Mitigation
   (reverse-patch tracking) scoped as a Phase-independent followup.

---

### Priority 2 — Bounds-check hoisting within a block (Phase 2)

**Why second.** CoreMark runs list-walk inner loops; Dhrystone does
repeated struct field stores. Each load/store in our lowerer emits a
bounds check against the guest memory mask (`guestmem.go`). If a
base register was just checked, a follow-up access with a nearby
offset should reuse the check.

**Design.**

- In the IR block, after a load/store with base `rs1 + imm` passes
  bounds check, mark `rs1` as "verified for window
  [imm-overhang, imm+overhang]" for the remainder of the block.
- Follow-up accesses on the same `rs1` with offsets within the
  window skip the check.
- Invalidate the window on any write to `rs1`, any `jit.Call`, any
  syscall boundary.
- `overhang` = 4 KB (one page) is safe because we mmap a
  power-of-two guard region (see `guestmem.go`).

**Where.** Add a pass in `jit_emit_ir.go` that scans the raw block
before lowering, tracks verified-bases per instruction. Emit a
"skip-bounds" flag on subsequent load/store IR ops. Lowerer honors
the flag.

**Effort.** Medium. The analysis is local and linear; the IR already
tracks base/offset clearly.

**Expected gain.** ~5–15% on memory-heavy workloads. Smaller than
Phase 1, but compounds.

---

### Priority 3 — GP (and TP) constant propagation (Phase 3)

**Why third.** PIC code initializes `x3 (gp)` via
`auipc gp, X; addi gp, gp, Y` at `_start`. After that, every
gp-relative load becomes `lw a0, offset(gp)` — a base register
whose value is a compile-time constant. If we hoist gp to an
immediate, bounds checks also simplify (known-safe offsets can be
fully elided in Phase 2).

**Design.**

- At ELF load or first block scan, walk the block(s) reachable from
  entry. Detect `auipc gp, X; addi gp, gp, Y` → `gp = pc_of_auipc +
  (X<<12) + Y`. Cache `gpConst` on the `JIT`.
- During emission, when `rs1 == 3` (gp), use `gpConst + imm` as the
  address directly; skip the register load.
- Same pattern for `x4 (tp)` in thread-local code (rarer).

**Effort.** Small. Matches `tr_translate.cpp:451-484` mechanics.

**Expected gain.** Small on our benchmarks (none init gp that way in
a way that shows up in hot paths), but cheap and composes with
Phase 2.

---

### Priority 4 — Raise MaxIC on long hot loops (Phase 4, optional)

**Why.** bench_guest's 4100 insns/dispatch = `MaxIC`. Raising to 16384
halves the BudgetCheck rate; remaining Go round-trip cost is ~50%
smaller. Rough projection: bench_guest 3343 → ~4200 MIPS.

**Why deferred.** Bigger budget means longer uninterrupted JIT
execution, which means worse GC worst-case latency. Needs a
principled justification (e.g., couple it with a "yield every
N µs" timer rather than an insn-count budget).

**Design.** Single constant change + benchmark sweep. Accept it only
if P99 GC-pause stays under 10 ms on the benchmark harness.

---

### Priority 5 — Syscall-clobber optimization (Phase 5, deferred)

Modest on our benchmarks. Dhrystone is the most syscall-heavy and
it's already below the JALR bottleneck. Revisit after Phase 1.

---

### Priority 6 — Translation cache by segment hash (Phase 6, deferred)

Helps only cold start. Our blocks are tiny (max 16 KB scan region,
~2048 PCs) and compile fast. Low priority until Phases 1–2 close the
MIPS gap.

---

## Phase boundaries and gates

```
Phase 1 → gate on: CoreMark MIPS ≥ 2×; Dhrystone MIPS ≥ 2×;
          bench_guest MIPS within ±5% of baseline; all fuzz green.
Phase 2 → gate on: memstress-style workload MIPS ≥ +10%.
Phase 3 → gate on: at least one benchmark shows measurable gain
          (may be rolled in with Phase 2 since the analyses share
          substrate).
Phase 4 → separate GC-latency study; skip if controversial.
```

## Files (Phase 1 — summary)

See the "IR / lowering changes" and "Tests" subsections under
Priority 1 for complete per-file detail. Summary of touched files:

| file | change |
|------|--------|
| `ir/ir.go` | add `IRJalrIC` op |
| `ir/emit.go` | add `JalrIC(target, siteIdx)` |
| `ir/highlevel.go` | add `DynChainableRet(target, siteIdx)` |
| `ir/lower_amd64.go` | add `lowerJalrIC`, `emitJalrMissStub`; `jalrICs` on `lowerCtx` and `lowerResult` |
| `ir/lower_amd64_v2.go` | parity handler |
| `jit_emit_ir.go:2117-2128` | `emitJALR` → `DynChainableRet`; add `jalrSiteIdx` |
| `jit.go` | `jalrICPatchInfo` on block; counters `ChainPatchedJalr`, `JalrICMisses`; `patchJalrIC`; dispatch branch for `jitOKJalrMiss` |
| `jit_native.go` | backpatch sentinel + miss-stub pointer for each JALR IC |
| `bench/jit_chain_reference_test.go` | report new counters |
| `ir/lower_amd64_jalric_test.go` (new) | byte-level tests |
| `jit_jalr_ic_foundation_test.go` (new) | lowering-level integration |
| `jit_jalr_ic_test.go` (new) | two-block runtime integration |

## Non-goals (Phase 1)

- Not implementing bytecode preprocessing (we're JIT-only).
- Not porting libtcc integration — this work targets the Go JIT.
- Not implementing RAS — site IC subsumes it.
- Not implementing reverse-patch tracking for eviction invalidation
  (see risk note above — inherited, scoped separately).
- Not implementing 2-entry polymorphic IC — followup if measurement
  shows thrashing sites on real workloads.

## Risks (Phase 1)

- **Polymorphic thrashing:** CoreMark's state machine, C++ vtables
  (not present in these benchmarks but will appear in real
  workloads) can repeatedly miss the 1-entry IC. Still correct;
  gain degrades toward baseline. Measured via
  `JalrICMisses / total_jalrs` in the chain reference harness.
  Mitigate only if observed.
- **Eviction with inbound patches:** inherited, discussed above.
  Not made worse by this plan.
- **Self-modifying guest code:** not supported — the IC (like chain
  patches) assumes block code at a PC is stable. Out of scope.
- **Hit-rate surprise:** if observed hit rate on Dhrystone is
  <80%, the gain projection breaks. First thing to check: is the
  `cache_fn` slot being patched to real `chainEntry` values (grep
  `ChainPatchedJalr > 0` in the reference harness output)?

## Recommended execution order (Phase 1)

1. **Step 1.** Add `IRJalrIC` to `ir/ir.go` (op constant,
   `opNames`, category in `isTerminator`-equivalent at
   `ir/lower_amd64.go:445`). Add `JalrIC` emitter in `ir/emit.go`
   and `DynChainableRet` in `ir/highlevel.go`. No lowerer yet —
   just a compile-fail stub in `lower_amd64.go`.
2. **Step 2.** Implement `lowerJalrIC` and `emitJalrMissStub` in
   `ir/lower_amd64.go`. Write byte-level tests at this stage
   (`ir/lower_amd64_jalric_test.go`) — they should be green before
   touching any runtime code.
3. **Step 3.** Extend `compiledBlock` with `jalrICs`; update
   `jit_native.go` backpatch loop to initialize both slots per site
   (sentinel `cache_pc`, miss-stub pointer `cache_fn`). Add
   foundation test verifying the initial slot contents byte-by-byte.
4. **Step 4.** Implement `patchJalrIC` in `jit.go` and the
   dispatcher branch for `jitOKJalrMiss`. Add `ChainPatchedJalr`
   and `JalrICMisses` counters. Add V2 parity handler to
   `ir/lower_amd64_v2.go` (keeps tests green).
5. **Step 5.** Flip `emitJALR` in `jit_emit_ir.go:2117-2128` to
   emit `DynChainableRet` instead of `WriteBackAll + RetDyn`. This
   is the one-line production switch.
6. **Step 6.** Write the two-block integration test
   (`jit_jalr_ic_test.go`) verifying first-miss-then-hit and
   polymorphic-retry paths.
7. **Step 7.** Extend `bench/jit_chain_reference_test.go` to print
   the new counters.
8. **Step 8.** Run `make bench-chain-ref`; compare MIPS to
   pre-flip baseline. Run full regression: fuzz suite + ELF tests +
   `go test ./...`.
