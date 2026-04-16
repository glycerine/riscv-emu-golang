# Assembly Pipeline Notes

## How bytes end up in LSym.P

Confirmed by reading obj/plist.go, obj/ld.go, obj/pass.go, obj/x86/obj6.go,
obj/arm64/obj7.go, and the AssembleBlock wrapper we added to obj/plist.go.

### Call order (goasm.Ctx.Assemble)

```
1. ctxt.InitTextSym(sym, 0, src.NoXPos)  — initialises FuncInfo; sets up sym.Extra
2. obj.AssembleBlock(ctxt, sym, newprog)
       a. ctxt.Arch.ErrorCheck(ctxt, sym) — arch-specific sanity check (if set)
       b. mkfwd(sym)            — obj/ld.go; sets Prog.Forwd for forward-branch resolution
       c. linkpatch(ctxt, sym, newprog)  — obj/pass.go; per-instruction Progedit + branch fixup
       d. ctxt.Arch.Preprocess(ctxt, sym, newprog)
              x86: obj6.Preprocess — scans for ATEXT → sets sym.Func().Text;
                   expands RET (pop BP + RET on AMD64), handles TLS, stack analysis
              arm64: obj7.Preprocess — similar ATEXT scan, stack frame setup
       e. ctxt.Arch.Assemble(ctxt, sym, newprog)
              x86: span6() in asm6.go — iterates encoding until sizes stabilise → sym.P
              arm64: span7() in asm7.go
3. sym.P  — raw machine-code bytes
4. sym.R  — relocations (empty for in-memory JIT blocks with no symbol references)
```

### Why InitTextSym comes before AssembleBlock

`LSym.Func()` panics if `sym.Extra` has not been initialised to a `*FuncInfo`.
`InitTextSym` (obj/plist.go) allocates FuncInfo via `sym.NewFuncInfo()` and
stores it in `sym.Extra`. Preprocess then accesses `sym.Func()` safely.

Without InitTextSym the very first call to `sym.Func()` inside Preprocess
returns nil and Preprocess bails out immediately — no instructions are encoded.

### sym.Func().Text must be set explicitly

After `InitTextSym`, the caller **must** set `sym.Func().Text = atextProg`
(the ATEXT Prog, the first in the chain). This is what `cmd/asm/asm.go` does
at line 198: `nameAddr.Sym.Func().Text = prog`.

Both x86 Preprocess and arm64 Preprocess start with:
```go
if cursym.Func().Text == nil || cursym.Func().Text.Link == nil {
    return
}
```
If Text is nil, they return immediately and sym.P stays empty (no bytes encoded).

`goasm.Ctx.Assemble()` sets this:
```go
c.sym.Func().Text = c.firstProg   // after InitTextSym
```

### LinkArch.Init must be called

Architecture-specific instruction encoding tables (e.g. `instinit` for x86)
are registered as `LinkArch.Init`. The Go compiler calls this from
`ssagen.Arch.LinkArch.Init(base.Ctxt)` in `cmd/compile/internal/gc/main.go`.

`goasm.newLinkCtx()` must call it explicitly:
```go
if la.Init != nil {
    la.Init(ctxt)
}
```
Without this, x86 assembly fails with:
  `x86 tables not initialized, call x86.instinit first`

---

## CRITICAL: ATEXT is required

`obj6.Preprocess` (obj/x86/obj6.go) scans the Prog chain for `p.As == ATEXT`.
When found it sets:
```go
c.cursym.Func().Text = p
```
Without this, `sym.Func().Text` stays nil and Preprocess returns early —
**no instructions get encoded, sym.P stays empty**.

The ATEXT Prog must be the **first** Prog in the chain.

### Minimal ATEXT setup

```go
p := ctxt.NewProg()
p.As = obj.ATEXT
p.From.Type = obj.TYPE_MEM
p.From.Sym = sym            // the LSym being assembled
p.From.Name = obj.NAME_EXTERN
p.To.Type = obj.TYPE_TEXTSIZE
p.To.Offset = 0             // frame size 0 (trampoline owns the frame)
p.To.Val = int32(0)         // arg size 0
```

