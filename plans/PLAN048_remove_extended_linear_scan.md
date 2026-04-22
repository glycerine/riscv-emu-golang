# Plan: Delete the Extended Linear Scan (ELS) Register Allocator

## Context

`/Users/jaten/ris/ir/` currently has two register allocators:

1. **ELS** — Extended Linear Scan, implemented in `ir/regalloc.go` (1130 lines). Type `Allocator`, constructor `NewAllocator()`. Original implementation with full liveness analysis, interval sets, spill-cost/resurrection, and forward/backward branch live-range extension.
2. **Fixed Static Mapping** — implemented in `ir/regalloc_fixed.go` (206 lines). Type `FixedStaticAllocator`, constructor `NewFixedStaticAllocator()`. Priority-ordered, no liveness analysis. Used by `NewJIT()` as the default.

The fixed allocator has superseded ELS. `NewJIT()` already defaults to fixed, and `jit.SetAllocStrategy("els")` is the only code path that still instantiates `NewAllocator()`. A known divergence bug at bench-guest block ~612061 (see `bench/els_diag_test.go`) also demonstrates ELS is no longer actively maintained.

Goal: delete ELS entirely so the codebase has a single allocator. Preserve shared infrastructure (types, interface, common helpers) that the fixed allocator needs. Per user direction, any surviving test that currently constructs `NewAllocator()` should be retargeted to `NewFixedStaticAllocator()` *where possible*; tests that probe ELS-only internals must be deleted.

## Surviving Shared Surface in `ir/regalloc.go`

After deletion, `regalloc.go` keeps only these items (all used by `regalloc_fixed.go` or by external callers):

- Types: `Interval`, `AllocKind` (+ constants `AllocUnused`, `AllocReg`, `AllocStack`), `IntervalAlloc`, `Allocation`, `RegMove`, `RegPool`
- Interface: `RegAllocator`
- Exported helpers:
  - `MaxVReg(b *Block) VReg` — used at `ris/jit_emit_ir.go:928`, `ris/ir/lower_amd64*_test.go`, `ris/ir/lower_amd64_dcache_test.go`, `ris/ir/lower_amd64_chain_test.go`, `ris/ir/lower_amd64_jalric_test.go`
  - `BlockHasDivMul(b *Block) bool` — used at `ris/ir/lower_amd64.go:73`, `ris/ir/lower_amd64_v2.go:36`

Everything else in `regalloc.go` is ELS-private and gets deleted: sort adapters (`intervalsByStart`, `iepByPoint`), internal types (`intervalSet`, `iep`, `allocState`), the `Allocator` struct, `NewAllocator()`, `(*Allocator).Allocate()`, and the ELS helpers (`computeIntervalSets`, `classifyVRegs`, `computeSpillCosts`, `spillIdentify`, `spillResurrect`, `assignRegisters`, `insertMoves`, `buildIEP`, `computeCount`, `extendLoopLiveRanges`, `extendForwardBranchRanges`, `instrDefs`, `instrUses`, `removeReg`, `findSCCs`).

## Files to Delete Entirely

| File | Why |
|------|-----|
| `ris/ir/els_test.go` | All 9 tests probe ELS-only behaviors (forward-branch extension, loop extension, spill holes, spill resurrection, masked-load patterns). Not portable to fixed. |
| `ris/ir/els_fuzz_test.go` | 4 fuzz tests that validate ELS-only invariants via `computeIntervalSets`. |
| `ris/bench/els_diag_test.go` | Whole file is ELS-vs-Fixed comparison. |

## Files to Modify

### `ris/ir/regalloc.go` — rewrite (keep filename, shrink ~1130 → ~100 lines)

Strip all ELS code; leave only the shared surface listed above. `import "sort"` becomes unused and is removed.

### `ris/ir/regalloc_test.go` (1396 lines) — extensive trimming

Delete:
- All `TestInstrDefs_*`, `TestInstrUses_*`, `TestIntervalSets_*`, `TestBuildIEP_*`, `TestComputeCount_*`, `TestClassifyVRegs_*` — these call ELS-internal functions that no longer exist.
- All `TestComputeSpillCosts_*` — `computeSpillCosts` is ELS-only.
- ELS-specific `TestAllocate_*`: `TestAllocate_SpillResurrection`, `TestAllocate_NoResurrection`, `TestAllocate_OneLongVsManyShort`, `TestAllocate_IntervalHoleReuse`, `TestAllocate_PreferSameReg` (these assert ELS-specific spill/reuse policy).
- Fuzz tests: `FuzzRegAllocInvariants`, `FuzzLiveRangeConsistency`, `FuzzSpillResurrection` (check ELS-only invariants).

Keep (and re-run under fixed):
- `TestBlockHasDivMul_*`, `TestMaxVReg_*` — test helpers that survive.
- Generic `TestAllocate_*` that assert allocator-agnostic invariants (no conflicts, VReg zero never allocated, pinned regs honored, used VRegs get a slot). Examples: `TestAllocate_EmptyBlock`, `TestAllocate_AllFitNoOverlap`, `TestAllocate_VRegZeroNeverAllocated`, `TestAllocate_PinnedRegs*`, `TestAllocate_IntAndFP_SeparatePools`, `TestAllocate_GuestFPRegs`.
- Test helpers: `makeBlock`, `testPool`, `assertAllocReg`, `assertAllocStack`, `assertAllocUnused`, `regAt`, `assertRegAt`, `assertNoConflicts` — allocator-agnostic.

