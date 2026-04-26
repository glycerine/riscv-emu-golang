# Phase 5: ABJIT Optimizations

## Context

Phase 4 wired abjit into RunJIT. Current benchmark: abjit = 2070 MIPS vs rv8 = 2232 MIPS (~7% gap). The gap comes from:
1. **Copy-in/copy-out**: 1584 bytes moved per dispatch (532 in + 516 out + 536 save/restore)
2. **JALR always misses**: Every JALR returns to Go dispatch (no decoder_cache, no inline IC)
3. **Syscall always cold**: Every ECALL returns to Go (no inline dispatch)
4. **IRCall unsupported**: Blocks with external calls fall back to interpreter

Phase 5 addresses all of these plus AOT integration.

---

## Step 1: Persistent register file buffer (eliminate save/restore)

**Problem**: `abjitDispatch` uses the shadow page as the register file. The shadow page overlaps guest address space, so we must save 568 bytes before use and restore after. This is 1136 bytes of memcpy per dispatch for zero value.

**Fix**: Allocate a persistent heap buffer (extended `abjit.State`) on the JIT struct. Use it as `regFileBase` instead of the shadow page. No save/restore needed.

### 1a. Extend abjit.State with decoder cache fields

**File**: `~/ris/abjit/abjit.go`

```go
type State struct {
    X             [32]uint64  // 0
    F             [32]uint64  // 256
    FCSR          uint32      // 512
    _             uint32      // 516
    MemBase       uintptr     // 520
    MemMask       uint64      // 528
    PC            uint64      // 536
    IC            uint64      // 544
    Status        uint64      // 552
    FaultAddr     uint64      // 560
    DCBase        uintptr     // 568  (decoder cache base)
    DCMask        uint64      // 576  (decoder cache mask)
    VAddrBegin    uint64      // 584  (segment start)
    SegSize       uint64      // 592  (segment size)
}
```

Update `TestStateLayout` to verify new offsets.

### 1b. Add abjitState field to JIT struct

**File**: `~/ris/jit.go`

```go
type JIT struct {
    ...
    useABJIT   bool
    abjitState *abjit.State  // persistent register file buffer; allocated on first use
    ...
}
```

