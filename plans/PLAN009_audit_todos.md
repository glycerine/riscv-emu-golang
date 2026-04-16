# Plan: Disposition of Deferred goasm/ Audit Items

## Context

The previous plan (a 23-issue audit of `/Users/jaten/ris/goasm/`) has been
applied. Five items were marked "deferred" at the end of that pass:

- Issue 11 — more golden tests for instruction encoding
- Issue 15 — `Ctx.Reset` recycling (avoid full Link re-allocation)
- Issue 20 — `EXTRACTED.txt` machine-parseable format
- Issue 22 — extra test that locks in the "Reset / second New
  preserves once-only LinkArch.Init" property
- Issue 16 — multi-symbol API (multiple text symbols per Ctx)

This plan re-evaluates each, decides what to do now vs. leave deferred,
and gives an executable patch for the items selected for action.

Repo: `/Users/jaten/ris/`
Go: 1.26.2 darwin/amd64; module name `riscv`

---

## Per-issue disposition

### Issue 11 — More golden tests — DO NOW

**What it is**: extend `goasm/api_test.go` with byte-level golden tests
for instruction shapes that are missing today. The audit listed 12
specific gaps (backward branch, `MOVQ $0x123456789ABCDEF, AX`,
`disp32`, mem-store-from-reg, `SHLQ CX, AX`, `MULQ`/`IDIVQ`, conditional
jumps, `MOVL` zero-ext, `MOVBQSX`/`MOVBQZX`, SSE float ops, plus ARM64
`BL`/`B.EQ`/`LDR`/`STR`/`FADDD`/`CBZ`).

**Why it was deferred**: bulk + tedium. Each test requires a manual
round-trip through `go tool asm` + `go tool objdump` to capture the
expected bytes.

**Re-evaluation**: Phase 2 (the real RISC-V→host JIT) will soon need
exactly these instruction shapes. Catching encoder regressions there is
much harder than catching them at the goasm boundary. It's mechanical
work with zero risk of breaking anything else, so doing it now is a net
win.

**Recommendation**: **DO NOW.** Add ~12 new tests targeting the gaps
the JIT will exercise first.

---

### Issue 15 — `Ctx.Reset` recycling — DEFER (re-confirmed)

**What it is**: today `Reset()` allocates a brand-new `obj.Link` (via
`newLinkCtx`). The proposed fix kept the same Link and just swapped in
a fresh LSym (`jit_block_<n>`).

**Why it was deferred**: not a measured bottleneck.

**Re-evaluation**: the proposed recycle has a real downside — the
`ctxt.hash` and `ctxt.Text` slices grow without bound (every Reset
adds a `jit_block_<n>` LSym that no one ever frees). For a long-running
emulator this is a memory leak, not a win. The cleanest fix would
require a `Compact()` API that prunes both, which is genuinely Phase 2
scope. The per-Reset Link allocation is also small (a few maps + a
single `LinkArch.Init` early-return).

**Recommendation**: **KEEP DEFERRED.** Touch this only when profiling
shows it matters; Phase 2 is the right time to design `Compact()`.

---

### Issue 20 — `EXTRACTED.txt` machine-parseable format — DO NOW

**What it is**: `EXTRACTED.txt` is a single human-prose line:

```
Extracted goasm from Go go version go1.26.2 darwin/amd64 on Thu Apr 16 16:23:34 -03 2026
```

The proposed format is structured `key: value` lines (`go_version`,
`go_root`, `host_goos`, `host_goarch`, `extracted_at`).

**Why it was deferred**: "trivial; defer to next extraction cycle."

**Re-evaluation**: there is no near-term re-extraction event scheduled,
so "next extraction cycle" could be far off. Two-step fix that takes
~2 minutes and unblocks any future tooling that wants to consult the
file. Risk: zero — the file is metadata, no code reads it.

**Recommendation**: **DO NOW.** Edit the script (already partially
touched in the previous session) so future runs emit the new format,
and rewrite the in-tree `EXTRACTED.txt` by hand to match.

---

### Issue 22 — Lock-in test for once-only LinkArch.Init — DO NOW

**What it is**: a small unit test that creates two `Ctx` instances
back-to-back (and a Reset cycle), asserting that:

1. The encoder produces no `phase error in optab` / `phase error in
   avxOptab` diags on the second `New()`.
2. Both Ctx instances produce identical bytes for the same trivial
   prog list.
3. (Implicit) `LinkArch.Init`'s early-return path is exercised.

**Why it was deferred**: marked as a follow-up to Issue 7's main
patch.

**Re-evaluation**: `TestConcurrentNew` already exercises the Once
indirectly by running 16 New()+Assemble() calls under `-race`. A
serial test adds little new coverage but is cheap and gives a clearer
diagnostic when the property breaks (e.g., if a future Go upgrade
removes the `if ycover[0] != 0 { return }` early return). Cost: ~20
lines of test.

**Recommendation**: **DO NOW.** Belt-and-suspenders for Issue 7's
fix.

---

### Issue 16 — Multi-symbol API — ALREADY DONE (doc) / DEFER (real refactor)

