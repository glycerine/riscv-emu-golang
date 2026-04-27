# Plan: Fix Lazy JIT JALR Dispatch (Eliminate Go Round-Trip)

## Context

Lazy-compiled JIT blocks have no fast path for indirect jumps (JALR). `StepBlock` passes `dcBase=0` to dispatch (jit.go:585-589), so the decoder-cache lookup in `abjitJalrIC`/`rv8JalrIC` immediately misses. The 2-slot JALR IC that was supposed to handle this is dead code (`lowerOps.jalrICs` is never populated). **Every lazy JALR is a Go round-trip** — a performance gap with no mitigation.

The 2-slot IC was removed (commit 06f038a) because patching MOVABS slots in hot code caused SMC pipeline stalls. PLAN036 measured a 26% CoreMark regression (750 → 553 MIPS) with the 2-slot IC vs the L1 decoder cache. However, that comparison was AOT-vs-AOT; for lazy blocks where the decoder cache doesn't exist, the alternative is a Go round-trip on every JALR, which is far worse.

## Phase 1: Test — Prove the Problem Exists

Add a test that runs a guest program with JALR in **lazy-only mode** (`DisableAutoAOT=true`) and asserts that JALR dispatch doesn't round-trip back to Go for previously-seen targets.

### File: `jit_chaining_test.go` (new test)

**`TestLazyJIT_JALR_NoGoRoundTrip`**: Hand-craft a small RISC-V program with a JAL/JALR call-return pair in a loop. Run with `DisableAutoAOT=true`. After warmup iterations, check `jit.JalrICMisses` — currently this will show 1 miss per JALR (every iteration), proving the bug. After the fix, it should show at most 1-2 misses total (cold start), then zero.

Guest program structure:
```
entry:
  jal ra, func      # call func (saves return in ra)
  j entry           # loop back

func:
  nop               # do something
  jalr x0, ra, 0    # return via ra
```

This creates two blocks: the call site and the function body. The JALR return should be cacheable after the first miss.

### File: `jit_chaining_test.go` (new test)

**`TestLazyJIT_JALR_Benchmark`**: Same program but benchmarkable. Measure MIPS with `DisableAutoAOT=true` and compare to AOT mode. This quantifies the lazy JALR penalty.

## Phase 2: Benchmark — 2-Slot IC vs L1 Decoder Cache vs Go Round-Trip

Before choosing a fix, benchmark all three strategies on CoreMark and Dhrystone:

### File: `bench/jit_jalr_bench_test.go` (new file)

Three benchmarks per workload:

| Benchmark | Mode | JALR path |
|-----------|------|-----------|
| `BenchmarkJALR_Lazy_NoIC` | `DisableAutoAOT=true`, no 2-slot | Go round-trip (current) |
| `BenchmarkJALR_Lazy_2SlotIC` | `DisableAutoAOT=true`, 2-slot restored | 2-slot inline cache |
| `BenchmarkJALR_AOT_DecoderCache` | AOT enabled | L1 decoder cache (current) |

Each benchmark reports: MIPS, `JalrICMisses`, `JalrICDeopts`, total dispatch cycles.

This answers the key question: is the 2-slot IC faster than a Go round-trip for lazy blocks? (Almost certainly yes, but we verify.) It also measures how close 2-slot gets to the L1 decoder cache.

## Phase 3: Restore 2-Slot IC for Lazy Blocks

The 2-slot IC should be emitted **alongside** the decoder cache lookup, not instead of it. When `dcBase != 0` (AOT), the decoder cache handles it. When `dcBase == 0` (lazy), the 2-slot IC provides a fast path.

### Lowerer changes

**Files**: `lower_amd64_abjit.go` (`abjitJalrIC`) and `lower_amd64_rv8.go` (`rv8JalrIC`)

Modify the IRJalrIC lowering to emit:

```
; --- Decoder cache lookup (unchanged) ---
MOVQ dcBase, DX
TEST DX, DX
JEQ  .try_2slot          ; dcBase=0 → skip to 2-slot IC
; ... bounds check, index, load, hit/miss ...
JMP .dc_miss             ; decoder cache miss → also try 2-slot

.try_2slot:
; --- 2-slot inline cache ---
MOVABS R10, <sentinel_pc0>    ; patchable slot 0
CMP    target, R10
JEQ    .hit0
MOVABS R10, <sentinel_pc1>    ; patchable slot 1
CMP    target, R10
JNE    .miss                  ; both slots miss → Go round-trip

; hit1: jump to cached fn[1]
ADDQ   $frameSize, RSP
MOVABS R10, <sentinel_fn1>
JMP    R10

.hit0:
ADDQ   $frameSize, RSP
MOVABS R10, <sentinel_fn0>
JMP    R10

.dc_miss:                     ; decoder cache miss fallback
; (fall through to .try_2slot for lazy coexistence)
; OR: go directly to .miss if decoder cache should be authoritative

.miss:
; return jitOKJalrMiss to Go
```

**Design choice**: When decoder cache hits, skip 2-slot entirely (no SMC concern). When decoder cache misses (or dcBase=0), try 2-slot. This means:
- AOT blocks: decoder cache fast path, 2-slot never touched → no SMC
- Lazy blocks: 2-slot provides O(1) hit for bi-modal sites

### Lowerer plumbing

Append to `lc.jalrICs` in both `abjitJalrIC` and `rv8JalrIC` with the emitted MOVABS Prog pointers. The existing `jalrICInfo` struct (lower_amd64_shared.go:109-117) already has the right fields.

### Backpatch and patching

Already implemented and wired up:
- `backpatchJalrICs` (jit_native.go:109-143) — initializes sentinel slots
- `aotBackpatchJalrICs` (jit_aot.go:252-285) — same for AOT
- `tryPatchJalrIC` (jit.go:951-986) — shift-policy runtime patching
- `jalrICDeoptThreshold = 16` (jit.go:52) — stops patching polymorphic sites

These will activate once `lowerResult.JalrICs` is non-empty.

## Phase 4: Verify

1. **Run Phase 1 test**: `TestLazyJIT_JALR_NoGoRoundTrip` should now pass — after warmup, JalrICMisses stops increasing.

2. **Run Phase 2 benchmarks**: `BenchmarkJALR_Lazy_2SlotIC` should be significantly faster than `BenchmarkJALR_Lazy_NoIC` and within ~2x of `BenchmarkJALR_AOT_DecoderCache`.

3. **Regression check**: `go test -v ./...` — all existing tests pass, AOT benchmarks don't regress (2-slot code is never reached when decoder cache hits).

4. **CoreMark/Dhrystone AOT**: Verify no MIPS regression — the 2-slot code is dead code in the AOT path (decoder cache always hits first).

## Critical Files

| File | Change |
|------|--------|
| `jit_chaining_test.go` | New tests: lazy JALR round-trip detection |
| `bench/jit_jalr_bench_test.go` | New benchmarks: 3-way JALR comparison |
| `lower_amd64_abjit.go:483-572` | Add 2-slot IC emission after decoder cache miss |
| `lower_amd64_rv8.go:541-633` | Same |
| `lower_amd64_shared.go:109-117` | Already has `jalrICInfo` — no change needed |
| `lower_amd64_ops.go:60` | Already has `jalrICs` field — no change needed |
| `jit_native.go:109-143` | Already wired — activates when JalrICs non-empty |
| `jit.go:951-986` | Already wired — `tryPatchJalrIC` handles misses |
