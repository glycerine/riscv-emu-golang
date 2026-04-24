# Plan: Diagnose and Fix rv8 Performance Regression

## Context

The rv8-inspired rewrite (Stages 0-11 of `rv8plan.md`) implemented a 12-register layout faithful to the CARRV 2017 paper. All tests pass, but performance regressed ~3.6%: **3262 MIPS vs 3385 MIPS** on the `bench_guest.elf` workload.

**Root cause confirmed**: Stage 12 (CISC Memory Operands) was not implemented. The bloat test at `jit_bloat_test.go:45-48` documents this explicitly:

```
// After rv8 always-stage lowerer (2026-04-23): ir=105,
// host=1582 (+503 bytes). Expected: every operand is staged
// through RAX/RCX. CISC memory operands (Stage 12) will
// recover most of this.
```

The "always-stage" pattern forces every spilled operand through RAX/RCX before the actual ALU operation, producing **+47% code bloat** (1582 bytes vs 1079 bytes for the reference block). This hurts I-cache utilization and instruction throughput.

Partial CISC infrastructure already exists: `spilledRegFileOff()` (line 478) and `emitRM`/`emitMR`/`emitMI` helpers are in place. The `rv8Binop` slow path already uses CISC for the B operand (line 892). The task is to extend CISC coverage to all operations.

---

## Phase 1: Diagnostics (read-only, no code changes)

### Step 1.1: Run dispatch counters
```bash
cd ~/ris && go test -run TestJIT_ChainReference -v ./bench/
cd ~/ris && go test -run TestJIT_DispatchStats -v ./bench/
```
Record: `DispatchOK`, `DispatchInterp`, `ChainPatched`, `insns/DispatchOK`, `MIPS`.
**Purpose**: Confirm chaining is working and interpreter fallback is minimal.

### Step 1.2: Run code size comparison
```bash
cd ~/ris && go test -run TestLower_CodeSize_V1_vs_V2 -v ./bench/
cd ~/ris && go test -run TestBloat_BenchGuest_0x10de -v .
```
Record: host bytes, IR ops, avg bytes/block.

### Step 1.3: CPU profile
```bash
cd ~/ris && go test -run='^$' -bench='^BenchmarkCPU_FullExecution_JIT_Fixed$' \
    -cpuprofile=cpu.prof -benchtime=3x ./bench/
go tool pprof -top cpu.prof
```
**Purpose**: See whether time is spent in Go dispatch overhead, JIT compilation, or the native code itself. If native code dominates (expected), the code quality fix (Stage 12) is the right lever.

### Step 1.4: Add vv() tracing to lowerInstr
In `ir/lower_amd64_rv8.go:287` (the `lowerInstr` switch), add temporary `vv()` logging to count fast-path vs slow-path hits per opcode during `TestBloat_BenchGuest_0x10de -v`. Example:
```go
func (lc *lowerCtxRV8) lowerInstr(ins *IRInstr) error {
    vv("lower[%d] op=%s dst=%v(kind=%d) A=%v(kind=%d) B=%v(kind=%d)",
       lc.idx, ins.Op, ins.Dst, lc.allocKindSafe(ins.Dst),
       ins.A, lc.allocKindSafe(ins.A), ins.B, lc.allocKindSafe(ins.B))
    // ...existing switch...
}
```
**Purpose**: Know which operations dominate and how many hit the fast path vs slow path.

---

## Phase 2: Correctness Fix

### Step 2.0: Fix FP staging register conflict
**File**: `ir/lower_amd64.go:38-43`

X14 and X15 are used as FP staging registers (`rv8StgFA`, `rv8StgFB`) but are also in the FP allocation pool. If the allocator assigns a RISC-V FP register to X14/X15, staging will silently clobber it.

**Fix**: Remove X14 and X15 from `RV8Pool.FPRegs`:
```go
fpRegs := []int16{
    goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, ..., goasm.REG_AMD64_X13,
    // X14 and X15 reserved for FP staging (rv8StgFA, rv8StgFB)
}
```

