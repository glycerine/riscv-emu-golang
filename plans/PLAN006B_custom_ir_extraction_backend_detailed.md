# Go-Native JIT Backend: Tiny IR + Extracted obj Encoders

The detailed version of PLAN006A.

## Context

Our current JIT: RISC-V → C source → TCC → native code via cgo. Works (1924 MIPS) but has three problems:

1. **LGPL** on TCC. We want BSD-3-Clause / MIT.
2. **cgo tax** on every block compilation.
3. **Parsing overhead**: TCC lexes/parses/typechecks C we already know is well-formed.

**Goal**: pure-Go JIT backend. Readability of writing C, code quality ≥ TCC, cold path 10-100× faster, multi-arch for free via Go's extracted obj encoders.

**End state**: no cgo (for the JIT path), no libtcc.a, BSD-licensed end-to-end, amd64 + arm64 on day one, other Go-supported arches available by writing ~3K LoC lowering files.

## Architecture Diagram

```
┌──────────────────────────────────────────────────────────────────────┐
│  jit_emit.go  — RISC-V decoder (structure unchanged)                 │
│    Walks guest bytes, invokes emitter helpers:                       │
│      e.Add(dst, a, b), e.MaskedLoad(...), e.BudgetCheck(...)         │
│    Produces []IRInstr (target-agnostic)                              │
└──────────────────────────────────────────────────────────────────────┘
                                 │
                                 ▼
┌──────────────────────────────────────────────────────────────────────┐
│  ir/emit.go  — Emitter helpers (low + high level)                    │
│  ir/peephole.go  — Sliding-window optimizer (online, no 2nd pass)    │
└──────────────────────────────────────────────────────────────────────┘
                                 │
                                 ▼
┌──────────────────────────────────────────────────────────────────────┐
│  ir/regalloc.go  — Linear-scan register allocator                    │
│    VRegs → HostReg or StackSlot                                      │
└──────────────────────────────────────────────────────────────────────┘
                                 │
                                 ▼
┌──────────────────────────────────────────────────────────────────────┐
│  ir/lower_amd64.go / ir/lower_arm64.go  — IR → obj.Prog              │
│    Per-op lowering functions                                         │
│    Online label fixup (forward refs patched as targets emit)         │
│    Arch-specific peephole (address-mode folding, LEA, etc.)          │
└──────────────────────────────────────────────────────────────────────┘
                                 │
                                 ▼
┌──────────────────────────────────────────────────────────────────────┐
│  internal/goasm/  — Extracted from $GOROOT, BSD-3-Clause             │
│    obj.Prog → machine code bytes                                     │
│    Reuses Go's mature amd64 / arm64 encoders                         │
└──────────────────────────────────────────────────────────────────────┘
                                 │
                                 ▼
┌──────────────────────────────────────────────────────────────────────┐
│  internal/jitcall/call_{amd64,arm64}.s  — Trampolines                │
│    Call into mmap'd code buffer with ABI-appropriate reg setup       │
└──────────────────────────────────────────────────────────────────────┘
```

---

# Phase 1: Extract `cmd/internal/obj` — Detailed Implementation

## Goal
A self-contained, buildable Go package at `internal/goasm/` that exposes Go's amd64 and arm64 instruction encoders as a library. Pure BSD-3-Clause (inherited from Go).

## Step 1.1: Write `scripts/extract-goasm.sh`

Bash script that copies selected files from `$GOROOT/src/cmd/internal/` to `internal/goasm/`, rewrites imports, and skips files we don't need.

**Full script** (add to repo as `scripts/extract-goasm.sh`):

```bash
#!/usr/bin/env bash
set -euo pipefail

GOROOT=${GOROOT:-$(go env GOROOT)}
DEST=internal/goasm
SRC=$GOROOT/src/cmd/internal

# Files to copy from each source package
declare -A KEEP=(
    [obj]="abi_string.go addrtype_string.go data.go go.go inl.go ld.go line.go link.go pass.go plist.go stringer.go sym.go textflag.go util.go"
    [obj/x86]="*.go"
    [obj/arm64]="*.go"
    [objabi]="*.go"
    [src]="*.go"
    [sys]="*.go"
    [hash]="*.go"
)

# Files explicitly excluded even when using glob
declare -A SKIP=(
    [obj]="dwarf.go fips140.go mkcnames.go objfile.go pcln.go line_test.go objfile_test.go sizeof_test.go"
    [obj/x86]="asm_test.go obj6_test.go pcrelative_test.go"
    [obj/arm64]="asm_test.go"
)

rm -rf "$DEST"
mkdir -p "$DEST"

for pkg in "${!KEEP[@]}"; do
    mkdir -p "$DEST/$pkg"
    for pattern in ${KEEP[$pkg]}; do
        for f in $SRC/$pkg/$pattern; do
            [ -f "$f" ] || continue
            base=$(basename "$f")
            skip=0
            for s in ${SKIP[$pkg]:-}; do
                if [ "$base" = "$s" ]; then skip=1; break; fi
            done
            [ $skip -eq 1 ] && continue
            cp "$f" "$DEST/$pkg/$base"
        done
    done
done

# Rewrite imports: cmd/internal/... → riscv/internal/goasm/...
find "$DEST" -name '*.go' | while read f; do
    sed -i.bak -E '
        s|"cmd/internal/obj"|"riscv/internal/goasm/obj"|g
        s|"cmd/internal/obj/x86"|"riscv/internal/goasm/obj/x86"|g
        s|"cmd/internal/obj/arm64"|"riscv/internal/goasm/obj/arm64"|g
        s|"cmd/internal/objabi"|"riscv/internal/goasm/objabi"|g
        s|"cmd/internal/src"|"riscv/internal/goasm/src"|g
        s|"cmd/internal/sys"|"riscv/internal/goasm/sys"|g
        s|"cmd/internal/hash"|"riscv/internal/goasm/hash"|g
        s|"cmd/internal/dwarf"|"riscv/internal/goasm/stub"|g
        s|"cmd/internal/goobj"|"riscv/internal/goasm/stub"|g
    ' "$f"
    rm "$f.bak"
done

# Copy LICENSE
cp "$GOROOT/LICENSE" "$DEST/LICENSE"
echo "Extracted to $DEST. Go version: $(go version)" > "$DEST/EXTRACTED.txt"
```

## Step 1.2: Write stub package for removed deps

Create `internal/goasm/stub/stub.go`:

```go
// Package stub provides empty implementations of cmd/internal/dwarf and
// cmd/internal/goobj that obj references but we don't need. These stubs
// let the extracted obj package compile without pulling in DWARF and
// object-file-format code we don't use (we emit raw machine code, not
// object files).
package stub

// DWARF stubs — obj uses these for debug info, which we don't emit.
type Context interface{}
type Sym struct{}
type Var struct{}
type Scope struct{}
// ... add stubs as compiler errors reveal what's needed ...

// goobj stubs — object-file format types. We don't produce object files.
// ... add stubs as needed ...
```

The initial pass will cause compile errors listing missing types. Add stubs one-by-one until it builds. Expected scope: ~50-100 lines of stubs total.

## Step 1.3: Remove calls to skipped subsystems

After imports are rewritten and stubs added, some files will reference skipped functionality. For each compile error:

- **DWARF emission calls** (in `link.go`, `plist.go`): comment out or replace with no-op stubs
- **Object file writing** (in `ld.go`, `sym.go`): comment out
- **PCLN table generation** (in `pass.go`): comment out

