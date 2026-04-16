# Go-Native JIT Backend: Tiny IR + Extracted obj Encoders

## Note: superceded by PLAN006B with more detail.

## Context

Our current JIT emits C source → TCC → native code via cgo. This works (1924 MIPS) but has three problems we want to fix:

1. **LGPL**: TCC is LGPL. We want BSD-3-Clause or MIT on our code. (The Go standard library — including `cmd/internal/obj` — is BSD-3-Clause, compatible with our goals.)
2. **cgo tax**: every block compilation crosses the Go/C boundary. This is a cold-path cost but adds up and locks us to cgo-capable platforms.
3. **Parsing overhead**: TCC lexes, parses, and typechecks C we already know is well-formed. We emit it; no need to re-parse it.

**Goal**: pure-Go JIT backend with the readability of writing C, the code quality of TCC or better, and cold-path performance 10-100x faster (because we skip lex/parse/typecheck entirely).

**Non-goal**: port TCC. We're replacing it with something better-suited to our exact needs.

## Specification Language: Go, Not C

C gave us readable arithmetic, free register allocation, free instruction selection, structured control flow. We can get all of this from Go with a thin abstraction layer:

```go
// Today's C emission:
// e.emit("r%d = r%d + r%d;\n", rd, rs1, rs2)

// Proposed Go-native emission:
e.Add(e.VR(rd), e.VR(rs1), e.VR(rs2))
```

The `e.Add(...)` call builds an `IRInstr{Op: IRAdd, ...}` and appends to the block's instruction list. No strings, no parsing, no cgo.

**Readability comparison** (real-world example from our emitter):

Today's C emission for a load:
```c
{ uint64_t addr = r10 + 0LL;
  if (__builtin_expect((addr | (addr+7)) & ~mem_mask, 0)) {
    x[1] = r1; x[10] = r10;
    return (JITResult){0x1000ULL, ic, 3, addr}; }
  r1 = (int64_t)(*(int64_t*)(mem_base + (addr & mem_mask)));
}
```

Equivalent Go-native emission:
```go
addr := e.Tmp()                         // fresh vreg for addr
e.Add(addr, e.VR(rs1), e.ImmI(imm))     // addr = rs1 + imm
e.CheckBounds(addr, 8, e.FaultExit(pc, jitLoadFault))  // masked OOB check
e.LoadSigned(e.VR(rd), addr, 8)          // r[rd] = signed 8-byte load at (mem_base + addr&mask)
```

**Fewer lines, type-checked at compile time, no string formatting, composable helpers.**

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  jit_emit.go  — RISC-V decoder (mostly unchanged structure)  │
│    Walks bytes, calls emitter helpers like e.Add, e.Load...  │
│    Produces []IRInstr (target-agnostic)                      │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  ir/regalloc.go  — Linear-scan register allocator (~400 LoC) │
│    Assigns HostReg or StackSlot to each VReg                 │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  ir/lower_amd64.go  —  IR → obj.Prog  (~3K LoC)              │
│  ir/lower_arm64.go  —  IR → obj.Prog  (~3K LoC)              │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  internal/goasm/obj/  — Extracted from $GOROOT, BSD-3-Clause │
│    obj.Prog → machine code bytes                             │
│    Uses Go's mature encoders for amd64, arm64, riscv64,      │
│    ppc64, mips64, s390x, wasm                                │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  internal/jitcall/call_{amd64,arm64}.s  — Trampolines (exist)│
└──────────────────────────────────────────────────────────────┘
```

## The IR: ~30 Ops

Target-agnostic 3-address code:

```go
type IROp uint8
const (
    IRLoad IROp = iota  // dst = load[T](base + off)
    IRStore             // store[T](base + off, src)
    IRAdd; IRSub; IRMul; IRDivS; IRDivU; IRRem
    IRShl; IRShr; IRSar
    IRAnd; IROr; IRXor; IRNot; IRNeg
    IRMov               // reg → reg
    IRConst             // imm → reg
    IRExt               // sign/zero extend
    IRBranch            // cond + target label
    IRJump              // unconditional → label
    IRLabel
    IRCall              // external C ABI: jit_sqrtf, etc.
    IRRet               // return {pc, ic, status, fault_addr}
    IRFAdd; IRFSub; IRFMul; IRFDiv; IRFSqrt; IRFCmp; IRFCvt
    IRMulHS; IRMulHU    // high 64 of 128-bit product (for MULH etc.)
    IRSelect            // dst = cond ? a : b (for Zicond)
    IRWriteback         // writeback dirty vregs to x[] array
)

