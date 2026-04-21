# FP Spec Correctness: Every Path, Every Layer

## Context

Our work on fuzzoracle turned up NaN-payload mismatches between our
Go emulator and libriscv. Fixing libriscv led to audit findings that
our own Go JIT has latent FP bugs with the same shape as libriscv's
— and a far worse one: **JIT `FADD.S 1.0, 2.0` returns canonical
NaN (`0x7FC00000`) instead of `3.0`**. The JIT's FP emission is
effectively unusable. It hasn't bitten production because
`bench_guest` doesn't use FP and `coremark` barely does, but the
code is wrong.

This plan fixes it properly, with no deferred work. Goals:

- **A)** Correct C++ on every path (interpreter, bytecode, binary
  translator). **Already done** in earlier work — left as-is.
- **B)** Correct Go on every path (interpreter, JIT native IR,
  RunCached bytecode). **To do.**
- **C)** Full testing on the Go side — JIT-specific FP tests that
  actually exercise the JIT's native-code path. **To do.**
- **D)** Full testing on the C++ side — extend the existing
  test files to cover two-NaN canon and all four FMA variants.
  **To do.**

No pragmatic half-fixes. No "future optimization can do X." No
interpreter-fallback-for-correctness shortcuts.

## Root cause analysis (Go JIT)

Confirmed empirically: JIT `FADD.S(1.0, 2.0)` → `0x7FC00000`
(canonical NaN), not `3.0`. Traced to a register-class mismatch in
`boxF32` / `unboxF32` at `jit_emit_ir.go:117-157`:

- `boxF32` (jit_emit_ir.go:117) constructs a 64-bit NaN-boxed value
  via `AndImm` + `Or` on a mix of FP-typed and untyped VRegs, then
  `Mov(dst, boxed)` where `dst` is a guest FP register (VReg 32-63,
  XMM-allocated by `regalloc_fixed.go`) and `boxed` is an untyped
  temp (GPR-allocated).
- `Mov` in `ir/emit.go:126` is hardcoded to `I64` type. The lowerer
  (`lower_amd64.go:1255 lowerMov`) emits `AMOVQ` regardless of the
  actual register classes. With `dst` in XMM and `boxed` in GPR,
  the Go assembler *does* recognize AMOVQ with cross-class operands
  and emits a valid `MOVQ xmm, r64`/`MOVQ r64, xmm` opcode — but the
  upper-32 bits of the XMM can carry stale data from prior FP ops
  (`ADDSS` writes low 32 and *preserves* high bits).
- `unboxF32` (jit_emit_ir.go:135) has the inverse problem. `Mov(srcInt,
  src)` where `src` is a guest FP VReg and `srcInt` is untyped — the
  MOVQ transfers 64 bits but if `src` was ever written with an
  F32-domain op that doesn't clear high bits (like ADDSS), the high
  32 read by `ShrImm(upper, srcInt, 32)` can be stale garbage, not
  the expected NaN-box `0xFFFFFFFF` — so the branch `upper == check`
  fails and returns canonical NaN from the fallthrough path.

**That's why 1.0 + 2.0 returns canonical NaN**: unboxing f3=2.0 sees
a stale high half, classifies it as malformed box, returns canonical
NaN from the canonicalization branch, then the downstream FAdd
propagates NaN.

Additional Go JIT bugs audited and confirmed:

1. **No NaN canonicalization on FADD/FSUB/FMUL/FDIV/FSQRT output.**
   `boxF32(rd, result)` writes the result bits verbatim. If the
   hardware produces `0xFFC00000` (negative NaN), we store that,
   not `0x7FC00000`. The interpreter calls `canonNaN32` explicitly;
   the JIT doesn't.
2. **FMA is non-fused.** `emitFMA` (jit_emit_ir.go:1721) emits
   `FMul + FAdd` — two roundings, §11.6 violation.
3. **`emitFMinMax` (jit_emit_ir.go:358) returns `b` on "a is NaN"
   and `a` on "b is NaN".** If both NaN: `aNaN == 0` → `aIsNaN` →
   `dst = b` (still NaN, not canonical). Also: `FCmp(..., LT)`
   follows IEEE 754 where `-0 == +0`; the `-0 < +0` convention is
   not honored.
4. **`RunCached` bytecode dispatch** (run_cached.go) — needs audit;
   may or may not share the bug depending on whether it re-uses
   `boxF32` helpers.

