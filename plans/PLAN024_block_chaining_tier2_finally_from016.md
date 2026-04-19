# Plan: Block Chaining (Tier 2) + Default to Fixed Allocator

## Context

The native IR JIT runs at 73 MIPS — slower than the interpreter (161 MIPS) and 26x slower than TCC JIT (1927 MIPS). Root cause: every block exit does a Go round-trip (~40 instructions of overhead: RET, restore 6 callee-saves, copy 32-byte Result, Go dispatch, cache lookup, setup 6 args, save 6 callee-saves, CALL). Block chaining eliminates this for hot paths by jumping directly from one compiled block to the next.

**Tier 1 audit**: All 8 instruction categories (DIV/REM, CLZ/CTZ/CPOP, FMIN/FMAX, ORC.B, REV8, RORIW, and their W variants) are fully implemented with no bails or gaps. Solid foundation.

## Step 0: Default to FixedStaticAllocator

Same MIPS as ELS, fewer allocations, simpler code path. One-line change.

**File**: `jit.go`

- `NewJIT()`: change `ir.NewAllocator()` → `ir.NewFixedStaticAllocator()`
- `SetAllocStrategy()`: swap default — `"els"` creates `NewAllocator()`, default creates `NewFixedStaticAllocator()`

## Block Chaining Design

### Current exit (every block, including hot paths)

```asm
WriteBackAll            ; store dirty VRegs to x[]/f[]
MOV [RBX+0], pc_imm    ; Result.PC
MOV [RBX+8], RBP       ; Result.IC
MOV [RBX+16], status   ; Result.Status
MOV [RBX+24], 0        ; Result.FaultAddr
ADD RSP, frame_size     ; epilogue
RET                     ; → jitcall epilogue → Go dispatch → next block
```

### Chained exit (hot paths after patching)

```asm
WriteBackAll            ; store dirty VRegs to x[]/f[]
ADD RSP, frame_size     ; deallocate spill frame
MOVABS R10, <target>    ; 10-byte encoding, initially → slow_exit stub
JMP R10                 ; direct jump to next block's chain entry
```

### Two entry points per block

```asm
fn:                           ; from jitcall.Call (normal entry)
  MOV R12, RSI                ; xBase
  MOV R13, RDX                ; fBase
  MOV R14, R8                 ; memBase
  MOV R15, R9                 ; memMask
  MOV RBX, RDI                ; sret buffer
  MOV [RBX + 80], RCX         ; save fcsr pointer at fixed location for chains
  XOR RBP, RBP                ; IC = 0

chainEntry:                   ; from another block (chain entry)
  SUB RSP, frame_size         ; allocate spill frame
  MOV R10, [RBX + 80]         ; load fcsr from fixed location (saved by first block)
  MOV [RSP+stackSlots*8], R10 ; store in this block's fcsr spill slot
  ; ... block body (accesses fcsr via [RSP+stackSlots*8] as before) ...
```

Chain entry skips pinned reg setup and IC zeroing. Pinned regs (R12-R15, RBX) and IC (RBP) survive from the previous block.

**fcsr handling**: RBX (sret buffer ptr) is pinned and always live. The sret buffer uses offsets 0-31, callee-saves use 32-79, so offset 80 is free. The first block in a chain saves `RCX` (fcsr pointer from SysV args) to `[RBX + 80]`. Every block's chain entry loads it from there into its own spill slot. Block body code is unchanged — it still reads fcsr via `[RSP + stackSlots*8]`. This means **all blocks can chain, including FP blocks**.

### Slow exit stub (miss path)

Emitted at end of block for each chain exit. Initially targeted by the MOVABS:

```asm
slow_exit_N:            ; RSP is clean (spill frame already deallocated)
  MOV qword [RBX+0], target_pc
  MOV qword [RBX+8], RBP
  MOV qword [RBX+16], 0     ; jitOK
  MOV qword [RBX+24], 0
  RET                        ; → jitcall.Call
```

### Patching

