# Phase 2: IR Definition Layer

## Context

The RISC-V emulator's JIT currently emits C source text, compiled by TCC via cgo. Phase 2 creates a target-agnostic intermediate representation (`ir/` package) that will replace the C text emission. This IR sits between the RISC-V decoder and future architecture-specific lowering (Phases 3-4). All files go in `/Users/jaten/ris/ir/`.

Module: `riscv` (from `/Users/jaten/ris/go.mod`). The `ir/` directory does not yet exist.

---

## Files to Create (10 total: 5 source + 5 test)

### Implementation Order (dependency chain)

```
1. ir/ir.go           -- types only, no deps
2. ir/emit_impl.go    -- depends on ir.go (private template methods + emit())
3. ir/peephole.go     -- depends on ir.go (called by emit())
4. ir/emit.go         -- depends on emit_impl.go + peephole.go (public API)
5. ir/highlevel.go    -- depends on emit.go (multi-instruction patterns)
```

Tests written alongside each file. All files use `package ir`.

---

## File 1: `ir/ir.go` — Core Types

**Types and constants:**

- `VReg` (uint16): virtual register. `VRegZero=0`, VRegs 1-31 = guest x1-x31, 32-63 = guest f0-f31, `VRegTempStart=64`+
- `Type` (uint8): `I8, I16, I32, I64, F32, F64`
- `Pred` (uint8): `EQ, NE, LT, LE, GT, GE, LTU, LEU, GTU, GEU`
- `Label` (uint16): jump target ID within a block
- `IROp` (uint8): ~50 operations enumerated below
- `IRInstr` struct: fixed-size `{Op IROp, T Type, U Type, Dst VReg, A VReg, B VReg, Imm int64, Imm2 int64, Pred Pred, Scale uint8}`
- `Block` struct: `{Instrs []IRInstr, Labels map[Label]int, NextLabel Label, CTab []CSym, VRegLive []VRegLiveness}`
- `CSym` struct: `{Name string, Addr uintptr}`
- `VRegLiveness` struct: `{Start, End int}`

**IROp enumeration (grouped):**

```
IROpInvalid = 0 (sentinel)

Memory:     IRLoad, IRStore, IRLoadX, IRStoreX
Int ALU:    IRAdd, IRAddImm, IRSub, IRSubImm, IRMul, IRDivS, IRDivU, IRRem,
            IRMulHS, IRMulHU, IRMulHSU, IRNeg,
            IRShl, IRShlImm, IRShr, IRShrImm, IRSar, IRSarImm
Bitwise:    IRAnd, IRAndImm, IROr, IROrImm, IRXor, IRXorImm, IRNot
Compare:    IRSet, IRSetImm
Data move:  IRMov, IRConst, IRSext, IRZext
Control:    IRLabel, IRBranch, IRBranchImm, IRJump, IRCall, IRRet
FP:         IRFAdd, IRFSub, IRFMul, IRFDiv, IRFSqrt, IRFCmp, IRFNeg, IRFAbs,
            IRFCvtToI, IRFCvtToU, IRFCvtFromI, IRFCvtFromU, IRFCvtFF
Pseudo:     IRMarkLive, IRMarkDead, IRWriteback
```

**Helper functions:**
- `NewBlock() *Block` — allocates Labels map
- `widthToType(width int) Type` — 1->I8, 2->I16, 4->I32, 8->I64
- `(t Type) Size() int` — returns byte count
- `String()` methods on VReg, Type, Pred, IROp, IRInstr (for debug/test output)

**IRInstr field mapping conventions:**
- Load: `{Dst: result, A: base, Imm: offset}`
- Store: `{A: base, B: value, Imm: offset}` (Dst unused, set VRegZero)
- LoadX/StoreX: same + `Scale` field for index scaling
- Ret: `{Imm: pc, Imm2: status, A: faultAddr}`
- Branch: `{A, B: compared regs, Pred, Imm: label}`
- BranchImm: `{A: reg, Imm: label, Imm2: immediate value, Pred}`
- Conversion ops: `T` = destination type, `U` = source type

---

## File 2: `ir/emit_impl.go` — Internal Helpers

Private template methods called by all public emitters. Central `emit()` function.

**Methods on `*Emitter`:**
- `op3(op, t, dst, a, b)` — 3-reg ops. Skip if dst==VRegZero. Calls emit() + MarkDirty.
- `op2i(op, t, dst, a, imm)` — reg+imm ops. Same VRegZero guard.
- `op2(op, t, dst, a)` — 2-reg ops.
- `opConst(dst, imm)` — IRConst.
- `opSet(op, dst, a, b, pred)` — comparison ops.
- `opSetImm(op, dst, a, imm, pred)` — comparison with imm.
- `opExt(op, dst, a, fromT)` — sign/zero extend. T = source width.
- `emit(ins IRInstr)` — append to Block.Instrs, register label if IRLabel, run peephole loop.