Drop-and-see rule: after the retarget, compile and run. If a kept `TestAllocate_*` fails because fixed assigns different registers or uses different spill slots, delete that test — it was checking ELS implementation detail, not correctness.

### `ris/ir/lower_amd64_test.go` (line 36-39)

Change `helperTestAllocate`:
```go
func helperTestAllocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
    a := NewFixedStaticAllocator()   // was: NewAllocator()
    return a.Allocate(b, pool, pinned, freq)
}
```
This automatically retargets every caller in `regalloc_test.go` (32 call sites) and the two in this file (lines 1207, 1368) to fixed.

### `ris/jit.go` (lines 240-252)

Simplify `SetAllocStrategy` by removing the `"els"` case. Two reasonable options — recommend **option A**:

**Option A (recommended):** delete `SetAllocStrategy` entirely, since fixed is the only choice and `NewJIT()` already installs it. Remove the `irAlloc ir.RegAllocator` interface field too — assign `FixedStaticAllocator` directly. Update the few callers (see below).

**Option B:** keep `SetAllocStrategy` as a no-op that always installs fixed (useful if the method is called from downstream code we don't control). Given all callers are in-tree, option A is cleaner.

Callers to update (all pass "fixed" already, so they just need the `SetAllocStrategy` call removed after option A):
- `ris/bench/jit_aot_bench_test.go:18`
- `ris/bench/jit_chain_reference_test.go:40`
- `ris/bench/jit_bench_test.go:111` (inside `benchJITELF`; delete the `strategy` parameter — see next)

### `ris/bench/jit_bench_test.go`

Delete ELS-using benchmarks:
- `BenchmarkCPU_FullExecution_JIT` (lines 46-48) — calls `benchJITWith(b, "els")`. The companion `BenchmarkCPU_FullExecution_JIT_Fixed` (lines 50-52) covers the fixed case.
- `BenchmarkJIT_CoreMark_ELS` (lines 85-87).
- `BenchmarkJIT_Dhrystone_ELS` (lines 95-97).

Simplify `benchJITWith` and `benchJITELF` to drop the `strategy` parameter; remove `jit.SetAllocStrategy(strategy)` (line 111). Rename remaining callers accordingly.

### `ris/bench/lower_bench_test.go`

Three call sites (lines 95, 136, 217): `ir.NewAllocator()` → `ir.NewFixedStaticAllocator()`.

### `ris/Makefile`

- Line 167: drop "ELS vs Fixed vs" from `bench-alloc` help text (or rename target — keep `bench-alloc` but update description).
- Lines 176-177: `bench-jit-coremark`/`bench-jit-dhrystone` help text → "CoreMark under JIT"/"Dhrystone under JIT" (no "Fixed vs ELS").
- Lines 524, 531: banner echoes — drop "vs ELS".
- Lines 603-616: `bench-alloc` target prints 4 MIPS rows (interpreter, ELS, Fixed, TCC). Drop the "Go JIT — ELS allocator (native)" stanza (lines 610-616), since `BenchmarkCPU_FullExecution_JIT` is being deleted.
- Line 610: update "ELS allocator" label (covered by the stanza deletion above).
- Lines 851-869: delete the 4 `FuzzELS_*` stanzas `[9/19]` through `[12/19]` from the `fuzz-all` target. Renumber remaining stanzas 13-19 down to 9-15 (and adjust the `/19` denominator in each label).

## Verification

Run from `/Users/jaten/ris`:

1. **Compile cleanly:** `go build ./...`
2. **Unit tests pass:** `go test ./ir/`
   - Expect all surviving `TestAllocate_*`, `TestBlockHasDivMul_*`, `TestMaxVReg_*`, plus the unchanged `lower_amd64*_test.go` suite, to pass.
3. **Full suite:** `go test ./...`
4. **Residual-reference sweep:** verify nothing still mentions the deleted symbols —
   - `grep -rn "NewAllocator\b" .` → zero hits in `.go` files (plans/ may still mention it, that's fine)
   - `grep -rn "FuzzELS\|TestELS" .` → zero hits in `.go` files
   - `grep -rn "\"els\"" .` → zero hits in `.go` files
5. **Bench smoke:** `make bench-quick` runs without errors.
6. **Makefile sanity:** `make fuzz-all` lists 15 stanzas, not 19 (or whatever the new count is), and no `FuzzELS_*` targets.

## Notes / Decisions

- **No changes needed in `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/`** — that's a separate TCC-based project and does not import `ris/ir`.
- **The `plans/` directory keeps historical ELS references** — per `CLAUDE.md`, plans/ is user archives and is not for modification.
- **Bench binaries** (`bench.test`) will rebuild on next `go test`; no cleanup needed.