type Type uint8
const ( I8 Type = iota; I16; I32; I64; F32; F64 )

type Pred uint8       // for IRBranch/IRFCmp
const ( EQ; NE; LT; LE; GT; GE; LTU; LEU; GTU; GEU )

type VReg uint16      // virtual register, 0 = "discard" (maps to x0)

type IRInstr struct {
    Op   IROp
    T    Type
    Dst  VReg
    A, B VReg
    Imm  int64     // const / offset / label
    Pred Pred
}
```

The IR's simplicity is its virtue. Each `IRInstr` lowers to 1-3 native instructions. Bugs in the IR itself are minimal because there's so little of it.

## Extracting `cmd/internal/obj`

From previously-researched scope:

| Package | LoC | Role |
|---------|-----|------|
| `obj/` core (selected files, excluding dwarf/pcln/objfile/fips) | ~4K | Prog, Link, pass, encode |
| `obj/x86/` | ~16K (mostly generated tables) | amd64 encoder |
| `obj/arm64/` | ~13K | arm64 encoder |
| `objabi/`, `src/`, `sys/`, `hash/` | ~3K | Support |
| **Total** | **~35K** | |

Extraction strategy: copy relevant files into `internal/goasm/`, rewrite imports from `cmd/internal/...` to `riscv/internal/goasm/...`, stub out unused subsystems (DWARF emission, ELF file output, PCLN tables, FIPS). BSD-3-Clause license preserved via `internal/goasm/LICENSE`.

An extraction script (`scripts/extract-goasm.sh`) makes this reproducible on future Go releases.

## Register Allocation

Linear-scan, one pass over the IR per block. Input: VRegs used in the block. Output: each VReg mapped to either a host register (from a pool of callee-saved regs + some arg regs) or a stack slot.

Simple algorithm:
1. First pass: compute VReg live ranges (start = first def, end = last use)
2. Sort VRegs by start of live range
3. Scan: assign each VReg a free host register; if none available, spill the one whose live range ends latest
4. Record mapping for lowering pass

~400-500 LoC of Go. Same algorithm libriscv's JIT uses.

## Per-Arch Lowering

Each IR op lowers to 1-3 `obj.Prog` structs. Example lowering for `IRAdd` on amd64:

```go
// IRAdd: Dst = A + B
// If Dst and A are the same VReg and in registers, we can emit:
//   ADDQ hostB, hostA     (in-place)
// Otherwise:
//   MOVQ hostA, hostDst
//   ADDQ hostB, hostDst

func lowerAdd(ctx *LowerCtx, ins *IRInstr) {
    dst := ctx.HostReg(ins.Dst)
    a := ctx.HostReg(ins.A)
    b := ctx.HostReg(ins.B)
    if dst == a {
        ctx.Emit(x86.AADDQ, b, dst)
    } else {
        ctx.Emit(x86.AMOVQ, a, dst)
        ctx.Emit(x86.AADDQ, b, dst)
    }
}
```

Each op is a small function like this. ~3K LoC per arch for all ops.

## Phased Implementation

### Phase 1: Extract obj (3-4 weeks)

- `scripts/extract-goasm.sh`: copies $GOROOT files, rewrites imports
- Get `internal/goasm/` to compile standalone
- Validation: assemble known instruction sequences, compare bytes to `go tool asm` output

### Phase 2: IR definition + emitter helpers (2 weeks)

- Define `IRInstr`, `IROp`, `Type`, `Pred`, `VReg`
- Emitter helpers: `e.Add`, `e.Load`, `e.Store`, `e.Branch`, ...
- ~40 helper methods total, each ~5-10 LoC

### Phase 3: Linear-scan register allocator (2-3 weeks)

- Live range analysis over IR
- Allocation algorithm
- Spill/reload for overflow
- Unit tests with synthetic IR programs

### Phase 4: amd64 lowering (4-6 weeks)

- One lowering function per IR op
- Use `obj.Prog` API for instruction encoding
- Handle addressing modes (SIB for load/store)
- Handle branch target fixup (forward refs resolved after full emission)
- Validation against existing TCC output for same RISC-V inputs

### Phase 5: Port jit_emit.go to produce IR (4-6 weeks)

- Keep `jit_emit.go`'s high-level structure (region scan, instruction decode, emit loop)
- Replace `e.emit("C text...")` calls with `e.Add(...)` etc.
- Run our full test suite (23 JIT units + ~90 lockstep) to validate correctness instruction-by-instruction
- Budget check at backward branches: emitted via `e.Branch(IRBudgetExceeded, ...)` helper

### Phase 6: arm64 lowering (3-4 weeks)

- Per-arch lowering file
- Benefits from phase 3-5 groundwork
- ARM64 AAPCS trampoline for `jitcall.Call` (analogous to amd64 one, ~50 LoC)

### Phase 7: Deprecate TCC (1 week)

- Put `jit_tcc.go` behind `//go:build tcc` build tag as fallback
- Remove `vendor/tcc/` — no longer needed
- Remove cgo from default build

