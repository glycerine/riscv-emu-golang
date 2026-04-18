# Phase 4: AMD64 Lowering — Implementation Plan

## Context

Phases 1-3 are complete:
- **Phase 1** (`goasm/`): Extracted Go assembler backends, `goasm.Ctx` API, 20+ golden tests — all passing
- **Phase 2** (`ir/`): 44 IR ops, Emitter, peephole optimizer, 158 unit tests + 4 fuzz targets
- **Phase 3** (`ir/regalloc.go`): Extended Linear Scan allocator, 63 unit tests + 3 fuzz targets — all 221 ir/ tests pass

Phase 4 adds the **amd64 lowering layer**: converting an IR `Block` + `Allocation` into x86-64 machine code via the goasm `obj.Prog` API. This is the bridge from target-agnostic IR to executable native code.

**Goal**: Given a register-allocated IR block, emit correct x86-64 machine code for every IR op, handling spill/reload, label resolution, and special x86 constraints (implicit operands for DIV/MUL, CL for variable shifts, large immediates).

## Files to Create

1. `ir/lower_amd64.go` — Main lowering (~2000 LoC)
2. `ir/lower_amd64_test.go` — Tests (~1500 LoC)

## Architecture: AMD64 Register Convention

### JIT Block Entry (SysV AMD64 ABI from trampoline)

The Go trampoline (`call_amd64.s`) calls the JIT block with:
- RDI = sret buffer (JITResult: {pc, ic, status, faultAddr} at offsets 0/8/16/24)
- RSI = x[] pointer (guest integer registers)
- RDX = f[] pointer (guest FP registers)
- RCX = fcsr pointer
- R8 = mem_base
- R9 = mem_mask

### Prologue Register Shuffle

The JIT block prologue moves ABI args to callee-saved pinned registers:

```
MOV RSI → R12    ; x[] base
MOV RDX → R13    ; f[] base
MOV R8  → R14    ; mem_base
MOV R9  → R15    ; mem_mask
MOV RDI → RBX    ; sret buffer
; RCX (fcsr) → stored on stack [RSP + fcsrOffset] (rarely needed)
```

### Register Categories

| Category | Registers | Count | Notes |
|----------|-----------|-------|-------|
| **Pinned** | R12, R13, R14, R15, RBX, RSP | 6 | Never allocated. R12=x[], R13=f[], R14=memBase, R15=memMask, RBX=sret |
| **Scratch** | R10, R11 | 2 | Reserved for lowerer (spill/reload, large imm, DIV save/restore) |
| **Int pool** | RAX, RCX, RDX, RBP, RSI, RDI, R8, R9 | 8 | Available for register allocation |
| **Int pool (div/mul)** | RCX, RBP, RSI, RDI, R8, R9 | 6 | When block has DIV/MUL, RAX+RDX excluded |
| **FP pool** | XMM0-XMM15 | 16 | Available for FP register allocation |

### Pinned Parameter VRegs (from Emitter)

| Emitter field | VReg | Pinned host reg |
|---------------|------|-----------------|
| xBase | t64 | R12 |
| fBase | t65 | R13 |
| ic | t66 | (regular allocated, init 0) |
| memBase | t67 | R14 |
| memMask | t68 | R15 |

### Allocator Integration

```go
// Caller constructs pool and pinned map, then calls:
pool := AMD64Pool(block)                         // checks BlockHasDivMul
pinned := map[VReg]int16{t64: 0, t65: 1, ...}   // VReg → pool index
alloc := Allocate(block, pool, pinned, nil)
LowerAMD64(ctx, block, alloc)
```

The allocator assigns pool-relative indices (0, 1, 2, ...). The lowerer maps these to actual x86 register constants via lookup tables (`intRegMap`, `fpRegMap`).

## Core Data Structures

### `lowerCtx` — lowering state

```go
type lowerCtx struct {
    blk     *Block
    alloc   *Allocation
    c       *goasm.Ctx        // goasm assembler context
    idx     int               // current IR instruction index

    // Label resolution
    labelProg map[Label]*obj.Prog    // label → NOP Prog at that point
    pending   map[Label][]*obj.Prog  // forward-ref branches waiting for label

    // Register mapping: allocator host-reg ID → x86 register constant
    intRegMap []int16   // e.g., [0]=REG_AX, [1]=REG_CX, ...
    fpRegMap  []int16   // e.g., [0]=REG_X0, [1]=REG_X1, ...

    // Pinned registers (x86 constants)
    regXBase   int16   // R12
    regFBase   int16   // R13
    regMemBase int16   // R14
    regMemMask int16   // R15
    regSret    int16   // RBX
    scratch1   int16   // R10
    scratch2   int16   // R11

    // Frame layout
    stackSlots int     // from Allocation.StackSlots
    fcsrOffset int64   // stack offset for saved fcsr pointer
}
```

