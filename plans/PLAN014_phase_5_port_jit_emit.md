# Phase 5: Port jit_emit.go to Produce IR

## Context

The RISC-V emulator's JIT currently translates basic blocks into C source strings (`jit_emit.go`), compiled by TCC via cgo. The `ir/` package is **fully implemented** (50+ IR ops, emitter API, peephole optimizer, ELS register allocator, complete AMD64 lowering with 253+ unit tests). Phase 5 replaces every `e.emit("C text")` call with equivalent `ir.Emitter` API calls, so `emitBlock()` produces an `*ir.Block` instead of a C string.

The decode-and-dispatch skeleton (emit32 switch, emitRVC quadrant handlers, scanRegion, classifyFlow, immediate helpers) stays structurally identical. Only the **output side** changes.

---

## IR Gaps to Fill First

Before porting `jit_emit.go`, add these missing IR capabilities:

### 1. `IRRemU` — unsigned remainder
- Current IR has `IRRem` (signed) but no unsigned variant
- REMU is used by RV64M extension
- **ir/ir.go**: Add `IRRemU` op after `IRRem`
- **ir/emit.go**: Add `RemU(dst, a, b VReg)` method
- **ir/lower_amd64.go**: Lower as `XORQ RDX,RDX; DIVQ b; MOVQ RDX,dst`
- **Tests**: Unit test in `ir/lower_amd64_test.go`, emit test in `ir/emit_test.go`

### 2. `IRRetDyn` — return with runtime-computed PC
- Current `IRRet` takes PC as a constant (`Imm`). JALR computes target PC at runtime from a register
- **ir/ir.go**: Add `IRRetDyn` op — PC comes from VReg `A`, status from `Imm`, faultAddr from `B`
- **ir/emit.go**: Add `RetDyn(pcVReg VReg, status int, faultAddr VReg)` method
- **ir/lower_amd64.go**: Same as `lowerRet` but store `A` register value into sret PC field instead of immediate
- **Tests**: Unit test for lowering, emit test

### 3. CLZ/CTZ/CPOP via `IRCall`
- Currently the C path uses inline C helpers. For the IR path, use `IRCall` to Go-exported helper functions
- **Root package**: Add `jit_helpers.go` with exported `JitClz64`, `JitCtz64`, `JitCpop64` (and 32-bit variants) functions
- Register these in the `ir.Block.CTab` at emission time
- No new IR ops needed — reuse existing `IRCall` infrastructure

### 4. FMIN/FMAX via `IRCall`
- The C path uses custom `jit_fminf`/`jit_fmaxf` with NaN semantics
- Add Go helper functions `JitFminf`, `JitFmaxf`, `JitFmin`, `JitFmax`
- Emit as `IRCall`

---

## File Structure

### New files
| File | Build tag | Purpose |
|------|-----------|---------|
| `jit_emit_ir.go` | `!tcc` | IR-emitting `emitter` struct and all emission methods |
| `jit_decode.go` | *(none)* | Shared decode-only functions extracted from `jit_emit.go` |
| `jit_native.go` | `!tcc` | `irCompile()` pipeline, `allocExec()`, `compiledBlock` struct |
| `jit_helpers.go` | *(none)* | Go helper functions for CLZ/CTZ/CPOP/FMIN/FMAX callable via IRCall |
| `jit_emit_ir_test.go` | `!tcc` | Unit tests for the IR emitter path |

### Modified files
| File | Change |
|------|--------|
| `jit_emit.go` | Add `//go:build tcc` tag so it only compiles under TCC mode |
| `jit_tcc.go` | Add `//go:build tcc` tag |
| `jit.go` | Replace direct `tccCompile(res.csrc)` with `jitCompile(res)` dispatched per build tag |
| `ir/ir.go` | Add `IRRemU`, `IRRetDyn` ops |
| `ir/emit.go` | Add `RemU()`, `RetDyn()` methods |
| `ir/lower_amd64.go` | Add lowering for `IRRemU`, `IRRetDyn` |

### Extracted to `jit_decode.go` (shared, no build tag)
These are pure decode functions with no emission logic:
- `classifyFlow()`, `scanRegion()`, `regionInfo` struct
- `flowSeq/flowBranch/flowJump/flowTerm` constants
- `jImm()`, `bImm()`, `sImm()`, `loadInfo()`, `storeInfo()`, `branchCmp()`
- `rvcSignedImm6()`, `rvcJOffset()`, `rvcBOffset()`
- `emitResult` struct (with `csrc string` under `tcc` tag, `block *ir.Block` under `!tcc` tag — or use interface)

