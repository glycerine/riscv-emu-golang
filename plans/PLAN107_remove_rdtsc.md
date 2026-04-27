# Plan: Remove RDTSC Timing from JIT Trampolines and Lowerer

## Context

`cpu.cycle` accumulates RDTSC deltas (TSC clock ticks), not guest instruction counts. Users expect `Cycle()` to return instructions executed, and MIPS calculations based on it are meaningless. RDTSC also adds ~10ns overhead per block dispatch (two serializing instructions). We have proper IC counting for lockstep/debug; lightweight per-block IC will come later. For now, remove RDTSC everywhere and zero the Cycles field.

## Changes

### 1. `internal/jitcall/call_amd64.s` — Remove RDTSC from both `Call` and `CallAOT`

**Call function (lines 68-72, 78-82):** Remove the RDTSC start capture before `CALL AX` and the RDTSC end/delta after. Set `ret_Cycles` to 0 instead.

**CallAOT function (lines 154-158, 164-168):** Same — remove start/end RDTSC, zero the Cycles return slot.

The `24(SP)` stack slot (TSC start stash) becomes unused and can be left as padding or reclaimed.

### 2. `abjit/trampoline_amd64.s` — Remove RDTSC start capture

**Lines 24-29:** Remove the RDTSC + SHL + OR + store to `48(SP)`. The `SP+48` slot becomes unused.

### 3. `lower_amd64_abjit.go` — Remove RDTSC from exit thunk

**Lines 210-220 (`emitExitThunk`):** Remove the 6 instructions that read RDTSC, load the start value from `SP+48`, compute the delta, and store to `State.Cycles`. Replace with a single `MOVQ $0, [BP+592]` (zero State.Cycles) or simply omit the write entirely since the field will be documented as unused.

**Line 34:** Keep `abjitCyclesOff = 592` (the State struct layout doesn't change — field is reserved for future lightweight IC).

### 4. `abjit/abjit.go` — Update Cycles field comment

**Line 31:** Change comment from `// TSC cycles (RDTSC delta, written by exit thunk)` to `// Reserved for future per-block instruction count (not currently populated)`.

### 5. `internal/jitcall/call.go` — Update Cycles field comment

**Line 17:** Change from `// TSC cycles spent in native code (RDTSC delta)` to `// Reserved for future instruction count (currently zero)`.

### 6. `jit.go` — Stop accumulating RDTSC into cpu.cycle

**Line 593 (StepBlock):** `cpu.cycle += res.Cycles` → `cpu.cycle += uint64(blk.numInsns)` (static instruction count per block — coarse but correct direction).

**Line 785 (RunJIT):** Same change.

This makes `cpu.Cycle()` return an approximate instruction count rather than TSC ticks. It's imprecise (doesn't account for early block exits) but directionally correct and unblocks future per-block IC.

### 7. `jit_abjit.go` — Keep Cycles copy (now always 0)

**Line 50:** `Cycles: s.Cycles` — leave as-is, it'll just copy 0.

### 8. `jit_sandbox_cgo.go` — Keep Cycles copy (now always 0)

**Line 39:** `Cycles: uint64(r.cycles)` — leave as-is for the C sandbox path.

### 9. `abjit_overview.md` — Update documentation

**Line 329:** Change `RDTSC delta` to `Reserved (future IC)`.
**Line 344:** Remove `RDTSC` from trampoline frame description.

### 10. `abjit/abjit_test.go` — Keep offset test

**Line 284:** The `{"Cycles", ..., 592}` offset test stays — the field still exists at offset 592, just unused.

## Files changed

| File | Change |
|------|--------|
| `internal/jitcall/call_amd64.s` | Remove 4 RDTSC sequences (2 per function), zero Cycles return |
| `abjit/trampoline_amd64.s` | Remove RDTSC start capture (5 instructions) |
| `lower_amd64_abjit.go` | Remove exit thunk RDTSC delta (6 instructions) |
| `abjit/abjit.go` | Update Cycles comment |
| `internal/jitcall/call.go` | Update Cycles comment |
| `jit.go` | Replace `res.Cycles` with `blk.numInsns` (lines 593, 785) |
| `abjit_overview.md` | Update State layout and frame docs |

## Verification

```bash
cd ~/ris && go test -v -run 'TestLazyJIT_JALR|TestABJIT_NoJIT|TestRISCVTests_UI_JIT_Lazy' -count=1 .
cd ~/ris && go test -count=1 ./...
cd ~/ris && make quad  # benchmarks should be slightly faster (no RDTSC overhead)
```