### Exported Functions

```go
// AMD64Pool returns the RegPool for amd64 lowering.
// If the block contains DIV/MUL ops, RAX and RDX are excluded.
func AMD64Pool(b *Block) RegPool

// AMD64Pinned returns the pinned VReg → host-reg-index map for parameter VRegs.
// The indices are into the RegPool arrays (not raw x86 register constants).
func AMD64Pinned() map[VReg]int16

// LowerAMD64 converts a register-allocated IR Block into x86-64 obj.Progs
// appended to ctx. After calling this, ctx.Assemble() produces native bytes.
func LowerAMD64(ctx *goasm.Ctx, b *Block, alloc *Allocation) error
```

## Helper Functions

### Register Resolution

```go
// hostReg returns the x86 register constant for VReg v at instruction idx.
// Looks up the IntervalAlloc containing idx and maps through intRegMap/fpRegMap.
func (lc *lowerCtx) hostReg(v VReg, idx int) int16

// use loads VReg v into a host register and returns that register.
// If v is in a register: returns its host register directly.
// If v is on stack: emits MOVQ spillSlot(RSP), scratch and returns scratch.
// scratchIdx selects which scratch register to use (0=R10, 1=R11).
func (lc *lowerCtx) use(v VReg, scratchIdx int) int16

// def returns the host register that VReg v should be written to.
// If v is in a register: returns its host register.
// If v is on stack: returns scratch (caller must call defCommit after).
func (lc *lowerCtx) def(v VReg) int16

// defCommit writes back to stack if VReg v is spilled.
// Called after the instruction that writes to def(v).
func (lc *lowerCtx) defCommit(v VReg, hostReg int16)
```

### Prog Emission Helpers

```go
// emitRR emits a register-register instruction: op src, dst
func (lc *lowerCtx) emitRR(op obj.As, src, dst int16)

// emitRI emits a register-immediate instruction: op $imm, dst
func (lc *lowerCtx) emitRI(op obj.As, imm int64, dst int16)

// emitRM emits a register-memory load: op base+disp, dst
func (lc *lowerCtx) emitRM(op obj.As, base int16, disp int64, dst int16)

// emitMR emits a memory store: op src, base+disp
func (lc *lowerCtx) emitMR(op obj.As, src int16, base int16, disp int64)

// emitRMX emits an indexed load: op base+idx*scale+disp, dst
func (lc *lowerCtx) emitRMX(op obj.As, base, idx int16, scale int8, disp int64, dst int16)

// emitJcc emits a conditional jump: Jcc → target prog
func (lc *lowerCtx) emitJcc(op obj.As, target *obj.Prog)

// emitJmp emits an unconditional jump
func (lc *lowerCtx) emitJmp(target *obj.Prog)

// emitNOP emits a NOP (used as label anchor)
func (lc *lowerCtx) emitNOP() *obj.Prog

// loadImm64 loads a 64-bit immediate into a register.
// Uses XORQ for 0, MOVL for uint32, MOVQ for everything else.
func (lc *lowerCtx) loadImm64(imm int64, dst int16)

// spillLoad emits MOVQ spillSlot(RSP), dst
func (lc *lowerCtx) spillLoad(slot int16, dst int16)

// spillStore emits MOVQ src, spillSlot(RSP)
func (lc *lowerCtx) spillStore(src int16, slot int16)
```

### Label Binding

```go
// bindLabel associates a branch Prog with an IR Label.
// If the label is already placed, sets p.To.SetTarget(target).
// Otherwise, queues p in the pending map for later resolution.
func (lc *lowerCtx) bindLabel(l Label, p *obj.Prog)

// placeLabel emits a NOP, records it as the label target,
// and resolves all pending forward references.
func (lc *lowerCtx) placeLabel(l Label)
```

## Per-Op Lowering (all 44 IR ops)

### Data Movement

