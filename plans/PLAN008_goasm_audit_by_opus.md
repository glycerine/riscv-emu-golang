# Plan: Audit & Bug Fixes for `goasm/` Package

## Context

The `goasm/` package (`/Users/jaten/ris/goasm/`) is a freshly extracted copy of
Go 1.26.2's `cmd/internal/obj` instruction encoders, repackaged as a standalone,
BSD-3-Clause, no-cgo JIT-emission library for the `riscv` emulator (Plan 006B,
Phase 1). All 18 tests currently pass and the package compiles cleanly. This
plan is an audit / code review that catalogs every bug, sharp edge, missing
feature, and stylistic issue found, and prescribes a fix for each.

The critical invariant is: **`goasm.Ctx` must produce the exact bytes that
`go tool asm` would produce for the equivalent .s source, and produce a clean
error (never a `log.Fatalf` or nil-deref panic) on malformed input.** The
audit found several places where this invariant is violated and several
correctness/usability gaps that will bite real callers (especially the planned
JIT integration in later phases).

Critical files referenced:
- `/Users/jaten/ris/goasm/api.go`           — public API
- `/Users/jaten/ris/goasm/api_test.go`      — golden + smoke tests
- `/Users/jaten/ris/goasm/regs.go`          — register constant re-exports
- `/Users/jaten/ris/goasm/DRIVE.md`         — pipeline notes
- `/Users/jaten/ris/goasm/EDITS.md`         — extraction edit log
- `/Users/jaten/ris/goasm/EXTRACTED.txt`    — Go-version stamp
- `/Users/jaten/ris/goasm/obj/plist.go`     — `AssembleBlock`, `InitTextSym`
- `/Users/jaten/ris/goasm/obj/x86/obj6.go`  — `preprocess`, `errorCheck`, `instinit` callsite
- `/Users/jaten/ris/goasm/obj/arm64/obj7.go` — `preprocess`, `buildop` callsite
- `/Users/jaten/ris/goasm/obj/sym.go`       — `Linknew`, `LookupInit`
- `/Users/jaten/ris/goasm/obj/link.go`      — `Diag`, `LSym.Func`, `LSym.NewFuncInfo`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/scripts/extract-goasm.sh`

---

## Severity legend

- **P0** — runtime correctness bug or crash on plausible input; must fix before
  the package can be used as a JIT backend.
- **P1** — silent misbehavior, missing safety check, or major omission likely
  to cause integration pain; should fix soon.
- **P2** — style, doc accuracy, or extraction-script hygiene; fix when convenient.

---

## Issue 1 — `ctxt.DiagFlush` is nil; reachable code calls it then log.Fatalf-s; convert to a recoverable panic so Assemble can return a clean error (P0)

**Where**: `api.go:56-66` (`init`) leaves `ctxt.DiagFlush` nil. 17 sites
in the extracted obj/* packages call `ctxt.DiagFlush()`, in every case
immediately followed by `log.Fatalf`. Sites:

- `obj/x86/asm6.go:4844, 4870, 4895, 5342` (bad CALL/code paths)
- `obj/x86/obj6.go:772`              (auto-SPWRITE in non-IsAsm path; gated by Issue 2)
- `obj/arm64/obj7.go:885`            (auto-SPWRITE; gated by Issue 2)
- `obj/arm64/asm7.go:2741`           (assembler error)
- `obj/arm/asm5.go:1274`             (assembler error)
- `obj/arm/obj5.go:517`              (auto-SPWRITE; gated by Issue 2)
- `obj/loong64/asm.go:1377`          (assembler error)
- `obj/loong64/obj.go:478`           (auto-SPWRITE; gated by Issue 2)
- `obj/mips/asm0.go:931`             (assembler error)
- `obj/mips/obj0.go:484`             (auto-SPWRITE; gated by Issue 2)
- `obj/ppc64/obj9.go:1109`           (auto-SPWRITE; gated by Issue 2)
- `obj/riscv/obj.go:650`             (auto-SPWRITE; gated by Issue 2)
- `obj/s390x/objz.go:488`            (auto-SPWRITE; gated by Issue 2)

If user input ever triggers any of these paths today we get
"panic: runtime error: invalid memory address or nil pointer
dereference" inside the nil `DiagFlush` call — the worst-case failure
mode for a library: an unrecoverable panic with no useful message. Even
if we set DiagFlush to a no-op, the next line (`log.Fatalf`) would still
terminate the host process, which is unacceptable for a library used
inside a long-running emulator.

**Fix**: in `Ctx.init()`, install `DiagFlush` as a small function that
**panics** with the accumulated diagnostics. Then have `Assemble`
recover from that panic and return it as a regular `error`. This
converts the otherwise-fatal exit into a normal Go error path, so a
malformed Prog list can be reported to the caller without bringing down
the surrounding process.

The `log.Fatalf` lines in extracted code stay exactly as they are —
they are reference / safety net, but in practice they are unreachable
because the panic unwinds the goroutine before the next statement
executes.

We also defensively wire `ctxt.Bso` to a discard writer, so any code
that calls `ctxt.Logf(...)` (which dereferences `Bso`) does not nil-deref
before reaching the panic-DiagFlush path. The auto-SPWRITE branches all
do `ctxt.Logf(...)` then `ctxt.Diag(...)` then `ctxt.DiagFlush()`; without
the Bso wiring, we'd nil-deref in Logf instead of cleanly panicking in
DiagFlush. Issue 2 (set `IsAsm=true`) makes the SPWRITE branch
unreachable in practice, but the Bso plumbing remains a cheap belt-and-
suspenders for any other future Logf call site.

Concretely, edit `api.go`:

```go
// goasmFatal is the type panicked from DiagFlush. Assemble recovers it
// and returns it as an error. We use a distinct type so that other
// panics (real bugs) propagate normally.
type goasmFatal struct {
    diags []string
}