Interpreter path (cpu.go:660-695) is correct — uses `fenv.MAddF32`
hardware FMA and explicit `canonNaN32/canonNaN64`.

## Design

### Phase 1 — Fix `boxF32` / `unboxF32` register-class handling

Root cause: implicit-type `Mov` crossing register classes without
an explicit bit-cast IR op. Two options:

- **Chosen:** Route every cross-class move through `MovT(dst, src,
  t)` where `t` matches the **destination** register class
  (`F64` → XMM, `I64` → GPR). The lowerer already respects `ins.T`
  when selecting dst's class. Verify `lowerMov` correctly emits
  MOVQ between the two; x86 supports it via the encoding `66 0F
  6E` (gpr→xmm) and `66 0F 7E` (xmm→gpr).
- Alternative considered: add a dedicated `IRBitcast(dst, src,
  srcTy, dstTy)` IR op. Rejected — `MovT` already provides enough
  information for `lowerMov` to do the right thing; redundant op.

Rewrite `boxF32`:

```go
func (e *emitter) boxF32(rd uint32, val ir.VReg) {
    em := e.irEm
    dst := e.fregDst(rd)
    // Bit-cast val (F32-typed XMM) into an integer GPR temp.
    intVal := em.Tmp()
    em.MovT(intVal, val, ir.I64)  // explicit: XMM → GPR
    // Mask to low 32 bits (clears any garbage in high 32).
    low := em.Tmp()
    em.AndImm(low, intVal, 0xFFFFFFFF)
    // OR in NaN-box high word.
    hi := em.Tmp()
    em.Const(hi, int64(-1)<<32)  // 0xFFFFFFFF00000000
    boxed := em.Tmp()
    em.Or(boxed, low, hi)
    // Bit-cast boxed (I64-typed GPR) into the FP destination.
    em.MovT(dst, boxed, ir.F64)  // explicit: GPR → XMM
}
```

Rewrite `unboxF32` analogously: use `MovT(_, _, I64)` for the
initial XMM→GPR bitcast. The rest of the logic (ShrImm / branch /
Zext / final MovT to F32) stays.

### Phase 2 — Add NaN canonicalization on every JIT-emitted FP op

Add a helper `canonF32(val ir.VReg) ir.VReg` that:

1. Bit-casts val to an integer temp.
2. Computes `absBits = intBits & 0x7FFFFFFF`.
3. Compares `absBits > 0x7F800000` — the IEEE 754 NaN predicate on
   the bit pattern (greater than +infinity-as-int).
4. If NaN, bit-casts `Const(0x7FC00000)` back to F32; else returns
   the original val.

Use `MovT` for bit-casts. Use `Branch` + `MovT` merge pattern
matching the existing `unboxF32` style.

Analogous `canonF64` for `F64` with threshold `0x7FF0000000000000`
and canonical `0x7FF8000000000000`.

Wire into every FP-producing op in `jit_emit_ir.go`:

- `emitFPOpS` cases 0x00 (FADD), 0x01 (FSUB), 0x02 (FMUL), 0x03
  (FDIV), 0x0B (FSQRT), 0x08 (FCVT.S.D) — wrap the `result` in
  `canonF32` before `boxF32`.
- `emitFPOpD` same list with `canonF64`.
- FMA path (rewritten in Phase 3) — canonicalize.
- FMIN/FMAX (rewritten in Phase 4) — already canon'd via the
  emitFMinMax rewrite.

Int→FP and FP→int conversions (FCVT.W.S, FCVT.S.W, etc.) don't
produce NaN from non-NaN inputs so they're out of scope. (They can
still overflow, which is handled separately in existing code.)

### Phase 3 — Add `IRFma` op + `VFMADD213SS/SD` lowering

New IR op: `IRFma(dst, a, b, c, type)` meaning `dst = a*b + c` with
one rounding.

x86 provides 3 FMA variants by operand ordering:

- `VFMADD213SS dst, b, c`: `dst = dst*b + c`  — first operand is
  dst-and-a.
- `VFMADD132SS dst, c, b`: `dst = dst*b + c` (different encoding)
- `VFMADD231SS dst, b, c`: `dst = b*c + dst`

We'll use `VFMADD213SS` (and `VFMADD213SD` for f64) because it
maps naturally to the operand pattern `dst starts as a, is
multiplied by b, plus c`.