**Key behavior of `emit()`:**
```go
func (e *Emitter) emit(ins IRInstr) {
    e.Block.Instrs = append(e.Block.Instrs, ins)
    if ins.Op == IRLabel {
        e.Block.Labels[Label(ins.Imm)] = len(e.Block.Instrs) - 1
    }
    for e.tryPeephole() {}
}
```

---

## File 3: `ir/peephole.go` — Online Sliding-Window Optimizer

Called by `emit()` after every append. Looks at last N instructions, rewrites if pattern matches.

**Tunable constant:** `const PeepholeSz = 4` — sliding window depth, exported so it can be tuned for performance vs compile speed tradeoff. Starts at 4.

**Patterns (checked in order):**

| # | Pattern | Rewrite |
|---|---------|---------|
| 1 | `IRMov dst,dst` (self-move) | Delete |
| 2 | `IRAddImm dst,a,0` | Delete if dst==a, else IRMov |
| 3 | `IRShlImm dst,a,0` | Delete if dst==a, else IRMov |
| 4 | `IRShrImm dst,a,0` | Same |
| 5 | `IRSarImm dst,a,0` | Same |
| 6 | `IRAndImm dst,a,-1` | Delete if dst==a, else IRMov |
| 7 | `IROrImm dst,a,0` | Delete if dst==a, else IRMov |
| 8 | `IRXorImm dst,a,0` | Delete if dst==a, else IRMov |
| 9 | `IRConst tmp,0` + `IRStore ...,tmp,...` | Fold to IRStore with A=VRegZero (only for temps) |

Patterns cascade: pattern 2 can produce an IRMov that then matches pattern 1.

**Helper:** `vregUsedLater(vr, startIdx)` — conservative: true for guest regs (< VRegTempStart), false for temps.

---

## File 4: `ir/emit.go` — Public Emitter API

**Emitter struct:**
```go
type Emitter struct {
    Block   *Block
    dirty   []bool   // dirty[vr] = written but not written back
    nextTmp VReg     // starts at VRegTempStart (64)
    xBase   VReg     // param: pointer to x[32] array
    fBase   VReg     // param: pointer to f[32] array
    ic      VReg     // param: instruction counter
    memBase VReg     // param: guest memory base
    memMask VReg     // param: guest memory mask
}
```

**Constructor:** `NewEmitter() *Emitter` — allocates Block, dirty slice (128 initial), sets nextTmp=64, pre-allocates param VRegs (xBase, fBase, ic, memBase, memMask) as the first 5 temps.

**Public methods (one per IR op):**

Integer ALU: `Add, AddT, AddImm, Sub, SubImm, Mul, DivS, DivU, Rem, MulHS, MulHU, MulHSU, Neg, Shl, ShlImm, Shr, ShrImm, Sar, SarImm`

Bitwise: `And, AndImm, Or, OrImm, Xor, XorImm, Not`

Compare: `Set(dst, a, b, pred), SetImm(dst, a, imm, pred)`

Data movement: `Mov, Const, Sext, Zext`

Memory:
- `Load(dst, base, imm, t, signed)` — emits IRLoad, then IRSext or IRZext if t < I64
- `Store(base, imm, src, t)` — emits IRStore
- `LoadX(dst, base, idx, scale, t, signed)` — indexed load
- `StoreX(base, idx, scale, src, t)` — indexed store

Control flow:
- `NewLabel() Label` — allocate without placing
- `PlaceLabel(l)` — emit IRLabel for previously allocated label
- `Branch(a, b, pred, target)` — conditional branch
- `BranchImm(a, imm, pred, target)` — compare-immediate branch
- `Jump(target)` — unconditional jump
- `Call(sym, addr) int` — register external symbol, emit IRCall
- `Ret(pc, status, faultAddr)` — emit IRRet

FP: `FAdd, FSub, FMul, FDiv, FSqrt, FCmp, FNeg, FAbs, FCvtToI, FCvtToU, FCvtFromI, FCvtFromU, FCvtFF`

VReg management:
- `Tmp() VReg` — allocate fresh temp, grows dirty slice if needed
- `XReg(i uint32) VReg` — guest x0-x31 (panics if i>31)
- `FRegV(i uint32) VReg` — guest f0-f31 (panics if i>31)

---

## File 5: `ir/highlevel.go` — High-Level Helpers

Multi-instruction patterns built on low-level emitters.

**Constant:** `MaxIC = 4096` (backward-branch budget limit)

**Methods:**

- **`MaskedLoad(dst, base, memBase, mask VReg, off int64, width int, signed bool, faultLabel Label)`**
  Sequence: addr=base+off, OOB check via `(addr|(addr+width-1)) & ~mask != 0`, branch to faultLabel, masked deref via `*(base+(addr&mask))`, load with sign/zero extend.

