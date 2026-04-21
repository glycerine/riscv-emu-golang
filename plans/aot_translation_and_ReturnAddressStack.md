# Whole-program AOT translation + Return Address Stack

## Context

### Current state (already shipped, on master)

The Go RISC-V JIT uses lazy per-region compilation with a 4096-entry
direct-mapped block cache. A 2-way JALR inline cache with shift
replacement and thrash-deopt (Phase 1.5) is in place. Measured on
three benchmarks (median of 10 runs):

|workload    |baseline MIPS|shipped MIPS (2-way IC)|Δ       |
|------------|------------:|----------------------:|-------:|
|bench_guest |        3244 |                  3204 |  noise |
|coremark    |         722 |                   769 |  +6.5% |
|dhrystone   |         511 |                   618 | +21.0% |

### What still limits us

Profile of what JALRs actually are in our benchmarks (verified with
`llvm-objdump -d` on the ELFs):

- Dhrystone: 19 `ret` instructions, **zero** indirect calls.
- CoreMark: 46 `ret` instructions, **one** indirect call (the
  `list_cmp cmp(...)` callback in `core_list_mergesort`).
- No C++ vtables, no virtual dispatch.

**99% of hot JALRs are `ret`.** A per-site inline cache is the
canonical optimization for *vtable*-style polymorphism and is the
wrong structural fit for return-dispatch polymorphism. The natural
fit is a **Return Address Stack** (RAS): on every call push the
return address, on every return pop it.

### Why whole-program AOT is a prerequisite

For RAS to be fast, the pushed entry must contain *both* the guest
return PC and the native entry address of the corresponding
JIT-compiled block. Under lazy compilation, the continuation block
(the one starting at `pc+4` after a `jal`) may not yet exist when
the `jal` is emitted — the push has nothing to stash. Options are
all awkward: eagerly compile continuations at each `jal` emission;
push just `pc+4` and pay for a PC→block lookup on every return
(defeats the purpose); or translate everything up front.

Translating the full program at ELF load solves the problem
structurally and unlocks several other wins:

1. RAS push trivially stashes `(pc+4, &blockTable[pc+4].chainEntry)`.
2. All statically-resolvable JAL edges become direct JMPs — no
   MOVABS-sentinel machinery at runtime.
3. Block eviction risk (inbound chain patches becoming stale) goes
   away — compiled blocks are permanent.
4. The "first call is slow, subsequent calls are fast" effect
   disappears.

The benchmark ELFs are tiny (.text is 2.9 KB for Dhrystone, 8.1 KB
for CoreMark) so AOT compile time is negligible.

### Scope of this plan

Phase 2 delivers:
- A whole-program BFS that translates every reachable region at
  ELF load, packing all compiled code into one mmap region.
- A PC → compiled-block map that replaces the direct-mapped cache.
- A lazy-fallback path preserved for PCs the static BFS misses
  (JALR indirect targets, CSR-containing regions).
- A Return Address Stack: shadow stack in Go memory, pushed at
  `jal rd=ra`, popped at `jalr rd=x0 rs1=ra`, with mispredict
  fallback to the existing 2-way JALR IC.

Nothing else changes (interpreter untouched, BudgetCheck untouched,
V2 lowerer untouched, Phase 1.5 IC machinery intact as the RAS
fallback path).

## Investigation findings (Agents confirmed these)

1. **`scanRegion`** (`jit_decode.go:104–151`) already does BFS per
   region, max 2048 insns / 16 KB range. A "block" in our codebase
   is already multi-basic-block. AOT is just N repetitions of this
   same scan, seeded by the static successor edges discovered in
   each region.

2. **`classifyFlow`** (`jit_decode.go:25–90`) returns the correct
   instruction size for both RVC (2-byte) and standard (4-byte)
   encodings. No mixed-size ambiguity for the byte walk.

3. **`emitBlock`** (`jit_emit_ir.go`) returns an `emitResult` per
   region. Today the set of static successor PCs discovered in the
   region lives internally (in `deferredExits` + emitter label
   tracking) and isn't exposed to the caller. We must expose it.

4. **`elf.go:76–172` `LoadELF`** returns the entry PC.
   `FindSymbolAddr` (elf.go:208–293) can parse the symbol table on
   demand — useful for cross-checking BFS coverage in tests, not
   required at runtime.

