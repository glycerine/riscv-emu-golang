# Plan: Fix AOT Mega-Blocks (Match libriscv's Block Sizing)

## Context

Our AOT produces ~100 blocks of 3-8 instructions per 1200-byte test ELF because `enumerateBlockRanges` splits at every branch target. This is a bug — we intended libriscv-style mega-blocks but got the enumeration wrong. libriscv uses 1250+ instructions per block, splitting only at hard terminators. The fix: coarsen the enumeration, raise the block cap, and add multi-entry support so the decoder cache covers internal PCs.

## Changes

### 1. `aot.go` — New `enumerateCoarseBlockRanges`

Replace the fine-grained enumeration with one that only splits at `flowTerm` (ECALL, EBREAK, JALR, JAL-with-link). Branches within a range become internal gotos handled by `emitBlockRange`.

```go
func enumerateCoarseBlockRanges(mem *GuestMemory, textBase, textSize uint64) []blockRange {
    textEnd := textBase + textSize
    var ranges []blockRange
    blockStart := textBase
    pc := textBase
    for pc < textEnd {
        fc, _, insnSize := classifyFlow(mem, pc)
        if insnSize == 0 {
            pc += 2
            continue
        }
        pc += insnSize
        if fc == flowTerm {
            if pc > blockStart {
                ranges = append(ranges, blockRange{startPC: blockStart, endPC: pc})
            }
            blockStart = pc
        }
    }
    if blockStart < textEnd {
        ranges = append(ranges, blockRange{startPC: blockStart, endPC: textEnd})
    }
    return ranges
}
```

Keep the old `enumerateBlockRanges` for now (tests may reference it).

### 2. `jit.go:355` — `compileAOTRegion` uses coarse ranges

```go
func (j *JIT) compileAOTRegion(mem *GuestMemory, begin, end, size uint64, writable bool) {
    ranges := enumerateCoarseBlockRanges(mem, begin, size)  // was: enumerateBlockRanges
    ...
}
```

### 3. `jit_emit_ir.go` — Raise block cap for AOT

