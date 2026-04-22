# Fix: Boundary ECALL Hot Path Emits Chain Exit → Infinite Native Loop

## Context

When an ECALL (e.g., `write()`) is the **last instruction** in a function-level block—so its continuation PC equals `endPC`—the ECALL hot path (syscall handled natively, `RAX==0`) falls through past `ReloadSyscallRegs` into an `IRChainExit` that `finalize()` emits as the block's fall-through return. In AOT compilation, this chain exit can be pre-patched to a target block that jumps back (e.g., `_start` → JALR → main), creating an **unbreakable native infinite loop** with no budget check.

This bug is separate from Plan 060 (which fixed sret[120] writes and dispatch table completeness). Plan 060's changes are already committed.

### Concrete trigger (bench_guest.elf)

1. `main` block (0x1000–0x10FC): ECALL at 0x10F8, continuation = 0x10FC = endPC
2. `_start` block (0x10FC–0x110C): calls main via JALR
3. `emitSyscall(0x10FC, dispatcher)` emits: WriteBackAll → ClearDirtySyscallRegs → **IRSyscall** → ReloadSyscallRegs (IRLoad x10, IRLoad x11)
4. Loop exits (`e.pc == e.regionEnd`). `finalize()` runs.
5. `lastIRWasTerminator()` sees the last IR is **IRLoad** (not a terminator) → returns `false`
6. `finalize()` emits `emitChainableReturn(0x10FC)` → **IRChainExit(0x10FC)**
7. AOT pre-patches this chain exit to `_start`'s chainEntry
8. Hot path: write succeeds → reload → chain_exit → _start → JALR main → main body → write → chain_exit → **infinite native loop**

The boundary ECALL label (placed later in `finalize()`) emits a correct terminal `IRRet`, but it's only reachable via the dispatch table—not by the hot-path fall-through.

## Fix

### Step 1: Terminal return for boundary ECALL fall-through

**File**: `jit_emit_ir.go`, function `finalize()`, lines 668–670

Change the fall-through logic to emit a terminal return (not a chain exit) when the block ends with a boundary ECALL:

```go
// BEFORE (bug):
if !e.lastIRWasTerminator() {
    e.emitChainableReturn(e.pc)
}

// AFTER (fix):
if !e.lastIRWasTerminator() {
    if len(e.boundaryEcallConts) > 0 {
        e.emitReturn(e.pc, jitOK)
    } else {
        e.emitChainableReturn(e.pc)
    }
}
```

**Why this is correct**: `boundaryEcallConts` is only populated when the last RISCV instruction emitted was an ECALL whose continuation equals `endPC`. In that case, the hot-path fall-through (past `ReloadSyscallRegs`) must be a terminal return to Go—not a chainable exit that could be pre-patched back to the same block or a caller that re-enters it.

**Why not fix `lastIRWasTerminator()` instead**: `IRSyscall` is non-terminal—its hot path intentionally continues to the next IR instruction. Adding it to the terminator list would suppress the fall-through entirely, skipping `ReloadSyscallRegs` and the terminal return. The issue isn't that a terminator is missing; it's that the fall-through at the boundary needs to be a terminal return, not a chain exit.

### Step 2: Unit test — IR emission correctness

**File**: `jit_emit_ir_test.go`, new function `TestEmit_BoundaryEcall_TerminalReturn`

This test constructs a block where ECALL is the last instruction (continuation = endPC) and verifies the IR:

```go
func TestEmit_BoundaryEcall_TerminalReturn(t *testing.T) {
    if !DirectSyscallEnabled() {
        t.Skip("direct syscall not enabled — boundary ECALL takes legacy path")
    }

    mem, err := NewGuestMemory(Size64MB)
    if err != nil { t.Fatal(err) }
    defer mem.Free()

    // Function: 5 instructions, ECALL is last.
    // addi x17, x0, 64  (a7 = SYS_write)
    // addi x10, x0, 1   (a0 = stdout)
    // lui  x11, 2        (a1 = 0x2000)
    // addi x12, x0, 5   (a2 = 5)
    // ecall
    mem.Store32(0x1000, 0x04000893)
    mem.Store32(0x1004, 0x00100513)
    mem.Store32(0x1008, 0x000025B7)
    mem.Store32(0x100C, 0x00500613)
    mem.Store32(0x1010, 0x00000073) // ECALL

    res := emitBlockLinear(mem, 0x1000, 0x1014)
    if res == nil {
        t.Fatal("emitBlockLinear returned nil")
    }

    endPC := uint64(0x1014)

    // The fall-through after ECALL hot path must be IRRet, not IRChainExit.
    for _, ins := range res.block.Instrs {
        if ins.Op == ir.IRChainExit && uint64(ins.Imm) == endPC {
            t.Fatalf("found IRChainExit targeting endPC 0x%x — "+
                "boundary ECALL hot path would chain instead of terminal-returning", endPC)
        }
    }

    foundRet := false
    for _, ins := range res.block.Instrs {
        if ins.Op == ir.IRRet && uint64(ins.Imm) == endPC && ins.Imm2 == 0 {
            foundRet = true
            break
        }
    }
    if !foundRet {
        t.Fatalf("no IRRet(pc=0x%x, status=jitOK) found — "+
            "boundary ECALL hot path has no terminal return", endPC)
    }
}
```

**What the test verifies**:
- No `IRChainExit` targets `endPC` (would be the pre-patchable infinite loop)
- An `IRRet(endPC, jitOK)` exists (the correct terminal return for the hot-path fall-through)
- Requires `DirectSyscallEnabled()` because the bug only manifests with the inline ECALL path (when disabled, `emitSyscall` takes the legacy `emitReturn+terminated` path which is already correct)

## Critical Files

- `jit_emit_ir.go` — `finalize()` line 668 (the one-line fix)
- `jit_emit_ir_test.go` — new `TestEmit_BoundaryEcall_TerminalReturn`

## Verification

1. `cd ~/ris && go test -v -run TestEmit_BoundaryEcall_TerminalReturn .` — new test passes
2. `go test -v -run 'TestScanUsedRegs|TestEmit_|TestJITCompile' .` — existing emit tests pass
3. `go test -v -run 'TestAOT_' .` — AOT tests pass
4. `go test -v -run 'TestChaining_' .` — chain patching tests pass
5. `go test -v -run 'TestRISCVTests_UI_JIT' .` — all 55 RISC-V tests pass
6. `go test -v -run TestHelloGoCPU .` — hello interpreter test passes
7. `go test -v -run TestHelloGoCPU_JIT_DirectSyscall .` — hello JIT test passes
8. `go test ./ir/` — ir package tests pass

**Note**: `TestJIT_BenchGuest_Smoke` may still hang due to OTHER remaining bugs (AOT segment infrastructure issues identified in the prior investigation). This fix addresses the boundary-ECALL hot-path chain-exit specifically. The remaining AOT hang is a separate issue to investigate after this fix lands.