The goasm.Ctx.NewATEXT() helper builds this correctly.

---

## Prog setup reference

### AMD64 instruction Prog fields

| Field | Purpose |
|---|---|
| `p.As` | Instruction opcode (e.g. `x86.AMOVQ`, `x86.AADDQ`, `obj.ARET`) |
| `p.From.Type` | Source operand kind: `obj.TYPE_REG`, `TYPE_CONST`, `TYPE_MEM` |
| `p.From.Reg` | Source register constant (e.g. `x86.REG_AX`) |
| `p.From.Offset` | Immediate value or memory displacement |
| `p.From.Name` | Memory addressing mode: `obj.NAME_NONE` for `[reg+disp]` |
| `p.To.Type` | Destination operand kind |
| `p.To.Reg` | Destination register |
| `p.To.Offset` | Destination displacement |

### Memory operand (e.g. `MOVQ 8(BX), AX`)

```go
p.As = x86.AMOVQ
p.From.Type = obj.TYPE_MEM
p.From.Reg = x86.REG_BX
p.From.Offset = 8
p.From.Name = obj.NAME_NONE   // [reg+offset], no symbol
p.To.Type = obj.TYPE_REG
p.To.Reg = x86.REG_AX
```

### Branch (forward)

```go
jmp := ctxt.NewProg()
jmp.As = x86.AJMP            // or x86.AJEQ etc.
jmp.To.Type = obj.TYPE_BRANCH
// jmp.To.SetTarget(targetProg) — call after emitting targetProg
```

Forward branches: emit the branch Prog first with nil target, emit the target
Prog, then call `jmp.To.SetTarget(targetProg)` before calling Assemble.

### RET

```go
r := ctxt.NewProg()
r.As = obj.ARET
// no From/To fields needed
```

On AMD64, Preprocess expands ARET to `POP BP; RET` when a frame pointer was set up.
For JIT trampolines with frame size 0, it stays as a bare `RET`.

---

## LookupInit

Located in obj/sym.go:
```go
func (ctxt *Link) LookupInit(name string, init func(s *LSym)) *LSym
```
Looks up by name; runs `init` only on first creation.  Safe to call from New/Reset.

---

## span6 signature (confirmed)

```go
func span6(ctxt *obj.Link, s *obj.LSym, newprog obj.ProgAlloc)
```
Called via `ctxt.Arch.Assemble` which is set to `span6` in `x86/obj6.go init()`.
Iterates encoding until all instruction sizes stabilise, then writes to `s.P`.

---

## InitTextSym signature (confirmed, Go 1.22)

```go
func InitTextSym(ctxt *Link, s *LSym, flag int, pos src.XPos)
```
Located in obj/plist.go. `flag` is `obj.NOSPLIT`, `obj.NOFRAME`, etc. or 0 for
plain text symbols. `pos` is the source position; pass `src.NoXPos` for JIT blocks.

---

## AssembleBlock (our addition to obj/plist.go)

```go
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

Stripped pipeline: no `linkpcln` (pcln.go excluded), no `populateDWARF`,
no DWARF line-number table, no SEH.

---

## Headtype

`obj6.Preprocess` inspects `ctxt.Headtype` for TLS (thread-local storage) and
stack-frame decisions.  For JIT blocks that do not use TLS, almost any valid
headtype works.  goasm.Ctx sets it to the host OS:

```go
switch runtime.GOOS {
case "darwin": ctxt.Headtype = objabi.Hdarwin
case "linux":  ctxt.Headtype = objabi.Hlinux
...
}
```

---

## goasm.Ctx API summary

```go
c := goasm.New(goasm.AMD64)   // or ARM64, RISCV, etc.
c.Append(c.NewATEXT())        // always first
// ... append instruction Progs built with c.NewProg() ...
c.Append(c.NewRET())          // terminate chain
bytes, err := c.Assemble()    // returns raw machine code
```

Full architecture list available via `goasm.LinkArch(arch)` — returns a
`*obj.LinkArch` for the requested target.
