# Phase 5-B — Incremental Inline ECALL, with VizJit observability first

## Context

`~/ris/plan.md` ("Phase 5 — Inline ECALL to close the 33 ns libriscv
gap") is a single monolithic edit across 6 files. A prior attempt to
apply it got wedged: something broke, and there was no way to tell
which of the four correlated changes was responsible, so work stalled.

We want the same end state — ECALL inlined into the JIT block, closing
the ~33 ns gap to libriscv — but broken into **atomic, flag-gated,
individually-verifiable steps**, with a debug dump added *first* so we
can see what the JIT emits at every stage and correlate guest RISC-V →
IR → host x86 when something misbehaves.

Since the codebase now **pre-translates the entire binary at load
time** (AOT; `jit_aot.go:jitCompileAOTSegment`), very little
just-in-time compilation happens at runtime. That means dumping once
per AOT-compiled block (a few hundred files per run) is cheap and
sufficient — no need to dump per execution.

## What changed since `~/ris/plan.md` was written

The plan references code that is no longer current. Before executing,
we must adjust for:

1. **ELS regalloc is deleted** (commit `55918a8`). Only
   `ir/regalloc_fixed.go` (FixedStaticAllocator) remains. The plan's
   long "Our analog (allocator-agnostic)" section collapses to the
   fixed-static case only — every AllocReg VReg lives in a
   caller-saved host reg and is therefore clobbered by the CALL.
2. **TCC backend is deleted** (commits `78b3338`, `bd7e15d`). Plan's
   "TCC limitations" notes are obsolete.
3. **AOT pre-translation pipeline exists** (`jit_aot.go`,
   `jit_segment.go`). Blocks are discovered and compiled at
   `InstallAOT()` time from static CFG analysis in `aot.go`. A few
   paths still lazy-compile (`jit_native.go`), but the hot loop is
   AOT.
4. **V2 lowerer (`ir/lower_amd64_v2.go`) does not implement IRSyscall
   fast path** — it falls back to `v2Ret(..., status=jitEcall)`. AOT
   uses V1 (`LowerAMD64AOT` in `ir/lower_amd64.go`). We will ship V1
   only and defer V2.
5. **Emitter uses `dirty[]` for writeback tracking** (`ir/emit.go`,
   `ir/highlevel.go:WriteBackAll`). The plan's proposed
   `touched[]`/`needsReload[]` are *additions*, not replacements.
6. **`currentSyscallDispatcherAddr()`** already exists in
   `jit_syscall.go`; dispatcher returns 0=handled / 1=fallback per
   `internal/syscalls` SysV contract. Hellobench wire-up is complete.
7. **Existing debug infra is minimal**: `SetDebugJIT(bool)` in
   `jit.go:19` only gates compile-failure prints. `goasm/api.go:242`
   exposes `Ctx.DumpProgs()` (Go asm syntax). `ir.IRInstr.String()`
   (`ir/ir.go:324`) pretty-prints IR. `tools/disasm_riscv.py`
   disassembles guest code from an ELF.
8. **VizJit scaffold already exists**: `ir/ir.go:8-18` declares
   exported `ir.VIZJIT_DIR` with a default on-path, and `init()`
   lets env var `GOCPU_VIZJIT` override it. Step 0 builds the dump
   logic on top of this scaffold.

## Critique of the original plan

1. **All-or-nothing**: four correlated edits (flow classify, emitter
   continuation, lazy reload, lowerer inline check) land in one
   commit. If `make hello` crashes, the bisection target is
   1-of-4. That is how the prior attempt wedged.
2. **Hidden lowerer work**: "Mirror in `lower_amd64_v2.go`" makes V2
   sound like a sed-replace. It is not — V2 has different ABI
   assumptions. Skip V2 entirely for Phase 5-B.
3. **Zero runtime visibility**: when the JIT misbehaves we cannot see
   what it emitted, what IR it built, or which guest instruction
   maps to the broken x86. The first thing we add is a dump.
4. **`touched[]` / `needsReload[]` overlap with `dirty[]`**. The plan
   does not say whether these cooperate or replace. They should be
   additions: `dirty` governs writeback (pre-CALL), `touched` governs
   reload (post-CALL).
5. **Cold fallback is bundled with the hot path change**. We can ship
   "ECALL still terminates but without the block-exit round-trip" as
   an intermediate step, then add the in-block fallback stub after.

## Revision (2026-04-22) — pivot to Option D

After Steps 0–3 landed, a bug surfaced in Step 3: clearing `dirty[]`
inside `ir.(*Emitter).Syscall` (a natural move when the emitter
continues past ECALL) caused `TestHelloGoCPU_JIT_DirectSyscall` to
hang. Root cause: `deferredExit` structs (registered when a branch
targets outside the current block range) read `e.dirty[]` via
`WriteBackAll()` at **finalize time**, not at registration
time. Pre-ECALL branch exits synthesized in `finalize()` would see
the post-Syscall (cleared) dirty[] and emit no writebacks, so a
`bnez a3, 0x101c` taken-path jumped to a chain target with
decremented `x13` never stored back to memory → infinite loop.

Four ways to handle the pre/post-ECALL state transition were
enumerated (see "Rationale" below Step 5). We pivot from the
original plan's **in-block continuation + touched/needsReload + dirty
clear** (a hybrid of options A & C) to **Option D**: post-ECALL
instructions live in their own IR block, reached from the ECALL
block via the existing chain-exit machinery.

This preserves today's "one block = one dirty epoch" invariant that
`dirty[]`, `deferredExit`, `finalize`, and `WriteBackAll` all depend
on. The cost is a chain-exit on the hot path (~1 ns MOVABS+JMP R10)
plus the post-ECALL block's own prologue loads (~2 ns for loads of
regs used post-ECALL). Against a 33 ns budget, this ~3 ns residual
is acceptable.

