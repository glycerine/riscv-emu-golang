# Fix V1 Lowerer: Native-Code Trace + R11 Clobber Bug

## Context

The V1 lowerer has a confirmed bug at `maxBlockInsns>=15` that produces wrong shift results. The block at pc=0x34e in rv64ui-p-srl has `SHR x14=x1,x2` where x2 is stack-allocated (spill slot 0). V1 produces 0x40 (wrong), V2 produces 0x01000000 (correct). The value 0x80000000 should be shifted right by 7.

**Root cause identified**: `lowerShift` uses R11 (amd64Scratch2) for two conflicting purposes:
1. `use(ins.B, 1)` loads the spill slot into R11 (scratch register for scratchIdx=1)
2. `needCXSave` or `aSavedToR11` saves CX to R11, **clobbering the loaded shift count**

After the clobber, `MOV R11, CX` puts CX's own old value into CX instead of the shift count. The shift uses the wrong amount.

V2 avoids this by using `XCHG CX, R11` which swaps without destroying either value.

## The Bug in Detail

### Reproducer

`TestBisectBlockSize` with `rv64ui-p-srl`: n=1..14 pass, n=15 fails. Block at pc=0x34e contains `SHR x14=x1,x2` at IR index 28.

Register allocation for this instruction:
- x1 (a) → RAX (register-allocated)
- x2 (b) → STACK, spill slot 0, kind=2
- x14 (dst) → R9 (register-allocated)

### What V1 generates (buggy)

```
use(ins.B, 1) → spillLoad(0, R11) → MOVQ 0(RSP), R11   // R11 = 7 (shift count)
use(ins.A, 0) → RAX                                       // a = RAX
def(ins.Dst)  → R9                                         // dst = R9

lowerShift:
  needCXSave = (R11!=CX && R9!=CX && isCXLive()) = true  // some VReg live in CX
  a!=CX → skip aSavedToR11
  needCXSave → MOVQ CX, R11          ← CLOBBERS b! R11 now = old CX, not 7
  MOVQ R11, CX                        ← puts old CX back into CX (not the shift count!)
  MOVQ RAX, R9                        ← value to shift
  SHRQ CL, R9                         ← shifts by CX's original value, not 7!
  MOVQ R11, CX                        ← "restores" CX
```

### What V2 generates (correct)

```
stageInt(A, 0) → MOVQ RAX, R10        // R10 = value
stageInt(B, 1) → MOVQ 0(RSP), R11     // R11 = 7 (shift count)
needCXSave → XCHG R11, CX             // CX = 7, R11 = old CX (both preserved!)
SHRQ CL, R10                          // shifts R10 by 7. Correct!
MOVQ R10, R9                          // write to dst
MOVQ R11, CX                          // restore CX from R11
```

### Root Cause

`lowerShift` saves CX to R11 (`amd64Scratch2`) with a plain MOV, but `use(ins.B, 1)` already loaded the shift count into R11 from a spill slot. The MOV destroys the loaded value. V2 avoids this by using `XCHG` which swaps without loss.

The same bug occurs when `a==CX`: the `aSavedToR11` path does `MOVQ CX, R11` which also clobbers a spill-loaded b in R11.

## Implementation Plan

### Step 1: Add Native-Code Dump Infrastructure

**File: `goasm/api.go`** — add `DumpProgs()` method:

```go
// DumpProgs returns a human-readable listing of all Progs.
func (c *Ctx) DumpProgs() string {
    var sb strings.Builder
    for p := c.firstProg; p != nil; p = p.Link {
        fmt.Fprintf(&sb, "%s\n", p.InstructionString())
    }
    return sb.String()
}
```

**File: `jit_native.go`** — add `jitCompileDebug()`:

```go
type compileDebugInfo struct {
    code  []byte // assembled native bytes
    progs string // symbolic Prog listing (Go asm syntax)
}

func jitCompileDebug(res *emitResult, useV2 bool) (*compiledBlock, *compileDebugInfo, error) {
    // Same as jitCompileWith, but captures ctx.DumpProgs() and code bytes
}
```

### Step 2: Write the Diagnostic Test

**File: `jit_emit_ir_test.go`** — add `TestNativeTrace_0x34e`:

```go
func TestNativeTrace_0x34e(t *testing.T) {
    // 1. Emit block at pc=0x34e with maxBlockInsns=15
    // 2. Compile with V1 → get Prog listing + bytes
    // 3. Compile with V2 → get Prog listing + bytes
    // 4. Log both Prog listings side by side
    // 5. Write bytes to temp files, disassemble with llvm-objdump:
    //    llvm-objdump -d --triple=x86_64 --bytes /tmp/v1.bin
    // 6. Log disassembly for both
    // 7. Diff at instruction level: find first divergence
}
```

This test will confirm the R11 clobber is visible in the native code and give us exact byte-level evidence.

### Step 3: Fix `lowerShift` in V1

**File: `ir/lower_amd64.go`** lines 876-959.

The fix: when `b == amd64Scratch2` (R11, from spill load), use `XCHG` instead of `MOV` for the CX save. This is exactly what V2 does — one instruction, no value destruction.

