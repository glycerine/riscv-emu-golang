# Phase 2b Follow-Up: Profile-Driven Pass to Close Coremark Regression

## Context

Phase 2b (multi-segment `DecodedExecuteSegment`s for LuaJIT-style
dynamic code) shipped. Benchstat vs Phase 2a (median of 10):

| workload    | Phase 2a | Phase 2b | Δ     |
|-------------|---------:|---------:|------:|
| bench_guest |     3275 |     3246 | -0.9% |
| coremark    |      959 |      895 | -6.7% |
| dhrystone   |      771 |      768 | -0.4% |

bench_guest and dhrystone are within noise. Coremark is outside
the ±2% Phase 2b target. Goal of this follow-up: restore coremark
to ≥ 940 MIPS (within 2% of Phase 2a) **without** reverting any
Phase 2b correctness behavior.

The user has also asked: *"Is the ExecRegion check per cache
miss inherent? Can it be finessed?"* — this plan addresses that
explicitly.

## Root-cause analysis (hot-path trace)

Per-block dispatch in `RunJIT` (jit.go:591–610). Every
Phase 2b addition visible in code:

```go
pc := cpu.pc
blk := j.lookupBlock(pc)                                   // ← findSegment inside
if blk != nil {
    var res jitcall.Result
    if len(j.aotSegments) > 0 {                            // NEW branch (jit.go:594)
        seg := blk.segment                                  // NEW load (jit.go:600)
        if seg == nil {                                     // NEW null-chain (601–606)
            seg = j.hotSegment
            if seg == nil { seg = j.aotSegments[0] }
        }
        res = jitcall.CallAOT(blk.fn, …,
            seg.decoderCacheBase, seg.decoderCacheMask,     // 2 reads *seg
            seg.vaddrBegin, seg.vaddrEnd-seg.vaddrBegin)    // 2 reads *seg + 1 sub
    } else {
        res = jitcall.Call(…)                               // legacy lazy-only path
    }
```

`lookupBlock` (jit.go:388–399):

```go
if s := j.findSegment(pc); s != nil {                       // NEW call (jit.go:389)
    if blk, ok := s.blocks[pc]; ok { return blk }
}
idx := cacheIdx(pc); if j.cache[idx].pc == pc { … }
```

`findSegment` (jit.go:356–367):

```go
if s := j.hotSegment; s != nil && pc >= s.vaddrBegin && pc < s.vaddrEnd {
    return s                                                // hot-cache hit: 2 reads + 2 cmp
}
for _, s := range j.aotSegments { … }                       // linear scan on miss
```

Cache-miss-only path (jit.go:703–709):

```go
if !j.InterpOnly && len(cpu.mem.execRegions) > 0 {          // only fires on lookupBlock nil
    if seg := j.nextExecuteSegment(&cpu.mem, pc); seg != nil {
        if _, ok := seg.blocks[pc]; ok { continue }
    }
}
```

**Where the ~64 MIPS went on coremark** (steady state, hot
inner loop):

1. `findSegment` hot-cache bounds check on *every* dispatch:
   1 pointer load + 2 compares + 1 branch. Phase 2a: 0.
2. `len(aotSegments) > 0` branch. Phase 2a: 0.
3. `blk.segment` load + nil-chain. Phase 2a: directly used
   `j.aotSegment.…`, no indirection, no null-check.

Coremark dispatches at ~895 MIPS = ~3.35 ns/block on 3 GHz.
Adding ~0.2–0.3 ns/block fits the observed 64 MIPS delta.

**The ExecRegion check is NOT on the steady-state hot path** —
it only fires when `lookupBlock` returns nil (cache miss). On a
coremark inner loop that path is cold. It *can* still be
finessed (see Optimization 2) but has ~0 coremark impact.

## Design

### Optimization 1 (primary) — `soleSegment` fast-path

Cache the single installed segment on the JIT struct so the
one-segment case (coremark, dhrystone, bench_guest, 99% of
real guests) skips findSegment *and* the blk.segment null-
chain.

**Invariant**: `j.soleSegment != nil` iff `len(aotSegments) == 1`.
Re-evaluated at every mutation site via one helper
`j.refreshSoleSegment()`.

