# Plan: Replace Block-Level with Function-Level Compilation

## Context

GoCPU splits code at ECALL terminators and backward branch targets, producing 3 blocks for the hello program (0x1000, 0x101c, 0x1022). libriscv compiles the entire function as one unit with internal labels and inline syscalls. We want to replace GoCPU's block-level compilation entirely with function-level compilation, for both the AOT path and the lazy JIT path.

## Scope

- **Replace** block-level compilation -- not opt-in, not coexisting
- **Both paths**: AOT (`jitCompileAOTSegment`) and lazy JIT (`emitBlock` in `RunJIT`)
- Modify `IRSyscall` directly to become non-terminal (no new `IRSyscallInline` op)
- Replace `emitBlockRange`/`emitBlockLinear`/`emitBlock` with function-level equivalents
- Replace `enumerateBlockRanges` with function boundary detection

---

## Phase 1: IR Layer -- Make `IRSyscall` Non-Terminal

### 1.1 Change `IRSyscall` semantics
**File:** `ir/ir.go`

`IRSyscall` (line 223) currently says "Terminator. WriteBackAll must precede." Change the comment: it is now a **mid-block call** that can be followed by more IR instructions. The lowerer handles both the success (continue) and failure (exit) paths.

Remove `IRSyscall` from `lastIRWasTerminator()` in `jit_emit_ir.go:360`.

### 1.2 Add `ReloadAllGuest()` and `ClearAllDirty()`
**File:** `ir/highlevel.go`

```go
func (e *Emitter) ClearAllDirty() {
    for i := range e.dirty { e.dirty[i] = false }
}

func (e *Emitter) ReloadAllGuest() {
    for i := uint32(1); i < 32; i++ {
        vr := VReg(i)
        e.Load(vr, e.xBase, int64(i)*8, I64, false)
        e.MarkDirty(vr)
    }
}
```

Reload all 31 integer regs unconditionally. Syscall can modify any register. Matches libriscv's `LOAD_REGS`. Cost is negligible vs syscall overhead.

### 1.3 Update `Syscall()` on Emitter
**File:** `ir/emit.go`

Remove the comment at line 258 saying "The emitter always terminates the IR block at Syscall." The method still emits `IRSyscall` with the same fields. The caller is now responsible for emitting `WriteBackAll` before, `ClearAllDirty` + `ReloadAllGuest` after, and **not** setting `e.terminated`.

---

## Phase 2: Lowerer -- Non-Terminal `lowerSyscall`

### 2.1 Rewrite `lowerSyscall(ins *IRInstr)`
**File:** `ir/lower_amd64.go` (line 2008)

Replace the current terminator implementation. New sequence:

```
// 1. Save live caller-saved regs (same pattern as lowerCall, line 1953)
liveInt, liveFP := lc.liveCallerSaved()
<SUB RSP, saveSize>
<store liveInt/liveFP to stack>

// 2. SysV args: RDI=xBase(R12), RSI=memBase(R14), RDX=memMask(R15)
MOVQ R12, RDI
MOVQ R14, RSI
MOVQ R15, RDX

// 3. CALL dispatcher
MOVABS R10, <addr>; CALL R10

// 4. Check result
TESTQ RAX, RAX
JNE   L_fallback

// 5. Hot path: restore saved regs, fall through to next IR instruction
<load liveInt/liveFP from stack>
<ADD RSP, saveSize>
JMP L_continue

// L_fallback: syscall not handled, must exit to Go
<load liveInt/liveFP from stack>
<ADD RSP, saveSize>
<write sret: pc=resumePC(Imm), ic=RBP, status=RAX, faultAddr=0>
<deallocate spill frame>
RET

L_continue:
// next IR instruction executes here
```

Key difference: the hot path (RAX==0) **does not exit** -- it restores regs and continues. Only the cold path (RAX!=0, unhandled syscall) exits via RET.

### 2.2 Remove the old `InlineSyscall` chain-exit logic
The old code (line 2042-2067) emitted a chain exit on the hot path to jump to the post-ECALL block. That concept no longer exists -- the post-ECALL code is in the same function. Remove the `InlineSyscall` flag, the chain-exit-on-success path, and the `ir.InlineSyscall` variable.