Steps:

1. **`ir/ir.go` (or wherever IR opcodes live)**: add `IRFma`
   constant and register its textual name.
2. **`ir/emit.go`**: add `FMA(dst, a, b, c VReg, t Type)` method.
3. **`ir/lower_amd64.go`**:
   - Add `lowerFMA`: if `dst != a`, emit `MOVSS a → dst` (or
     `MOVSD`). Then emit `VFMADD213SS dst, b, c` (or `SD`).
   - The assembler's `VFMADD213SS` is already understood via `x86.
     AVFMADD213SS` constants; check that they exist. If not, add
     the `cmd/internal/obj/x86` constant (already vendored via the
     goasm extraction).
   - Add `IRFma` to the FP register-class set so dst/a/b/c are all
     XMM.
4. **`ir/regalloc_fixed.go`**: ensure 3-source op allocation is
   supported. Current binary FP ops have 2 uses; FMA needs 3. Check
   that the allocator handles `len(Uses) == 3` for FP.
5. **`jit_emit_ir.go:emitFMA`**: rewrite using `IRFma`. Handle all
   four RISC-V sign variants:

   | RISC-V  | Semantics        | x86 mapping |
   |---------|------------------|-------------|
   | FMADD   | `a*b + c`        | `Fma(a,b,c)` |
   | FMSUB   | `a*b - c`        | `Fma(a, b, -c)` — emit FNeg on c first |
   | FNMADD  | `-(a*b) - c`     | `-Fma(a,b,c)` — FNeg result |
   | FNMSUB  | `-(a*b) + c`     | `Fma(-a, b, c)` — FNeg on a first |

   Or use the dedicated x86 variants (`VFMSUB213SS`, `VFNMADD213SS`,
   `VFNMSUB213SS`) to avoid the extra FNeg. Choose based on which
   x86 opcodes are already in the Go assembler. Preference: use the
   dedicated variants for minimal emitted instructions.

6. **Canonicalize the result** via `canonF32/F64` before `boxF32`.

7. **Rounding modes**: RISC-V FMA takes a 3-bit rm in funct3. Our
   interpreter ignores this (always uses RNE). The JIT should
   match for parity — no MXCSR save/restore. Document this.

Host requirement: x86 AVX/FMA instruction support. Our target
machines (Intel i7-1068NG7 and similar) have FMA. Add a guard at
JIT initialization: `cpuid` check for FMA bit; if absent, set
`InterpOnly` or fail loudly. Out of scope for this plan — assume
FMA is available (our Makefile uses `-march=native` effectively).

### Phase 4 — Rewrite `emitFMinMax`

Current bugs (`jit_emit_ir.go:358`):

- Both-NaN returns `b` (still NaN, not canonical qNaN).
- `-0 == +0` in IEEE LT/GT, so -0/+0 ordering is wrong.

Rewrite:

```go
func (e *emitter) emitFMinMax(dst, a, b ir.VReg, t ir.Type, isMax bool) {
    // 1. Check both-NaN: if so, dst = canonical qNaN, done.
    // 2. Check a-NaN only: dst = b, done.
    // 3. Check b-NaN only: dst = a, done.
    // 4. Both numeric. Check both-zero via bit compare:
    //    if (intBits(a) | intBits(b)) & ~SignBit) == 0:
    //        - isMax: dst = +0 if either sign bit clear, else -0
    //        - else:  dst = -0 if either sign bit set, else +0
    // 5. Else numeric compare (FCmp LT for MIN, GT for MAX).
}
```

All branches use the existing IR primitives (Branch, Const, MovT,
FCmp). No new IR needed. The signed-zero path is bit-level, not
FP compare, matching our Go `fminF32/fmaxF32` helpers and
libriscv's `fmin32_rv`/`fmax32_rv` callbacks.

### Phase 5 — Audit `RunCached` bytecode path

File: `run_cached.go`. Likely contains a switch over decoded
bytecodes. If it routes FP ops directly (not through `boxF32` /
`unboxF32`), verify it:

- Uses `fenv.*` hardware FP (single-rounding FMA).
- Canonicalizes via `canonNaN{32,64}`.
- Uses `fminF32/fmaxF32` for FMIN/FMAX.

If any are missing, fix using the same patterns as cpu.go. Likely
no changes needed — RunCached may simply call into the same helper
functions — but verify.

### Phase 6 — Test plan (Go)

New file `jit_fp_correctness_test.go`. For each of:

| op      | normal inputs       | NaN-producing inputs        | canon expected |
|---------|---------------------|------------------------------|----------------|
| FADD.S  | `1.0 + 2.0 = 3.0`   | `+inf + -inf`                | yes            |
| FADD.D  | `1.0 + 2.0 = 3.0`   | `+inf + -inf`                | yes            |
| FSUB.S  | `5.0 - 3.0 = 2.0`   | `+inf - +inf`                | yes            |
| FSUB.D  | same                | same                         | yes            |
| FMUL.S  | `2.0 * 3.0 = 6.0`   | `0 * +inf`                   | yes            |
| FMUL.D  | same                | same                         | yes            |
| FDIV.S  | `6.0 / 2.0 = 3.0`   | `-0 / -0`                    | yes            |
| FDIV.D  | same                | same                         | yes            |
| FSQRT.S | `sqrt(4) = 2`       | `sqrt(-1)`                   | yes            |
| FSQRT.D | same                | same                         | yes            |
| FMIN.S  | `min(2,3) = 2`      | `min(-0,+0) = -0`            | + both-NaN     |
| FMAX.S  | `max(2,3) = 3`      | `max(-0,+0) = +0`            | + both-NaN     |
| FMIN.D  | same                | same                         | same           |
| FMAX.D  | same                | same                         | same           |
| FMADD.S | `2*3+1 = 7`         | `(0.1*10-1)` fused vs non    | yes            |
| FMSUB.S | `2*3-1 = 5`         | similar                      | yes            |
| FNMADD.S| `-(2*3+1) = -7`     | similar                      | yes            |
| FNMSUB.S| `-(2*3-1) = -5`     | similar                      | yes            |
| *.D variants | same            | same                         | yes            |

Two tests per row (normal + NaN/special). ~40 tests.

For FMA "fused" witness, pin the expected result to the exact bit
pattern of the fused result (e.g., `0.1f * 10.0f + (-1.0f)` fused =
`0x32800000` / non-fused = `0x00000000`).

**Every test runs through the JIT** (via `jit.StepBlock`). Tests
assert the JIT compiled a block (check `len(j.lazyBlocks) > 0`) AND
that the result is correct. This guarantees we're exercising the
JIT path, not the interpreter fallback.

### Phase 7 — Test plan (C++)

Extend the existing files in `xendor/libriscv/tests/instructions/`:

- `test_fp_fmin_fmax.cpp`: add cases for two-NaN → canonical qNaN
  (both f32 and f64). These would fail pre-fix (interpreter gated
  on `fcsr_emulation`; translator callbacks didn't canonicalize).
  Post-fix: both canonicalize unconditionally.
- `test_fp_fma.cpp`: currently covers FMADD.S, FMADD.D, FNMADD.S.
  Add FMSUB.S and FNMSUB.S — all four variants.
- `test_fp_nan_canon.cpp`: already covers FDIV.S, FDIV.D, FSQRT.S.
  Add FADD.S (+inf - inf), FSUB.S, FMUL.S (0 * inf). These exercise
  the bytecode path after the FLPUT_F{32,64} canon macro fix.

All tests exercise BOTH paths (step_one for interpreter, simulate
for translator); the libriscv CI can run both.

## Files to modify

| file | change |
|------|--------|
| `ir/ir.go` (or types file) | add `IRFma` opcode constant |
| `ir/emit.go` | add `FMA(dst, a, b, c, t)` method |
| `ir/lower_amd64.go` | add `lowerFMA` using VFMADD213SS/SD; handle F32/F64 |
| `ir/regalloc_fixed.go` (if needed) | 3-source FP op support |
| `jit_emit_ir.go` | rewrite `boxF32`, `unboxF32`, `emitFMA`, `emitFMinMax`; add `canonF32`, `canonF64` helpers; call canon in every FP-producing case |
| `run_cached.go` | audit; fix if FP ops bypass canon |
| `jit_fp_correctness_test.go` (new) | ~40 JIT-exercising tests |
| `xendor/libriscv/tests/instructions/test_fp_fma.cpp` | add FMSUB.S, FNMSUB.S |
| `xendor/libriscv/tests/instructions/test_fp_fmin_fmax.cpp` | add two-NaN canon |
| `xendor/libriscv/tests/instructions/test_fp_nan_canon.cpp` | add FADD.S, FSUB.S, FMUL.S NaN-producing cases |

Unchanged: interpreter (cpu.go, fenv), GuestMemory, JIT dispatcher,
C++ libriscv source (all three layers already correct from earlier
work).

## Execution order

1. **Fix boxF32 / unboxF32.** Write a minimal test that JIT FADD(1, 2)
   = 3. Current state: red. Apply Phase 1 fix. Green.
2. **Add canonF32 / canonF64.** Wire into FADD/FSUB/FMUL/FDIV/FSQRT.
   Add NaN-producing tests. Verify canonical output.
3. **Add IRFma + VFMADD213 lowering.** Add FMA op, lower, update
   emitFMA. Test with 4 sign variants + fused witness.
4. **Rewrite emitFMinMax.** Test with -0/+0, one-NaN, two-NaN.
5. **Audit RunCached.** Grep for boxF32 usage; fix if any shared
   bug.
6. **Run full Go test suite** (`go test . ./ir/ ./bench/
   ./fuzzoracle/`). All green.
7. **Run C++ libriscv tests** after adding the new cases. All
   green.
8. **Fuzz sweep** (`make fuzz-oracle fuzz-fd fuzz-rvc fuzz-amo
   fuzz-bitmanip`, 60s each). All green.
9. **Bench regression** (`make bench-chain-ref` × 10, benchstat).
   Within ±2% of Phase 2c medians. Coremark doesn't use FP so no
   impact expected; if it does, VFMADD213 is faster than the old
   buggy path anyway.

## Verification

### Correctness
- 40 new Go JIT FP tests all green.
- 6+ new C++ tests all green.
- Existing Go + C++ tests all green.
- All fuzz targets green 60s each.

### Performance
- Coremark/dhrystone/bench_guest MIPS within ±2% of Phase 2c
  baseline. The hot path for non-FP code is untouched.
- FP-heavy micro-benchmark (to be added): proper VFMADD213 should
  be within ~1.5× of interpreter path. Not a ship blocker.

### Invariants (for code review)
- Every `boxF32` / `boxF64` caller canonicalizes NaN via
  `canonF32/canonF64` OR the canon is baked into `boxF32` itself.
- Every cross-class IR Mov uses `MovT` with an explicit type that
  matches the destination register class.
- No `Mov` with implicit I64 where the dst is FP-classified.

## Non-goals

- **No MXCSR rounding-mode support.** Our interpreter already
  ignores RISC-V funct3 rm and uses RNE. Matching in the JIT is
  correct parity.
- **No sNaN→qNaN silencing** for operations that pass through
  their input. RISC-V semantics on signaling NaN are nuanced;
  current behavior (pass through the input NaN with NV flag set)
  is preserved.
- **No AVX-512 FMA variants.** Standard AVX VFMADD213 is enough.
- **No FP benchmark.** Can add one later; not a ship blocker.

## Risks / edge cases

- **IRFma allocator support for 3 sources**: verify before coding
  lowerFMA; if the allocator is strictly binary, a small extension
  is needed. Check `ir/regalloc_fixed.go:Allocate` and the
  `IRInstr.Uses` access pattern.
- **VFMADD213 opcode presence in Go assembler**: verify
  `x86.AVFMADD213SS` et al. exist in the vendored `cmd/internal/obj/
  x86` tables. If not, add them (straightforward — they're in
  upstream Go, may need backport).
- **`MovT` XMM↔GPR**: the Go assembler encodes `AMOVQ` between
  XMM and GPR correctly; we rely on this. If the lowered code turns
  out to use the wrong encoding, the fix is in `lower_amd64.go:
  lowerMov` (type-dispatched to select AMOVQ vs AMOVSS).
- **`canonF32` branch overhead**: one IRBranch per FP op adds
  emitted-code size and a predictable-not-taken branch. Cost on
  coremark: unmeasurable (no FP). Cost on FP-heavy guests: 1-2
  cycles per op — negligible vs the FP op itself.
- **Coremark regression**: if the new JIT FP path is slower than
  the old (broken) path, coremark MIPS could drop. Likely not —
  coremark hardly does FP. Measure to confirm.
- **`RunCached` audit turns up a fix**: this is fine; one more
  patch with the same pattern. Scope creep mitigated by doing the
  audit in one pass.