---

## Step-by-Step Implementation

### Step 0: Build Tag Isolation + Shared Decode Extraction

**Goal**: TCC path continues working while IR path is developed in parallel.

1. Extract decode-only functions from `jit_emit.go` into `jit_decode.go` (no build tag)
2. Add `//go:build tcc` to `jit_emit.go` and `jit_tcc.go`
3. Create stub `jit_emit_ir.go` with `//go:build !tcc` containing just the new `emitResult` and empty `emitBlock`
4. Create stub `jit_native.go` with `//go:build !tcc` containing `compiledBlock` struct and `jitCompile` function
5. Update `jit.go` to call `jitCompile(res)` which is defined per build tag

**`emitResult` under `!tcc`**:
```go
type emitResult struct {
    block    *ir.Block
    startPC  uint64
    endPC    uint64
    numInsns int
    regsUsed [32]bool
}
```

**Test**: `go test -tags tcc -run TestJIT_ADD .` — all existing tests still pass.

---

### Step 1: Core Emitter Infrastructure

**Goal**: New `emitter` struct with IR emitter, helper methods replacing `rd()/rs()/emit()/emitLabel()/advancePC()/emitReturn()/finalize()`.

**New `emitter` struct** in `jit_emit_ir.go`:
```go
type emitter struct {
    mem        *GuestMemory
    startPC    uint64
    pc         uint64
    irEm       *ir.Emitter
    numInsns   int
    regsUsed   [32]bool
    terminated bool
    visited    map[uint64]bool
    regionEnd  uint64
    gotoTargets map[uint64]bool
    pcLabels   map[uint64]ir.Label  // guest PC → IR label
    icEmitted  bool

    // Lazy-load tracking
    intLoaded  [32]bool  // x registers loaded from x[] array
    fpLoaded   [32]bool  // f registers loaded from f[] array

    // Fault labels (shared across block)
    loadFaultLabel  ir.Label
    storeFaultLabel ir.Label

    // Deferred external exits
    deferredExits []deferredExit
}

type deferredExit struct {
    label    ir.Label
    targetPC uint64
}
```

**Key method translations**:

| Old method | New method | Implementation |
|---|---|---|
| `rd(r) string` → C var name | `xregDst(r) ir.VReg` | Returns `irEm.XReg(r)`, sets `regsUsed[r]`, marks `intLoaded[r]=true` (no load — write-only) |
| `rs(r) string` → C var name | `xreg(r) ir.VReg` | Returns `irEm.XReg(r)`, sets `regsUsed[r]`, lazy-loads from x[] on first access |
| `emit(fmt, args)` | *(deleted)* | Each call site replaced by specific IR calls |
| `emitLabel()` | `emitLabel()` | `irEm.PlaceLabel(getOrCreateLabel(e.pc))` |
| `emitIC()` | `emitIC()` | `irEm.AddImm(irEm.IC(), irEm.IC(), 1)` |
| `advancePC(sz)` | `advancePC(sz)` | Same logic: bump pc/numInsns, emit IC if !icEmitted |
| `emitWriteBackAll()` | `emitWriteBackAll()` | `irEm.WriteBackAll()` |
| `emitReturn(pc, status)` | `emitReturn(pc, status)` | `irEm.WriteBackAll(); irEm.Ret(pc, status, ir.VRegZero)` |
| `emitReturnFault(pc, status, addr)` | `emitReturnFault(pc, status, addrVReg)` | `irEm.FaultExit(pc, status, addrVReg)` — note: addr is now a VReg, not a string expr |
| `finalize()` | `finalize()` | Emit fall-through return, bail labels, deferred exits; return `&emitResult{block: irEm.Block, ...}` |

**Lazy register loading** — critical for correctness:
```go
func (e *emitter) xreg(r uint32) ir.VReg {
    if r == 0 { return ir.VRegZero }
    e.regsUsed[r] = true
    vr := e.irEm.XReg(r)
    if !e.intLoaded[r] {
        e.irEm.Load(vr, e.irEm.XBase(), int64(r)*8, ir.I64, false)
        e.intLoaded[r] = true
    }
    return vr
}

func (e *emitter) xregDst(r uint32) ir.VReg {
    if r == 0 { return ir.VRegZero }
    e.regsUsed[r] = true
    e.intLoaded[r] = true  // suppress future load — we're about to overwrite
    return e.irEm.XReg(r)
}
```