Initialize lazily in `abjitDispatch` (not in `NewJIT` — avoid allocation when abjit isn't used).

### 1c. Rewrite abjitDispatch to use persistent buffer

**File**: `~/ris/jit_abjit.go`

```go
func abjitDispatch(fn uintptr, cpu *CPU, j *JIT,
    dcBase uintptr, dcMask, vBegin, segSize uint64) jitcall.Result {

    if j.abjitState == nil {
        j.abjitState = abjit.NewState()
    }
    s := j.abjitState
    rf := s.RegFileBase()

    // Copy CPU state → buffer (no save/restore needed).
    s.X = cpu.x
    s.F = cpu.f
    s.FCSR = cpu.fcsr
    s.MemBase = cpu.mem.Base()
    s.MemMask = cpu.mem.Mask()
    s.DCBase = dcBase
    s.DCMask = dcMask
    s.VAddrBegin = vBegin
    s.SegSize = segSize

    abjit.CallJIT(fn, rf)

    res := jitcall.Result{
        PC:        s.PC,
        IC:        s.IC,
        Status:    s.Status,
        FaultAddr: s.FaultAddr,
    }

    // Copy buffer → CPU state.
    cpu.x = s.X
    cpu.f = s.F
    cpu.fcsr = s.FCSR

    return res
}
```

**Savings**: Eliminates 568-byte save + 568-byte restore = 1136 bytes of memcpy per dispatch. Also eliminates all `unsafe.Pointer` arithmetic — uses typed struct fields instead.

### 1d. Update RunJIT call site

**File**: `~/ris/jit.go`

Pass segment params to `abjitDispatch` (same pattern as the rv8 sandboxCall):

```go
if j.useABJIT {
    var dcBase uintptr
    var dcMask, vBegin, segSize uint64
    if seg := j.soleSegment; seg != nil {
        dcBase = seg.decoderCacheBase
        dcMask = seg.decoderCacheMask
        vBegin = seg.vaddrBegin
        segSize = seg.vaddrSize
    } else if len(j.aotSegments) > 0 {
        seg := blk.segment
        if seg == nil { seg = j.hotSegment }
        if seg == nil { seg = j.aotSegments[0] }
        dcBase = seg.decoderCacheBase
        dcMask = seg.decoderCacheMask
        vBegin = seg.vaddrBegin
        segSize = seg.vaddrSize
    }
    res = abjitDispatch(blk.fn, cpu, j, dcBase, dcMask, vBegin, segSize)
}
```

### 1e. Add abjitState cleanup in Close()

**File**: `~/ris/jit.go` — In `Close()`, nil out `j.abjitState` (GC collects it).

---

## Step 2: Decoder cache lookup in JALR

**Problem**: Every JALR returns `jitOKJalrMiss` to Go. With AOT, there's a decoder_cache that maps guest PCs to block chainEntry pointers. The rv8 lowerer consults it inline; abjit doesn't.

**Fix**: Emit inline decoder_cache lookup in `abjitJalrIC`. On hit, jump directly to the cached chainEntry (bypassing Go dispatch).

### 2a. Add State offset constants

**File**: `~/ris/ir/lower_amd64_abjit.go`

```go
const (
    abjitDCBaseOff    = 568
    abjitDCMaskOff    = 576
    abjitVAddrBeginOff = 584
    abjitSegSizeOff   = 592
)
```

### 2b. Rewrite abjitJalrIC

**File**: `~/ris/ir/lower_amd64_abjit.go`

The new implementation:
1. Save target PC to State.PC and scratch slot
2. Stage IC to State
3. Store regs back
4. Load dcBase from [RBP+568]; if zero → miss
5. Bounds check: (target - vaddrBegin) < segSize
6. Compute index: ((target - vaddrBegin) * 4) & dcMask
7. Load entry: *(dcBase + index)
8. If non-zero: dealloc frame → JMP entry (hit path)
9. Miss: write Status=jitOKJalrMiss, FaultAddr=siteIdx → exit thunk

The x86-64 instruction sequence follows the rv8 pattern but uses `[RBP+offset]` instead of `[RAX+sret_offset]`:

```asm
; target already in stgB (RCX)
MOV [RBP+536], RCX              ; State.PC = target

; Load dcBase
MOV RAX, [RBP+568]              ; RAX = dcBase
TEST RAX, RAX
JE .miss                        ; no cache → miss

; Bounds check
MOV RDX, RCX                    ; RDX = target (copy)
SUB RDX, [RBP+584]              ; RDX = target - vaddrBegin
CMP RDX, [RBP+592]              ; cmp vs segSize
JAE .miss                       ; out of bounds → miss

; Index
SHL RDX, 2                      ; RDX = (target-vaddrBegin)*4
AND RDX, [RBP+576]              ; RDX &= dcMask

; Lookup
MOV RDX, [RAX + RDX]            ; RDX = *(dcBase + index)
TEST RDX, RDX
JE .miss                        ; empty slot → miss

; HIT: jump to cached block
ADD RSP, frameSize
JMP RDX

.miss:
MOV qword [RBP+552], JitOKJalrMiss
MOV qword [RBP+560], siteIdx
; ... exit thunk ...
```

---

## Step 3: 2-way JALR inline cache

**Problem**: Even with decoder_cache, the first JALR to a lazy-compiled target misses. The rv8 path has a 2-way associative IC (patchable MOVABS slots) that handles polymorphic JALR sites.

**Fix**: Emit the same MOVABS sentinel pattern in `abjitJalrIC`. The existing `backpatchJalrICs` and `tryPatchJalrIC` in `jit_native.go` / `jit.go` handle initialization and runtime patching — they're target-agnostic.

### 3a. Emit 2-way IC before decoder_cache lookup

Add to `abjitJalrIC`, before the decoder_cache lookup:

```asm
; Slot 0: fast path
MOVABS R10, <sentinel_pc0>       ; patchable: target PC
CMP    target, R10
JNE    .slot1
MOVABS R10, <sentinel_fn0>       ; patchable: chainEntry
ADD    RSP, frameSize
JMP    R10

.slot1:
MOVABS R10, <sentinel_pc1>       ; patchable: target PC
CMP    target, R10
JNE    .dc_lookup
MOVABS R10, <sentinel_fn1>       ; patchable: chainEntry
ADD    RSP, frameSize
JMP    R10

.dc_lookup:
; ... decoder_cache lookup from Step 2 ...
```

### 3b. Populate JalrICDesc in LowerResult

Record the MOVABS Progs and miss stub in the result:

```go
lc.jalrICs = append(lc.jalrICs, jalrICInfo{
    siteIdx:  int(ins.Imm),
    pcMov:    [2]*obj.Prog{pcMov0, pcMov1},
    fnMov:    [2]*obj.Prog{fnMov0, fnMov1},
})
```

Then in `LowerAMD64_ABJIT`, build `JalrICDesc` entries:
```go
for _, ic := range lc.jalrICs {
    result.JalrICs = append(result.JalrICs, JalrICDesc{
        SiteIdx:  ic.siteIdx,
        PcMov:    ic.pcMov,
        FnMov:    ic.fnMov,
        StubProg: ic.stubProg,
    })
}
```

Existing `backpatchJalrICs` in `jit_native.go` handles the rest (initializes pc=0xFFFF..., fn=stubAddr). Existing `tryPatchJalrIC` in `jit.go` handles runtime patching with shift semantics.

---

## Step 4: Inline syscall dispatch

**Problem**: Every ECALL exits to Go with Status=jitEcall. The rv8 path calls a C dispatcher inline; if it returns 0 (handled), it chains to the post-ECALL block without returning to Go.

**Fix**: The C syscall dispatcher is a plain SysV-ABI function (no CGO needed). Call it directly from abjit-generated code. The abjit trampoline's 65KB Go stack frame provides ample space.

### 4a. Rewrite abjitSyscall

**File**: `~/ris/ir/lower_amd64_abjit.go`

```
Stage IC to State
Spill live caller-saved regs (RDX, RSI, RDI, R8-R11, X0-X13)
Set up SysV args:
  RDI ← RBP  (guest register array)
  RSI ← memBase (from host reg or [RBP+520])
  RDX ← memMask (from host reg or [RBP+528])
Load dispatcher address from CTab[ins.Imm2]
CALL RAX

TEST RAX, RAX
JNE .cold

; Hot path (RAX==0): syscall handled, chain to resumePC
Reload spilled regs
Write IC to State
Dealloc frame
MOVABS RCX, <sentinel>  (backpatchable to post-ECALL block)
JMP RCX

.cold:
; Cold path (RAX!=0): return to Go
Reload spilled regs
abjitRet(resumePC, Status=jitEcall)
```

The hot-path chain exit gets recorded in `lc.chainExits` just like a normal chain exit, so `tryPatchChain` links it to the post-ECALL block.

### 4b. Handle dispatcher address = 0

When `currentSyscallDispatcherAddr()` returns 0 (direct syscall disabled), the emitter emits `IRSyscall` with `Imm2 = -1` (no CTab entry). In this case, `abjitSyscall` falls through to the cold path unconditionally (matching current behavior).

---

## Step 5: IRCall via gocall

**Problem**: Blocks containing `IRCall` fail compilation. The rv8 lowerer calls C functions via direct CALL. The abjit path has no C functions but has the gocall mechanism for Go callbacks.

**Fix**: Emit the 34-byte gocall callback sequence for IRCall. Spill/reload caller-saved registers around the call.

### 5a. Implement abjitCall

**File**: `~/ris/ir/lower_amd64_abjit.go`

```go
func (lc *lowerCtxABJIT) abjitCall(ins *IRInstr) error {
    if int(ins.Imm) >= len(lc.blk.CTab) {
        return fmt.Errorf("ir.LowerAMD64_ABJIT: IRCall CTab[%d] out of range", ins.Imm)
    }
    sym := lc.blk.CTab[ins.Imm]

    // Spill live caller-saved registers
    liveInt, liveFP := lc.identifyLiveCallerSaved()
    saveSize := (len(liveInt) + len(liveFP)) * 8
    if saveSize > 0 {
        lc.emitRI(x86.ASUBQ, int64(saveSize), goasm.REG_AMD64_SP)
    }
    for i, r := range liveInt {
        lc.emitMR(x86.AMOVQ, r, goasm.REG_AMD64_SP, int64(i*8))
    }
    for i, r := range liveFP {
        lc.emitMR(x86.AMOVSD, r, goasm.REG_AMD64_SP, int64((len(liveInt)+i)*8))
    }

    // Emit gocall sequence (34 bytes)
    lc.emitGocallSequence(sym.Addr)

    // Reload
    for i, r := range liveInt {
        lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(i*8), r)
    }
    for i, r := range liveFP {
        lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64((len(liveInt)+i)*8), r)
    }
    if saveSize > 0 {
        lc.emitRI(x86.AADDQ, int64(saveSize), goasm.REG_AMD64_SP)
    }

    return nil
}
```

### 5b. Emit gocall sequence in goasm

The gocall callback is 34 bytes of raw machine code. Need to emit via goasm Progs. The sequence:

```
MOVABS R11, gocallAddr       (10 bytes)
LEA    R10, [RIP+17]         (7 bytes)
MOV    [RSP+frameSize], R10  (note: RSP offset accounts for spill frame)
MOVABS R10, goFuncAddr       (10 bytes)
JMP    R11                   (3 bytes)
```

The `[RSP+frameSize]` stores the resume address in the trampoline's resume slot. After spill frame adjustment, this points to `[RSP + saveSize + lc.frameSize]` relative to the current RSP.

Actually, the gocall mechanism stores the resume address at [RSP+0] where RSP is the **trampoline's** RSP (before the lowerer's SUB RSP, frameSize). So the offset from the current RSP is `lc.frameSize + saveSize`.

