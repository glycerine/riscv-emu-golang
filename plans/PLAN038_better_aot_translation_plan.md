# Whole-program AOT into a DecodedExecuteSegment with DecoderData lookup

## Terminology (adopted from libriscv)

To make cross-referencing with libriscv's codebase straightforward,
we use the same names for structurally-equivalent concepts:

| libriscv name | meaning here |
|---|---|
| `DecodedExecuteSegment` | a contiguous guest-VA exec region with its own decoder cache; owns the AOT-compiled native code for that region |
| `DecoderData` | one entry in a segment's decoder cache — flat array indexed by `(pc - vaddr_begin) >> 1` (our guests always support compressed, so SHIFT=1) |
| `decoder_cache` | the flat `DecoderData[]` array owned by a segment |
| `m_vaddr_begin` / `m_vaddr_end` | segment's guest-VA range |
| `m_translator_mappings` | per-entry native-entry pointer (our `chainEntry`) |
| `m_is_stale` | segment's decoder cache needs re-decode (deferred to 2b) |
| `is_likely_jit` | segment is writable-and-executable (deferred to 2b) |
| "next_execute_segment" | lookup/create-on-miss dispatch (deferred to 2b) |

In libriscv (interpreter + optional TCC-translated), `DecoderData`
holds a bytecode handler pointer. In our JIT-only model it holds a
`chainEntry` (native entry into AOT-compiled code for that PC).
Same shape, different payload.

## Context

### Current state (shipped on master)

Lazy per-region compile with a 4096-entry direct-mapped block
cache; 2-way JALR inline cache with shift replacement and thrash
deopt. Measured (median of 10 runs):

|workload    |baseline MIPS|shipped MIPS (2-way IC)|Δ       |
|------------|------------:|----------------------:|-------:|
|bench_guest |        3244 |                  3204 |  noise |
|coremark    |         722 |                   769 |  +6.5% |
|dhrystone   |         511 |                   618 | +21.0% |

### What still limits us

From `llvm-objdump -d` on the benchmark ELFs:

- Dhrystone: 19 `ret`, zero indirect calls.
- CoreMark: 46 `ret`, one indirect call (`list_cmp`).
- No C++ vtables.

**99% of hot JALRs are `ret`.** The per-site IC thrashes on
return polymorphism. A direct guest-PC → native-entry lookup
eliminates both the IC's cache overhead and its thrashing.

### What libriscv does — the reference we're following

Per investigation of `xendor/libriscv/lib/libriscv/`:

- Multiple **`DecodedExecuteSegment`**s per machine, each holding
  a flat **`DecoderData[]`** array indexed by PC (shifted).
- JALR hot path (`cpu_dispatch.cpp:219-226`): bounds check against
  *current* segment, then index into its decoder cache. ~4 insns.
- Segment miss (`cpu.cpp:87-200`): scan existing segments, or
  create a new one on the fly for writable-executable pages. This
  is what lets LuaJIT / V8 / any dynamic-codegen runtime work —
  `next_execute_segment(pc)` transparently creates + decodes a
  new segment the first time JALR lands in guest-written code.
- Stale detection (`cpu_dispatch.cpp:257-259`): illegal-bytecode
  handler flags `m_is_stale = true`, forcing re-decode on next
  access. No FENCE.I required.

### Design rationale

**Why a guest-PC → native-entry mapping must exist.** Native code
doesn't sit at guest VAs — translation expands each guest insn
into a variable number of native bytes. Given a runtime guest PC
(e.g., `ra` at `ret`), we must look up the native entry.

Today we do this via a Go map (baseline) or the per-site 2-way IC
(Phase 1.5). Both are *caches* over the same mapping. A flat
`DecoderData[]` array populated at AOT load is a *perfect hash* of
that mapping for PCs in the compiled segment — O(1), no collisions,
no caching, no prediction.

### Sandboxing invariant — each segment's DecoderData is its own sandbox