Key fact that makes Option D nearly free: today's AOT enumerator
(`aot.go:collectBranchTargets` → `aot.go:74`) already registers
post-ECALL PC as a block start when ECALL classifies as `flowTerm`.
Step 2's `InlineEcallEnabled` → `flowSeq` change was what made the
post-ECALL PC *stop* being a block start. Step 4 reverts that.

## Plan — 7 steps, each independently verifiable

Each step is one commit. Each has a go-test that proves it works and
a rollback path. No step proceeds until its test passes and `make
hello` is still correct (though not yet faster until Step 5).

---

### Step 0 — `VizJit`: per-block dump at AOT compile time

**Goal**: make every subsequent step observable.

**Activation** — already wired in `ir/ir.go:8-18`:
- Package-scoped exported variable `ir.VIZJIT_DIR string` with a
  non-empty default path. Writing is ON by default without requiring
  an env var (convenient for ad-hoc runs and where setting env
  vars is painful, e.g. IDE launch configurations).
- Env var `GOCPU_VIZJIT=<dir>` overrides `VIZJIT_DIR` at `init()`
  time. To disable, set `ir.VIZJIT_DIR = ""` before `InstallAOT()`.
- One file per block compiled by `jitCompileAOTSegment` (and
  `jitCompileDebug` for lazy blocks) written to that directory.

**Filename**: `<rand>.gocpu.asm.pc_<hex>.asm`
- `<rand>` is a 16-hex-char random prefix from `crypto/rand`,
  generated **once per emulator run** and reused for every block in
  that run. Putting it **first** makes a sorted `ls` listing group
  all outputs from one run together.
- `<hex>` is the block's entry PC (e.g. `0x00010234`).
- Reruns do not overwrite prior dumps because each run has a fresh
  `<rand>`.

**File contents** (three sections, in order):

```
# gocpu VizJit dump
# entry PC: 0x00010234
# byte range: 0x00010234..0x0001025c (40 bytes, 10 insns)
# host code: 0x7f3a40001000, 127 bytes

== Guest RISC-V ==
0x00010234  0x00050513  mv     a0, a0
0x00010238  0x00008067  ret
...

== IR ==
IRMov.I64 v10 = v10
IRLoad.I64 v1 = [xBase + 8]
...

== Host (goasm Progs) ==
MOVQ R12, DI
MOVQ R14, SI
CALL R11
...
```

**Implementation sketch**:

