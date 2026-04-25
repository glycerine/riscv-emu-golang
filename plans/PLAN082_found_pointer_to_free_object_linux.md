# GC Crash Analysis: "found pointer to free object"

## Context

The test suite crashes on a remote Linux box (Go 1.26.0) with:
```
runtime: marked free object in span 0x7bcf2c3105a8, elemsize=16 freeindex=0
(bad use of unsafe.Pointer or having race conditions? try -d=checkptr or -race)
fatal error: found pointer to free object
```
The crash occurs during `TestRISCVTests_UI_JIT/ld_st` — a JIT-mode test of RISC-V load/store instructions. The GC's background sweeper finds a Go pointer referencing a freed 16-byte heap object.

## Answer: Is this caused by storing JIT code pointers in Go memory?

**No, not directly.** The `uintptr` fields (`fn`, `chainEntry`, `nativeCodeBase`, `decoderCacheBase`) are invisible to the GC — it doesn't trace `uintptr`. The `nativeMmap []byte` slices point to mmap'd memory outside the Go heap, which the GC also skips. These design choices are correct.

However, the crash IS related to the JIT infrastructure through **two contributing causes** identified below.

## Root Cause 1: Missing `JIT.Close()` in test helper (confirmed)

**File:** `jit_test.go:114` — `runJITWithOS()`

```go
func runJITWithOS(cpu *CPU) (exitCode int, err error) {
    ...
    jit := NewJIT()
    err = jit.RunJIT(cpu)
    return   // ← no jit.Close()!
}
```

Every call to `runJITWithOS` leaks all lazy-compiled block mmaps. By the time `TestRISCVTests_UI_JIT/ld_st` runs, dozens of previous subtests have leaked mmap regions. These leaked regions:
- Fragment the virtual address space
- Prevent the OS from coalescing freed spans
- Can cause the Go runtime's arena hints to overlap with stale mmap regions on Go 1.26's expanded heap layout

**Fix:** Add `defer jit.Close()` after `NewJIT()`.

## Root Cause 2: `unsafe.Pointer` conversions from stored `uintptr` (likely trigger)

Multiple hot-path locations convert a *stored* `uintptr` back to `unsafe.Pointer` via arithmetic — a pattern that violates Go's `unsafe.Pointer` rules:

| File | Line | Pattern |
|------|------|---------|
| `run_cached.go` | 122, 265, 518, 539, 677, 718 | `*(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(...)))` |
| `exec_slot.go` | 69, 86, 109, 126 | same pattern |
| `exec_slot32.go` | 55, 118 | same pattern |
| `guestmem.go` | 248, 259, 299 | `unsafe.Pointer(m.base + uintptr(...))` |
| `jit.go` (patchChainTarget) | 807 | `unsafe.Pointer(codeBase + uintptr(...))` |
| `jit.go` (readJalrICSlot) | 870 | same pattern |

Go's rules say `unsafe.Pointer(uintptrExpr)` is only valid when the `uintptr` was obtained from `unsafe.Pointer` **in the same expression**. Here, `cpu.mem.base` is a struct field stored long before use. The pointer happens to target C mmap memory (not Go heap), so previous Go versions ignored this. **Go 1.26 may enforce stricter checking or its GC arena layout may cause the stale-uintptr conversion to confuse span tracking.**

The `elemsize=16` in the crash (a span of 16-byte objects, all freed) doesn't match any specific JIT type — it's likely collateral damage: the illegal `unsafe.Pointer` conversion produces a value that the GC interprets as pointing into an unrelated freed span.

## Root Cause 3 (speculative): JIT native code heap corruption

If JIT-emitted code writes past the register file arrays (`x[32]` or `f[32]` at RSI/RDX), it would overwrite adjacent fields in the heap-allocated `CPU` struct or even neighboring heap objects. This would produce exactly this class of GC crash — random-looking span corruption. The `ld_st` test exercises load/store emission heavily, making it a likely trigger for any off-by-one in address calculation.

## Proposed Fix Plan

### Step 1: Add `defer jit.Close()` to `runJITWithOS`
**File:** `jit_test.go:133`
```go
jit := NewJIT()
defer jit.Close()   // ← add this
err = jit.RunJIT(cpu)
```
This fixes the mmap leak. Alone it may resolve the crash by eliminating the address space fragmentation that triggers the GC's span confusion on Go 1.26.

### Step 2: Verify on the remote Linux box
Run `make test` on the remote box after Step 1. If the crash disappears, we're done.

### Step 3 (if crash persists): Run diagnostic builds
```bash
go test -count=1 -gcflags=all=-d=checkptr -v .      # detect unsafe.Pointer misuse
go test -count=1 -race -v .                           # detect data races
```
These will pinpoint the exact line where an illegal conversion or race occurs.

### Step 4 (if checkptr flags the `mem.base` pattern): Fix unsafe.Pointer usage
The `cpu.mem.base` pattern can be made legal by keeping the original `unsafe.Pointer` alive. Options:
- Store `base` as `unsafe.Pointer` instead of `uintptr`, with pointer arithmetic in single expressions
- Use `runtime.Pinner` (Go 1.21+) to pin the mmap region and use a proper pointer
- Accept the `uintptr` pattern but add `//go:nosplit` + `runtime.KeepAlive` guards

This is a larger change affecting `GuestMemory`, `CPU`, and all interpreter fast paths. Only pursue if Step 1 alone doesn't fix the crash.

## Critical Files
- `jit_test.go:114-136` — `runJITWithOS` (missing Close)
- `jit.go:354-370` — `JIT.Close()`
- `guestmem.go:131-158` — `GuestMemory` struct with `base uintptr`
- `run_cached.go:122,265,518,539,677,718` — hot-path unsafe.Pointer conversions
- `jit_native.go:17-26` — global `jitCtx` reuse

## Verification
After Step 1: `make test` on the remote Linux box (Go 1.26.0). The test suite should complete without the GC panic. If it still crashes, proceed to Step 3.