Same pattern for `freg(r)` / `fregDst(r)` using `irEm.FRegV(r)` and `irEm.FBase()`.

**`getOrCreateLabel`**:
```go
func (e *emitter) getOrCreateLabel(pc uint64) ir.Label {
    if l, ok := e.pcLabels[pc]; ok { return l }
    l := e.irEm.NewLabel()
    e.pcLabels[pc] = l
    return l
}
```

**`finalize()`** replacement:
1. If not terminated: emit fall-through `WriteBackAll() + Ret(e.pc, 0, VRegZero)`
2. Emit bail labels: for each `target ∈ gotoTargets` not in `visited`, emit `PlaceLabel(getOrCreateLabel(target)); WriteBackAll(); Ret(target, 0, VRegZero)`
3. Emit deferred external exits: for each `deferredExit`, emit `PlaceLabel(label); WriteBackAll(); Ret(targetPC, 0, VRegZero)`
4. Emit load/store fault handlers: `PlaceLabel(loadFaultLabel); FaultExit(e.pc, jitLoadFault, faultAddrVReg)` and similarly for store fault
5. Return `emitResult`

**`emitBlock()`**: Same structure — `scanRegion` then walk PCs — but creates `ir.NewEmitter()` instead of `strings.Builder`. Pre-allocates fault labels.

**Tests**: Unit test creating an emitter, calling `advancePC(4)`, verifying the block contains `IRAddImm` for IC. Test `finalize()` produces terminal `IRRet`.

---

### Step 2: Integer ALU — emitOp, emitOpImm, emitOp32, emitOpImm32

**Translation patterns** (inside each switch case, replace `e.emit(...)` with IR call):

**emitOp (opcode 0x33) — R-type**:
- Each case currently does: `d := e.rd(rd); a := e.rs(rs1); b := e.rs(rs2); e.emit("d = a op b")`
- New pattern: `dst := e.xregDst(rd); a := e.xreg(rs1); b := e.xreg(rs2); e.irEm.Add(dst, a, b)`

| funct7 | funct3 | RISC-V | IR call |
|--------|--------|--------|---------|
| 0x00 | 0 | ADD | `Add(dst, a, b)` |
| 0x20 | 0 | SUB | `Sub(dst, a, b)` |
| 0x00 | 1 | SLL | `Shl(dst, a, b)` |
| 0x00 | 2 | SLT | `Set(dst, a, b, LT)` |
| 0x00 | 3 | SLTU | `Set(dst, a, b, LTU)` |
| 0x00 | 4 | XOR | `Xor(dst, a, b)` |
| 0x00 | 5 | SRL | `Shr(dst, a, b)` |
| 0x20 | 5 | SRA | `Sar(dst, a, b)` |
| 0x00 | 6 | OR | `Or(dst, a, b)` |
| 0x00 | 7 | AND | `And(dst, a, b)` |
| 0x01 | 0 | MUL | `Mul(dst, a, b)` |
| 0x01 | 1 | MULH | `MulHS(dst, a, b)` ← **no longer bails!** |
| 0x01 | 2 | MULHSU | `MulHSU(dst, a, b)` ← **no longer bails!** |
| 0x01 | 3 | MULHU | `MulHU(dst, a, b)` ← **no longer bails!** |
| 0x01 | 4 | DIV | `DivS(dst, a, b)` |
| 0x01 | 5 | DIVU | `DivU(dst, a, b)` |
| 0x01 | 6 | REM | `Rem(dst, a, b)` |
| 0x01 | 7 | REMU | `RemU(dst, a, b)` ← **new IR op** |