func (e *goasmFatal) Error() string {
    return "goasm: assembler raised fatal: " + strings.Join(e.diags, "; ")
}

func (c *Ctx) init() {
    c.ctxt = newLinkCtx(c.arch)
    c.ctxt.DiagFunc = func(msg string, args ...any) {
        c.errors = append(c.errors, fmt.Sprintf(msg, args...))
    }
    // DiagFlush is invoked by the extracted obj/* code immediately
    // before log.Fatalf in unrecoverable error paths. We panic with a
    // typed value so Assemble's deferred recover converts the fatal
    // path into a normal error return — log.Fatalf is left in the
    // extracted source for reference, but the panic unwinds the
    // goroutine before it executes.
    c.ctxt.DiagFlush = func() {
        snapshot := append([]string(nil), c.errors...)
        panic(&goasmFatal{diags: snapshot})
    }
    // Bso would be dereferenced by ctxt.Logf in the same fatal paths.
    // Wire it to a discard writer so we don't nil-deref before
    // reaching DiagFlush.
    c.ctxt.Bso = bufio.NewWriter(io.Discard)

    c.sym = c.ctxt.LookupInit("jit_block", func(s *obj.LSym) {
        s.Type = objabi.STEXT
    })
    c.firstProg = nil
    c.last = nil
}
```

…and wrap `Assemble`:

```go
func (c *Ctx) Assemble() (out []byte, err error) {
    if c.firstProg == nil {
        return nil, fmt.Errorf("goasm: empty prog list")
    }
    c.last.Link = nil

    defer func() {
        if r := recover(); r != nil {
            if gf, ok := r.(*goasmFatal); ok {
                err = gf
                out = nil
                return
            }
            // Not our panic — re-raise so real bugs surface normally.
            panic(r)
        }
    }()

    c.ctxt.InitTextSym(c.sym, 0, src.NoXPos)
    c.sym.Func().Text = c.firstProg
    obj.AssembleBlock(c.ctxt, c.sym, c.ctxt.NewProg)

    if len(c.errors) > 0 {
        return nil, fmt.Errorf("goasm: assembly errors: %v", c.errors)
    }
    out = make([]byte, len(c.sym.P))
    copy(out, c.sym.P)
    return out, nil
}
```

Add `bufio`, `io`, and `strings` to the import block. `strings` is for
`Error()`; `bufio`+`io` for the discard writer.

**Verification**: write `TestErr_FatalRecovers` that emits a Prog
guaranteed to hit one of the 17 fatal sites — the simplest is
`p.As = obj.ACALL; p.To.Type = obj.TYPE_BRANCH` (no target, no Sym;
hits `obj/x86/asm6.go:4842-4845`):

```go
func TestErr_FatalRecovers(t *testing.T) {
    c := goasm.New(goasm.AMD64)
    c.Append(c.NewATEXT())
    bad := c.NewProg()
    bad.As = obj.ACALL
    bad.To.Type = obj.TYPE_BRANCH
    // intentionally leave bad.To.Sym == nil and no target
    c.Append(bad)
    c.Append(c.NewRET())

    _, err := c.Assemble()
    if err == nil {
        t.Fatal("expected error from malformed CALL, got nil")
    }
    if !strings.Contains(err.Error(), "call without target") {
        t.Errorf("expected 'call without target' in error, got: %v", err)
    }
}
```

The test must complete normally (no nil-deref panic, no process exit
from `log.Fatalf`). `go test ./goasm/ -run TestErr_FatalRecovers -v`
prints PASS.

Also add a negative test that asserts non-`goasmFatal` panics still
propagate (so we don't accidentally swallow genuine bugs in the
extracted encoder). Done by injecting a panic via a custom prog
preprocessor — or omit if too contrived.

---

## Issue 2 — `ctxt.IsAsm` left false; triggers auto-SPWRITE log.Fatalf and unwanted jump padding (P0)

**Where**: `api.go:56-66` does not set `ctxt.IsAsm`. The Go assembler sets
this to `true` for ALL hand-written assembly. Consequences of leaving it
false:

a. `obj/x86/obj6.go:766-774` and `obj/arm64/obj7.go:881-889`: any Prog that
writes to SP without setting `Spadj` triggers a chain of
`ctxt.Logf` (nil `Bso` deref) → `ctxt.Diag(…)` → `ctxt.DiagFlush()` (nil
deref) → `log.Fatalf("bad SPWRITE")`.

b. `obj/x86/asm6.go:1980-1993` (`needPadJumps`): when `IsAsm` is false the
encoder enables 32-byte jump padding, which inserts NOP padding bytes into
the output to align jumps. This silently changes output bytes for any
non-trivial branch sequence, surprising callers who expect deterministic
encoding.

c. `obj/x86/asm6.go:2184` and `2185`: `if pPrev != nil && !ctxt.IsAsm && c > c0`
records padding fixups in a `nopPad` list — also disabled by setting IsAsm.

Treating goasm Progs as hand-written assembly is exactly correct: we are
the assembler, not the compiler.

**Fix**: in `Ctx.init()`, after creating ctxt:

```go
c.ctxt.IsAsm = true
```

**Side-effect to verify**: `obj/plist.go:102` and `:237` use `IsAsm` to
inject FUNCDATA refs in `Flushplist`. We never call `Flushplist`, so this
has no effect. `obj/sym.go:228` uses it inside the dead `isNonPkgSym`. Safe.

**Verification**: add a test `TestAMD64_Padding_Disabled` that emits a
sequence with a JMP near a 32-byte boundary and asserts the byte length
exactly matches the unpadded encoding (no inserted NOPs). Confirm by
reproducing the bytes with `go tool asm`.

---

## Issue 3 — Default `NewATEXT()` does NOT set NOSPLIT/NOFRAME, so any block with a CALL silently emits prologue + `runtime.morestack` reference (P0)

**Where**: `api.go:84-94` (`NewATEXT`) and `api.go:124` (`InitTextSym(c.sym, 0, src.NoXPos)`).
The flag passed to `InitTextSym` is hard-coded to `0`, so neither
`obj.NOSPLIT` (0x4) nor `obj.NOFRAME` (0x200) is set on the LSym.

For an ATEXT with frame size 0 and no CALL instructions, x86 / arm64
preprocess auto-promote the function to NOFRAME + leaf-NOSPLIT, so today's
tests work. But the moment a caller emits a CALL/BL:

- AMD64 (`obj/x86/obj6.go:637-651, 663-697`): bpsize becomes 8; autoffset
  becomes 8; `stacksplit()` injects code that references
  `runtime.morestack_noctxt` via `ctxt.Lookup("runtime.morestack_noctxt")`
  — a symbol that does not exist in our hash table. The encoder will
  produce a CALL with a relocation to a phantom symbol; at runtime the JIT
  code will jump to garbage.
- ARM64 (`obj/arm64/obj7.go:528-577`): same — autosize += 8 for LR save,
  stacksplit inserts a BL to `runtime.morestack`.

This will be the first thing that breaks when Phase 2 wires up the real
JIT (RISC-V CALL → host CALL).

**Fix**: two-part.

(a) Make the default safe. Change `Assemble()` to pass
`obj.NOSPLIT|obj.NOFRAME`:

```go
const defaultFlags = obj.NOSPLIT | obj.NOFRAME
c.ctxt.InitTextSym(c.sym, defaultFlags, src.NoXPos)
```

(b) Provide a public field on `Ctx` to override (for callers that DO want a
real prologue):

```go
// Flags is the bitmask of obj.{NOSPLIT,NOFRAME,WRAPPER,...} attached to
// the text symbol via InitTextSym. Default: NOSPLIT|NOFRAME (correct for
// trampoline-style JIT blocks that do not own a Go stack frame).
Flags int
```

…and use `c.Flags` (defaulting to `defaultFlags`) in `Assemble()`. Set
the default in `init()`.

**Verification**: add a test `TestAMD64_Call_NoMorestack` that emits an
ATEXT + ACALL to an arbitrary symbol + RET. Assert that
`c.Sym().R` (relocations) does NOT contain a relocation against
`runtime.morestack` / `runtime.morestack_noctxt`. Then add a
`TestAMD64_FlagsOverride_StackCheck` that sets `c.Flags = 0`, emits the
same prog list, and asserts the morestack reloc IS present (proves the
override knob works).

---

## Issue 4 — `Assemble()` is one-shot per Ctx, but the contract is undocumented and the second call silently appends "symbol redeclared" (P1)

**Where**: `api.go:116-141`. Calling `Assemble()` twice without `Reset()`
in between:

1. Second `InitTextSym` call sees `s.Func() != nil` and runs
   `ctxt.Diag("symbol redeclared")`, which appends to `c.errors`. The
   bytes still get encoded, but the returned error is "assembly errors:
   [symbol redeclared]" — confusing.
2. Both calls share the same Prog chain (firstProg/last not cleared by
   Assemble). So Append-after-Assemble silently extends the previously
   encoded chain.

**Fix**: choose ONE of the following and document it.

- **Option A (preferred)**: enforce one-shot usage — at the top of
  `Assemble`, if `c.sym.Func() != nil` already, return an error
  `goasm: Assemble already called; call Reset before re-assembling`.
- **Option B**: make `Assemble` idempotent / re-usable by replacing the
  LSym with a fresh one before each call (effectively the same as the
  current `Reset()`, just inlined into `Assemble`).

I recommend Option A — it surfaces caller misuse loudly, and Reset is
already the documented re-use pattern.

Update the package doc comment at top of `api.go` to spell out the
lifecycle: `New() -> Append* -> Assemble() -> (Reset() -> Append* -> Assemble())*`.

**Verification**: add `TestCtx_DoubleAssembleErrs` — call `Assemble()`
twice without Reset, assert the second returns a non-nil error matching
`goasm: Assemble already called`.

---

## Issue 5 — `Append()` does NOT validate that the first prog is ATEXT; later `Preprocess` then misinterprets random fields and may nil-deref (P1)

**Where**: `api.go:105-112`. The doc comment says "The first prog appended
must be the ATEXT prog (from NewATEXT)" but nothing enforces it. If a user
forgets and appends an instruction first:

- `c.firstProg` becomes the non-ATEXT prog.
- `Assemble` sets `c.sym.Func().Text = c.firstProg`.
- `obj/x86/obj6.go:622-623` reads `p := cursym.Func().Text; autoffset := int32(p.To.Offset)`
  — treats the wrong prog's `To.Offset` as a frame size.
- `obj/x86/obj6.go:638` reads `p.From.Sym.NoFrame()` — for a non-ATEXT
  prog `p.From.Sym` is typically nil → **nil deref panic**.

**Fix**: add at the top of `Assemble()`:

```go
if c.firstProg.As != obj.ATEXT {
    return nil, fmt.Errorf("goasm: first Prog must be ATEXT (use Ctx.NewATEXT); got %v", c.firstProg.As)
}
```

(Also, optionally, validate in `Append`: if `c.last == nil && p.As != obj.ATEXT`,
return an error/panic. But returning errors from Append is awkward — better
to enforce at `Assemble` time.)

**Verification**: add `TestErr_FirstProgNotATEXT` that appends a MOVQ
without ATEXT and asserts the returned error mentions ATEXT.

---

## Issue 6 — `regs.go` is missing real registers from several backends (P1)

**Where**: `regs.go`. Audit against `obj/<arch>/a.out.go`:

| Backend | Missing in regs.go (incomplete list)                                        |
|---------|-----------------------------------------------------------------------------|
| WASM    | `REG_F16`..`REG_F31` (we have only F0..F15); also `REG_PC_B`                |
| MIPS    | `REG_HI`, `REG_LO`, `REG_M0`..`REG_M31` (CP0 control regs), `REGZERO` alias |
| ARM     | `REG_FPSR`, `REG_FPCR`, `REG_CPSR`, `REG_SPSR`                              |
| PPC64   | vector regs `REG_V0`..`REG_V63`, `REG_VS0`..`REG_VS63`, CTR/LR aliases      |
| LoongArch64 | vector regs `REG_V0`..`REG_V31`, `REG_X0`..`REG_X31` (LSX/LASX 256-bit) |
| RISC-V (Go's) | `REG_FCSR`, named ABI aliases (`REG_RA`, `REG_SP`, `REG_GP`, …)        |

The WASM `F16-F31` omission is a real gap — users targeting WASM cannot
emit f64 ops without it.

**Fix**: extend `regs.go` to expose the complete set per backend. Group
new constants in their existing arch sections following the existing
`REG_<ARCH>_<NAME>` convention. Also add the `_PC_B` for WASM.

To keep the file size tractable, consider adding a one-line comment at
the start of each section linking to the source file:
`// See riscv/goasm/obj/wasm/a.out.go for the canonical list.`