- New file: `jit_vizjit.go`
  - `func vizJitSessionTag() string` — returns the 16-hex-char
    `crypto/rand` tag, initialized once via `sync.Once` on first
    call.
  - `func vizJitEnabled() (dir string, ok bool)` — reads
    `ir.VIZJIT_DIR`; returns `("", false)` if empty. Creates dir on
    first enabled call (idempotent `os.MkdirAll`).
  - `func vizJitDumpAOT(startPC, endPC uint64, mem *GuestMemory,
    block *ir.Block, progs string, codeLen int, codeBase uintptr)`
  - Called at end of Pass 1 of `jitCompileAOTSegment` (before `bc`
    is appended) — we already have `res.block`, `lowerResult`, and
    `code` in scope (`jit_aot.go:44-78`).
  - Guest disassembly: decode each insn with the existing
    `decodeInsn32` (`decode.go:9`) and render a compact line. Write
    a tiny mnemonic dispatcher — about 100 LOC covering the
    instructions the RISC-V test ELFs use. Fall back to `??? raw=0x…`
    for anything unknown. (Do **not** shell out to the Python tool —
    adds a process dependency and ELF-path coupling.)
  - IR listing: iterate `block.Instrs` and call existing
    `IRInstr.String()` (`ir/ir.go:324`).
  - Host code: `ctx.DumpProgs()` (`goasm/api.go:242`). For AOT we
    need to capture `progs` before `ctx` goes out of scope — extend
    `aotBlockCompile` with a `progs string` field.

- Wire into lazy path too: `jit_native.go:jitCompileDebug` already
  captures `progDump`; reuse it.

- Opportunistic extras: also write
  `<rand>.gocpu.asm.index.txt` at `InstallAOT` return, listing
  `(pc → filename)` for quick lookup.

**Note on `ir/ir.go` default path**: the current default value in
`ir/ir.go:10` contains a typo (`/User/jaten/...` missing the `s`).
Step 0 should fix it to `/Users/jaten/...` or better, make the
default an empty string and require explicit opt-in via env var or
programmatic assignment. Discuss with user before changing —
preserving current behavior (default on) is the user's stated
intent, so fixing the typo is probably enough.

**Test**:
- `GOCPU_VIZJIT=/tmp/vj go test -run TestHello .` — verify files
  appear with the `<rand>.gocpu.asm.pc_<hex>.asm` pattern, sorted
  `ls /tmp/vj` shows all block dumps from one run contiguous, files
  have three sections, at least one block's guest disasm matches
  `tools/disasm_riscv.py` output for that PC range.
- `ir.VIZJIT_DIR = ""` at test start → no files written, zero-cost
  path; verify no perf regression in a quick benchmark.

**Rollback**: revert the commit. Setting `ir.VIZJIT_DIR = ""`
disables dumping without reverting.

---

### Step 1 — Plumb the `InlineEcallEnabled` flag (no behavior change)

**Goal**: introduce the gate without changing emitted code.

**Edits**:
- `jit_syscall.go`: add

  ```go
  var inlineEcallEnabled bool  // default false
  func SetInlineEcallEnabled(on bool) { inlineEcallEnabled = on }
  func InlineEcallEnabled() bool       { return inlineEcallEnabled }
  ```

- No other file touched. Flag is read in Steps 2–5.

**Test**:
- `go test ./...` — all existing tests still pass. Flag default off =
  today's behavior bit-for-bit.

**Rollback**: trivial.

---

### Step 2 — `classifyFlow`: ECALL → `flowSeq` when flag on

**Goal**: let the AOT scanner discover code past an ECALL, so the
block is eligible to contain post-ECALL instructions. Emitter still
terminates at ECALL (Step 3 fixes that).

**Edit** (`jit_decode.go:89-90`):

```go
case 0x73:
    if InlineEcallEnabled() && insn == 0x00000073 { // ECALL only
        return flowSeq, 0, 4
    }
    return flowTerm, 0, 4
```

**Test**:
- New test `TestClassifyFlow_EcallGated` in `jit_decode_test.go`:
  with flag off, ECALL classifies as `flowTerm`; with flag on,
  `flowSeq`. Also confirm EBREAK and CSR\* still return `flowTerm`.
- `GOCPU_VIZJIT=/tmp/vj-s2 ... TestHello` with flag on — VizJit
  dumps should now show blocks that *scan past* ECALL in the CFG
  (visible in the index file and in the range headers). Emitter
  still stops at the ECALL so host code is identical.

**Rollback**: toggle flag off, or revert the `if` branch.

---

### Step 3 — Emitter continues past ECALL (still exits at block end)

**Goal**: when the flag is on, the emitter keeps emitting the next
instruction after ECALL in the same IR block. The V1 lowerer still
terminates the host block at IRSyscall (the inline status check is
Step 5), so functionally the block stops, but we verify the emitter
no longer hard-terminates.