- **`GuestStore(base, memBase, mask VReg, off int64, src VReg, width int, faultLabel Label)`**
  Same OOB check, then store.

- **`WriteBackAll()`** — iterate dirty[1..31] storing to xBase, dirty[32..63] storing to fBase.

- **`WriteBackReg(vr VReg)`** — single vreg writeback, marks clean.

- **`FaultExit(pc uint64, status int, faultAddr VReg)`** — WriteBackAll + Ret.

- **`BudgetCheck(target Label, targetPC uint64)`** — BranchImm(ic >= MaxIC -> exit), Jump(target), place exit label with WriteBackAll + Ret.

- **`MarkDirty(vr VReg)`** — set dirty[vr]=true (no-op for VRegZero).

- **`IsDirty(vr VReg) bool`** — query dirty state.

---

## Test Files

### `ir/ir_test.go`
- VReg, Type, Pred, IROp constant values and distinctness
- IRInstr zero value is IROpInvalid
- NewBlock() returns initialized struct
- String() methods produce expected output
- widthToType() and Type.Size() correctness
- XReg/FReg boundary checks (via the helper functions, not Emitter methods)

### `ir/emit_impl_test.go`
- op3, op2i, op2, opConst, opSet, opSetImm, opExt: each appends correct IRInstr
- VRegZero writes: each template skips emission entirely
- emit() registers labels in Block.Labels
- emit() triggers peephole (emit IRMov self -> deleted)
- MarkDirty called on successful emission

### `ir/peephole_test.go`
- Each of the 9 patterns: verify rewrite fires
- Cascade: AddImm(dst,dst,0) -> Mov(dst,dst) -> deleted (two cascaded rewrites)
- Non-matching cases preserved (AddImm with non-zero imm, etc.)
- Const+Store fold: only for temps, not guest regs; only for imm=0
- vregUsedLater: true for guest regs, false for temps

### `ir/emit_test.go`
- Every public method: call once, inspect Block.Instrs for correct Op/T/Dst/A/B/Imm
- VRegZero discard: Add(VRegZero,...) -> empty Instrs
- Tmp() returns monotonically increasing VRegs starting at 64
- XReg/FRegV range checks (panic on >31)
- Load with signed sub-I64: produces IRLoad + IRSext (2 instructions)
- Load with unsigned sub-I64: produces IRLoad + IRZext
- Load I64: just IRLoad (no extension)
- Label/NewLabel/PlaceLabel: label allocation and Block.Labels mapping
- Branch, BranchImm, Jump: correct Pred/Imm encoding
- Call: registers in CTab, returns index
- Ret: Imm=pc, Imm2=status, A=faultAddr
- FP ops: each with F32 and F64, verify T field
- FCvt: verify T and U fields
- Dirty tracking: writes mark dirty, reads don't

### `ir/highlevel_test.go`
- MaskedLoad: verify instruction sequence (addr calc, OOB check, masked deref, load)
- GuestStore: same structure with Store instead of Load
- WriteBackAll with nothing dirty -> no stores emitted
- WriteBackAll with some dirty -> correct stores to xBase/fBase at proper offsets
- WriteBackReg: single store, clears dirty
- FaultExit: WriteBackAll sequence + IRRet
- BudgetCheck: BranchImm + Jump + label + WriteBackAll + Ret
- MarkDirty/IsDirty: basic behavior, VRegZero no-op, dynamic growth past initial slice
- End-to-end: simulate a mini RISC-V block (ADDI + SW + ECALL) and verify full IR sequence

---

## Verification

```bash
# After each file pair (source + test), run:
cd /Users/jaten/ris && go test -v ./ir/

# Full pass after all files written:
cd /Users/jaten/ris && go test -v -count=1 ./ir/

# Verify no compilation issues with the rest of the module:
cd /Users/jaten/ris && go build ./...
```

---

## Key Design Decisions

1. **IRInstr is fixed-size** — no slices/maps/pointers inside. Cache-friendly for lowering passes.
2. **VRegZero writes are no-ops** — emit_impl silently discards, matching RISC-V x0 semantics.
3. **Peephole is online** — runs after every append via emit(), no separate pass needed.
4. **IRStore uses A=base, B=value** — Dst is unused (VRegZero) since stores have no register result.
5. **IRRet uses Imm=pc, Imm2=status** — status is always a compile-time constant, not a register.
6. **Label split: NewLabel() + PlaceLabel()** — supports forward branches where target is placed later.
7. **Param VRegs** — xBase, fBase, ic, memBase, memMask are pre-allocated temps in NewEmitter().
8. **MaxIC = 4096** — exported constant for backward-branch budget, matching current jit_emit.go.
