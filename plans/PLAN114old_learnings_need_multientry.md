# Plan: Rollback AOT Mega-Block Attempt + Lessons Learned

## Context

We attempted to coarsen AOT block enumeration (split only at flowTerm instead of every branch target) to match libriscv's mega-block model. The attempt exposed several prerequisites that must be addressed first.

## Step 1: Rollback

Revert all changes from this session in these files:

| File | Revert to |
|------|-----------|
| `aot.go` | Remove `enumerateCoarseBlockRanges` |
| `jit.go:355` | Restore `enumerateBlockRanges` call |
| `jit_emit_ir.go` | Remove `aotMode`, `pcLabels` on emitResult, revert `emitBlockRange` signature to 3 args, revert block cap logic |
| `lower_amd64_shared.go` | Remove `LabelProgs` from `LowerResult` |
| `lower_amd64_rv8.go` | Remove `LabelProgs` population |
| `lower_amd64_abjit.go` | Remove `LabelProgs` population |
| `jit_aot.go` | Remove `emitRes` field, revert Pass 1 iteration loop to simple `for _, r := range ranges`, revert segment back-link loop |

## Lessons Learned for Next Attempt

### 1. Multi-entry blocks need a switch(pc) dispatcher in the lowerer

The critical missing piece. libriscv generates `switch(pc) { case 0x1000: goto label; ... }` at each function entry. Our lowerer produces one entry point (chainEntry) after the prologue. Jumping to an internal label skips the prologue (register loads, frame setup), corrupting state.

**Next attempt must**: Add a pc-dispatch prologue to the lowerer. The compiled block receives the target PC (via a register or the sret struct), and a switch at the top routes to the right label after the prologue runs. This requires changes to `lower_amd64_rv8.go` and `lower_amd64_abjit.go`.

### 2. The emitter correctly fragments at backward jumps — iteration handles it

When `emitBlockRange` hits a backward unconditional jump (C.J, JAL rd=0), it sets `terminated=true` and stops. Code after the jump is dead within that path. The Pass 1 iteration loop (`for pc := r.startPC; pc < r.endPC; pc = res.endPC`) correctly compiles the remainder as separate blocks. This fragmentation is unavoidable without the switch-dispatch mechanism.

### 3. Without multi-entry, interior PCs fall to lazy compilation

Chain exits targeting PCs in the middle of a mega-block miss the `blocks` map, return to Go dispatch, and trigger lazy compilation. For the test ELFs, many `bne` instructions target 0x484 (failure path) which is inside a coarse block. This creates many lazy compilations, negating the AOT speedup.

### 4. The stopper_load doesn't break loops (by design)

The stopper page is PROT_READ by default. Backward branches succeed and loop. `RequestPreemption()` (PROT_NONE) is never called during normal test execution. The old model avoided infinite loops because the AUIPC+SW tohost fusion detected writes and exited the block before the loop. This is a latent issue, not introduced by mega-blocks.

### 5. Block count reduction was modest (37 vs ~100)

The coarse enumeration only splits at flowTerm, but many instructions ARE flowTerm: every CSR, JALR, ECALL, and JAL-with-link. The test ELFs have many CSR instructions, so the reduction was only ~3x, not the 10-20x expected.

## Architecture for Next Attempt

The correct order of operations:

1. **Lowerer: add switch(pc) dispatch** — Each compiled block accepts a target PC parameter. A switch at block entry (after prologue) routes to the right internal label. The decoder cache and chain patching store the target PC, not a code offset.

2. **Then coarsen enumeration** — With switch-dispatch, interior PCs work correctly. Mega-blocks have full decoder cache coverage.

3. **Then raise block cap** — `aotMode` with cap=500+ makes blocks span multiple basic blocks, improving register allocation scope.

## Verification After Rollback

```bash
cd ~/ris && go build ./...
cd ~/ris && go test -v -run TestRISCVTests_UI_JIT_AOT -count=1 .
cd ~/ris && go test -v -count=1 .
```
