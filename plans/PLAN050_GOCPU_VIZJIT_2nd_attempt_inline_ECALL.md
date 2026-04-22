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

## Plan — 7 steps, each independently verifiable

Each step is one commit. Each has a go-test that proves it works and
a rollback path. No step proceeds until its test passes and `make
hello` is still correct (though not yet faster until Step 5).

---

### Step 0 — `VizJit`: per-block dump at AOT compile time

**Goal**: make every subsequent step observable. Activated by env var
`GOCPU_VIZJIT=<dir>`; when set, every block compiled by
`jitCompileAOTSegment` (and `jitCompileDebug` for lazy blocks) writes
**one file per block** to that directory.

**Filename**: `gocpu.asm.pc_<hex>.call_<rand>.asm`
- `<hex>` is the block's entry PC (e.g. `0x00010234`).
- `<rand>` is a 16-hex-char random suffix from `crypto/rand`
  (generated **once per emulator run** and reused for every block in
  that run, so all dumps from one invocation share a session tag and
  reruns do not overwrite prior dumps).

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
  - `func vizJitDir() string` — reads `GOCPU_VIZJIT` once via
    `sync.Once`, creates dir if set and non-empty, returns `""` if
    disabled. Returns stable session-tag string too.
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

- Opportunistic extras: also write `gocpu.asm.index.<rand>.txt` at
  `InstallAOT` return, listing `(pc → filename)` for quick lookup.

**Test**:
- `GOCPU_VIZJIT=/tmp/vj go test -run TestHello .` — verify files
  appear, have three sections, at least one block's guest disasm
  matches `tools/disasm_riscv.py` output for that PC range.
- Unset env var → no files written, zero-cost path.

**Rollback**: revert the commit. No behavior change when env var
unset.

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

### Step 4 — Lazy reload: `touched[]` + `needsReload[]`

**Goal**: when flag on and emitter continues past ECALL, any guest
reg that was touched pre-ECALL gets reloaded *at first use* after
ECALL. Registers never read post-ECALL pay zero reload cost —
matches libriscv's `from_reg(n)` laziness.

**Edits** to `ir/emit.go`:

- Add fields to `Emitter`:

  ```go
  touched     [64]bool // set on every XReg(i)/FRegV(i) read
  needsReload [64]bool // set after Syscall; cleared on reload
  ```

- Change `XReg` / `FRegV` from pass-through accessors to reload-aware
  hooks (replaces current `ir/emit.go:49-77`):

  ```go
  func (e *Emitter) XReg(i uint32) VReg {
      if i > 31 { panic("ir.Emitter.XReg: index > 31") }
      vr := VReg(i)
      if i != 0 && e.needsReload[i] {
          e.Load(vr, e.xBase, int64(i)*8, I64, false)
          e.needsReload[i] = false
      }
      e.touched[i] = true
      return vr
  }
  // FRegV analogous with VReg(32+i) and F64
  ```

- Extend `ir.(*Emitter).Syscall` (`ir/emit.go:261`):

  ```go
  e.emit(IRInstr{Op: IRSyscall, Imm: int64(resumePC), Imm2: int64(idx)})
  for i := 0; i < 64; i++ {
      if e.touched[i] {
          e.needsReload[i] = true
      }
  }
  for i := range e.dirty { e.dirty[i] = false }
  ```

- Caller contract in `jit_emit_ir.go:emitSyscall` is unchanged — it
  still pre-emits `e.irEm.WriteBackAll()` before `Syscall()`.

**Safety check for FixedStaticAllocator**: confirm that every
x0..x31/f0..f31 VReg that may be allocated to a host reg ends up
in a **caller-saved** x86 reg. Callee-saved RBX/RBP/R12–R15 are
pinned to JIT-context (sret/IC/xBase/fBase/memBase/memMask) and
are not handed out by the allocator. Verify in
`ir/regalloc_fixed.go` — if any VReg could land in a callee-saved
reg, it would survive the CALL and the reload would be
redundant-but-safe (an extra IRLoad). Either way the reload is
correct; document the observation so future regalloc changes don't
silently break the assumption.

**Test**:
- New unit test in `ir/emit_test.go`:
  `TestEmitter_LazyReloadAfterSyscall`. Construct an emitter, call
  `XReg(10)` (touched), emit a `Syscall`, call `XReg(10)` again —
  verify exactly one `IRLoad` is emitted in the block between the
  two uses. Call `XReg(10)` a third time — no additional IRLoad.
- `TestHello` with flag on still correct.
- VizJit inspection: the `== IR ==` section for a hot loop with
  ECALL should show `IRLoad` ops just after `IRSyscall` for each
  loop-carried register that is actually used post-ECALL. Confirm
  `a0/a1/a2/a7` do **not** produce reloads if they are rematerialized
  by `li` at the top of the next iteration (because they are not
  read post-ECALL in that block).

**Rollback**: flag off; `XReg`/`FRegV` hooks are inert when
`needsReload` never gets set.

---

### Step 5 — V1 `lowerSyscall`: inline TESTQ+JZ + cold fallback stub