5. **`patchChainTarget`** (`jit.go:477–481`) accepts any RWX address
   + offset. Moving from per-block mmap to one big mmap doesn't
   break it.

6. **`compiledBlock.fn`** is an opaque `uintptr`. The trampoline
   (`internal/jitcall/call_amd64.s`) doesn't care about block layout.

7. **Current interpreter fallback** at `jit.go:433–458` (triggered
   when `emitBlock` returns nil or lowering fails) is preserved
   unchanged and becomes our fallback path for PCs the BFS misses.

8. **`LowerAMD64`** expects per-block `goasm.Ctx`. We keep this
   invariant: each region is lowered+assembled in its own context,
   then the resulting byte sequences are concatenated into one big
   mmap at known offsets. No global-lowering refactor.

## Design

### Part A — Whole-program BFS at ELF load

New file: `aot.go`.

```go
type regionPlan struct {
    startPC uint64
    endPC   uint64
    result  *emitResult
}

// AOTScanProgram performs a reachability BFS from entryPC,
// translating every compilable region exactly once. Regions whose
// emitBlock returns nil (e.g., CSR-containing, unsupported opcodes)
// are skipped; their PCs remain eligible for lazy fallback at run
// time. Returns plans in BFS order.
func AOTScanProgram(mem *GuestMemory, entryPC uint64) []regionPlan
```

Driver sketch:

```
worklist = [entryPC]
seen = {}            // PC → regionPlan index
plans = []
while worklist:
    pc = worklist.pop()
    if pc in seen: continue
    res = emitBlock(mem, pc)
    if res == nil or res.numInsns == 0: continue
    plans.append(regionPlan{pc, res.endPC, res})
    idx = len(plans)-1
    for p in [res.startPC .. res.endPC) by instrSize:
        seen[p] = idx
    for tgt in res.discoveredExits:    // new field, see Part B
        worklist.append(tgt)
return plans
```

### Part B — Expose static successors from `emitBlock`

`emitResult` (`jit_emit_ir.go`) gains a field:

```go
type emitResult struct {
    // ...existing fields...
    discoveredExits []uint64  // static successor PCs outside this region
}
```

Populated during emission:
- Every `JAL rd!=0, imm` to a target outside the scan region → append target.
- Every `JAL rd==0, imm` (unconditional jump) to a target outside → append.
- Every `BRANCH` with a target outside → append.
- Every fallthrough PC after a region terminator (JAL-with-link,
  JALR, ECALL) → append (it's the caller's return site).

Duplicates are fine; the BFS driver dedups via `seen`.

### Part C — Batch compile into one mmap

New function in `jit_native.go`:

```go
func (j *JIT) jitCompileAOT(plans []regionPlan) (*AOTProgram, error)
```

Steps:

1. For each plan, run the existing per-region pipeline with a fresh
   `goasm.Ctx`: `Allocate → LowerAMD64 → ctx.Assemble`. Capture
   `(bytes []byte, LowerResult)`. Note the `bytes` length.
2. Compute total size: `sum(len(bytes)) + alignment padding`. Assign
   each plan a base offset within the unified buffer.
3. Single `allocExec(total)` mmap.
4. For each plan, copy its `bytes` to `execMem[baseOffset:]`. Add
   `baseOffset` to every `Prog.Pc` captured in the LowerResult (so
   chainEntry / chainExits / jalrICs offsets become *global* byte
   offsets into `execMem`).
5. Build `compiledBlock` for each plan: `fn = &execMem[baseOffset]`,
   `chainEntry = fn + chainEntryProg.Pc`, jalrICs populated via
   `backpatchJalrICs` (Phase 1.5 helper, unchanged).
6. **Pre-resolve static chain exits.** For each chain-exit in each
   plan: if the target PC lives in the AOT set, write the target's
   absolute `chainEntry` directly into the MOVABS imm64. This
   replaces all runtime chain-exit patching for static edges.
7. Return the `AOTProgram` with the unified mmap base, size, and
   block table.

The sub-steps 1-2 can run in parallel per-region; 3-6 are sequential.

### Part D — Block table

New struct in `jit.go`:

```go
type AOTProgram struct {
    codeBase uintptr              // start of unified mmap
    codeSize int
    blocks   map[uint64]*compiledBlock  // PC → block
                                         // covers both AOT and lazy
}
```