**Edits**:
- `jit_emit_ir.go:1049-1054`:

  ```go
  case 0x00000073: // ECALL
      e.advancePC(4)
      e.emitSyscall(e.pc, currentSyscallDispatcherAddr())
      if !InlineEcallEnabled() || currentSyscallDispatcherAddr() == 0 {
          e.terminated = true
      }
  ```

- Emitter's `dirty[]` must be reset after `Syscall()` because
  `WriteBackAll()` already fired. Add to `ir.(*Emitter).Syscall`
  (see Step 4 for the same hook).

**Important invariant at this step**: even though the emitter
continues, the V1 lowerer for IRSyscall still emits
`emitEpilogue()` → RET. So in compiled code the block still exits
at ECALL. Post-ECALL IR that we emit is **dead code** at the host
level. That is *intentional* for this step: we are proving the IR
generation is stable without changing host behavior. Step 5
removes the epilogue.

**Test**:
- `go test -run TestJIT_ .` — all JIT tests still green.
- `TestHello` still correct (bit-for-bit).
- VizJit diff: compare Step 2 vs Step 3 dumps for a block with
  ECALL. The `== IR ==` section should now contain ops after
  `IRSyscall`; the `== Host ==` section should be unchanged (RET
  after the CALL).

**Rollback**: flag off.

---

### Step 4 — Option D: restore `flowTerm` for ECALL; emitter terminates unconditionally

**Goal**: undo Steps 2 & 3's behavioral changes. ECALL unconditionally
classifies as `flowTerm` and unconditionally terminates the emitter.
This restores the natural AOT-enumerator behavior where post-ECALL
PC becomes a block start (via `aot.go:74`'s `termFT[pc+insnSize]`
registration), which Step 5 will target with an inline chain exit.

**The `InlineEcallEnabled` flag stays in place** but no longer gates
any emitter/classifier behavior. It will gate *only* Step 5's
lowerer change (inline `TESTQ+JNZ+ChainExit` vs today's
unconditional-epilogue).

**Edits**:

- `jit_decode.go` — revert Step 2. Remove the `InlineEcallEnabled()`
  check in `case 0x73`:

  ```go
  case 0x73: // SYSTEM (ECALL, EBREAK, CSR)
      return flowTerm, 0, 4
  ```

- `jit_emit_ir.go` — revert Step 3. ECALL unconditionally terminates:

  ```go
  case 0x00000073: // ECALL
      e.advancePC(4)
      e.emitSyscall(e.pc, currentSyscallDispatcherAddr())
      e.terminated = true
  ```

- `ir/emit.go:Syscall` — the load-bearing comment added after the
  Step 3 hang (documenting "do NOT clear dirty[]") can be trimmed
  to a one-line reminder since the "continue past ECALL" pressure
  is gone. But leave the function body as-is (no dirty[] clear).

**No `touched[]` / `needsReload[]`.** No `deferredExit` snapshot.
No lowerer-side multi-epoch bookkeeping. Each AOT block — the one
ending at ECALL and the one starting at post-ECALL PC — has its
own `dirty[]` epoch, independently computed.

**Safety check (FixedStaticAllocator)**: a post-ECALL block entered
by chain-exit must assume all guest regs are in memory (prologue
will IRLoad only those used). Today's allocator already behaves
this way — every block starts with a prologue that loads regs
it will read; unused regs stay in memory. No change needed.

**Test**:
- `go test ./...` — all tests still pass. This is a revert, so
  flag-off and flag-on behavior should be bit-identical to the
  pre-Step-2 state.
- Update `TestClassifyFlow_EcallGated` — it currently asserts
  flag-gated ECALL classification. Rewrite as a negative
  assertion: `classifyFlow` returns `flowTerm` for ECALL
  regardless of `InlineEcallEnabled()` state. EBREAK and CSR\*
  continue to return `flowTerm`.
- `TestInlineEcall_HelloEndToEnd` — still passes; with flag on,
  the emitter terminates at ECALL (same as flag off today), so
  output is bit-identical. Update the test comment — the "Step 3
  regression" narrative it guards against no longer applies; the
  test now guards that flag-on end-to-end remains correct before
  Step 5 adds the fast-path inline check.
- VizJit dumps with flag on: blocks terminate at ECALL again, AND
  a separate dump file appears for the post-ECALL PC as an
  adjacent AOT block. Verify via the `.index.txt` that both PCs
  are registered as block entries.