**Zbb (0x05/0x20/0x30 funct7)**:
- ANDN: `t := Tmp(); Not(t, b); And(dst, a, t)`
- ORN: `t := Tmp(); Not(t, b); Or(dst, a, t)`
- XNOR: `Xor(dst, a, b); Not(dst, dst)` (or `t := Tmp(); Xor(t, a, b); Not(dst, t)`)
- MIN: `Set(t, a, b, LT); Branch(t, VRegZero, NE, takeA); Mov(dst, b); Jump(done); PlaceLabel(takeA); Mov(dst, a); PlaceLabel(done)`
- MAX: Same with `GT`
- MINU/MAXU: Same with `LTU`/`GTU`
- ROL/ROR: `t1 := Tmp(); Shl(t1, a, b); t2 := Tmp(); sub := Tmp(); Const(sub, 64); Sub(sub, sub, b); Shr(t2, a, sub); Or(dst, t1, t2)`
- CLZ/CTZ/CPOP: `IRCall` to helper functions
- BEXT: `t := Tmp(); Shr(t, a, b); AndImm(dst, t, 1)`
- REV8/ORC.B: Emit as sequence of shifts/masks/ORs matching the C code logic

**Zba (SH1ADD/SH2ADD/SH3ADD)**:
- `t := Tmp(); ShlImm(t, a, 1/2/3); Add(dst, b, t)`

**Zbs (BSET/BCLR/BINV)**:
- BSET: `t := Tmp(); Const(t2, 1); Shl(t, t2, b); Or(dst, a, t)`
- BCLR: `t := Tmp(); Const(t2, 1); Shl(t, t2, b); Not(t, t); And(dst, a, t)`
- BINV: `t := Tmp(); Const(t2, 1); Shl(t, t2, b); Xor(dst, a, t)`

**Zicond (CZERO.EQZ/CZERO.NEZ)**:
- CZERO.EQZ: `skip := NewLabel(); done := NewLabel(); Branch(b, VRegZero, NE, skip); Const(dst, 0); Jump(done); PlaceLabel(skip); Mov(dst, a); PlaceLabel(done)`
- CZERO.NEZ: Swap the condition (use EQ)

**emitOpImm (opcode 0x13)**:
| RISC-V | IR |
|---|---|
| ADDI (imm=0, rs1=0): LI | `Const(dst, 0)` |
| ADDI (imm=0): MV | `Mov(dst, src)` |
| ADDI | `AddImm(dst, src, imm)` |
| SLTI | `SetImm(dst, src, imm, LT)` |
| SLTIU | `SetImm(dst, src, imm, LTU)` |
| XORI (imm=-1): NOT | `Not(dst, src)` |
| XORI | `XorImm(dst, src, imm)` |
| ORI | `OrImm(dst, src, imm)` |
| ANDI | `AndImm(dst, src, imm)` |
| SLLI | `ShlImm(dst, src, shamt)` |
| SRLI | `ShrImm(dst, src, shamt)` |
| SRAI | `SarImm(dst, src, shamt)` |

Zbb immediate ops (BSETI, BCLRI, RORI, SEXT.B, SEXT.H, CLZ, CTZ, CPOP, ORC.B, REV8, BEXTI):
- BSETI: `t := Tmp(); Const(t, 1<<shamt); Or(dst, src, t)`
- BCLRI: `t := Tmp(); Const(t, ^(int64(1)<<shamt)); And(dst, src, t)`
- RORI: `t1 := Tmp(); ShrImm(t1, src, shamt); t2 := Tmp(); ShlImm(t2, src, 64-shamt); Or(dst, t1, t2)`
- SEXT.B: `Sext(dst, src, I8)`
- SEXT.H: `Sext(dst, src, I16)`
- CLZ/CTZ/CPOP: `IRCall` to Go helpers

**emitOp32/emitOpImm32 (W-suffix ops)**:
These do 32-bit arithmetic then sign-extend. Pattern:
```
// ADDW: dst = sext32(a + b)
Add(t, a, b)
Sext(dst, t, I32)
```
Same for SUBW, SLLW, SRLW, SRAW, MULW, DIVW, DIVUW, REMW, REMUW, and immediate variants.

**Bail cases**: All `e.terminated = true` for unknown funct3/funct7 remain the same — set `e.terminated = true` and return.

**Tests**: For each instruction family, write a test that:
1. Encodes a single RISC-V instruction + ECALL into guest memory
2. Calls `emitBlock()` to get `*ir.Block`
3. Verifies expected IR ops are present
4. Full-pipeline test: allocate → lower → assemble → execute via trampoline → compare to interpreter

---

### Step 3: Memory — emitLoad, emitStore, emitFPLoad, emitFPStore