### 2.3 Update V2 lowerer
**File:** `ir/lower_amd64_v2.go` (line 540-544)

The V2 lowerer currently falls back to a terminator `IRRet` for `IRSyscall`. Update it to the same non-terminal pattern (or mark V2 as not supporting function-level and keep it for testing only).

---

## Phase 3: Emitter -- Replace `emitBlockRange` with `emitFunctionRange`

### 3.1 Replace `emitBlockRange()`
**File:** `jit_emit_ir.go` (line 890)

Rename to `emitFunctionRange()`. Changes:

**(a) ECALL does not set `e.terminated`.** Replace lines 1098-1104:
```go
case 0x00000073: // ECALL
    e.advancePC(4)
    addr := currentSyscallDispatcherAddr()
    if addr == 0 {
        e.emitReturn(e.pc, jitEcall)
        e.terminated = true
    } else {
        // Inline syscall: writeback, call, reload, continue
        e.irEm.WriteBackAll()
        e.irEm.ClearAllDirty()
        e.irEm.Syscall(e.pc, addr)
        e.irEm.ReloadAllGuest()
        // Do NOT set e.terminated -- emission continues
    }
```

**(b) Pre-create labels** for all intra-function branch targets and ECALL continuation PCs (pc+4). Add a pre-scan before the main emission loop that calls a new `collectInternalTargets(mem, startPC, endPC)` and creates IR labels for each.

**(c) Raise limits.** Increase `maxBlockInsns` and `maxBlockIRInsns` (or replace them with function-level constants). Functions are larger than blocks.

### 3.2 Replace `emitBlock()` and `emitBlockLinear()`
**File:** `jit_emit_ir.go`

- `emitBlockLinear(mem, startPC, endPC)` → calls `emitFunctionRange(mem, startPC, endPC)`
- `emitBlock(mem, pc)` → determines function boundaries around `pc`, then calls `emitFunctionRange`. For the lazy path, use a heuristic to find function extent (scan forward for RET/end-of-region).

### 3.3 Replace `scanUsedRegs()`
**File:** `jit_emit_ir.go` (line 709)

Current version stops at SYSTEM/JALR/JAL-with-link. The function-level version must scan the **entire** function range without stopping at ECALL, so the prologue loads all registers the function ever references.

### 3.4 Update `finalize()`
**File:** `jit_emit_ir.go` (line 657)

The `lastIRWasTerminator()` check (line 667) no longer considers `IRSyscall` terminal, so after the last instruction in the function, if it's an ECALL, `finalize` will correctly emit a fall-through chain exit to the post-function PC (or the function ends with an explicit RET/exit syscall as most do).

---

## Phase 4: AOT Driver -- Replace Block Enumeration with Function Enumeration

### 4.1 Replace `enumerateBlockRanges` with `enumerateFunctionRanges`
**File:** `aot.go`

Replace the existing `collectBranchTargets` + `enumerateBlockRanges` with function-level enumeration:

1. **With ELF symbols**: Parse STT_FUNC symbols from the symbol table (existing `elf64Sym` parsing in `elf.go`). Each symbol gives a `{startPC, startPC+size}` range. Cover gaps between symbols with synthetic ranges.
2. **Without symbols (fallback)**: Treat the entire text section as one function. This is safe — the emitter handles all control flow internally.

Also add `collectInternalTargets(mem, startPC, endPC) (branchTargets map[uint64]struct{}, ecallContinuations []uint64)` — a linear scan that returns all intra-function branch/jump targets and ECALL pc+4 addresses. Used by Phase 3.1b for pre-creating labels.

### 4.2 Update `jitCompileAOTSegment`
**File:** `jit_aot.go` (line 38)

- Pass 1 now iterates function ranges (not block ranges)
- Calls `emitFunctionRange()` instead of `emitBlockLinear()`
- One `compiledBlock` per function (fewer, larger blocks)
- decoder_cache: register the function's `startPC` → `chainEntry`. For mid-function entry (e.g., external JALR targeting a post-ECALL continuation), the function prologue always re-enters from the top, which is correct because `cpu.pc` is already set by the dispatch loop and the function loads all registers from x[].

### 4.3 Update `nextExecuteSegment`
**File:** `jit_segment.go` (line 39)

Replace `enumerateBlockRanges` call with `enumerateFunctionRanges`.