| IR Op | x86-64 Sequence | Notes |
|-------|-----------------|-------|
| `IRMov` | `MOVQ a, dst` | Skipped if a == dst (peephole already handles) |
| `IRConst` imm=0 | `XORQ dst, dst` | Shorter encoding, auto-clears flags |
| `IRConst` imm fits uint32 | `MOVL $imm, dst` | Auto-zero-extends to 64-bit |
| `IRConst` imm fits int32 | `MOVQ $imm, dst` | Sign-extended |
| `IRConst` 64-bit | `MOVABSQ $imm, dst` | Full 64-bit immediate encoding |
| `IRSext` T=I32 | `MOVSLQ a, dst` | MOVSXD |
| `IRSext` T=I16 | `MOVSWQ a, dst` | |
| `IRSext` T=I8 | `MOVSBQ a, dst` | |
| `IRZext` T=I32 | `MOVL a, dst` | Auto-zeros upper 32 |
| `IRZext` T=I16 | `MOVZWQ a, dst` | |
| `IRZext` T=I8 | `MOVZBQ a, dst` | |

### Integer ALU

| IR Op | x86-64 Sequence | Notes |
|-------|-----------------|-------|
| `IRAdd` | `[MOVQ a,dst;] ADDQ b,dst` | Skip MOV if dst==a; commutative |
| `IRAddImm` +1 | `[MOVQ a,dst;] INCQ dst` | Shorter encoding |
| `IRAddImm` -1 | `[MOVQ a,dst;] DECQ dst` | |
| `IRAddImm` int32 | `[MOVQ a,dst;] ADDQ $imm,dst` | |
| `IRAddImm` 64-bit | `MOVQ $imm,scratch; [MOVQ a,dst;] ADDQ scratch,dst` | |
| `IRSub` | `MOVQ a,dst; SUBQ b,dst` | NOT commutative |
| `IRSubImm` | `[MOVQ a,dst;] SUBQ $imm,dst` | |
| `IRMul` | `[MOVQ a,dst;] IMULQ b,dst` | 2-operand IMUL; commutative |
| `IRNeg` | `[MOVQ a,dst;] NEGQ dst` | |

### DIV/MUL High (implicit RAX:RDX)

These all use scratch registers to save/restore RAX and RDX around the operation:

| IR Op | x86-64 Sequence |
|-------|-----------------|
| `IRDivS` | `MOVQ RAX→scratch1; MOVQ RDX→scratch2; MOVQ a→RAX; CQO; IDIVQ b; MOVQ RAX→dst; MOVQ scratch1→RAX; MOVQ scratch2→RDX` |
| `IRDivU` | Same but `XORQ RDX,RDX` instead of `CQO`, and `DIVQ` instead of `IDIVQ` |
| `IRRem` | Same as IRDivS but take result from RDX instead of RAX |
| `IRMulHS` | `save RAX/RDX; MOVQ a→RAX; IMULQ b (1-op); MOVQ RDX→dst; restore` |
| `IRMulHU` | Same but `MULQ b` (unsigned) |
| `IRMulHSU` | `save RAX/RDX; MOVQ a→RAX; MULQ b; adjustment if a<0; restore` |

