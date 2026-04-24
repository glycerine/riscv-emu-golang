# Plan: Replace Block-Level with Function-Level Compilation

## Context

GoCPU splits code at ECALL terminators and backward branch targets, producing 3 blocks for the hello program. libriscv compiles the entire function as one unit with internal labels and inline `api.system_call()`. After the syscall, libriscv's `LOAD_SYS_REGS` reloads **only registers 10-11 (a0, a1)** — the syscall return values (see `tr_emit.cpp:2218-2224`). Callee-saved registers (s0-s11, sp, gp, tp) are never reloaded — ECALL preserves them.

## Scope
- **Replace** block-level compilation entirely (AOT + lazy JIT)
- **Incremental**: each step has tests written first, then implementation
- **Efficient syscall reload**: only a0/a1 (x10/x11), not all 31 registers

---

## Step 1: IR Layer — `ReloadSyscallRegs` + `ClearDirtySyscallRegs`

### 1a: Write tests
**File:** `ir/highlevel_test.go`

Test that `ReloadSyscallRegs()` emits exactly 2 Load instructions (for x10, x11) and marks them dirty. Test that `ClearDirtySyscallRegs()` clears dirty flags for only x10, x11.

### 1b: Implement
**File:** `ir/highlevel.go`

```go
func (e *Emitter) ClearDirtySyscallRegs() {
    for _, vr := range []VReg{10, 11} {
        if int(vr) < len(e.dirty) { e.dirty[vr] = false }
    }
}

func (e *Emitter) ReloadSyscallRegs() {
    for _, vr := range []VReg{10, 11} {
        e.Load(vr, e.xBase, int64(vr)*8, I64, false)
        e.MarkDirty(vr)
    }
}
```

Only a0 (x10) and a1 (x11) — matching libriscv's `LOAD_SYS_REGS` which loops `reg = 10..11`. Callee-saved registers (sp, s0-s11, gp, tp) are preserved by ECALL per RISC-V ABI. The GoCPU dispatcher only writes x[10] (return value).

### 1c: Verify
`go test -run TestReloadSyscallRegs ./ir/`

---

## Step 2: Make `IRSyscall` Non-Terminal in IR

### 2a: Write tests
**File:** `ir/ir_test.go`

Test that `IRSyscall` is NOT in `lastIRWasTerminator()`'s terminator list. Test that an IR block with `[IRConst, IRSyscall, IRConst, IRRet]` is valid (4 instructions, syscall in the middle).

### 2b: Implement
**Files:** `ir/ir.go`, `jit_emit_ir.go`

- Update `IRSyscall` doc comment (line 223): remove "Terminator."
- In `jit_emit_ir.go:360`, remove `IRSyscall` from `lastIRWasTerminator()`.
- Update `ir/emit.go:256-258`: remove "The emitter always terminates the IR block at Syscall" doc.

### 2c: Verify
`go test -run TestIRSyscallNotTerminator ./ir/`

---

## Step 3: Rewrite `lowerSyscall` as Non-Terminal

### 3a: Write tests
**File:** `ir/lower_amd64_test.go`

Test: construct a Block with `[IRLoad(x10), IRWriteback, IRSyscall, IRConst(x10, 42), IRRet]`. Lower via `LowerAMD64`. Verify no error, and that the assembled bytes are non-empty. (Proves the lowerer handles mid-block IRSyscall without crashing.)

### 3b: Implement
**File:** `ir/lower_amd64.go` (replace `lowerSyscall` at line 2008)

New sequence — save/restore caller-saved regs around the CALL (like `lowerCall` does), hot path falls through:

```
// Save live caller-saved host registers
<push liveInt/liveFP to stack>

// SysV args: RDI=xBase(R12), RSI=memBase(R14), RDX=memMask(R15)
MOVQ R12, RDI; MOVQ R14, RSI; MOVQ R15, RDX

// CALL dispatcher
MOVABS R10, <addr>; CALL R10

// Check result
TESTQ RAX, RAX
JNE   L_fallback

// Hot path: restore, continue to next IR instruction
<pop liveInt/liveFP>
JMP L_continue

L_fallback:
  <pop liveInt/liveFP>
  <write sret: pc=resumePC, ic=RBP, status=RAX, faultAddr=0>
  <dealloc frame; RET>

L_continue:
  // next IR instruction
```

### 3c: Remove old `InlineSyscall` chain-exit logic
Remove lines 2042-2067 (the chain-exit hot path). Remove `ir.InlineSyscall` variable from `ir/ir.go`. Remove `inlineEcallEnabled`/`SetInlineEcallEnabled`/`InlineEcallEnabled` from `jit_syscall.go`.

### 3d: Verify
`go test -run TestLowerSyscallNonTerminal ./ir/`

---

## Step 4: Update Emitter — ECALL No Longer Terminates

### 4a: Write tests
**File:** `jit_emit_ir_test.go`

Test: emit a small function-like range containing `li a0,1; ecall; addiw a3,a3,-1; bnez a3,-8`. Verify `emitBlockRange` produces a single `emitResult` spanning the full range (not terminated at ECALL). Verify the IR contains an `IRSyscall` followed by 2 Load instructions (reload a0, a1) and then the `addiw` lowering.

### 4b: Implement
**File:** `jit_emit_ir.go`

