# Phase 2b: Multi-segment `DecodedExecuteSegment`s

## Context

Phase 2a shipped whole-program AOT for a single segment (`.text`
from the ELF) with a mask-bounded read-only `decoder_cache` for
JALR fast-dispatch. Measured (median of 10):

| workload    | lazy 2-way IC | AOT (shipped) | Δ vs lazy |
|-------------|--------------:|--------------:|----------:|
| bench_guest |          3204 |          3275 |  +1%      |
| coremark    |           769 |           959 | +33%      |
| dhrystone   |           618 |           771 | +51%      |

Phase 2b extends Phase 2a to multiple segments, with dynamic
creation when JALR targets guest-writable+executable regions,
and adds ref-counting on segments so a future Machine.Clone /
fork can share the (immutable, mprotect'd RO) code and decoder
caches between parent and child Machines without copying them.

Motivating use case: LuaJIT-style guests — guest code that
writes its own machine code into mmap'd executable memory and
then jumps to it. In Phase 2a such guests run correctly but
slowly (dynamic code misses `.text`'s decoder_cache, falls
through to lazy-compile). Phase 2b makes the dynamic code region
a first-class segment with its own decoder_cache.

### Scope decisions (confirmed)

| decision | choice |
|---|---|
| Fork API | **No.** Only add ref-counting infrastructure. Actual Machine.Clone and CoW guest memory stay out — they ship as Phase 2c (or later). |
| FENCE.I behavior | **No-op on segments**, matches libriscv. Guests that self-modify without re-mapping get stale dispatch. LuaJIT's normal pattern (write code into a *fresh* mmap, never re-use an address) is unaffected. |
| Segment-boundary discovery | **ExecRegion table** on `GuestMemory`. Populated from ELF PT_LOAD R-X at load; maintained at runtime via os.go syscall hooks (feature-flagged — not required for Phase 2b's tests). No page-by-page host-side permission scan; our flat guest mmap has no per-page perms. |

## Terminology

Inherited from Phase 2a; Phase 2b activates the two fields
Phase 2a deferred:

| libriscv name | meaning here |
|---|---|
| `m_is_likely_jit` | `isLikelyJIT bool` on segment — backs RW-X guest pages |
| `next_execute_segment(pc)` | Go-side lookup-or-create on JALR miss into an unclaimed exec region |

(The libriscv `m_is_stale` backstop is deferred indefinitely —
we have no runtime-decode loop where it could fire.)

## Phase 2a state (what we're generalizing)

Key single-segment assumptions that must become many-segment
aware (from live code inspection):

| file:line | code | generalize to |
|---|---|---|
| `jit.go:119–124` | `JIT.aotSegment *DecodedExecuteSegment` | `aotSegments []*DecodedExecuteSegment` plus `hotSegment` cache |
| `jit.go:185–197` | `InstallAOT` → `j.aotSegment = seg` | loop over PT_LOAD R-X; append one segment each |
| `jit.go:218–229` | `lookupBlock(pc)` reads `j.aotSegment` | `findSegment(pc)` + lookup in its `blocks` |
| `jit.go:424–429` | `RunJIT` passes `seg.decoderCacheBase/Mask/vaddrBegin/size` | pass the *containing* segment's params, re-selected per dispatch |
| `jit_aot.go:35` | `jitCompileAOTSegment(..., vaddrBegin, vaddrEnd)` | unchanged (already per-segment) |
| `ir/lower_amd64.go:611–697` | `emitDecoderCacheLookup` uses `[RBX+88..112]` | unchanged (trampoline publishes current segment's values each CallAOT) |
| `internal/jitcall/call_amd64.s:103–125` | `CallAOT` publishes 4 sret slots | unchanged |

**Critical property**: the JALR asm sequence is completely
segment-agnostic — it reads the "current segment" parameters
from the sret extension at `[RBX+88..112]`, which the Go-side
dispatcher publishes on every `CallAOT`. This means **cross-
segment JALRs just take the existing `.dc_miss` fallback path
(2-way IC → Go round-trip), and Go re-dispatches with the new
segment's parameters**. No asm change needed.

## Design

### Part A — `aotSegments []*DecodedExecuteSegment` + `findSegment(pc)`

Replace the single pointer with a slice. Keep a `hotSegment`
cache to short-circuit the common case of "same segment as last
dispatch."

```go
type JIT struct {
    ...
    aotSegments []*DecodedExecuteSegment
    hotSegment  *DecodedExecuteSegment
    ...
}

func (j *JIT) findSegment(pc uint64) *DecodedExecuteSegment {
    if s := j.hotSegment; s != nil && pc >= s.vaddrBegin && pc < s.vaddrEnd {
        return s
    }
    for _, s := range j.aotSegments {
        if pc >= s.vaddrBegin && pc < s.vaddrEnd {
            j.hotSegment = s
            return s
        }
    }
    return nil
}
```

`lookupBlock(pc)` (jit.go:218) becomes:
```go
if s := j.findSegment(pc); s != nil {
    if blk, ok := s.blocks[pc]; ok { return blk }
}
// fall through to direct-mapped lazy cache (unchanged)
```

`RunJIT` dispatch (jit.go:424–429): select the segment per
block (via `findSegment(blk.pc)` — or cache the segment pointer
on `compiledBlock` at AOT-install time so it's one field read).

### Part B — Segment metadata extensions

```go
type DecodedExecuteSegment struct {
    // Phase 2a fields (unchanged)
    vaddrBegin       uint64
    vaddrEnd         uint64
    nativeCodeBase   uintptr
    nativeCodeSize   int
    decoderCacheMmap []byte
    decoderCacheBase uintptr
    decoderCacheMask uint64
    blocks           map[uint64]*compiledBlock

    // Phase 2b additions
    isLikelyJIT bool         // backs RW-X guest pages
    refcount    atomic.Int32 // 1 on install; Retain/Release manage
}
```

Segments are semantically immutable once `jitCompileAOTSegment`
returns: mmaps allocated, chain exits pre-patched, decoder_cache
already mprotect RO, `blocks` read-only. This is the property
that makes sharing safe.

### Part C — `GuestMemory` exec region table

```go
type ExecRegion struct {
    VAddrBegin  uint64
    VAddrEnd    uint64
    IsLikelyJIT bool // true ⇔ writable by guest (RW-X page)
}

type GuestMemory struct {
    ... existing fields ...
    execRegions []ExecRegion // small N; linear scan
}

func (m *GuestMemory) AddExecRegion(begin, end uint64, isJIT bool)
func (m *GuestMemory) RemoveExecRegion(begin, end uint64)
func (m *GuestMemory) FindExecRegion(pc uint64) *ExecRegion
```

Populated at ELF load from PT_LOAD R-X headers. Maintained at
runtime via os.go syscall hooks (see Part F — optional wiring,
not required for Phase 2b test coverage). The table is guest-VA
metadata only; it does not affect the mask-based sandbox
invariant (`hostPtr = base + (addr & mask)`). No mprotect on the
flat guest mmap itself.

### Part D — ELF PT_LOAD R-X enumeration

New in `elf.go`:
```go
type ExecLoad struct {
    VAddr    uint64
    MemSz    uint64
    Writable bool // true ⇔ PF_W set (rare: RW-X PT_LOAD)
}

func FindExecLoads(data []byte) ([]ExecLoad, bool)
```

Walks `hdr.PhOff` entries (same pattern as `loadELFReader` at
elf.go:127), filters `ph.Type == ptLoad && (ph.Flags & PF_X)`,
returns the list. `PF_W = 0x2`, `PF_X = 0x1` — add these
constants alongside the existing `ptLoad`.

Keep `FindTextSection` (used by existing tests) but
`InstallAOT` migrates to `FindExecLoads` for correctness on
binaries with multiple R-X loads.

### Part E — Dynamic segment creation (`nextExecuteSegment`)

New in `jit_segment.go`:
```go
func (j *JIT) nextExecuteSegment(mem *GuestMemory, pc uint64) *DecodedExecuteSegment {
    region := mem.FindExecRegion(pc)
    if region == nil {
        return nil // not exec; caller falls to lazy
    }
    // Compile the whole region as a new segment.
    size := region.VAddrEnd - region.VAddrBegin
    ranges := enumerateBlockRanges(mem, region.VAddrBegin, size)
    seg, err := j.jitCompileAOTSegment(mem, ranges, region.VAddrBegin, region.VAddrEnd)
    if err != nil {
        return nil
    }
    seg.isLikelyJIT = region.IsLikelyJIT
    seg.refcount.Store(1)
    j.aotSegments = append(j.aotSegments, seg)
    return seg
}
```

Wired into `StepBlock` / `RunJIT` at the point where
`lookupBlock(pc)` returns nil and `findSegment(pc)` is nil and
the existing lazy path would fire. Tried *before* the lazy path:
if pc is in an exec region, create a segment; else fall to lazy.

### Part F — os.go syscall hooks (optional)

For guests that grow exec regions at runtime (real LuaJIT,
synthetic test binaries), the OS personality's mmap / mprotect
handlers call `mem.AddExecRegion` on `+X` transitions and
`mem.RemoveExecRegion` on `-X` transitions.

Phase 2b **does not require** any new OS personality to be
wired. The ExecRegion API exists; the hooks are added when a
particular OS personality needs them. Tests exercise dynamic
creation via `mem.AddExecRegion` called directly from the test
body (synthesizing a guest that set up exec memory).

### Part G — Ref-counting (fork readiness)

```go
func (s *DecodedExecuteSegment) Retain() { s.refcount.Add(1) }

func (s *DecodedExecuteSegment) Release() {
    if s.refcount.Add(-1) == 0 {
        syscall.Munmap(s.nativeCodeMmap) // held as []byte for this
        syscall.Munmap(s.decoderCacheMmap)
    }
}
```

This requires keeping the native-code mmap as `[]byte` (not just
`uintptr`) so `Munmap` has something to unmap. Phase 2a's
`jit_aot.go:84–89` already has `execMem []byte` locally; we
stash it on the segment.

`(j *JIT) Close()` iterates `aotSegments` and calls `Release()`
on each. Add to any existing JIT teardown path; add
`t.Cleanup(j.Close)` in Phase 2b-new tests so mmaps don't leak.

Ref-counting is the **only** Phase 2b enabler for fork. The
actual `Machine.Clone()` / CoW guest memory is Phase 2c. The
value of landing ref-counting now: segments are already
structurally immutable after install; adding the ref-count
cements that invariant in code and prevents future changes from
accidentally mutating a shared segment.

### Part H — Invalidation API (explicit only)

```go
func (j *JIT) InvalidateSegment(pc uint64)       // single segment containing pc
func (j *JIT) InvalidateExecRegion(b, e uint64)  // all segments in range
```

Removes the segment from `aotSegments`, clears `hotSegment` if
it matches, calls `Release()`. Does *not* fire from in-guest
FENCE.I (user decision: match libriscv). Callable by:

- Tests (verifying re-creation)
- Future os.go syscall hooks (munmap, mprotect -X) as part of
  Part F wiring
- A future Phase 2c FENCE.I opt-in, should some guest need it

### Part I — `InstallAOT` loops over PT_LOAD R-X

```go
func (j *JIT) InstallAOT(mem *GuestMemory, elfBytes []byte) error {
    loads, ok := FindExecLoads(elfBytes)
    if !ok || len(loads) == 0 {
        return nil
    }
    for _, load := range loads {
        mem.AddExecRegion(load.VAddr, load.VAddr+load.MemSz, load.Writable)
        ranges := enumerateBlockRanges(mem, load.VAddr, load.MemSz)
        seg, err := j.jitCompileAOTSegment(mem, ranges, load.VAddr, load.VAddr+load.MemSz)
        if err != nil {
            continue
        }
        seg.isLikelyJIT = load.Writable
        seg.refcount.Store(1)
        j.aotSegments = append(j.aotSegments, seg)
    }
    return nil
}
```

For dhrystone / coremark (single R-X load = `.text`), behavior
is identical to Phase 2a.

## Files to modify

| file | change |
|------|--------|
| `jit.go` | `aotSegment` → `aotSegments []*DecodedExecuteSegment`, add `hotSegment`, `findSegment`, update `lookupBlock`, update `RunJIT` dispatch to pass per-block segment params to `CallAOT`; new `Close()`, `InvalidateSegment`, `InvalidateExecRegion`; rewrite `InstallAOT` to loop over PT_LOAD R-X |
| `jit_aot.go` | segment init: set `refcount=1`, keep `execMem []byte` on segment for Munmap |
| `jit_segment.go` (new) | `Retain`/`Release`, `nextExecuteSegment`, segment-level helpers |
| `aot.go` | unchanged (generic) |
| `elf.go` | add `FindExecLoads`, constants `pfX=0x1`, `pfW=0x2` |
| `guestmem.go` | add `execRegions []ExecRegion` + `AddExecRegion` / `RemoveExecRegion` / `FindExecRegion` |
| `aot_test.go` | multi-segment create, cross-segment JALR correctness, `InvalidateSegment` roundtrip, dynamic segment creation, ref-count balance |
| `bench/jit_chain_reference_test.go` | no change (single-segment ELF benchmarks unchanged) |

Unchanged: interpreter; `ir/lower_amd64.go`; the JALR asm
sequence; `internal/jitcall/call_amd64.s`; guest memory sandbox
invariants; the 2-way IC; `os.go` (unless a concrete personality
wires mmap/mprotect hooks — out of scope).

## Execution order

1. **Step 1 — ExecRegion table.** Add `ExecRegion` struct + the
   three methods on `GuestMemory`. Unit test add/remove/find
   (overlapping adds, boundary hits, empty).
2. **Step 2 — PT_LOAD R-X enumeration.** `FindExecLoads` in
   elf.go. Unit test against dhrystone + coremark ELFs (expect
   one load each) and against a hand-crafted ELF with two R-X
   loads.
3. **Step 3 — Multi-segment refactor (no behavior change).**
   `aotSegments []*DecodedExecuteSegment` + `hotSegment` +
   `findSegment`; update `lookupBlock` and `RunJIT` dispatch.
   `InstallAOT` temporarily still appends one segment (from
   `FindTextSection`) — this step is a pure refactor. All Phase
   2a tests stay green; dhrystone/coremark MIPS within ±2%.
4. **Step 4 — `InstallAOT` loops over PT_LOAD R-X.** Flip to
   `FindExecLoads`. Add a hand-crafted-ELF test that produces
   ≥2 segments, exercising a cross-segment JALR: first call
   into the second segment pays the Go round-trip, subsequent
   calls within it hit its decoder_cache.
5. **Step 5 — Ref-counting.** `refcount atomic.Int32`,
   `Retain`/`Release`, `JIT.Close()`. Keep `execMem []byte` on
   segment for Munmap. Test: install, Close, assert mmaps are
   unmapped (munmap of a pointer already-unmapped would
   SIGSEGV, so we just assert Close succeeds and nothing leaks).
6. **Step 6 — Dynamic segment creation.** `nextExecuteSegment`;
   wire into the dispatch path before lazy. Test: after
   `mem.AddExecRegion(base, end, true)` and writing valid RISC-V
   instructions at `base`, a JALR to `base` creates a new
   segment, its decoder_cache is populated, a second JALR hits
   the fast path (counter evidence: `JalrICMisses` bumps once,
   not per-call).
7. **Step 7 — Invalidation API.** `InvalidateSegment`,
   `InvalidateExecRegion`. Test: install, invalidate, next
   dispatch re-creates; guest-visible state unchanged.
8. **Step 8 — Regression + benchmark.**
   - `go test . ./ir/ ./bench/`
   - `go test ./riscv-elf-tests/...`
   - `make fuzz-oracle`, `fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
     `fuzz-bitmanip` (30 s each)
   - `make bench-chain-ref` × 10 runs; dhrystone/coremark MIPS
     within ±2% of Phase 2a medians (959, 771)

## Verification

### Correctness

- Full unit test suite green (`go test ./... ./ir/ ./bench/`).
- Fuzz targets (oracle, fd, rvc, amo, bitmanip) 30 s each.
- riscv-elf-tests suite passes unchanged.

### Phase 2b specific tests (in `aot_test.go`)

| test | assertion |
|------|-----------|
| `TestAOT_MultiSegment_Install` | synthetic ELF with two R-X PT_LOADs → `len(j.aotSegments) == 2`, both fully populated |
| `TestAOT_CrossSegmentJALR` | first JALR into segment B pays Go round-trip (`JalrICMisses` += 1); subsequent calls inside B hit decoder_cache (no further bumps) |
| `TestAOT_DynamicSegmentCreate` | after `mem.AddExecRegion` + writing valid instructions, JALR triggers `nextExecuteSegment`; segment appears in `j.aotSegments`; second call hits decoder_cache |
| `TestAOT_InvalidateSegment_Roundtrip` | install → invalidate `.text` → next dispatch re-creates → result deterministic |
| `TestAOT_RefcountBalance` | install, Close, assert every segment's `refcount == 0` at end |
| `TestAOT_SinglePTLoad_UnchangedBehavior` | dhrystone / coremark unchanged (one segment, same counters, same MIPS ± noise) |

### Performance

- dhrystone / coremark MIPS within ±2% of Phase 2a medians.
  (Same single-segment dispatch path; any regression is a
  refactor bug.)
- No new benchmark targets for Phase 2b — multi-segment workloads
  (LuaJIT-style) will arrive with Phase 2c + a LuaJIT guest ELF.

## Non-goals (Phase 2b)

- **No Machine.Clone() / Fork API.** Ref-counting only; fork
  shipped in 2c.
- **No CoW guest memory.** Shipped in 2c (via memfd on Linux /
  tmpfile-or-eager-copy on Darwin).
- **No FENCE.I-driven invalidation.** Matches libriscv. Guests
  that SMC without re-mapping get stale dispatch (documented).
- **No per-page guest memory permissions.** `ExecRegion` is a
  coarse range map, not a page table.
- **No JALR asm changes.** Cross-segment JALRs pay one Go
  round-trip per boundary crossing (acceptable: same cost as
  today's `dc_miss` path, which already dispatches via Go).
- **No interpreter, V2 lowerer, or 2-way IC changes.**
- **No automatic OS-personality syscall hook wiring.** The
  ExecRegion API exists; personalities that need it wire in
  separately.

## Risks / edge cases

- **SMC without re-mapping** — guest writes to an AOT-compiled
  PC without changing its exec region. Translation becomes
  stale. Matches libriscv's behavior *without* its
  illegal-bytecode backstop. Mitigations: (a) user can call
  `InvalidateSegment` explicitly; (b) Phase 2c can add an
  opt-in FENCE.I invalidation path.
- **Segment list growth** — linear `findSegment` is O(n). A
  long-running LuaJIT guest creating many small regions could
  make n large. Typical n for our target workloads: 1–10. If
  profiling shows n>50 hurting, upgrade to interval tree. Out
  of scope for Phase 2b.
- **Overlapping ExecRegions** — `AddExecRegion` must coalesce
  or reject overlaps. Chosen: coalesce into one (last-writer-
  wins on `IsLikelyJIT`). Test covers this.
- **Cross-segment JALR frequency** — every cross-segment call
  pays a Go round-trip. For `.text`-only benchmarks, n=1 so
  zero crossings. For guests with multiple R-X loads (e.g.,
  dynamic linking), crossings are rare (function-call
  granularity, not loop granularity).
- **Ref-count on teardown forgotten** — tests that forget
  `j.Close()` leak mmaps. Mitigation: `t.Cleanup(j.Close)`
  helper used consistently in new tests.
- **PT_LOAD R-X with PF_W** — legal but rare (RW-X segments).
  Handled: `isLikelyJIT=true` on load; treated as a JIT segment
  from the start. Matches libriscv's `is_likely_jit` heuristic.
- **decoder_cache size for many segments** — each segment's
  cache is ≥ 8 bytes, rounded to power of two. A 4 KB segment
  → 16 KB cache. 100 such segments → 1.6 MB. Acceptable. If a
  guest creates thousands of tiny regions, we'd re-evaluate.

## Phase 2c — sketch (not this plan)

- `Machine.Clone()` that `Retain`s all segments and either
  eager-copies guest memory (darwin, simplest) or maps CoW via
  memfd+MAP_PRIVATE (Linux).
- os.go mmap/mprotect hooks that update the ExecRegion table
  and invalidate segments on `-X` transitions.
- Opt-in FENCE.I invalidation for SMC-heavy guests (behind a
  JIT flag; default off to preserve libriscv parity).
- Segment-pointer indirection in the JALR hot path if profiling
  shows cross-segment round-trips matter.
- `madvise MADV_FREE` on segment Release for eager RAM return.
- Benchmark: a LuaJIT guest ELF targeting ≥ 1.5 × interpreter
  speedup on Lua microbenchmarks.