**Principle**: every memory access whose address derives from
guest-controlled inputs must be bounded by a mask-based
`hostPtr = base + (addr & mask)` pattern — the same safety
envelope that protects the main guest memory region.

Our decoder cache satisfies this by construction:

- Each `DecodedExecuteSegment`'s `decoder_cache` lives in its own
  **dedicated mmap**, separate from main guest memory. Size is
  rounded up to the next power of two so mask addressing works.
- Guest code cannot reach it: guest loads/stores use the main
  guest-memory base (`R14`) and mask (`R15`). The decoder_cache
  base is a different pinned pointer (sret extension), never used
  as base for a guest load/store.
- The mapping is `mprotect`'d **read-only** after the segment's
  cache is populated. Neither a JIT bug nor a hostile guest can
  corrupt it.
- Access is mask-bounded: `chainEntry = decoder_cache_base +
  (byte_offset & decoder_cache_mask)`. Identical in shape to a
  guest `lw`. A wild `ra` cannot read past the cache.

Guest code that writes new machine code into its own memory (a
LuaJIT-style use case) writes to the *main guest memory* sandbox,
not to any DecoderData cache. Our emulator translates that guest
code into new segments as needed (Phase 2b).

### Why linear scan for Phase 2a

For the initial segment (`.text` from the ELF):

- `.text` and `.rodata` are separate sections in our benchmark ELFs
  (verified: `llvm-objdump -h` on `bench/dhrystone.elf` /
  `bench/coremark.elf`), so `.text` is contiguous valid code.
- Linear scan = one sequential pass over `.text` bytes.
  Cache-friendly, catches every valid instruction (including
  those reached only via indirect calls that static BFS would
  miss).
- Complexity O(N) where N is `.text` size. Benchmark `.text`
  sizes: dhrystone 2.9 KB, coremark 8.1 KB — AOT compile time is
  negligible.

