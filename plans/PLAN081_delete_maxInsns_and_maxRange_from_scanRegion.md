# Plan: Remove Hardcoded JIT Compilation Limits in `scanRegion`

## Context

The lazy JIT path uses `scanRegion()` in `jit_decode.go:103-154` to BFS-discover the code region to compile. Two hardcoded constants artificially truncate large functions:

- `maxInsns = 2048` (line 104): caps distinct PCs visited by BFS
- `maxRange = 16384` (line 105): caps byte-range from entry PC to 16KB

These cause heisenbugs: functions with >2048 reachable PCs or spanning >16KB are silently truncated. Branches to truncated code produce unexpected chain exits. The AOT path (`collectBranchTargets` in `aot.go:51-83`) has no such limits — it already compiles entire text sections.

## Approach

Modify **one function** (`scanRegion` in `jit_decode.go`) to remove both limits. `flowCall` continues to follow call targets (preserves RAS inlining). The BFS is naturally bounded by terminators and instruction encoding limits (JAL: ±1MB, BEQ: ±4KB).

### Change 1: Remove `maxInsns` limit

**File:** `jit_decode.go`

- Delete `const maxInsns = 2048` (line 104)
- Change loop condition from `len(worklist) > 0 && visited.len() < maxInsns` to `len(worklist) > 0` (line 111)
- Change `newU64setSized(maxInsns)` to `newU64setSized(256)` (line 107) — initial capacity hint only; map grows dynamically

### Change 2: Remove `maxRange` limit

**File:** `jit_decode.go`

- Delete `const maxRange = 16384` (line 105)
- Line 118: change `if pc < entryPC || pc > entryPC+maxRange` to `if pc < entryPC` — keep backward-PC exclusion (emitter walks forward only)
- Lines 137, 141, 146: change `target >= entryPC && target <= entryPC+maxRange` to `target >= entryPC` — remove upper bound on all forward targets

## Other limits assessed — no changes needed

| Limit | Location | Why it stays |
|-------|----------|-------------|
| `MaxIC = 1<<16` | `ir/highlevel.go:14` | Runtime GC preemption, not a compilation limit. Block is fully compiled; MaxIC forces exit at backward branches after 65536 insns. |
| `blockCacheSize = 4096` | `jit.go:170` | Direct-mapped dispatch cache. Doesn't prevent compilation — just causes eviction/recompilation on collision. With larger blocks after this change, cache pressure actually decreases. |
| `jalrICDeoptThreshold = 16` | `jit.go:52` | IC patching optimization. Not a compilation limit. |
| `initialDirtySize = 128` | `ir/emit.go:18` | Grows dynamically via `growDirty()`. Just an initial hint. |
| `gotoTargets capacity 256` | `jit_emit_ir.go:936` | Initial capacity hint. Grows dynamically. |

## Files modified

- `jit_decode.go` — sole file changed (lines 103-151 of `scanRegion`)

## Verification

1. `go test -v .` — full unit test suite
2. `go test -v ./...` — includes fuzzoracle tests
3. `make bench-quick` — performance regression check (expect improvement: fewer truncated blocks, fewer chain exits)
4. Manual inspection: verify `TestInlineEcall_HelloEndToEnd` and `TestBloat_BenchGuest_0x10de` still pass (these exercise the lazy JIT path end-to-end)