**What it is**: today each Ctx owns exactly one LSym (`jit_block`). A
real multi-symbol API would let one Ctx emit multiple named entry
points in a single assembly pass.

**Original disposition**: doc-only fix to call out the limitation in
the package comment.

**Status**: the doc-only fix has *already* landed during the previous
implementation pass — `api.go:12-13` reads:

> `// Each Ctx assembles exactly one text symbol named "jit_block". To
> // produce multiple independent symbols, use multiple Ctx instances.`

The full multi-symbol refactor (parallel `firstProg`/`last` per LSym,
`Ctx.SetSym(name)` switcher, etc.) is genuine Phase 2+ scope and not
needed by anything we plan to build first.

**Recommendation**: **DEFER (real refactor).** Doc note already in
place; nothing to do.

---

## Summary of dispositions

| Issue | Verdict | Rationale (one line) |
|-------|---------|----------------------|
| 11 — more golden tests | **DO NOW** | Mechanical, additive, unblocks JIT integration confidence. |
| 15 — Reset recycle | **DEFER** | Premature optimization; proposed fix introduces a leak. |
| 20 — EXTRACTED.txt format | **DO NOW** | 2-minute fix, eliminates parsing irregularity. |
| 22 — once-only Init test | **DO NOW** | Belt-and-suspenders for Issue 7; ~20 LOC. |
| 16 — multi-symbol API | **DONE (doc) / DEFER (refactor)** | Doc note landed; refactor is Phase 2+. |

Net new work: Issues 11, 20, 22.

---

## Execution plan for the items selected

### A. Issue 11 — Golden test additions

**File to modify**: `/Users/jaten/ris/goasm/api_test.go`

For each row below:
1. Write a `.s` file in `/tmp/t.s` using the `TEXT ·f(SB),NOSPLIT|NOFRAME,$0-0`
   wrapper.
2. `GOARCH=amd64 GOOS=linux /usr/local/go/bin/go tool asm -o /tmp/t.o /tmp/t.s`
3. `GOARCH=amd64 GOOS=linux /usr/local/go/bin/go tool objdump /tmp/t.o`
   to read out the bytes.
4. Add a Go test that emits the same Prog and asserts those bytes via
   `assertBytes`.

Same `GOARCH=arm64` recipe for ARM64 cases.

**AMD64 tests to add (10):**

| Name | Prog list | Why it matters for JIT |
|------|-----------|------------------------|
| `TestAMD64_JMP_backward` | label: NOP; CMPQ AX,BX; JNE label | RV loop body |
| `TestAMD64_MOVABS_const` | MOVQ $0x123456789ABCDEF, AX (10-byte) | RV LUI+ADDI immediates that don't fit sign-ext-32 |
| `TestAMD64_MOVQ_load_disp32` | MOVQ 0x10000(BX), AX | RV LD with large offsets |
| `TestAMD64_MOVQ_store_reg` | MOVQ AX, 8(BX) | RV SD store path |
| `TestAMD64_SHLQ_CL` | SHLQ CX, AX | RV SLL/SLLW |
| `TestAMD64_MULQ` | MOVQ $7,AX; MOVQ $6,BX; MULQ BX | RV MUL/MULH |
| `TestAMD64_IDIVQ` | MOVQ $0,DX; MOVQ $42,AX; MOVQ $7,BX; IDIVQ BX | RV DIV/REM |
| `TestAMD64_JEQ_forward` | CMPQ AX,BX; JEQ label; ...; label: RET | RV BEQ/BNE |
| `TestAMD64_MOVL_zeroext` | MOVL AX, AX (zero-extends to RAX) | RV ADDIW / SLLIW behaviour |
| `TestAMD64_MOVBQSX` | MOVBQSX AL, AX | RV LB sign-extend |

**SSE/floating-point tests to add (4):**

| Name | Prog list |
|------|-----------|
| `TestAMD64_MOVSD_load` | MOVSD 0(BX), X0 |
| `TestAMD64_ADDSD` | ADDSD X1, X0 |
| `TestAMD64_MULSD` | MULSD X1, X0 |
| `TestAMD64_SQRTSD` | SQRTSD X0, X0 |

**ARM64 tests to add (5):**

| Name | Prog list | Why it matters |
|------|-----------|----------------|
| `TestARM64_LDR_post` | MOVD 8(R0), R1 | RV LD on arm64 host |
| `TestARM64_STR` | MOVD R1, 8(R0) | RV SD on arm64 host |
| `TestARM64_CBZ_forward` | CBZ R0, label; ...; label: RET | RV BEQ X,X0,label |
| `TestARM64_FADDD` | FADDD F1, F2, F0 (Fd = Fn+Fm in arm64) | RV FADD.D |
| `TestARM64_BL_self` | BL label; label: RET | RV JAL+RET round trip |

For tests that use `c.Ctxt().Lookup("foo")` to make the relocation
target real (e.g. BL self), assert via `len(c.Sym().R) != 0` only when
the test cares; for the encoding tests we just need the bytes.