### 5c. gocallAddr access from ir/ package

The `gocallAddr` lives in the `abjit` package. The `ir/` package can't import `abjit/`. Solution: pass `gocallAddr` to the lowerer via a new field in `RegPolicy` or as a parameter to `LowerAMD64_ABJIT`.

Add to `RegPolicy`:
```go
type RegPolicy struct {
    Name       string
    Pool       func(*Block) RegPool
    Pinned     func() map[VReg]int16
    Lower      func(*goasm.Ctx, *Block, *Allocation) (*LowerResult, error)
    GocallAddr uintptr  // 0 for rv8; set by abjit
}
```

Set in `PolicyABJIT`:
```go
var PolicyABJIT = RegPolicy{
    Name:       "abjit",
    Pool:       ABJITPool,
    Pinned:     ABJITPinned,
    Lower:      LowerAMD64_ABJIT,
    GocallAddr: 0,  // set at runtime by SetRegPolicy
}
```

In `SetRegPolicy`:
```go
if p.Name == "abjit" {
    p.GocallAddr = abjit.GocallAddr()
}
```

---

## Step 6: Lazy FP copy optimization

**Problem**: Every dispatch copies all 32 FP registers (256 bytes each way = 512 bytes) even for integer-only blocks.

**Fix**: Track whether the block uses FP registers. If not, skip the FP copy.