## Files to Create/Modify

| Path | Action |
|------|--------|
| `scripts/extract-goasm.sh` | NEW: extraction script |
| `internal/goasm/**` | NEW: extracted obj (~35K LoC) |
| `internal/goasm/LICENSE` | NEW: BSD-3-Clause (Go's) |
| `ir/ir.go` | NEW: IR types |
| `ir/emit.go` | NEW: emitter helpers (~40 methods) |
| `ir/regalloc.go` | NEW: linear-scan allocator |
| `ir/lower_amd64.go` | NEW: IR → obj.Prog for amd64 |
| `ir/lower_arm64.go` | NEW: IR → obj.Prog for arm64 |
| `ir/fixup.go` | NEW: forward-reference label fixup |
| `jit_emit.go` | MODIFY: produce IR instead of C text |
| `jit_tcc.go` | MODIFY: `//go:build tcc` tag |
| `jit.go` | MODIFY: call into goasm backend for compilation |
| `internal/jitcall/call_arm64.s` | NEW: AAPCS trampoline |
| `Makefile` | MODIFY: remove TCC rebuild steps from default; add `tcc` tagged path |
| `vendor/tcc/` | DELETE (after phase 7) |

## Verification

```bash
# Extracted obj produces correct bytes
go test ./internal/goasm/...

# IR + lowering produces correct machine code for synthetic IR programs
go test ./ir/...

# Existing JIT tests pass on Go-native backend (default)
go test -count=1 -run 'TestJIT_' -timeout 60s .
go test -count=1 -run 'TestRISCVTests_Lockstep_U[IMAC]' -timeout 120s .
go test -count=1 -run 'TestRISCVTests_U[IMAC]_JIT' -timeout 60s .

# TCC fallback still works (regression check)
go test -count=1 -tags tcc -run 'TestJIT_' -timeout 60s .

# Throughput comparison (expect Go-native ≈ TCC, probably slightly higher)
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=3x ./bench/
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -tags tcc -benchtime=3x ./bench/

# Cold-path comparison (expect 10-100x speedup on compilation)
# (new micro-benchmark: time to compile N fresh blocks)
go test -run='^$' -bench='BenchmarkBlockCompile' -benchtime=3x .

# Cross-arch
GOARCH=arm64 go test -count=1 -run 'TestJIT_' .
```

**Expected outcomes:**
- Hot-code throughput ≈ TCC (both produce comparable native code for this workload — or better, since obj has a slightly better encoder than TCC)
- Cold-path 10-100x faster (no lex/parse/typecheck, no cgo)
- Smaller binary (no libtcc.a; no cgo runtime)
- Clean build on any Go-supported target (amd64, arm64, and eventually any of Go's other targets with small lowering ports)
- MIT/BSD-licensed end-to-end

## Timeline Estimate

| Phase | Effort |
|-------|--------|
| 1: Extract obj | 3-4 weeks |
| 2: IR + helpers | 2 weeks |
| 3: Register allocator | 2-3 weeks |
| 4: amd64 lowering | 4-6 weeks |
| 5: Port emitter | 4-6 weeks |
| 6: arm64 lowering | 3-4 weeks |
| 7: Deprecate TCC | 1 week |
| **Total** | **4-6 months for both arches** |

## Why This Is the Right Shape

- **Licensing**: BSD-3-Clause end-to-end (Go's license on obj, ours on IR + lowering + emitter)
- **Portability**: every Go-supported arch is a new lowering file away (~3K LoC), not a new encoder from scratch
- **Speed**: cold path 10-100x faster than TCC; hot path matches or beats
- **Simplicity**: ~4K LoC of hand-written Go (emitter + IR + allocator + lowering) that we own, on top of ~35K extracted LoC of Go's own encoder
- **Maintainability**: one annual re-extraction of Go's obj package; our own code stable once written
- **Validation**: ris's lockstep test suite catches codegen bugs per-instruction with exact PC — invaluable during porting

## Risk Mitigation

- **Obj extraction might be entangled**: we'll discover this in phase 1. Mitigation: if obj has unexpected dependencies we can't stub, fall back to keeping TCC (rebuild for ARM64 as interim).
- **Our register allocator might underperform TCC's**: both are simple single-pass allocators. We can port libriscv's allocator code (C, easy to translate) if ours falls short.
- **Obj API surprises**: obj is designed to produce object files, not raw bytes. We may need careful path through the assemble-but-don't-write-file flow. Mitigation: study how Go's own assembler driver works, or use `obj.Prog` without the object-file layer.

## Comparison with Alternatives (for reference)

| Path | Effort | License | Speed | Portability |
|------|--------|---------|-------|-------------|
| **Status quo (TCC + cgo)** | 0 | LGPL | TCC-level | libtcc.a per host arch |
| **Rebuild libtcc.a for ARM64** | 1-2 days | LGPL | TCC-level | Apple Silicon unblocked |
| **Port TCC to Go** | 8-12 months | LGPL (derivative) | TCC-level | Multi-arch via port per arch |
| **Go-native IR + obj (this plan)** | **4-6 months** | **BSD** | **≥ TCC** | **Free for every Go arch** |

## Design Decisions (resolved)

### Forward-reference branches — online, no second pass

Work with **labels**, not byte offsets, throughout IR construction and lowering. Every branch targets a label ID (symbolic). The lowering pass emits `obj.Prog` with `obj.TYPE_BRANCH` and a `Pcond` pointer to the target Prog. `obj` resolves Prog→byte-offset internally when we call `Assemble()`.

So from **our** perspective, there's never a second pass over the IR or Progs for branch fixup — we just assign labels and emit branches that reference them. Forward refs are resolved automatically at final assembly by obj's encoder.

**Implementation**: at IR emit time, when we encounter a label that hasn't been emitted yet, we allocate a label ID and note it. When the target IR instruction is emitted, we record `labelID → *IRInstr`. During lowering, when we emit a `Prog` whose target is a label, we either:
- Point `Prog.Pcond` at the already-emitted Prog (backward ref — trivial), OR
- Queue a pending-pointer (forward ref) that gets filled in when the target Prog is later emitted.

No full re-pass. Forward refs are patched as their targets appear, one at a time, as we go.

This also benefits from **always using long-form branches** (fixed 6-byte conditional branches on amd64, 4-byte on arm64). Size doesn't change based on target distance, so peephole rewrites upstream never invalidate downstream branch encodings.

### Peephole optimization — online, sliding window

Maintain a small window of the last N (~4-8) emitted `IRInstr` entries. When emitting a new one, check for patterns against the window. If matched, splice the window entries and replace with the optimized sequence.

Examples:
- `IRMov a, a` → no-op (don't emit)
- `IRAdd dst, x, 0` or `IRMul dst, x, 1` → `IRMov dst, x`
- `IRConst t, 0; IRStore _, t` → `IRStore _, immzero` (x86 has direct immediate-zero stores)
- `IRXor t, t, t` → `IRConst t, 0` (zero idiom; on amd64 it's already efficient)
- `IRShl t, x, 1` + load/store → sometimes foldable into addressing mode (arch-specific peephole at lowering time)

**Two-layer peephole**:
1. **IR-level peephole** (arch-neutral, during IR emission): arithmetic simplifications, redundant moves. ~200 LoC.
2. **Lowering-level peephole** (arch-specific, during Prog emission): address-mode folding, LEA tricks on amd64, shifted-reg operands on arm64. ~200 LoC per arch.

Both are online with sliding windows — no dedicated second pass. Peephole during IR emission can't invalidate branch targets because branches reference labels, not byte offsets.

### IR helpers — both high- and low-level

**Low-level** (one IR op each, ~40 methods):
```go
e.Add(dst, a, b)
e.Mov(dst, src)
e.Load(dst, base, off, I64, signed)
e.Branch(pred, a, b, label)
```

**High-level** (macros that emit multiple IR ops, ~15 methods for common patterns):
```go
e.MaskedLoad(dst, base, off, I64, signed, faultExit)  // bounds check + load
e.GuestStore(base, off, val, I32, faultExit)           // bounds check + store
e.WriteBackAll()                                        // dirty vregs → x[] array
e.FaultExit(pc, faultKind)                              // writeback + return JITResult
e.BudgetCheck(target)                                   // if (ic < MAX_IC) goto target; else writeback+return
```

The high-level helpers are implemented in terms of the low-level ones. Both are available. The emitter uses high-level for common patterns, drops to low-level for one-off tweaks.
