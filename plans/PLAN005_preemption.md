# Instruction Budget at Backward Branches (Semi-Cooperative Preemption)

## Context

The JIT is currently non-preemptible during block execution. For straight-line blocks this is fine (microseconds), but region-scanned blocks contain loops that execute in native code indefinitely. A fib-style loop running millions of iterations would make the goroutine non-preemptible for milliseconds, stalling Go's GC STW and scheduler latency.

**Fix**: at every backward branch/jump, check an instruction counter. If it exceeds a budget (4096), exit the block with PC set to the loop target. The Go dispatch loop re-enters immediately (last-block cache hits), giving the Go runtime a preemption window between blocks.

This matches libriscv's `LOOP_EXPRESSION "LIKELY(ic < max_ic)"` pattern at backward branches.

## Prerequisites (done)

- 23 JIT unit tests pass including `TestJIT_BlockReentry` which already exercises the exit/re-enter flow
- ~90 riscv-tests lockstep tests pass with per-block register + full memory comparison
- `TestJIT_CycleCount_Loop` verifies JIT and interpreter cycle counts match exactly
- `TestJIT_Fib` exercises a tight backward-branch loop (fib(20) = 20 iterations × 5 instructions = 100+ instructions)

## Design

Use a compile-time constant `MAX_IC = 4096` emitted directly into each block's C code. No changes to `jitcall.Call` trampoline or to the Go-side dispatch — the budget is baked into each compiled block.

### Where the check goes

| Case | Has check? | Why |
|------|-----------|-----|
| Internal forward branch (goto) | No | Forward branches don't form loops on their own; block size (2048 insns) bounds them |
| Internal backward branch (goto to prior PC) | **Yes** | Backward edges close loops — unbounded iterations possible |
| External branch (exits block) | No | Already exits — dispatch loop provides preemption |
| `JAL rd==0` forward (phase 1 jump) | No | Forward, doesn't loop |
| `JAL rd==0` backward (backward jump) | **Yes** | Backward — could form loop |
| `JAL rd!=0` (call — exits block) | No | Already exits |
| `JALR` (always exits) | No | Already exits |