Expected: ~20-40 lines of surgery in `obj/*.go` to neuter unused features. Keep commented-out lines (don't delete) so future re-extractions preserve the edits as context.

**File**: `internal/goasm/EDITS.md` — running log of every manual edit. Future re-extractions reference this to reapply them.

## Step 1.4: API wrapper `internal/goasm/api.go`

Expose a minimal API on top of the extracted obj package:

```go
package goasm

import (
    "fmt"
    "riscv/internal/goasm/obj"
    "riscv/internal/goasm/obj/x86"
    "riscv/internal/goasm/obj/arm64"
    "riscv/internal/goasm/objabi"
    "riscv/internal/goasm/sys"
)

// Arch selects the target ISA.
type Arch int
const (
    AMD64 Arch = iota
    ARM64
)

// Ctx holds per-block assembler state.
type Ctx struct {
    arch   Arch
    link   *obj.Link
    prog   *obj.Prog   // first Prog in list
    last   *obj.Prog   // last appended
    text   *obj.LSym   // text symbol for our "function"
    pcDiff int64       // running PC for diagnostics
}

// New creates a Ctx for the given arch.
func New(arch Arch) *Ctx {
    var linkArch *obj.LinkArch
    switch arch {
    case AMD64:
        linkArch = &x86.Linkamd64
    case ARM64:
        linkArch = &arm64.Linkarm64
    default:
        panic("unsupported arch")
    }
    ctx := obj.Linknew(linkArch)
    ctx.Flag_optimize = false
    ctx.DiagFunc = func(msg string, args ...any) {
        // collect errors; emit via Assemble() return
    }
    // Create a dummy text symbol to hold our Progs
    text := ctx.Lookup("jit_block")
    text.Type = objabi.STEXT
    return &Ctx{arch: arch, link: ctx, text: text}
}

// NewProg allocates an empty Prog that can be configured and appended.
func (c *Ctx) NewProg() *obj.Prog {
    p := c.link.NewProg()
    p.Ctxt = c.link
    return p
}

// Append adds a Prog to the list.
func (c *Ctx) Append(p *obj.Prog) {
    if c.prog == nil {
        c.prog = p
    } else {
        c.last.Link = p
    }
    c.last = p
}

// Assemble runs obj's assembly pipeline and returns the native bytes.
// Relocates internal labels (Pcond refs); returns error on any diagnostic.
func (c *Ctx) Assemble() ([]byte, error) {
    // Populate the text symbol with our Prog list
    c.text.Func = &obj.FuncInfo{}
    // Drive the assembler:
    //   1. link.Flushplist — inlines, passes over list
    //   2. link.assembleProg — per-arch assemble via linkArch.Assemble
    if err := c.drive(); err != nil {
        return nil, err
    }
    // Return the encoded text section bytes
    return c.text.P, nil
}

// drive runs obj's assembly passes.
func (c *Ctx) drive() error {
    // Details filled in once we trace through how Go's linker drives obj.
    // Reference: src/cmd/link/internal/ld/pass.go and
    // src/cmd/internal/obj/link.go's Flushplist.
    //
    // Minimum passes needed:
    //   - obj.Flushplist(c.link, ...)  // walks Progs, prepares for assembly
    //   - linkArch.Assemble(c.link, c.text, ...)  // encodes to bytes in text.P
    return nil
}
```

**Detailed study task for phase 1**: read `$GOROOT/src/cmd/internal/obj/plist.go` (Flushplist) and `$GOROOT/src/cmd/internal/obj/x86/asm6.go` (span6/assemble) to understand the exact sequence of calls to go from `[]*Prog` to encoded bytes. Document findings in `internal/goasm/DRIVE.md`.

## Step 1.5: Register constants

Create `internal/goasm/regs.go` exposing register constants as Go names:

```go
package goasm

import (
    "riscv/internal/goasm/obj/x86"
    "riscv/internal/goasm/obj/arm64"
)

// AMD64 general-purpose registers (64-bit).
const (
    REG_AMD64_RAX = x86.REG_AX
    REG_AMD64_RCX = x86.REG_CX
    REG_AMD64_RDX = x86.REG_DX
    REG_AMD64_RBX = x86.REG_BX
    REG_AMD64_RSP = x86.REG_SP
    REG_AMD64_RBP = x86.REG_BP
    REG_AMD64_RSI = x86.REG_SI
    REG_AMD64_RDI = x86.REG_DI
    REG_AMD64_R8  = x86.REG_R8
    REG_AMD64_R9  = x86.REG_R9
    REG_AMD64_R10 = x86.REG_R10
    REG_AMD64_R11 = x86.REG_R11
    REG_AMD64_R12 = x86.REG_R12
    REG_AMD64_R13 = x86.REG_R13
    REG_AMD64_R14 = x86.REG_R14
    REG_AMD64_R15 = x86.REG_R15
)

// AMD64 SSE/XMM registers (for FP).
const (
    REG_AMD64_XMM0 = x86.REG_X0
    // ... through XMM15 ...
)

// ARM64 general-purpose registers.
const (
    REG_ARM64_X0 = arm64.REG_R0
    // ... through X30 ...
)

// ARM64 SIMD/FP registers.
const (
    REG_ARM64_V0 = arm64.REG_F0
    // ... through V31 ...
)
```

## Step 1.6: Validation

Create `internal/goasm/api_test.go` with golden tests:

```go
func TestAssembleAddAMD64(t *testing.T) {
    c := New(AMD64)

    // Emit: MOVQ $0x42, AX; RET
    p := c.NewProg()
    p.As = x86.AMOVQ
    p.From.Type = obj.TYPE_CONST
    p.From.Offset = 0x42
    p.To.Type = obj.TYPE_REG
    p.To.Reg = REG_AMD64_RAX
    c.Append(p)

    p = c.NewProg()
    p.As = obj.ARET
    c.Append(p)

    bytes, err := c.Assemble()
    if err != nil { t.Fatal(err) }

    // Expected: 48 C7 C0 42 00 00 00 C3
    want := []byte{0x48, 0xC7, 0xC0, 0x42, 0x00, 0x00, 0x00, 0xC3}
    if !bytesEqual(bytes, want) {
        t.Errorf("got %x want %x", bytes, want)
    }
}
```

Cross-check: write an equivalent `.s` file, run `go tool asm`, compare bytes. Do this for ~20 diverse instruction sequences covering arithmetic, loads/stores, branches, and FP for both amd64 and arm64.

---

# Phase 2: IR Definition — Detailed Spec

## File: `ir/ir.go`

```go
package ir

// VReg is a virtual register index. 0 is reserved for "discard" (sink writes,
// zero reads — mirrors RISC-V's x0). Emitter allocates fresh VRegs via e.Tmp()
// or uses fixed IDs 1..31 for RISC-V guest regs r1..r31, and 32..63 for f0..f31.
type VReg uint16

const (
    VRegZero VReg = 0       // discard/x0
    // 1..31   = guest x1..x31
    // 32..63  = guest f0..f31
    // 64+     = temporaries
    VRegTempStart VReg = 64
)

// Type distinguishes operand sizes and classes.
type Type uint8
const (
    I8  Type = iota
    I16
    I32
    I64
    F32
    F64
)

// Pred is a comparison predicate for IRBranch and IRFCmp.
type Pred uint8
const (
    EQ Pred = iota
    NE
    LT   // signed
    LE
    GT
    GE
    LTU  // unsigned
    LEU
    GTU
    GEU
)

// Label identifies a jump target within a block.
type Label uint16

// IROp enumerates the IR operations. ~40 ops total.
type IROp uint8

const (
    // Invalid — sentinel for uninitialized Instr.
    IROpInvalid IROp = iota

    // Memory ops (work on host memory, used for bounds-checked guest access
    // and for intra-block scratch).
    IRLoad      // Dst = load[T](Base + Imm)
    IRStore     // store[T](Base + Imm, Src)
    IRLoadX     // Dst = load[T](Base + Idx*Scale)  — indexed, amd64/arm64 hw addressing
    IRStoreX    // store[T](Base + Idx*Scale, Src)

    // Integer arithmetic
    IRAdd       // Dst = A + B
    IRAddImm    // Dst = A + Imm
    IRSub
    IRSubImm
    IRMul
    IRDivS      // signed / ; amd64 traps on div-by-zero, arm64 returns 0
    IRDivU
    IRRem
    IRMulHS     // signed high-64 of 128-bit product (MULH)
    IRMulHU     // unsigned high-64 (MULHU)
    IRMulHSU    // signed × unsigned high-64 (MULHSU)
    IRNeg
    IRShl       // logical left shift (dst, src, amount)
    IRShlImm
    IRShr       // logical right
    IRShrImm
    IRSar       // arithmetic right
    IRSarImm

    // Bitwise
    IRAnd
    IRAndImm
    IROr
    IROrImm
    IRXor
    IRXorImm
    IRNot       // bitwise complement

    // Comparison (produces 0/1 in Dst)
    IRSet       // Dst = (A pred B) ? 1 : 0
    IRSetImm    // Dst = (A pred Imm) ? 1 : 0

    // Data movement
    IRMov       // Dst = A     (reg-reg copy)
    IRConst     // Dst = Imm   (load immediate)
    IRSext      // sign-extend from T (e.g., (int64)(int32)x)
    IRZext      // zero-extend from T

    // Control flow
    IRLabel     // marks target; referenced by Imm (label id)
    IRBranch    // if (A pred B) goto label(Imm)
    IRBranchImm // if (A pred Imm2) goto label(Imm)   — Imm is label, Imm2 separate
                // (For encoding compactness we fold this into CmpImm + Branch on flags.)
    IRJump      // goto label(Imm)
    IRCall      // call external C ABI symbol; Imm = symbol index in ctab
    IRRet       // return (JITResult){pc=Imm, ic=<var>, status=Src1, fault=Src2}
                // Special-cased by lowerer; writes cached regs back.

    // FP
    IRFAdd
    IRFSub
    IRFMul
    IRFDiv
    IRFSqrt
    IRFCmp      // Dst = (A pred B) ? 1 : 0, FP compare
    IRFNeg
    IRFAbs
    IRFCvtToI   // int = convert(FP)     T=Dst type (I32/I64), U=src FP type
    IRFCvtToU   // uint
    IRFCvtFromI // FP = convert(int)
    IRFCvtFromU
    IRFCvtFF    // F32↔F64

    // Pseudo-ops for register liveness / writeback control
    IRMarkLive  // declares A live here (allocator hint)
    IRMarkDead  // declares A dead here (allocator hint)
    IRWriteback // writes dirty vregs back to x[] array (high-level helper)
)

// IRInstr is one IR operation. Fixed-size struct (no slices) for cache locality.
type IRInstr struct {
    Op    IROp
    T     Type   // operand type
    U     Type   // secondary type (for conversions)
    Dst   VReg
    A     VReg
    B     VReg   // also: Idx for IRLoadX/IRStoreX
    Imm   int64  // immediate / offset / label / call-symbol-index
    Imm2  int64  // for IRBranchImm (separate imm from label)
    Pred  Pred
    Scale uint8  // 1/2/4/8 for IRLoadX/IRStoreX
}

// Block holds the IR for a single JIT block.
type Block struct {
    Instrs  []IRInstr
    Labels  map[Label]int   // label ID → index in Instrs where IRLabel sits
    NextLabel Label         // fresh label allocator
    CTab    []CSym          // external call symbols (jit_sqrtf, jit_trace, ...)
    // Usage tracking for register allocation (populated by regalloc pass)
    VRegLive []VRegLiveness
}

// CSym describes an external C ABI symbol.
type CSym struct {
    Name string
    Addr uintptr  // function pointer in host address space
    // For calls: we emit MOV $addr, tmpreg; CALL tmpreg
}

// VRegLiveness: start and end IRInstr index for a VReg's live range.
type VRegLiveness struct {
    Start, End int
}
```

## File: `ir/emit.go` — Low-level helpers

One helper per IR op. Full list:

```go
package ir

// Emitter wraps a Block and exposes helper methods.
type Emitter struct {
    Block *Block
    dirty []bool   // dirty[vr] = true if vr has been written but not written back
    // ... plus peephole window (see Phase 2b) ...
}

// ── Integer ALU ──
func (e *Emitter) Add(dst, a, b VReg)                { e.op3(IRAdd, I64, dst, a, b) }
func (e *Emitter) AddImm(dst, a VReg, imm int64)     { e.op2i(IRAddImm, I64, dst, a, imm) }
func (e *Emitter) AddT(dst, a, b VReg, t Type)       { e.op3(IRAdd, t, dst, a, b) }
func (e *Emitter) Sub(dst, a, b VReg)                { e.op3(IRSub, I64, dst, a, b) }
func (e *Emitter) Mul(dst, a, b VReg)                { e.op3(IRMul, I64, dst, a, b) }
func (e *Emitter) DivS(dst, a, b VReg)               { e.op3(IRDivS, I64, dst, a, b) }
func (e *Emitter) DivU(dst, a, b VReg)               { e.op3(IRDivU, I64, dst, a, b) }
func (e *Emitter) Rem(dst, a, b VReg)                { e.op3(IRRem, I64, dst, a, b) }
func (e *Emitter) MulHS(dst, a, b VReg)              { e.op3(IRMulHS, I64, dst, a, b) }
func (e *Emitter) MulHU(dst, a, b VReg)              { e.op3(IRMulHU, I64, dst, a, b) }
func (e *Emitter) MulHSU(dst, a, b VReg)             { e.op3(IRMulHSU, I64, dst, a, b) }
func (e *Emitter) Neg(dst, a VReg)                   { e.op2(IRNeg, I64, dst, a) }
func (e *Emitter) Shl(dst, a, b VReg)                { e.op3(IRShl, I64, dst, a, b) }
func (e *Emitter) ShlImm(dst, a VReg, shift int64)   { e.op2i(IRShlImm, I64, dst, a, shift) }
func (e *Emitter) Shr(dst, a, b VReg)                { e.op3(IRShr, I64, dst, a, b) }
func (e *Emitter) ShrImm(dst, a VReg, shift int64)   { e.op2i(IRShrImm, I64, dst, a, shift) }
func (e *Emitter) Sar(dst, a, b VReg)                { e.op3(IRSar, I64, dst, a, b) }
func (e *Emitter) SarImm(dst, a VReg, shift int64)   { e.op2i(IRSarImm, I64, dst, a, shift) }

// ── Bitwise ──
func (e *Emitter) And(dst, a, b VReg)                { e.op3(IRAnd, I64, dst, a, b) }
func (e *Emitter) AndImm(dst, a VReg, imm int64)     { e.op2i(IRAndImm, I64, dst, a, imm) }
func (e *Emitter) Or(dst, a, b VReg)                 { e.op3(IROr, I64, dst, a, b) }
func (e *Emitter) OrImm(dst, a VReg, imm int64)      { e.op2i(IROrImm, I64, dst, a, imm) }
func (e *Emitter) Xor(dst, a, b VReg)                { e.op3(IRXor, I64, dst, a, b) }
func (e *Emitter) XorImm(dst, a VReg, imm int64)     { e.op2i(IRXorImm, I64, dst, a, imm) }
func (e *Emitter) Not(dst, a VReg)                   { e.op2(IRNot, I64, dst, a) }

// ── Comparison ──
func (e *Emitter) Set(dst, a, b VReg, p Pred)        { e.opSet(IRSet, dst, a, b, p) }
func (e *Emitter) SetImm(dst, a VReg, imm int64, p Pred) { e.opSetImm(IRSetImm, dst, a, imm, p) }

// ── Data movement ──
func (e *Emitter) Mov(dst, a VReg)                   { e.op2(IRMov, I64, dst, a) }
func (e *Emitter) Const(dst VReg, imm int64)         { e.opConst(dst, imm) }
func (e *Emitter) Sext(dst, a VReg, fromT Type)      { e.opExt(IRSext, dst, a, fromT) }
func (e *Emitter) Zext(dst, a VReg, fromT Type)      { e.opExt(IRZext, dst, a, fromT) }

// ── Memory ──
// Load: dst = *(T*)(base + imm)   — for guest memory, use MaskedLoad high-level helper
func (e *Emitter) Load(dst, base VReg, imm int64, t Type, signed bool) {
    // ... produces IRLoad with T=t; sign/zero handled via IRSext/IRZext if needed
}
// Store: *(T*)(base + imm) = src
func (e *Emitter) Store(base VReg, imm int64, src VReg, t Type) { /* ... */ }
// LoadX: dst = *(T*)(base + idx*scale)   — arch-native indexed addressing
func (e *Emitter) LoadX(dst, base, idx VReg, scale uint8, t Type, signed bool) { /* ... */ }

// ── Control flow ──
func (e *Emitter) Label() Label { /* allocate + emit IRLabel */ return 0 }
func (e *Emitter) Branch(a, b VReg, p Pred, target Label) { /* ... */ }
func (e *Emitter) Jump(target Label)                 { /* ... */ }
func (e *Emitter) Ret(pc uint64, status int, faultAddr VReg) {
    // Emits writeback of dirty vregs + IRRet
}

// ── External call ──
func (e *Emitter) Call(sym string, args ...VReg) VReg {
    // Register sym in Block.CTab if new; emit IRCall.
    // Args are passed per C ABI of the host arch (SysV AMD64 or AAPCS).
    // Returns VReg holding the result (if any).
    return 0
}

// ── FP ──
func (e *Emitter) FAdd(dst, a, b VReg, t Type)       { /* F32 or F64 */ }
func (e *Emitter) FSub(dst, a, b VReg, t Type)       { /* ... */ }
func (e *Emitter) FMul(dst, a, b VReg, t Type)       { /* ... */ }
func (e *Emitter) FDiv(dst, a, b VReg, t Type)       { /* ... */ }
func (e *Emitter) FSqrt(dst, a VReg, t Type)         { /* ... */ }
func (e *Emitter) FCmp(dst, a, b VReg, p Pred, t Type) { /* ... */ }
func (e *Emitter) FNeg(dst, a VReg, t Type)          { /* ... */ }
func (e *Emitter) FAbs(dst, a VReg, t Type)          { /* ... */ }
func (e *Emitter) FCvtToI(dst, a VReg, fromT, toT Type) { /* saturating cast */ }
func (e *Emitter) FCvtFromI(dst, a VReg, fromT, toT Type) { /* ... */ }
func (e *Emitter) FCvtFF(dst, a VReg, fromT, toT Type) { /* F32↔F64 */ }

// ── VReg management ──
func (e *Emitter) Tmp() VReg { /* allocate fresh VReg ≥ VRegTempStart */ return 0 }
func (e *Emitter) XReg(i uint32) VReg { return VReg(i) }      // guest x0..x31 → VReg 0..31
func (e *Emitter) FReg(i uint32) VReg { return VReg(32 + i) } // guest f0..f31 → VReg 32..63
```

## File: `ir/highlevel.go` — High-level helpers

Common patterns built on top of low-level:

```go
package ir

// MaskedLoad performs a bounds-checked guest memory load:
//   addr = base + off
//   if (addr | addr+width) & ~mask: goto faultLabel
//   dst = *(T*)(memBase + (addr & mask))  (with sign/zero extend based on T and signed)
//
// Inputs: dst is the VReg for the result, base is the VReg holding rs1+imm,
// memBase is the VReg holding mem_base argument, mask is the VReg holding mem_mask.
// faultLabel is where to jump on OOB (typically a label that does writeback + return).
//
// Misaligned access is handled by the lowering layer: if the width > 1 and the
// address is misaligned, the lowerer falls back to byte-by-byte. Alternatively
// the emitter could handle this via a high-level helper MaskedLoadU.
func (e *Emitter) MaskedLoad(dst, base, memBase, mask VReg, off int64, width int, signed bool, faultLabel Label) {
    addr := e.Tmp()
    e.AddImm(addr, base, off)
    // OOB check: (addr | (addr + width-1)) & ~mask != 0 → fault
    tmp1 := e.Tmp()
    e.AddImm(tmp1, addr, int64(width-1))
    e.Or(tmp1, addr, tmp1)
    maskNot := e.Tmp()
    e.Not(maskNot, mask)
    e.And(tmp1, tmp1, maskNot)
    e.Branch(tmp1, VRegZero, NE, faultLabel)
    // Masked deref
    masked := e.Tmp()
    e.And(masked, addr, mask)
    host := e.Tmp()
    e.Add(host, memBase, masked)
    t := widthToType(width)
    e.Load(dst, host, 0, t, signed)
}

// GuestStore: bounds-checked guest memory store.
func (e *Emitter) GuestStore(base, memBase, mask VReg, off int64, src VReg, width int, faultLabel Label) {
    // ... analogous to MaskedLoad ...
}

// WriteBackAll: write all dirty cached vregs to x[] array.
// Used before block exits. The list of dirty vregs is tracked by the Emitter.
func (e *Emitter) WriteBackAll() {
    for vr := VReg(1); vr < 32; vr++ {
        if e.dirty[vr] {
            // Emit: x[vr] = r<vr>
            // This is a guest-regfile write. Use e.Store with the x array base as
            // base reg and vr*8 as offset. The x array base is passed as a
            // parameter to the block function — tracked as VReg in Emitter state.
            e.Store(e.xBaseReg(), int64(vr)*8, vr, I64)
        }
    }
    // Also F registers
    for vr := VReg(32); vr < 64; vr++ {
        if e.dirty[vr] {
            e.Store(e.fBaseReg(), int64(vr-32)*8, vr, I64)
        }
    }
}

// FaultExit: emit writeback + IRRet with fault code.
func (e *Emitter) FaultExit(pc uint64, status int, faultAddr VReg) {
    e.WriteBackAll()
    e.Ret(pc, status, faultAddr)
}

// BudgetCheck at a backward branch target:
//   if (ic < maxIC) goto target
//   else writeback + return (pc=target, ic, OK, 0)
func (e *Emitter) BudgetCheck(target Label, targetPC uint64) {
    // Compare ic (special VReg) to maxIC constant
    tooBig := e.Label()
    e.BranchImm(e.icReg(), maxIC, GE, tooBig)
    e.Jump(target)
    e.Emit(tooBig)
    e.WriteBackAll()
    e.Ret(targetPC, 0, VRegZero)
}

// WriteBackReg: write a single vreg to x[] and mark clean.
func (e *Emitter) WriteBackReg(vr VReg) { /* ... */ }

// MarkDirty: record that vr was written.
func (e *Emitter) MarkDirty(vr VReg) { e.dirty[vr] = true }
```

## File: `ir/peephole.go` — Online sliding-window peephole

```go
package ir

// peepholeWindow is the sliding window depth. 4 suffices for our patterns.
const peepholeWindow = 4

// tryPeephole is called after each emit. It looks at the last N instructions
// and rewrites if any pattern matches. Returns true if a rewrite happened;
// the caller may then re-check (patterns can cascade).
func (e *Emitter) tryPeephole() bool {
    n := len(e.Block.Instrs)
    if n == 0 { return false }

    // Pattern: IRMov a, a → delete
    last := &e.Block.Instrs[n-1]
    if last.Op == IRMov && last.Dst == last.A {
        e.Block.Instrs = e.Block.Instrs[:n-1]
        return true
    }

    // Pattern: IRAddImm dst, a, 0 → IRMov dst, a   (unless dst=a, then delete)
    if last.Op == IRAddImm && last.Imm == 0 {
        if last.Dst == last.A {
            e.Block.Instrs = e.Block.Instrs[:n-1]
        } else {
            *last = IRInstr{Op: IRMov, T: I64, Dst: last.Dst, A: last.A}
        }
        return true
    }

    // Pattern: IRMulImm dst, a, 1 → IRMov
    // Pattern: IRShlImm dst, a, 0 → IRMov
    // Pattern: IRAndImm dst, a, -1 → IRMov
    // Pattern: IROrImm dst, a, 0 → IRMov
    // Pattern: IRXorImm dst, a, 0 → IRMov

    // Pattern: IRConst t, 0; IRStore base, off, t, type
    //   → IRStoreImmZero base, off, type  (arch-specific; encodes zero inline)
    // Only fold if t is not used elsewhere. Our single-block IR makes this
    // straightforward: if t is a fresh temp (index >= VRegTempStart) and
    // only used in the immediately-following IRStore, we can fold.
    if n >= 2 {
        prev := &e.Block.Instrs[n-2]
        if prev.Op == IRConst && prev.Imm == 0 && prev.Dst >= VRegTempStart &&
            last.Op == IRStore && last.A == prev.Dst && !e.vregUsedLater(prev.Dst, n-1) {
            // Replace both with store-immediate-zero (a pseudo-op at IR level;
            // lowering handles the arch-specific encoding).
            *prev = IRInstr{Op: IRStore, T: last.T, Dst: last.Dst, A: VRegZero, Imm: last.Imm}
            e.Block.Instrs = e.Block.Instrs[:n-1]
            return true
        }
    }

    // ... 5-10 more patterns as we find them ...

    return false
}

// vregUsedLater: scan from idx onward for any use of vr. For intra-window
// peephole (just 1-2 instructions ahead), this is a trivial check.
func (e *Emitter) vregUsedLater(vr VReg, startIdx int) bool {
    // Since we're peepholing online, startIdx is typically n-1 or n-2.
    // The "future" is empty — peephole sees only the past. So we treat
    // a vreg as "used later" if we can't prove it isn't. For fresh temps
    // (>= VRegTempStart) that appear only in the window we're rewriting,
    // we know they're dead after the window. For guest regs (< VRegTempStart),
    // assume still live.
    if vr < VRegTempStart {
        return true  // could be used in subsequent instrs or at block exit
    }
    // For temps, search forward in window (we only see the past, so "later"
    // is always false for temps at the end of the current window).
    return false
}

// After each append to e.Block.Instrs, call tryPeephole repeatedly until it
// returns false:
func (e *Emitter) emit(ins IRInstr) {
    e.Block.Instrs = append(e.Block.Instrs, ins)
    for e.tryPeephole() {
    }
}
```

## File: `ir/emit_impl.go` — Internal helpers

```go
package ir

func (e *Emitter) op3(op IROp, t Type, dst, a, b VReg) {
    if dst != VRegZero {
        e.emit(IRInstr{Op: op, T: t, Dst: dst, A: a, B: b})
        e.MarkDirty(dst)
    }
    // If dst is VRegZero, still emit for side effects (e.g., x0 writes discarded
    // — here we skip entirely since RISC-V semantics say it's a no-op).
}

func (e *Emitter) op2i(op IROp, t Type, dst, a VReg, imm int64) {
    if dst != VRegZero {
        e.emit(IRInstr{Op: op, T: t, Dst: dst, A: a, Imm: imm})
        e.MarkDirty(dst)
    }
}

func (e *Emitter) op2(op IROp, t Type, dst, a VReg) {
    if dst != VRegZero {
        e.emit(IRInstr{Op: op, T: t, Dst: dst, A: a})
        e.MarkDirty(dst)
    }
}

func (e *Emitter) opConst(dst VReg, imm int64) {
    if dst != VRegZero {
        e.emit(IRInstr{Op: IRConst, T: I64, Dst: dst, Imm: imm})
        e.MarkDirty(dst)
    }
}

// ... etc for every op type ...
```

---

# Phase 3: Register Allocator — Detailed Algorithm

## File: `ir/regalloc.go`

Linear-scan allocator. Single pass over the IR block. Output: each VReg gets either a `HostReg` (host register index) or a `StackSlot` (byte offset into a spill area in the block's stack frame).

## Data structures

```go
package ir

// Allocation: per-VReg assignment.
type Allocation struct {
    Assignments []VRegAlloc // index by VReg
    StackSlots  int         // number of 8-byte slots needed
}

type VRegAlloc struct {
    Kind  AllocKind
    Host  int16 // HostReg if Kind==AllocReg, else unused
    Slot  int16 // StackSlot offset if Kind==AllocStack
}

type AllocKind uint8
const (
    AllocUnused AllocKind = iota  // never seen
    AllocReg
    AllocStack
)

// RegPool: available host regs for integer and FP.
type RegPool struct {
    IntRegs []int16   // host reg IDs usable for integer
    FPRegs  []int16   // host reg IDs usable for FP
}

// AMD64 SysV pool (available in our trampoline after saving callee-saved):
//   Integer: BX, BP (callee-saved); R8-R15 (mostly caller-saved, but we control
//   the block, so anything we don't spill across calls is usable);
//   AX, CX, DX, SI, DI, R8, R9 (arg regs, free since we captured mem_base etc.)
//
// We reserve:
//   RSP — stack pointer (untouchable)
//   RSI — x[] pointer (pinned: we access x[i] via RSI base)
//   RDX — f[] pointer (pinned)
//   RCX — fcsr pointer (pinned)
//   R8  — mem_base (pinned)
//   R9  — mem_mask (pinned)
//   RDI — sret buffer for JITResult (pinned)
// Available for allocation:
//   RAX, RBX, RBP, R10, R11, R12, R13, R14, R15 — 9 integer regs
//   XMM0..XMM15 — 16 FP regs
```

## Algorithm (Poletto & Sarkar linear-scan, adapted)

```go
// Allocate performs linear-scan register allocation on the block.
func Allocate(b *Block, pool RegPool) *Allocation {
    // Step 1: compute live ranges.
    //   For each VReg: Start = first instruction index that defines it,
    //                  End = last instruction index that uses or defines it.
    ranges := computeLiveRanges(b)

    // Step 2: sort VRegs by Start ascending.
    order := sortVRegsByStart(ranges)

    // Step 3: linear scan.
    //   active = set of currently-alive VRegs, ordered by End ascending.
    //   For each VReg v in order:
    //     - Expire: remove from active any VReg whose End < v.Start; free their regs.
    //     - If a free reg is available: assign it to v.
    //     - Else: spill. Choose spill victim = VReg in active with largest End.
    //       If victim.End > v.End, spill victim (assign stack slot), reassign
    //         victim's reg to v, and put victim in spilled list.
    //       Else: spill v directly.

    alloc := &Allocation{
        Assignments: make([]VRegAlloc, countVRegs(b)),
    }
    active := make([]activeEntry, 0, 16)
    freeRegs := append([]int16{}, pool.IntRegs...)  // int pool; FP handled separately

    for _, v := range order {
        expireOld(&active, &freeRegs, ranges[v].Start)
        if isFPVReg(v) {
            // ... similar using pool.FPRegs ...
        } else {
            if len(freeRegs) > 0 {
                reg := freeRegs[0]
                freeRegs = freeRegs[1:]
                alloc.Assignments[v] = VRegAlloc{Kind: AllocReg, Host: reg}
                activeInsert(&active, activeEntry{v, reg, ranges[v].End})
            } else {
                spill(alloc, &active, &freeRegs, v, ranges[v])
            }
        }
    }

    return alloc
}

// computeLiveRanges: walk the block once, recording first def and last use.
func computeLiveRanges(b *Block) []Range {
    ranges := make([]Range, maxVReg(b)+1)
    for i := range ranges { ranges[i] = Range{Start: -1, End: -1} }
    for idx, ins := range b.Instrs {
        // Defs (Dst)
        if ins.Dst != VRegZero {
            if ranges[ins.Dst].Start < 0 {
                ranges[ins.Dst].Start = idx
            }
            ranges[ins.Dst].End = idx
        }
        // Uses (A, B, and maybe Idx for LoadX)
        for _, u := range []VReg{ins.A, ins.B} {
            if u != VRegZero && ranges[u].Start >= 0 {
                ranges[u].End = idx
            }
        }
    }
    // Special: vregs for guest regs that are live at block exit need End = last instr.
    // The emitter's WriteBackAll() ensures dirty guest vregs are written before exit.
    // Non-dirty guest vregs don't need to be live at exit.
    return ranges
}

// expireOld: remove active entries whose End < currentStart.
func expireOld(active *[]activeEntry, freeRegs *[]int16, currentStart int) {
    kept := (*active)[:0]
    for _, e := range *active {
        if e.End < currentStart {
            *freeRegs = append(*freeRegs, e.Reg)
        } else {
            kept = append(kept, e)
        }
    }
    *active = kept
}

// spill: handle register pressure overflow.
func spill(alloc *Allocation, active *[]activeEntry, freeRegs *[]int16, v VReg, r Range) {
    // Find active entry with largest End.
    maxIdx := 0
    for i, e := range *active {
        if e.End > (*active)[maxIdx].End {
            maxIdx = i
        }
    }
    victim := (*active)[maxIdx]
    if victim.End > r.End {
        // Spill victim, give its reg to v.
        alloc.Assignments[victim.VReg] = VRegAlloc{Kind: AllocStack, Slot: int16(alloc.StackSlots)}
        alloc.StackSlots++
        alloc.Assignments[v] = VRegAlloc{Kind: AllocReg, Host: victim.Reg}
        (*active)[maxIdx] = activeEntry{v, victim.Reg, r.End}
    } else {
        // Spill v directly.
        alloc.Assignments[v] = VRegAlloc{Kind: AllocStack, Slot: int16(alloc.StackSlots)}
        alloc.StackSlots++
    }
}

type Range struct { Start, End int }
type activeEntry struct { VReg VReg; Reg int16; End int }
```

## Spill handling in lowering

When lowering sees `alloc.Assignments[v].Kind == AllocStack`, the lowerer inserts:
- Load from stack before use: `MOVQ slot(SP), scratchReg` → use scratchReg
- Store to stack after def: `MOVQ scratchReg, slot(SP)`

The lowerer reserves one or two "scratch" host regs that are never allocated (e.g., R10, R11 on amd64) specifically for reload/spill. This is simpler than keeping scratch regs in the pool and detecting when they're free.

## Pinned registers

Guest `x` and `f` arrays are accessed via pinned host regs (RSI, RDX on amd64; X1, X2 on arm64). The allocator treats these regs as unavailable. The emitter's helpers for reading/writing guest regs use the pinned base reg plus a compile-time offset.

**For cached guest regs**: the emitter decides up-front that guest regs r1..r31 map to VRegs 1..31. The allocator decides per-block which of those VRegs get host regs vs. stay in memory (read from x[] when needed, written back before exit).

## Tests

`ir/regalloc_test.go`: unit tests with hand-built IR programs:
- No pressure → all VRegs get regs
- Overflow → some spill
- Live ranges with gaps → correct expiration
- Interleaved FP and integer → separate pools correct

---

# Phase 4: amd64 Lowering — Full Op-by-Op Detail

## File: `ir/lower_amd64.go`

```go
package ir

import (
    "riscv/internal/goasm"
    "riscv/internal/goasm/obj"
    "riscv/internal/goasm/obj/x86"
)

// LowerAMD64 converts a Block + Allocation into obj.Progs appended to ctx.
func LowerAMD64(ctx *goasm.Ctx, b *Block, alloc *Allocation) error {
    lc := &lowerCtx{
        ctx:     ctx,
        alloc:   alloc,
        scratch: []int16{goasm.REG_AMD64_R10, goasm.REG_AMD64_R11},  // scratch pool
        xBase:   goasm.REG_AMD64_RSI,  // pinned
        fBase:   goasm.REG_AMD64_RDX,  // pinned
        fcsr:    goasm.REG_AMD64_RCX,  // pinned
        memBase: goasm.REG_AMD64_R8,   // pinned
        memMask: goasm.REG_AMD64_R9,   // pinned
        sretBuf: goasm.REG_AMD64_RDI,  // pinned (JITResult)
        labels:  make(map[Label]*obj.Prog),
        pending: make(map[Label][]*obj.Prog),
    }
    // Prologue: save callee-saved, allocate spill frame.
    lc.emitPrologue(alloc.StackSlots)

    // Instruction-by-instruction lowering.
    for idx, ins := range b.Instrs {
        lc.idx = idx
        if err := lc.lowerInstr(&ins); err != nil {
            return err
        }
    }

    return nil
}

func (lc *lowerCtx) lowerInstr(ins *IRInstr) error {
    switch ins.Op {
    case IRLabel:
        lc.emitLabel(Label(ins.Imm))
    case IRAdd:
        lc.emitAdd(ins)
    case IRAddImm:
        lc.emitAddImm(ins)
    // ... switch over all IR ops ...
    default:
        return fmt.Errorf("lower amd64: unhandled op %d", ins.Op)
    }
    return nil
}
```

## Detailed per-op lowerings for amd64

The following gives the **exact obj.Prog sequence** for each IR op. In each, `dst`, `a`, `b` are the allocated host regs for the IR operands. When a VReg is on stack, a `load1` helper emits a reload into scratch[0] and returns that reg; similarly `store1` spills.

### IRAdd: `dst = a + b`

```go
// If dst == a: one ADDQ
// Else if dst == b: one ADDQ (commutative)
// Else: MOVQ a,dst ; ADDQ b,dst
func (lc *lowerCtx) emitAdd(ins *IRInstr) {
    dst := lc.use(ins.Dst, /*def*/ true)
    a   := lc.use(ins.A,   /*def*/ false)
    b   := lc.use(ins.B,   /*def*/ false)
    if dst == a {
        lc.emit2(x86.AADDQ, b, dst)
    } else if dst == b {
        lc.emit2(x86.AADDQ, a, dst)
    } else {
        lc.emit2(x86.AMOVQ, a, dst)
        lc.emit2(x86.AADDQ, b, dst)
    }
    lc.defCommit(ins.Dst, dst)
}

// emit2 creates a two-operand Prog: opcode src→dst.
func (lc *lowerCtx) emit2(op obj.As, src, dst int16) {
    p := lc.ctx.NewProg()
    p.As = op
    p.From.Type = obj.TYPE_REG
    p.From.Reg = src
    p.To.Type = obj.TYPE_REG
    p.To.Reg = dst
    lc.ctx.Append(p)
}
```

### IRAddImm: `dst = a + imm`

```go
// If imm fits in int32: ADDQ $imm, dst (after MOVQ a, dst if needed)
// If imm doesn't fit: MOVQ $imm, scratch; ADDQ scratch, dst
// Special: imm == 0 → IRMov (handled by peephole, shouldn't reach here)
// Special: imm == 1 → INCQ (shorter encoding); imm == -1 → DECQ
func (lc *lowerCtx) emitAddImm(ins *IRInstr) {
    dst := lc.use(ins.Dst, true)
    a   := lc.use(ins.A, false)
    if dst != a {
        lc.emit2(x86.AMOVQ, a, dst)
    }
    switch {
    case ins.Imm == 1:
        p := lc.ctx.NewProg(); p.As = x86.AINCQ
        p.To.Type = obj.TYPE_REG; p.To.Reg = dst
        lc.ctx.Append(p)
    case ins.Imm == -1:
        p := lc.ctx.NewProg(); p.As = x86.ADECQ
        p.To.Type = obj.TYPE_REG; p.To.Reg = dst
        lc.ctx.Append(p)
    case ins.Imm >= -(1<<31) && ins.Imm < (1<<31):
        p := lc.ctx.NewProg(); p.As = x86.AADDQ
        p.From.Type = obj.TYPE_CONST; p.From.Offset = ins.Imm
        p.To.Type = obj.TYPE_REG; p.To.Reg = dst
        lc.ctx.Append(p)
    default:
        scr := lc.scratch[0]
        p := lc.ctx.NewProg(); p.As = x86.AMOVQ
        p.From.Type = obj.TYPE_CONST; p.From.Offset = ins.Imm
        p.To.Type = obj.TYPE_REG; p.To.Reg = scr
        lc.ctx.Append(p)
        lc.emit2(x86.AADDQ, scr, dst)
    }
    lc.defCommit(ins.Dst, dst)
}
```

### IRSub, IRSubImm

Analogous to IRAdd/IRAddImm, using `SUBQ`. Not commutative — must MOV a→dst first if dst != a.

### IRMul: `dst = a * b`

amd64 `IMULQ` has several forms:
- Two-operand: `IMULQ r, r` → dst *= src (dst is both operand and destination)
- Three-operand with immediate: `IMULQ $imm, src, dst`

For IR `dst = a * b`:
```go
if dst == a:
    IMULQ b, dst
else if dst == b:
    IMULQ a, dst  // commutative
else:
    MOVQ a, dst; IMULQ b, dst
```

### IRDivS: `dst = (int64)a / (int64)b`

amd64 `IDIVQ` uses RDX:RAX as implicit dividend, divides by the operand, returns quotient in RAX, remainder in RDX.

```go
// Sequence:
//   MOVQ a, RAX
//   CQO                 // sign-extend RAX to RDX:RAX
//   IDIVQ b             // RAX = quotient, RDX = remainder
//   MOVQ RAX, dst
// Issue: RAX and RDX are pinned (or used for other purposes). We must
// save/restore them around the IDIV if they hold live values.
//
// Simplification: reserve RAX and RDX as "scratch for division" — allocator
// knows not to place a live vreg there for the duration of the block if any
// DIV/MUL is present.
//
// OR: spill/reload around each division.
```

Decision: **for blocks containing DIV/MUL, the allocator excludes RAX and RDX from the pool.** Small capacity cost (2 fewer regs) for simpler lowering. If we hit a block with extreme register pressure and divisions, we can improve later.

### IRDivU

Same as IRDivS but use `DIVQ` (unsigned) and `XORQ RDX, RDX` instead of `CQO`.

### IRRem

Same as IRDivS but take the result from RDX instead of RAX.

### IRMulHS: signed high-64 of 128-bit product

amd64 `IMULQ` (one-operand form) gives RDX:RAX = RAX * operand (sign-extended).

```go
// MOVQ a, RAX
// IMULQ b        (one-operand form)
// MOVQ RDX, dst
```

### IRMulHU, IRMulHSU

IRMulHU: use `MULQ` (unsigned one-operand). IRMulHSU is trickier — there's no direct instruction. The standard trick:

```c
// If b is non-negative, MULHSU = MULHS (signed high mul of a by b)
// If a is negative, MULHSU = MULHS + b (add b to the high word when a is neg)
// Actual impl:
uint64_t mulhsu(int64_t a, uint64_t b) {
    int64_t h = (int64_t)(((__int128)a * (__int128)b) >> 64);
    return (uint64_t)h;
}
```

Since we don't have `__int128` in C (TCC limitation avoided), but on amd64 we can do:

```asm
# MULHSU: dst = high_signed × unsigned
MOVQ a, RAX
MULQ b          # RDX:RAX = unsigned(a) * b
# Now adjust: if a < 0, subtract b from RDX (because unsigned mul treats a as
# 2^64 + a; we need to adjust by -b when a was negative)
TESTQ RAX, RAX  # No, need to test original a
# Simpler: just do signed-unsigned properly:
MOVQ a, RDX
SARQ $63, RDX   # RDX = sign bit of a replicated
ANDQ b, RDX     # if a<0: RDX=b, else RDX=0
MOVQ a, RAX
MULQ b          # RDX:RAX = unsigned(a) * b ; wait, this overwrites RDX
```

Given the complexity, simplest approach: **bail to interpreter for MULHSU** (we already do). Revisit if benchmarks demand it.

Similarly for complex FP conversions that don't map to a single SSE instruction.

### IRShl: `dst = a << (b & 63)`

amd64 shift-by-reg uses CL register:
```go
// MOVQ a, dst    (if dst != a)
// MOVB b, CL     (low 8 bits of b)
// SHLQ CL, dst
```

Requires CL (low byte of RCX) — but RCX is our pinned `fcsr` pointer. We need to save/restore it:
```go
// Use scratch: MOVQ RCX, scratch ; MOVQ b, RCX ; SHLQ CL, dst ; MOVQ scratch, RCX
```

**Decision**: reserve a scratch gpr (R10) across the block. For shifts, swap RCX → R10 temporarily. Or: reassign `fcsr` pointer from RCX to a different pinned reg (R11 is free-ish).

Let me revise: let's pin `fcsr` to R11 instead of RCX, freeing RCX for shifts. Updated pinning:
- RSI = x[]
- RDX = f[]
- R11 = fcsr     (moved off RCX)
- R8 = mem_base
- R9 = mem_mask
- RDI = sret
- RCX = free (used by shifts)
- RAX, RDX = reserved for DIV/MUL blocks (or free pool otherwise)
- RBX, RBP, R10, R12-R15 = allocation pool

This means updating `call_amd64.s` trampoline to load fcsr into R11 after setup. Easy change.

Actually even simpler: use `BMI2` `SHLX` which takes the shift count in any GPR, not CL. Modern amd64 chips (2013+) support BMI2. We'd detect at init and pick BMI2 path if available.

**Decision**: use SHLX/SHRX/SARX from BMI2 when available (most 2015+ hardware). Fall back to CL-based shifts otherwise (rare).

### IRShlImm: `dst = a << imm`

```go
// MOVQ a, dst  (if dst != a)
// SHLQ $imm, dst
```
Uses immediate encoding, no CL issue.

### IRAnd, IROr, IRXor: bitwise

Straightforward:
```go
// MOVQ a, dst  (if dst != a)
// ANDQ b, dst  (or ORQ, XORQ)
```

### IRAndImm with imm = 0xFFFFFFFF: zero-extend low 32 bits

Fold to `MOVL dst, dst` (amd64 MOVL auto-zeros upper 32). Arch-specific peephole at lowering.

### IRNot: `dst = ~a`

```go
// MOVQ a, dst  (if dst != a)
// NOTQ dst
```

### IRNeg: `dst = -a`

```go
// MOVQ a, dst  (if dst != a)
// NEGQ dst
```

### IRSet: `dst = (a pred b) ? 1 : 0`

```go
// CMPQ b, a       (AT&T-style: src2 first in obj)
// SETcc dst       (SETE, SETNE, SETL, SETLE, SETG, SETGE, SETB, SETBE, SETA, SETAE)
// MOVZBQ dst, dst (zero-extend the 0/1 byte to full 64-bit)
```

### IRMov: `dst = a`

Just `MOVQ a, dst` if they differ. (Peephole removes when they're the same.)

### IRConst: `dst = imm`

```go
// If imm == 0: XORQ dst, dst   (shorter encoding, auto-zeros)
// Else if imm fits in int32: MOVQ $imm, dst  (sign-extended)
// Else if imm fits in uint32: MOVL $imm, dst  (zero-extended)
// Else: MOVQ $imm, dst  (full 64-bit immediate)
```

### IRSext (T=I32 to I64): `dst = (int64)(int32)a`

```go
// MOVSXD a, dst   (AMD64 sign-extend 32 to 64)
```

For T=I16: `MOVSWQ`. For T=I8: `MOVSBQ`.

### IRZext

For T=I32: `MOVL a, dst` (auto-zeros upper 32). For smaller: `MOVZWQ` / `MOVZBQ`.

### IRLoad: `dst = *(T*)(base + imm)`

```go
// Build memory operand base + imm:
p := lc.ctx.NewProg()
p.As = loadOp(ins.T, ins.Signed)  // MOVQ / MOVL / MOVSXD / MOVZBQ / etc.
p.From.Type = obj.TYPE_MEM
p.From.Reg = base
p.From.Offset = ins.Imm
p.To.Type = obj.TYPE_REG
p.To.Reg = dst
lc.ctx.Append(p)
```

`loadOp(T, signed)` table:

| T | signed | opcode |
|---|--------|--------|
| I8 | true | MOVSBQ |
| I8 | false | MOVZBQ |
| I16 | true | MOVSWQ |
| I16 | false | MOVZWQ |
| I32 | true | MOVSXD (MOVSLQ) |
| I32 | false | MOVL |
| I64 | either | MOVQ |
| F32 | — | MOVSS |
| F64 | — | MOVSD |

### IRStore: `*(T*)(base + imm) = src`

```go
p := lc.ctx.NewProg()
p.As = storeOp(ins.T)  // MOVB / MOVW / MOVL / MOVQ / MOVSS / MOVSD
p.From.Type = obj.TYPE_REG
p.From.Reg = src
p.To.Type = obj.TYPE_MEM
p.To.Reg = base
p.To.Offset = ins.Imm
lc.ctx.Append(p)
```

### IRLoadX: `dst = *(T*)(base + idx*scale)`

amd64 SIB addressing:
```go
p.From.Type = obj.TYPE_MEM
p.From.Reg = base
p.From.Index = idx
p.From.Scale = int16(ins.Scale)  // 1, 2, 4, or 8
p.From.Offset = 0
```

### IRLabel: `marks a branch target`

Emit a `obj.ANOP` Prog (no-op placeholder). Record the mapping `Label → *obj.Prog`. Resolve pending forward branches.

```go
func (lc *lowerCtx) emitLabel(l Label) {
    p := lc.ctx.NewProg()
    p.As = obj.ANOP  // no encoding bytes emitted
    lc.ctx.Append(p)
    lc.labels[l] = p
    // Resolve pending forward branches targeting this label
    for _, branch := range lc.pending[l] {
        branch.To.Val = p  // obj uses .Val as *Prog for TYPE_BRANCH
    }
    delete(lc.pending, l)
}
```

### IRBranch: `if (a pred b) goto label`

```go
func (lc *lowerCtx) emitBranch(ins *IRInstr) {
    a := lc.use(ins.A, false)
    b := lc.use(ins.B, false)
    // CMP b, a  (AT&T/obj convention)
    cmp := lc.ctx.NewProg()
    cmp.As = x86.ACMPQ
    cmp.From.Type = obj.TYPE_REG; cmp.From.Reg = b
    cmp.To.Type = obj.TYPE_REG; cmp.To.Reg = a
    lc.ctx.Append(cmp)
    // Jcc target
    jmp := lc.ctx.NewProg()
    jmp.As = branchOp(ins.Pred)  // AJEQ / AJNE / AJLT / AJLE / AJGT / AJGE / AJCS / etc.
    jmp.To.Type = obj.TYPE_BRANCH
    lc.ctx.Append(jmp)
    lc.bindLabel(Label(ins.Imm), jmp)
}

// bindLabel: if label already defined, set Val; else queue as pending.
func (lc *lowerCtx) bindLabel(l Label, p *obj.Prog) {
    if target, ok := lc.labels[l]; ok {
        p.To.Val = target
    } else {
        lc.pending[l] = append(lc.pending[l], p)
    }
}
```

Predicate → opcode table:
- EQ → AJEQ
- NE → AJNE
- LT → AJLT (signed)
- LE → AJLE
- GT → AJGT
- GE → AJGE
- LTU → AJCS (below)
- LEU → AJLS (below-equal)
- GTU → AJHI (above)
- GEU → AJCC (above-equal)

### IRJump

```go
jmp := lc.ctx.NewProg()
jmp.As = obj.AJMP
jmp.To.Type = obj.TYPE_BRANCH
lc.ctx.Append(jmp)
lc.bindLabel(Label(ins.Imm), jmp)
```

### IRCall: call external C ABI function

Our emitted blocks call `jit_sqrtf`, `jit_sqrt`, and potentially `jit_trace`. These are plain C functions (SysV AMD64 ABI). Our block's register state needs to be saved before the call.

```go
// Save caller-saved regs currently holding live vregs.
//   AMD64 SysV caller-saved: RAX, RCX, RDX, RSI, RDI, R8, R9, R10, R11 + XMM0-15
//   Callee-saved (preserved): RBX, RBP, R12, R13, R14, R15
// If live vregs are in callee-saved regs, they're fine.
// If in caller-saved regs, we must spill to stack before call, reload after.
//
// Set up args per SysV: first 6 ints in RDI,RSI,RDX,RCX,R8,R9; first 8 FP in
// XMM0-XMM7. But RSI, RDX, RCX, R8, R9, RDI are OUR pinned regs!
//
// We need to temporarily reassign them. Standard approach: save them, restore
// after the call.
```

This is complex enough it deserves its own helper:

```go
// emitCCall handles an external call:
//   1. Save live caller-saved regs to stack
//   2. Save pinned regs (RSI, RDX, RCX, R8, R9, RDI) if they'll be clobbered by args
//   3. Load args into RDI, RSI, RDX, RCX, R8, R9 (ints) / XMM0-XMM7 (fp)
//   4. Load symbol addr, call via register
//   5. Restore pinned regs
//   6. Restore live caller-saved regs
//   7. Move return value (RAX / XMM0) into result VReg's host reg
func (lc *lowerCtx) emitCCall(ins *IRInstr) {
    // For ic-aware call sequence, see ABI handling section below.
    // Since our JIT blocks make very few C calls (mostly sqrtf/sqrt at FP ops),
    // we can afford a conservative save-everything approach.
    // TODO: optimize later if profiling shows it matters.
}
```

For phase 4a implementation, start simple: save all caller-saved, restore all. Profile later.

### IRRet: `return (JITResult){pc=Imm, ic, status=Src1, faultAddr=Src2}`

```go
func (lc *lowerCtx) emitRet(ins *IRInstr) {
    // Fields at offsets 0/8/16/24 of sret buffer (RDI):
    //   pc (uint64) at 0
    //   ic (uint64) at 8
    //   status (uint64) at 16
    //   fault_addr (uint64) at 24

    // Emit: MOVQ $pc, (RDI)
    lc.emitStoreMem(x86.AMOVQ, lc.sretBuf, 0, lc.immReg(uint64(ins.Imm)))
    // ic is in a dedicated VReg (tracked via Emitter's icReg); store it at +8
    icHost := lc.resolveVReg(lc.icVReg())
    lc.emitStoreMem(x86.AMOVQ, lc.sretBuf, 8, icHost)
    // status at +16
    if ins.A != VRegZero {
        lc.emitStoreMem(x86.AMOVQ, lc.sretBuf, 16, lc.use(ins.A, false))
    } else {
        lc.emitStoreMemImm(x86.AMOVQ, lc.sretBuf, 16, 0)
    }
    // fault_addr at +24
    if ins.B != VRegZero {
        lc.emitStoreMem(x86.AMOVQ, lc.sretBuf, 24, lc.use(ins.B, false))
    } else {
        lc.emitStoreMemImm(x86.AMOVQ, lc.sretBuf, 24, 0)
    }
    // Epilogue: restore callee-saved, dealloc frame, RET
    lc.emitEpilogue()
}
```

### IRFAdd/FSub/FMul/FDiv (F64)

```go
// MOVSD a, dst    (if dst != a)
// ADDSD b, dst    (or SUBSD, MULSD, DIVSD)
```

### IRFSqrt (F64)

```go
// SQRTSD a, dst
```

Takes source directly, no MOV-first needed if dst != a.

### IRFCmp

amd64 `UCOMISD` (unordered compare) sets EFLAGS. Then SETcc + MOVZBQ for 0/1.

```go
// UCOMISD a, b
// SETE dst  (for EQ)
// Actual predicate table (FP uses unordered-aware conditions):
//   EQ: SETE + SETNP   (must be Equal AND Not Parity — rule out NaN)
//   Actually for IEEE equal: use SETNE, SETP to OR them, then NOT
// Simpler: Use COMISD which sets flags; for each predicate emit SETcc:
//   EQ: SETNP + SETE via AND (or use fcomieq-style: MOV 0, CMOVE 1)
// For full IEEE correctness, use the sequence from x86 manuals.
```

Detailed FP comparison lowering is arch-specific. Reference: Go's own runtime handles this in `runtime/alg.go` and the cmp implementations.

### IRFCvtToI, IRFCvtFromI, IRFCvtFF

amd64:
- F32 → I32: CVTTSS2SIL
- F64 → I32: CVTTSD2SIL
- F32 → I64: CVTTSS2SIQ
- F64 → I64: CVTTSD2SIQ
- I32 → F32: CVTSI2SSL
- I32 → F64: CVTSI2SDL
- F32 → F64: CVTSS2SD
- F64 → F32: CVTSD2SS

Unsigned conversions are trickier (x86 has signed-only cvt). Use signed cvt + fixup for 64-bit unsigned. Reference libriscv's cpu.cpp for the exact sequences.

### IRCall

See above — the non-trivial case. Allocate ~100 LoC for the full save-call-restore sequence.

## Prologue / epilogue

```go
func (lc *lowerCtx) emitPrologue(stackSlots int) {
    // Our trampoline (call_amd64.s) gives us a ~64K frame and has saved
    // callee-saved regs to known offsets. Our block's prologue just needs to
    // allocate our own stack slots (for spills).
    //
    // If stackSlots > 0:
    //   SUBQ $(stackSlots*8), SP
    // End of block: ADDQ $(stackSlots*8), SP + RET
    //
    // Since the trampoline also does SUBQ $frame and a RET, we share its frame.
    // Our "epilogue" just RETs (trampoline restores callee-saved).
    if stackSlots > 0 {
        p := lc.ctx.NewProg()
        p.As = x86.ASUBQ
        p.From.Type = obj.TYPE_CONST; p.From.Offset = int64(stackSlots) * 8
        p.To.Type = obj.TYPE_REG; p.To.Reg = goasm.REG_AMD64_RSP
        lc.ctx.Append(p)
    }
    lc.stackSlots = stackSlots
}

func (lc *lowerCtx) emitEpilogue() {
    if lc.stackSlots > 0 {
        p := lc.ctx.NewProg()
        p.As = x86.AADDQ
        p.From.Type = obj.TYPE_CONST; p.From.Offset = int64(lc.stackSlots) * 8
        p.To.Type = obj.TYPE_REG; p.To.Reg = goasm.REG_AMD64_RSP
        lc.ctx.Append(p)
    }
    p := lc.ctx.NewProg()
    p.As = obj.ARET
    lc.ctx.Append(p)
}
```

## Full lowering test

`ir/lower_amd64_test.go`: unit tests that lower hand-built IR programs and verify byte-exact output against `go tool asm` output for equivalent assembly source.

---

# Phase 5: Port jit_emit.go to Produce IR

Current structure (`jit_emit.go`) has:
- `emitBlock(mem, pc)`: top-level
- `emit32(insn)`, `emitRVC(insn)`: dispatch
- Per-op helpers: `emitOp`, `emitOpImm`, `emitLoad`, `emitStore`, `emitBranch`, `emitJAL`, `emitJALR`, `emitFMA`, `emitFPOp`, etc.
- Uses `e.emit(format, ...)` to write C strings.

## Port strategy: mechanical replacement

Replace every `e.emit("C text")` with equivalent IR helper calls. Keep the decode-and-dispatch structure.

## Per-file table: current C → IR translation

For each op family in the current emitter, the IR equivalent:

### emitOp (opcode 0x33)

Current (for funct7=0x00, funct3=0: ADD):
```go
e.emit("    %s = %s + %s;\n", d, a, b)
```

Port:
```go
e.Add(vregDst, vregA, vregB)
```

Where `vregDst = e.XReg(rd)`, etc. The emitter wraps RISC-V register indices in VRegs.

### emitOpImm (opcode 0x13)

Current for funct3=0, imm=0 (MV): `e.emit("%s = %s;\n", d, a)`.
Port: `e.Mov(vregDst, vregA)`.

Current for ADDI: `e.emit("%s = %s + %dLL;\n", d, a, imm)`.
Port: `e.AddImm(vregDst, vregA, imm)`.

### emitLoad

Current (integer load with bounds check + masked deref):
```go
e.emit("    { uint64_t addr = %s + %dLL;\n", ...)
e.emit("      if ((addr|(addr+%d)) & ~mem_mask) { writeback; return fault }\n", ...)
e.emit("      if (addr & %d) byte_by_byte; else regular;\n", ...)
```

Port:
```go
// High-level helper encapsulates bounds check + mask + optional misalign fallback:
e.MaskedLoad(vregDst, vregRs1, off, widthToType(width), signed, faultLabel)
```

`faultLabel` is pre-allocated at the start of emitBlock for each fault type:
```go
loadFaultLabel := e.Label()
storeFaultLabel := e.Label()
// ... at end of block, emit bodies:
e.Emit(loadFaultLabel)
e.FaultExit(currentPC, jitLoadFault, faultAddrVReg)
```

### emitStore

Similar to emitLoad, port `e.emit(...)` to `e.GuestStore(...)`.

### emitBranch

Current emits: `if (cond) goto b_ADDR` for internal branches, or bounds check + return for external.

Port:
```go
// Internal branch:
internal := ...  // same logic as today
if internal {
    // Allocate/look up label for target PC
    targetLabel := e.getOrCreateLabel(targetPC)
    if target < e.pc {
        // Backward: wrap in budget check
        e.BudgetCheck(targetLabel, targetPC)
    } else {
        e.Branch(vregRs1, vregRs2, cmpPred, targetLabel)
    }
} else {
    // External: emit branch to a synthetic exit label whose body does return
    exitLabel := e.Label()
    e.Branch(vregRs1, vregRs2, cmpPred, exitLabel)
    // Later emit exit body
    e.Emit(exitLabel)
    e.FaultExit(targetPC, jitOK, VRegZero)   // not a fault; just exit block
}
```

### emitJAL

Current for rd==0: `e.emit("goto b_%x;\n", target)`.

Port:
```go
if rd == 0 {
    targetLabel := e.getOrCreateLabel(target)
    origPC := e.pc - insnSize
    if target < origPC {
        e.BudgetCheck(targetLabel, target)
    } else {
        e.Jump(targetLabel)
    }
    e.pc = target  // emitter's cursor moves to follow
    return
}
// rd != 0: link + exit
e.ConstImm(e.XReg(rd), int64(e.pc+insnSize))  // link
e.FaultExit(target, jitOK, VRegZero)
```

### emitJALR

Current: compute target = (rs1 + imm) & ~1; writeback; return.

Port:
```go
target := e.Tmp()
e.AddImm(target, e.XReg(rs1), imm)
e.AndImm(target, target, ^int64(1))
if rd != 0 {
    e.ConstImm(e.XReg(rd), int64(e.pc+insnSize))
}
e.WriteBackAll()
e.RetDyn(target, jitOK, VRegZero)  // new helper: return with dynamic PC from a VReg
```

### FP emission

Each FP op maps to an IR FP op. Boxing/unboxing for F32 becomes IR helpers:
```go
// boxF32(reg) → set upper 32 bits to 0xFFFFFFFF
func (e *Emitter) BoxF32(fvreg VReg, f32 VReg) {
    // f[fvreg] = (uint64)(f32 & 0xFFFFFFFF) | 0xFFFFFFFF00000000
    e.ZextT(f32, f32, I32)  // zero-extend
    hi := e.Tmp()
    e.Const(hi, int64(0xFFFFFFFF00000000))
    e.Or(fvreg, f32, hi)
}
```

## Emitter state augmentations

The current `emitter` struct has fields like `regsUsed`, `gotoTargets`, `visited`, etc. The new IR-emitting Emitter has:

```go
type emitter struct {
    // ... existing decode state ...
    block     *ir.Block
    irEm      *ir.Emitter
    pcLabels  map[uint64]ir.Label  // guest PC → IR label ID
    dirty     [64]bool              // which vregs have been written
    // ... etc ...
}

func (e *emitter) getOrCreateLabel(pc uint64) ir.Label {
    if l, ok := e.pcLabels[pc]; ok { return l }
    l := e.irEm.NewLabel()
    e.pcLabels[pc] = l
    return l
}
```

## Backward-branch budget

Replace the current C-emitting budget check:
```go
e.emit("if (__builtin_expect(ic < %d, 1)) goto b_%x; writeback; return...\n", ...)
```

With:
```go
e.irEm.BudgetCheck(targetLabel, targetPC)
```

## Testing strategy

The existing lockstep test suite is the acceptance test. Port one op at a time:
1. Implement IR + lowering for ADD
2. Run `TestJIT_ADD` — should pass
3. Run `TestRISCVTests_Lockstep_UI/add` — should pass
4. Move on to next instruction family

Each family gets its own commit. A bug is localized to the specific IR op or lowering that introduced it.

---

# Phase 6: arm64 Lowering

Same structure as phase 4, different instruction mnemonics. Key ARM64 differences:

- **Registers**: X0-X30 (64-bit), W0-W30 (32-bit), V0-V31 (FP/SIMD)
- **Arithmetic**: `ADD X1, X2, X3` (three-operand) — no separate MOV needed
- **Shifts**: `LSL X1, X2, #5` or `LSL X1, X2, X3` — count in reg (no CL constraint)
- **Loads/stores**: `LDR X1, [X2, #8]`, `LDR X1, [X2, X3, LSL #3]` (scaled)
- **Branches**: `B.EQ target`, `CBZ X1, target` (compare-branch-zero), `CBNZ`
- **Multiplication**: `MUL X1, X2, X3` (low 64), `UMULH X1, X2, X3`, `SMULH X1, X2, X3` — cleaner than amd64
- **Division**: `SDIV`, `UDIV` — don't use special registers, just three-operand

### Pinned regs (arm64 AAPCS)

- X0 = sret pointer (hidden first arg for struct return ≥ 16 bytes)
- X1 = x[]
- X2 = f[]
- X3 = fcsr
- X4 = mem_base
- X5 = mem_mask
- X6..X15 = free (caller-saved, we use for scratch/allocation pool)
- X19..X28 = callee-saved (we save in trampoline, use for allocation pool)
- X29 = FP (frame pointer), X30 = LR (link register)
- SP = stack

### Detailed per-op lowering (abbreviated here; follow amd64 pattern file)

- IRAdd: `ADD dst, a, b` (1 insn, no MOV)
- IRAddImm (imm fits in 12 bits, optionally shifted by 12): `ADD dst, a, #imm`
- IRAddImm larger: `MOVZ scratch, #imm16; MOVK scratch, #imm16, LSL #16; ADD dst, a, scratch`
- IRMul: `MUL dst, a, b`
- IRMulHS: `SMULH dst, a, b` (one instruction!)
- IRMulHU: `UMULH dst, a, b`
- IRMulHSU: no direct instr, compute via adjustment like amd64 — or just bail to interpreter as we do now
- IRDivS: `SDIV dst, a, b` (arm64 returns 0 for div-by-zero, unlike amd64 which traps — matches RISC-V behavior!)
- IRDivU: `UDIV dst, a, b`
- IRRem: `SDIV tmp, a, b; MSUB dst, tmp, b, a` (remainder = a - quotient × b)
- IRShl: `LSL dst, a, b`
- IRShlImm: `LSL dst, a, #imm`
- IRBranch: `CMP a, b; B.<cond> target`
- IRBranch with imm=0: `CBZ a, target` or `CBNZ` — nice short encoding
- IRLoad: `LDR dst, [base, #imm]`
- IRStore: `STR src, [base, #imm]`
- IRLoadX: `LDR dst, [base, idx, LSL #scale]`

Full table in `ir/lower_arm64.go`, ~3K LoC mirroring the amd64 one.

### ARM64 trampoline: `internal/jitcall/call_arm64.s`

```asm
#include "textflag.h"

// func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
//           memBase uintptr, memMask uint64) Result
//
// AArch64 AAPCS: first arg = hidden sret pointer when result > 16 bytes.
// Our Result is 32 bytes, so sret is passed in X8 (AAPCS sret reg) on arm64.
//
// But Go uses its own calling convention... for asm funcs with
// ABIInternal (Go 1.17+), args come in registers R0-R7 (same as AAPCS).
// For ABI0 (old-style), args come via FP on stack.
//
// Following the amd64 trampoline's pattern: use ABI0 (Plan 9 asm default).
// Go ABI0 for func Call(fn, x, f, fcsr, memBase, memMask uintptr) Result:
//   args via FP:     fn=0(FP), x=8(FP), f=16(FP), fcsr=24(FP),
//                    memBase=32(FP), memMask=40(FP)
//   return at:       ret+48(FP)
//
// AArch64 C ABI for the CALLED function (the JIT block):
//   X8 (indirect result location register) = sret buffer pointer
//   X0 = x, X1 = f, X2 = fcsr, X3 = memBase, X4 = memMask
//
// Frame size: NOSPLIT limit on arm64 is also ~792. We use $65536 (no NOSPLIT)
// to give TCC-like generous stack headroom.
TEXT ·Call(SB), $65536-80

    // Save callee-saved GPRs that JIT block may clobber.
    // AArch64 callee-saved: X19-X28, X29 (FP), X30 (LR), SP, V8-V15 (low 64).
    STP   R19, R20, 16(RSP)
    STP   R21, R22, 32(RSP)
    STP   R23, R24, 48(RSP)
    STP   R25, R26, 64(RSP)
    STP   R27, R28, 80(RSP)
    STR   R29,      96(RSP)   // FP
    STR   R30,     104(RSP)   // LR

    // Load args into AAPCS regs.
    LEAQ   0(RSP), R8          // X8 = hidden sret ptr (local buffer at [0, 32))
    MOVD   x+8(FP),  R0
    MOVD   f+16(FP), R1
    MOVD   fcsr+24(FP), R2
    MOVD   memBase+32(FP), R3
    MOVD   memMask+40(FP), R4

    // Call the JIT'd native function.
    MOVD   fn+0(FP), R9
    BLR    R9

    // Copy Result (32 bytes at [0,32)) into Go return area.
    LDP    0(RSP),  R5, R6
    LDP    16(RSP), R7, R10
    MOVD   R5,  ret_PC+48(FP)
    MOVD   R6,  ret_IC+56(FP)
    MOVD   R7,  ret_Status+64(FP)
    MOVD   R10, ret_FaultAddr+72(FP)

    // Restore callee-saved.
    LDP   16(RSP), R19, R20
    LDP   32(RSP), R21, R22
    LDP   48(RSP), R23, R24
    LDP   64(RSP), R25, R26
    LDP   80(RSP), R27, R28
    LDR   96(RSP), R29
    LDR  104(RSP), R30

    RET
```

Note: ARM64 Plan 9 assembly uses its own mnemonics (MOVD instead of MOVQ, etc.). Double-check exact syntax against existing Go arm64 asm files in the runtime.

---

# Phase 7: Deprecate TCC

## Build tag split

In `jit_tcc.go` add at the top:
```go
//go:build tcc
// +build tcc
```

In a new `jit_native.go` (default):
```go
//go:build !tcc
// +build !tcc

package riscv

import "riscv/ir"

func compile(block *ir.Block, arch ir.Arch) (*compiledBlock, error) {
    alloc := ir.Allocate(block, ir.PoolFor(arch))
    ctx := goasm.New(arch)
    if err := ir.Lower(ctx, arch, block, alloc); err != nil {
        return nil, err
    }
    bytes, err := ctx.Assemble()
    if err != nil {
        return nil, err
    }
    // mmap + copy into executable memory
    mem, err := allocExec(len(bytes))
    if err != nil {
        return nil, err
    }
    copy(mem, bytes)
    return &compiledBlock{fn: uintptr(unsafe.Pointer(&mem[0])), backing: mem}, nil
}
```

Update `jit.go`'s `RunJIT` to call `compile(...)` instead of `tccCompile(...)`.

## Remove vendor/tcc, Makefile targets

After validation passes:
- Delete `vendor/tcc/`
- Remove cgo imports from jit_tcc.go (keep file with build tag for fallback)
- Update Makefile: remove `libriscv-build` dependency from default path (keep it under `tcc` tag)

---

# Testing Plan (cross-phase)

## Phase 1 tests: goasm extraction
- `go test ./internal/goasm/...`: every sample assembly sequence produces bytes identical to `go tool asm` output

## Phase 2 tests: IR emitter
- `go test ./ir/...`: verify IR struct is constructed correctly from helper calls
- Peephole unit tests: apply known-optimizable sequences, assert rewrites

## Phase 3 tests: register allocator
- Synthetic IR programs with known live ranges
- Verify allocation decisions: no conflicts, valid spill choices

## Phase 4 tests: amd64 lowering
- Per-op unit tests: hand-build IR for each op, lower, compare bytes vs. expected
- Integration: port `TestJIT_ADD`, `TestJIT_LoadStore`, etc. — same tests but backed by native emit

## Phase 5 tests: full JIT on goasm backend
- `go test -run 'TestJIT_' ...`: all 23 existing unit tests pass
- `go test -run 'TestRISCVTests_Lockstep_U[IMAC]' ...`: all ~90 lockstep tests pass (with per-block register + 32KB memory comparison)
- Any divergence points to specific IR op or lowering bug via the exact PC reported

## Phase 6 tests: arm64
- Cross-compile + run under qemu-system-aarch64 OR on actual Apple Silicon
- Same test suite as phase 5, with GOARCH=arm64

## Cold path benchmark
- New test: `BenchmarkBlockCompile` — times the path from RISC-V bytes to executable code
- Compare goasm path to TCC path (with `tcc` tag)
- Expected: 10-100× speedup on goasm path

---

# Timeline Estimate (detailed per phase)

| Phase | Subtasks | Effort |
|-------|----------|--------|
| 1 | Extraction script (2d), stubs (3d), drive() research (3d), api_test.go (5d) | **3-4 weeks** |
| 2 | ir.go (2d), emit.go (5d), highlevel.go (5d), peephole.go (3d) | **2 weeks** |
| 3 | Live range analysis (3d), allocator (5d), spill handling (2d), tests (3d) | **2-3 weeks** |
| 4a | amd64 int ops (7d), loads/stores (4d), branches (3d) | **2-3 weeks** |
| 4b | amd64 mul/div (3d), shifts (2d), FP ops (7d), IRCall ABI (4d) | **2-3 weeks** |
| 5 | Port emitOp family (4d), loads/stores (3d), branches (3d), FP (5d), JAL/JALR (2d) | **3-4 weeks** |
| 6 | arm64 lowering (3-4w), arm64 trampoline (1w), validation (1w) | **5-6 weeks** |
| 7 | Build tag split (2d), remove TCC (1d), docs (2d) | **1 week** |
| **Total** | | **4-6 months** |

---

# Risk Mitigation (detailed)

| Risk | Mitigation |
|------|------------|
| obj extraction has more deps than expected | Stub package + commented-out edits tracked in EDITS.md. If truly entangled, fall back to rebuilding libtcc.a for ARM64 as interim. |
| Obj API awkward for "emit bytes without writing object file" | Study Go's linker for examples; worst case, we emit Progs into a fake section and manually read back its bytes. |
| Register allocator produces worse code than TCC | Benchmark via BenchmarkCPU_FullExecution_JIT; if worse, adopt libriscv's allocator (we have its source). |
| IRCall ABI handling is wrong | Small isolated component; test in isolation against C standard-library symbols (dlsym sqrtf, call from JIT'd code, verify result). |
| arm64 trampoline has ABI bugs | Validate against Go's own assembler conventions; reference `$GOROOT/src/runtime/asm_arm64.s` for ABI examples. |
| Forward branch fixup has bugs | Online fixup model makes bugs local: if a forward branch isn't resolved, assembly fails with a clear error; easy to find. |

---

# File Manifest

```
scripts/
  extract-goasm.sh         # NEW: extraction script (Phase 1.1)

internal/goasm/             # NEW, all extracted (Phase 1)
  LICENSE                   # Go's BSD-3-Clause
  EXTRACTED.txt             # version stamp
  EDITS.md                  # manual edits log
  DRIVE.md                  # obj driver research notes
  stub/stub.go              # empty stubs for removed deps
  api.go                    # Ctx, New, NewProg, Append, Assemble
  regs.go                   # register constants
  api_test.go               # byte-exact tests vs `go tool asm`
  obj/...                   # extracted from $GOROOT
  obj/x86/...
  obj/arm64/...
  objabi/...
  src/...
  sys/...
  hash/...

ir/                         # NEW (Phases 2, 3, 4, 6)
  ir.go                     # types: IRInstr, VReg, Type, Pred, Label, Block
  emit.go                   # low-level helpers (Phase 2)
  emit_impl.go              # internal helpers
  highlevel.go              # MaskedLoad, FaultExit, etc. (Phase 2)
  peephole.go               # online window optimizer (Phase 2)
  regalloc.go               # linear-scan allocator (Phase 3)
  regalloc_test.go
  lower_amd64.go            # amd64 lowering (Phase 4)
  lower_amd64_test.go
  lower_arm64.go            # arm64 lowering (Phase 6)
  lower_arm64_test.go
  fixup.go                  # label/branch online fixup
  c_call.go                 # IRCall ABI handler
  abi.go                    # per-arch pinned regs, pool definitions

jit_emit.go                 # MODIFY: emit IR instead of C text (Phase 5)
jit_tcc.go                  # MODIFY: //go:build tcc (Phase 7)
jit_native.go               # NEW: default compile path using ir + goasm (Phase 7)
jit.go                      # MODIFY: call compile() instead of tccCompile()

internal/jitcall/
  call_amd64.s              # UNCHANGED
  call_arm64.s              # NEW (Phase 6)

Makefile                    # MODIFY: default to goasm; tcc target under tag
vendor/tcc/                 # DELETE after Phase 7 validation
```

---

# Acceptance Criteria

1. ✅ `go test -count=1 -run 'TestJIT_' -timeout 60s .` passes (all 23 unit tests)
2. ✅ `go test -count=1 -run 'TestRISCVTests_Lockstep_U[IMAC]' -timeout 120s .` passes (all ~90 lockstep)
3. ✅ `go test -count=1 -run 'TestRISCVTests_U[IMAC]_JIT' -timeout 60s .` passes
4. ✅ `go test -count=1 -tags tcc -run 'TestJIT_' -timeout 60s .` passes (TCC fallback regression)
5. ✅ `go test -count=1 -v -run TestJIT_BenchGuest_Smoke -timeout 30s ./bench/` passes (2.5B insn smoke)
6. ✅ `GOARCH=arm64 go test -count=1 -run 'TestJIT_' .` passes (cross-arch build)
7. ✅ No cgo in default build (verify via `go list -f '{{.CgoFiles}}' ./...` — empty for non-tcc build)
8. ✅ `make bench-cpu` shows throughput ≥ 1800 MIPS (≥ 90% of TCC baseline)
9. ✅ Cold-path compile benchmark shows ≥ 10× speedup vs TCC path
10. ✅ Binary size drops by ≥ 500KB (removal of libtcc.a and cgo overhead)
