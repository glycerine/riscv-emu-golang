# Audit: `cpu.go` RV64 interpreter vs. Spike (riscv-isa-sim)

**Auditor:** Claude (Opus 4.8), automated source-level audit
**Date:** 2026-06-18
**Reference:** `xendor/riscv-isa-sim/` (Spike), `riscv/insns/*.h`, `riscv/mmu.{h,cc}`,
`riscv/csrs.cc`, `riscv/decode_macros.h`, `riscv/processor.cc`
**Audited file:** `cpu.go`
**Snapshot audited:** `sha256 0cc0d67c4cd5425d30005368906e9e41cc8a938d4a6eb23028c9dbc88a078738`
(git HEAD `175d9da`, mtime `2026-06-18 00:06:58`). Line numbers below refer to this
snapshot.

> ⚠️ **The file changed underneath this audit.** When the audit began, `cpu.go` was
> at commit `7e6f693` and contained a cluster of serious Zba/Zbb/Zbc **decode bugs**
> (CLZ/CTZ/CPOP/SEXT.B/SEXT.H, RORIW, SLLI.UW≥32, ZEXT.H). Those were independently
> confirmed by execution (they returned *illegal instruction* or wrong results), and
> were then **fixed in parallel** by commit `15cd6ff` *"fix the bitmanip Zba,Zbb,Zbc
> mess. The big bugs were real"*. This report re-audited the **post-fix** snapshot and
> verified those fixes are correct (see §3). All findings in §1–§2 are against the
> current, post-fix code.

---

## How the audit was done

1. Read all of `cpu.go`, `float.go`, and the `internal/fenv` package.
2. Compared each instruction's semantics against the matching Spike `riscv/insns/*.h`
   leaf and the encodings in `riscv/encoding.h`.
3. Compared trap / CSR / privileged behavior against `processor.cc`, `csrs.cc`,
   `mmu.{h,cc}`, `decode_macros.h`.
4. For every concrete claim, built the instruction word and executed it through
   `cpu.stepFromInsn(...)` to confirm actual behavior (the harness used is reproduced
   in §4 and should be added as a regression test once fixes land).

Severity key: **HIGH** = wrong computed result bits for code a normal toolchain emits;
**MED** = wrong/absent trap or architectural-state divergence reachable by ordinary
privileged software; **LOW** = only reachable by malformed/reserved encodings, or
config-dependent.

---

## Summary of original outstanding findings

| #  | Sev  | Area | One-line |
|----|------|------|----------|
| B1 | HIGH | FP   | Dynamic/static **rounding mode (`rm`/`frm`) is ignored** — every FP op uses host round-to-nearest-even; float→int always truncates (RTZ). |
| B2 | MED  | FP   | Reserved `rm` (5,6, or DYN with reserved `frm`) is **not validated** → should raise illegal-instruction. |
| B3 | MED  | FP   | `FCVT.D.S` sets **no fflags at all** (no NV on signaling-NaN input). |
| B4 | MED  | Priv | `MRET` does **not clear `mstatus.MPRV`** when returning to a privilege < M. |
| B5 | MED  | Priv | `SRET` does **not clear `mstatus.MPRV`**. |
IGNORE: | B6 | MED  | FP   | No **`mstatus.FS==0` check** — FP instructions execute even when FP unit is "Off". |
| B7 | MED  | CSR  | **No CSR privilege / read-only access control** — U/S code can read & write M-mode CSRs. |
IGNORE: | B8 | MED  | Mem  | Misaligned loads/stores are **performed transparently** instead of trapping (Spike default, without Zicclsm, traps). |
| B9 | MED  | CSR  | `mstatus`/`sie`/`mie`/`mip` writes are **not masked** (WPRI / read-only / `SD` bit not maintained). |
| B10| LOW  | CSR  | No `mcounteren`/`scounteren` enforcement on `cycle`/`time`/`instret`. |
| B11| LOW  | CSR  | `CSRRS`/`CSRRC` suppress the write based on **source value == 0** instead of **`rs1 == x0`**; CSR is read even when `rd == x0`. |
| B12| LOW  | Priv | `WFI`, `SFENCE.VMA`, `SRET` skip the `TW`/`TVM`/`TSR` and U-mode privilege checks. |
| B13| LOW  | Decode | Several `funct3`/`funct7` reserved encodings execute (e.g. BINV, CZERO, SH\*ADD) instead of raising illegal. |
| B14| LOW  | Priv | `mtvec == 0` is used as a sentinel for "no M handler"; a guest that legitimately sets `mtvec=0` won't vector traps to address 0. |
| B15| LOW  | Decode | Zicond (`CZERO.*`) and other extras are accepted unconditionally regardless of advertised ISA. |