### 6a. Add hasFP flag to compiledBlock

**File**: `~/ris/jit.go`

```go
type compiledBlock struct {
    ...
    hasFP bool  // block uses FP registers
}
```

Set during compilation based on whether any FP VRegs were allocated.

### 6b. Conditional FP copy in abjitDispatch

```go
if blk.hasFP {
    s.F = cpu.f
}
// ... execute ...
if blk.hasFP {
    cpu.f = s.F
}
```

Pass `hasFP` to `abjitDispatch` or make it a method on a dispatch context.

---

## Files summary

| File | Action | Description |
|------|--------|-------------|
| `~/ris/abjit/abjit.go` | Modify | Extend State with DC fields; add GocallAddr() export |
| `~/ris/abjit/abjit_test.go` | Modify | Verify new State offsets |
| `~/ris/ir/lower_amd64_abjit.go` | Modify | JALR IC + decoder cache + syscall + IRCall |
| `~/ris/ir/lower_amd64.go` | Modify | GocallAddr field in RegPolicy |
| `~/ris/jit.go` | Modify | abjitState field, DC params in dispatch, hasFP |
| `~/ris/jit_abjit.go` | Rewrite | Persistent buffer, no save/restore |
| `~/ris/jit_native.go` | No change | backpatchJalrICs already handles abjit blocks |
| `~/ris/jit_abjit_test.go` | Modify | Update for new abjitDispatch signature |
| `~/ris/bench/jit_bench_test.go` | No change | Benchmark already exists |

## Dependency order

```
Step 1 (persistent buffer + State extension)
    ↓
Step 2 (decoder cache lookup)  ←  needs DC fields from Step 1
    ↓
Step 3 (2-way JALR IC)         ←  emitted before DC lookup from Step 2
 NOPE: THIS WAS REPLACED BY THE DECODER CACHE LOOKUP (in rv8).
 
Step 4 (inline syscall)        ←  independent, but benefits from Step 1
Step 5 (IRCall/gocall)         ←  independent
Step 6 (lazy FP copy)          ←  independent, benefits from Step 1
```

Steps 4/5/6 can be done in any order after Step 1. Steps 2-3 must be sequential.

## Verification

```bash
# After each step:
go build ./...
go test -v ./abjit/
go test -v ./ir/ -run TestLower
go test -v -run TestABJIT -timeout 120s .
go test -count=1 -timeout 300s .

# Performance after all steps:
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/
```

## Expected impact

| Optimization | Estimated MIPS gain | Workloads affected |
|-------------|--------------------|--------------------|
| Eliminate save/restore | +50-100 | All |
| Decoder cache JALR | +200-500 | JALR-heavy (function calls) |
| 2-way JALR IC | +50-100 | Polymorphic call sites |
| Inline syscall | +100-200 | Syscall-heavy (I/O programs) |
| IRCall/gocall | +50 | Blocks with external calls |
| Lazy FP copy | +20-50 | Integer-only blocks |

Target: close or exceed the rv8 path (3400 MIPS).