**Verification**: add a compile-only `regs_test.go` that takes the address
of every constant (forces them to compile-resolve); and add a doc-comment
test that lists what is intentionally NOT exposed (e.g., RISC-V CSR
encoding base, x86 segment selectors).

---

## Issue 7 — `goasm.New()` mutates package globals; concurrent first-time calls race (P1)

**Where**: `obj/x86/asm6.go:2259` (`instinit`) and `obj/arm64/asm7.go`
(`buildop`) write to package-level globals (`ycover`, `opindex`, `optab`,
`oprange`, `xcmp`, etc.). Each guards with a fast-path early-return if
already initialized, but neither holds a lock. Two goroutines both calling
`goasm.New(goasm.AMD64)` for the first time may both see the early-return
condition as false and execute the init body in parallel, racing on those
arrays. The `-race` detector currently passes only because tests run
sequentially.

This is a pre-existing property of Go's obj package (the compiler always
calls Init before any goroutines fan out), but our API exposes
`goasm.New()` to library users who may not know.

**Fix**: serialize first-use of each architecture inside `newLinkCtx`:

```go
var (
    initOnce sync.Map // map[Arch]*sync.Once
)

func newLinkCtx(arch Arch) *obj.Link {
    var la *obj.LinkArch
    switch arch {
    case AMD64: la = &x86.Linkamd64
    case ARM64: la = &arm64.Linkarm64
    default:    panic(...)
    }
    ctxt := obj.Linknew(la)
    ...
    if la.Init != nil {
        once, _ := initOnce.LoadOrStore(arch, new(sync.Once))
        once.(*sync.Once).Do(func() { la.Init(ctxt) })
    }
    return ctxt
}
```

