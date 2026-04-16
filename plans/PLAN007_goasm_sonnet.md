# Plan: goasm/ Extraction — Phase 1 of Plan 006B

## Context

This is Phase 1 of PLAN006B: replace the TCC-based JIT (LGPL-licensed, cgo overhead, string parsing) with a pure-Go JIT pipeline. Phase 1 extracts Go's internal `cmd/internal/obj` instruction encoders into a standalone `goasm/` package under the `riscv` module, giving us BSD-3-Clause, no-cgo machine-code emission for amd64 and arm64.

Repo: `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang`
Module: `riscv` (go.mod at repo root)
Go version: 1.22.2, GOROOT: `/usr/local/go`

---

## Sub-step 1.1 — Write `scripts/extract-goasm.sh`

**File**: `scripts/extract-goasm.sh`

Complete bash script. Execute once; re-run to refresh after Go upgrades (re-applying EDITS.md manually afterward).

```bash
#!/usr/bin/env bash
set -euo pipefail

GOROOT=${GOROOT:-$(go env GOROOT)}
DEST=goasm
SRC=$GOROOT/src/cmd/internal

declare -A KEEP=(
    [obj]="abi_string.go addrtype_string.go data.go go.go inl.go ld.go line.go link.go pass.go plist.go stringer.go sym.go textflag.go util.go"
    [obj/x86]="*.go"
    [obj/arm64]="*.go"
    [objabi]="*.go"
    [src]="*.go"
    [sys]="*.go"
    [hash]="*.go"
)

declare -A SKIP=(
    [obj]="dwarf.go fips140.go mkcnames.go objfile.go pcln.go line_test.go objfile_test.go sizeof_test.go"
    [obj/x86]="asm_test.go obj6_test.go pcrelative_test.go"
    [obj/arm64]="asm_test.go"
    [objabi]=""
    [src]=""
    [sys]=""
    [hash]=""
)

rm -rf "$DEST/obj" "$DEST/objabi" "$DEST/src" "$DEST/sys" "$DEST/hash"
mkdir -p "$DEST"

for pkg in "${!KEEP[@]}"; do
    mkdir -p "$DEST/$pkg"
    for pattern in ${KEEP[$pkg]}; do
        for f in $SRC/$pkg/$pattern; do
            [ -f "$f" ] || continue
            base=$(basename "$f")
            skip=0
            for s in ${SKIP[$pkg]:-}; do
                [ "$base" = "$s" ] && skip=1 && break
            done
            [ $skip -eq 1 ] && continue
            cp "$f" "$DEST/$pkg/$base"
        done
    done
done

# Rewrite imports in-place
find "$DEST" -name '*.go' | while read f; do
    sed -i.bak -E '
        s|"cmd/internal/obj/x86"|"riscv/goasm/obj/x86"|g
        s|"cmd/internal/obj/arm64"|"riscv/goasm/obj/arm64"|g
        s|"cmd/internal/obj"|"riscv/goasm/obj"|g
        s|"cmd/internal/objabi"|"riscv/goasm/objabi"|g
        s|"cmd/internal/src"|"riscv/goasm/src"|g
        s|"cmd/internal/sys"|"riscv/goasm/sys"|g
        s|"cmd/internal/hash"|"riscv/goasm/hash"|g
        s|"cmd/internal/dwarf"|"riscv/goasm/stub"|g
        s|"cmd/internal/goobj"|"riscv/goasm/stub"|g
        s|"internal/fips140"|"riscv/goasm/stub"|g
    ' "$f"
    rm "$f.bak"
done

cp "$GOROOT/LICENSE" "$DEST/LICENSE"
echo "Extracted goasm from Go $(go version) on $(date)" > "$DEST/EXTRACTED.txt"
echo "Done. Run: go build ./goasm/..."
```

**Execute**:
```bash
chmod +x scripts/extract-goasm.sh
bash scripts/extract-goasm.sh
```

---

## Sub-step 1.2 — Initial build attempt + stub discovery

Run `go build ./goasm/...` and collect all compilation errors. These errors enumerate the missing types. Expected missing symbols from skipped packages:

### Known missing categories (from Go 1.22 source analysis):

**From `cmd/internal/dwarf`** (referenced by `obj/link.go`, `obj/plist.go`, `obj/sym.go`):
- `dwarf.Context` — interface with methods for emitting DWARF data
- `dwarf.Sym` — symbol info for DWARF generation
- `dwarf.Var` — variable info
- `dwarf.Scope` — lexical scope
- `dwarf.InlCall` — inlined call info
- `dwarf.Range` — PC range
- `dwarf.FnState` — per-function DWARF state
- `dwarf.AbstractFunc` — abstract function representation
- `dwarf.PutFunc`, `dwarf.PutInlinedFunc` — DWARF write functions
- `dwarf.AppendUleb128`, `dwarf.AppendSleb128` — LEB encoding

**From `cmd/internal/goobj`** (referenced by `obj/link.go` for LSym.Extra):
- `goobj.SymRef` — symbol reference type
- Possibly `goobj.Reloc`, `goobj.Sym`

