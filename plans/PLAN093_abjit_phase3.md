# Phase 3: LowerAMD64_ABJIT — abjit lowerer for the IR pipeline

## Context

Phase 1 added pluggable `RegPolicy` to `ir/` (PolicyRV8, PolicyABJIT
with 11-register pool, RBP pinned). Phase 2 built the standalone
`abjit` package (trampoline, CodeBuilder, State, 14 passing tests,
2.65ns trampoline overhead).

Phase 3 creates `LowerAMD64_ABJIT` — an AMD64 lowerer that converts
register-allocated IR blocks into x86-64 machine code compatible with
the abjit trampoline's calling convention (2-arg callJIT, RBP =
regFileBase, gocall callbacks, 65KB frame).

**Goal**: Given an IR Block + Allocation, produce native code that runs
via `callJIT(code, regFileBase)` and writes results to the State struct.
Chain exits, JALR miss returns, and inline mask-based memory access all
work. Production RunJIT integration is deferred to Phase 4.

---

## Design Decisions

### 1. Result communication: State struct, not sret buffer

rv8 uses an sret buffer (passed via RDI in the jitcall trampoline) to
return PC/IC/Status/FaultAddr. abjit has no sret buffer. Instead, the
JIT code writes results to the State struct at known [RBP+offset].

Extend `abjit.State`:
```
Offset 536: PC        — 8 bytes
Offset 544: IC        — 8 bytes
Offset 552: Status    — 8 bytes
Offset 560: FaultAddr — 8 bytes
```

### 2. Prologue: single entry (no first/chain split)

rv8 has separate first-entry (RBP from RSI, sret from RDI) and
chain-entry (RAX=sret) paths. In abjit, RBP is always regFileBase
(set by trampoline on first entry, preserved across chains). Both paths
do the same thing: allocate spill frame, load registers from [RBP],
load IC from [RBP+544], load memBase/memMask from [RBP+520/528].

### 3. Exit: shared thunk

Every block exit (normal return, slow stub, JALR miss) needs the same
7-instruction trampoline-restore sequence. Emit it once as an "exit
thunk" at the end of the block. Exit paths write result fields to State,
then JMP to the thunk.

```asm
exit_thunk:
  MOV [RSP+8], RBX       ; restore callee-saves
  MOV [RSP+24], R12
  MOV [RSP+32], R13
  MOV [RSP+40], R15
  ADD RSP, 0xFFF8         ; undo Go prologue SUB
  POP RBP
  RET
```