Struct addition (`jit.go` around line 188, near hotSegment):

```go
// soleSegment is aotSegments[0] when exactly one segment is
// installed, else nil. Maintained as an invariant; enables the
// RunJIT fast path to skip findSegment and the blk.segment
// null-chain in the common single-segment case.
soleSegment *DecodedExecuteSegment
```

Helper (near findSegment, jit.go:353):

```go
func (j *JIT) refreshSoleSegment() {
    if len(j.aotSegments) == 1 {
        j.soleSegment = j.aotSegments[0]
    } else {
        j.soleSegment = nil
    }
}
```

Mutation sites (call `refreshSoleSegment()` after each):
- `InstallAOT` (jit.go:267, once after the `for` loop)
- `Close` (jit.go:282–283, after clearing aotSegments)
- `InvalidateSegment` (jit.go:310, after splice)
- `InvalidateExecRegion` (jit.go:339, after `aotSegments = kept`)
- `nextExecuteSegment` (jit_segment.go:48, after append)

`lookupBlock` rewrite (jit.go:388):

```go
func (j *JIT) lookupBlock(pc uint64) *compiledBlock {
    if s := j.soleSegment; s != nil {
        // Fast path: single segment, inline the bounds check.
        if pc >= s.vaddrBegin && pc < s.vaddrEnd {
            if blk, ok := s.blocks[pc]; ok {
                return blk
            }
        }
    } else if len(j.aotSegments) > 0 {
        // Multi-segment path (preserved unchanged).
        if s := j.findSegment(pc); s != nil {
            if blk, ok := s.blocks[pc]; ok {
                return blk
            }
        }
    }
    idx := cacheIdx(pc)
    if j.cache[idx].pc == pc {
        return j.cache[idx].blk
    }
    return nil
}
```

`RunJIT` dispatch rewrite (jit.go:593–614):

```go
if seg := j.soleSegment; seg != nil {
    // Fast path: no findSegment, no blk.segment deref, no
    // null-chain. Publish the segment's params directly.
    res = jitcall.CallAOT(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
        cpu.mem.Base(), cpu.mem.Mask(),
        seg.decoderCacheBase, seg.decoderCacheMask,
        seg.vaddrBegin, seg.vaddrEnd-seg.vaddrBegin)
} else if len(j.aotSegments) > 0 {
    // Multi-segment path (preserved unchanged).
    seg := blk.segment
    if seg == nil {
        seg = j.hotSegment
        if seg == nil { seg = j.aotSegments[0] }
    }
    res = jitcall.CallAOT(blk.fn, …,
        seg.decoderCacheBase, seg.decoderCacheMask,
        seg.vaddrBegin, seg.vaddrEnd-seg.vaddrBegin)
} else {
    res = jitcall.Call(blk.fn, …)  // lazy-only, no AOT
}
```

**Why this should close the gap**: on coremark the fast path
shaves every Phase 2b cost — no findSegment call, no hotSegment
pointer check, no blk.segment deref, no null-chain. What
remains is `s.blocks[pc]` (same as Phase 2a) + four reads from
`*seg` (same as Phase 2a) + one extra `if seg := j.soleSegment;
seg != nil` check (1 load + 1 branch, well-predicted taken).
Net: we should *match* Phase 2a on single-segment workloads.

### Optimization 2 (contingent) — `hasExecRegions` flag + drop redundant findSegment

Only land if Optimization 1 doesn't hit the ±2% target or if a
profile of a lazy-heavy workload shows the cache-miss path
matters. Strictly hygiene otherwise.

- Add `j.hasExecRegions bool`, maintained by `AddExecRegion`
  and `RemoveExecRegion` on `GuestMemory` via a callback or
  re-read at `InstallAOT`. Simpler: check
  `len(j.aotSegments) > 0 || j.hasExecRegions` once, compute
  the flag during `InstallAOT` and on each mutation. The cost
  is indirection — `cpu.mem.execRegions` is one pointer + slice
  header dereference, i.e., 16 bytes already near-cache. The
  real win is a cleaner gate, not cycles.
