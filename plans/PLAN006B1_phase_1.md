# Go-Native JIT Backend: Tiny IR + Extracted obj Encoders

This is just phase 1 of PLAN006B.

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
│  goasm/  — Extracted from $GOROOT, BSD-3-Clause             │
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
A self-contained, buildable Go package at `goasm/` that exposes Go's amd64 and arm64 instruction encoders as a library. Pure BSD-3-Clause (inherited from Go).

## Step 1.1: Write `scripts/extract-goasm.sh`

Bash script that copies selected files from `$GOROOT/src/cmd/internal/` to `goasm/`, rewrites imports, and skips files we don't need.

**Full script** (add to repo as `scripts/extract-goasm.sh`):

```bash
#!/usr/bin/env bash
set -euo pipefail

GOROOT=${GOROOT:-$(go env GOROOT)}
DEST=goasm
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

# Rewrite imports: cmd/internal/... → riscv/goasm/...
find "$DEST" -name '*.go' | while read f; do
    sed -i.bak -E '
        s|"cmd/internal/obj"|"riscv/goasm/obj"|g
        s|"cmd/internal/obj/x86"|"riscv/goasm/obj/x86"|g
        s|"cmd/internal/obj/arm64"|"riscv/goasm/obj/arm64"|g
        s|"cmd/internal/objabi"|"riscv/goasm/objabi"|g
        s|"cmd/internal/src"|"riscv/goasm/src"|g
        s|"cmd/internal/sys"|"riscv/goasm/sys"|g
        s|"cmd/internal/hash"|"riscv/goasm/hash"|g
        s|"cmd/internal/dwarf"|"riscv/goasm/stub"|g
        s|"cmd/internal/goobj"|"riscv/goasm/stub"|g
    ' "$f"
    rm "$f.bak"
done

# Copy LICENSE
cp "$GOROOT/LICENSE" "$DEST/LICENSE"
echo "Extracted to $DEST. Go version: $(go version)" > "$DEST/EXTRACTED.txt"
```

## Step 1.2: Write stub package for removed deps

Create `goasm/stub/stub.go`:

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

- **DWARF emission calls** (in `link.go`, `plist.go`): comment out.
- **Object file writing** (in `ld.go`, `sym.go`): comment out
- **PCLN table generation** (in `pass.go`): comment out

Expected: ~20-40 lines of surgery in `obj/*.go` to neuter unused features. Keep commented-out lines (don't delete) so future re-extractions preserve the edits as context.

**File**: `goasm/EDITS.md` — running log of every manual edit. Future re-extractions reference this to reapply them.

## Step 1.4: API wrapper `goasm/api.go`

Expose a minimal API on top of the extracted obj package:

```go
package goasm

import (
    "fmt"
    "riscv/goasm/obj"
    "riscv/goasm/obj/x86"
    "riscv/goasm/obj/arm64"
    "riscv/goasm/objabi"
    "riscv/goasm/sys"
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

**Detailed study task for phase 1**: read `$GOROOT/src/cmd/internal/obj/plist.go` (Flushplist) and `$GOROOT/src/cmd/internal/obj/x86/asm6.go` (span6/assemble) to understand the exact sequence of calls to go from `[]*Prog` to encoded bytes. Document findings in `goasm/DRIVE.md`.

## Step 1.5: Register constants

Create `goasm/regs.go` exposing register constants as Go names:

```go
package goasm

import (
    "riscv/goasm/obj/x86"
    "riscv/goasm/obj/arm64"
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

Create `goasm/api_test.go` with golden tests:

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