Replaces the 4096-entry direct-mapped cache. The map covers all PCs
compiled at load time and absorbs any PC compiled lazily during
execution.

`RunJIT` dispatch change (in `jit.go:RunJIT`):

```go
blk := prog.blocks[pc]
if blk == nil {
    // Lazy fallback — same code that runs today.
    blk = j.lazyCompile(pc)
    if blk == nil { /* interpret one step */ continue }
    prog.blocks[pc] = blk
}
res := jitcall.Call(blk.fn, ...)
```

Old cache fields (`cache`, `blockCacheShift`, etc.) are removed once
the map-based table is fully in place.

### Part E — Return Address Stack

Shadow stack in Go-allocated memory. Sized and published at JIT init:

- Capacity: 1024 frames × 16 bytes/frame = 16 KB.
- Frame layout: `{guest_pc uint64, chain_entry uintptr}` at frame
  offset +0 and +8.
- Three pointer fields published into the sret buffer by the
  trampoline (and stable across all chained entries, like
  fcsr at `[RBX+80]`):
  - `[RBX+88]` = top pointer (grows upward from base)
  - `[RBX+96]` = base (constant; check for underflow)
  - `[RBX+104]` = limit (constant; check for overflow)

Trampoline (`internal/jitcall/call_amd64.s`) gets three new `MOVQ`
instructions at entry to stash the RAS pointers.

**RAS push** — emitted at `emitJAL(rd=x1, target)` when the target
is statically known AND the continuation block is in the AOT set:

```asm
; Input: continuation_chainEntry is a known absolute address.
; tgt = static target address (the callee).
MOVQ   88(RBX), R10            ; top
MOVQ   $<pc_after_jal>, (R10)   ; frame.pc
MOVABS R11, $<cont_chainEntry>  ; imm64
MOVQ   R11, 8(R10)             ; frame.chainEntry
LEAQ   16(R10), R10            ; new top
CMPQ   R10, 104(RBX)           ; overflow?
JAE    .ras_no_push            ; skip push when full (graceful)
MOVQ   R10, 88(RBX)
.ras_no_push:
```

If the continuation is NOT in the AOT set at emit time (lazy block),
we simply omit the RAS push and let the eventual return fall through
to the IC fallback. (This only affects PCs reached via
indirect-discovered code, i.e., the lazy path.)

**RAS pop + predict** — emitted at `emitJALR(rd=x0, rs1=x1)` (the
`ret` pseudo-instruction), *replacing* the current IC-first
sequence. On hit, jump directly; on mispredict, fall through into
the existing 2-way IC:

```asm
; tgt = (x1 & ~1), computed as today.
MOVQ   88(RBX), R10
CMPQ   R10, 96(RBX)            ; underflow?
JBE    .ras_fallback
LEAQ   -16(R10), R10
MOVQ   (R10), R11              ; popped guest PC
CMPQ   R11, tgt
JNE    .ras_fallback            ; tail-call or longjmp
MOVQ   8(R10), R11              ; popped chainEntry
MOVQ   R10, 88(RBX)            ; commit pop
JMP    R11
.ras_fallback:
    ; existing 2-way IC sequence (Phase 1.5, unchanged)
```

For other JALR forms (`rs1 != x1` — indirect calls and tail calls),
skip the RAS sequence entirely and emit only the 2-way IC.

### Part F — Lazy fallback (retained)

PCs that BFS misses (JALR-discovered targets, CSR-containing code)
still work:

1. Dispatch miss on `prog.blocks[pc]`.
2. Call `j.lazyCompile(pc)` — wraps the existing `emitBlock` +
   `jitCompileWith` path, allocates into a separate "lazy" mmap,
   inserts into `prog.blocks`.
3. Chain patches from AOT blocks to lazy blocks use the existing
   MOVABS sentinel + runtime backpatch machinery (unchanged).

Lazy-compiled blocks do NOT emit RAS pushes (we may not know the
continuation yet). Their returns take the IC fallback path.

### Part G — Direct JMP for static edges (optional, defer to end)

A code-size win, not a speed win. At IR emission time, if a chain
exit's target is known to be in the AOT set, emit `JMP rel32`
(5 bytes) instead of `MOVABS R10, sentinel; JMP R10` (13 bytes).
Saves 8 bytes per static edge in the code stream. Do this only
after core AOT + RAS is green and measured. Skip if time-constrained.

## Files to modify