This prevents the race without making single-threaded use any slower.

**Verification**: add `TestConcurrentNew` that spawns N goroutines each
calling `goasm.New(goasm.AMD64)` and `goasm.New(goasm.ARM64)`, then
performing one tiny Assemble; run under `-race`. With the current code,
this will report a data race; with the fix, it must pass.

---

## Issue 8 — `extract-goasm.sh` copies `obj/arm64/asm_arm64_test.go` but not its companion `.s`; arm64 host builds the test binary fail (P1)

**Where**: `scripts/extract-goasm.sh:64-67`:

```bash
copy_pkg "$SRC/obj/arm64" "obj/arm64" \
    asm_test.go
```

`asm_arm64_test.go` declares Go functions with no body (forward-decls for
`testvmovs`, `testmovk`, `testCombined`) backed by `asm_arm64_test.s` in
the original tree. The script copies only `*.go`, so the `.s` is left
behind. On `GOARCH=arm64` hosts, `go test ./goasm/obj/arm64/` fails:

```
goasm/obj/arm64/asm_arm64_test.go:9:6: missing function body
goasm/obj/arm64/asm_arm64_test.go:10:6: missing function body
... (and 3 more)
```

Reproduced via `GOOS=linux GOARCH=arm64 go test -c -o /dev/null
./goasm/obj/arm64/`.

**Fix**: add `asm_arm64_test.go` to the skip list in extract-goasm.sh:

```bash
copy_pkg "$SRC/obj/arm64" "obj/arm64" \
    asm_test.go \
    asm_arm64_test.go
```

Then delete `goasm/obj/arm64/asm_arm64_test.go` from the working tree and
record the deletion in EDITS.md (or note it in the script comment).

While auditing, also verify other arch test files for the same pattern.
None of `obj/{arm,loong64,mips,ppc64,riscv,s390x,wasm}/` currently has a
GOARCH-tagged `*_test.go` we forgot to skip — but this should be checked
with:

```bash
find $GOROOT/src/cmd/internal/obj -name '*_test.s'
```

If anything turns up, add to skip list.