Note: RSP is at trampoline level when the thunk runs (caller already
did ADD RSP, frameSize to deallocate the lowerer's spill frame).

### 4. Chain exit: no sret passing

rv8 chain exit: storeRegsBack → load sret → write IC to sret →
dealloc frame → MOVABS sentinel → JMP. Next block receives RAX=sret.

abjit chain exit: storeRegsBack → write IC to [RBP+544] → dealloc
frame → MOVABS sentinel → JMP. No sret. Next block's chain entry
loads IC from [RBP+544] (same State struct).

### 5. Memory access: inline mask-based (same as rv8)

memBase at [RBP+520], memMask at [RBP+528] — same offsets in abjit
State and rv8 register file. All inline masking code (`AND mask, addr;
ADD memBase, addr`) is identical. No callbacks for memory access.

### 6. Syscall: cold path only

rv8 has inline syscall dispatch (calls C dispatcher, chains on
success). abjit has no C dispatcher. All ECALLs write Status=1 to State
and exit. The Go dispatch loop handles the syscall.

### 7. JALR IC: simple miss return

rv8 JALR IC does a decoder_cache lookup (AOT optimization). abjit has
no decoder_cache. Every JALR writes Status=jitOKJalrMiss + site index
to State and exits. The Go dispatch loop handles lookup and patching.
(Current rv8 lowerer also doesn't use the 2-slot inline cache — it uses
decoder_cache lookup. jalrICs slice is always empty.)

### 8. IRCall: gocall-based

rv8's IRCall calls C functions via direct CALL. abjit has no C
functions. IRCall uses the gocall mechanism: spill caller-saved
registers, emit 34-byte gocall callback sequence, reload after return.
This handles CTab entries that are Go function pointers. If no CTab
entry, fall back to error.

### 9. Shared per-op code: extract lowerOps

The rv8 lowerer is ~2200 lines. ~1600 lines are per-op instruction
lowering (arithmetic, comparisons, branches, memory, FP) that is
identical for both lowerers. Extract these into a `lowerOps` struct
in `lower_amd64_ops.go`. Both `lowerCtxRV8` and `lowerCtxABJIT` embed
`lowerOps` and override only the context-specific ops.

**Common ops** (~45): IRMov, IRConst, IRSext, IRZext, IRAdd, IRSub,
IRMul, IRDiv, IRRem, all shifts, all bitwise, IRSet, IRLoad, IRStore,
IRLoadX, IRStoreX, IRMisalignLoad, IRMisalignStore, IRBranch, IRJump,
IRLabel, all FP ops, all FP conversions, IRMarkLive, IRMarkDead,
IRWriteback, IRFence.

**Context-specific ops** (~6): IRRet, IRRetDyn, IRChainExit, IRJalrIC,
IRSyscall, IRCall.

### 10. Frame layout

```
Lowerer's frame (within trampoline's 65KB):
  [RSP+0 .. spillSlots*8-1]   = spill slots
  [RSP+spillSlots*8]           = scratch A (8 bytes, DIV/MUL RDX save)
  [RSP+spillSlots*8+8]         = scratch B (8 bytes, retDyn PC save)
  Total frameSize = spillSlots*8 + 16
```

No sret slot (rv8 has sretOffset at spillSlots*8 and 24 bytes of
fixed overhead). abjit has only 16 bytes of fixed overhead.

### 11. Staging registers: same as rv8

RAX (stgA), RCX (stgB) for integer staging. XMM14 (stgFB), XMM15
(stgFA) for FP staging. Never in the allocation pool.

---

## Step 1: Extend abjit.State

**File**: `~/ris/abjit/abjit.go`

Add result fields after MemMask:

```go
type State struct {
    X         [32]uint64   // offset 0
    F         [32]uint64   // offset 256
    FCSR      uint32       // offset 512
    _         uint32       // offset 516
    MemBase   uintptr      // offset 520
    MemMask   uint64       // offset 528
    PC        uint64       // offset 536
    IC        uint64       // offset 544
    Status    uint64       // offset 552
    FaultAddr uint64       // offset 560
}
```

**File**: `~/ris/abjit/abjit_test.go`

Update TestStateLayout to verify new field offsets:
```go
{"PC", unsafe.Offsetof(s.PC), 536},
{"IC", unsafe.Offsetof(s.IC), 544},
{"Status", unsafe.Offsetof(s.Status), 552},
{"FaultAddr", unsafe.Offsetof(s.FaultAddr), 560},
```

---

## Step 2: Extract lowerOps to lower_amd64_ops.go

**File**: `~/ris/ir/lower_amd64_ops.go` (NEW, ~1700 lines)

### 2a. Define lowerOps struct

```go
type lowerOps struct {
    blk        *Block
    alloc      *Allocation
    c          *goasm.Ctx
    idx        int
    rIdx       regIndex
    fpSet      map[VReg]bool
    cxLive     []regEntry
    labelProg  map[Label]*obj.Prog
    pending    map[Label][]*obj.Prog
    stackSlots int
    frameSize  int64
    chainEntryProg *obj.Prog
    chainExits     []chainExitInfo
    jalrICs        []jalrICInfo
}
```

### 2b. Define staging constants

Rename from rv8-prefixed to generic:

```go
const (
    stgA  int16 = goasm.REG_AMD64_AX  // integer staging slot A
    stgB  int16 = goasm.REG_AMD64_CX  // integer staging slot B
    stgFA int16 = goasm.REG_AMD64_X15 // FP staging slot A
    stgFB int16 = goasm.REG_AMD64_X14 // FP staging slot B
)

const (
    intRegOffset = 0   // x[r] at [RBP + r*8]
    fpRegOffset  = 256 // f[r] at [RBP + 256 + r*8]
)
```

### 2c. Move emit helpers

All of these become methods on `*lowerOps`:
- `emit2`, `emitRI`, `emitRM`, `emitMR`, `emitMI`, `emitUnary`
- `emitCmpRI`, `loadImm`, `loadSpill`, `storeSpill`
- `loadFPSpill`, `storeFPSpill`
- `emitRMMovzx`, `emitMRByte`

### 2d. Move staging/resolution helpers

All become methods on `*lowerOps`:
- `stageInt`, `stageFP`, `writeDst`, `writeDstFP`, `commitDst`
- `directReg`, `hostReg`, `isVRegFP`, `isRegLive`
- `spilledRegFileOff`, `spilledMemOp`
- `regFileOff` (free function, stays free)

### 2e. Move storeRegsBack

Identical for both lowerers — writes allocated registers to [RBP].
Becomes method on `*lowerOps`.

### 2f. Move per-op lowering functions

Rename `rv8X` → `opsX` for all per-op functions. All become methods
on `*lowerOps`:

```
rv8Mov       → opsMov
rv8Const     → opsConst
rv8Sext      → opsSext
rv8Zext      → opsZext
rv8Binop     → opsBinop
rv8BinopImm  → opsBinopImm
rv8Unary     → opsUnary
rv8Neg       → (handled by opsUnary)
rv8Div       → opsDiv
rv8MulHigh   → opsMulHigh
rv8MulHSU    → opsMulHSU
rv8Shift     → opsShift
rv8ShiftImm  → opsShiftImm
rv8Set       → opsSet
rv8SetImm    → opsSetImm
rv8Load      → opsLoad
rv8Store     → opsStore
rv8LoadX     → opsLoadX
rv8StoreX    → opsStoreX
rv8Branch    → opsBranch
rv8BranchImm → opsBranchImm
rv8Jump      → opsJump
rv8MisalignLoad  → opsMisalignLoad
rv8MisalignStore → opsMisalignStore
rv8FPBinop   → opsFPBinop
rv8FPUnary   → opsFPUnary
rv8FNeg      → opsFNeg
rv8FAbs      → opsFAbs
rv8FCmp      → opsFCmp
rv8FCvtToI   → opsFCvtToI
rv8FCvtFromI → opsFCvtFromI
rv8FCvtFF    → opsFCvtFF
```

Plus label helpers: `placeLabel`, `bindLabel`, `resolveForward`.

### 2g. Create lowerInstrCommon

```go
func (lc *lowerOps) lowerInstrCommon(ins *IRInstr) (bool, error) {
    switch ins.Op {
    case IROpInvalid:
        return true, fmt.Errorf("invalid op at index %d", lc.idx)
    case IRMov:
        lc.opsMov(ins)
    case IRConst:
        lc.opsConst(ins)
    // ... all ~45 common ops ...
    case IRMarkLive, IRMarkDead, IRWriteback:
        // no-op
    default:
        return false, nil  // not handled — context-specific
    }
    return true, nil
}
```

---

## Step 3: Refactor lower_amd64_rv8.go

**File**: `~/ris/ir/lower_amd64_rv8.go` (MODIFIED, ~600 lines down from ~2200)

### 3a. Embed lowerOps

```go
type lowerCtxRV8 struct {
    lowerOps
    sretOffset int64
}
```

### 3b. Keep rv8-specific functions

These stay in lower_amd64_rv8.go as methods on `*lowerCtxRV8`:
- `emitPrologue` (first-entry / chain-entry split, sret setup)
- `emitEpilogue` (ADD RSP frameSize; RET)
- `stageICToScratch` (saves IC to [RSP+sretOffset+8])
- `rv8Ret` (writes to sret buffer)
- `rv8RetDyn` (writes dynamic PC/Status to sret)
- `rv8ChainExit` (loads sret, writes IC, chains)
- `emitSlowExitStub` (writes PC/Status to sret, RET)
- `rv8Syscall` (inline C dispatcher + cold path)
- `rv8Call` (CALL to C function with caller-save spill)
- `rv8JalrIC` (decoder_cache lookup)

### 3c. Update lowerInstr

```go
func (lc *lowerCtxRV8) lowerInstr(ins *IRInstr) error {
    if handled, err := lc.lowerOps.lowerInstrCommon(ins); handled || err != nil {
        return err
    }
    switch ins.Op {
    case IRRet:
        lc.rv8Ret(ins)
    case IRRetDyn:
        lc.rv8RetDyn(ins)
    case IRChainExit:
        lc.rv8ChainExit(ins)
    case IRJalrIC:
        lc.rv8JalrIC(ins)
    case IRCall:
        lc.rv8Call(ins)
    case IRSyscall:
        lc.rv8Syscall(ins)
    default:
        return fmt.Errorf("ir.LowerAMD64_RV8: unhandled op %v at index %d",
            ins.Op, lc.idx)
    }
    return nil
}
```

### 3d. Update LowerAMD64_RV8 entry point

Change `lc := &lowerCtxRV8{...}` initialization to embed lowerOps:
```go
lc := &lowerCtxRV8{
    lowerOps: lowerOps{
        blk:        b,
        alloc:      alloc,
        c:          ctx,
        rIdx:       rIdx,
        fpSet:      fpSet,
        cxLive:     cxLive,
        labelProg:  make(map[Label]*obj.Prog),
        pending:    make(map[Label][]*obj.Prog),
        stackSlots: alloc.StackSlots,
    },
}
lc.sretOffset = int64(lc.stackSlots) * 8
lc.frameSize = lc.sretOffset + 24
```

### 3e. Rename staging constant references

All references to `rv8StgA`, `rv8StgB`, `rv8StgFA`, `rv8StgFB` in
rv8-specific functions → `stgA`, `stgB`, `stgFA`, `stgFB`.

All references to `rv8IntRegOffset`, `rv8FPRegOffset` → `intRegOffset`,
`fpRegOffset`.

---

## Step 4: Create lower_amd64_abjit.go

**File**: `~/ris/ir/lower_amd64_abjit.go` (NEW, ~500 lines)

### 4a. State offset constants

```go
const (
    abjitMemBaseOff   = 520 // [RBP+520] = State.MemBase
    abjitMemMaskOff   = 528 // [RBP+528] = State.MemMask
    abjitPCOff        = 536 // [RBP+536] = State.PC
    abjitICOff        = 544 // [RBP+544] = State.IC
    abjitStatusOff    = 552 // [RBP+552] = State.Status
    abjitFaultAddrOff = 560 // [RBP+560] = State.FaultAddr
)
```

### 4b. Context struct

```go
type lowerCtxABJIT struct {
    lowerOps
    scratchAOff int64     // [RSP+spillSlots*8]: scratch A
    scratchBOff int64     // [RSP+spillSlots*8+8]: scratch B
    exitThunk   *obj.Prog // shared exit thunk (callee-restore + RET)
}
```

### 4c. LowerAMD64_ABJIT entry point

```go
func LowerAMD64_ABJIT(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
    if alloc == nil {
        return nil, fmt.Errorf("ir.LowerAMD64_ABJIT: nil allocation")
    }

    rIdx := buildRegIndex(alloc)
    fpSet := make(map[VReg]bool)
    var cxLive []regEntry
    for i := range alloc.IntervalMap {
        ia := &alloc.IntervalMap[i]
        if isXMMReg(ia.Host) {
            fpSet[ia.Interval.VReg] = true
        }
        if ia.Host == goasm.REG_AMD64_CX {
            cxLive = append(cxLive, regEntry{
                start: ia.Interval.Start,
                end:   ia.Interval.End,
                host:  ia.Host,
            })
        }
    }
    for vr := VReg(32); vr < 64; vr++ {
        fpSet[vr] = true
    }
    sort.Sort(regEntriesByStart(cxLive))

    lc := &lowerCtxABJIT{
        lowerOps: lowerOps{
            blk:        b,
            alloc:      alloc,
            c:          ctx,
            rIdx:       rIdx,
            fpSet:      fpSet,
            cxLive:     cxLive,
            labelProg:  make(map[Label]*obj.Prog),
            pending:    make(map[Label][]*obj.Prog),
            stackSlots: alloc.StackSlots,
        },
    }
    lc.scratchAOff = int64(lc.stackSlots) * 8
    lc.scratchBOff = lc.scratchAOff + 8
    lc.frameSize = lc.scratchAOff + 16

    lc.emitPrologue()

    for idx := range b.Instrs {
        lc.idx = idx
        if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
            return nil, err
        }
    }

    if len(lc.pending) > 0 {
        return nil, fmt.Errorf("ir.LowerAMD64_ABJIT: %d unresolved labels", len(lc.pending))
    }

    // Emit slow exit stubs.
    for i := range lc.chainExits {
        lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
    }

    // Emit shared exit thunk (after all stubs).
    lc.emitExitThunk()

    result := &LowerResult{ChainEntryProg: lc.chainEntryProg}
    for i := range lc.chainExits {
        result.ChainExits = append(result.ChainExits, ChainExitDesc{
            TargetPC: lc.chainExits[i].targetPC,
            MovProg:  lc.chainExits[i].movProg,
            StubProg: lc.chainExits[i].stubProg,
        })
    }
    return result, nil
}
```

### 4d. Prologue

```go
func (lc *lowerCtxABJIT) emitPrologue() {
    // Chain entry point (also first entry — identical in abjit).
    // RBP already = regFileBase (set by trampoline or preserved from
    // previous chained block).
    lc.chainEntryProg = lc.c.NewProg()
    lc.chainEntryProg.As = obj.ANOP
    lc.c.Append(lc.chainEntryProg)

    // Allocate lowerer's spill frame.
    lc.emitRI(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)

    // Load allocated RISC-V integer registers from register file.
    for vr := VReg(1); vr < 32; vr++ {
        if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
            host := lc.rIdx.lookup(vr, 0)
            if host >= 0 {
                lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, int64(vr)*8, host)
            }
        }
    }

    // Load allocated FP registers.
    for vr := VReg(32); vr < 64; vr++ {
        if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
            host := lc.rIdx.lookup(vr, 0)
            if host >= 0 {
                off := int64(fpRegOffset) + int64(vr-32)*8
                lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_BP, off, host)
            }
        }
    }

    // Load IC from State.IC. On first call this is 0 (caller pre-clears).
    // On chain entry it carries forward from previous block.
    if int(VRIC) < len(lc.alloc.Kind) {
        lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitICOff, stgA)
        switch lc.alloc.Kind[VRIC] {
        case AllocReg:
            host := lc.rIdx.lookup(VRIC, 0)
            if host >= 0 {
                lc.emit2(x86.AMOVQ, stgA, host)
            }
        case AllocStack:
            lc.storeSpill(stgA, lc.alloc.SpillSlot[VRIC])
        }
    }

    // Load memBase/memMask from State (AFTER regs, so they win on
    // host register conflicts).
    if int(VRMemBase) < len(lc.alloc.Kind) {
        switch lc.alloc.Kind[VRMemBase] {
        case AllocReg:
            host := lc.rIdx.lookup(VRMemBase, 0)
            if host >= 0 {
                lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemBaseOff, host)
            }
        case AllocStack:
            lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemBaseOff, stgA)
            lc.storeSpill(stgA, lc.alloc.SpillSlot[VRMemBase])
        }
    }
    if int(VRMemMask) < len(lc.alloc.Kind) {
        switch lc.alloc.Kind[VRMemMask] {
        case AllocReg:
            host := lc.rIdx.lookup(VRMemMask, 0)
            if host >= 0 {
                lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemMaskOff, host)
            }
        case AllocStack:
            lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemMaskOff, stgA)
            lc.storeSpill(stgA, lc.alloc.SpillSlot[VRMemMask])
        }
    }
}
```

### 4e. Exit thunk

Emitted ONCE at the end of the block. All exit paths JMP here after
writing result fields to State and deallocating the spill frame.

```go
func (lc *lowerCtxABJIT) emitExitThunk() {
    lc.exitThunk = lc.c.NewProg()
    lc.exitThunk.As = obj.ANOP
    lc.c.Append(lc.exitThunk)

    // Restore callee-saves from trampoline frame.
    // RSP is at trampoline level here (spill frame already deallocated).
    lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 8, goasm.REG_AMD64_BX)
    lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 24, goasm.REG_AMD64_R12)
    lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 32, goasm.REG_AMD64_R13)
    lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 40, goasm.REG_AMD64_R15)

    // Undo Go prologue: ADD RSP, 0xFFF8; POP RBP; RET.
    lc.emitRI(x86.AADDQ, 0xFFF8, goasm.REG_AMD64_SP)

    // POP RBP (restore caller's frame pointer).
    popProg := lc.c.NewProg()
    popProg.As = x86.APOPQ
    popProg.To.Type = obj.TYPE_REG
    popProg.To.Reg = goasm.REG_AMD64_BP
    lc.c.Append(popProg)

    retProg := lc.c.NewProg()
    retProg.As = obj.ARET
    lc.c.Append(retProg)
}
```

**Note on POPQ**: If goasm doesn't have APOPQ, use the equivalent
two-instruction sequence: `MOV [RSP], RBP; ADD RSP, 8`.

### 4f. IC staging (write IC to State before storeRegsBack)

```go
func (lc *lowerCtxABJIT) stageICToState() {
    if int(VRIC) >= len(lc.alloc.Kind) {
        return
    }
    switch lc.alloc.Kind[VRIC] {
    case AllocReg:
        hr := lc.hostReg(VRIC)
        if hr >= 0 {
            lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_BP, abjitICOff)
        }
    case AllocStack:
        lc.loadSpill(lc.alloc.SpillSlot[VRIC], stgA)
        lc.emitMR(x86.AMOVQ, stgA, goasm.REG_AMD64_BP, abjitICOff)
    }
}
```

This writes IC directly to [RBP+544] BEFORE storeRegsBack. Safe because
storeRegsBack only writes guest regs to [RBP+0..511], never [RBP+544].

### 4g. abjitRet (IRRet handler)

```go
func (lc *lowerCtxABJIT) abjitRet(ins *IRInstr) {
    // Stage IC to State before storeRegsBack.
    lc.stageICToState()
    lc.storeRegsBack()

    // Write Result.PC.
    lc.loadImm(ins.Imm, stgB)
    lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)

    // Write Result.Status.
    lc.emitMI(x86.AMOVQ, ins.Imm2, goasm.REG_AMD64_BP, abjitStatusOff)

    // Write Result.FaultAddr.
    if ins.A != VRegZero {
        fa := lc.stageInt(ins.A, 1) // RCX
        lc.emitMR(x86.AMOVQ, fa, goasm.REG_AMD64_BP, abjitFaultAddrOff)
    } else {
        lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)
    }

    // Deallocate spill frame and jump to exit thunk.
    lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
    jp := lc.c.NewProg()
    jp.As = obj.AJMP
    jp.To.Type = obj.TYPE_BRANCH
    jp.To.SetTarget(lc.exitThunk)
    lc.c.Append(jp)
}
```

**Problem**: exitThunk is not yet emitted when abjitRet runs (it's
emitted after all instructions). **Solution**: use forward reference.
Set jp.To.SetTarget(lc.exitThunk) — since exitThunk is a *obj.Prog
that will be appended later, the assembler resolves it at assembly time.

BUT: lc.exitThunk is nil when abjitRet runs (set in emitExitThunk
which runs after the instruction loop). **Fix**: pre-create the thunk
NOP prog before the instruction loop, append it later:

In LowerAMD64_ABJIT, before the instruction loop:
```go
lc.exitThunk = lc.c.NewProg()
lc.exitThunk.As = obj.ANOP
```

In emitExitThunk, instead of creating a new NOP:
```go
func (lc *lowerCtxABJIT) emitExitThunk() {
    lc.c.Append(lc.exitThunk)  // append pre-created NOP
    // ... rest of thunk instructions ...
}
```

This way, lc.exitThunk is valid (non-nil) during the instruction loop,
and the forward JMP references resolve correctly.

### 4h. abjitRetDyn (IRRetDyn handler)

```go
func (lc *lowerCtxABJIT) abjitRetDyn(ins *IRInstr) {
    // Stage dynamic PC from VReg A to scratch B slot on stack.
    var pcStaged bool
    if ins.A != VRegZero {
        hr := lc.hostReg(ins.A)
        if hr >= 0 {
            lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_SP, lc.scratchBOff)
            pcStaged = true
        }
    }

    lc.stageICToState()
    lc.storeRegsBack()

    // Result.PC
    if pcStaged {
        lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.scratchBOff, stgB)
        lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)
    } else if ins.A != VRegZero {
        pcReg := lc.stageInt(ins.A, 1)
        lc.emitMR(x86.AMOVQ, pcReg, goasm.REG_AMD64_BP, abjitPCOff)
    } else {
        lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitPCOff)
    }

    // Result.Status
    lc.emitMI(x86.AMOVQ, ins.Imm, goasm.REG_AMD64_BP, abjitStatusOff)

    // Result.FaultAddr
    if ins.B != VRegZero {
        fa := lc.stageInt(ins.B, 1)
        lc.emitMR(x86.AMOVQ, fa, goasm.REG_AMD64_BP, abjitFaultAddrOff)
    } else {
        lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)
    }

    lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
    jp := lc.c.NewProg()
    jp.As = obj.AJMP
    jp.To.Type = obj.TYPE_BRANCH
    jp.To.SetTarget(lc.exitThunk)
    lc.c.Append(jp)
}
```

### 4i. abjitChainExit (IRChainExit handler)

```go
func (lc *lowerCtxABJIT) abjitChainExit(ins *IRInstr) {
    // Write IC to State before storeRegsBack.
    lc.stageICToState()
    lc.storeRegsBack()

    // Deallocate spill frame (RSP back to trampoline level).
    lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

    // MOVABS RCX, <sentinel> (10-byte, backpatchable).
    const sentinel = int64(0x7BADC0DE7BADC0DE)
    p := lc.c.NewProg()
    p.As = x86.AMOVQ
    p.From.Type = obj.TYPE_CONST
    p.From.Offset = sentinel
    p.To.Type = obj.TYPE_REG
    p.To.Reg = stgB
    lc.c.Append(p)

    lc.chainExits = append(lc.chainExits, chainExitInfo{
        targetPC: uint64(ins.Imm),
        movProg:  p,
    })

    // JMP RCX (to chained block's chainEntry, or slow exit stub).
    jp := lc.c.NewProg()
    jp.As = obj.AJMP
    jp.To.Type = obj.TYPE_REG
    jp.To.Reg = stgB
    lc.c.Append(jp)
}
```

**Key difference from rv8**: no sret loading, no IC-to-sret write.
IC already in State. RBP already points to State. Next block's chain
entry loads registers from the same [RBP].

### 4j. emitSlowExitStub

```go
func (lc *lowerCtxABJIT) emitSlowExitStub(targetPC uint64) *obj.Prog {
    first := lc.c.NewProg()
    first.As = obj.ANOP
    lc.c.Append(first)

    // Registers already stored back. IC already in State.
    // RSP at trampoline level (frame deallocated by chain exit).

    // Result.PC = targetPC
    lc.loadImm(int64(targetPC), stgB)
    lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)

    // Result.Status = 0
    lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitStatusOff)

    // Result.FaultAddr = 0
    lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)

    // Jump to exit thunk (callee restore + RET).
    jp := lc.c.NewProg()
    jp.As = obj.AJMP
    jp.To.Type = obj.TYPE_BRANCH
    jp.To.SetTarget(lc.exitThunk)
    lc.c.Append(jp)

    return first
}
```

### 4k. abjitSyscall (IRSyscall handler — cold path only)

```go
func (lc *lowerCtxABJIT) abjitSyscall(ins *IRInstr) {
    // No inline dispatcher. Always return to Go with Status=jitEcall.
    // WriteBackAll was already emitted by the emitter.
    lc.abjitRet(&IRInstr{
        Op:   IRRet,
        Imm:  ins.Imm,  // resume PC (pc+4)
        Imm2: 1,         // Status = jitEcall
        A:    VRegZero,   // FaultAddr = 0
    })
}
```

### 4l. abjitJalrIC (IRJalrIC handler — simple miss return)

```go
func (lc *lowerCtxABJIT) abjitJalrIC(ins *IRInstr) {
    // Save target PC before storeRegsBack.
    if ins.A != VRegZero {
        hr := lc.hostReg(ins.A)
        if hr >= 0 {
            lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_SP, lc.scratchBOff)
        } else {
            a := lc.stageInt(ins.A, 0)
            lc.emitMR(x86.AMOVQ, a, goasm.REG_AMD64_SP, lc.scratchBOff)
        }
    }

    lc.stageICToState()
    lc.storeRegsBack()

    // Write Result.PC = target.
    if ins.A != VRegZero {
        lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.scratchBOff, stgB)
        lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)
    } else {
        lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitPCOff)
    }

    // Write Result.Status = jitOKJalrMiss.
    lc.emitMI(x86.AMOVQ, int64(JitOKJalrMiss), goasm.REG_AMD64_BP, abjitStatusOff)

    // Write Result.FaultAddr = siteIdx.
    lc.emitMI(x86.AMOVQ, int64(ins.Imm), goasm.REG_AMD64_BP, abjitFaultAddrOff)

    lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
    jp := lc.c.NewProg()
    jp.As = obj.AJMP
    jp.To.Type = obj.TYPE_BRANCH
    jp.To.SetTarget(lc.exitThunk)
    lc.c.Append(jp)
}
```

### 4m. abjitCall (IRCall handler — gocall-based)

For Phase 3, IRCall returns an error. The emitter should not generate
IRCall for blocks that will be lowered with PolicyABJIT. If it does,
compilation fails gracefully and the block falls back to interpretation.

```go
func (lc *lowerCtxABJIT) abjitCall(ins *IRInstr) error {
    return fmt.Errorf("ir.LowerAMD64_ABJIT: IRCall not supported (index %d)", lc.idx)
}
```

### 4n. lowerInstr dispatch

```go
func (lc *lowerCtxABJIT) lowerInstr(ins *IRInstr) error {
    if handled, err := lc.lowerOps.lowerInstrCommon(ins); handled || err != nil {
        return err
    }
    switch ins.Op {
    case IRRet:
        lc.abjitRet(ins)
    case IRRetDyn:
        lc.abjitRetDyn(ins)
    case IRChainExit:
        lc.abjitChainExit(ins)
    case IRJalrIC:
        lc.abjitJalrIC(ins)
    case IRCall:
        return lc.abjitCall(ins)
    case IRSyscall:
        lc.abjitSyscall(ins)
    default:
        return fmt.Errorf("ir.LowerAMD64_ABJIT: unhandled op %v at index %d",
            ins.Op, lc.idx)
    }
    return nil
}
```

---

## Step 5: Wire PolicyABJIT.Lower

**File**: `~/ris/ir/lower_amd64.go`

Change PolicyABJIT from:
```go
var PolicyABJIT = RegPolicy{
    Name:   "abjit",
    Pool:   ABJITPool,
    Pinned: ABJITPinned,
}
```

To:
```go
var PolicyABJIT = RegPolicy{
    Name:   "abjit",
    Pool:   ABJITPool,
    Pinned: ABJITPinned,
    Lower:  LowerAMD64_ABJIT,
}
```

---

## Step 6: Tests

### 6a. Unit tests for State extension

**File**: `~/ris/abjit/abjit_test.go` (MODIFY)

Add PC/IC/Status/FaultAddr to TestStateLayout checks.

### 6b. Lowerer unit test: assemble and inspect

**File**: `~/ris/ir/lower_amd64_abjit_test.go` (NEW)

Test that LowerAMD64_ABJIT produces valid goasm output for simple IR
blocks. Verify assembly succeeds and code size is reasonable. Does NOT
execute the code (ir/ can't import abjit/).

```go
func TestLowerABJIT_BasicBlock(t *testing.T) {
    // Build IR: X[0] = X[1] + X[2], return PC=0x1000.
    e := NewEmitter()
    r1 := VReg(1)
    r2 := VReg(2)
    dst := e.Tmp()
    e.Block.Instrs = append(e.Block.Instrs,
        IRInstr{Op: IRAdd, Dst: dst, A: r1, B: r2, T: I64},
        IRInstr{Op: IRWriteback, Dst: VReg(0), A: dst},
        IRInstr{Op: IRRet, Imm: 0x1000, Imm2: 0},
    )

    pool := ABJITPool(e.Block)
    pinned := ABJITPinned()
    alloc := /* allocate registers */
    ctx := goasm.NewCtx()
    result, err := LowerAMD64_ABJIT(ctx, e.Block, alloc)
    if err != nil {
        t.Fatal(err)
    }
    code, err := ctx.Assemble()
    if err != nil {
        t.Fatal(err)
    }
    t.Logf("code size: %d bytes", len(code))
    if result.ChainEntryProg == nil {
        t.Error("missing chain entry prog")
    }
}
```

### 6c. Refactoring verification: rv8 tests still pass

Run existing ir/ tests to verify the lowerOps extraction didn't break
anything:

```bash
cd ~/ris && go test -v ./ir/ -run TestLower
```

### 6d. End-to-end test (root package)

**File**: `~/ris/abjit_integration_test.go` (NEW, conditional)

Compile an IR block with PolicyABJIT, mmap it, run via abjit.callJIT,
verify State results. This test imports both `riscv/ir` and
`riscv/abjit`.

```go
func TestABJIT_EndToEnd_Add(t *testing.T) {
    // Build IR: X[0] = X[1] + X[2]
    // Allocate with ABJITPool/ABJITPinned
    // Lower with LowerAMD64_ABJIT
    // Assemble
    // mmap + copy code
    // Create abjit.State with X[1]=7, X[2]=35
    // callJIT(codeAddr, state.RegFileBase())
    // Assert state.X[0] == 42
    // Assert state.PC == expected
    // Assert state.Status == 0
}
```

### 6e. Cross-check: State offsets match lowerer constants

```go
func TestABJIT_StateOffsets(t *testing.T) {
    var s abjit.State
    checks := []struct{ name string; got, want uintptr }{
        {"PC", unsafe.Offsetof(s.PC), 536},
        {"IC", unsafe.Offsetof(s.IC), 544},
        {"Status", unsafe.Offsetof(s.Status), 552},
        {"FaultAddr", unsafe.Offsetof(s.FaultAddr), 560},
    }
    for _, c := range checks {
        if c.got != c.want {
            t.Errorf("%s offset = %d, want %d", c.name, c.got, c.want)
        }
    }
}
```

---

## Step 7: Verification

### 7a. Build

```bash
cd ~/ris && go build ./...
```

### 7b. Run ir/ tests (verify refactoring didn't break rv8)

```bash
cd ~/ris && go test -v ./ir/ -count=1
```

### 7c. Run abjit/ tests (verify State extension is backward-compatible)

```bash
cd ~/ris && go test -v ./abjit/ -count=1
```

### 7d. Run root package tests (verify no regressions)

```bash
cd ~/ris && go test -count=1 -timeout 300s .
```

### 7e. Run end-to-end test

```bash
cd ~/ris && go test -v -run TestABJIT_EndToEnd .
```

### 7f. Benchmarks (abjit trampoline still fast)

```bash
cd ~/ris/abjit && go test -run='^$' -bench=. -benchtime=3s
```

---

## Files summary

| File | Action | Lines | Description |
|------|--------|-------|-------------|
| `~/ris/abjit/abjit.go` | Modify | +4 | Add PC/IC/Status/FaultAddr to State |
| `~/ris/abjit/abjit_test.go` | Modify | +4 | Verify new State field offsets |
| `~/ris/ir/lower_amd64_ops.go` | Create | ~1700 | Shared lowerOps + per-op lowering |
| `~/ris/ir/lower_amd64_rv8.go` | Modify | -1600 | Embed lowerOps, keep rv8-specific |
| `~/ris/ir/lower_amd64_abjit.go` | Create | ~500 | abjit lowerer |
| `~/ris/ir/lower_amd64.go` | Modify | +1 | Wire PolicyABJIT.Lower |
| `~/ris/ir/lower_amd64_abjit_test.go` | Create | ~60 | Lowerer unit test |
| `~/ris/abjit_integration_test.go` | Create | ~80 | End-to-end execution test |

## What this does NOT do

- Does not wire abjit into RunJIT dispatch loop (Phase 4)
- Does not implement decoder_cache JALR IC (future optimization)
- Does not implement inline syscall dispatch (future optimization)
- Does not support IRCall/gocall callbacks (returns error)
- Does not add AOT compilation support
- Does not modify the root package's JIT dispatch path
