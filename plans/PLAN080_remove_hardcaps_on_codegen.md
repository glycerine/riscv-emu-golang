# Plan: Remove maxBlockInsns and maxBlockIRInsns hard limits

## Context

Both `maxBlockInsns = 2048` and `maxBlockIRInsns = 2048` silently truncate JIT blocks mid-emission. In practice, real code always terminates blocks at branches/ECALL/JALR, so the limits almost never fire. But in AOT mode, a straight-line block that somehow exceeds 2048 instructions would be silently truncated — a lurking correctness hazard. The `maxBlockIRInsns` comment references "O(N*L) in the register allocator's spill phase" which refers to the deleted ELS (Extended Linear Scan) allocator. The current Fixed Static Mapping allocator has no such complexity concern.

## Changes

### 1. `jit_emit_ir.go` — remove limits and stale comments

**Delete** lines 18-27 (both declarations + comments):
```go
// maxBlockInsns limits the number of RISC-V instructions per JIT block.
// Variable so tests can adjust it without recompilation.
var maxBlockInsns = 2048

// maxBlockIRInsns limits the total IR instructions per block. Each RISC-V
// instruction expands to ~5-10 IR ops; very large blocks hit O(N*L) in the
// register allocator's spill phase where L is average interval length.
// 4096 IR instructions covers ~500-800 RISC-V instructions — large enough
// for good optimization, small enough for fast compilation.
const maxBlockIRInsns = 2048
```

**Simplify emission loop** at line 961-962:
```go
// Before:
for e.numInsns < maxBlockInsns && !e.terminated && e.pc < e.regionEnd &&
    len(irEm.Block.Instrs) < maxBlockIRInsns {

// After:
for !e.terminated && e.pc < e.regionEnd {
```

Note: `e.numInsns` field stays — it counts emitted instructions per block (used for diagnostics, debug logging, and the zero-check at line 998). It just stops being compared against a limit.

### 2. `jit.go` — remove ELS reference from SetAllocStrategy comment

**Replace** lines 242-246:
```go
// Before:
// SetAllocStrategy used to switch between Extended Linear Scan and Fixed
// Static Mapping allocators; ELS has been removed. The method is retained
// as a no-op that reinstalls the Fixed Static Mapping allocator and clears
// cached blocks, so existing callers (passing any strategy name) continue
// to work.

// After:
// SetAllocStrategy reinstalls the Fixed Static Mapping allocator and clears
// cached blocks, so existing callers continue to work.
```

### 3. `jit_emit_ir_test.go` — delete dead diagnostic tests, preserve correctness tests

**DELETE** these functions (dead diagnostic code — all logging commented out, no meaningful assertions, exist solely to manipulate `maxBlockInsns`):

| Function | Lines | Why delete |
|----------|-------|------------|
| `TestBisectBlockSize` | 1390-1433 | Bisects maxBlockInsns sizes with no assertions |
| `TestBisectBlockSize2` | 1435-1482 | Same pattern, narrower range |
| `TestBisectBlockSize3` | 1484-1530 | Same pattern |
| `TestDumpBlock_0x34e` | 1532-1580 | All logging commented out, no correctness check |
| `findHost` | 1582-1589 | Helper only used by deleted tests |
| `regName` | 1591-1602 | Helper only used by deleted tests |
| `TestDumpBlock_0x34e_v2` | 1604-1636 | All logging commented out, no correctness check |
| `TestDumpBlock_0x34e_v3` | 1638-1679 | All logging commented out, no correctness check |

**MODIFY** `TestNativeTrace_0x34e` (lines 1681-1804):
- Remove lines 1694-1695 (`maxBlockInsns = 15` / `defer`)
- This test has a real V1/V2 comparison assertion at line 1800 (`t.Error("V1/V2 register mismatch!")`) — that stays
- Block at 0x34e may be larger without the limit, but the correctness check is independent of block size

**MODIFY** `TestSRL_RealBlock_V1vV2` (lines 1132-1133):
- Update comments referencing `maxBlockInsns=9` and `maxBlockInsns=2048` — remove the stale block-size references from the comment text

### Tests preserved (no changes needed)

These tests don't reference `maxBlockInsns` or `maxBlockIRInsns`:
- `TestSRL_RealBlock_V1vV2` — V1/V2 lockstep comparison (only mentions limits in comments)
- `TestDumpBlock_ld_st_0x1a0` — backward-branch budget-check test
- `TestNativeTrace_sraw` / `testNativeTraceW` — V1/V2 comparison for sraw
- All tests using `numInsns` on `emitResult` struct (that field is kept as a per-block statistic)

## Files modified

| File | Change |
|------|--------|
| `jit_emit_ir.go` | Delete 2 declarations + stale comments, simplify loop |
| `jit.go` | Shorten SetAllocStrategy comment |
| `jit_emit_ir_test.go` | Delete 8 functions, modify 2 tests |

## Verification

```bash
go build ./...
go test -v -run 'TestNativeTrace_0x34e' .
go test -v -run 'TestSRL_RealBlock_V1vV2' .
go test -v -run 'TestDumpBlock_ld_st_0x1a0' .
go test -v .
go test -v ./...
```