**Verification**: re-run `GOOS=linux GOARCH=arm64 go test -c ./goasm/...`;
must compile. Add a CI matrix row for `GOARCH=arm64` if/when CI exists.

---

## Issue 9 — Smoke test mmap is `PROT_READ|PROT_WRITE|PROT_EXEC`; will SIGKILL on darwin/arm64 without `MAP_JIT` (P1)

**Where**: `api_test.go:521-525`. On Apple Silicon (darwin/arm64), the
kernel hardens W^X: an anonymous mapping cannot have `PROT_WRITE | PROT_EXEC`
simultaneously unless it also has `MAP_JIT` flag, AND the process must
toggle `pthread_jit_write_protect_np()` around writes. The current test
silently never runs there (we are on darwin/amd64), but as soon as anyone
runs the suite on an M-series Mac it will fail.

**Fix**: make the smoke test arch-+-OS aware. On darwin/arm64, either
(a) skip with a clear `t.Skip("requires MAP_JIT on darwin/arm64; see internal/jitcall")`,
or (b) use the proper darwin-arm64 sequence: mmap with
`syscall.MAP_JIT|syscall.MAP_ANON|syscall.MAP_PRIVATE`,
toggle write protection via cgo or `pthread_jit_write_protect_np`, and
flush the icache. Option (a) is much simpler and matches the package's
"no cgo" design constraint; defer real darwin-arm64 execution to a later
phase that can use `internal/jitcall`.

Implementation sketch for (a):

```go
if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
    t.Skip("smoke execute test: darwin/arm64 requires MAP_JIT; tracked separately")
}
```

…before the mmap.

**Verification**: cross-builds for darwin/arm64 will now skip cleanly.

---

## Issue 10 — `assertBytes` silently trims ALL trailing zeros; can mask real encoding bugs (P2)

**Where**: `api_test.go:95-103`.

```go
trimmed := bytes.TrimRight(got, "\x00")
if !bytes.Equal(trimmed, want) { ... }
```

The intent is to absorb arm64 alignment-padding zeros at the symbol's
tail. But this also accepts any spurious trailing zero bytes the encoder
might mistakenly emit for AMD64 sequences.

A better invariant: `got[:len(want)] == want` AND
`got[len(want):]` is all zeros AND `len(got) - len(want)` is < `arch_alignment`.

**Fix**: rewrite `assertBytes`:

```go
func assertBytes(t *testing.T, got, want []byte) {
    t.Helper()
    if len(got) < len(want) {
        t.Errorf("output shorter than expected: got %d bytes %s, want %d bytes %s",
            len(got), hexFmt(got), len(want), hexFmt(want))
        return
    }
    if !bytes.Equal(got[:len(want)], want) {
        t.Errorf("bytes mismatch:\n  got  %s\n  want %s", hexFmt(got[:len(want)]), hexFmt(want))
        return
    }
    // Trailing bytes (alignment padding) must be zeros.
    for i, b := range got[len(want):] {
        if b != 0 {
            t.Errorf("non-zero trailing byte at offset %d: 0x%02X (full output: %s)",
                len(want)+i, b, hexFmt(got))
            return
        }
    }
}
```

**Verification**: existing tests still pass; add a regression case where
`got = want + [0x01]` and assert the helper FAILS the test (negative test
written via `testing.T` substitute or `t.Run` wrapper).

---

## Issue 11 — Test coverage gaps vs. promised "25+ golden tests" (P2)

**Where**: `api_test.go` — only 13 byte-level golden tests for AMD64 and
3 for ARM64; the original plan promised 25+.

Concrete gaps that should be filled, in order of integration importance
for the planned RISC-V JIT backend:

1. **Backward branch** (loop body): emit `target: ...; cmp; jne target`.
2. **Long immediate** (`MOVQ $0x123456789ABCDEF, AX`): 10-byte MOVABS.
3. **Multi-byte displacement** (`MOVQ 0x10000(BX), AX`): tests disp32.
4. **Memory store from register** (we have store-imm and load, but not
   `MOVQ AX, 8(BX)`).
5. **Shift by CL** (`SHLQ CX, AX`).
6. **MUL/DIV** (we have IMULQ but not `MULQ`/`IDIVQ`/`DIVQ`).
7. **Conditional jumps** (`JEQ`, `JNE`, `JLT`, `JGT`, `JCC`): even one is
   sufficient to lock down condition encoding.
8. **MOVL zero-extension** (32→64).
9. **MOVBQSX / MOVBQZX** (sign/zero extend byte).
10. **SSE float ops** (`MOVSD`, `ADDSD`, `MULSD`, `SQRTSD`): the planned
    JIT must emit these.
11. ARM64: `BL`, `B.EQ`, `LDR`/`STR`, `FADDD`/`FMULD`/`FSQRTD`,
    `CBZ`/`CBNZ`.
12. **Error paths**: the 4 tests below (Issues 1, 4, 5).

For each, derive bytes via:

```bash
GOARCH=amd64 GOOS=linux /usr/local/go/bin/go tool asm -o /tmp/t.o /tmp/t.s
GOARCH=amd64 GOOS=linux /usr/local/go/bin/go tool objdump /tmp/t.o
```

…where `/tmp/t.s` contains:

```
TEXT ·f(SB),NOSPLIT|NOFRAME,$0-0
    <instruction>
    RET
```

**Fix**: extend `api_test.go` with the above. Group by category with
section comments matching the existing `─── …` style. Aim for ≥25 tests.

**Verification**: `go test ./goasm/ -v` lists ≥25 tests; coverage of
`obj/x86/asm6.go` rises (run `go test -coverprofile`).

---