When a slow exit returns to Go with `Status=jitOK`:
1. Go compiles target block if needed
2. Go finds the previous block's chain exit matching `Result.PC`
3. Overwrites the 8-byte immediate in the MOVABS to point to target block's `chainEntry`
4. Future executions jump directly — no Go involved

### IC accumulation

RBP (instruction counter) accumulates across the entire chain without reset. Budget checks at backward branches (`BudgetCheck`: if IC >= 4096, exit to Go) ensure GC preemption windows. When finally returning to Go (via slow exit or ECALL), `Result.IC` includes all instructions from the chain.

### Non-chainable exits (unchanged)

- ECALL/EBREAK — need Go exception handling
- Load/store faults — need Go fault delivery
- JALR (indirect jump) — target PC unknown at compile time

## Implementation Steps

### Step 1: New IR opcode `IRChainExit`

**File**: `ir/ir.go`
- Add `IRChainExit` to the opcode enum: `{targetPC=Imm, exitIdx=Imm2}`
- Add to `irOpNames`

**File**: `ir/emit.go`
- Add `ChainExit(targetPC uint64, exitIdx int)` method

**File**: `ir/highlevel.go`
- Add `ChainableRet(targetPC uint64, exitIdx int)` — calls `WriteBackAll()` then `ChainExit()`

### Step 2: Register allocation for `IRChainExit`

**Files**: `ir/regalloc.go`, `ir/regalloc_fixed.go`
- `IRChainExit` has no register operands (all immediates). Handle like `IRRet` — block terminator, no uses/defs.

### Step 3: Emit chain exits in the emitter

**File**: `jit_emit_ir.go`

Add to `emitter`:
```go
exitIdx  int   // counter for chain exit indices
```

Add helper:
```go
func (e *emitter) emitChainableReturn(pc uint64) {
    idx := e.exitIdx
    e.exitIdx++
    e.irEm.ChainableRet(pc, idx)
}
```

Replace `emitReturn(pc, jitOK)` → `emitChainableReturn(pc)` in:
- `finalize()` — fall-through return, bail labels, deferred external exits
- `emitJAL()` — JAL rd=0 to external target

Keep `emitReturn(pc, jitEcall)` and `emitReturn(pc, jitEbreak)` unchanged.

Add `numChainExits` to `emitResult`.

### Step 4: Lower `IRChainExit`

**File**: `ir/lower_amd64.go`

Add to `lowerCtx`:
```go
type chainExitInfo struct {
    targetPC  uint64
    movProg   *obj.Prog  // the MOVABS Prog — read Pc after assembly for patch offset
}
chainExits     []chainExitInfo
chainEntryProg *obj.Prog  // NOP at chain entry point — read Pc after assembly
```

**Modify `emitPrologue()`**: Insert chain entry marker (NOP) between pinned reg setup and spill frame allocation:
```go
func (lc *lowerCtx) emitPrologue() {
    // ── Normal entry (from jitcall.Call) ──
    lc.emitRR(x86.AMOVQ, RSI, R12)   // xBase
    lc.emitRR(x86.AMOVQ, RDX, R13)   // fBase
    lc.emitRR(x86.AMOVQ, R8, R14)    // memBase
    lc.emitRR(x86.AMOVQ, R9, R15)    // memMask
    lc.emitRR(x86.AMOVQ, RDI, RBX)   // sret
    // Save fcsr pointer at fixed location for future chain entries.
    // [RBX+80] is free: sret uses 0-31, callee-saves use 32-79.
    lc.emitMR(x86.AMOVQ, RCX, RBX, 80)  // [RBX+80] = fcsr ptr
    lc.emitRR(x86.AXORQ, RBP, RBP)   // IC = 0

    // ── Chain entry point (jumped to from another block) ──
    // Pinned regs (R12-R15, RBX) and IC (RBP) already set.
    lc.chainEntryProg = lc.emitNOP()

    // ── Common: allocate spill frame ──
    if lc.frameSize > 0 {
        lc.emitRI(x86.ASUBQ, lc.frameSize, RSP)
        // Load fcsr from fixed location into this block's spill slot.
        // On normal entry: [RBX+80] was just saved above.
        // On chain entry: [RBX+80] was saved by the first block in the chain.
        lc.emitRM(x86.AMOVQ, RBX, 80, amd64Scratch1)  // R10 = [RBX+80]
        lc.emitMR(x86.AMOVQ, amd64Scratch1, RSP, int64(lc.stackSlots)*8) // spill slot = R10
    }
}
```

