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
If monomorphic returns dominate (they do in structured code), a
one-entry-per-site IC converts ~95% of JALRs from Go round-trips
into direct jumps, collapsing the per-return cost from ~100 ns to
~5 ns. Rough projection: Dhrystone 527 → 2000+ MIPS, CoreMark 750
→ 2500+ MIPS.

**Design.** Symmetric to our existing chain-exit machinery:

At each JALR site, emit a fixed 3-instruction check sequence inline,
followed by two 8-byte patch slots embedded in code (reached via
RIP-relative addressing):

```asm
; Input: r_tgt holds the computed (rs1 + imm) & ~1 target.
writeback_all                        ; IC to RBP, dirty regs to sret
cmpq r_tgt, QWORD PTR [rip + .cache_pc]   ; 7B: 48 3B 0D <disp32>
jne  .miss                                ; 2B: short; 6B if far
jmp  QWORD PTR [rip + .cache_fn]          ; 6B: FF 25 <disp32>

.cache_pc: .quad 0x7BADC0DE7BADC0DE    ; 8B sentinel
.cache_fn: .quad 0x7BADC0DE7BADC0DE    ; 8B sentinel (→ slow-exit stub)

.miss:
    ; fall through to current IRRetDyn lowering (writeback pc, IC,
    ; status, return to Go). Dispatcher in Go updates both slots.
```

Total size per JALR site: ~30 bytes (vs. current ~20 for IRRetDyn) —
acceptable.

**Patching flow** (mirrors block chaining at `jit.go:422-425`):

- First entry to site: `cache_fn` points to a slow-exit stub; `cache_pc`
  is a sentinel that never matches any real PC. `cmp` fails →
  fallthrough → writeback → return to Go.
- Go dispatcher sees the slow-exit return, resolves the block for
  target PC, then calls `patchJalrIC(site, targetPC, blockFn)` which
  atomically writes `cache_pc = targetPC; cache_fn = blockFn`.
- Next JALR to the same target: `cmp` matches → `jmp [cache_fn]` →
  one machine instruction dispatch, no Go round-trip.
- JALR to different target: `cmp` misses → slow path → re-patch
  (simple monomorphic IC; polymorphic callers will thrash but still
  correct).

**Mechanics details:**

- Patch slots live in the same `execMem` region as the block code,
  RW mapping during patch, execute-only during run (or W^X via
  `mprotect` — we already handle this for chain patches).
- Record per-site metadata in `block.jalrICs []jalrICInfo{patchPC,
  patchFn, sitePC}`.
- The dispatcher distinguishes IC miss from regular IRRetDyn via a
  new `jitOK_jalr_miss` status code (or reuse `jitOK` plus an
  out-of-band flag in sret). Simplest: extend sret with a
  `jalrSiteIdx` field populated by the `.miss` path; dispatcher
  reads it and patches.

**Variant — is RAS worth it instead/also?**
A shadow Return Address Stack would predict the ~90% of JALRs that
are `ret` (rs1=x1, rd=x0) even more cheaply (no compare, just a pop
+ direct jump). But it's an extra data structure with its own
correctness hazards (tail calls, setjmp/longjmp, exceptions,
signal-like resumption). **Defer.** Site IC is strictly simpler,
handles all JALR flavors uniformly, and monomorphic hit rate is
high enough in real code that RAS is a minor additional win.

**Cost of a wrong prediction.** One extra cmp+branch before the slow
path. ~2 ns. Safe.

**IR/lowering changes:**

- `ir/ir.go`: add `IRJalrIC` op with fields `{targetVReg=A,
  siteIdx=Imm, status=Imm2}`.
- `ir/emit.go`: add `(*Emitter).JalrIC(target VReg, siteIdx int)`.
- `ir/lower_amd64.go`: add `lowerJalrIC` that emits the
  cmp/jne/jmp-m sequence plus the two 8-byte embedded slots. Record
  patch offsets on `lowerResult`.
- `jit.go`: add `ChainPatchedJalr`, `JalrICHits`, `JalrICMisses`
  counters. Add `patchJalrIC(blk, siteIdx, tgtPC, tgtFn)`.
- `jit_native.go`: after assembly, initialize `cache_fn` to the
  slow-exit stub and `cache_pc` to a sentinel that can't match any
  real PC (e.g., `0xFFFFFFFFFFFFFFFF`).
- `jit_emit_ir.go:2117`: `emitJALR` changes from `RetDyn(tgt, …)` to
  `JalrIC(tgt, siteIdx)` + increment `e.jalrSiteIdx`. The `.miss`
  path internally still spills state via the existing RetDyn
  lowering — one implementation.