---

## 1. HIGH-severity findings

### B1 — Floating-point rounding mode is ignored

**What Spike does.** Every arithmetic/conversion leaf sets
`softfloat_roundingMode = RM` first, where (`decode_macros.h:165–167`):

```c
#define validate_rm(rm) ({ require(rm < 5); rm; })
#define RM (insn.rm() == 7 ? validate_rm(STATE.frm->read()) : validate_rm(insn.rm()))
```

so the instruction's `rm` field (bits 14:12), or `frm` when `rm==DYN(7)`, selects
RNE/RTZ/RDN/RUP/RMM. e.g. `fadd_s.h`, `fcvt_w_s.h` (`f32_to_i32(FRS1_F, RM, true)`).

**What `cpu.go` does.** The `rm` field is never read for arithmetic. All ops route
through `internal/fenv`, whose amd64/arm64 asm performs the op at the host's default
rounding (round-to-nearest-even) and never programs MXCSR/FPCR rounding control
(`internal/fenv/ops_amd64.s` only clears/reads *exception* bits, never bits 13–14).
Float→int conversions (`float.go` `fcvtWS`, `fcvtLD`, …) use Go's `int32(f)`/`int64(f)`
casts, which are **RTZ only**. Float→float and int→float (`FADD`, `FCVT.S.D`,
`FCVT.S.L`, …) use host RNE.

**Impact.** Any instruction with a non-RNE `rm`, or `DYN` with `frm != RNE`, yields the
wrong result bits. Float→int with `rm ∈ {RNE,RDN,RUP,RMM}` (as emitted for
`lrint`/`rint`/`nearbyint`/`round`) is wrong; only the RTZ form (C `(int)` casts)
matches by accident.

**Evidence (executed).** `FADD.S f1,f2,f3,rup` with `f2=1.0 (0x3F800000)`,
`f3=2^-24 (0x33800000)` — exact sum is the midpoint, so RNE→`0x3F800000`,
RUP→`0x3F800001`:

```
FADD.S 1.0,2^-24,RUP -> 0x3f800000   (RUP-correct = 0x3f800001)   ← uses RNE, WRONG
```

**Code locations.** `cpu.go:1199–1240` (FMADD family), `cpu.go:1243–1503` (FPFUNC:
add/sub/mul/div/sqrt, all FCVT). `internal/fenv/ops_amd64.s`, `internal/fenv/ops.go`,
`float.go:146–290`.

**Fix plan.**
1. Decode `rm` in `stepFromInsn` for opcodes `0x43/0x47/0x4B/0x4F` and `0x53`
   (arithmetic/sqrt/cvt sub-functions only — not FSGNJ/FMIN/FMV/FCLASS/compare, which
   have no `rm`). Resolve `DYN(7)` to `frm = (fcsr>>5)&7`. If the resolved mode is
   `≥ 5`, raise `ErrIllegalInstruction` (see B2).
2. Plumb the mode into `fenv`: add `SetRoundMode(m)`/`RestoreRoundMode()` that program
   MXCSR[14:13] on amd64 and FPCR[23:22] on arm64, bracket each op, OR add
   rounding-aware variants. Map RISC-V RNE/RTZ/RDN/RUP→x86 00/11/01/10; RMM has no
   hardware equivalent and must be emulated (round-to-nearest, ties-away).