**Integer loads** — use `MaskedLoad` high-level helper:
```go
func (e *emitter) emitLoad(rd, rs1 uint32, imm int64, funct3 uint32) {
    width, signed := irLoadInfo(funct3)
    if width == 0 { e.terminated = true; return }
    dst := e.xregDst(rd)
    base := e.xreg(rs1)

    if width > 1 {
        // Check alignment; bail to interpreter if misaligned
        addr := e.irEm.Tmp()
        e.irEm.AddImm(addr, base, imm)
        misalignLabel := e.irEm.NewLabel()
        alignBits := e.irEm.Tmp()
        e.irEm.AndImm(alignBits, addr, int64(width-1))
        e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, misalignLabel)
        // Aligned path
        e.irEm.MaskedLoad(dst, base, e.irEm.MemBase(), e.irEm.MemMask(),
            imm, width, signed, e.loadFaultLabel)
        doneLabel := e.irEm.NewLabel()
        e.irEm.Jump(doneLabel)
        // Misaligned: bail to interpreter at current PC
        e.irEm.PlaceLabel(misalignLabel)
        e.irEm.WriteBackAll()
        e.irEm.Ret(e.pc, jitOK, ir.VRegZero)
        e.irEm.PlaceLabel(doneLabel)
    } else {
        // Byte loads: never misaligned
        e.irEm.MaskedLoad(dst, base, e.irEm.MemBase(), e.irEm.MemMask(),
            imm, 1, signed, e.loadFaultLabel)
    }
}
```

**Integer stores** — use `GuestStore`:
Same pattern with alignment check + `GuestStore`. Bail to interpreter for misaligned.

**FP loads (FLW, FLD)**:
- FLD: `MaskedLoad(freg(rd), base, memBase, memMask, imm, 8, false, loadFaultLabel)` with alignment fault check
- FLW: Load 32-bit value into temp, then NaN-box:
  ```
  MaskedLoad(tmp, base, memBase, memMask, imm, 4, false, loadFaultLabel)
  // NaN-box: f[rd] = tmp | 0xFFFFFFFF00000000
  hi := Tmp(); Const(hi, 0xFFFFFFFF00000000)
  Or(fregDst(rd), tmp, hi)
  ```

**FP stores (FSW, FSD)**:
- FSD: `GuestStore(base, memBase, memMask, imm, freg(rs2), 8, storeFaultLabel)`
- FSW: Extract low 32 bits then store:
  ```
  tmp := Tmp(); Zext(tmp, freg(rs2), I32)
  GuestStore(base, memBase, memMask, imm, tmp, 4, storeFaultLabel)
  ```

**Tests**: Tests for LB/LH/LW/LD/LBU/LHU/LWU, SB/SH/SW/SD, FLW/FLD/FSW/FSD. Test OOB fault. Test alignment bail for multi-byte loads.

---

### Step 4: Control Flow — emitBranch, emitJAL, emitJALR

**`branchPred(funct3)`** — maps RISC-V branch funct3 to `ir.Pred`:
```
0→EQ, 1→NE, 4→LT, 5→GE, 6→LTU, 7→GEU
```

**emitBranch**:
```go
func (e *emitter) emitBranch(rs1, rs2, funct3 uint32, offset int64) {
    target := e.pc + uint64(offset)
    pred := branchPred(funct3)

    e.irEm.AddImm(e.irEm.IC(), e.irEm.IC(), 1)  // IC++ before branch
    e.icEmitted = true

    a := e.xreg(rs1); b := e.xreg(rs2)

    internal := e.visited[target] ||
        (e.regionEnd > 0 && target >= e.startPC && target < e.regionEnd)

    if internal {
        targetLabel := e.getOrCreateLabel(target)
        if target < e.pc {
            // Backward: taken → budget check
            takenLabel := e.irEm.NewLabel()
            e.irEm.Branch(a, b, pred, takenLabel)
            // fall-through continues
            e.irEm.PlaceLabel(takenLabel)
            e.irEm.BudgetCheck(targetLabel, target)
        } else {
            e.irEm.Branch(a, b, pred, targetLabel)
        }
        e.gotoTargets[target] = true
    } else {
        // External: defer exit body
        exitLabel := e.irEm.NewLabel()
        e.irEm.Branch(a, b, pred, exitLabel)
        e.deferredExits = append(e.deferredExits, deferredExit{exitLabel, target})
    }
}
```

