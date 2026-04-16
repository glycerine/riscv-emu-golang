# Manual Edits to Extracted goasm/ Files

Re-running `scripts/extract-goasm.sh` overwrites these files. After re-extraction,
re-apply every edit below in the order listed.

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

**Add `AssembleBlock` function** before `InitTextSym`:
```go
// AssembleBlock is a stripped-down Flushplist that encodes a single text
// symbol to native bytes (sym.P) without DWARF, PCLN, or SEH.
//
// The Prog chain must begin with an ATEXT Prog so that Preprocess sets
// sym.Func().Text correctly. Call InitTextSym first to initialize FuncInfo.
func AssembleBlock(ctxt *Link, sym *LSym, newprog ProgAlloc) {
    if ctxt.Arch.ErrorCheck != nil {
        ctxt.Arch.ErrorCheck(ctxt, sym)
    }
    mkfwd(sym)
    linkpatch(ctxt, sym, newprog)
    ctxt.Arch.Preprocess(ctxt, sym, newprog)
    ctxt.Arch.Assemble(ctxt, sym, newprog)
}
```

**Remove `linkpcln` and `populateDWARF` calls** in `Flushplist`'s "Turn functions into machine code" loop:
```go
// DELETE these two lines:
linkpcln(ctxt, s)
ctxt.populateDWARF(plist.Curfn, s)
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

---

## goasm/obj/sym.go

**Remove `"cmd/internal/goobj"` import**.

**Remove `NumberSyms` function entirely** (~125 lines, from `func (ctxt *Link) NumberSyms()` through its closing `}`). This function uses `goobj.PkgIdx*` constants and is only needed for object-file writing.

**Remove `isNonPkgSym` function** if it has no remaining callers (it was only called from `NumberSyms`). If the compiler complains it's unused, delete it. If it is kept as dead code, add a comment.

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
