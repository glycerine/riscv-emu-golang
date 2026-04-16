# Manual Edits to Extracted goasm/ Files

Re-running `scripts/extract-goasm.sh` overwrites these files. After re-extraction,
re-apply every edit below in the order listed.

To verify the current set of in-tree edits against vanilla Go, run:

```bash
diff -ru $GOROOT/src/cmd/internal/obj/  goasm/obj/  | less
```

The intent is that this diff shrinks over time. Anything new shown by
the diff that isn't documented below should be added here (or dropped
from the tree if redundant).

---

## goasm/obj/link.go

**Remove imports** (lines were `"cmd/internal/dwarf"` and `"cmd/internal/goobj"`):
- Delete both import lines entirely from the import block.

**Remove `DwFixups`, `DwTextCount`, `Imports`, `DebugInfo` fields** from the `Link` struct:
```go
// DELETE these lines:
DwFixups             *DwarfFixupTable
DwTextCount          int
Imports              []goobj.ImportedPkg
DebugInfo            func(ctxt *Link, fn *LSym, info *LSym, curfn Func) ([]dwarf.Scope, dwarf.InlCalls)
```

**Remove `Fingerprint` field** from the `Link` struct:
```go
// DELETE this line:
Fingerprint goobj.FingerprintType // fingerprint of symbol indices, to catch index mismatch
```

**Change `UsedFiles` type** in the `Pcln` struct:
```go
// FROM:
UsedFiles map[goobj.CUFileIndex]struct{}
// TO:
UsedFiles map[uint32]struct{}
```

---

## goasm/obj/line.go

**Remove entire `AddImport` function** and its `"cmd/internal/goobj"` import.
Keep only `getFileIndexAndLine`. Final file:
```go
package obj

import (
    "riscv/goasm/src"
)

func (ctxt *Link) getFileIndexAndLine(xpos src.XPos) (int, int32) {
    pos := ctxt.InnermostPos(xpos)
    if !pos.IsKnown() {
        pos = src.Pos{}
    }
    return pos.FileIndex(), int32(pos.RelLine())
}
```

---

## goasm/obj/plist.go

**Add `AssembleBlock` function** before `InitTextSym`. The order matches
upstream `Flushplist` exactly so future Go upgrades produce minimal
diffs:
```go
// AssembleBlock is a stripped-down Flushplist that encodes a single
// text symbol to native bytes (sym.P) without DWARF, PCLN, or SEH.
//
// The Prog chain must begin with an ATEXT Prog so that Preprocess sets
// sym.Func().Text correctly. Call InitTextSym first to initialize
// FuncInfo.
//
// The pass order mirrors Flushplist exactly (mkfwd, ErrorCheck,
// linkpatch, Preprocess, Assemble).
func AssembleBlock(ctxt *Link, sym *LSym, newprog ProgAlloc) {
    mkfwd(sym)
    if ctxt.Arch.ErrorCheck != nil {
        ctxt.Arch.ErrorCheck(ctxt, sym)
    }
    linkpatch(ctxt, sym, newprog)
    ctxt.Arch.Preprocess(ctxt, sym, newprog)
    ctxt.Arch.Assemble(ctxt, sym, newprog)
}
```

**Remove `setFIPSType` calls** in `InitTextSym` and `GloblPos`:
```go
// In InitTextSym, DELETE:
s.setFIPSType(ctxt)
// And DELETE the "Set up DWARF entries" comment and:
ctxt.dwarfSym(s)

// In GloblPos, DELETE:
s.setFIPSType(ctxt)
```

(The `linkpcln` and `populateDWARF` calls referenced in earlier
revisions of this file were not present in the Go 1.26 extraction;
nothing to remove.)

---

## goasm/obj/sym.go

**Remove `"cmd/internal/goobj"` import**.

**Remove `NumberSyms` function entirely** (~125 lines, from `func (ctxt *Link) NumberSyms()` through its closing `}`). This function uses `goobj.PkgIdx*` constants and is only needed for object-file writing.

**Remove `isNonPkgSym` function entirely** — was only called from
`NumberSyms`, has no remaining callers in the goasm extraction.

**Fix `usedFiles` type** in `traverseFuncAux`:
```go
// FROM:
usedFiles := make([]goobj.CUFileIndex, 0, len(pc.UsedFiles))
// TO:
usedFiles := make([]uint32, 0, len(pc.UsedFiles))
```

---

## goasm/obj/data.go

**Remove `setFIPSType` calls** in `prepwrite`:
```go
// DELETE these lines:
s.setFIPSType(ctxt)   // after s.Type = objabi.SDATA
s.setFIPSType(ctxt)   // after s.Type = objabi.SNOPTRDATA
```

**Remove `checkFIPSReloc` call** in `AddRel`:
```go
// DELETE the if-block:
if s.Type.IsFIPS() {
    s.checkFIPSReloc(ctxt, rel)
}
```

---

## goasm/objabi/flag.go

**Remove `"internal/bisect"` import**.

**In `NewDebugFlag`**, change the type-switch panic message and remove `**bisect.Matcher` case:
```go
// FROM:
panic(fmt.Sprintf("debug.%s has invalid type %v (must be int, string, or *bisect.Matcher)", f.Name, f.Type))
case *int, *string, **bisect.Matcher:
// TO:
panic(fmt.Sprintf("debug.%s has invalid type %v (must be int or string)", f.Name, f.Type))
case *int, *string:
```

**In `DebugFlag.Set`**, remove the `**bisect.Matcher` case from the type-switch:
```go
// DELETE this case block:
case **bisect.Matcher:
    var err error
    *vp, err = bisect.New(valstring)
    if err != nil {
        log.Fatalf("debug flag %v: %v", name, err)
    }
```

---

## Deleted test files (add to skip lists in extract-goasm.sh if not already there)

These files import `internal/testenv` which is stdlib-only:
- `goasm/abi/abi_test.go` — delete after extraction
- `goasm/objabi/flag_test.go` — delete after extraction
- `goasm/objabi/line_test.go` — delete after extraction
- `goasm/objabi/path_test.go` — delete after extraction

These are already in the `copy_pkg` skip lists in the extract script.

`goasm/obj/arm64/asm_arm64_test.go` — delete after extraction. Its
companion `asm_arm64_test.s` (forward-declared bodies for testvmovs /
testmovk / testCombined) is not extracted. Without the .s, the test
fails to build on `GOARCH=arm64` with "missing function body". The
extract script now lists this file in the skip list for `obj/arm64/`.

---

## Files left intact (no edits needed, despite earlier plan suggestions)

- `goasm/obj/x86/seh.go` — Compiles and runs cleanly on all platforms in
  the host matrix. The earlier plan suggested a `//go:build windows`
  guard; not required.

---

## NOT edited: log.Fatalf and ctxt.DiagFlush call sites

The 17 `ctxt.DiagFlush()` + `log.Fatalf(...)` call sites in
`obj/{x86,arm64,arm,loong64,mips,ppc64,riscv,s390x}/*.go` are
intentionally left as-is. `goasm.Ctx.init()` installs `DiagFlush` as a
function that panics with a typed value; `goasm.Ctx.Assemble()` uses a
deferred recover to convert that panic into a normal error return. The
`log.Fatalf` line that follows is therefore unreachable in practice
(the panic unwinds the goroutine first), but is left as a safety net /
documentation aid.