**emitJAL**:
```go
func (e *emitter) emitJAL(rd uint32, offset int64, insnSize uint64) {
    target := e.pc + uint64(offset)
    if rd != 0 {
        e.irEm.Const(e.xregDst(rd), int64(e.pc+insnSize))
    }
    e.advancePC(insnSize)

    if rd == 0 {
        origPC := e.pc - insnSize
        targetLabel := e.getOrCreateLabel(target)
        if target < origPC {
            e.irEm.BudgetCheck(targetLabel, target)
        } else {
            e.irEm.Jump(targetLabel)
        }
        e.gotoTargets[target] = true
        e.pc = target
        return
    }
    // rd != 0: call — exit block
    e.irEm.WriteBackAll()
    e.irEm.Ret(target, jitOK, ir.VRegZero)
    e.terminated = true
}
```

**emitJALR** — uses new `IRRetDyn`:
```go
func (e *emitter) emitJALR(rd, rs1 uint32, imm int64, insnSize uint64) {
    // Read rs1 BEFORE writing rd (aliasing!)
    target := e.irEm.Tmp()
    e.irEm.AddImm(target, e.xreg(rs1), imm)
    e.irEm.AndImm(target, target, ^int64(1))
    if rd != 0 {
        e.irEm.Const(e.xregDst(rd), int64(e.pc+insnSize))
    }
    e.advancePC(insnSize)
    e.irEm.WriteBackAll()
    e.irEm.RetDyn(target, jitOK, ir.VRegZero)
    e.terminated = true
}
```

**Edge case — register aliasing in JALR**: `rs1` must be read before `rd` is written (they may be the same register). The ordering above is correct: `xreg(rs1)` (read + lazy-load) happens before `xregDst(rd)` (write).

**Tests**: BEQ/BNE/BLT/BGE/BLTU/BGEU with forward and backward targets. JAL rd=0, JAL rd=1. JALR. Budget check with tight loop.

---

### Step 5: Floating-Point Arithmetic — emitFPOpS, emitFPOpD, emitFMA

**NaN-boxing helpers** (methods on `emitter`):

```go
// boxF32: NaN-box a 32-bit result into f[rd]
func (e *emitter) boxF32(rd uint32, val ir.VReg) {
    dst := e.fregDst(rd)
    masked := e.irEm.Tmp()
    e.irEm.AndImm(masked, val, 0xFFFFFFFF)
    hi := e.irEm.Tmp()
    e.irEm.Const(hi, int64(uint64(0xFFFFFFFF00000000)))
    e.irEm.Or(dst, masked, hi)
}

// unboxF32: extract 32-bit float bits, canonical NaN if not properly boxed
func (e *emitter) unboxF32(rs uint32) ir.VReg {
    src := e.freg(rs)
    upper := e.irEm.Tmp()
    e.irEm.ShrImm(upper, src, 32)
    check := e.irEm.Tmp()
    e.irEm.Const(check, 0xFFFFFFFF)
    result := e.irEm.Tmp()
    okLabel := e.irEm.NewLabel()
    doneLabel := e.irEm.NewLabel()
    e.irEm.Branch(upper, check, ir.EQ, okLabel)
    e.irEm.Const(result, 0x7FC00000) // canonical NaN
    e.irEm.Jump(doneLabel)
    e.irEm.PlaceLabel(okLabel)
    e.irEm.Zext(result, src, ir.I32)
    e.irEm.PlaceLabel(doneLabel)
    return result
}
```

**F32 arithmetic** (FADD.S, FSUB.S, FMUL.S, FDIV.S):
```go
a := e.unboxF32(rs1)  // 32-bit int bits
b := e.unboxF32(rs2)
result := e.irEm.Tmp()
e.irEm.FAdd(result, a, b, ir.F32)  // lowerer handles int↔XMM transfer
e.boxF32(rd, result)
```

**F64 arithmetic** (FADD.D, FSUB.D, FMUL.D, FDIV.D):
```go
a := e.freg(rs1)
b := e.freg(rs2)
dst := e.fregDst(rd)
e.irEm.FAdd(dst, a, b, ir.F64)
```

**FSQRT.S/D**: `e.irEm.FSqrt(result, a, F32/F64)` — the lowerer emits SQRTSS/SQRTSD.