3. Rewrite the `fcvt*` helpers in `float.go` to round to integer **using the selected
   mode** before the range check (e.g. via `math.RoundToEven`/`Floor`/`Ceil`/`Trunc`
   then convert), so NX/saturation match Spike's `f*_to_i*`.

**Test plan.** Table-driven `stepFromInsn` test: for each of the 5 modes, a vector that
distinguishes them — `FADD.S` of midpoint operands; `FCVT.W.S` of `2.5`, `-2.5`,
`2.6`; `FCVT.L.D` near 2^63. Cross-check golden values against Spike
(`spike --isa=rv64gc -d`), or against softfloat directly.

---

## 2. MED / LOW findings

### B2 — Reserved rounding-mode not rejected  (MED)
`decode_macros.h:165` `require(rm < 5)`: `rm ∈ {5,6}`, or `rm==DYN` with `frm ∈ {5,6,7}`,
must raise illegal-instruction. `cpu.go` never inspects `rm`, so these execute.
**Fix:** fold the `rm ≥ 5` check into the B1 decode. **Test:** `FADD.S …, rm=5` and
`FADD.S …, rm=DYN` with `frm=6` must both return `ErrIllegalInstruction`.

### B3 — `FCVT.D.S` produces no FP flags  (MED, executed)
`cpu.go:1422–1425`:
```go
case 0x08: // FCVT.D.S
    src := unboxF32(c.FReg(rs1))
    r := float64(f32frombits(src))
    c.SetFReg(rd, boxF64(canonNaN64(f64bits(r))))   // no c.fcsr |= …
```
single→double is always exact, but a **signaling-NaN input must raise NV**
(`fcvt_d_s.h` ends with `set_fp_exceptions`). The sibling `FCVT.S.D`
(`cpu.go:1294–1298`) does `c.fcsr |= fenv.FFlags()`; this path does nothing.
**Evidence:** `FCVT.D.S` of sNaN `0x7F800001` → `fcsr = 0x0` (expected NV=`0x10`).
**Fix:** `if isSNaNF32(src) { c.fcsr |= fflagNV }` (single→double sets only NV).
**Test:** assert `fcsr&0x10 != 0` after converting a boxed sNaN.

### B4 / B5 — `MRET`/`SRET` don't clear `mstatus.MPRV`  (MED)
Spike `mret.h`: `if (prev_prv != PRV_M) s = set_field(s, MSTATUS_MPRV, 0);`
Spike `sret.h` (non-virt path): `STATE.mstatus->write(set_field(…, MSTATUS_MPRV, 0));`.
i.e. any xRET to a privilege below M clears MPRV. `cpu.go` MRET (`1080–1092`) and SRET
(`1093–1110`) never touch `statusMPRV (1<<17)`. A guest that set MPRV to do a
load/store "as" another mode, then `MRET`/`SRET` to U/S, keeps MPRV set and mis-routes
subsequent effective-privilege memory accesses.
**Fix:** in MRET, after computing `nextPriv`, `if nextPriv != PrivMachine { c.mstatus &^= statusMPRV }`.
In SRET (always returns to ≤S) `c.mstatus &^= statusMPRV`.
**Test:** set `mstatus.MPRV`, set `MPP=U`, execute `MRET`, assert MPRV==0.

### B6 — No `mstatus.FS == 0` trap for FP instructions  (MED)
Spike gates every F/D op with `require_fp` → `fflags->verify_permissions` which raises
illegal when `mstatus.FS == 0`. `cpu.go` has no FS check anywhere; FP always works, and
FP ops never set `mstatus.FS` dirty either. **Fix:** at entry to opcodes
`0x07/0x27/0x43/0x47/0x4B/0x4F/0x53` and the F-CSR accesses, `if c.mstatus&statusFS==0 { return ErrIllegalInstruction }`,
and set `FS=Dirty(3)` on any f-register/`fcsr` write. **Test:** clear `mstatus.FS`,
execute `FADD.S`, expect illegal; verify FS becomes dirty after an enabled FP op.