| file | change |
|------|--------|
| `aot.go` (new) | `AOTScanProgram` BFS driver |
| `elf.go` | add `.text` base/size alongside entry from `LoadELF` (optional; BFS works without, but tests want it for coverage checks) |
| `jit_decode.go` | expose static successor PCs from region scan, or add a helper that enumerates them |
| `jit_emit.go` + `jit_emit_ir.go` | `emitResult.discoveredExits []uint64`; track during emission |
| `jit_native.go` | new `jitCompileAOT`; keep existing `jitCompileWith` for lazy path |
| `jit.go` | `AOTProgram` struct, new block map, `RunJIT` dispatch swap, remove direct-mapped cache, RAS slice allocation on `NewJIT` |
| `ir/emit.go` + `ir/highlevel.go` | `RASPush(retPC uint64, chainEntryPtr uintptr)` and `RASPop()` helper API (or raw IR ops + lowerer sequences) |
| `ir/lower_amd64.go` | `lowerRASPush`, `lowerRASPop` emitting the asm sequences above; `lowerJALR` ret form calls `lowerRASPop` before falling into the IC sequence |
| `internal/jitcall/call_amd64.s` | publish RAS base/top/limit into sret buffer at `[RBX+88/96/104]` on entry |
| `aot_test.go` (new) | `AOTScanProgram` coverage + `jitCompileAOT` output + RAS behavior |
| `bench/jit_chain_reference_test.go` | add RAS hit/miss/overflow/underflow counters to output |

No change to: interpreter, BudgetCheck, V2 lowerer, the 2-way IC
machinery (it stays as the RAS fallback), chain-exit machinery (it
stays for lazy-block targets), the existing `emitBlock` /
`jitCompileWith` path (reused by the lazy fallback).

## Execution order

1. **Step 1 — Expose successors.** Add `discoveredExits` to
   `emitResult`; populate in `emitJAL` and `emitBranch` when target
   is outside the region. Unit test: `emitBlock` on a small synthetic
   RISC-V snippet returns the expected exit set.
2. **Step 2 — BFS driver.** Write `AOTScanProgram` in `aot.go`. Unit
   test on `bench/dhrystone.elf`: compare BFS-covered PC count
   against `FindSymbolAddr` function bounds; expect full coverage of
   statically-reachable code.
3. **Step 3 — Batch compile.** Write `jitCompileAOT`. Reuse
   per-region `LowerAMD64` in a fresh `goasm.Ctx`; concatenate bytes
   into one mmap; rebase Prog.Pc fields; pre-resolve static chain
   exits. Unit test: verify every plan's `chainEntry` is within
   `[codeBase, codeBase+codeSize)`; verify every static chain-exit
   sentinel has been overwritten with a real target address.
4. **Step 4 — Block map + dispatch.** Replace the direct-mapped cache
   with `AOTProgram.blocks`. Reroute `RunJIT` to consult the map
   first, fall back to lazy-compile path on miss. All existing tests
   must stay green.
5. **Step 5 — Measure baseline AOT (no RAS).** Run
   `make bench-chain-ref` 10 runs; record medians. Expect small gain
   from eliminated runtime chain-exit backpatches.
6. **Step 6 — RAS infrastructure.**
   a. Extend `internal/jitcall/call_amd64.s` sret slots at
      `[RBX+88/96/104]`.
   b. Allocate 16 KB RAS slice on `NewJIT`; pass pointers through the
      trampoline.
   c. Add `lowerRASPush` / `lowerRASPop` in `ir/lower_amd64.go`.
7. **Step 7 — Wire RAS into JAL/JALR emission.**
   a. `emitJAL` (rd=x1, static target in AOT set): emit RAS push
      with known `continuation_chainEntry`.
   b. `emitJALR` (rd=x0, rs1=x1): prepend RAS pop + predict; on
      mispredict fall into the existing IC sequence.
   c. Other JALR forms (indirect calls): unchanged (IC only).
8. **Step 8 — Tests for RAS.**
   - Call/ret round trip: push then pop restores correct native
     entry; state matches interpreter reference.
   - Tail call: next ret mispredicts, falls back to IC cleanly.
   - Overflow: 1025 pushes saturate without corruption; pop after
     returns to correct stack state.
   - Underflow: pop on empty falls back to IC.