**FMIN/FMAX**: Use `IRCall` to Go helper functions with proper NaN semantics.

**FMADD/FMSUB/FNMSUB/FNMADD**: Emit as `FMul` + `FAdd/FSub` (not fused). This matches TCC behavior. Example FMADD.S: `FMul(t, a, b, F32); FAdd(result, t, c, F32)`.

**FSGNJ/FSGNJN/FSGNJX**: Bit manipulation on raw register bits.
- FSGNJ.S: `s1 := unbox(rs1); s2 := unbox(rs2); t1 := AndImm(s1, 0x7FFFFFFF); t2 := AndImm(s2, 0x80000000); Or(result, t1, t2); boxF32(rd, result)`
- FSGNJN.S: Same but negate sign from rs2: `XorImm(t2, s2, 0x80000000); AndImm(t2, t2, 0x80000000)`
- FSGNJX.S: `XorImm` of the sign bits
- D variants: same pattern with 64-bit masks on raw FP register values

**Tests**: Test each FP op. Focus on NaN-boxing correctness — upper 32 bits must be 0xFFFFFFFF for F32 results.

---

### Step 6: FP Comparisons and Conversions

**FP comparisons** (emitFcmpS, emitFcmpD):
- FEQ.S: `a := unboxF32(rs1); b := unboxF32(rs2); FCmp(xregDst(rd), a, b, EQ, F32)`
- FLT.S: `FCmp(xregDst(rd), a, b, LT, F32)`
- FLE.S: `FCmp(xregDst(rd), a, b, LE, F32)`
- D variants: operate directly on 64-bit FP register values

**FP→Int conversions** (emitFcvtToIntS, emitFcvtToIntD):
| RISC-V | IR |
|---|---|
| FCVT.W.S (rs2=0) | `a := unboxF32(rs1); FCvtToI(t, a, F32, I32); Sext(xregDst(rd), t, I32)` |
| FCVT.WU.S (rs2=1) | `FCvtToU(t, a, F32, I32); Sext(xregDst(rd), t, I32)` |
| FCVT.L.S (rs2=2) | `FCvtToI(xregDst(rd), a, F32, I64)` |
| FCVT.LU.S (rs2=3) | `FCvtToU(xregDst(rd), a, F32, I64)` |

D variants: same pattern without unboxing.

**Int→FP conversions** (emitFcvtFromIntS, emitFcvtFromIntD):
| RISC-V | IR |
|---|---|
| FCVT.S.W (rs2=0) | `t := Tmp(); Sext(t, xreg(rs1), I32); FCvtFromI(result, t, I32, F32); boxF32(rd, result)` |
| FCVT.S.WU (rs2=1) | `Zext(t, xreg(rs1), I32); FCvtFromU(result, t, I32, F32); boxF32(rd, result)` |
| FCVT.S.L (rs2=2) | `FCvtFromI(result, xreg(rs1), I64, F32); boxF32(rd, result)` |
| FCVT.S.LU (rs2=3) | `FCvtFromU(result, xreg(rs1), I64, F32); boxF32(rd, result)` |

**FCVT.S.D / FCVT.D.S**:
- S.D: `a := freg(rs1); FCvtFF(result, a, F64, F32); boxF32(rd, result)`
- D.S: `a := unboxF32(rs1); FCvtFF(fregDst(rd), a, F32, F64)`

**FMV.X.W / FMV.W.X / FMV.X.D / FMV.D.X**:
- FMV.X.W: `Sext(xregDst(rd), freg(rs1), I32)` — sign-extend low 32 bits
- FMV.W.X: `boxF32(rd, xreg(rs1))`
- FMV.X.D: `Mov(xregDst(rd), freg(rs1))`
- FMV.D.X: `Mov(fregDst(rd), xreg(rs1))`

**FCLASS.S/D**: Keep bailing (`e.terminated = true`), same as current.

**Tests**: Each conversion variant. Round-trip tests (int→float→int). NaN input behavior.

---

### Step 7: RVC (Compressed Instructions)

Mostly mechanical — `emitRVC_Q0/Q1/Q2` delegate to the 32-bit emitters already ported in Steps 2-6.

The only RVC-specific C emission is:
- **C.LUI**: `e.emit("r%d = %dLL;\n", rd, uimm)` → `e.irEm.Const(e.xregDst(rd), uimm)`
- **C.LI**: already calls emitOpImm → no change
- **C.ADDI16SP**: calls emitOpImm → no change
- **C.EBREAK**: `e.emitReturn(e.pc, jitEbreak)` → same with IR