### B7 — No CSR privilege / read-only enforcement  (MED)
Spike `csrs.cc:34–50` `verify_permissions`: `priv < csr_priv` (bits 9:8 of the CSR
address) → illegal; `write && csr_read_only` (bits 11:10 == 11) → illegal.
`cpu.go readCSR/writeCSR` (`1847–1997`) switch purely on address and ignore `c.priv`.
A U-mode or S-mode guest can read/write `mstatus`, `satp`, `mepc`, etc. **Fix:** before
dispatch in the SYSTEM/Zicsr arm (`cpu.go:1114`), check
`if PrivilegeMode((csrAddr>>8)&3) > c.priv { return ErrIllegalInstruction }` and, for
writes, `if (csrAddr>>10)&3 == 3 { return ErrIllegalInstruction }`. **Test:** from
`PrivUser`, `csrr t0, mstatus` and `csrw mscratch, t0` must both be illegal; `csrw cycle,x1`
illegal from any mode.

### B8 — Misaligned accesses are silently performed, not trapped  (MED, config-dependent)
Spike (`mmu.cc:299–316`, `is_misaligned_enabled() = extension_enabled(EXT_ZICCLSM)`)
**throws `load/store_address_misaligned` by default**; only with Zicclsm does it split
and perform them. `cpu.go` loads/stores use the `load16→load16U` fallback pattern
(`cpu.go:395–483`, `1147–1196`) which *retries unaligned and succeeds*. So all
ordinary misaligned accesses succeed here but trap on a stock Spike. (The Go `misa`
`RV64IMAFDCSU` does not advertise Zicclsm.) **Fix (if exactness desired):** gate the
`*U` fallback on a `c.misalignOK` flag derived from advertised Zicclsm; otherwise
propagate the `FaultMisalign`. **Test:** unaligned `LW`/`SW` should produce a
misaligned trap (cause 4/6) when Zicclsm is off. *Note:* AMO/LR/SC already propagate
misalignment (`cpu.go:927–988` has no `*U` fallback) — but the cause should be
**store/AMO-misaligned (6)** for AMO/SC even though the access starts with a `load32`;
verify the fault→cause mapping in `note.go`/`run_cached.go` reports 6, not 4 (Spike's
`convert_load_traps_to_store_traps`, `mmu.h:171–183`).

### B9 — CSR writes are not masked (WPRI / read-only / `SD`)  (MED)
`writeCSR` stores raw values: `mstatus = val` (`1961`), `sie = val` (`1936`),
`mie = val` (`1967`), etc. Spike maintains WARL/WPRI masks, keeps read-only sub-fields,
and recomputes `mstatus.SD` (bit 63) from FS/XS/VS. Consequences vs Spike:
* `mstatus.SD` is never set → reading `mstatus`/`sstatus` after FP use shows `SD=0`
  (Spike shows 1). `sstatusMask` (`cpu.go:50`) also omits `SD`.
* Writing reserved bits to `mstatus`/`sie`/`mie` reads them back (Spike reads 0).
**Fix:** apply per-CSR legal-value masks; recompute SD on read. **Test:** write
`0xFFFFFFFFFFFFFFFF` to `mstatus`, read back, compare to Spike's WARL result; set FS=11,
read `mstatus`, expect bit 63 set.

### B10 — No counter-enable enforcement  (LOW)
`cycle`/`time`/`instret` (`0xC00/0xC01/0xC02`) are returned regardless of
`mcounteren`/`scounteren`. Spike raises illegal (or virtual) when the relevant
enable bit is clear and `priv < M`. **Fix:** check `mcounteren`/`scounteren` bit for
the counter index against `c.priv`. **Test:** `mcounteren.CY=0`, read `cycle` from
S/U → illegal.