Phase 2b will add a linear-scan-or-BFS-style path for dynamically
discovered exec regions (LuaJIT's JITted code areas).

### Scope of this plan (Phase 2a)

Delivers:

1. **`LoadELFBytes`** extended to return `.text` base and size
   alongside the entry PC.
2. **Linear `.text` scan at ELF load**, producing a block-plan
   list for every basic block.
3. **`jitCompileAOTSegment`**: batch-compile all block plans into
   one contiguous native-code mmap.
4. **Initial `DecodedExecuteSegment`**: owns the native-code
   mmap + a `decoder_cache` mmap (flat array indexed by
   `(pc - vaddr_begin) >> 1`, each slot = 8 bytes holding either
   the block's `chainEntry` or 0).
5. **Pre-resolved chain exits**: every static chain-exit sentinel
   in the segment whose target is in the segment gets overwritten
   with the target's absolute `chainEntry` at load time — no
   runtime backpatching for static edges.
6. **JALR hot path** emits a bounds check against the current
   segment, followed by mask-bounded decoder_cache load + JMP.
7. **Fallback path preserved**: JALR whose target is outside the
   AOT segment (or hits a zero slot from an untranslated region)
   falls through to the existing lazy-compile path + 2-way IC.
   This is the correctness backstop for LuaJIT and any
   dynamic-codegen guest — dynamic code runs via the existing
   lazy compiler (correct, interpreter-pace at worst).

Not in scope for this plan:

- **Multiple segments / dynamic segment creation** (Phase 2b).
  LuaJIT-style hot loops will still work correctly via the lazy
  path; they just won't get fast dispatch until Phase 2b lands.
- **Stale detection** (Phase 2b). Guest writing to an
  already-compiled PC is a known limitation, unchanged from today.
- **FENCE.I** (Phase 2b). Currently treated as a no-op; keep that.

## Investigation findings (baseline)

1. **`scanRegion`** (`jit_decode.go:104–151`) currently does BFS
   per emit call. We add a linear scanner alongside it for AOT;
   lazy fallback retains the existing BFS path.
2. **`classifyFlow`** (`jit_decode.go:25–90`) returns RVC-aware
   instruction size and CF kind. Reused by the new linear scanner.
3. **Benchmark `.text` is tiny**: dhrystone 2.9 KB, coremark
   8.1 KB. Compile time + decoder_cache size negligible.
4. **`elf.go:76–172` `LoadELF`** returns only the entry PC today.
   Must also expose `.text` base/size — `FindSymbolAddr`
   (elf.go:208–293) shows the section-header walk pattern to reuse.
5. **`patchChainTarget`** (`jit.go:477–481`) works on any RWX
   address + offset. Single big mmap is compatible.
6. **`compiledBlock.fn`** is opaque `uintptr`; trampoline
   (`internal/jitcall/call_amd64.s`) doesn't care where code
   lives.
7. **Existing lazy-compile path** (`jit.go:433–458`) is retained
   for JALRs outside the AOT segment.
8. **`LowerAMD64`** expects per-block `goasm.Ctx`. We keep that:
   lower each block in its own ctx, concatenate assembled bytes
   into the segment's native-code mmap. No global-lowering
   refactor.

## Design

### Part A — Read `.text` bounds from the ELF

`elf.go` extension: alongside the entry PC, also return `.text`
base VMA and size. Parse section headers for a `SHT_PROGBITS`
entry whose name (via `.shstrtab`) is `.text`. Share a helper
with `FindSymbolAddr`.

```go
func LoadELFBytes(mem *GuestMemory, elfBytes []byte) (entry, textBase, textSize uint64, err error)
```

Callers that don't need the new values ignore them.

### Part B — Pass 1: collect static branch targets

New file `aot.go`. One linear walk of
`[textBase, textBase+textSize)`:

```go
func collectBranchTargets(mem *GuestMemory, textBase, textSize uint64) (
    targets map[uint64]struct{},
    terminatorFallthroughs map[uint64]struct{},
) {
    targets = {}
    termFT  = {}
    pc := textBase
    for pc < textBase+textSize:
        insn, size := fetchInsn(mem, pc)
        flow := classifyFlow(insn, size)
        switch flow.kind:
            case branchCond, branchUncond, jalDirect:
                t := pc + int64(flow.immOffset)
                if t in [textBase, textBase+textSize):
                    targets.add(t)
            case terminator:  // JALR, ECALL, EBREAK, JAL-with-link
                termFT.add(pc + size)
        pc += size
    return
}
```

No BFS, no worklist. Strictly sequential.

### Part C — Pass 2: enumerate block ranges + emit IR per block

```go
type blockPlan struct {
    startPC uint64
    endPC   uint64
    result  *emitResult
}

func AOTEnumerateBlocks(mem *GuestMemory, textBase, textSize uint64) []blockPlan {
    targets, termFT := collectBranchTargets(mem, textBase, textSize)
    starts := sort({textBase} ∪ targets ∪ termFT)
    // Restrict to PCs strictly inside [textBase, textBase+textSize).

    var plans []blockPlan
    for i := 0; i < len(starts); i++:
        startPC := starts[i]
        endPC   := textBase + textSize
        if i+1 < len(starts):
            endPC = starts[i+1]
        res := emitBlockLinear(mem, startPC, endPC)
        if res == nil || res.numInsns == 0:
            continue  // untranslatable — DecoderData slot stays 0;
                      // lazy fallback at run time
        plans = append(plans, blockPlan{startPC, res.endPC, res})
    return plans
}
```

`emitBlockLinear(mem, startPC, endPC)` is a variant of
`emitBlock` that walks instructions sequentially from `startPC`
up to `endPC` or the first terminator — no BFS, no
fall-through-follow beyond the range. Reuses `emit32`, `emitRVC`,
and the label/defer machinery. Internal branches (both endpoints
in this block) resolve via labels; branches leaving the range
become chain exits (same as today).

### Part D — Batch compile into the segment

New `jit_native.go` function:

```go
func (j *JIT) jitCompileAOTSegment(plans []blockPlan, vaddrBegin, vaddrEnd uint64) (*DecodedExecuteSegment, error)
```

Steps:

1. For each plan, run the per-region pipeline in a fresh
   `goasm.Ctx`: `Allocate → LowerAMD64 → ctx.Assemble`. Capture
   `(bytes, LowerResult)`.
2. Sum byte lengths, allocate one native-code mmap via
   `allocExec(total)`.
3. Copy each plan's bytes at its assigned offset. Add offset to
   every captured `Prog.Pc` so chainEntry / chainExits / jalrICs
   become global byte offsets inside the segment's native mmap.
4. Build each `compiledBlock`: `fn = &nativeMmap[baseOffset]`,
   `chainEntry = fn + chainEntryProg.Pc`, jalrICs populated via
   `backpatchJalrICs` (unchanged).
5. **Pre-resolve static chain exits.** For every chain-exit whose
   target PC is in the AOT set, write the target's absolute
   `chainEntry` into the MOVABS imm64 directly. No runtime
   chain-exit backpatching for static edges.
6. **Build the `decoder_cache`** (the DecoderData array):
   - `minSize = (vaddr_end - vaddr_begin) / 2 * 8` bytes.
   - Round `cacheSize` up to next power of two.
   - `mmap(cacheSize, PROT_READ|PROT_WRITE, MAP_ANON|MAP_PRIVATE)`.
   - For every compiled block at guest PC `p`:
     `*(uintptr*)(cache_base + ((p - vaddr_begin)/2)*8) =
     block.chainEntry`. Slots for untranslatable PCs remain 0.
   - `mprotect(cache_base, cacheSize, PROT_READ)` — make read-only.
7. Populate the `DecodedExecuteSegment`:
   ```go
   seg := &DecodedExecuteSegment{
       vaddrBegin:       vaddrBegin,
       vaddrEnd:         vaddrEnd,
       nativeCodeBase:   uintptr(&nativeMmap[0]),
       nativeCodeSize:   len(nativeMmap),
       decoderCacheBase: cache_base,
       decoderCacheMask: cacheSize - 1,
       blocks:           map[uint64]*compiledBlock{...}, // AOT + lazy
   }
   ```
8. Return the segment.

### Part E — DecodedExecuteSegment + dispatch

```go
type DecodedExecuteSegment struct {
    vaddrBegin       uint64                    // guest VA range
    vaddrEnd         uint64
    nativeCodeBase   uintptr                   // unified native code mmap
    nativeCodeSize   int
    decoderCacheBase uintptr                   // DecoderData[] mmap (RO)
    decoderCacheMask uint64                    // power-of-two - 1
    blocks           map[uint64]*compiledBlock // PC → block (AOT + any
                                                 // lazy addition during run)
}
```

In Phase 2a, the JIT holds exactly one segment (the AOT segment
for `.text`), plus the existing lazy-blocks infrastructure for
everything else.

`RunJIT` dispatch pseudocode:

```go
seg := j.aotSegment
if pc >= seg.vaddrBegin && pc < seg.vaddrEnd {
    // Fast path: PC in AOT segment — try decoder_cache.
    // (But dispatch from Go takes the blocks map anyway, since
    // the first-call fn-pointer lookup is easier via the map.
    // The decoder_cache is primarily read from JIT-emitted JALR
    // code, not from the Go dispatch loop.)
    blk := seg.blocks[pc]
    if blk != nil {
        res := jitcall.Call(blk.fn, ...)
        ...
    }
} else {
    // Lazy path — existing code, unchanged.
    blk := j.lazyCompile(pc)
    ...
}
```

### Part F — JALR hot path using the decoder_cache (the big win)

This is where performance arrives. The JALR sequence emitted in
every AOT block replaces the current 2-way IC hot path. Hit cost:
~9 cycles including branch-predicted conditionals.

Host pinning via sret-buffer extension (trampoline stashes once
at entry, stable across chained entries, same technique as fcsr
at `[RBX+80]`):

- `[RBX+88]`  = `decoderCacheBase` (current segment's DecoderData mmap)
- `[RBX+96]`  = `decoderCacheMask` (current segment's mask = size-1)
- `[RBX+104]` = `vaddrBegin` (current segment's guest VA start)
- `[RBX+112]` = `vaddrEnd - vaddrBegin` (current segment's size)

JALR sequence replacing the current 2-way IC for ret-form JALR
(`rs1 == x1`, `rd == x0`) and indirect-call JALR alike:

```asm
; Input: tgt = (rs1 + imm) & ~1, in some allocatable reg.
; Writeback already done via WriteBackAll.
;
; --- correctness: is tgt in the current segment? ---
MOVQ   104(RBX), R10            ; R10 = vaddrBegin
SUBQ   R10, tgt                 ; off = tgt - vaddrBegin (may underflow)
CMPQ   tgt, 112(RBX)            ; off < segSize?  (unsigned)
JAE    .dcache_miss              ; out of segment → fallback
;
; --- mask-bounded DecoderData load (same shape as guest lw) ---
SHRQ   $1, tgt                  ; idx = off / 2 (2-byte insn alignment)
SHLQ   $3, tgt                  ; byte offset = idx * 8
ANDQ   96(RBX), tgt             ; offset & decoderCacheMask
MOVQ   88(RBX), R10             ; decoderCacheBase
MOVQ   (R10, tgt), R11          ; chainEntry = decoder_cache[idx]
;
TESTQ  R11, R11
JZ     .dcache_miss              ; 0 → PC inside untranslated block
ADDQ   $frameSize, RSP          ; dealloc our frame (if frameSize > 0)
JMP    R11
.dcache_miss:
    ; existing 2-way IC sequence (Phase 1.5, unchanged) — rare case:
    ; JALR to outside AOT segment, or to a PC inside an untranslated
    ; block within the segment (e.g., CSR block).
```

- The range check on `tgt` is a correctness gate.
- The `AND mask` is the sandbox-pattern mask — even if range
  check were buggy, the mask keeps the load inside the
  segment's decoder_cache mmap.
- The existing 2-way IC sequence stays as the fallback; it
  almost never fires for code inside the AOT segment.

### Part G — Lazy fallback (correctness path for dynamic code)

Preserved and essential:

1. `RunJIT` loop sees `pc` outside `aotSegment.[begin, end)`, OR
   JALR's decoder_cache hit was 0 (untranslated block inside the
   segment).
2. Consult `j.lazyBlocks` map.
3. If found, dispatch as today.
4. If not, call the existing `emitBlock` + `jitCompileWith` path.
   Compile the block into a separately-allocated mmap. Store in
   `lazyBlocks`.

This is how LuaJIT-style guests get correct execution in Phase 2a.
Dynamic code runs at interpreter/lazy-JIT pace, not AOT pace —
but it runs correctly.

Lazy-compiled blocks chain back to AOT blocks via the existing
MOVABS-sentinel + runtime-backpatch machinery (unchanged). Only
static edges inside the AOT segment are pre-resolved at load.

### Part H — Forward compatibility with Phase 2b

Phase 2b will add:

- Multiple `DecodedExecuteSegment`s (a `[]*DecodedExecuteSegment`
  on the JIT struct).
- A "current segment" slot in the sret buffer that gets swapped
  when JALR crosses a segment boundary.
- **Dynamic segment creation on demand**: when JALR's target is
  outside every known segment, Go-side logic scans guest memory
  for a contiguous run of valid instructions around `tgt`,
  compiles it as a new segment, and switches to it. Matches
  libriscv's `next_execute_segment()` model.
- Stale detection + FENCE.I handling.

Phase 2a's design is forward-compatible: the JALR asm sequence
already reads "current segment" pointers from the sret buffer
rather than hardcoding the single AOT segment's addresses. In
Phase 2b, the sret slots change on segment switch; the emitted
JALR code doesn't need to change.

## Files to modify

| file | change |
|------|--------|
| `elf.go` | add `.text` base/size to `LoadELFBytes` return; share section-header helper with `FindSymbolAddr` |
| `aot.go` (new) | `collectBranchTargets`, `AOTEnumerateBlocks`, `AOTCompileELF` top-level driver |
| `jit_decode.go` | expose `classifyFlow` + a `fetchInsn` helper |
| `jit_emit.go` + `jit_emit_ir.go` | `emitBlockLinear(mem, startPC, endPC)` — variant without BFS |
| `jit_native.go` | new `jitCompileAOTSegment(plans, vaddrBegin, vaddrEnd)`: batch lower, build native-code mmap, build decoder_cache mmap, pre-resolve static chain exits |
| `jit.go` | `DecodedExecuteSegment` struct; `JIT.aotSegment *DecodedExecuteSegment`; `JIT.lazyBlocks map[uint64]*compiledBlock` for the fallback path; `RunJIT` dispatch consults `aotSegment.blocks` first, then `lazyBlocks`, then lazy-compiles |
| `ir/emit.go` + `ir/highlevel.go` | emitter API for JALR-via-decoder-cache sequence |
| `ir/lower_amd64.go` | `lowerJalrDecoderCache` emitting the 9-insn lookup; on miss, fall into the existing 2-way IC lowering |
| `internal/jitcall/call_amd64.s` | publish current segment's `decoderCacheBase` / `decoderCacheMask` / `vaddrBegin` / `segSize` at `[RBX+88/96/104/112]` on entry |
| `aot_test.go` (new) | linear-scan coverage, batch-compile output, pre-patch of static chain exits, decoder_cache population, RO-mapping, guest-isolation, JALR dispatch behavior |
| `bench/jit_chain_reference_test.go` | report decoder_cache hit/miss counters |

Not changed: interpreter, BudgetCheck, V2 lowerer, chain-exit
machinery (stays for AOT→lazy and lazy→lazy edges), the 2-way IC
(stays as decoder-cache-miss fallback).

## Execution order

1. **Step 1 — ELF `.text` bounds.** Extend `LoadELFBytes` to
   return `.text` base/size. Unit test against
   `llvm-objdump -h` on benchmark ELFs.
2. **Step 2 — Linear scan.** `collectBranchTargets`,
   `AOTEnumerateBlocks` in `aot.go`. Unit test: block count on
   dhrystone matches `#branch-targets + #terminator-fallthroughs
   + 1` within ±2.
3. **Step 3 — Linear emitter.** `emitBlockLinear(mem, startPC,
   endPC)`. Reuses `emit32`/`emitRVC`/label machinery. No BFS.
4. **Step 4 — Batch compile + decoder_cache.** `jitCompileAOTSegment`.
   Unit tests: every block's `chainEntry > 0`, every static
   chain-exit imm64 is a real target address (not the sentinel),
   every AOT-translated PC has the correct `decoder_cache` entry,
   untranslatable PCs have zero entries, decoder_cache is RO
   post-load.
5. **Step 5 — Dispatch swap.** Replace direct-mapped cache with
   `aotSegment.blocks` + `lazyBlocks`. All existing tests green.
6. **Step 6 — Measure (AOT without new JALR path).** 10-run
   median of `make bench-chain-ref`. Expect small gain from
   eliminated runtime chain-exit patches on static edges.
7. **Step 7 — JALR decoder_cache lowering.**
   - Extend trampoline to stash 4 new sret slots.
   - Add `lowerJalrDecoderCache` emitting the 9-insn sequence
     with fallback into the existing 2-way IC.
   - Flip `emitJALR` to call `lowerJalrDecoderCache`.
8. **Step 8 — Tests for dispatch behavior.**
   - Ret-form JALR to AOT PC: decoder_cache hit, direct JMP, no
     Go round-trip.
   - Ret-form JALR to non-AOT PC (e.g., synthesized dynamic
     code): range-check fails, falls into 2-way IC → lazy
     compile → dispatch. Correct execution.
   - Indirect-call JALR (CoreMark's `cmp`): same decoder_cache
     path as ret; hits if target in segment.
9. **Step 9 — Full regression + benchmark.**
   - `go test ./... ./ir/ ./bench/ ./riscv-elf-tests/...`
   - `make fuzz-oracle`, `fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
     `fuzz-bitmanip` (30 s each).
   - `make bench-chain-ref` × 10 runs; medians vs targets.
10. **Step 10 (optional) — Direct-JMP at static edges.** Replace
    MOVABS+JMP R10 with JMP rel32 for AOT-target chain exits.
    Code-size polish.

## Verification

### Correctness

- `go test ./... ./ir/ ./bench/`
- `go test ./riscv-elf-tests/...`
- `make fuzz-oracle`, `fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
  `fuzz-bitmanip` (30 s each).

### AOT-specific (new `aot_test.go`)

- **Linear scan coverage**: for each benchmark ELF,
  `len(AOTEnumerateBlocks(...))` within ±2 of
  `#branch-targets + #terminator-fallthroughs + 1`.
- **Batch compile sanity**: every block's `chainEntry` within
  `[nativeCodeBase, nativeCodeBase+nativeCodeSize)` and non-zero.
- **Static pre-patch**: every chain-exit whose target is in the
  AOT segment has its MOVABS imm64 replaced with that target's
  absolute chainEntry (not the sentinel).
- **decoder_cache population**: for every compiled block, the
  slot at `decoderCacheBase + ((p - vaddrBegin)/2)*8` equals
  `block.chainEntry`. Untranslatable ranges are zero.
- **decoder_cache is read-only**: test attempts a raw write to
  `decoderCacheBase` via `(*uintptr)(unsafe.Pointer(...))` after
  recovering a SIGSEGV — confirms RO.
- **decoder_cache isolated from guest `lw`**: a guest-style load
  computing `base + (addr & guest_mask)` with `addr =
  decoderCacheBase` targets the main guest memory region, never
  the decoder_cache mmap.

### Dispatch

- **In-segment ret**: JALR to an AOT-compiled PC dispatches via
  decoder_cache; no Go round-trip (`DispatchOK` counter does not
  bump for that call).
- **Out-of-segment ret**: JALR to a PC outside `aotSegment`'s
  range (e.g., a dynamic mmap region used by a LuaJIT-style
  guest): falls into 2-way IC → lazy compile → dispatch. Correct
  execution. `JalrICMisses` counter bumps.
- **Untranslated-in-segment**: JALR lands on a PC inside the AOT
  segment but in a CSR-containing block that `emitBlockLinear`
  skipped. `decoder_cache[idx] == 0`, falls into 2-way IC →
  lazy path → interpreter fallback.
- **Hostile out-of-range `ra`**: a wild `ra` value (e.g., 0 or
  far above `.text`) fails the bounds check cleanly, takes the
  fallback path, no crash.

### Performance targets

`make bench-chain-ref` × 10 runs, medians:

- bench_guest: within ±5% of baseline (3210 MIPS).
- CoreMark: ≥ 1200 MIPS (≥ 1.6 × baseline 707).
- Dhrystone: ≥ 900 MIPS (≥ 1.7 × baseline 511).

Counter expectations on return-dominated benchmarks:

- `DispatchOK` drops dramatically (pretty much only
  BudgetCheck returns).
- `ChainPatchedJalr` → near zero (no runtime IC patching under
  AOT).
- `JalrICMisses` → near zero (IC fires only for untranslatable
  / out-of-segment cases, which don't exist in these benchmarks).
- `JalrICDeopts` → 0.

For LuaJIT-style guests (not covered by current benchmarks,
but expected behavior):

- `.text` code hits decoder_cache fast path: fast.
- Dynamic JITted code falls into lazy path: slow but correct.
  Addressed in Phase 2b.

## Risks / edge cases

- **Per-region lowering stays isolated** in its own `goasm.Ctx`.
  No global regalloc or label refactor.
- **Untranslatable opcodes** (e.g., CSR) return nil from
  `emitBlockLinear`; block omitted; decoder_cache slot stays 0;
  lazy fallback handles at runtime.
- **`.text` interleaved with data** isn't the case on our
  benchmarks. Design relies on this. If a future ELF breaks it,
  `emitBlockLinear` skips unparseable ranges — adequate for
  safety, not exercised here.
- **`DebugV1V2` mode** assumes lazy per-block compile. Deferred.
- **Sret buffer extension** beyond `[RBX+80]` fcsr: four new
  8-byte slots at 88/96/104/112. Isolated to trampoline and the
  new JALR lowerer.
- **decoder_cache size for larger ELFs**: proportional to
  `(.text size / 2) × 8` rounded to next power of two. Typical
  ELF `.text` of 1 MB → ~4 MB cache. Acceptable.
- **Guest jumps to data region** (hostile / buggy): bounds check
  fails, fallback fires, no crash.
- **Self-modifying guest code within `.text`**: our
  decoder_cache is RO, so guest stores to `.text` addresses
  (via its own `base + (addr & mask)` path, which reaches main
  guest memory, not decoder_cache) will update the guest's
  memory but not the decoder_cache. Dispatch continues with
  stale translations. Known limitation; Phase 2b adds stale
  detection. Out of scope now.
- **Guest dynamic-codegen outside `.text`** (LuaJIT / V8 /
  similar): runs via the lazy path in Phase 2a. Correct but
  slow. Phase 2b adds multi-segment for fast dispatch.

## Non-goals (Phase 2a)

- No interpreter changes.
- No BudgetCheck / MaxIC changes.
- No V2 lowerer changes.
- 2-way JALR IC stays — it is the decoder-cache-miss fallback.
- MOVABS-sentinel chain-exit machinery stays for AOT → lazy
  edges and lazy → lazy edges.
- **No multiple segments** — exactly one `DecodedExecuteSegment`
  (for `.text`). Phase 2b adds dynamic segment creation.
- **No stale detection** — guests writing to `.text` get stale
  translations. Phase 2b addresses.
- **No FENCE.I handling** — remains a no-op. Phase 2b addresses.
- No guest-code introspection beyond static `.text` walk at load.
- No global IR lowering (per-block `goasm.Ctx` preserved).
- Deferred to later phases: bounds-check hoisting, GP constant
  propagation, raising MaxIC, syscall-clobber optimization,
  segment translation cache.

## Phase 2b — sketch (not part of this plan)

For reference, so the Phase 2a design doesn't paint us into a
corner:

- Turn `j.aotSegment` into `j.segments []*DecodedExecuteSegment`.
- Add "current segment" to the sret buffer; update on
  segment-crossing dispatch (Go side).
- On JALR miss in current segment + miss in all other segments:
  call `nextExecuteSegment(pc)` which scans guest memory around
  `pc` for a contiguous run of valid instructions, linear-scans
  + compiles them into a new segment, appends to `segments`.
- Mirror libriscv's `is_likely_jit` detection by noting when a
  segment's backing pages are writable — if yes, the segment is
  a LuaJIT-style region and we plan for re-decode on next miss.
- Add illegal-bytecode / stale detection: when a dispatch into a
  stale translation trips a marker, mark the segment stale and
  re-decode on next access. Same pattern as libriscv's
  `cpu_dispatch.cpp:257-259`.
- FENCE.I: optionally invalidate the segment containing the
  current PC, or the segment containing the `rs1` address if the
  instruction implies a narrower scope. Safe default: invalidate
  current segment.