- At `jit_segment.go:29`, the `findSegment(pc)` inside
  `nextExecuteSegment` is a defensive "segment concurrently
  appeared" check. In the single-goroutine RunJIT loop this is
  impossible — the caller at jit.go:704 only reaches here on
  `lookupBlock` miss, which itself just consulted findSegment.
  Drop the line. Saves a pointless linear-scan on the cold path.

### Optimization 3 (contingent) — pre-computed `vaddrSize` on segment

Only land if Optimization 1 misses target. Adds `vaddrSize
uint64` to `DecodedExecuteSegment`, set in `jitCompileAOTSegment`
as `vaddrEnd - vaddrBegin`. Replaces the subtraction in
`RunJIT` (jit.go:610) with a direct load. Single-digit
cycles/block win at best; fine-grained tuning.

### Optimization 4 (do NOT attempt in this pass)

Changing `jitcall.CallAOT`'s ABI to take a
`*DecodedExecuteSegment` requires `internal/jitcall/call_amd64.s`
edits (arg marshaling, sret extension publishing at
call_amd64.s:103–125), invalidates the existing JALR asm
sequence's sret read offsets, and widens the change surface by
an order of magnitude. Not justified unless 1–3 all miss target.

## Files to modify

| file | change |
|------|--------|
| `jit.go` | add `soleSegment` field; add `refreshSoleSegment()`; rewrite `lookupBlock` + `RunJIT` dispatch with fast-path; call refresh at 4 mutation sites (`InstallAOT`, `Close`, `InvalidateSegment`, `InvalidateExecRegion`) |
| `jit_segment.go` | call `j.refreshSoleSegment()` after the append at line 48; optionally drop the defensive `findSegment` at line 27–31 (Opt 2) |
| `Makefile` (optional) | add `bench-jit-coremark-prof` target if we decide the ergonomics are worth it — not strictly required |

Unchanged: interpreter, `ir/`, `internal/jitcall/`, the JALR
asm sequence, `GuestMemory`, ExecRegion API, ref-counting,
every Phase 2b test.

## Execution order

1. **Baseline profile + benchstat.** Capture current Phase 2b
   state before any change.
   ```bash
   CM_ELF=bench/coremark.elf \
     go test -count=1 -run='^$' -bench='^BenchmarkJIT_CoreMark_Fixed$' \
     -benchtime=10x -cpuprofile=cpu_base.prof ./bench/
   go tool pprof -list 'RunJIT|lookupBlock|findSegment' cpu_base.prof \
     > prof_base.txt
   # 10-run median:
   for i in $(seq 1 10); do
     go test -count=1 -run='^$' -bench='^BenchmarkJIT_CoreMark_Fixed$' \
       -benchtime=1x ./bench/ >> base.txt
   done
   ```
   Verify `findSegment`, `lookupBlock`, and the `blk.segment`
   null-chain in `RunJIT` show measurable time in the profile.
   If they don't (<1% each), the regression is elsewhere and
   this plan needs re-scoping.

2. **Implement Optimization 1.** Single focused commit: add
   `soleSegment` + `refreshSoleSegment` + fast-path in
   `lookupBlock` and `RunJIT` + refresh at all 5 mutation
   sites. No behavior change on multi-segment paths; they run
   the unchanged code.

3. **Regression test.**
   ```bash
   go test . ./ir/ ./bench/
   go test ./riscv-elf-tests/...
   ```
   All green before measuring.

4. **Post-Opt-1 measurement.** Re-run the 10-run benchstat
   commands for coremark, dhrystone, and bench_guest. Compare
   with `benchstat base.txt opt1.txt`. Target: coremark ≥ 940
   MIPS (p < 0.05 improvement over Phase 2b baseline), no
   regression >2% on dhrystone or bench_guest.

5. **If Opt 1 misses (<940 MIPS coremark):** implement
   Optimization 3 (pre-computed vaddrSize). Re-measure. If
   still short, widen the investigation with a finer profile.

6. **Optimization 2** (hygiene): implement independently of
   the benchmark target. Saves a defensive scan on a cold
   path. Gate solely on "is the change clean" — it is.