After adding these, total tests rises from 27 → ~46. Group by the
existing `─── AMD64 byte tests ───` / `─── ARM64 byte tests ───` /
`─── SSE float tests ───` section markers (add a new SSE section).

**Verification**:
```bash
cd /Users/jaten/ris
go test ./goasm/ -v 2>&1 | grep -c '^=== RUN'    # ≥ 46
go test ./goasm/ -race -count=2                    # PASS
```

---

### B. Issue 20 — `EXTRACTED.txt` format

**File to modify (script)**: `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/scripts/extract-goasm.sh`

Replace the final `echo "Extracted goasm from Go ..."` block with:

```bash
{
    echo "go_version: $(go env GOVERSION)"
    echo "go_root:    $(go env GOROOT)"
    echo "host_goos:  $(go env GOOS)"
    echo "host_goarch:$(go env GOARCH)"
    echo "extracted_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$DEST/EXTRACTED.txt"
```

**File to modify (in-tree)**: `/Users/jaten/ris/goasm/EXTRACTED.txt`

Hand-edit to match the new format using the values that produced the
existing file (Go 1.26.2 / darwin / amd64 / 2026-04-16). Approximate
content:

```
go_version: go1.26.2
go_root:    /usr/local/go
host_goos:  darwin
host_goarch:amd64
extracted_at: 2026-04-16T19:23:34Z
```

(Exact `extracted_at` UTC value isn't reproducible from local time
zone; pick a value within an hour of the original timestamp — the field
is informational.)

**Verification**:
```bash
cat /Users/jaten/ris/goasm/EXTRACTED.txt    # five key:value lines
grep -E '^[a-z_]+:' /Users/jaten/ris/goasm/EXTRACTED.txt   # parses as key:value
```

---

### C. Issue 22 — `TestArchTablesInitOnce`

**File to modify**: `/Users/jaten/ris/goasm/api_test.go`

Add a single test that proves second-and-subsequent `New()` calls do
NOT re-trigger arch-table init (which would otherwise produce
`phase error in optab` diags via `ctxt.Diag`). Use `goasm.New` twice
plus a `Reset()`, capture the bytes from each, and assert all three are
identical and the error returns are nil.

Sketch:

```go
// TestArchTablesInitOnce — Issue 22: a second goasm.New for the same
// arch (and a Reset) must not re-run instinit/buildop in a way that
// yields "phase error" diags. We assert: no errors, and identical
// bytes from three independent encodings of the same prog list.
func TestArchTablesInitOnce(t *testing.T) {
    encode := func() []byte {
        c := goasm.New(goasm.AMD64)
        c.Append(c.NewATEXT())
        c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
        c.Append(c.NewRET())
        b, err := c.Assemble()
        if err != nil {
            t.Fatalf("Assemble: %v", err)
        }
        return b
    }

    a := encode()
    b := encode()

    c := goasm.New(goasm.AMD64)
    c.Append(c.NewATEXT())
    c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
    c.Append(c.NewRET())
    if _, err := c.Assemble(); err != nil {
        t.Fatalf("third Assemble: %v", err)
    }
    c.Reset()
    c.Append(c.NewATEXT())
    c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
    c.Append(c.NewRET())
    d, err := c.Assemble()
    if err != nil {
        t.Fatalf("post-reset Assemble: %v", err)
    }

    if !bytes.Equal(a, b) || !bytes.Equal(a, d) {
        t.Errorf("inconsistent bytes across encodings:\n  a=%X\n  b=%X\n  d=%X", a, b, d)
    }
}
```

Place it next to `TestConcurrentNew` in the "Concurrency / Issue 7"
section so the related properties live together.

**Verification**:
```bash
cd /Users/jaten/ris
go test ./goasm/ -v -run TestArchTablesInitOnce
go test ./goasm/ -race -count=2          # PASS
```

---

## Critical files modified

| File | Item(s) |
|------|---------|
| `/Users/jaten/ris/goasm/api_test.go` | Issue 11, Issue 22 |
| `/Users/jaten/ris/goasm/EXTRACTED.txt` | Issue 20 |
| `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/scripts/extract-goasm.sh` | Issue 20 |

No edits to `api.go`, `regs.go`, or any extracted `obj/*` source files.

---

## Verification (end-to-end)

After all three patches:

```bash
cd /Users/jaten/ris
go build ./goasm/...
go test ./goasm/ -v 2>&1 | grep '^=== RUN' | wc -l    # ≥ 46
go test ./goasm/ -race -count=2
GOOS=linux GOARCH=arm64 go test -c -o /dev/null ./goasm/...
cat /Users/jaten/ris/goasm/EXTRACTED.txt              # five key:value lines
```

All commands must succeed.

---

## Items remaining as deferred after this plan

- **Issue 15** (`Reset` recycling): wait for profile data demonstrating
  the per-Reset Link allocation is a bottleneck; design `Compact()`
  alongside.
- **Issue 16** (multi-symbol Ctx): wait until a concrete use case
  appears (none in Phase 2 plans).

These remain documented in the previous version of this plan (in git
history) for future reference.