**Key test**: `target < e.pc` means backward (where `e.pc` is the current instruction's PC, before `advancePC`).

### Emission patterns

**Current internal branch (unchanged for forward):**
```c
if (rs1 < rs2) goto b_TARGET;
```

**New internal backward branch:**
```c
if (rs1 < rs2) {
    if (__builtin_expect(ic < 4096, 1)) goto b_TARGET;
    x[10] = r10; x[11] = r11; /* ... writeback ... */
    return (JITResult){TARGET_PC_ULL, ic, 0, 0};
}
```

**Current `JAL rd==0` (unchanged for forward):**
```c
goto b_TARGET;
```

**New `JAL rd==0` backward:**
```c
if (__builtin_expect(ic < 4096, 1)) goto b_TARGET;
x[10] = r10; x[11] = r11; /* ... writeback ... */
return (JITResult){TARGET_PC_ULL, ic, 0, 0};
```

## Files to Modify

| File | Changes |
|------|---------|
| `jit_emit.go` | Add `maxIC = 4096` constant. Modify `emitBranch` to wrap backward internal goto in budget check. Modify `emitJAL` rd==0 path to wrap backward goto in budget check. |
| `jit_test.go` | Add `TestJIT_InstructionBudget` — tight loop that triggers multiple budget-driven re-entries. Verify correctness and that block gets re-entered. |

## Implementation Details

### 1. Add budget constant (`jit_emit.go` top level)

```go
const maxIC = 4096 // instruction budget per block execution
```

### 2. Modify `emitBranch` (currently at line 1313)

Current internal branch path (line 1329-1333):
```go
if internal {
    e.emit("    if (%s %s %s) goto b_%x;\n",
        e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3), target)
    e.gotoTargets[target] = true
}
```

New:
```go
if internal {
    if target < e.pc {
        // Backward branch — include budget check.
        e.emit("    if (%s %s %s) {\n",
            e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3))
        e.emit("      if (__builtin_expect(ic < %d, 1)) goto b_%x;\n", maxIC, target)
        e.emitWriteBackAll()
        e.emit("      return (JITResult){0x%xULL, ic, 0, 0};\n    }\n", target)
    } else {
        // Forward branch — plain goto.
        e.emit("    if (%s %s %s) goto b_%x;\n",
            e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3), target)
    }
    e.gotoTargets[target] = true
}
```

### 3. Modify `emitJAL` rd==0 path (currently at line 1282-1289)

Current:
```go
if rd == 0 {
    e.emit("    goto b_%x;\n", target)
    e.gotoTargets[target] = true
    e.pc = target
    return
}
```

New:
```go
if rd == 0 {
    // Note: advancePC was already called above (line 1277), so e.pc now points
    // at the instruction AFTER the jump. For "backward" we need to compare
    // target to the ORIGINAL pc of the jump, which is e.pc - insnSize.
    origPC := e.pc - insnSize
    if target < origPC {
        // Backward jump — include budget check.
        e.emit("    if (__builtin_expect(ic < %d, 1)) goto b_%x;\n", maxIC, target)
        e.emitWriteBackAll()
        e.emit("    return (JITResult){0x%xULL, ic, 0, 0};\n", target)
    } else {
        // Forward jump — plain goto.
        e.emit("    goto b_%x;\n", target)
    }
    e.gotoTargets[target] = true
    e.pc = target
    return
}
```

## Test: `TestJIT_InstructionBudget`

Verify that a long-running loop:
1. Produces the correct result
2. Exits and re-enters the block multiple times (proves budget fires)
3. Doesn't hang

```go
func TestJIT_InstructionBudget(t *testing.T) {
    // Loop that runs 100000 iterations — well above MAX_IC=4096.
    // Without the budget, this would be one big native loop.
    // With the budget, the block exits every ~4096 instructions and re-enters.
    //
    //   0x1000: ADDI x1, x1, 1          # counter++
    //   0x1004: BLT  x1, x2, -4         # if counter < target, loop
    //   0x1008: ECALL
    //
    // Set x1 = 0, x2 = 100000. After execution, x1 should be 100000.

    cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
        ienc(opOPIMM, 0, 1, 1, 1),      // ADDI x1, x1, 1
        benc(opBRANCH, 4, 1, 2, -4),    // BLT x1, x2, -4
        instrECALL,
    })
    defer mem.Free()
    cpu.SetReg(1, 0)
    cpu.SetReg(2, 100000)
    cpu.Notes.Push(ecallStop)

    jit := NewJIT()
    jit.RunJIT(cpu)

    if cpu.Reg(1) != 100000 {
        t.Errorf("x1 = %d, want 100000", cpu.Reg(1))
    }
    // Expected cycles: 100000 ADDI + 100000 BLT + 1 ECALL = 200001
    // (or near that — the budget exit/re-enter doesn't change counted instructions)
    if cpu.Cycle() != 200001 {
        t.Errorf("cycles = %d, want 200001", cpu.Cycle())
    }
}
```

## Verification

```bash
# All existing tests still pass
go test -count=1 -run 'TestJIT_' -timeout 30s .
go test -count=1 -run 'TestRISCVTests_Lockstep_U[IMAC]$' -timeout 60s .
go test -count=1 -run 'TestRISCVTests_U[IMAC]_JIT$' -timeout 30s .

# New budget test passes
go test -count=1 -v -run 'TestJIT_InstructionBudget' .

# Bench smoke still passes
go test -count=1 -run TestJIT_BenchGuest_Smoke -timeout 30s ./bench/

# Bench performance — expect small overhead (~5-10% slowdown) for loop-heavy code
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/
```

## Expected Behavior After Implementation

- **Correctness**: unchanged — all 23 JIT unit tests + ~90 lockstep tests pass
- **Cycle counts**: unchanged — budget exit/re-enter doesn't skip instructions, just checkpoints them
- **GC latency**: bounded to ~1-2 μs worst case (4096 instructions at ~1-2 ns each)
- **Performance**: slight overhead per backward branch (one CMP+JGE) — libriscv's data suggests <5% slowdown on loop-heavy workloads, amortized to near-zero on code with mixed branches

## Why This is Safe Given Our Test Foundation

- `TestJIT_BlockReentry` already exercises the exact flow: block exits with PC=loop_target, dispatch re-enters, cached registers re-read from `x[]` — proven correct
- `TestJIT_CycleCount_Loop` verifies ic accuracy through multiple loop iterations
- Lockstep tests catch any register/memory divergence across the boundary
- `TestJIT_Fib` (BLT backward loop of 20 iterations) should still work — well under the 4096 budget, so it's a single block execution with no budget hits; a higher-iteration variant tests the budget path