**Rollback**: revert the commit.

---

### Step 5 — V1 `lowerSyscall`: inline TESTQ+JNZ + ChainExit to post-ECALL block

**Goal**: close the 33 ns gap (minus ~3 ns chain-exit + prologue
residual) by replacing the unconditional `emitEpilogue()` after the
dispatcher CALL with: (a) an inline status check (TESTQ+JNZ),
(b) a hot-path chain exit to the post-ECALL block (which exists
thanks to Step 4's `flowTerm` revert), and (c) a cold-path fallback
that preserves today's round-trip-to-Go behavior.

**Edit** (`ir/lower_amd64.go:lowerSyscall`, currently ~lines
1991-2036):

Gated on `InlineEcallEnabled()`. When off, emit the original
unconditional-epilogue body (bit-identical to today). When on:

```asm
; setup args (unchanged)
MOVQ   R12, RDI
MOVQ   R14, RSI
MOVQ   R15, RDX
MOVABS $dispatcher, R11
CALL   R11

; inline status check: 0 = handled (hot), nonzero = fallback (cold)
TESTQ  RAX, RAX
JNZ    L_cold_<unique>

; hot path: chain exit to post-ECALL block
<dealloc spill frame, identical to lowerChainExit prologue-undo>
MOVABS $resumePC_sentinel, R10   ; imm64 patched by Pass 3 / tryPatchChain
JMP    R10

L_cold_<unique>:
; cold path: dispatcher asked for host fallback (slow path)
MOVABS $resumePC, R10
MOVQ   R10, 0(RBX)    ; sret.PC
MOVQ   RBP, 8(RBX)    ; sret.IC
MOVQ   $1, 16(RBX)    ; status = jitEcall
MOVQ   $0, 24(RBX)    ; sret.FaultAddr
<emitEpilogue>        ; restore + RET
```

**Reuse existing chain-exit machinery**: the `MOVABS+JMP R10`
sequence mirrors `lowerChainExit` (`ir/lower_amd64.go:451-473`).
Register the MOVABS imm64 position in `chainExitInfo` via the same
path IRChainExit uses, so:

- **Pass 3** of `jitCompileAOTSegment` (`jit_aot.go:149-170`)
  pre-resolves the sentinel to the post-ECALL block's `chainEntry`
  when that block is in the same AOT segment (common case).
- **`tryPatchChain`** (`jit.go:906-918`) handles runtime
  cross-segment resolution on first ECALL fall-through.

**Label allocation**: use `goasm`'s label machinery (match the
convention from existing branch lowering). `L_cold_<unique>` must
be unique per IRSyscall instance — blocks with multiple ECALLs (if
any) must not collide.

**Do not touch `lower_amd64_v2.go`.** Add a one-line comment noting
V2 is parked for Phase 5-B.

**Test**:
- `go test -run TestHello .` — correctness (flag on).
- `go test -run 'TestSRL_RealBlock' ./ir/` — V1 parity (flag off,
  bit-identical to today).
- `TestInlineEcall_HelloEndToEnd` — end-to-end hello with flag on;
  produces 10000 "Hello, Go CPU!\n" lines. VizJit dump of the
  block ending in ECALL should show the new
  `TESTQ+JNZ+MOVABS+JMP/cold-epilogue` pattern. A separate dump
  file appears for the post-ECALL block's PC.
- `make hello` with flag on → `GoCPU direct callback` drops from
  ~54 ns to ≤ 24 ns/call (libriscv baseline ~21 ns; the ~3 ns
  residual comes from chain-exit + post-ECALL block's prologue
  loads).
- Regression probe: force the dispatcher to always return 1 (add
  a test-only override of `currentSyscallDispatcherAddr`, or
  patch the dispatcher fn to return 1) → confirm the cold-path
  fallback still produces correct output (slower but correct).
- Chain-patch probe: after an AOT run with the flag on, inspect
  `j.ChainPatched` — count should include chain exits emitted by
  `lowerSyscall`. If patching fails for these, the hot path
  degrades to the slow-exit stub but remains correct; log a
  warning.

**Rollback**: flag off → unconditional-epilogue path restored.

---

### Rationale: Option D chosen over Option B

Between Steps 3 and 4 we enumerated four ways to reconcile the
pre/post-ECALL state transition with the existing `dirty[]` /
`deferredExit` machinery:

- **A — Inline WriteBack at branch registration**: emit
  `WriteBackAll+ChainExit` right at the branch site instead of
  deferring to `finalize()`. Removes temporal coupling but
  fragments the block's tail layout with inline epilogues-around-
  jumps.
- **B — Per-exit dirty[] snapshot**: each `deferredExit` stores a
  copy of `dirty[]` at registration. `finalize` flushes using
  the snapshot, not current `dirty[]`. Combined with `touched[]`/
  `needsReload[]` on the Emitter, permits in-block continuation
  with lazy reloads — matches libriscv's `from_reg(n)` laziness.
- **C — Two-epoch tracking (`dirtyPre` + `dirtyPost`)**: doesn't
  generalize beyond one ECALL per block; feels like a hack.
- **D — Post-ECALL as a separate IR block**: emitter terminates
  at ECALL. Lowerer emits TESTQ+JNZ+ChainExit. Each block owns
  its own `dirty[]` epoch, independently computed.

**Chosen: D.**

**Why not B**: B is correct, but it retrofits "multi-epoch"
semantics onto `ir/emit.go` and `ir/highlevel.go`, whose invariants
are built on "one block = one epoch." Every future mid-block state-
clobbering hook (inline CSR handling, inline IRQ polling, dispatched
indirect calls) would have to audit and respect the snapshot
mechanism, and the `touched`/`needsReload` accessor hooks complicate
`XReg` / `FRegV` — accessors that currently sit on the fastest path
of the emitter.

**Why D is nearly free**: the AOT enumerator
(`aot.go:collectBranchTargets` → `aot.go:74`) already registers
post-ECALL PC as a block start when ECALL classifies as `flowTerm`.
Step 4's revert of Step 2 restores exactly that. No enumerator
changes needed. The chain-exit machinery (MOVABS+JMP R10, Pass 3
pre-resolve, `tryPatchChain` runtime patching) is battle-tested.

**Perf delta B vs D**: B closes the full 33 ns gap. D leaves
roughly 3 ns: ~1 ns for the chain exit's MOVABS+JMP R10 (well-
predicted after first execution), ~2 ns for the post-ECALL block's
prologue loads. Note that B's `needsReload` IRLoads cost the same
as the prologue loads — the raw load count is identical; only the
placement differs. So the *true* delta is ~1 ns (the chain exit),
which is small vs the 33 ns budget and small in absolute terms.

**Debuggability**: D produces two VizJit dump files per ECALL site.
The pre-ECALL block's dump ends with
`TESTQ+JNZ+MOVABS+JMP/cold-epilogue`; the post-ECALL block's dump
starts with a standard prologue. Easier to reason about than B's
single multi-epoch block dump.

---

### Step 6 — Default `InlineEcallEnabled = true`, run full validation

**Goal**: flip the default once all prior steps have landed and CI
is green. Keep the setter for rollback.

**Edits**:
- `jit_syscall.go`: `var inlineEcallEnabled bool = true`.

**Test**:
- Full suite: `go test ./...`, `make fuzz-oracle`, `make fuzz-fd`,
  `make fuzz-rvc`, `make fuzz-amo`, `make fuzz-bitmanip` (all
  require `make bench-setup` first).
- `make bench`, `make bench-cpu` — record MIPS before/after in the
  commit message.

**Rollback**: `SetInlineEcallEnabled(false)` at process start, or
flip the default back.

---

### (Deferred) Step 7 — V2 lowerer

Not part of Phase 5-B. V2 isn't on the AOT hot path. Open question
whether to delete V2 or bring it to parity; decide separately.

## Files modified (cumulative across steps)

| File | Steps | Purpose |
|---|---|---|
| `jit_vizjit.go` *(new)* | 0 | VizJit dump implementation |
| `jit_aot.go` | 0 | Call vizJitDumpAOT at end of Pass 1 |
| `jit_native.go` | 0 | Call vizJitDumpAOT from jitCompileDebug |
| `jit_syscall.go` | 1,6 | Flag plumbing + default flip |
| `jit_decode.go` | 2,4 | S2: ECALL→flowSeq when flag on; S4: reverts to unconditional flowTerm |
| `jit_emit_ir.go` | 3,4 | S3: conditional terminate at ECALL; S4: reverts to unconditional terminate |
| `ir/emit.go` | 4 | Trim Step-3-era "don't clear dirty[]" comment to one-liner |
| `ir/lower_amd64.go` | 5 | Inline TESTQ+JNZ+ChainExit on hot path + cold epilogue in `lowerSyscall` |
| `jit_decode_test.go` | 2,4 | S2: TestClassifyFlow_EcallGated (flag-gated); S4: rewrite as negative assertion |
| `jit_decode_test.go` + others | 4,5 | TestInlineEcall_HelloEndToEnd remains; update comment to reflect Option D |