**Tests:**

- `ir/lower_amd64_jalric_test.go` — encoding (cmp/jne/jmp bytes at
  expected offsets), sentinel placement.
- `jit_jalr_ic_test.go` — drive a block through JALR to a known
  target, verify first call misses (slow path, Go round-trip),
  verify second call hits (no Go round-trip), verify
  `JalrICHits/Misses` counters.
- Extend `TestJIT_ChainReference` output to include `JalrICHits`,
  `JalrICMisses`, `insns/JalrMiss`.

**Fuzz/regression:**

- `make fuzz-oracle` / `-fd` / `-rvc` / `-amo` / `-bitmanip` unchanged
  (shouldn't touch JALR semantics).
- Re-run `make bench-chain-ref`. Expected: CoreMark/Dhrystone MIPS
  rise ≥3×; bench_guest unchanged (it has no JALR-heavy loop).

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

## Files (Phase 1 in detail)

- **Modify:**
  - `ir/ir.go` — new `IRJalrIC` op.
  - `ir/emit.go` — `JalrIC(target, siteIdx)` entry.
  - `ir/lower_amd64.go` — `lowerJalrIC`; register `jalrICs` on
    `lowerResult` (parallel to `chainExits`).
  - `ir/lower_amd64_v2.go` — same handler (V2 parity; V2 is not
    production, but keep tests green).
  - `jit_emit_ir.go:2117-2128` — `emitJALR` emits `JalrIC` not
    `RetDyn`. Add `e.jalrSiteIdx` counter.
  - `jit.go` — new counters; `patchJalrIC`; extend `block` struct
    with `jalrICs []jalrICInfo`.
  - `jit_native.go` — initialize IC slots to sentinel + slow-exit
    stub after assembly (mirror chain-exit backpatch).
  - `bench/jit_chain_reference_test.go` — report new counters.
- **New:**
  - `ir/lower_amd64_jalric_test.go` — encoding/byte-level tests
    (mirrors `lower_amd64_chain_test.go`).
  - `jit_jalr_ic_test.go` — runtime-behavior tests (mirrors
    `jit_chaining_test.go`).

## Verification

1. Byte-level: IC slot bytes at known offsets after assembly; sentinel
   values in place before backpatch.
2. Runtime: drive `emitJALR` through a test CPU. First JALR
   `JalrICMisses++`; second JALR to same target `JalrICHits++` and
   produces a single-JMP dispatch with no `runJIT` re-entry.
3. Correctness: `make fuzz-oracle`, `-fd`, `-rvc`, `-amo`,
   `-bitmanip` green.
4. ELF tests: `go test ./riscv-elf-tests/...` green.
5. Regression: `go test ./...` green.
6. Perf: `make bench-chain-ref`. Expected numbers (rough):
   - bench_guest unchanged (no JALR pressure).
   - CoreMark: `JalrICHits / (JalrICHits + JalrICMisses) > 0.9`;
     MIPS rises from 750 toward 2000+.
   - Dhrystone: same IC hit rate; MIPS rises from 527 toward 1500+.

## Non-goals / risks

- Not implementing bytecode preprocessing (we're JIT-only).
- Not porting libtcc/TinyCC integration (we have it as a separate
  backend; this work targets the Go JIT).
- Not implementing RAS in Phase 1 — IC subsumes it.
- **Cache-eviction risk:** `blocks` is direct-mapped; if block B is
  evicted while another block's JALR IC still points to B, the
  direct jump lands in stale or freed memory. Mitigation: invalidate
  inbound JALR ICs on block eviction, same as inbound chain patches.
  Plan this into `jit.go`'s eviction path explicitly (track inbound
  refs per block — same infra chain patching already needs; reuse).
- **Self-modifying guest code:** breaks the IC cache. Out of scope
  (libriscv uses segment hash for this; Phase 6).
- **Polymorphic dispatch (virtual calls, function pointers stored in
  tables):** thrashes the 1-entry IC. If real-world code shows
  polymorphic hot paths, extend to 2-entry IC in a follow-up — same
  mechanism, double the slots.

## Recommended execution order

1. Part 1 — Add `IRJalrIC` to `ir/ir.go` and `ir/emit.go`.
2. Part 2 — Implement `lowerJalrIC` with byte-level tests.
3. Part 3 — Extend `block` with `jalrICs`; backpatch in `jit_native.go`.
4. Part 4 — Implement `patchJalrIC`; dispatcher wiring.
5. Part 5 — Flip `emitJALR` to use `JalrIC`.
6. Part 6 — Add runtime tests; run `make bench-chain-ref`.
7. Part 7 — Regression sweep.