**Test**: Run `go test ./ir/` and `go test ./...`. Verify all FP exhaustive tests pass.

---

## Phase 3: Stage 12 — CISC Memory Operands

Each step is independently committable with all tests green. Lower `maxHostBytes` in `jit_bloat_test.go` after each step.

### Step 3.1: Fix `rv8Binop` fast-path gate (line 875)

**Current**: `if dstHR >= 0 && aHR >= 0 && dstHR != bHR`

The `dstHR != bHR` guard prevents `MOV aHR, dstHR` from clobbering B when dst and B share a host register. But when `dstHR == aHR`, there is no MOV, so `dstHR == bHR` is safe.

**Fix**: Handle the `dstHR == bHR && dstHR != aHR` case with staging:
```go
if dstHR >= 0 && aHR >= 0 {
    if dstHR == bHR && dstHR != aHR {
        // B lives in dst's register — stage through RAX to avoid clobber
        lc.emit2(x86.AMOVQ, aHR, rv8StgA)
        if bOff := lc.spilledRegFileOff(ins.B); bOff >= 0 {
            lc.emitRM(op, goasm.REG_AMD64_BP, bOff, rv8StgA)
        } else {
            lc.emit2(op, bHR, rv8StgA)
        }
        lc.emit2(x86.AMOVQ, rv8StgA, dstHR)
    } else {
        // existing fast-path (dstHR != bHR)
        ...
    }
    lc.commitDst(ins.Dst, dstHR)
    return
}
```

**Test**: `TestRV8Binop_DstEqB` — block where Dst and B are the same VReg in the same host register.

### Step 3.2: CISC `rv8Mov` — skip staging when one operand is spilled, other is in register
**File**: `ir/lower_amd64_rv8.go:791`

Add before the slow-path fallthrough:
```go
// CISC: dst in register, A spilled → load directly from [RBP+off]
if dstHR >= 0 {
    if aOff := lc.spilledRegFileOff(ins.A); aOff >= 0 {
        lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, aOff, dstHR)
        lc.commitDst(ins.Dst, dstHR)
        return
    }
}
// CISC: A in register, dst spilled → store directly to [RBP+off]
if aHR >= 0 {
    if dstOff := lc.spilledRegFileOff(ins.Dst); dstOff >= 0 {
        lc.emitMR(x86.AMOVQ, aHR, goasm.REG_AMD64_BP, dstOff)
        return // no commitDst needed — already written to memory
    }
}
```

**Savings**: 7 bytes per occurrence (eliminates staging MOV).

### Step 3.3: CISC `rv8Sext` / `rv8Zext` — spill-to-register direct load
**File**: `ir/lower_amd64_rv8.go:816, 842`

Same pattern as `rv8Mov`: when dst is in a host register and A is spilled, load directly with the extending opcode:
```go
if dstHR >= 0 {
    if aOff := lc.spilledRegFileOff(ins.A); aOff >= 0 {
        lc.emitRM(op, goasm.REG_AMD64_BP, aOff, dstHR)
        lc.commitDst(ins.Dst, dstHR)
        return
    }
}
```

### Step 3.4: CISC `rv8BinopImm` — `OP [RBP+off], imm32` when dst==A spilled
**File**: `ir/lower_amd64_rv8.go:905`

When `ins.Dst == ins.A` and both are spilled, operate directly on memory:
```go
if ins.Dst == ins.A {
    if dstOff := lc.spilledRegFileOff(ins.Dst); dstOff >= 0 {
        imm := ins.Imm
        if imm >= -(1<<31) && imm < (1<<31) {
            lc.emitMI(op, imm, goasm.REG_AMD64_BP, dstOff)
            return // no staging, no commit needed
        }
    }
}
```

**Savings**: Up to 14 bytes per occurrence (eliminates both stage-A and commit MOVs). `AddImm` is the most frequent IR opcode, making this the highest-impact single change.

