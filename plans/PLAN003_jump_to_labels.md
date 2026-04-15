# Phase 2: Region-Based Blocks (Eliminate Undefined Label Errors)

## Context

Phase 1 (follow J/C.J with rd==0) improved JIT from 1255→1566 MIPS and halved dispatch count. But some blocks fail TCC compilation with "label used but not defined" because forward conditional branches emit `goto b_ADDR` to PCs past the emitted range. These blocks fall back to interpreter.

**Root cause**: `emitBranch` decides goto vs exit using `e.visited[target]`, which only reflects the past. Forward targets are never visited yet, so they always exit. But after following a J, the larger block contains forward branches whose targets ARE within the code we're about to emit — we just don't know it yet.

**Fix**: Pre-scan the control flow graph before emitting. Determine the full region extent. Emit ALL instructions in the range. Forward branches within the region use goto.

## Implementation

### Step 1: `classifyFlow` — lightweight instruction classifier

**File: `jit_emit.go`** — new function, add before `emitBlock`

Only determines control flow type, not full semantics. Reuses existing immediate extractors (bImm, jImm, rvcJOffset, rvcBOffset).

```go
type flowClass int
const (
    flowSeq    flowClass = iota // next = pc + insnSize
    flowBranch                   // next = pc + insnSize AND target
    flowJump                     // unconditional J/C.J rd==0: next = target only
    flowTerm                     // no successors (JALR, JAL rd!=0, ECALL, CSR, unknown)
)

func classifyFlow(mem *GuestMemory, pc uint64) (flowClass, uint64, uint64)
```

32-bit (opcode & 0x7F):
- 0x63 (BRANCH) → flowBranch, target = pc + bImm
- 0x6F (JAL) → rd==0: flowJump; rd!=0: flowTerm
- 0x67 (JALR) → flowTerm
- 0x73 (SYSTEM) → flowTerm
- everything else → flowSeq

16-bit RVC (quadrant + funct3):
- Q1 f3=5 (C.J) → flowJump
- Q1 f3=6 (C.BEQZ), f3=7 (C.BNEZ) → flowBranch
- Q2 f3=4: C.JR/C.EBREAK/C.JALR → flowTerm; C.MV/C.ADD → flowSeq
- everything else → flowSeq

### Step 2: `scanRegion` — BFS region discovery

**File: `jit_emit.go`** — new function

```go
type regionInfo struct {
    endPC   uint64 // exclusive: first PC past region
    pcCount int    // number of distinct PCs found
}

func scanRegion(mem *GuestMemory, entryPC uint64) regionInfo
```

BFS from entryPC:
1. Pop PC from worklist
2. Skip if: visited, < entryPC (backward out of range), > entryPC + 16384 (range cap)
3. Call `classifyFlow(mem, pc)` — skip if fetch fails
4. Mark visited, update maxEnd = max(maxEnd, pc + insnSize)
5. Add successors based on flow class:
   - flowSeq → push pc + insnSize
   - flowBranch → push BOTH pc + insnSize AND target (if target >= entryPC, within range)
   - flowJump → push target ONLY
   - flowTerm → nothing
6. Stop at 2048 visited PCs

Returns `regionInfo{endPC: maxEnd, pcCount: len(visited)}`.

### Step 3: Emitter struct additions

```go
type emitter struct {
    // ... existing ...
    regionEnd   uint64          // endPC from scanRegion
    gotoTargets map[uint64]bool // PCs referenced by goto (for bail labels)
}
```

### Step 4: Modify `emitBlock` — two-phase

```go
func emitBlock(mem *GuestMemory, pc uint64) *emitResult {
    region := scanRegion(mem, pc)  // Phase 1: scan
    e := &emitter{
        ..., regionEnd: region.endPC, gotoTargets: make(map[uint64]bool),
    }
    // Phase 2: emit ALL instructions from startPC to regionEnd
    for e.numInsns < 2048 && !e.terminated && e.pc < e.regionEnd {
        // existing visited check, fetch, emit
    }
    ...
}
```

Loop condition adds `e.pc < e.regionEnd` — spatial bound from pre-scan. Instruction limit increases from 512 to 2048.

### Step 5: Modify `emitBranch` — region-aware forward goto

```go
func (e *emitter) emitBranch(rs1, rs2, funct3 uint32, offset int64) {
    target := e.pc + uint64(offset)
    cmp := branchCmp(funct3)

    // Internal if: already emitted OR will be emitted (within region)
    internal := e.visited[target] ||
        (e.regionEnd > 0 && target >= e.startPC && target < e.regionEnd)

    if internal {
        e.emit("    if (%s %s %s) goto b_%x;\n", ...)
        e.gotoTargets[target] = true
    } else {
        // External — exit block
        e.emit("    if (...) { writeback; return ...; }\n")
    }
}
```

Key: `target < e.regionEnd` means "label WILL exist because we emit ALL PCs in the range."

### Step 6: Modify `emitJAL` — record goto targets

In the rd==0 path, add:
```go
e.gotoTargets[target] = true
```

### Step 7: Bail labels in `finalize()` — safety net

After the body, before the fall-through return, emit bail labels for any goto target that was referenced but never emitted (block terminated early mid-region):

```go
for target := range e.gotoTargets {
    if !e.visited[target] {
        fmt.Fprintf(&out, "b_%x:\n", target)
        // writeback all cached registers
        fmt.Fprintf(&out, "    return (JITResult){0x%xULL, ic, 0, 0};\n", target)
    }
}
```

**Guarantee**: every `goto b_ADDR` hits either a real instruction label (visited) or a bail label (not visited). No more "label used but not defined" errors.

## Edge Cases

| Case | Handling |
|------|----------|
| Untranslatable insn mid-region (CSR, FCLASS) | Block terminates early; bail labels cover orphaned gotos |
| ECALL/EBREAK mid-region | Same — bail labels |
| Fetch failure mid-region | Same — bail labels |
| Very dense branch code | BFS caps at 2048 PCs, 16KB range |
| Backward branch below entryPC | Excluded from region scan |
| Region with no forward branches | Degenerates to current behavior |

## Files to Modify

| File | Changes |
|------|---------|
| `jit_emit.go` | classifyFlow, scanRegion, emitter struct, emitBlock, emitBranch, emitJAL, finalize |
| `jit_test.go` | Unit tests for classifyFlow, scanRegion, forward-branch integration |

## Implementation Order

1. Add `classifyFlow` function (pure addition, no risk)
2. Add `scanRegion` function (pure addition)
3. Add `regionEnd` + `gotoTargets` to emitter struct
4. Modify `emitBlock` loop (scan + spatial bound)
5. Modify `emitBranch` (region-aware forward goto)
6. Modify `emitJAL` (record goto targets)
7. Add bail labels in `finalize()`
8. Test + benchmark

## Verification

```bash
go test -run TestJIT -v                           # unit tests
go test -run TestJIT_BenchGuest_Smoke -v ./bench/  # smoke (should have NO TCC errors)
go test -timeout 120s ./...                        # full regression
go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=1x ./bench/
```

Expected: zero TCC "label used but not defined" errors, further MIPS improvement from forward branches staying in native code.
