# Code Review: abjit Implementation

## Context

The abjit ("Aaron Balke JIT trampoline") is a new JIT execution path that achieves 2.8ns trampoline overhead (12x faster than CGO) by running JIT code on the same goroutine with GC-safe Go callbacks. It's implemented across ~2,000 lines in 6 packages. The implementation spans phases 0-3: assembly trampoline, CodeBuilder, State struct, pluggable RegPolicy, and a full IR lowerer.

Overall quality is high: clean separation of concerns, comprehensive test coverage (14 unit + 6 IR + 5 integration tests + 3 benchmarks), well-documented design decisions, and verified GC safety. The decoder cache lookup in `abjitJalrIC` using `((target - vaddrBegin) << 2) & dcMask` is correct for `uintptr` entries at half-word granularity.

---

## Bugs Found

### BUG 1: Stack misalignment before CALL instructions (Medium-High)

**Files:** `ir/lower_amd64_abjit.go:92`, `ir/lower_amd64_rv8.go:80` (shared issue)

**Problem:** When `stackSlots` is odd, RSP is not 16-byte aligned before CALL instructions in `abjitSyscall` (line 429) and `abjitCall` (line 610). This violates the SysV AMD64 ABI.

**Analysis:**
- After Go prologue (PUSH RBP + SUB RSP, 65528): RSP mod 16 = 8
- Lowerer frame: `frameSize = stackSlots*8 + 24`
- Even stackSlots: frameSize mod 16 = 8 → RSP mod 16 = 0 (aligned)
- Odd stackSlots: frameSize mod 16 = 0 → RSP mod 16 = 8 (MISALIGNED)

**Impact:** Currently latent for abjit (no CTab entries wired up yet), but will crash or corrupt when inline syscall dispatch is enabled. The rv8 lowerer (lines 493, 551) has the same latent issue.

**Fix in `ir/lower_amd64_abjit.go:92` and `ir/lower_amd64_rv8.go:80`:**
```go
lc.frameSize = lc.sretOffset + 24
if lc.frameSize%16 == 0 {
    lc.frameSize += 8  // ensure RSP is 16-byte aligned after SUB
}
```

### BUG 2: Double misalignment in `abjitCall` (Medium)

**File:** `ir/lower_amd64_abjit.go:595-598`

**Problem:** The dynamic caller-save area doesn't maintain alignment:
```go
saveSize := int64(len(liveInt)+len(liveFP)) * 8
if saveSize > 0 {
    lc.emitRI(x86.ASUBQ, saveSize, goasm.REG_AMD64_SP)
}
```
If `len(liveInt)+len(liveFP)` is odd, this shifts RSP by 8 mod 16, undoing alignment.

**Fix:** Round saveSize up to 16-byte boundary:
```go
saveSize := int64(len(liveInt)+len(liveFP)) * 8
if saveSize%16 != 0 {
    saveSize += 8
}
```

### BUG 3: `abjitCall` silently ignores invalid CTab index (Low)

**File:** `ir/lower_amd64_abjit.go:572-573`

**Problem:** Returns nil instead of an error when CTab index is out of range. Could mask IR emitter bugs.
```go
if int(ins.Imm) >= len(lc.blk.CTab) {
    return nil  // should this be an error?
}
```

**Fix:** Return an error:
```go
if int(ins.Imm) >= len(lc.blk.CTab) {
    return fmt.Errorf("ir.abjitCall: CTab index %d out of range (len=%d)", ins.Imm, len(lc.blk.CTab))
}
```

---

## Style / Minor Issues

### STYLE 1: Stale comment on PolicyABJIT

**File:** `ir/lower_amd64.go:111`

Current: `// Lower is nil until lower_amd64_abjit.go exists.`  
Lower is now set. Remove the stale clause.

### STYLE 2: No bounds checking in `CodeBuilder.emit()`

**File:** `abjit/emit.go:52-55`

`emit()` does `copy(c.buf[c.off:], b)` without checking overflow. Only affects unit tests (production uses goasm), but a panic guard would be safer. Low priority.

### STYLE 3: Benchmark ignores errors

**File:** `jit_abjit_test.go:159`

```go
mem, _ := NewGuestMemory(Size4GB)  // error ignored
entry, _ := LoadELFBytes(mem, data)  // error ignored
```

Should check errors or at minimum `b.Fatal` on failure.

### STYLE 4: Redundant RBP save in trampoline

**File:** `abjit/trampoline_amd64.s:18`

`MOVQ BP, 16(SP)` saves a value that is never restored (POP RBP restores from the PUSH'd location instead). Harmless — 5 bytes, one-time cost — but the comment "not explicitly restored" could be expanded to explain WHY it's safe (POP restores the caller's RBP from the PUSH location).

---

## Design Invariants Worth Documenting

### `storeRegsBack` only writes `AllocReg` registers

**File:** `ir/lower_amd64_ops.go:403-421`

If a modified guest register is spilled to the lowerer's stack (`AllocStack`), `storeRegsBack` does NOT write it back to the register file at `[RBP + vr*8]`. This is correct IF the register allocator guarantees that all modified guest architectural registers (x1-x31, f0-f31) are in host registers at exit points — or IRWriteback instructions handle spilled ones. This invariant should be documented with a comment at `storeRegsBack`.

### FaultAddr field is overloaded

In `abjitJalrIC` (line 565), `FaultAddr` stores the JALR site index, not a fault address. This is by design (the Go dispatcher reads it as a site index). Worth a one-line comment at the State struct field definition noting the dual use.

---

## Fix Plan

1. **Fix BUG 1** — Add frame alignment padding in both `lower_amd64_abjit.go:92` and `lower_amd64_rv8.go:80`
2. **Fix BUG 2** — Round saveSize in `abjitCall` (line 595)
3. **Fix BUG 3** — Return error for invalid CTab index in `abjitCall` (line 572)
4. **Fix STYLE 1** — Update stale comment at `ir/lower_amd64.go:111`
5. **Fix STYLE 3** — Add error checks in benchmark at `jit_abjit_test.go:159`
6. **Document** the `storeRegsBack` invariant at `ir/lower_amd64_ops.go:403`

## Verification

```bash
go test -v -run TestABJIT ./...
go test -v -run TestLowerABJIT ./ir/
go test -v ./abjit/
go test -v -run TestJIT ./...       # verify rv8 path not regressed
go test -v -run TestRISCV ./...     # full riscv-tests suite
```