### Step 3.5: CISC `rv8ShiftImm` — `SHL [RBP+off], imm` when dst==A spilled
**File**: `ir/lower_amd64_rv8.go:986`

Same pattern as BinopImm:
```go
if ins.Dst == ins.A {
    if dstOff := lc.spilledRegFileOff(ins.Dst); dstOff >= 0 {
        lc.emitMI(op, ins.Imm, goasm.REG_AMD64_BP, dstOff)
        return
    }
}
```

### Step 3.6: `rv8LoadX` / `rv8StoreX` — use directReg before staging
**File**: `ir/lower_amd64_rv8.go:1229, 1248`

Currently always stages both base and index. Add directReg checks:
```go
func (lc *lowerCtxRV8) rv8LoadX(ins *IRInstr) {
    base := lc.directReg(ins.A)
    if base < 0 {
        base = lc.stageInt(ins.A, 0)
    }
    idx := lc.directReg(ins.B)
    if idx < 0 {
        idx = lc.stageInt(ins.B, 1)
    }
    // ...rest unchanged...
}
```

**Note**: Must verify base != idx when both come from directReg to avoid SIB encoding issues. If they collide with staging registers, fall back.

---

## Phase 4: Verification

### Step 4.1: Code size regression gate
After each CISC step, run:
```bash
go test -run TestBloat_BenchGuest_0x10de -v .
```
Lower `maxHostBytes` in the test to lock in the improvement.

**Target**: Recover ~100-230 bytes of the 503-byte regression → host bytes from 1582 to ~1350-1450.

### Step 4.2: Full test suite
```bash
go test ./...
```

### Step 4.3: MIPS measurement
```bash
make bench
```
**Target**: Recover from 3262 MIPS to ~3350-3400+ MIPS.

### Step 4.4: Dispatch counter re-check
```bash
go test -run TestJIT_ChainReference -v ./bench/
```
Verify dispatch ratios haven't changed (chaining should be unaffected by code quality changes).

---

## Key Files

| File | Role |
|------|------|
| `ir/lower_amd64_rv8.go` | All CISC changes (Steps 3.1-3.6) |
| `ir/lower_amd64.go:38-43` | FP pool fix (Step 2.0) |
| `jit_bloat_test.go` | Code size regression gate |
| `bench/jit_chain_reference_test.go` | Dispatch counter diagnostics |
| `bench/jit_bench_test.go:26-44` | Dispatch stats test |
| `bench/lower_bench_test.go:167-204` | Execution MIPS benchmark |
| `rv8plan.md` | Master plan reference |

## Existing Helpers to Reuse

| Helper | Location | Purpose |
|--------|----------|---------|
| `spilledRegFileOff(v)` | `lower_amd64_rv8.go:478` | Returns `[RBP+r*8]` offset for spilled RISC-V registers |
| `directReg(v)` | `lower_amd64_rv8.go:644` | Returns host register for VRegs 1-63, -1 otherwise |
| `emitRM(op, base, off, dst)` | `lower_amd64_rv8.go:691` | `dst = OP [base+off]` — load/ALU from memory |
| `emitMR(op, src, base, off)` | `lower_amd64_rv8.go:702` | `[base+off] = OP src` — store/ALU to memory |
| `emitMI(op, imm, base, off)` | `lower_amd64_rv8.go:713` | `[base+off] = OP imm` — immediate to memory |
| `vv(fmt, args...)` | `ir/vprint.go:80` | Debug logging (set `forceQuiet = false` to enable) |

## Commit Order

1. Phase 1 diagnostics (vv tracing, counter runs — may be temporary)
2. Step 2.0: FP pool conflict fix (correctness)
3. Step 3.1: Binop fast-path gate fix
4. Step 3.4: BinopImm CISC (highest impact)
5. Step 3.2: Mov CISC
6. Step 3.3: Sext/Zext CISC
7. Step 3.5: ShiftImm CISC
8. Step 3.6: LoadX/StoreX directReg
9. Phase 4 verification (bench, counters, full test suite)