Existing functions to reuse (do not reinvent):
- `ir.(*Emitter).WriteBackAll` — `ir/highlevel.go:75` (left untouched
  by Option D)
- `goasm.(*Ctx).DumpProgs` — `goasm/api.go:242`
- `ir.IRInstr.String` — `ir/ir.go:324`
- `decodeInsn32` — `decode.go:9` (for VizJit guest disasm)
- `currentSyscallDispatcherAddr` — `jit_syscall.go`
- **Chain-exit machinery (core of Step 5 Option D)**:
  - `lowerChainExit` — `ir/lower_amd64.go:451-473` (the MOVABS+JMP R10
    pattern and `chainExitInfo` registration to copy)
  - Pass 3 of `jitCompileAOTSegment` — `jit_aot.go:149-170`
    (intra-segment resolve)
  - `tryPatchChain` / `patchChainTarget` — `jit.go:906-918`
    (runtime cross-segment patching)
  - `aot.go:collectBranchTargets` `case flowTerm` at line 74 —
    already registers post-ECALL PC as a block start when ECALL
    classifies as `flowTerm` (Step 4 restores this)

## End-to-end verification

After each step, in order:

1. `go test -v .` — unit tests.
2. `go test -v ./ir/` — IR + lowerer tests.
3. `go test -v ./internal/syscalls/` — dispatcher contract.
4. `go test -run TestHello .` — byte-for-byte hello correctness.
5. With `GOCPU_VIZJIT=/tmp/vj-stepN`: inspect one block's dump;
   diff against prior step's dump for the same PC.

At Step 4 specifically:
- With flag on, VizJit dumps should show blocks terminating at
  ECALL (same as flag off), AND a separate dump file per
  post-ECALL PC. Confirm via `.index.txt` that both PCs are
  registered as block entries.
- `TestHelloGoCPU_JIT_DirectSyscall` should NOT hang (the Step 3
  dirty-clear bug is gone because the emitter no longer continues
  past ECALL).

At Step 5 specifically:
6. `make hello` — all 5 lines byte-for-byte; expect `GoCPU direct
   callback` ≤ 24 ns/call (libriscv baseline ~21 ns; Option D's
   ~3 ns residual = chain-exit + post-ECALL prologue).
7. `make fuzz-oracle` after `make bench-setup` — oracle parity vs
   libriscv.
8. VizJit diff: the pre-ECALL block's host section now ends with
   `TESTQ RAX,RAX / JNZ L_cold / MOVABS R10,<target> / JMP R10 / L_cold: ...`
   instead of the unconditional epilogue.

At Step 6:
9. Full fuzz suite (`fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
   `fuzz-bitmanip`).
10. `make bench-cpu` and `make bench` for MIPS regression check.

## Why this will not wedge

- **Every step is reversible by one flag flip** (`InlineEcallEnabled
  = false`) until Step 6, and by reverting one commit at/after Step
  6.
- **Option D preserves the "one block = one epoch" invariant.**
  `ir/emit.go`, `ir/highlevel.go:WriteBackAll`, and `finalize`'s
  `deferredExit` loop are untouched. The Step 3 hang's root cause
  (temporal coupling of `dirty[]` across `Syscall`) cannot recur
  because `dirty[]` is never mutated mid-block.
- **Every step has a specific test that must pass** before
  proceeding. If a test fails, we know exactly which step is broken
  and its blast radius is one file (mostly).
- **VizJit exists from Step 0 on**. When something behaves
  unexpectedly, we can diff per-block dumps across steps. Option
  D's separate post-ECALL block makes regressions easier to
  localize: if the ECALL block's host code is wrong we know it's
  `lowerSyscall`; if the post-ECALL block is wrong it's independent
  AOT codegen that would have broken non-ECALL tests too.
- **V2 lowerer is explicitly out of scope** — no hidden parity work.
