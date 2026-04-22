# Plan: Function-Level AOT Compilation for GoCPU

## Context

The `make hello-lib` comparison shows GoCPU producing 3 assembly output files while libriscv produces 1. This is because `enumerateBlockRanges()` in `aot.go` splits at ECALL terminators and backward branch targets:

- **0x1000** -- textBase (always a block start)
- **0x101c** -- backward branch target from `c.bnez a3,-8` at 0x1024
- **0x1022** -- fallthrough after ECALL at 0x101e (termFT from `classifyFlow`)

libriscv compiles the entire function `f_1000` as one C function with internal labels (`f_1000_101c`) and inline `api.system_call()`. The goal is to do the same in GoCPU: compile entire functions as single IR units with internal labels, inline syscall calls, and one register allocation.

## Approach: New `IRSyscallInline` op + `emitFunctionRange()`

Keep existing block-level AOT working. Add function-level as opt-in (flag `functionLevelAOT`).

---

## Phase 1: IR Layer -- Non-Terminal Syscall

### 1.1 Add `IRSyscallInline` op
**File:** `ir/ir.go`
- Add `IRSyscallInline` after `IRSyscall` (line ~223). Same fields: `Imm=resumePC`, `Imm2=CTab index`.
- Update `irOpNames`, `String()`, `lastIRWasTerminator()` (it is NOT a terminator).

### 1.2 Add `ReloadAllGuest()` and `ClearAllDirty()`
**File:** `ir/highlevel.go`
- `ClearAllDirty()`: zeros the entire `e.dirty` slice.
- `ReloadAllGuest()`: emits Load for all 31 integer regs (x1..x31) from `xBase`, marks each dirty. Unconditional -- syscall can modify any register. Matches libriscv's `LOAD_REGS`.

### 1.3 Add `SyscallInline()` on Emitter
**File:** `ir/emit.go`
- Same CTab registration as `Syscall()`, but emits `IRSyscallInline`. Does NOT set terminated.

---

## Phase 2: Lowerer -- `lowerSyscallInline`

### 2.1 Add `lowerSyscallInline(ins *IRInstr)`
**File:** `ir/lower_amd64.go`

Key difference from `lowerSyscall`: it is NOT a terminator. Sequence:

```
// Save live caller-saved regs (same as lowerCall)
<push liveInt/liveFP>

// SysV args: RDI=xBase(R12), RSI=memBase(R14), RDX=memMask(R15)
MOVQ R12, RDI
MOVQ R14, RSI  
MOVQ R15, RDX

// CALL dispatcher
MOVABS R10, <addr>; CALL R10

// Check result
TESTQ RAX, RAX
JNE   L_fallback

// Hot path: restore saved regs, continue
<pop liveInt/liveFP>
JMP L_continue

L_fallback:
  <pop liveInt/liveFP>
  <write sret: pc=resumePC, status=RAX>
  <epilogue RET>

L_continue:
  // execution falls through to next IR instruction
```

### 2.2 Wire into `lowerInstr` dispatch (line ~798)
Add `case IRSyscallInline: lc.lowerSyscallInline(ins)`.

---

## Phase 3: Emitter -- Function-Level Emission

### 3.1 Add `emitFunctionRange()`
**File:** `jit_emit_ir.go`

New function alongside `emitBlockRange()`. Differences:

**(a) ECALL does not terminate.** Instead of `e.emitSyscall()` + `e.terminated = true`:
```go
e.irEm.WriteBackAll()
e.irEm.ClearAllDirty()
e.irEm.SyscallInline(e.pc, dispatcherAddr)
// Reload all 31 integer regs
for i := uint32(1); i < 32; i++ {
    e.irEm.Load(VReg(i), e.irEm.XBase(), int64(i)*8, ir.I64, false)
    e.irEm.MarkDirty(VReg(i))
}
```
Emission continues -- `e.terminated` stays false.

**(b) Backward branches stay internal.** Already handled: `emitBranch` checks `target >= e.startPC && target < e.regionEnd` and uses internal labels + `BudgetCheck`. Since `regionEnd` = function endPC, all intra-function branches are internal.

**(c) Pre-create labels** for all ECALL continuation PCs (pc+4) and all intra-function branch targets before the main loop.

### 3.2 Add `scanUsedRegsFunction()`
**File:** `jit_emit_ir.go`

Variant of `scanUsedRegs()` (line 709) that does NOT stop at ECALL/SYSTEM -- scans the entire function range so the prologue loads all registers the function will ever reference.

### 3.3 Raise limits
- `maxFunctionInsns = 8192` (vs current `maxBlockInsns`)
- `maxFunctionIRInsns = 16384` (vs current `maxBlockIRInsns`)

---

## Phase 4: AOT Driver

### 4.1 Function boundary detection
**File:** `aot.go`

Add `enumerateFunctionRanges()`: uses ELF symbol table (STT_FUNC symbols from `elf.go`) when available. Fallback: entire text section as one function.

Add `collectInternalBranchTargets(mem, startPC, endPC)`: linear pass returning intra-function branch targets and ECALL PCs (used by Phase 3.1c).

### 4.2 Function-level path in `jitCompileAOTSegment`
**File:** `jit_aot.go`

When `functionLevelAOT` is true:
- Use `enumerateFunctionRanges()` instead of `enumerateBlockRanges()`
- Call `emitFunctionRange()` instead of `emitBlockLinear()` per range
- One `compiledBlock` per function
- decoder_cache entry at function startPC only (mid-function external targets fall back to lazy block compilation)

### 4.3 Configuration
```go
var functionLevelAOT bool = false // opt-in
```

---

## Phase 5: VizJit

**File:** `jit_vizjit.go`

One dump file per function (vs per block). Index file reflects function ranges. Cosmetic change.

---

## Key Design Decisions

1. **New `IRSyscallInline` op** (not modifying existing `IRSyscall`): preserves backward compatibility.
2. **Reload all 31 integer registers after ECALL**: simple, correct, matches libriscv. Cost is negligible vs syscall overhead.
3. **Function-level is opt-in** (`functionLevelAOT = false` default): zero risk to existing code.
4. **Mid-function external entry falls back to lazy block compilation**: correct but slower for external JALR to mid-function PCs. Optimizable later.

## Critical Files
- `jit_emit_ir.go` -- `emitFunctionRange()`, `emitSyscallInlineSequence()`, `scanUsedRegsFunction()`
- `ir/lower_amd64.go` -- `lowerSyscallInline()`, wire into `lowerInstr`
- `ir/ir.go` -- `IRSyscallInline` op
- `ir/highlevel.go` -- `ReloadAllGuest()`, `ClearAllDirty()`
- `ir/emit.go` -- `SyscallInline()` helper
- `aot.go` -- `enumerateFunctionRanges()`, `collectInternalBranchTargets()`
- `jit_aot.go` -- function-level path in `jitCompileAOTSegment`

## Verification
1. `go test -run TestInlineEcall_HelloEndToEnd .` with `functionLevelAOT = true`
2. Compare `debug_vizjit_dir` output: should produce 1 file matching libriscv's 1 file
3. `cd ~/ris && make hello-lib` and diff assembly outputs
4. `make bench-quick` to compare performance
5. `go test -v ./...` to verify no regressions with `functionLevelAOT = false`
