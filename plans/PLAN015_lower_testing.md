# Fix + Clean-Room Rewrite of AMD64 IR Lowerer

## Context

`ir/lower_amd64.go` (1534 lines) converts register-allocated IR to x86-64 machine code. Code review found 3 confirmed bugs, all caused by ad-hoc register conflict resolution. The current design makes each lowering function independently handle aliasing between source operands, destinations, and implicit x86 registers (CX for shifts, RAX:RDX for division). This is error-prone.

Two-pass approach:
- **Pass A**: Fix the 3 confirmed bugs in the existing lowerer
- **Pass B**: Clean-room rewrite (`lower_amd64_v2.go`) using an "always-stage" approach that makes aliasing bugs structurally impossible
- **Pass C**: Randomized lockstep testing comparing V1 vs V2

## Pass A: Bug Fixes in `lower_amd64.go`

### BUG 1: lowerShift — dst==CX save/restore overwrites result
**File**: `ir/lower_amd64.go:839`

When the allocator assigns the shift destination to CX, `isCXLive()` returns true (because dst's own interval includes this instruction). The lowerer saves old CX, computes the shift, writes result to CX, then **restores old CX — destroying the result**.

```go
// BEFORE (line 839):
needCXSave := b != goasm.REG_AMD64_CX && lc.isCXLive()

// AFTER:
needCXSave := b != goasm.REG_AMD64_CX && dst != goasm.REG_AMD64_CX && lc.isCXLive()
```

### BUG 2: lowerBinopImm — large immediate clobbers operand
**File**: `ir/lower_amd64.go:700-704`

When A is stack-allocated, `use(A, 1)` returns R11 (scratch2). For >32-bit immediates, the code loads into R11 (hardcoded), clobbering A.

```go
// BEFORE (line 702):
scr := amd64Scratch2

// AFTER:
scr := amd64Scratch2
if dst == amd64Scratch2 {
    scr = amd64Scratch1
}
```

### BUG 3: lowerFCmp EQ/NE — scratch conflict when dst is spilled
**File**: `ir/lower_amd64.go:1413, 1427`

When Dst is stack-allocated, `def()` returns R10 (scratch0). Then `bReg = byteReg(R10)` and `scrByte = byteReg(amd64Scratch1) = byteReg(R10)` — same register. SETE and SETNP write the same byte, making the AND a no-op.

```go
// AFTER (line 1413 and 1427):
scrByte := byteReg(amd64Scratch1)
if dst == amd64Scratch1 {
    scrByte = byteReg(amd64Scratch2)
}
```

### Bug fix tests
Add to `ir/lower_amd64_test.go`:
- `TestLower_ShiftDstCX`: Force shift destination into CX via register pressure
- `TestLower_BinopImmLargeStack`: Stack-allocated VReg + >32-bit immediate
- `TestLower_FCmpEQ_DstStack`: FCmp EQ with stack-allocated destination

## Pass B: Clean-Room `lower_amd64_v2.go`

### Core Design: "Always-Stage"

Every instruction stages ALL source operands into fixed staging registers BEFORE touching any destination. Staging registers are never in the allocation pool, so aliasing is impossible by construction.

| Role | Integer | FP |
|------|---------|-----|
| Staging A | R10 (scratch1) | XMM15 |
| Staging B | R11 (scratch2) | XMM14 |

### Files

| File | Action | Lines |
|------|--------|-------|
| `ir/lower_amd64_v2.go` | Create | ~1200 |
| `ir/lower_amd64_v2_test.go` | Create | ~400 |
| `lockstep_v1v2_test.go` | Create | ~300 |

### API

```go
func AMD64Pool_V2(b *Block) RegPool    // same int pool, FP pool minus XMM14/XMM15
func LowerAMD64_V2(ctx *goasm.Ctx, b *Block, alloc *Allocation) error
```

### Key Helpers

```go
type lowerCtxV2 struct {
    // Same fields as lowerCtx — no embedding (prevents accidental V1 method calls)
    blk *Block; alloc *Allocation; c *goasm.Ctx; idx int
    labelProg map[Label]*obj.Prog; pending map[Label][]*obj.Prog
    stackSlots int; frameSize int64
}

// stageInt: ALWAYS loads VReg into R10 (idx=0) or R11 (idx=1). Never returns an allocated reg.
func (lc *lowerCtxV2) stageInt(v VReg, idx int) int16

// writeDst: returns allocated reg for Dst, or R10 if spilled (safe because sources consumed).
func (lc *lowerCtxV2) writeDst(v VReg) int16

// commitDst: spills back to stack if needed.
func (lc *lowerCtxV2) commitDst(v VReg, hostReg int16)
```

### Lowering Patterns

**Binary op** (Add, Sub, And, Or, Xor, Mul):
```
stageInt(A, 0) → R10
stageInt(B, 1) → R11
OP R11, R10          (result in R10)
MOV R10, dst         (if dst != R10)
commitDst
```

**BinopImm** (AddImm, etc.):
```
stageInt(A, 0) → R10
if imm fits 32 bits: OP $imm, R10
else: loadImm64(imm, R11); OP R11, R10
MOV R10, dst
commitDst
```

**Shift** (Shl, Shr, Sar):
```
stageInt(A, 0) → R10      (value)
stageInt(B, 1) → R11      (count)
dst = writeDst(Dst)
if isCXLive() && dst != CX:
    XCHG R11, CX           (count → CX, old CX → R11 for restore)
else:
    MOV R11, CX
SHL/SHR/SAR CL, R10
MOV R10, dst
if saved: MOV R11, CX      (restore)
commitDst
```

**Division** (DivS, DivU, Rem, RemU):
```
stageInt(A, 0) → R10      (dividend)
stageInt(B, 1) → R11      (divisor)
MOV R10, RAX
CQO / XOR RDX,RDX
IDIVQ/DIVQ R11            (divisor in R11, never clobbered by CQO/XOR)
MOV RAX/RDX, dst
commitDst
```

**MulHigh** (MulHS, MulHU):
```
stageInt(A, 0) → R10
stageInt(B, 1) → R11
MOV R10, RAX
IMULQ/MULQ R11
MOV RDX, dst
commitDst
```

**MulHSU**:
```
stageInt(A, 0) → R10
stageInt(B, 1) → R11
MOV R10, RAX
SAR $63, R10               (sign mask in R10)
AND R11, R10               (correction = sign_neg ? b : 0)
MULQ R11                   (RDX:RAX = RAX * R11)
SUB R10, RDX               (apply correction)
MOV RDX, dst
commitDst
```

**FCmp EQ/NE**: use `byteReg(dst)` for one SETcc and `byteReg(other_scratch)` for the other — guaranteed different.

### What V2 shares with V1
Utility functions duplicated (not embedded) to prevent accidental V1 method inheritance:
- `emitRR`, `emitRI`, `emitRM`, `emitMR`, `emitMI`, `emitUnary`, `loadImm64`, `emitCmpRI`
- `byteReg`, `loadOp`, `storeOp`, `predToJcc`, `predToSETcc`, `predToFPSETcc`
- `spillLoad`, `spillStore`, `fpSpillLoad`, `fpSpillStore`
- `placeLabel`, `bindLabel`, `hostRegFor`
- Prologue/epilogue (identical structure)

### Implementation Order
1. `lowerCtxV2` struct + `stageInt`/`stageFP`/`writeDst`/`commitDst` + emission helpers
2. `AMD64Pool_V2`, `LowerAMD64_V2` entry + prologue/epilogue
3. Simple ops: Const, Mov, Sext, Zext, Neg, Not
4. Binary ops: Add/Sub/And/Or/Xor/Mul + Imm variants
5. Shifts: Shl/Shr/Sar + Imm
6. Div/MulHigh: DivS/DivU/Rem/RemU/MulHS/MulHU/MulHSU
7. Set/SetImm
8. Memory: Load/Store/LoadX/StoreX
9. Control flow: Label/Branch/BranchImm/Jump/Ret/RetDyn
10. FP: FPBinop/FPUnary/FNeg/FAbs/FCmp/conversions
11. Call/Writeback/MarkLive/MarkDead

## Pass C: Randomized Lockstep Testing

### Random IR Generator (`genRandomBlock`)
- Generates valid IR: defined-before-use, labels match branches, ends with Ret
- Weighted op selection (more ALU, fewer DIV)
- Edge case injection (10%): VRegZero sources, A==B, large immediates
- DIV guarded: only uses Const sources known non-zero
- Block sizes: 1-50 instructions

### Lockstep Harness
```
for each random block:
    alloc1 = Allocate(blk, AMD64Pool(blk), ...)
    alloc2 = Allocate(blk, AMD64Pool_V2(blk), ...)
    code1 = LowerAMD64(blk, alloc1) → Assemble
    code2 = LowerAMD64_V2(blk, alloc2) → Assemble
    exec both with identical x[], f[], fcsr, mem
    compare Result structs (PC, IC, Status, FaultAddr)
    on mismatch: dump IR block + disassembly
```

### Test Files
- `ir/lower_amd64_v2_test.go`: V2 unit tests + assembly-level comparison (no execution)
- `lockstep_v1v2_test.go` (`//go:build amd64 && !tcc`): execution-level comparison via `jitcall.Call`
- Future: `FuzzLockstep_V1_V2` using Go's coverage-guided fuzzer

## Verification

1. Pass A: existing ELF tests (srl, sllw, sraw, srlw, lw) should pass after bug fixes
2. Pass B: all existing `TestLower_*` tests duplicated for V2
3. Pass C: 10,000+ random blocks, V1 vs V2 lockstep
4. Once V2 passes everything, switch `jit_native.go:41` from `LowerAMD64` to `LowerAMD64_V2`