### B11 — `CSRRS`/`CSRRC` write-suppression uses value, not `rs1`; CSR read not suppressed for `rd==x0`  (LOW)
`cpu.go:1138`: `if src != 0 || funct3 == 1 || funct3 == 5`. For register `CSRRS`/`CSRRC`
the spec suppresses the *write* iff `rs1 == x0`, **not** iff the value is 0. So
`csrrs t0, <read-only csr>, x5` with `x5==0` must still attempt the write and raise
illegal on a read-only CSR; here it is silently skipped. Also, `CSRRW`/`CSRRWI` with
`rd==x0` must **not read** the CSR (avoid read side-effects); `readCSR` is always
called (`1116`). **Fix:** branch on `rs1` (register forms) / `uimm` (immediate forms)
for write-enable, and skip the read when `rd==0 && (funct3==1||funct3==5)`.
**Test:** `csrrs x0, time, x5(=0)`… and read-only-CSR write detection.

### B12 — Missing trap-virtualization / privilege checks for WFI, SFENCE.VMA, SRET  (LOW)
* **WFI** (`cpu.go:1111`, no-op always): Spike `wfi.h` → illegal in U-mode (S
  implemented), or when `mstatus.TW=1` in S-mode.
* **SFENCE.VMA** (`cpu.go:1112–1113`, always flushes): Spike `sfence_vma.h` requires
  `PRV_S`, and `PRV_M` when `mstatus.TVM=1`; U-mode → illegal.
* **SRET** (`cpu.go:1093`): only checks `priv==U`; Spike also requires M when
  `mstatus.TSR=1` in S-mode.
**Fix:** add the `TW`/`TVM`/`TSR` and privilege checks. **Test:** `WFI` from U →
illegal; `SFENCE.VMA` from U → illegal; `SRET` from S with `TSR=1` → illegal.

### B13 — Reserved encodings execute instead of trapping  (LOW)
Several R-type arms don't validate `funct3` and fall through to a computed value:
`case 0x34` BINV (`cpu.go:781`, any `funct3`), `case 0x07` CZERO (`721`, `funct3∉{5,7}`
writes 0), `case 0x10` SH\*ADD (`736`, `funct3∉{2,4,6}` writes 0), plus the OP-IMM
`SLTI/SLTIU/XORI/ORI/ANDI` arms which never `return ErrIllegalInstruction` for a bad
sub-op (none exists, so harmless). Spike rejects these via exact `MASK_*`. **Fix:** add
`default: return ErrIllegalInstruction` (and explicit `funct3` guards) to those arms.
**Test:** e.g. `funct7=0x34, funct3=0, opcode=0x33` → expect illegal.

### B14 — `mtvec == 0` sentinel  (LOW)
`trapToMachineAt` (`cpu.go:269–272`) treats `mtvec==0` as "no machine handler" and
returns false (so `ECALL` escapes to the OS layer). On real hardware/Spike, `mtvec=0`
is a valid base and traps vector to PC 0. Harmless for the firmware/Linux flows (mtvec
is set), but a deliberate `mtvec=0` guest diverges. Document, or use a separate
"OS-emulation enabled" flag instead of overloading `mtvec`.

### B15 — Extensions accepted unconditionally  (LOW)
`CZERO.EQZ/NEZ` (Zicond, `cpu.go:721`) and the bit-manip ops execute regardless of what
`misa`/the ISA string advertises. Against a Spike configured *without* Zicond/Zbb these
are illegal. Only matters if the comparison target's ISA string is narrower than what
`cpu.go` implements. Keep the two configs in sync when differential-testing.

---

## 3. Previously-found decode bugs — now FIXED (verified)

The original `cpu.go` (commit `7e6f693`) had these, all **confirmed broken by
execution** during this audit, then fixed by `15cd6ff`. Re-verified correct in the
current snapshot and cross-checked against `riscv/encoding.h` / `riscv/insns/*.h`:

| Instruction | Old behavior (executed) | Now | Spike ref |
|---|---|---|---|
| `CLZ/CTZ/CPOP/SEXT.B/SEXT.H` (OP-IMM `0x13`) | **illegal instruction** (outer case `0x60`, should be funct6 `0x18`) | correct (`cpu.go:508–522`) | `MATCH_CLZ 0x60001013` |
| `RORIW` (OP-IMM-32 `0x1B`) | **illegal instruction** (`>>1` vs case `0x30`) | correct (`cpu.go:595–596`) | `MATCH_RORIW 0x6000501b` |
| `SLLI.UW` shamt ≥ 32 | wrong result (5-bit shamt, misdecoded as SLLIW) | correct, 6-bit shamt (`cpu.go:575–576`) | `MASK_SLLI_UW 0xfc00707f` |
| `ZEXT.H` (RV64, `0x3B`) | returned low **32** bits (ran as ADD.UW) | `rs1 & 0xFFFF` (`cpu.go:862–866`) | `packw rd,rs,x0`, `MATCH_PACKW 0x800403b` |
| `CLMULR` | looped `i<63` (off-by-one) | `i<64`, `a>>(63-i)` (`cpu.go:678–685`) | `clmulr.h` |
| `CLZW/CTZW/CPOPW` | wrong opcode space (`0x3B`) | moved to `0x1B` (`cpu.go:580–585`) | `MATCH_CLZW 0x6000101b` |

Also spot-checked and **correct** in the current code: M-extension (MUL/MULH/MULHSU/
MULHU/DIV/REM and W-forms, incl. div-by-zero & signed-overflow specials), all RVC
immediate reconstructions (quadrants 0/1/2), AMO ops + sign-extension, B/J/JAL/JALR
immediate forms, MIN/MAX/MINU/MAXU, BSET/BCLR/BSETI/BCLRI/BINV/BEXT(I), ROL/ROR/ROLW/
RORW/RORI, ORC.B, REV8, FSGNJ\*, FMIN/FMAX NaN+±0 rules, FCLASS, FMA sign conventions.

---

## 4. Reproduction harness

The throwaway tests below were used to confirm the findings (they were removed from the
tree because the asserting ones intentionally fail against the buggy paths; re-add as
regression tests once B1/B3 are fixed). Package `riscv`, same dir as `cpu.go`.

```go
func auditRun(t *testing.T, insn uint32, x2, x3 uint64) (uint64, error) {
    mem, _ := NewGuestMemory(Size64MB)
    cpu := NewCPU(*mem)
    cpu.SetPC(0x1000); cpu.SetReg(2, x2); cpu.SetReg(3, x3)
    return cpu.Reg(1), cpu.stepFromInsn(insn) // rd=x1
}

// B1: rounding mode ignored. FADD.S f1,f2,f3,rup = 0x003130D3
//   f2=1.0(0x3F800000), f3=2^-24(0x33800000); RUP must give 0x3F800001, code gives 0x3F800000.
// B3: FCVT.D.S f1,f2 = 0x420100D3 with f2=boxF32(0x7F800001) sNaN; fcsr must have NV(0x10), code gives 0.
```

Confirmed-FIXED encodings (now all pass): `CLZ 0x60011093`(→63), `CTZ 0x60111093`(→3),
`CPOP 0x60211093`(→8), `SEXT.B 0x60411093`(0xFF→-1), `SEXT.H 0x60511093`,
`RORIW 0x6041509b`, `SLLI.UW#32 0x0a01109b`(→0xFFFFFFFF00000000),
`ZEXT.H 0x080140bb`(0x12345678→0x5678).

---

## 5. Recommended fix order

1. **B1 + B2 + B3** (FP rounding & flags) — the only HIGH item; affects real numeric
   results. Implement together (decode `rm` once, validate, plumb to `fenv` + rewrite
   `fcvt*`).
2. **B7, B6, B9** (CSR access control, FS gating, write masking) — needed for any
   privileged/`-p`/`-v` differential test against Spike to pass.
3. **B4/B5, B8, B12** — privileged corner cases exercised by OS code.
4. **B10, B11, B13, B14, B15** — hardening / exactness for malformed encodings and
   narrow-ISA differential runs.