**From `internal/fips140`** (referenced by `obj/link.go` or `objabi`):
- `fips140.Supported() bool` or similar

---

## Sub-step 1.3 — Create `goasm/stub/stub.go`

Create a stub package that makes the extracted code compile without pulling in heavy deps. **Iterative process**: run build, add stub type, repeat until clean.

**File**: `goasm/stub/stub.go`

```go
// Package stub provides empty implementations of cmd/internal/dwarf,
// cmd/internal/goobj, and internal/fips140 so that the extracted
// goasm/obj package compiles without DWARF debug-info generation,
// object-file writing, or FIPS enforcement—none of which we need for
// JIT code emission.
package stub

// ===== cmd/internal/dwarf stubs =====
// We don't generate DWARF; these types exist only to satisfy the
// type-checker on the ~10 call sites in obj/link.go and obj/plist.go.

type Context interface{}

type Sym struct {
    Name   string
    Offset int32
}

type Var struct {
    Name   string
    Abbrev int
}

type Scope struct {
    Vars   []*Var
    Scopes []Scope
    Ranges []Range
}

type Range struct{ Low, High uint64 }

type InlCall struct {
    AbsFuncSym *Sym
    CallFile   string
    CallLine   uint32
    CallCol    uint32
    Children   []InlCall
    Ranges     []Range
}

type FnState struct {
    Closurevars []Var
}

type AbstractFunc struct{}

func PutFunc(ctxt Context, s *Sym, isStmt bool, name string, absfn *AbstractFunc,
    startPC uint64, endPC uint64, scopes []Scope, inlCalls []InlCall) error {
    return nil
}
func PutInlinedFunc(ctxt Context, calls []InlCall, s *Sym) error { return nil }
func PutConcreteFunc(ctxt Context, s *Sym, isStmt bool) error    { return nil }
func AppendUleb128(b []byte, v uint64) []byte                     { return b }
func AppendSleb128(b []byte, v int64) []byte                      { return b }

// ===== cmd/internal/goobj stubs =====
// LSym.Extra may reference goobj.SymRef; stubs make it compile.

type SymRef struct{ PkgIdx, SymIdx uint32 }

type Reloc struct {
    Off  int32
    Siz  uint8
    Type uint16
    Add  int64
    Sym  SymRef
}

// ===== internal/fips140 stubs =====
// obj may call fips140.Supported() to gate certain transformations.

func Supported() bool { return false }
func InReader() bool  { return false }
```

**Adding stubs iteratively**: After each `go build ./goasm/...`, copy the error type name and add it here. Log each addition in `goasm/EDITS.md` (Sub-step 1.7).

---

## Sub-step 1.4 — Code surgery in extracted files

After stubs are in place, some files will still fail because they call methods/functions on stubbed types that don't match signatures, or reference skipped global variables.

### 1.4a — `goasm/obj/plist.go`

**Add** exported `AssembleBlock` function (after all imports).

`mkfwd` is defined in `obj/ld.go`. The pipeline order matches what `Flushplist` does:
```go
// AssembleBlock is a stripped-down Flushplist that encodes a single text
// symbol to native bytes (sym.P) without generating DWARF, PCLN, or SEH.
//
// The Prog chain must begin with an ATEXT Prog (see goasm.Ctx.NewATEXT).
// Preprocess scans for ATEXT and sets sym.Func().Text; mkfwd and
// linkpatch then walk sym.Func().Text to resolve branch targets.
//
// Call order (matches Flushplist, confirmed from source study):
//   Preprocess → mkfwd → linkpatch → Assemble
// NOT the order shown in the overview plan — Preprocess comes FIRST
// because it sets sym.Func().Text which mkfwd/linkpatch depend on.
func AssembleBlock(ctxt *Link, sym *LSym, newprog ProgAlloc) {
    // Preprocess: sets sym.Func().Text (from ATEXT), expands RET, rewrites TLS.
    ctxt.linkArch.Preprocess(ctxt, sym, newprog)
    // mkfwd: in ld.go; sets Prog.Forwd for forward-branch resolution.
    mkfwd(sym)
    // linkpatch: in pass.go; resolves branch To.Offset → *Prog targets.
    linkpatch(ctxt, sym, newprog)
    // Assemble: encode → sym.P (span6 for amd64).
    ctxt.linkArch.Assemble(ctxt, sym, newprog)
    // skip linkpcln (pcln.go excluded), DWARF, SEH
}
```

**Important**: The original `Flushplist` calls Preprocess first (confirmed by reading plist.go carefully). The architecture-specific Preprocess must run before mkfwd/linkpatch because it establishes `sym.Func().Text` via the ATEXT scan. The initial plan had the order wrong — this is corrected here.

**Comment out** all DWARF-generating calls in `Flushplist`. Search for:
- `ctxt.generateDebugLinesSymbol(...)` — comment out
- Any `dwarf.Put*` or `dwarf.Append*` calls — comment out
- `ctxt.dwarfSym(...)` calls — comment out
- `linkpcln(ctxt, s, newprog)` call — comment out (pcln.go is excluded)