**Add `lowerChainExit()`**:
```go
func (lc *lowerCtx) lowerChainExit(ins *IRInstr) {
    // Deallocate spill frame
    if lc.frameSize > 0 {
        lc.emitRI(x86.AADDQ, lc.frameSize, RSP)
    }
    // MOVABS R10, <sentinel> — 10-byte encoding, placeholder for patching
    // Use sentinel > 32 bits to force MOVABS encoding
    const sentinel = int64(0x7BAD_C0DE_7BAD_C0DE)
    movProg := lc.emitRI(x86.AMOVQ, sentinel, amd64Scratch1)
    lc.chainExits = append(lc.chainExits, chainExitInfo{
        targetPC: uint64(ins.Imm),
        movProg:  movProg,
    })
    // JMP R10
    lc.emitJmpReg(amd64Scratch1)
}
```

**Emit slow exit stubs after main body** (in `LowerAMD64`):

After lowering all instructions but before returning, emit one stub per chain exit and backpatch the MOVABS sentinel to point to it:

```go
for i, ce := range lc.chainExits {
    stubProg := lc.emitSlowExitStub(ce.targetPC)
    // After assembly, patch MOVABS imm64 to stub address.
    // Record stub Prog for offset computation.
    lc.chainExits[i].stubProg = stubProg
}
```

The slow exit stub:
```go
func (lc *lowerCtx) emitSlowExitStub(targetPC uint64) *obj.Prog {
    // Write Result to sret buffer (RBX)
    lc.loadImm64(int64(targetPC), amd64Scratch1)
    lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0)  // PC
    lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)     // IC
    lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 16)             // Status=jitOK
    lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)             // FaultAddr=0
    // Note: no spill frame to deallocate — already done before MOVABS
    p := lc.c.NewProg()
    p.As = obj.ARET
    lc.c.Append(p)
    return firstProgOfStub  // record for offset computation
}
```

**Post-assembly backpatch**: After `ctx.Assemble()` produces bytes, use `Prog.Pc` to compute absolute addresses and overwrite the MOVABS sentinels:

```go
// In jit_native.go, after assembly:
codeBase := uintptr(unsafe.Pointer(&execMem[0]))
for _, ce := range lowerCtx.chainExits {
    stubAddr := codeBase + uintptr(ce.stubProg.Pc)
    patchOffset := int(ce.movProg.Pc) + 2  // imm64 starts at byte 2 of MOVABS
    binary.LittleEndian.PutUint64(execMem[patchOffset:], uint64(stubAddr))
}
```

### Step 5: Update `compiledBlock` and compilation pipeline

**File**: `jit_native.go`

```go
type chainPatchInfo struct {
    targetPC    uint64  // guest PC this exit goes to
    patchOffset int     // byte offset of imm64 in MOVABS within code[]
}

type compiledBlock struct {
    fn         uintptr
    chainEntry uintptr          // entry point that skips pinned reg setup
    chainExits []chainPatchInfo // for Go-side patching
    shadow     *compiledBlock
}
```

In `jitCompileWith()`, after assembly:
1. Compute `chainEntry = codeBase + chainEntryProg.Pc`
2. Backpatch MOVABS sentinels to slow exit stub addresses
3. Build `chainExits` slice from lowerer's `chainExitInfo`

**Plumbing**: The lowerer needs to return `chainExits` and `chainEntryProg` to the caller. Add a return struct or fields to the `LowerAMD64` function signature:

```go
type LowerResult struct {
    ChainEntryOffset int              // byte offset of chain entry
    ChainExits       []ChainExitDesc  // {targetPC, movProgPc}
}

func LowerAMD64(ctx *goasm.Ctx, blk *Block, alloc *Allocation) (*LowerResult, error)
```