All other RVC ops (`C.ADD`, `C.LW`, `C.SW`, `C.BEQZ`, `C.J`, `C.LWSP`, `C.SDSP`, etc.) delegate to `emitOp`, `emitLoad`, `emitStore`, `emitBranch`, `emitJAL`, `emitJALR`.

**Tests**: Existing RVC fuzz tests as acceptance criteria. Unit tests for C.ADD, C.LW, C.SW, C.BEQZ, C.J.

---

### Step 8: Integration — IR Compilation Pipeline

**`jit_native.go`** (`//go:build !tcc`):
```go
func jitCompile(res *emitResult) (*compiledBlock, error) {
    pool := ir.AMD64Pool(res.block)
    alloc := ir.Allocate(res.block, pool, ...)
    ctx := goasm.New(goasm.AMD64)
    if err := ir.LowerAMD64(ctx, res.block, alloc); err != nil {
        return nil, err
    }
    code, err := ctx.Assemble()
    if err != nil { return nil, err }
    execMem, err := allocExec(len(code))
    if err != nil { return nil, err }
    copy(execMem, code)
    return &compiledBlock{fn: uintptr(unsafe.Pointer(&execMem[0])), backing: execMem}, nil
}
```

**`allocExec`**: Uses `syscall.Mmap` with `PROT_READ|PROT_WRITE|PROT_EXEC`.

**`compiledBlock`** under `!tcc`:
```go
type compiledBlock struct {
    fn      uintptr
    backing []byte  // prevents GC of mmap'd memory
}
```

**jit.go change**: Replace `tccCompile(res.csrc)` with `jitCompile(res)` — both defined per build tag:
- `jit_tcc.go`: `func jitCompile(res *emitResult) (*compiledBlock, error) { return tccCompile(res.csrc) }`
- `jit_native.go`: `func jitCompile(res *emitResult) (*compiledBlock, error) { return irCompile(res.block) }`

**Tests**: Run all `TestJIT_*` tests without `-tags tcc`. Run lockstep tests.

---

## Verification Plan

### Per-step verification
Each step runs: `go test -tags tcc ./...` (TCC path unbroken) and step-specific unit tests for the IR path.

### Full acceptance (after Step 8)
1. `go test -count=1 -run 'TestJIT_' -timeout 60s .` — all 23+ JIT unit tests pass
2. `go test -count=1 -run 'TestRISCVTests_Lockstep_U[IMAC]' -timeout 120s .` — all ~90 lockstep tests pass
3. `go test -count=1 -run 'TestRISCVTests_U[IMAC]_JIT' -timeout 60s .` — official riscv-tests pass
4. `go test -count=1 -v -run TestJIT_BenchGuest_Smoke -timeout 30s ./bench/` — 2.5B insn smoke test
5. `go test -tags tcc -run TestJIT_ADD .` — TCC fallback still works
6. `go test ./ir/...` — all 253+ IR unit tests pass

### Key correctness risks
- **Register aliasing in JALR**: rs1 read before rd write — ordering in Step 4 handles this
- **IC double-counting**: `icEmitted` flag protocol preserved exactly from C path
- **NaN-boxing**: upper 32 bits must be 0xFFFFFFFF — box/unbox helpers in Step 5
- **Misaligned access**: bail to interpreter — simpler than C path's inline byte-by-byte, rare in practice
- **MULH/MULHSU/MULHU**: No longer bail — IR+lowerer handle them. This is a correctness improvement

---

## Critical Files

| File | Lines | Role |
|------|-------|------|
| `jit_emit.go` | 1795 | Source of all translation patterns (read-only reference under `tcc` tag) |
| `ir/emit.go` | ~300 | Emitter API — target of all IR calls |
| `ir/highlevel.go` | ~200 | MaskedLoad, GuestStore, WriteBackAll, BudgetCheck, FaultExit |
| `ir/ir.go` | ~270 | IR type definitions — add IRRemU, IRRetDyn |
| `ir/lower_amd64.go` | ~1200 | AMD64 lowerer — add lowerRemU, lowerRetDyn |
| `jit.go` | ~175 | Dispatch loop — minor change to call jitCompile |