### 4.4 Update lazy JIT path in `RunJIT`
**File:** `jit.go` (line 856)

`emitBlock(&cpu.mem, pc)` already delegates to the new function-level emission (Phase 3.2). The lazy path now compiles an entire function and inserts it. On subsequent dispatch, any PC within that function hits the compiled block.

To make the lazy path aware of function extent: `emitBlock(mem, pc)` needs to find the function boundaries containing `pc`. Strategy:
- Check `mem.FindExecRegion(pc)` for the bounding region
- Within that region, use heuristic function detection: scan backward from `pc` for a likely function prologue or the region start; scan forward for RET or region end
- Fallback: use the exec region bounds as the function range

### 4.5 Remove dead code
- Delete `enumerateBlockRanges()`, `collectBranchTargets()` from `aot.go`
- Delete `scanRegion()` (BFS) from `jit_decode.go`
- Remove `flowTerm` block-splitting logic from `classifyFlow` (keep `classifyFlow` itself for other uses but remove the block-boundary semantics)
- Remove `InlineSyscall` flag and `SetInlineEcallEnabled` / `InlineEcallEnabled` from `jit_syscall.go`
- Remove `ir.InlineSyscall` from `ir/ir.go`

---

## Phase 5: VizJit and Debugging

**File:** `jit_vizjit.go`

One dump file per function. Index file shows function ranges. The hello program now produces 1 `.asm` file matching libriscv's output.

---

## Phase 6: Test Updates

### 6.1 Update tests that depend on block-level semantics
- `TestInlineEcall_HelloEndToEnd` — still valid, but the inline ecall flag is gone. The test just runs hello and checks output.
- `jit_decode_test.go:34-53` — tests for `InlineEcallEnabled` flag: remove.
- `aot_test.go` — tests that check block counts need updating (fewer, larger blocks).

### 6.2 Core validation
- `go test -v .` — all existing CPU/memory/ELF tests
- `go test -v ./bench/` — benchmark guest runs correctly
- `cd ~/ris && make hello-lib` — compare assembly output (should now be 1 file)
- `make bench-quick` — performance comparison

---

## Key Design Decisions

1. **Modify `IRSyscall` directly** — no new op, no backward compat layer. Clean replacement.
2. **Reload all 31 integer regs after ECALL** — simple, correct, matches libriscv.
3. **lowerSyscall saves/restores caller-saved regs** around the CALL, exactly like `lowerCall` does. The hot path continues; only the cold path (unhandled syscall) exits.
4. **Lazy JIT uses heuristic function detection** — find exec region, use it as function bounds. No symbol table needed for the lazy path.
5. **decoder_cache entries at function startPC only** — mid-function external entry re-enters at function top (correct because prologue reloads all regs from x[]).

## Critical Files (in implementation order)
1. `ir/ir.go` — update `IRSyscall` semantics, remove `InlineSyscall`
2. `ir/highlevel.go` — add `ReloadAllGuest()`, `ClearAllDirty()`
3. `ir/emit.go` — update `Syscall()` doc
4. `ir/lower_amd64.go` — rewrite `lowerSyscall()` as non-terminal
5. `jit_emit_ir.go` — replace `emitBlockRange` with `emitFunctionRange`, update ECALL handling, update `scanUsedRegs`, update `finalize`/`lastIRWasTerminator`
6. `aot.go` — replace `enumerateBlockRanges` with `enumerateFunctionRanges`, add `collectInternalTargets`
7. `jit_aot.go` — update `jitCompileAOTSegment` for function ranges
8. `jit_segment.go` — update `nextExecuteSegment`
9. `jit.go` — update lazy path in `RunJIT`
10. `jit_syscall.go` — remove `InlineEcallEnabled` flag
11. `jit_decode.go` — remove `scanRegion` (BFS), clean up `classifyFlow` comments
12. `jit_vizjit.go` — update dump format

## Verification
1. `go test -v .` — all tests pass
2. `cd ~/ris && make hello-lib` — GoCPU produces 1 assembly file matching libriscv
3. `make bench-quick` — performance at least matches current
4. `go test -v ./bench/` — benchmark guests run correctly
5. `go test -v ./fuzzoracle/` — oracle tests still pass (if libriscv is set up)