### Step 6: Patching in dispatch loop

**File**: `jit.go`

Add `prevBlk` tracking and patching to `RunJIT`:

```go
func (j *JIT) RunJIT(cpu *CPU) error {
    var prevBlk *compiledBlock
    for {
        ...
        blk := j.lookupBlock(pc)
        if blk != nil {
            res := jitcall.Call(blk.fn, ...)
            cpu.pc = res.PC
            cpu.cycle += res.IC
            switch int(res.Status) {
            case jitOK:
                // Patch previous block's chain exit if target is now compiled
                if prevBlk != nil && len(prevBlk.chainExits) > 0 {
                    j.tryPatchChain(prevBlk, cpu.pc)
                }
                prevBlk = blk
                continue
            default:
                prevBlk = nil  // non-OK exits break the chain
                ...
            }
        }
        prevBlk = nil
        ...
    }
}

func (j *JIT) tryPatchChain(blk *compiledBlock, targetPC uint64) {
    target := j.lookupBlock(targetPC)
    if target == nil || target.chainEntry == 0 {
        return
    }
    for _, ce := range blk.chainExits {
        if ce.targetPC == targetPC {
            // Overwrite the MOVABS imm64 with target's chain entry address
            *(*uint64)(unsafe.Pointer(blk.fn + uintptr(ce.patchOffset))) = uint64(target.chainEntry)
            break
        }
    }
}
```

### Step 7: `emitNOP` and `emitJmpReg` helpers

**File**: `ir/lower_amd64.go`

These may not exist yet. Add:
```go
func (lc *lowerCtx) emitNOP() *obj.Prog {
    p := lc.c.NewProg()
    p.As = x86.ANOP
    lc.c.Append(p)
    return p
}

func (lc *lowerCtx) emitJmpReg(reg int16) {
    p := lc.c.NewProg()
    p.As = x86.AJMP
    p.To.Type = obj.TYPE_REG
    p.To.Reg = reg
    lc.c.Append(p)
}
```

## Files Modified

| File | Change |
|------|--------|
| `jit.go` | Default to Fixed allocator; add `prevBlk` + `tryPatchChain` to `RunJIT` |
| `jit_native.go` | `chainEntry`/`chainExits` in `compiledBlock`; backpatch MOVABS after assembly |
| `ir/ir.go` | Add `IRChainExit` opcode |
| `ir/emit.go` | Add `ChainExit()` method |
| `ir/highlevel.go` | Add `ChainableRet()` method |
| `ir/regalloc.go` | Handle `IRChainExit` (no uses/defs) |
| `ir/regalloc_fixed.go` | Handle `IRChainExit` (no uses/defs) |
| `ir/lower_amd64.go` | Chain entry marker in prologue; `lowerChainExit()`; slow exit stubs; `emitNOP()`; `emitJmpReg()`; return `LowerResult` |
| `jit_emit_ir.go` | `exitIdx` field; `emitChainableReturn()`; replace `emitReturn(pc, jitOK)` calls |

## Verification

```bash
# Step 0: Fixed allocator default — quick smoke
go test -v -run TestJIT_ADD .
go test -v -run TestRISCVTests_Lockstep_UI/add .

# After each step: full correctness
go test -count=1 -run TestRISCVTests -timeout 600s .
go test -count=1 -run TestRISCVTests_Lockstep_UI -timeout 600s .

# JIT unit tests
go test -count=1 -run TestJIT_ -timeout 30s .

# Benchmark: expect 73 → 500+ MIPS after chaining
go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=3x ./bench/

# Full alloc comparison
make bench-alloc
```

## Expected Performance

With chaining, hot loops (fib, sieve) become:
```
Block A → JMP Block B → JMP Block A  (tight loop, zero Go involvement)
```

The 45% dispatch overhead drops to near zero for chained paths. Expected: **500-2000 MIPS** (limited by code quality and compilation overhead, not dispatch). Compilation overhead still affects cold blocks but hot loops should be near-TCC speed.