Change ECALL handling at line 1098-1104:
```go
case 0x00000073: // ECALL
    e.advancePC(4)
    addr := currentSyscallDispatcherAddr()
    if addr == 0 {
        e.emitReturn(e.pc, jitEcall)
        e.terminated = true
    } else {
        e.irEm.WriteBackAll()
        e.irEm.ClearDirtySyscallRegs()
        e.irEm.Syscall(e.pc, addr)
        e.irEm.ReloadSyscallRegs()
        // NOT terminated — emission continues
    }
```

### 4c: Update `scanUsedRegs` (line 709)
Remove the early stop at SYSTEM opcode. The function-level scanner must scan the **entire** range so the prologue loads all registers used anywhere in the function.

### 4d: Verify
`go test -run TestEmitEcallNonTerminal .`

---

## Step 5: Replace Block Enumeration with Function Enumeration

### 5a: Write tests
**File:** `aot_test.go`

Test: for the hello ELF (0x1000..0x1030), `enumerateFunctionRanges` returns **1 range** covering the full text section — not 3 blocks. Also test `collectInternalTargets` returns the expected branch targets and ECALL continuation PCs.

### 5b: Implement
**File:** `aot.go`

Replace `enumerateBlockRanges` with `enumerateFunctionRanges(mem, textBase, textSize, elfData)`:
1. With ELF symbols: parse STT_FUNC symbols, return one range per function.
2. Without symbols: return the entire text section as one range.

Add `collectInternalTargets(mem, startPC, endPC)` — linear scan returning `(branchTargets map[uint64]struct{}, ecallContinuations []uint64)`. Used by the emitter to pre-create labels.

Replace `collectBranchTargets` and `enumerateBlockRanges` — delete them.

### 5c: Verify
`go test -run TestEnumerateFunctionRanges .`

---

## Step 6: Update AOT Driver

### 6a: Write tests
**File:** `aot_test.go`

Test: compile the hello ELF through `jitCompileAOTSegment` with function ranges. Verify it produces 1 compiled block (not 3). Verify the decoder_cache has an entry at 0x1000.

### 6b: Implement
**File:** `jit_aot.go`

- Pass 1: iterate function ranges, call `emitBlockLinear` (which now internally produces function-level IR since ECALL is non-terminal)
- The rest of the pipeline (regalloc, lower, assemble, mmap, chain exits, decoder_cache) is unchanged structurally — just fewer, larger blocks.

**File:** `jit_segment.go` (line 39)
- Replace `enumerateBlockRanges` call with `enumerateFunctionRanges`.

### 6c: Verify
`go test -run TestAOT_FunctionLevel .`

---

## Step 7: Update Lazy JIT Path

### 7a: Write tests
**File:** `jit_emit_ir_test.go`

Test: `emitBlock(mem, 0x1000)` for the hello program produces one `emitResult` spanning the full function, not just until the first ECALL.

### 7b: Implement
**File:** `jit_emit_ir.go`

Update `emitBlock(mem, pc)`: instead of `scanRegion` BFS (which stops at terminators), find the function extent. Strategy:
- Check for exec region bounds around `pc`
- Within the region, scan forward for the end of the function (heuristic: next function symbol, or end of region)
- Call `emitFunctionRange(mem, startPC, endPC)` (which is just `emitBlockRange` now that ECALL is non-terminal)

### 7c: Remove `scanRegion` BFS
Delete `scanRegion()` from `jit_decode.go`. It is no longer used.

### 7d: Verify
`go test -run TestLazyJIT_FunctionLevel .`

---

## Step 8: End-to-End Validation + Cleanup

### 8a: Run full test suite
```
go test -v .
go test -v ./bench/ -run Test
cd ~/ris && make hello-lib
```

Verify: GoCPU now produces **1 assembly file** in `debug_vizjit_dir`, matching libriscv's 1 file.

### 8b: Cleanup dead code
- Delete `scanRegion` from `jit_decode.go`
- Delete `collectBranchTargets`, `enumerateBlockRanges` from `aot.go`
- Remove `InlineSyscall`/`inlineEcallEnabled` infrastructure
- Update VizJit index format (fewer entries per segment)
- Remove `flowTerm` handling for ECALL in `classifyFlow` if no longer needed (keep `classifyFlow` itself for other uses)

### 8c: Performance check
`make bench-quick` — verify no regression.

---

## Key Design Decisions

1. **Reload only a0/a1 (x10/x11) after ECALL** — matches libriscv's `LOAD_SYS_REGS` (`tr_emit.cpp:2219`, loops reg 10..11). The dispatcher only writes x[10] (return value). Callee-saved registers (s0-s11, sp, gp, tp) are preserved by ECALL per RISC-V ABI.
2. **Modify `IRSyscall` directly** — no new op, replace semantics from "terminator" to "mid-block call."
3. **Incremental with test-first** — each step writes tests before implementation.
4. **Replace entirely** — no backward compat flag, no coexistence.

## Critical Files (in step order)
1. `ir/highlevel.go` + `ir/highlevel_test.go` — `ReloadSyscallRegs`, `ClearDirtySyscallRegs`
2. `ir/ir.go` + `jit_emit_ir.go` — `IRSyscall` non-terminal semantics
3. `ir/lower_amd64.go` — rewrite `lowerSyscall` as non-terminal
4. `jit_emit_ir.go` — ECALL handling, `scanUsedRegs`
5. `aot.go` — `enumerateFunctionRanges`, `collectInternalTargets`
6. `jit_aot.go` + `jit_segment.go` — function-level AOT driver
7. `jit.go` + `jit_decode.go` — lazy JIT path, remove `scanRegion`
8. `jit_syscall.go` — remove `InlineEcallEnabled` infrastructure