```go
func (lc *lowerCtx) lowerShift(ins *IRInstr, op obj.As) {
    a := lc.use(ins.A, 0)
    b := lc.use(ins.B, 1)
    dst := lc.def(ins.Dst)

    needCXSave := b != goasm.REG_AMD64_CX && dst != goasm.REG_AMD64_CX && lc.isCXLive()

    // Determine if b occupies a scratch register (from spill load).
    bInScratch := (b == amd64Scratch1 || b == amd64Scratch2)

    aSavedToR11 := false
    cxSaveReg := int16(-1) // where we saved old CX (-1 = not saved)

    if a == goasm.REG_AMD64_CX && b != goasm.REG_AMD64_CX {
        if bInScratch {
            // a==CX, b in scratch (R11). XCHG swaps: CX→R11 (save a), R11→CX (load count).
            lc.emitRR(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
            aSavedToR11 = true
            cxSaveReg = b // old CX (=a) is now in b's former location
            b = goasm.REG_AMD64_CX // b is now in CX
        } else {
            // a==CX, b in a regular register. Save a to R11, then MOV b→CX.
            lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
            aSavedToR11 = true
        }
    } else if needCXSave {
        if bInScratch {
            // CX live, b in scratch. XCHG: CX→scratch (save), scratch→CX (load count).
            lc.emitRR(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
            cxSaveReg = b
            b = goasm.REG_AMD64_CX
        } else {
            // CX live, b in regular register. Safe to save CX to R11 (b isn't there).
            lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
            cxSaveReg = amd64Scratch2
        }
    }

    // Move count (b) into CX if not already there.
    if b != goasm.REG_AMD64_CX {
        lc.emitRR(x86.AMOVQ, b, goasm.REG_AMD64_CX)
    }

    // Effective location of a.
    aEff := a
    if aSavedToR11 {
        aEff = amd64Scratch2
        if cxSaveReg == amd64Scratch2 {
            aEff = cxSaveReg // XCHG put a into b's old scratch location
        }
    }

    // Shift into dst.
    if dst == goasm.REG_AMD64_CX {
        scr := amd64Scratch1
        if aEff != scr { lc.emitRR(x86.AMOVQ, aEff, scr) }
        lc.emitRR(op, goasm.REG_AMD64_CX, scr)
        lc.emitRR(x86.AMOVQ, scr, dst)
    } else {
        if dst != aEff { lc.emitRR(x86.AMOVQ, aEff, dst) }
        lc.emitRR(op, goasm.REG_AMD64_CX, dst)
    }

    // Restore CX if saved.
    if cxSaveReg >= 0 && !aSavedToR11 {
        lc.emitRR(x86.AMOVQ, cxSaveReg, goasm.REG_AMD64_CX)
    } else if cxSaveReg >= 0 && aSavedToR11 && needCXSave {
        // CX was live AND a was CX. After XCHG, scratch holds old CX.
        // But old CX == a, which is what we wanted to save. CX restore
        // only needed if CX was live for a DIFFERENT VReg, which can't
        // happen (one VReg per register). So this branch is unreachable.
        lc.emitRR(x86.AMOVQ, cxSaveReg, goasm.REG_AMD64_CX)
    }

    lc.defCommit(ins.Dst, dst)
}
```

Key insight: when b is in a scratch register (came from spill load), **XCHG** atomically swaps the CX save and count load in one instruction, no clobber possible.

### Step 4: Audit All Scratch-Register Conflicts

Other lowering functions that call `use(x, 1)` (returns R11 from spill) and then write R11 for another purpose:

| Function | Uses scratch? | Writes R11? | Status |
|----------|--------------|-------------|--------|
| `lowerBinop` | use(B,1)→R11 | Only if large imm | Check: `lowerBinopImm` uses `amd64Scratch2` for large immediates |
| `lowerBinopImm` | use(A,1)→R11 | scratch2 for large imm | Already hardened (dst==scratch2 check) |
| `lowerSet` | use(B,1)→R11 | No | OK |
| `lowerDiv` | use(B,1)→R11 | Saves b to R11 if b==RAX | Already hardened |
| `lowerMulHigh` | use(B,1)→R11 | Saves b to R11 if b==RAX | Already hardened |
| `lowerMulHSU` | use(B,1)→R11 | Saves b to R11 if b==RAX | Already hardened |
| `lowerFCmp` | use via fpScratch | R11 not involved | OK |
| `lowerShiftImm` | use(A,1)→R11 | No CX handling | OK |

The shift case is the primary bug. The DIV/MUL cases were already hardened in previous sessions.

### Step 5: Verify with Native-Code Trace

After the fix, re-run:
1. `TestNativeTrace_0x34e` — V1 Prog listing should now use XCHG for the CX save, matching V2's behavior
2. `TestBisectBlockSize` — all sizes n=1..2048 should pass
3. `TestDebugV1V2_SRL` — no V1/V2 divergence
4. `TestMetaIterOrder_AllUI` — all iteration orders pass

### Step 6: Benchmark gc_riscv64

With V1 fixed and maxBlockInsns=2048:
```bash
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/
```

## Verification

```bash
# Native-code trace test (confirms bug, then fix)
go test -count=1 -run 'TestNativeTrace_0x34e' -timeout 30s -v .

# Bisect: all sizes should pass after fix
go test -count=1 -run 'TestBisectBlockSize' -timeout 60s -v .

# V1/V2 lockstep on SRL
go test -count=1 -run 'TestDebugV1V2_SRL' -timeout 30s -v .

# All ELF tests (full regression)
go test -count=1 -run 'TestRISCVTests_Lockstep_UI' -timeout 120s -v .

# Meta iteration order (non-determinism check)
go test -count=1 -run 'TestMetaIterOrder_AllUI' -timeout 120s -v .

# Exhaustive register-pair tests (shift conflicts)
go test -count=1 -run 'TestExhaustive' -timeout 60s -v ./ir/

# Benchmark: native JIT vs TCC on gc_riscv64
go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=1x ./bench/
```

## Critical Files

- `goasm/api.go` — add `DumpProgs()` method
- `jit_native.go` — add `jitCompileDebug()` for native-code dump
- `jit_emit_ir_test.go` — add `TestNativeTrace_0x34e`
- `ir/lower_amd64.go` — fix `lowerShift` R11 clobber (XCHG approach)