Add `aotMode bool` to the `emitter` struct. When set, use a much higher cap (500 guest instructions, matching libriscv's 1250-instruction spirit but conservative for our IR/regalloc):

```go
// in emitBlockRange, before the emission loop:
cap := PerBlockCapTimeToSplit
if e.aotMode {
    cap = 500
}

// in the cap check (line 1021):
if cap > 0 && int64(e.numInsns) >= cap {
    if int64(e.numInsns) >= cap*2 { break }
    ...
}
```

Wire `aotMode` through: `emitBlockLinear` sets `e.aotMode = true` (since it's only called from AOT), `emitBlock` (lazy) leaves it false.

### 4. `jit_emit_ir.go:32-38` — Expose pcLabels on emitResult

```go
type emitResult struct {
    block         *Block
    startPC       uint64
    endPC         uint64
    numInsns      int
    numChainExits int
    pcLabels      map[uint64]Label  // NEW: guest PC → IR label
}
```

In `finalize()` (line 792), copy the map:

```go
return &emitResult{
    ...
    pcLabels: e.pcLabels.m,  // expose the underlying map
}
```

### 5. `lower_amd64_shared.go:131` — Expose label→Prog mapping

```go
type LowerResult struct {
    ChainEntryProg  *obj.Prog
    LabelProgs      map[Label]*obj.Prog  // NEW: all IR labels → their NOP progs
    ChainExits      []ChainExitDesc
    JalrICs         []JalrICDesc
    GocallResumes   []GocallResumeDesc
}
```

### 6. `lower_amd64_rv8.go` + `lower_amd64_abjit.go` — Populate LabelProgs

After lowering, copy the lowerer's `labelProg` map to the result:

```go
// In LowerAMD64_RV8 (lower_amd64_rv8.go ~line 106):
result := &LowerResult{
    ChainEntryProg: lc.chainEntryProg,
    LabelProgs:     lc.labelProg,  // NEW
}

// Same in LowerAMD64_ABJIT (lower_amd64_abjit.go ~line 125):
result := &LowerResult{
    ChainEntryProg: lc.chainEntryProg,
    LabelProgs:     lc.labelProg,  // NEW
}
```

### 7. `jit_aot.go` — Multi-entry blocks for decoder cache + chain patching

After compiling each mega-block, build a map of internal entry points (guest PC → native offset within the block). Register each as a `compiledBlock` in the `blocks` map so the decoder cache and chain patching find them.

In Pass 2 (after mmap copy, line 107-149), add after `blocks[bc.startPC] = bc.blk`:

```go
// Register internal entry points for decoder cache + chain patching.
if res.pcLabels != nil && bc.lowerResult.LabelProgs != nil {
    for guestPC, label := range res.pcLabels {
        if guestPC == bc.startPC {
            continue // already registered as the block's main entry
        }
        prog, ok := bc.lowerResult.LabelProgs[label]
        if !ok || prog == nil {
            continue
        }
        entryAddr := blockBase + uintptr(prog.Pc)
        blocks[guestPC] = &compiledBlock{
            fn:         blockBase,
            chainEntry: entryAddr,
            hasFP:      bc.hasFP,
        }
    }
}
```

This needs `res` (the emitResult) to be retained on `aotBlockCompile`. Add field:

```go
type aotBlockCompile struct {
    ...
    emitRes *emitResult  // NEW: retained for pcLabels
}
```

Set it in Pass 1: `emitRes: res,`

Pass 3 (chain patching, line 161-173) and Pass 4 (decoder cache, line 189-199) already iterate `blocks` — they automatically pick up the internal entries with no changes.

Back-linking internal entries to their segment (Pass 4, line 224-228): already iterates `compiles` and sets `bc.blk.segment`. Add a second loop for internal entries:

```go
for _, bc := range compiles {
    if bc.blk != nil { bc.blk.segment = seg }
    // Internal entries also need segment back-link
    if bc.emitRes != nil && bc.emitRes.pcLabels != nil {
        for guestPC := range bc.emitRes.pcLabels {
            if blk, ok := blocks[guestPC]; ok && blk != nil {
                blk.segment = seg
            }
        }
    }
}
```

## Files Summary

| File | Change |
|------|--------|
| `aot.go` | New `enumerateCoarseBlockRanges` — split only at `flowTerm` |
| `jit.go:355` | `compileAOTRegion` calls coarse enumeration |
| `jit_emit_ir.go:32` | `pcLabels` field on `emitResult` |
| `jit_emit_ir.go:792` | Copy pcLabels in `finalize()` |
| `jit_emit_ir.go:88` | `aotMode bool` on emitter struct |
| `jit_emit_ir.go:976` | `emitBlockLinear` sets `aotMode = true` |
| `jit_emit_ir.go:1021` | Block cap uses `aotMode`-aware threshold |
| `lower_amd64_shared.go:131` | `LabelProgs` field on `LowerResult` |
| `lower_amd64_rv8.go:~106` | Populate `LabelProgs` from lowerer |
| `lower_amd64_abjit.go:~125` | Same |
| `jit_aot.go:30` | `emitRes` field on `aotBlockCompile` |
| `jit_aot.go:83` | Retain emitResult in Pass 1 |
| `jit_aot.go:~149` | Register internal entries in `blocks` map |
| `jit_aot.go:~224` | Back-link internal entries to segment |

## Expected Result

- Block count drops from ~100 to ~5-15 per test ELF
- Per-block overhead (emitter, regalloc, lower, assemble) amortized over 50-500 instructions instead of 3-8
- Decoder cache has entries for every branch target (via internal entries), preserving full chain patching
- Register allocation spans entire mega-block, keeping guest registers in host registers across basic block boundaries
- AOT test time: ~24s → ~2-4s (closer to lazy's 1.5s)

## Verification

```bash
# Primary: AOT should be dramatically faster
cd ~/ris && time go test -v -run TestRISCVTests_UI_JIT_AOT -count=1 .

# Compare with lazy
cd ~/ris && time go test -v -run TestRISCVTests_UI_JIT_Lazy -count=1 .

# Full riscv-tests suite — correctness
cd ~/ris && go test -v -count=1 -run 'TestRISCVTests.*JIT' .

# Bench smoke test
cd ~/ris && go test -v -run TestJIT_BenchGuest_Smoke -count=1 ./bench/

# Full test suite — no regressions
cd ~/ris && go test -v -count=1 .
```