Keep the surrounding loop structure intact; only the DWARF/PCLN lines get `// GOASM_REMOVED:` prefix comments.

### 1.4b — `goasm/obj/link.go`

Search for direct calls to DWARF functions or references to skipped packages:
- `generateDebugLinesSymbol` definition — comment out body (return nil)
- Any `fips140.*` calls — comment out, return defaults

### 1.4c — `goasm/obj/sym.go`

- `DwarfInterfaceMethodCount` function — if it calls actual dwarf logic, replace body with `return 0`

### 1.4d — `goasm/obj/x86/seh.go`

This file handles Windows SEH unwinding. It's OS-gated but may have compilation issues on macOS. If errors appear: add `//go:build windows` guard or comment out the file body, logging in EDITS.md.

### 1.4e — `goasm/objabi/flag.go`

This file registers command-line flags. In our library context, flag registration on import is undesirable but won't break builds. Leave as-is unless it imports unavailable packages.

### 1.4f — `goasm/objabi/zbootstrap.go`

This 1-line generated file sets `const defaultGOROOT`. Keep as-is (value doesn't matter for encoding).

---

## Sub-step 1.5 — Write `goasm/api.go`

The API wrapper. Exposes a minimal, idiomatic Go surface for building Prog lists and encoding them.

**Key facts confirmed from GOROOT study**:
- `LookupInit` exists in `obj/sym.go` — safe to use
- `sym.Func().Text` is set by arch **Preprocess** when it sees an ATEXT Prog — so the **first** Prog in the chain MUST be an ATEXT pseudo-instruction
- `mkfwd` is in `obj/ld.go` (not pass.go)
- `span6(ctxt, sym, newprog)` is the AMD64 encoder called by `ctxt.linkArch.Assemble`

**File**: `goasm/api.go`

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

// Ctx holds per-JIT-block assembler state.
// Create with New; reuse across blocks by calling Reset.
type Ctx struct {
    arch   Arch
    ctxt   *obj.Link
    sym    *obj.LSym
    last   *obj.Prog
    errors []string
}

// New creates a fresh Ctx for the given architecture.
// The first Prog you append should be an ATEXT pseudo-instruction
// (use NewATEXT() to build it); arch Preprocess sets sym.Func().Text
// when it encounters ATEXT.
func New(arch Arch) *Ctx {
    c := &Ctx{arch: arch}
    c.ctxt = newLinkCtx(arch)
    c.ctxt.DiagFunc = func(msg string, args ...any) {
        c.errors = append(c.errors, fmt.Sprintf(msg, args...))
    }
    c.sym = c.ctxt.LookupInit("jit_block", func(s *obj.LSym) {
        s.Type = objabi.STEXT
    })
    return c
}

// Reset clears state for reuse.
func (c *Ctx) Reset() {
    c.ctxt = newLinkCtx(c.arch)
    c.ctxt.DiagFunc = func(msg string, args ...any) {
        c.errors = append(c.errors, fmt.Sprintf(msg, args...))
    }
    c.errors = nil
    c.last = nil
    c.sym = c.ctxt.LookupInit("jit_block", func(s *obj.LSym) {
        s.Type = objabi.STEXT
    })
}

// NewProg allocates an uninitialized Prog linked to this context.
func (c *Ctx) NewProg() *obj.Prog {
    p := c.ctxt.NewProg()
    p.Ctxt = c.ctxt
    return p
}

// NewATEXT builds the required ATEXT pseudo-instruction that must be
// the first Prog in every function. Arch Preprocess sets
// sym.Func().Text when it encounters this instruction.
func (c *Ctx) NewATEXT() *obj.Prog {
    p := c.NewProg()
    p.As = obj.ATEXT
    p.From.Type = obj.TYPE_MEM
    p.From.Sym = c.sym
    p.From.Name = obj.NAME_EXTERN
    p.To.Type = obj.TYPE_TEXTSIZE
    p.To.Offset = 0   // frame size 0; trampoline owns the frame
    p.To.Val = int32(0) // arg size
    return p
}

// firstProg tracks the ATEXT Prog; set on first Append.
// (Add firstProg *obj.Prog to the Ctx struct.)

// Append adds p to the Prog chain. Call with NewATEXT() first,
// then instruction Progs, then a RET.
func (c *Ctx) Append(p *obj.Prog) {
    if c.last == nil {
        c.firstProg = p   // must be the ATEXT prog
    } else {
        c.last.Link = p
    }
    c.last = p
}

// Assemble encodes the Prog chain to native machine-code bytes.
// Returns error if any diagnostic was emitted.
func (c *Ctx) Assemble() ([]byte, error) {
    if c.last == nil {
        return nil, fmt.Errorf("goasm: empty prog list")
    }
    c.last.Link = nil // terminate chain

    obj.AssembleBlock(c.ctxt, c.sym, c.ctxt.NewProg)

    if len(c.errors) > 0 {
        return nil, fmt.Errorf("goasm: assembly errors: %v", c.errors)
    }
    out := make([]byte, len(c.sym.P))
    copy(out, c.sym.P)
    return out, nil
}

// Sym returns the text LSym (needed for ATEXT setup in tests/helpers).
func (c *Ctx) Sym() *obj.LSym { return c.sym }

// Ctxt returns the raw Link context for advanced use.
func (c *Ctx) Ctxt() *obj.Link { return c.ctxt }

func newLinkCtx(arch Arch) *obj.Link {
    var la *obj.LinkArch
    switch arch {
    case AMD64:
        la = &x86.Linkamd64
    case ARM64:
        la = &arm64.Linkarm64
    default:
        panic(fmt.Sprintf("goasm: unsupported arch %d", arch))
    }
    ctxt := obj.Linknew(la)
    // Headtype must be set; use the host OS default.
    // obj6.Preprocess checks this for some platform-specific behaviour.
    ctxt.Headtype = objabi.Hdarwin  // macOS default; adjust for Linux builds
    ctxt.Flag_optimize = false
    return ctxt
}

// HostArch returns AMD64 or ARM64 matching GOARCH.
func HostArch() Arch {
    switch sys.GOARCH {
    case "amd64":
        return AMD64
    case "arm64":
        return ARM64
    default:
        panic("goasm: unsupported host GOARCH: " + sys.GOARCH)
    }
}
```

**Note on Headtype**: `obj6.Preprocess` inspects `ctxt.Headtype` for TLS and stack-frame decisions. For JIT blocks we don't use TLS. Use `objabi.Hdarwin` on macOS, `objabi.Hlinux` on Linux. Can make it conditional on `runtime.GOOS` in the final version.

**Note on ATEXT and FuncInfo initialization**:
`LSym.Func()` returns nil if `Extra` is not initialized to a `*FuncInfo`. The normal path is `ctxt.InitTextSym(sym, flag, atextProg)` which Flushplist calls after finding each ATEXT. For AssembleBlock, we need to call `InitTextSym` (exported from obj) before calling AssembleBlock:

```go
func (c *Ctx) Assemble() ([]byte, error) {
    if c.last == nil {
        return nil, fmt.Errorf("goasm: empty prog list")
    }
    c.last.Link = nil

    // InitTextSym sets up FuncInfo and Text; required before AssembleBlock.
    // Signature: InitTextSym(ctxt *Link, s *LSym, flag int, text *Prog)
    // flag 0 = no special attrs; text = the ATEXT Prog (first in chain).
    obj.InitTextSym(c.ctxt, c.sym, 0, c.firstProg)

    obj.AssembleBlock(c.ctxt, c.sym, c.ctxt.NewProg)
    ...
}
```

This requires `Ctx` to store `firstProg` (set in `Append` on first call).

**Research task**: Verify `InitTextSym` signature in Go 1.22 by reading `$GOROOT/src/cmd/internal/obj/plist.go` during implementation. If the signature differs, adjust. Also check whether `AssembleBlock` needs the ATEXT Prog to be the `sym.Func().Text` before or if `InitTextSym` handles that.

---

## Sub-step 1.6 — Write `goasm/regs.go`

**File**: `goasm/regs.go`

```go
package goasm

import (
    "riscv/goasm/obj/x86"
    "riscv/goasm/obj/arm64"
)

// --- AMD64 general-purpose registers (64-bit) ---
const (
    REG_AMD64_AX  = x86.REG_AX
    REG_AMD64_CX  = x86.REG_CX
    REG_AMD64_DX  = x86.REG_DX
    REG_AMD64_BX  = x86.REG_BX
    REG_AMD64_SP  = x86.REG_SP
    REG_AMD64_BP  = x86.REG_BP
    REG_AMD64_SI  = x86.REG_SI
    REG_AMD64_DI  = x86.REG_DI
    REG_AMD64_R8  = x86.REG_R8
    REG_AMD64_R9  = x86.REG_R9
    REG_AMD64_R10 = x86.REG_R10
    REG_AMD64_R11 = x86.REG_R11
    REG_AMD64_R12 = x86.REG_R12
    REG_AMD64_R13 = x86.REG_R13
    REG_AMD64_R14 = x86.REG_R14
    REG_AMD64_R15 = x86.REG_R15
)

// --- AMD64 SSE/XMM registers ---
const (
    REG_AMD64_X0  = x86.REG_X0
    REG_AMD64_X1  = x86.REG_X1
    REG_AMD64_X2  = x86.REG_X2
    REG_AMD64_X3  = x86.REG_X3
    REG_AMD64_X4  = x86.REG_X4
    REG_AMD64_X5  = x86.REG_X5
    REG_AMD64_X6  = x86.REG_X6
    REG_AMD64_X7  = x86.REG_X7
    REG_AMD64_X8  = x86.REG_X8
    REG_AMD64_X9  = x86.REG_X9
    REG_AMD64_X10 = x86.REG_X10
    REG_AMD64_X11 = x86.REG_X11
    REG_AMD64_X12 = x86.REG_X12
    REG_AMD64_X13 = x86.REG_X13
    REG_AMD64_X14 = x86.REG_X14
    REG_AMD64_X15 = x86.REG_X15
)

// --- ARM64 integer registers ---
const (
    REG_ARM64_R0  = arm64.REG_R0
    REG_ARM64_R1  = arm64.REG_R1
    REG_ARM64_R2  = arm64.REG_R2
    REG_ARM64_R3  = arm64.REG_R3
    REG_ARM64_R4  = arm64.REG_R4
    REG_ARM64_R5  = arm64.REG_R5
    REG_ARM64_R6  = arm64.REG_R6
    REG_ARM64_R7  = arm64.REG_R7
    REG_ARM64_R8  = arm64.REG_R8
    REG_ARM64_R9  = arm64.REG_R9
    REG_ARM64_R10 = arm64.REG_R10
    REG_ARM64_R11 = arm64.REG_R11
    REG_ARM64_R12 = arm64.REG_R12
    REG_ARM64_R13 = arm64.REG_R13
    REG_ARM64_R14 = arm64.REG_R14
    REG_ARM64_R15 = arm64.REG_R15
    REG_ARM64_R16 = arm64.REG_R16
    REG_ARM64_R17 = arm64.REG_R17
    REG_ARM64_R18 = arm64.REG_R18
    REG_ARM64_R19 = arm64.REG_R19
    REG_ARM64_R20 = arm64.REG_R20
    REG_ARM64_R21 = arm64.REG_R21
    REG_ARM64_R22 = arm64.REG_R22
    REG_ARM64_R23 = arm64.REG_R23
    REG_ARM64_R24 = arm64.REG_R24
    REG_ARM64_R25 = arm64.REG_R25
    REG_ARM64_R26 = arm64.REG_R26
    REG_ARM64_R27 = arm64.REG_R27
    REG_ARM64_R28 = arm64.REG_R28
    REG_ARM64_R29 = arm64.REG_R29 // FP
    REG_ARM64_R30 = arm64.REG_R30 // LR
    REG_ARM64_ZR  = arm64.REGZERO // XZR/WZR
)

// --- ARM64 FP/SIMD registers ---
const (
    REG_ARM64_F0  = arm64.REG_F0
    REG_ARM64_F1  = arm64.REG_F1
    REG_ARM64_F2  = arm64.REG_F2
    REG_ARM64_F3  = arm64.REG_F3
    REG_ARM64_F4  = arm64.REG_F4
    REG_ARM64_F5  = arm64.REG_F5
    REG_ARM64_F6  = arm64.REG_F6
    REG_ARM64_F7  = arm64.REG_F7
    REG_ARM64_F8  = arm64.REG_F8
    REG_ARM64_F9  = arm64.REG_F9
    REG_ARM64_F10 = arm64.REG_F10
    REG_ARM64_F11 = arm64.REG_F11
    REG_ARM64_F12 = arm64.REG_F12
    REG_ARM64_F13 = arm64.REG_F13
    REG_ARM64_F14 = arm64.REG_F14
    REG_ARM64_F15 = arm64.REG_F15
    REG_ARM64_F16 = arm64.REG_F16
    REG_ARM64_F17 = arm64.REG_F17
    REG_ARM64_F18 = arm64.REG_F18
    REG_ARM64_F19 = arm64.REG_F19
    REG_ARM64_F20 = arm64.REG_F20
    REG_ARM64_F21 = arm64.REG_F21
    REG_ARM64_F22 = arm64.REG_F22
    REG_ARM64_F23 = arm64.REG_F23
    REG_ARM64_F24 = arm64.REG_F24
    REG_ARM64_F25 = arm64.REG_F25
    REG_ARM64_F26 = arm64.REG_F26
    REG_ARM64_F27 = arm64.REG_F27
    REG_ARM64_F28 = arm64.REG_F28
    REG_ARM64_F29 = arm64.REG_F29
    REG_ARM64_F30 = arm64.REG_F30
    REG_ARM64_F31 = arm64.REG_F31
)
```

**Note**: exact constant names (REG_AX vs REG_RAX, REG_R0 vs REG_X0) must be verified from the extracted `a.out.go` files after extraction. Adjust as needed.

---

## Sub-step 1.7 — Write `goasm/EDITS.md`

**File**: `goasm/EDITS.md`

Running log of every manual change to extracted files. Format:

```markdown
# Manual Edits to Extracted goasm/ Files

These edits were applied after extraction to remove dependencies on
skipped packages (dwarf, goobj, pcln, fips140). Reference this file
when re-running scripts/extract-goasm.sh after a Go upgrade.

## obj/plist.go
- Added `AssembleBlock(ctxt *Link, sym *LSym, newprog ProgAlloc)` after
  line N — stripped pipeline without DWARF/PCLN.
- Commented out `linkpcln(ctxt, s, newprog)` at line N (// GOASM_REMOVED)
- Commented out `ctxt.generateDebugLinesSymbol(...)` at line N
- [etc.]

## obj/link.go
- [list changes]

## obj/sym.go
- [list changes]

## obj/x86/seh.go
- [list changes if any]
```

This log is the key artifact for maintainability. **Add an entry for every edit made during Sub-steps 1.3 and 1.4.** Future re-extractions replay this log.

---

## Sub-step 1.8 — Write `goasm/DRIVE.md`

**File**: `goasm/DRIVE.md`

Documents the assembly pipeline (confirmed from source study):

```markdown
# Assembly Pipeline Notes

## How bytes end up in LSym.P (amd64)

Confirmed by reading obj/plist.go, obj/ld.go, obj/pass.go, obj/x86/obj6.go:

1. `obj.AssembleBlock(ctxt, sym, newprog)` (our wrapper in plist.go):
   a. `mkfwd(sym)` — defined in obj/ld.go; sets Prog.Forwd pointers for
      forward-branch resolution
   b. `linkpatch(ctxt, sym, newprog)` — in obj/pass.go; validates addresses,
      calls arch Progedit per-insn, resolves branch targets to *Prog pointers
   c. `ctxt.linkArch.Preprocess(ctxt, sym, newprog)` — x86: obj6.go;
      scans for ATEXT → sets sym.Func().Text; rewrites TLS, expands RET
      (pop BP + RET), stack-frame analysis
   d. `ctxt.linkArch.Assemble(ctxt, sym, newprog)` — x86: span6() in asm6.go;
      iterates encoding until sizes stabilize; writes to sym.P

2. sym.P holds the raw machine-code bytes.
3. sym.R holds relocations (none for our in-memory JIT blocks).

## CRITICAL: ATEXT is required

obj6.Preprocess (obj/x86/obj6.go line ~620) scans for p.As == ATEXT.
When found, it sets:  c.cursym.Func().Text = p
WITHOUT this, sym.Func().Text stays nil and Preprocess returns early —
no instructions get encoded.

The ATEXT prog must be the FIRST prog in the chain:
  p.As = obj.ATEXT
  p.From.Type = obj.TYPE_MEM
  p.From.Sym = sym           ← the LSym being assembled
  p.From.Name = obj.NAME_EXTERN
  p.To.Type = obj.TYPE_TEXTSIZE
  p.To.Offset = 0            ← frame size 0 (trampoline owns the frame)
  p.To.Val = int32(0)        ← arg size

## Prog setup checklist

For AMD64:
- p.As = instruction opcode (e.g. x86.AMOVQ, x86.AADDQ, obj.ARET)
- p.From.Type = obj.TYPE_REG / TYPE_CONST / TYPE_MEM
- p.From.Reg = register constant (x86.REG_AX etc.)
- p.From.Offset = immediate value or memory displacement
- p.To.Type / p.To.Reg / p.To.Offset — analogous for destination
- For memory ops: p.From.Name = obj.NAME_NONE (for [reg+offset])
- For branches: p.To.Type = obj.TYPE_BRANCH; p.To.SetTarget(targetProg)
- For forward branches: emit with nil target, fix up with SetTarget after
  emitting the target Prog (before Assemble)
- For RET: p.As = obj.ARET (no From/To fields needed)

## LookupInit

Located in obj/sym.go. Signature:
  func (ctxt *Link) LookupInit(name string, init func(s *LSym)) *LSym

Safe to use — looks up by name, runs init only on first creation.

## span6 signature (confirmed)

  func span6(ctxt *obj.Link, s *obj.LSym, newprog obj.ProgAlloc)

Called via ctxt.linkArch.Assemble which is set to span6 in x86/obj6.go.
```

---

## Sub-step 1.9 — Write `goasm/api_test.go`

**File**: `goasm/api_test.go`

25+ golden test cases. Expected bytes verified via:
```bash
# Write temp .s, assemble, extract .text bytes:
cat > /tmp/t.s << 'EOF'
TEXT ·f(SB),$0-0
  MOVQ $0x42, AX
  RET
EOF
go tool asm -o /tmp/t.o /tmp/t.s
go tool objdump /tmp/t.o
```

### AMD64 test cases:

```go
package goasm_test

import (
    "testing"
    "riscv/goasm"
    "riscv/goasm/obj"
    "riscv/goasm/obj/x86"
)

func amd64Ctx(t *testing.T) *goasm.Ctx {
    t.Helper()
    return goasm.New(goasm.AMD64)
}

// Helper: emit ATEXT prologue prog.
func atext(c *goasm.Ctx) *obj.Prog {
    p := c.NewProg()
    p.As = obj.ATEXT
    p.From.Type = obj.TYPE_MEM
    p.From.Sym = c.Sym()  // Ctx needs a Sym() accessor
    p.To.Type = obj.TYPE_TEXTSIZE
    p.To.Offset = 0
    return p
}

func TestAMD64_MOVQconst_RET(t *testing.T) {
    c := amd64Ctx(t)
    c.Append(atext(c))

    // MOVQ $0x42, AX
    p := c.NewProg()
    p.As = x86.AMOVQ
    p.From.Type = obj.TYPE_CONST
    p.From.Offset = 0x42
    p.To.Type = obj.TYPE_REG
    p.To.Reg = goasm.REG_AMD64_AX
    c.Append(p)

    // RET
    r := c.NewProg()
    r.As = obj.ARET
    c.Append(r)

    got, err := c.Assemble()
    if err != nil { t.Fatal(err) }
    // MOVQ $0x42, AX = 48 C7 C0 42 00 00 00; RET = C3
    want := []byte{0x48, 0xC7, 0xC0, 0x42, 0x00, 0x00, 0x00, 0xC3}
    if !bytesEqual(got, want) {
        t.Errorf("got %X want %X", got, want)
    }
}

func TestAMD64_ADDQ_RR(t *testing.T) { /* ADDQ BX,AX */ }
func TestAMD64_SUBQ_imm(t *testing.T) { /* SUBQ $1,AX == DECQ */ }
func TestAMD64_MOVQ_load(t *testing.T) { /* MOVQ 0(BX),AX */ }
func TestAMD64_MOVQ_store(t *testing.T) { /* MOVQ AX,8(BX) */ }
func TestAMD64_MOVQ_load_disp8(t *testing.T) { /* MOVQ 16(BX),AX */ }
func TestAMD64_CMPQ_JEQ(t *testing.T) { /* CMPQ AX,BX; JEQ target */ }
func TestAMD64_XORQ_zeroidiom(t *testing.T) { /* XORQ AX,AX → 48 31 C0 */ }
func TestAMD64_IMULQ(t *testing.T) { /* IMULQ BX,AX */ }
func TestAMD64_SHLQ_imm(t *testing.T) { /* SHLQ $3,AX */ }
func TestAMD64_SHLQ_reg(t *testing.T) { /* SHLQ CX,AX */ }
func TestAMD64_ANDQ(t *testing.T) { /* ANDQ BX,AX */ }
func TestAMD64_ORQ(t *testing.T) { /* ORQ BX,AX */ }
func TestAMD64_XORQ(t *testing.T) { /* XORQ BX,AX */ }
func TestAMD64_MOVQ_R12(t *testing.T) { /* MOVQ AX,R12 — tests REX.R */ }
func TestAMD64_MOVQ_large_const(t *testing.T) { /* MOVQ $0x123456789, AX — 10 bytes */ }
func TestAMD64_MOVL_zeroext(t *testing.T) { /* MOVL AX,AX — zero extends */ }
func TestAMD64_MOVSD_load(t *testing.T) { /* MOVSD 0(BX),X0 */ }
func TestAMD64_ADDSD(t *testing.T) { /* ADDSD X1,X0 */ }
func TestAMD64_MULSD(t *testing.T) { /* MULSD X1,X0 */ }
func TestAMD64_SQRTSD(t *testing.T) { /* SQRTSD X0,X0 */ }
func TestAMD64_branch_forward(t *testing.T) { /* forward JMP; backward JEQ */ }
func TestAMD64_NEGQ(t *testing.T) { /* NEGQ AX */ }
func TestAMD64_NOTQ(t *testing.T) { /* NOTQ AX */ }
func TestAMD64_MOVBQSX(t *testing.T) { /* MOVBQSX — sign-extend byte */ }

// --- ARM64 tests (only run on arm64 host or via cross-arch ctx) ---

func TestARM64_MOVDconst_RET(t *testing.T) { /* MOVD $0x42,R0; RET */ }
func TestARM64_ADD_3reg(t *testing.T) { /* ADD R1,R2,R3 */ }
func TestARM64_MUL(t *testing.T) { /* MUL R1,R2,R3 */ }
func TestARM64_SMULH(t *testing.T) { /* SMULH R1,R2,R3 */ }
func TestARM64_SDIV(t *testing.T) { /* SDIV R1,R2,R3 */ }
func TestARM64_MOVD_load(t *testing.T) { /* MOVD (R0),R1 */ }
func TestARM64_MOVD_store(t *testing.T) { /* MOVD R1,8(R0) */ }
func TestARM64_CBZ(t *testing.T) { /* CBZ R0,target */ }
func TestARM64_BEQ(t *testing.T) { /* CMP R0,R1; BEQ target */ }
func TestARM64_FADDD(t *testing.T) { /* FADDD F0,F1,F2 */ }
func TestARM64_FMOVD_load(t *testing.T) { /* FMOVD (R0),F0 */ }
func TestARM64_LSL_imm(t *testing.T) { /* LSL $3,R0,R1 */ }
func TestARM64_ANDS(t *testing.T) { /* AND R0,R1,R2 */ }

func bytesEqual(a, b []byte) bool {
    if len(a) != len(b) { return false }
    for i := range a { if a[i] != b[i] { return false } }
    return true
}
```

**How to derive expected bytes**:
```bash
# For each test, write the instruction as Go assembly, assemble, dump:
go tool asm -S /dev/null 2>&1  # lists opcodes
# Or use objdump on small ELF
```

The expected bytes for each test MUST be verified with `go tool asm` and `go tool objdump`, not guessed. This is the validation that proves the extraction produces correct output.

---

## Sub-step 1.10 — Expose `Sym()` on `Ctx`

The test helper `atext()` needs access to the LSym. Add to `api.go`:
```go
// Sym returns the text LSym for this block (needed for ATEXT setup).
func (c *Ctx) Sym() *obj.LSym { return c.sym }
```

---

## Sub-step 1.11 — Verify `go build ./goasm/...` and `go test ./goasm/...`

Final acceptance criteria:
1. `go build ./goasm/...` — zero errors, zero warnings
2. `go vet ./goasm/...` — zero issues
3. `go test ./goasm/ -v -run TestAMD64` — all AMD64 tests pass
4. `go test ./goasm/ -v -run TestARM64` — all ARM64 tests pass (even on amd64 host, the encoder works cross-arch)
5. `go test ./...` (root) — existing RISC-V tests still pass (goasm doesn't break anything)

---

## Sub-step 1.12 — EXTRACTED.txt and LICENSE check

Verify `goasm/LICENSE` is BSD-3-Clause (Go's license). Verify `goasm/EXTRACTED.txt` records the Go version used. This is the license documentation for the extraction.

---

## Critical Files

| File | Role |
|---|---|
| `scripts/extract-goasm.sh` | Source of truth for extraction |
| `goasm/stub/stub.go` | Stubs for skipped deps |
| `goasm/obj/plist.go` | Add `AssembleBlock()` here |
| `goasm/obj/link.go` | Surgery for DWARF method bodies |
| `goasm/obj/sym.go` | Surgery for dwarf calls |
| `goasm/api.go` | Public API — `New`, `Append`, `Assemble` |
| `goasm/regs.go` | Register constants |
| `goasm/api_test.go` | 25+ golden tests |
| `goasm/EDITS.md` | Maintenance log |
| `goasm/DRIVE.md` | Pipeline documentation |
| `$GOROOT/src/cmd/internal/obj/plist.go` | Study source |
| `$GOROOT/src/cmd/internal/obj/x86/asm6.go` | Study source |
| `$GOROOT/src/cmd/internal/obj/x86/obj6_test.go` | Reference for test setup |

---

## Dependency Map

```
goasm/          → goasm/obj, goasm/objabi, goasm/sys (register constants)
goasm/obj/      → goasm/objabi, goasm/src, goasm/sys, goasm/hash, goasm/stub
goasm/obj/x86/  → goasm/obj, goasm/objabi, goasm/sys
goasm/obj/arm64/→ goasm/obj, goasm/objabi, goasm/sys
goasm/objabi/   → (stdlib only: flag, runtime/debug, etc.)
goasm/src/      → (stdlib only)
goasm/sys/      → (stdlib only)
goasm/hash/     → (stdlib only: crypto/sha256 or internal/hash)
goasm/stub/     → (nothing — stdlib only)
```

---

## Execution Order

1. `bash scripts/extract-goasm.sh` — copy + rewrite imports
2. Create `goasm/stub/stub.go` with base stubs
3. `go build ./goasm/...` — iterate on stubs until clean
4. Apply code surgery (1.4a–1.4f) — iterate until clean
5. Write `goasm/api.go` + `goasm/regs.go`
6. Verify exact register constant names by reading extracted `a.out.go` files
7. Determine expected bytes for each test using `go tool asm` + `go tool objdump`
8. Write `goasm/api_test.go` with verified golden bytes
9. `go test ./goasm/ -v` — all pass
10. `go test ./...` — nothing regressed
11. Update `goasm/EDITS.md` with every change
12. Write `goasm/DRIVE.md` with pipeline notes

---

## Verification

End-to-end verification:
```bash
# Phase 1 complete when:
go build ./goasm/...       # zero errors
go vet ./goasm/...         # zero issues
go test ./goasm/ -v        # all golden tests pass
go test ./... -count=1     # root tests still pass

# Smoke test: can encode MOVQ $1, AX; RET and call it
# (written as a test in api_test.go using mmap+call)
```

The **most critical test** is one that:
1. Assembles a trivial function (e.g., `MOVQ $42, AX; RET`)
2. mmaps the bytes as executable
3. Calls via `jitcall.Call`
4. Verifies the return value is 42

This proves the full encode→execute path works, not just that bytes match a reference.

---

## Risks and Mitigations

| Risk | Mitigation |
|---|---|
| `LookupInit` doesn't exist in Go 1.22 obj | Use `Lookup` + manual init |
| `AssembleBlock` needs ATEXT Prog as first instruction | Add ATEXT setup in `Ctx.Append` or document requirement clearly in api.go |
| Stub types have wrong method signatures | Add methods as compile errors reveal them |
| ARM64 encoder fails cross-arch on amd64 host | Check if encoder is gated on GOARCH; if so, use `//go:build` in arm64 tests |
| `objabi.Hlinux` may not be the right head type | Check `Headtype` usage in obj6.go — if it causes errors use `objabi.Hdarwin` on macOS |
| seh.go x86 requires Windows-only imports | Add `//go:build windows` guard |
| `hash` package is `crypto/internal/hash` not `cmd/internal/hash` | Verify actual package; may not exist as `cmd/internal/hash` |