## Issue 12 — `Ctx.NewProg` redundantly assigns `p.Ctxt` already set by `obj.Link.NewProg` (P2)

**Where**: `api.go:75-79` and `obj/util.go:215-219`. `obj.Link.NewProg`
already does `p.Ctxt = ctxt`. The wrapper in api.go does it again. Not a
bug, just dead code.

**Fix**:

```go
func (c *Ctx) NewProg() *obj.Prog { return c.ctxt.NewProg() }
```

…and drop the inline assignment.

**Verification**: tests still pass; `git diff` shows only the dead line
removed.

---

## Issue 13 — `DRIVE.md` claims `obj.AssembleBlock` runs ErrorCheck before mkfwd; original `Flushplist` runs them in the OPPOSITE order — minor doc drift (P2)

**Where**: `DRIVE.md:13-22` describes the order as `ErrorCheck → mkfwd →
linkpatch → Preprocess → Assemble`. The actual `obj/plist.go:160-175`
`Flushplist` order is `mkfwd → ErrorCheck → linkpatch → Preprocess →
Assemble`. Our `AssembleBlock` (`obj/plist.go:183-191`) follows our DRIVE
order (ErrorCheck first), but the doc does not call out the divergence.

For x86 it does not matter (errorCheck is a no-op when Flag_dynlink is
false, which is our default). But the DRIVE.md text reads as "this is the
canonical order" when it isn't.

**Fix**: choose one — either reorder `AssembleBlock` to match
`Flushplist` (`mkfwd → ErrorCheck → linkpatch → Preprocess → Assemble`)
for fidelity, OR keep the current order and amend DRIVE.md to say
"differs from Flushplist (which runs mkfwd first); ErrorCheck is a
no-op for our flag set, so order doesn't matter in practice." Recommend
the former for least-surprise: it makes `git diff` against the upstream
more obvious in future Go upgrades.

Also update DRIVE.md "Headtype" section (lines 200-214) to mention that
`obj.Linknew` already sets Headtype from `buildcfg.GOOS` — our explicit
override is for the case where `GOOS` env var differs from `runtime.GOOS`
(e.g., cross-tooling).

Also update DRIVE.md "InitTextSym signature (confirmed, Go 1.22)" header
(line 173) to "Go 1.26" — this is the version we actually extracted from,
per `EXTRACTED.txt`.

Also update DRIVE.md `goasm.Ctx API summary` (lines 218-229) — it
claims `goasm.New(goasm.RISCV)` is supported; our `New` only handles
AMD64 and ARM64, and `goasm.LinkArch(arch)` does not exist. Either delete
the lying lines or add the helper.

**Verification**: re-read DRIVE.md after edits; manually reconcile each
claim against the code.

---

## Issue 14 — `EDITS.md` references files / changes that no longer apply, and is missing actual edits made (P2)

**Where**: `EDITS.md`.

a. References `**Remove `isNonPkgSym` function** if it has no remaining
callers` (line 106) — the current `obj/sym.go:226-248` keeps `isNonPkgSym`
with a "kept for traversal logic only" comment. The function is in fact
unreferenced (grep confirms). It should be deleted, or the comment in
sym.go is misleading.

b. Missing entries — the actual extracted tree has changes not listed in
EDITS.md, e.g., the `obj/sym.go` `usedFiles` change to `[]uint32` IS
present (line 323) but EDITS.md only mentions it under the existing
"Fix usedFiles type" bullet; the bullet should also call out the deletion
of any `goobj.CUFileIndex` lookup that might have appeared elsewhere.

c. EDITS.md never mentions that we kept `obj/x86/seh.go`. Originally the
plan suggested a `//go:build windows` guard. Today the file compiles fine
(`go build ./goasm/...` is clean). EDITS.md should record this no-op as a
deliberate decision.

d. EDITS.md does NOT capture the obvious manual edit `AssembleBlock`
appended to `obj/plist.go` — wait, it does at lines 60-78. But line 80
says "Remove `linkpcln` and `populateDWARF` calls" and the actual
plist.go's `Flushplist` (lines 160-175) does NOT call linkpcln or
populateDWARF — they were already absent. Confirm they were removed and
delete the bullet if redundant.

**Fix**: do a clean pass on EDITS.md:
1. Delete bullets that no longer match the source.
2. Audit each edit by `diff` against `$GOROOT/src/cmd/internal/obj/...`
   and re-list every concrete change.