**Key**: When BlockHasDivMul is true, RAX and RDX are excluded from the pool, so the save/restore of RAX/RDX is only needed if the lowerer uses them as temporaries for other purposes (they won't hold allocated VRegs).

**Simplification for Phase 4a**: When RAX/RDX are excluded from the pool, no save/restore needed — just use them directly as scratch for div/mul sequences. When they're in the pool (no div/mul in block), these ops won't appear, so no issue.

### Shifts

| IR Op | x86-64 Sequence | Notes |
|-------|-----------------|-------|
| `IRShlImm` | `[MOVQ a,dst;] SHLQ $imm,dst` | No CL issue with immediate |
| `IRShrImm` | `[MOVQ a,dst;] SHRQ $imm,dst` | |
| `IRSarImm` | `[MOVQ a,dst;] SARQ $imm,dst` | |
| `IRShl` | see below | Variable shift needs CL |
| `IRShr` | `MOVQ b→CX; [MOVQ a,dst;] SHRQ CL,dst` | |
| `IRSar` | `MOVQ b→CX; [MOVQ a,dst;] SARQ CL,dst` | |

**Variable shifts (IRShl/IRShr/IRSar)**: x86-64 requires shift amount in CL. Strategy:
1. If b is already in RCX: just shift
2. If RCX is free (not holding a live allocated VReg at this point): MOV b→CL, shift
3. If RCX holds a live VReg: `MOVQ RCX→scratch1; MOVQ b→CL; shift; MOVQ scratch1→RCX`

The lowerer checks `lc.isLiveIn(REG_CX, lc.idx)` to decide.

### Bitwise

| IR Op | x86-64 Sequence |
|-------|-----------------|
| `IRAnd` | `[MOVQ a,dst;] ANDQ b,dst` |
| `IRAndImm` 0xFFFFFFFF | `MOVL dst,dst` (zero-extend trick) |
| `IRAndImm` | `[MOVQ a,dst;] ANDQ $imm,dst` |
| `IROr` | `[MOVQ a,dst;] ORQ b,dst` |
| `IROrImm` | `[MOVQ a,dst;] ORQ $imm,dst` |
| `IRXor` | `[MOVQ a,dst;] XORQ b,dst` |
| `IRXorImm` | `[MOVQ a,dst;] XORQ $imm,dst` |
| `IRNot` | `[MOVQ a,dst;] NOTQ dst` |

### Comparison

| IR Op | x86-64 Sequence | Notes |
|-------|-----------------|-------|
| `IRSet` | `CMPQ b,a; SETcc dst_byte; MOVZBQ dst_byte,dst` | Pred → SETcc mapping below |
| `IRSetImm` | `CMPQ $imm,a; SETcc dst_byte; MOVZBQ dst_byte,dst` | |

Predicate → SETcc mapping:
- EQ → ASETEQ, NE → ASETNE
- LT → ASETLT, LE → ASETLE, GT → ASETGT, GE → ASETGE
- LTU → ASETCS, LEU → ASETLS, GTU → ASETHI, GEU → ASETCC

### Memory

| IR Op | x86-64 Sequence |
|-------|-----------------|
| `IRLoad` I64 | `MOVQ disp(base), dst` |
| `IRLoad` I32 signed | `MOVSLQ disp(base), dst` |
| `IRLoad` I32 unsigned | `MOVL disp(base), dst` |
| `IRLoad` I16 signed | `MOVSWQ disp(base), dst` |
| `IRLoad` I16 unsigned | `MOVZWQ disp(base), dst` |
| `IRLoad` I8 signed | `MOVSBQ disp(base), dst` |
| `IRLoad` I8 unsigned | `MOVZBQ disp(base), dst` |
| `IRLoad` F64 | `MOVSD disp(base), dst` |
| `IRLoad` F32 | `MOVSS disp(base), dst` |
| `IRStore` I64 | `MOVQ src, disp(base)` |
| `IRStore` I32 | `MOVL src, disp(base)` |
| `IRStore` I16 | `MOVW src, disp(base)` |
| `IRStore` I8 | `MOVB src, disp(base)` |
| `IRStore` F64 | `MOVSD src, disp(base)` |
| `IRStore` F32 | `MOVSS src, disp(base)` |
| `IRLoadX` | `MOVQ (base)(idx*scale), dst` | SIB addressing |
| `IRStoreX` | `MOVQ src, (base)(idx*scale)` | Note: Dst field is the value |

### Control Flow

| IR Op | x86-64 Sequence |
|-------|-----------------|
| `IRLabel` | `NOP` (anchor Prog); resolve pending forward branches |
| `IRBranch` | `CMPQ b,a; Jcc target` | Pred → Jcc mapping |
| `IRBranchImm` | `CMPQ $imm2,a; Jcc target` | Label in Imm, compare val in Imm2 |
| `IRJump` | `JMP target` | |
| `IRRet` | Write JITResult to sret buffer; epilogue; RET |
| `IRCall` | Save caller-saved; set up args; `MOVQ $addr,scratch; CALL scratch`; restore |

Predicate → Jcc mapping:
- EQ → AJEQ, NE → AJNE
- LT → AJLT, LE → AJLE, GT → AJGT, GE → AJGE
- LTU → AJCS, LEU → AJLS, GTU → AJHI, GEU → AJCC

### Block Return (IRRet)

```go
// IRRet fields: Imm=pc, Imm2=status, A=faultAddr
// JITResult struct at sret (RBX):
//   offset 0:  pc        (uint64)
//   offset 8:  ic        (uint64)  — from ic VReg
//   offset 16: status    (uint64)
//   offset 24: fault_addr (uint64)

emitRet:
    MOVQ $pc, 0(RBX)          // or loadImm64 + store
    MOVQ ic_reg, 8(RBX)       // ic VReg's host register
    MOVQ $status, 16(RBX)
    MOVQ faultAddr, 24(RBX)   // from VReg A (or 0 if VRegZero)
    // epilogue: restore frame, RET
```

### Floating Point

| IR Op | F64 instruction | F32 instruction |
|-------|----------------|-----------------|
| `IRFAdd` | `[MOVSD a,dst;] ADDSD b,dst` | `ADDSS` |
| `IRFSub` | `[MOVSD a,dst;] SUBSD b,dst` | `SUBSS` |
| `IRFMul` | `[MOVSD a,dst;] MULSD b,dst` | `MULSS` |
| `IRFDiv` | `[MOVSD a,dst;] DIVSD b,dst` | `DIVSS` |
| `IRFSqrt` | `SQRTSD a,dst` | `SQRTSS` |
| `IRFNeg` | `XORPD signMask,dst` | `XORPS` |
| `IRFAbs` | `ANDPD absMask,dst` | `ANDPS` |
| `IRFCmp` | `UCOMISD a,b; SETcc+fixup` | `UCOMISS` |

FP comparison requires special handling for NaN (unordered):
- EQ: `UCOMISD b,a; SETE+SETNP → AND → result`
- NE, LT, LE, etc.: each needs IEEE-correct condition code combination

### FP Conversions

| IR Op | x86-64 |
|-------|--------|
| `IRFCvtToI` F64→I64 | `CVTTSD2SIQ a, dst` |
| `IRFCvtToI` F64→I32 | `CVTTSD2SIL a, dst` |
| `IRFCvtToI` F32→I64 | `CVTTSS2SIQ a, dst` |
| `IRFCvtToI` F32→I32 | `CVTTSS2SIL a, dst` |
| `IRFCvtToU` | Signed cvt + fixup for large values (x86 lacks unsigned cvt) |
| `IRFCvtFromI` I64→F64 | `CVTSI2SDQ a, dst` |
| `IRFCvtFromI` I32→F64 | `CVTSI2SDL a, dst` |
| `IRFCvtFromI` I64→F32 | `CVTSI2SSQ a, dst` |
| `IRFCvtFromU` | Signed cvt + fixup for large values |
| `IRFCvtFF` F32→F64 | `CVTSS2SD a, dst` |
| `IRFCvtFF` F64→F32 | `CVTSD2SS a, dst` |

### Pseudo-ops

| IR Op | Action |
|-------|--------|
| `IRMarkLive` | No-op in lowering (allocator hint only) |
| `IRMarkDead` | No-op in lowering |
| `IRWriteback` | No-op (WriteBackAll already emits Store ops) |

## Prologue/Epilogue

### Prologue

```asm
;; 1. Move ABI args to pinned callee-saved regs
MOVQ RSI, R12          ; x[] → R12
MOVQ RDX, R13          ; f[] → R13
MOVQ R8, R14           ; mem_base → R14
MOVQ R9, R15           ; mem_mask → R15
MOVQ RDI, RBX          ; sret → RBX
;; fcsr (RCX) stored on stack if needed

;; 2. Allocate spill frame (if any)
SUBQ $(stackSlots*8 + fcsrSlot), RSP    ; only if stackSlots > 0
MOVQ RCX, fcsrOffset(RSP)               ; save fcsr pointer
```

### Epilogue (at each IRRet)

```asm
;; 1. Write JITResult fields to sret buffer (see IRRet above)
;; 2. Deallocate spill frame
ADDQ $(stackSlots*8 + fcsrSlot), RSP    ; only if stackSlots > 0
;; 3. RET (trampoline restores callee-saved regs)
RET
```

## TDD Workflow — 7 Phases (A-G)

### Phase A: Types, Stubs, Pool Definitions (compile, tests fail)

**`ir/lower_amd64.go`**: Define all types, pool construction, stub `LowerAMD64`.

**Tests (5)**:
1. `TestAMD64Pool_NoDiv` — 8 int regs, 16 FP regs
2. `TestAMD64Pool_WithDiv` — 6 int regs (no RAX/RDX), 16 FP regs
3. `TestAMD64Pinned` — correct VReg→index mapping for t64-t68
4. `TestLowerAMD64_EmptyBlock` — returns nil error, produces ATEXT+RET
5. `TestLowerAMD64_NilAlloc` — returns error

### Phase B: Prologue/Epilogue + Data Movement (tests 6-18)

Implement: prologue emission, epilogue, IRMov, IRConst, IRSext, IRZext.

**Tests**:
6. `TestLower_Prologue_NoSpills` — no SUB RSP
7. `TestLower_Prologue_WithSpills` — SUB RSP present
8. `TestLower_IRMov` — MOVQ encoding
9. `TestLower_IRConst_Zero` — XORQ encoding
10. `TestLower_IRConst_Int32` — MOVQ $imm encoding
11. `TestLower_IRConst_Uint32` — MOVL encoding
12. `TestLower_IRConst_Large` — MOVABSQ encoding
13. `TestLower_IRSext_I32` — MOVSLQ encoding
14. `TestLower_IRSext_I16` — MOVSWQ encoding
15. `TestLower_IRSext_I8` — MOVSBQ encoding
16. `TestLower_IRZext_I32` — MOVL encoding
17. `TestLower_IRZext_I16` — MOVZWQ encoding
18. `TestLower_IRZext_I8` — MOVZBQ encoding

### Phase C: Integer ALU + Bitwise (tests 19-38)

Implement: IRAdd, IRAddImm, IRSub, IRSubImm, IRMul, IRNeg, IRAnd, IRAndImm, IROr, IROrImm, IRXor, IRXorImm, IRNot.

**Tests**:
19. `TestLower_IRAdd_DstEqA` — single ADDQ
20. `TestLower_IRAdd_DstEqB` — single ADDQ (commutative)
21. `TestLower_IRAdd_Separate` — MOVQ+ADDQ
22. `TestLower_IRAddImm_Small` — ADDQ $imm
23. `TestLower_IRAddImm_Inc` — INCQ for imm=1
24. `TestLower_IRAddImm_Dec` — DECQ for imm=-1
25. `TestLower_IRAddImm_Large` — scratch + ADDQ for >32-bit imm
26. `TestLower_IRSub` — MOVQ+SUBQ
27. `TestLower_IRSubImm` — SUBQ $imm
28. `TestLower_IRMul_DstEqA` — IMULQ
29. `TestLower_IRMul_Separate` — MOVQ+IMULQ
30. `TestLower_IRNeg` — NEGQ
31. `TestLower_IRAnd` — ANDQ
32. `TestLower_IRAndImm` — ANDQ $imm
33. `TestLower_IROr` — ORQ
34. `TestLower_IROrImm` — ORQ $imm
35. `TestLower_IRXor` — XORQ
36. `TestLower_IRXorImm` — XORQ $imm
37. `TestLower_IRNot` — NOTQ
38. `TestLower_SpilledOperand` — verify spill load/store around ALU op

### Phase D: DIV/MUL High + Shifts (tests 39-50)

Implement: IRDivS, IRDivU, IRRem, IRMulHS, IRMulHU, IRShl, IRShlImm, IRShr, IRShrImm, IRSar, IRSarImm.

**Tests**:
39. `TestLower_IRDivS` — CQO+IDIVQ+result from RAX
40. `TestLower_IRDivU` — XORQ RDX,RDX + DIVQ
41. `TestLower_IRRem` — IDIVQ+result from RDX
42. `TestLower_IRMulHS` — 1-operand IMULQ + result from RDX
43. `TestLower_IRMulHU` — 1-operand MULQ + result from RDX
44. `TestLower_IRShlImm` — SHLQ $imm
45. `TestLower_IRShrImm` — SHRQ $imm
46. `TestLower_IRSarImm` — SARQ $imm
47. `TestLower_IRShl_RegShift` — MOVQ b→CL + SHLQ CL
48. `TestLower_IRShr_RegShift` — SHRQ CL
49. `TestLower_IRSar_RegShift` — SARQ CL
50. `TestLower_IRShl_CXLive` — save/restore CX around shift

### Phase E: Comparison + Memory (tests 51-66)

Implement: IRSet, IRSetImm, IRLoad, IRStore, IRLoadX, IRStoreX.

**Tests**:
51. `TestLower_IRSet_EQ` — CMPQ+SETEQ+MOVZBQ
52. `TestLower_IRSet_LT` — CMPQ+SETLT+MOVZBQ
53. `TestLower_IRSet_GEU` — CMPQ+SETCC+MOVZBQ
54. `TestLower_IRSetImm_NE` — CMPQ $imm+SETNE+MOVZBQ
55. `TestLower_IRLoad_I64` — MOVQ mem
56. `TestLower_IRLoad_I32_Signed` — MOVSLQ mem
57. `TestLower_IRLoad_I32_Unsigned` — MOVL mem
58. `TestLower_IRLoad_I8` — MOVZBQ / MOVSBQ
59. `TestLower_IRLoad_F64` — MOVSD mem
60. `TestLower_IRStore_I64` — MOVQ to mem
61. `TestLower_IRStore_I32` — MOVL to mem
62. `TestLower_IRStore_I8` — MOVB to mem
63. `TestLower_IRStore_F64` — MOVSD to mem
64. `TestLower_IRLoadX_SIB` — indexed load with scale
65. `TestLower_IRStoreX_SIB` — indexed store with scale
66. `TestLower_IRLoad_SpilledBase` — base VReg on stack

### Phase F: Control Flow + Block Exit (tests 67-80)

Implement: IRLabel, IRBranch, IRBranchImm, IRJump, IRRet, IRCall.

**Tests**:
67. `TestLower_IRLabel_PlacedBefore` — NOP anchor
68. `TestLower_IRBranch_ForwardRef` — Jcc with pending resolution
69. `TestLower_IRBranch_BackwardRef` — Jcc with known target
70. `TestLower_IRBranch_EQ` — CMPQ+JEQ
71. `TestLower_IRBranch_LTU` — CMPQ+JCS
72. `TestLower_IRBranchImm_GE` — CMPQ $imm+JGE
73. `TestLower_IRJump_Forward` — JMP with pending label
74. `TestLower_IRJump_Backward` — JMP with known label
75. `TestLower_IRRet_Const` — write sret + RET encoding
76. `TestLower_IRRet_WithFaultAddr` — VReg A stored to sret+24
77. `TestLower_IRRet_WithIC` — ic VReg stored to sret+8
78. `TestLower_IRCall_Simple` — save/call/restore sequence
79. `TestLower_MultipleRets` — each has its own epilogue
80. `TestLower_BranchLoop` — forward+backward branch integration

### Phase G: FP Ops + Conversions (tests 81-100)

Implement: IRFAdd, IRFSub, IRFMul, IRFDiv, IRFSqrt, IRFNeg, IRFAbs, IRFCmp, IRFCvtToI, IRFCvtToU, IRFCvtFromI, IRFCvtFromU, IRFCvtFF.

**Tests**:
81. `TestLower_IRFAdd_F64` — ADDSD
82. `TestLower_IRFAdd_F32` — ADDSS
83. `TestLower_IRFSub_F64` — SUBSD
84. `TestLower_IRFMul_F64` — MULSD
85. `TestLower_IRFDiv_F64` — DIVSD
86. `TestLower_IRFSqrt_F64` — SQRTSD
87. `TestLower_IRFSqrt_F32` — SQRTSS
88. `TestLower_IRFNeg_F64` — XORPD with sign mask
89. `TestLower_IRFAbs_F64` — ANDPD with abs mask
90. `TestLower_IRFCmp_EQ_F64` — UCOMISD + SETE+SETNP combo
91. `TestLower_IRFCmp_LT_F64` — UCOMISD + SETA (reversed comparison)
92. `TestLower_IRFCvtToI_F64_I64` — CVTTSD2SIQ
93. `TestLower_IRFCvtToI_F64_I32` — CVTTSD2SIL
94. `TestLower_IRFCvtToI_F32_I64` — CVTTSS2SIQ
95. `TestLower_IRFCvtFromI_I64_F64` — CVTSI2SDQ
96. `TestLower_IRFCvtFromI_I32_F64` — CVTSI2SDL
97. `TestLower_IRFCvtFromU_I64_F64` — signed cvt + fixup
98. `TestLower_IRFCvtFF_F32_F64` — CVTSS2SD
99. `TestLower_IRFCvtFF_F64_F32` — CVTSD2SS
100. `TestLower_FP_SpilledXMM` — FP VReg on stack

### Phase H: Execution Tests (tests 101-108)

These tests mmap+execute the generated code and verify results (following the pattern in `goasm/api_test.go`).

**Tests**:
101. `TestExec_AddConst` — emit IRConst+IRAdd, execute, verify result
102. `TestExec_SubMul` — multi-op chain
103. `TestExec_LoadStore` — load from memory, add, store back
104. `TestExec_BranchLoop` — simple counted loop
105. `TestExec_Spilled` — enough VRegs to force spills, verify correct results
106. `TestExec_DivRem` — division + remainder
107. `TestExec_FPAdd` — floating-point addition
108. `TestExec_RetStatus` — verify JITResult fields written correctly

### Phase I: Pseudo-ops + Edge Cases + Register Moves (tests 109-115)

**Tests**:
109. `TestLower_IRMarkLive_NoOp` — no code emitted
110. `TestLower_IRMarkDead_NoOp` — no code emitted
111. `TestLower_IRWriteback_NoOp` — no code emitted (stores already emitted)
112. `TestLower_VRegZero_NoEmit` — VRegZero operands produce no load
113. `TestLower_AllOpsHandled` — every IROp has a case in lowerInstr
114. `TestLower_RegMoves` — Allocation.Moves inserted as MOVQ at correct positions
115. `TestLower_PinnedRegsCorrect` — pinned VRegs use their pinned host regs

### Phase J: Fuzz Testing (3 fuzz targets)

**`FuzzLowerAMD64_NoPanic`**: Random IR blocks (4-byte tuples following fuzz_test.go pattern). Run Allocate + LowerAMD64 + Assemble. Assert:
1. No panic
2. LowerAMD64 returns nil error for valid blocks
3. Assemble returns non-empty bytes (or error, but no panic)

**`FuzzLowerAMD64_RoundtripInvariants`**: Random IR, lower, verify:
1. All labels referenced by branches are resolved
2. Output byte count > 0
3. No unresolved pending labels after lowering

**`FuzzLowerAMD64_ExecSafe`**: Small random blocks (5-10 instrs, no memory ops, no calls). Mmap+execute. Assert no crash (SIGSEGV etc). Don't verify results — just safety.

## Critical Design Decisions

1. **Callee-saved pinning**: x[], f[], memBase, memMask pinned to R12-R15 (callee-saved). Survives C calls naturally. Trampoline already saves these.

2. **Scratch registers R10/R11**: Never allocated. Available for spill/reload, large immediates, DIV save/restore, and CX swap around shifts. Two is sufficient since no IR op needs more than 2 scratch simultaneously.

3. **Allocator-relative register IDs**: The allocator produces indices (0,1,2...) into the RegPool. The lowerer maps these to x86 constants via lookup tables. This keeps the allocator arch-agnostic.

4. **DIV/MUL: pool exclusion, not save/restore**: When `BlockHasDivMul()`, RAX/RDX removed from pool so they're never allocated → div/mul can use them freely with no save/restore.

5. **Variable shifts: CX save/restore**: Rather than BMI2 (which requires CPUID check and isn't supported by all Go assembler versions), use the traditional CL-based approach with save/restore when CX holds a live value.

6. **Label resolution: pending map**: Forward branches store their Prog in a pending list. When the label is placed, all pending Progs are patched. Simple, O(1) per resolution.

7. **Spill slot addressing**: `spillSlot(RSP)` where slot offset = `slot * 8`. Prologue allocates frame with `SUB RSP, frameSize`.

8. **FP sign/abs**: Use XORPD/ANDPD with pre-loaded constant masks. The masks are loaded into scratch XMM register (or a dedicated constant pool). For Phase 4a, load mask from immediate into GPR scratch → MOVQ scratch, XMM.

9. **Unsigned FP conversions**: No direct x86 instruction for uint64↔float. Use signed conversion with range-check fixup: if value >= 2^63, subtract 2^63, convert, add 2^63.0 back.

10. **IRCall**: Conservative save-all for Phase 4a. Only sqrtf/sqrt are called from JIT blocks. Profile-guided optimization in Phase 4b if needed.

## Critical Files

- `ir/ir.go` — VReg, IRInstr, Block, IROp (input types) — lines 1-344
- `ir/emit.go` — Emitter with param VRegs t64-t68 — lines 1-300
- `ir/regalloc.go` — Allocation struct, Allocate(), BlockHasDivMul() — lines 1-500+
- `ir/highlevel.go` — MaskedLoad, GuestStore, WriteBackAll patterns — lines 1-133
- `goasm/api.go` — Ctx, NewProg, Append, Assemble — lines 1-299
- `goasm/regs.go` — REG_AMD64_* constants — lines 1-80
- `goasm/api_test.go` — Test helpers (immReg, regReg, memLoad, assertBytes, mmap/exec patterns)

## Verification

```bash
# Unit tests
go test -v -run 'TestLower_|TestExec_|TestAMD64' ./ir/

# Fuzz tests
go test -fuzz FuzzLowerAMD64_NoPanic -fuzztime 60s ./ir/
go test -fuzz FuzzLowerAMD64_RoundtripInvariants -fuzztime 30s ./ir/
go test -fuzz FuzzLowerAMD64_ExecSafe -fuzztime 30s ./ir/

# All ir tests (regression)
go test -v ./ir/

# Goasm tests (ensure no regression)
go test -v ./goasm/...
```