The cleanest path to "not a single bit different" is a **differential fuzzer**:
random instruction streams + random register/`fcsr`/`frm` state run lock-step against
`spike` (commit-log mode, `--isa=rv64gc_zba_zbb_zbc_zicond`), diffing GPR/FPR/`fcsr`/
`pc`/trap after each step. That mechanically surfaces B1–B3 and any residual decode gap.

---

## 6. 2026-06-18 Codex follow-up status

This section supersedes the "outstanding" wording above for the current tree.

### Fixed

* **B2/B3/B6:** FP instructions now validate static/dynamic `rm`, reject reserved
  `frm`, require `mstatus.FS != Off`, mark FP state dirty, and `FCVT.D.S` raises NV for
  signaling-NaN input.
* **B1, partial:** float-to-int conversions now honor RNE/RTZ/RDN/RUP/RMM. On amd64,
  FP arithmetic/conversion paths use MXCSR rounding for RNE/RTZ/RDN/RUP.
* **B4/B5:** `MRET`/`SRET` clear `mstatus.MPRV` when returning below M.
* **B7/B10/B12:** CSR privilege/read-only/counter checks and WFI/SFENCE/SRET
  privilege checks are enforced in strict firmware mode. This is the path used by
  `emu -bios` (`prepareBiosGuest` calls `EnableStrictCSR()` and starts in M-mode).
  The legacy process-mode harness remains permissive so riscv-tests reset-vector CSR
  probes keep working.
* **B9:** `mstatus`, `sstatus`, `sie`, `mie`, `mip`, `mideleg`, `medeleg`, and counter
  enable writes are masked to the modeled writable bits; `mstatus.SD` is maintained.
* **B11:** CSRRS/CSRRC write suppression now follows the encoded `rs1/uimm` field, and
  CSRRW/CSRRWI suppress the CSR read when `rd==x0`.
* **B13:** reserved Zicond/Zba/Zbs/base OP/OP-32 encodings now raise illegal instead
  of silently writing zero or executing as base arithmetic.
* **B14:** in strict firmware mode, `mtvec==0` is a valid trap vector address. In
  process-mode, `mtvec==0` remains the existing host-note escape hatch.
* **B15, resolved by advertisement:** the generated BIOS FDT now advertises `zicond`
  alongside the implemented Zba/Zbb/Zbc/Zicsr/Zifencei/Sstc set.

### Intentionally Kept / Did Not Fix

* **B8 misalignment:** intentionally kept permissive. The interpreter, cached
  interpreter, and JIT all retain bytewise retry paths for misaligned scalar/FP
  loads/stores/fetches. Comments were added at those fallback sites noting that strict
  Spike-style behavior would propagate `FaultMisalign`, but existing real tests depend
  on the permissive behavior.
* **Per-extension runtime gating:** not added. The CPU continues to implement the
  extension set it supports without a per-instruction ISA option. For the BIOS/Linux
  path, the advertised ISA was brought into sync by adding `zicond` to the FDT.
* **RMM FP arithmetic and non-amd64 host rounding:** RMM has no direct MXCSR
  equivalent, so arithmetic using RMM still falls back to the existing host behavior.
  Non-amd64 `SetRoundingMode` is currently a no-op. Float-to-int RMM is fixed because
  it is handled in Go before conversion. Exact RMM arithmetic needs a SoftFloat-style
  implementation or explicit software rounding.

### Regression coverage added/updated

* `audit_vs_spike_test.go` covers the fixed CSR, privilege, FP, reserved-decode, and
  `mtvec==0` cases.
* Existing strict CSR/SRET tests were adjusted to opt into strict firmware semantics.
* BIOS FDT tests now require `zicond` in both `riscv,isa-extensions` and the legacy
  `riscv,isa` string.
* The ARM64 QEMU red-test log was traced to the same process-mode CSR regression, not
  to an ARM64 lowerer mismatch. Re-running the focused ARM64 QEMU main lane after the
  strict-CSR scoping fix passed the interpreter riscv-tests, AOT JIT riscv-tests, lazy
  JIT riscv-tests, and `TestJITIC_MatchesInterpreter`.