3. Add a top-of-file note: "Auto-generated from `git diff
   goasm/obj/{plist,link,sym,data,line}.go` against vanilla Go 1.26.2
   extraction."

Note: Issue 1 does NOT require any edits to the extracted obj/* files —
the recoverable-panic strategy hooks `DiagFlush` from outside, and
leaves all `log.Fatalf` lines untouched as reference. So EDITS.md does
not gain any new entries from Issue 1.

**Verification**: each bullet in EDITS.md must correspond to a line that
exists (or is missing) in the extracted file vs. upstream.

---

## Issue 15 — `Ctx.Reset` re-runs `newLinkCtx` (and therefore `LinkArch.Init`); wasteful and reinforces Issue 7 (P2)

**Where**: `api.go:69-72`. Each `Reset` allocates a fresh `Link`, fresh
LSym hash table, and re-invokes `LinkArch.Init`. The arch init body
hits its early-return after the first call, but each Reset still pays the
allocation cost (modest, but non-zero) and ALSO triggers the race window
in Issue 7 if Reset runs concurrently with another `New`/`Reset`.

**Fix**: rather than allocating a new Link, recycle the existing one and
just clear the per-symbol state:

```go
func (c *Ctx) Reset() {
    c.errors = nil
    c.firstProg = nil
    c.last = nil
    // Drop the previous LSym from the hash table by allocating a fresh
    // unique name; this avoids the "redeclared" diag and keeps the
    // LinkArch.Init initialization shared across Reset calls.
    name := fmt.Sprintf("jit_block_%d", atomic.AddUint64(&c.gen, 1))
    c.sym = c.ctxt.LookupInit(name, func(s *obj.LSym) {
        s.Type = objabi.STEXT
    })
}
```

This requires adding `gen uint64` to the Ctx struct and importing
`sync/atomic`. The hash table will accumulate stale entries over time;
if that becomes a memory concern, also call `delete(c.ctxt.hash, oldName)`.

Trade-off: faster Reset, but the same Link is reused across blocks. That
means accumulated `ctxt.Text` entries grow forever; for short-lived
processes this is fine, but for long-running JIT this is a leak that
needs addressing in Phase 2 (perhaps a `Compact()` API).

If preserving the current "fresh ctxt every Reset" semantics is preferred,
at minimum guard `LinkArch.Init` with the once map from Issue 7. Either
way, Issue 7's fix is a prerequisite.

**Verification**: `TestCtx_ResetMany` that calls Reset+Assemble 1000
times; before fix, RSS grows linearly; after fix, growth is bounded.

---

## Issue 16 — `Ctx.NewATEXT` hard-codes `c.sym`; user cannot assemble multiple text symbols from one Ctx (P2)

**Where**: `api.go:84-94`. The ATEXT prog references `c.sym`. Given that
each Ctx only has one LSym (`jit_block`), this is internally consistent.
But it limits the API: there's no way to emit, say, a region of code with
two named entry points within one assembly pass.

For Phase 1 this is fine — JIT blocks are atomic. Document the limitation
in the package doc comment.

**Fix**: add a sentence to the `package goasm` doc:

```
// Each Ctx assembles exactly one text symbol named "jit_block". To
// produce multiple independent symbols, use multiple Ctx instances.
```

**Verification**: doc-only.

---

## Issue 17 — `hexFmt` builds a byte slice and stringifies; `fmt.Sprintf("% X", b)` does the same in 1 line (P2)

**Where**: `api_test.go:81-93`.

```go
func hexFmt(b []byte) string {
    if len(b) == 0 { return "<empty>" }
    var s []byte
    for i, v := range b {
        if i > 0 { s = append(s, ' ') }
        s = append(s, []byte(fmt.Sprintf("%02X", v))...)
    }
    return string(s)
}
```

This is equivalent to `fmt.Sprintf("% X", b)` (note the space-flag) plus
the empty-string special case.

**Fix**:

```go
func hexFmt(b []byte) string {
    if len(b) == 0 { return "<empty>" }
    return fmt.Sprintf("% X", b)
}
```

**Verification**: existing `assertBytes` failure messages render
identically; no test changes needed.

---

## Issue 18 — `callAt`'s funcval trick should use `*uintptr` directly, removing the address-of-a-stack-variable hop (P2)

**Where**: `api_test.go:559-565`.

```go
func callAt(addr uintptr) int64 {
    type fnType func() int64
    funcval := [1]uintptr{addr}
    fnptr := unsafe.Pointer(&funcval[0])     // fnptr = &funcval[0] = funcval pointer
    fn := *(*fnType)(unsafe.Pointer(&fnptr)) // fn = fnptr (as func value)
    return fn()
}
```

The double indirection is correct but obscures intent. A cleaner pattern
is:

```go
func callAt(addr uintptr) int64 {
    type fnType func() int64
    fp := &addr  // *uintptr — same shape as a Go function value
    fn := *(*fnType)(unsafe.Pointer(&fp))
    return fn()
}
```

Or, equivalently (idiomatic in Go's runtime tests):

```go
func callAt(addr uintptr) int64 {
    type fnType func() int64
    var fn fnType
    *(**uintptr)(unsafe.Pointer(&fn)) = &addr
    return fn()
}
```

Either is conceptually identical to the current code. The second form is
how `runtime.funcval` is constructed in Go's own tests and is the most
audit-friendly.

**Fix**: replace with the simpler form and update the doc comment to
mirror it.

**Verification**: smoke test still returns 42.

---

## Issue 19 — `goasm/obj/sym.go` keeps dead `isNonPkgSym` with a "kept for traversal" comment that is wrong (P2)

**Where**: `obj/sym.go:226-248`.

```go
// isNonPkgSym — kept for traversal logic only (not called from removed NumberSyms).
func isNonPkgSym(ctxt *Link, s *LSym) bool { ... }
```

`grep -r isNonPkgSym goasm/` returns only its own definition + the
comment in EDITS.md. The traversal functions in this file
(`traverseSyms`, `traverseFuncAux`, `traverseAuxSyms`) do not call it.
The comment is wrong, the function is dead code, and `go vet` doesn't
flag unused private functions but a future linter might.

**Fix**: delete `isNonPkgSym` entirely. Remove the corresponding bullet
from EDITS.md (or replace it with a "removed isNonPkgSym (was dead)"
entry).

**Verification**: `go build ./goasm/...` still clean; `grep -r
isNonPkgSym goasm/` returns nothing.

---

## Issue 20 — `EXTRACTED.txt` carries a stale Go-version string format ("go version go1.26.2 darwin/amd64"); machine-parseable form would help downstream tooling (P2)

**Where**: `EXTRACTED.txt`. The file has:

```
Extracted goasm from Go go version go1.26.2 darwin/amd64 on Thu Apr 16 16:23:34 -03 2026
```

The "go version go1.26.2 darwin/amd64" is the verbatim `go version`
output, which doubles "go" and embeds the host triple. For an artifact
file consumed by future re-extraction tooling, structured fields are
better.

**Fix**: rewrite the script's final line to emit something like:

```bash
{
    echo "go_version: $(go env GOVERSION)"
    echo "go_root: $(go env GOROOT)"
    echo "host_goos: $(go env GOOS)"
    echo "host_goarch: $(go env GOARCH)"
    echo "extracted_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$DEST/EXTRACTED.txt"
```

…and run the script once to regenerate. (Trivial; defer to the next
extraction cycle.)

**Verification**: `cat goasm/EXTRACTED.txt` — humans and `awk` both parse.

---

## Issue 21 — Comment fragment "// immProg builds a Prog…" mismatches the function name `immReg` it documents (P2)

**Where**: `api_test.go:33`.

```go
// immProg builds a Prog with a register destination and an immediate source.
func immReg(c *goasm.Ctx, as obj.As, imm int64, dstreg int16) *obj.Prog {
```

**Fix**: change `immProg` → `immReg` in the comment.

**Verification**: doc-only.

---

## Issue 22 — Missing tests prove the round-trip "Reset preserves global init" property (P2)

**Where**: `api_test.go`.

After Issue 7 / Issue 15 fixes, `Reset` will share the once-initialized
arch tables across Ctx lifetimes. We should lock that property in:

```go
func TestArchTablesInitOnce(t *testing.T) {
    // First Ctx triggers instinit/buildop.
    c1 := goasm.New(goasm.AMD64)
    c1.Append(c1.NewATEXT())
    c1.Append(immReg(c1, x86.AMOVQ, 1, x86.REG_AX))
    c1.Append(c1.NewRET())
    if _, err := c1.Assemble(); err != nil {
        t.Fatal(err)
    }
    // Second Ctx must not re-init (proven by absence of any
    // "phase error in optab" or similar diag) and must produce
    // identical bytes.
    c2 := goasm.New(goasm.AMD64)
    c2.Append(c2.NewATEXT())
    c2.Append(immReg(c2, x86.AMOVQ, 1, x86.REG_AX))
    c2.Append(c2.NewRET())
    b2, err := c2.Assemble()
    if err != nil { t.Fatal(err) }
    // …and same for ARM64 in parallel goroutines (after Issue 7 fix).
}
```

**Fix**: add the test as part of the Issue 7/15 patch.

---

## Issue 23 — package goasm comment refers only to amd64+arm64 but `regs.go` exposes 9 backends; doc inconsistency (P2)

**Where**: `api.go:1-15` says "Go's extracted obj instruction encoders
for amd64 and arm64". `regs.go` exports 9 backends. Either the API is
amd64+arm64 only (in which case why expose other regs?), or it supports
more (in which case the doc is wrong).

**Fix**: extend the package doc:

```
// Package goasm provides a thin API over Go's extracted obj instruction
// encoders. The Ctx (Assemble) path supports amd64 and arm64 host
// architectures; register-name re-exports cover all 9 backends shipped
// by Go (amd64, arm64, arm, loong64, mips, ppc64, riscv, s390x, wasm)
// so callers can construct cross-arch Progs by hand if they extend the
// New() switch in api.go.
```

…and add a TODO note: extending `New()` to other host arches is
mechanical (add a case in `newLinkCtx`), but each one needs its own
golden tests + smoke-execute pathway.

**Verification**: doc-only.

---

## Suggested execution order

The fixes are mostly independent. A recommended order that minimizes
re-running tests:

1. **Issue 1, 2, 3, 4, 5** — all in `api.go`; one PR; add the matching
   tests at the same time. These are P0/P1 and unblock real JIT use.
2. **Issue 7** — concurrency safety (also `api.go`); add `TestConcurrentNew`.
3. **Issue 6** — extend `regs.go`; add `regs_test.go`.
4. **Issue 8** — fix `extract-goasm.sh` and delete the orphan
   `asm_arm64_test.go`.
5. **Issue 9, 10, 11, 12, 17, 18, 21** — `api_test.go` cleanup pass + new
   golden tests.
6. **Issue 13, 14, 16, 20, 23** — documentation pass.
7. **Issue 15** — `Reset()` recycle; defer until Phase 2 starts emitting
   thousands of blocks (the leak is a real concern then).
8. **Issue 19** — drop dead `isNonPkgSym`.

After all fixes: `go test ./goasm/ -race`, `go test ./goasm/... -count=2`,
`go vet ./goasm/...`, and a manual `go test -c` for `GOOS=linux
GOARCH=arm64` and `GOOS=darwin GOARCH=arm64` to confirm the cross-build
matrix.

---

## Verification checklist (end-to-end)

After applying all fixes:

```bash
cd /Users/jaten/ris

# Compiles cleanly on host and one cross arch.
go build ./goasm/...
GOOS=linux GOARCH=arm64 go test -c -o /dev/null ./goasm/...

# Tests pass under race detector and twice in a row.
go test ./goasm/ -race -count=2 -v

# Vet stays clean (the abi/escape.go warning is in extracted code,
# unrelated; should remain the only output).
go vet ./goasm/... 2>&1 | grep -v 'goasm/abi/escape.go:.*possible misuse of unsafe.Pointer'

# Smoke executes the assembled bytes.
go test ./goasm/ -run TestSmoke -v

# Concurrent-init test passes.
go test ./goasm/ -run TestConcurrentNew -race -v

# Negative tests for malformed Progs return errors (not log.Fatalf).
go test ./goasm/ -run 'TestErr_' -v
```

All commands above should succeed, except possibly the smoke test on
`darwin/arm64` which now skips with a clear message.