7. **Full regression sweep.** Before reporting done:
   ```bash
   make fuzz-oracle   # 60s
   make fuzz-fd       # 60s
   make fuzz-rvc      # 60s
   make fuzz-amo      # 60s
   make fuzz-bitmanip # 60s
   make bench-chain-ref  # stability of reference counts
   ```

## Verification

### Correctness
- Full test suite (`go test . ./ir/ ./bench/`) green.
- Phase 2b tests green (multi-segment install, cross-segment
  JALR, dynamic segment create, invalidate round-trip, refcount
  balance) — these specifically exercise the non-fast-path
  branches.
- riscv-elf-tests green.
- 5 fuzz targets green for ≥ 60s each.

### Performance (10-run medians, benchstat p < 0.05)
- coremark ≥ 940 MIPS (≥ +5% over current 895; within 2% of
  Phase 2a's 959).
- dhrystone within ±2% of 768 (no regression from Opt 1).
- bench_guest within ±2% of 3246 (no regression from Opt 1).

### Invariant check (soleSegment)
Hand-trace each mutation site to confirm `soleSegment` is set
to the right value after:
- `InstallAOT` with 0, 1, or 2+ PT_LOAD R-X — coremark hits 1.
- `nextExecuteSegment` append — coremark never hits this in
  its steady state; Phase 2b's `TestAOT_DynamicSegmentCreate`
  exercises it (transition 0→1 or 1→2).
- `InvalidateSegment` removing the only segment → nil.
- `InvalidateExecRegion` removing all → nil.
- `Close` → nil (aotSegments = nil path).

## Non-goals (this pass)

- **No `jitcall.CallAOT` ABI change.** No asm edits.
- **No segment-pointer indirection in JALR hot path.**
  Cross-segment JALRs still pay a Go round-trip (Phase 2c
  concern).
- **No interval tree for findSegment.** Linear scan is fine
  for the realistic N ≤ 10 target.
- **No `hasExecRegions` wiring unless Opt 1 is insufficient.**
  The cache-miss path is cold on coremark; optimization there
  is hygiene, not performance.
- **No interpreter changes, no IR changes, no lowering
  changes.**
- **No Phase 2c work** (Machine.Clone, CoW guest memory,
  FENCE.I invalidation).

## Risks / edge cases

- **Stale `soleSegment` after Invalidate/Close.** Both paths
  already clear `hotSegment`; the new invariant adds
  `soleSegment` to the same cleanup set. One helper, called at
  5 sites — hard to miss if the helper is named explicitly
  (`refreshSoleSegment`) and the mutation sites are listed in
  a comment near the helper.
- **Fast path taken after `nextExecuteSegment` creates a 2nd
  segment.** `refreshSoleSegment` must be called *after* the
  append in `nextExecuteSegment`; otherwise dispatch continues
  with stale soleSegment pointing at segment[0], missing hits
  in segment[1]. Covered by existing Phase 2b test
  `TestAOT_DynamicSegmentCreate` (single→multi transition).
- **Concurrency.** JIT is single-goroutine per-RunJIT; there
  are no existing atomics or mutexes on `aotSegments` or
  `hotSegment`. `soleSegment` follows the same convention. If
  Phase 2c adds fork/share, all three fields flip together —
  no new invariant burden.
- **Regression hidden by noise.** Coremark bench noise is
  ~2–3% run-over-run. Require `benchstat` with p < 0.05
  across 10 runs to declare the change landed; a single run
  can't prove it.
- **Opt 1 actually *regresses*** something. The new `if seg :=
  j.soleSegment; seg != nil` introduces one new branch on the
  hot path. If branch prediction mishandles the one-segment →
  multi-segment transition (rare in reality; always-taken
  after `InstallAOT`), the net could go negative. Very unlikely
  in practice — the flag flips at most a handful of times per
  Machine lifetime. Profile after the change to confirm.

## What to expect

If Opt 1 works as designed: coremark should hit ~950–960 MIPS,
closing the gap. Dhrystone and bench_guest should nudge up
slightly (same mechanism applies — they're single-segment too),
which would be a bonus. If the win is smaller (~915–930), Opt 3
is the next knob. Below 900 after Opt 1 means the profile was
lying or something else moved in the tree; stop and re-profile.