**Goal**: remove the unconditional `emitEpilogue()` after the CALL.
When dispatcher returns 0, fall through to the next IR op. When it
returns 1, write the sret fallback and RET. This is the step that
actually closes the 33 ns gap.

**Edit** (`ir/lower_amd64.go:1991-2036`):

Replace the current body of `lowerSyscall` (keep arg-reg MOVs and
the MOVABS+CALL, drop the unconditional sret writes + epilogue):

```asm
; setup args
MOVQ R12, RDI
MOVQ R14, RSI
MOVQ R15, RDX
MOVABS $dispatcher, R11
CALL   R11
; inline status check — if RAX == 0, continue in-block
TESTQ  RAX, RAX
JZ     L_continue
; cold fallback: dispatcher returned 1, exit block with jitEcall
MOVABS $resumePC, R10
MOVQ   R10, 0(RBX)      ; sret.PC
MOVQ   RBP, 8(RBX)      ; sret.IC
MOVQ   $1, 16(RBX)      ; status = jitEcall
MOVQ   $0, 24(RBX)      ; sret.FaultAddr
<emitEpilogue>          ; restore + RET
L_continue:
; fall through — lazy IRLoads (Step 4) and later IR ops follow
```

Label allocation: use `goasm`'s label machinery (grep for existing
label patterns in lower_amd64.go, e.g. branch lowering, to match
the convention). The `L_continue` target should be a unique label
per IRSyscall instance to avoid collisions in blocks with >1
ECALL.

**Gate the new codegen on `InlineEcallEnabled()`**. When off, emit
the old unconditional-epilogue body so existing tests continue to
pass bit-for-bit.

**Do not** touch `lower_amd64_v2.go` in this commit. Add a comment
there noting V2 is intentionally parked.

**Test**:
- `go test -run TestHello .` — correctness.
- `go test -run 'TestSRL_RealBlock' ./ir/` — V1 parity unaffected.
- `make hello` with flag on → `GoCPU direct callback` should drop
  from ~54 ns to ≤ 21 ns; `GoCPU direct syscall` drops ~33 ns.
- Regression probe (as in original plan): artificially break the
  dispatcher to always return 1 → confirm the fallback path still
  produces correct output (slower but correct).
- VizJit diff on a hot-loop block: `== Host ==` should now show
  `TESTQ RAX,RAX / JZ L_continue / ... / L_continue: MOVQ ...`
  instead of an unconditional epilogue after `CALL R11`.

**Rollback**: flag off — old unconditional-epilogue path restored.

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
| `jit_decode.go` | 2 | ECALL→flowSeq when flag on |
| `jit_emit_ir.go` | 3 | Conditional terminate at ECALL |
| `ir/emit.go` | 4 | touched[]/needsReload[] + XReg/FRegV hooks + Syscall reset |
| `ir/lower_amd64.go` | 5 | Inline TESTQ+JZ+fallback in lowerSyscall |
| `jit_decode_test.go` | 2 | TestClassifyFlow_EcallGated |
| `ir/emit_test.go` | 4 | TestEmitter_LazyReloadAfterSyscall |

Existing functions to reuse (do not reinvent):
- `ir.(*Emitter).WriteBackAll` — `ir/highlevel.go:75`
- `ir.(*Emitter).Load` — existing IRLoad emitter
- `goasm.(*Ctx).DumpProgs` — `goasm/api.go:242`
- `ir.IRInstr.String` — `ir/ir.go:324`
- `decodeInsn32` — `decode.go:9` (for VizJit guest disasm)
- `currentSyscallDispatcherAddr` — `jit_syscall.go`

## End-to-end verification

After each step, in order:

1. `go test -v .` — unit tests.
2. `go test -v ./ir/` — IR + lowerer tests.
3. `go test -v ./internal/syscalls/` — dispatcher contract.
4. `go test -run TestHello .` — byte-for-byte hello correctness.
5. With `GOCPU_VIZJIT=/tmp/vj-stepN`: inspect one block's dump;
   diff against prior step's dump for the same PC.

At Step 5 specifically:
6. `make hello` — all 5 lines byte-for-byte; expect `GoCPU direct
   callback` ≤ 21 ns/call.
7. `make fuzz-oracle` after `make bench-setup` — oracle parity vs
   libriscv.

At Step 6:
8. Full fuzz suite (`fuzz-fd`, `fuzz-rvc`, `fuzz-amo`,
   `fuzz-bitmanip`).
9. `make bench-cpu` and `make bench` for MIPS regression check.

## Why this will not wedge

- **Every step is reversible by one flag flip** (`InlineEcallEnabled
  = false`) until Step 6, and by reverting one commit at/after Step
  6.
- **Every step has a specific test that must pass** before
  proceeding. If a test fails, we know exactly which step is broken
  and its blast radius is one file (mostly).
- **VizJit exists from Step 0 on**. When something behaves
  unexpectedly, we can diff per-block dumps across steps and see
  whether the regression is in the emitter (IR changed?), the
  lowerer (host changed?), or the runtime (neither changed but
  output differs?).
- **V2 lowerer is explicitly out of scope** — no hidden parity work.