9. **Step 9 — Full regression + benchmark.**
   - `go test ./... ./ir/ ./bench/ ./riscv-elf-tests/...`
   - `make fuzz-oracle`, `fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
     `fuzz-bitmanip` (30 s each).
   - `make bench-chain-ref` 10 runs; compute medians.
10. **Step 10 (optional) — Direct-JMP for static edges.** Replace
    static-target MOVABS+JMP R10 with `JMP rel32`. Code-size win,
    re-run regression + bench to confirm no regression.

## Verification

### Correctness

All existing tests stay green (we reuse lowering, retain the lazy
path, preserve IC as RAS fallback):

- `go test ./... ./ir/ ./bench/`
- `go test ./riscv-elf-tests/...`
- `make fuzz-oracle`, `fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
  `fuzz-bitmanip` (30 s each)

### AOT-specific

New `aot_test.go`:
- BFS coverage: for each benchmark ELF, every PC in every function
  (per `FindSymbolAddr`) is in the returned plan set, modulo
  untranslatable regions.
- Batch compile: every plan's `chainEntry > 0`, within
  `[codeBase, codeBase+codeSize)`.
- Static pre-patch: for every chain-exit whose target PC is in the
  AOT set, the MOVABS imm64 holds the target's absolute chainEntry,
  NOT the sentinel (`0x7BADC0DE7BADC0DE`).

### RAS

New `aot_test.go`:
- Call + ret: push entry, verify `[RBX+88]` advanced by 16 and the
  frame contents. After pop, top is back where it started.
- Tail call pattern: pop mispredicts, IC fallback writes the expected
  JalrICMisses counter.
- Overflow: push until limit, verify `[RBX+88]` saturates at limit
  without writing past it.
- Underflow: pop with top == base, verify IC fallback fires.

### Performance targets

`make bench-chain-ref` medians of 10 runs:
- bench_guest within ±5% of baseline (3210 MIPS).
- CoreMark ≥ 1200 MIPS (≥ 1.6 × baseline 707).
- Dhrystone ≥ 900 MIPS (≥ 1.7 × baseline 511).

Counter expectations (return-dominated workloads):
- `ChainPatchedJalr` near zero — RAS is the fast path.
- `JalrICMisses` much lower — only tail calls + CoreMark's one
  fn-pointer call exercise the IC.
- `JalrICDeopts` → 0 on Dhrystone/bench_guest; maybe 1-2 on CoreMark
  (the polymorphic comparator site).

## Risks / edge cases

- **Per-region lowering stays isolated** in its own `goasm.Ctx`.
  Global lowering would require regalloc and label-resolution
  reworks and is explicitly *not* done here.
- **Untranslatable opcodes (e.g., CSR)** cause `emitBlock` to fail
  for a region. BFS skips those regions; their PCs become lazy-path
  PCs at run time. Verified: current `noJIT` set handles per-PC
  interpreter fallback.
- **DebugV1V2 mode** assumes lazy compile. Deferred — debug-only
  feature, not in the shipped path.
- **Sret buffer extension** beyond fcsr's `[RBX+80]`: three new
  8-byte slots at 88 / 96 / 104. Only the trampoline and the new
  RAS lowerer touch these offsets.
- **RAS mispredict on `setjmp`/`longjmp`, signal-like resumption**:
  out of scope for our benchmark ELFs. IC fallback keeps correctness;
  performance degrades to Phase 1.5 on those paths.
- **Benchmark ELFs have function-pointer calls** (CoreMark's
  comparator). Those JALRs are NOT ret-form (`rs1 != x1`), so they
  skip the RAS path entirely and use the existing IC. No new work
  needed.
- **Memory size**: 16 KB RAS + 16 KB block-table map (for larger
  ELFs) is negligible.

## Non-goals

- No changes to the interpreter.
- No changes to the 2-way JALR IC — it is the RAS fallback.
- No changes to BudgetCheck / MaxIC.
- No changes to V2 lowerer (still RetDyn-based, no RAS).
- No removal of the MOVABS-sentinel chain-exit machinery — still
  needed for lazy-block inbound edges.
- No changes to the FP paths, memory paths, or ELF loader beyond the
  optional `.text` metadata exposure.
- No self-modifying guest code support (existing limitation).
- Deferred to later phases: bounds-check hoisting, GP constant
  propagation, raising MaxIC, syscall-clobber minimization, segment
  translation cache.
