# Code Review & Audit Fix Plan — `~/ris` (refactored Russell)

## Context

A bug audit of the IR-based JIT in `/Users/jaten/ris/` (running in parallel: 3
Explore agents + targeted manual verification) surfaced two real, related defects
in `jit_emit_ir.go`. Several agent-reported items were verified as **false
positives** and are excluded.

### Verified bugs

**Bug 1 — Wrong PC and FaultAddr returned from IR JIT load/store fault handlers**
(`/Users/jaten/ris/jit_emit_ir.go:437,442`)

```go
if e.hasLoadFault {
    e.irEm.PlaceLabel(e.loadFaultLabel)
    e.irEm.WriteBackAll()
    e.irEm.Ret(e.startPC, jitLoadFault, ir.VRegZero)   // ← startPC, addr=0
}
if e.hasStoreFault {
    e.irEm.PlaceLabel(e.storeFaultLabel)
    e.irEm.WriteBackAll()
    e.irEm.Ret(e.startPC, jitStoreFault, ir.VRegZero)  // ← startPC, addr=0
}
```

The TCC C-source emitter does this **correctly** at `jit_emit.go:698,768,815,841`,
returning `e.pc` (the faulting instruction) and the live `addr` value:

```go
e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitLoadFault)
```

Consequences in the IR JIT:
- `MemFault.Addr` is always `0`, breaking diagnostics, OS personalities, and any
  handler that decides based on the faulting address.
- `cpu.pc` is set to the *block start* on fault. The dispatch loop in
  `jit.go:342` does **not** force interpreter fallback on `jitLoadFault`/
  `jitStoreFault`; it builds a `MemFault`, delivers it via `NoteChain`, and on
  `NoteHandled` re-enters the same block at the same `startPC` → fault re-fires
  → **infinite loop**.

`plans/PLAN025_fix_fault_label_placement.md` documented the choice as
intentional ("interpreter replay produces the correct fault"), but no
interpreter-replay path was added to dispatch — the contract is half-built.
The recent commit `21a8024` ("fix infinite dispatch loop on mis-aligned access")
patched the *integer* misalign symptom by avoiding the fault label entirely
(byte-by-byte path); the underlying fault-label bug remains.

**Bug 2 — FP load/store misalign still routes through the buggy fault label**
(`/Users/jaten/ris/jit_emit_ir.go:1541,1573`)

```go
e.irEm.AndImm(alignBits, addr, int64(width-1))
e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, e.loadFaultLabel)   // FLW/FLD
...
e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, e.storeFaultLabel)  // FSW/FSD
```

Integer LH/LW/LD and SH/SW/SD now take a byte-by-byte path on misalign and never
hit the fault label. FP loads/stores trap on misalign and inherit Bug 1 (wrong
PC + addr=0, infinite-loop risk). The TCC C-source emitter handles FP misalign
the same way (see `jit_emit.go:815,841` — emits `addr` and `e.pc` correctly), so
the safe minimum is to fix Bug 1 first and let FP misalign trap with correct
fault info; the user has also asked to add the byte-by-byte FP misalign path so
FP behavior matches integer behavior.

### False positives (rejected after manual verification — do not fix)

- **`fcvtWUS` / `fcvtWUD` returning `0xFFFFFFFFFFFFFFFF`** (float.go:167,170,
  241,244). The inline comment says "0xFFFFFFFF sign-extended"; this is correct
  per the RISC-V spec — *all* FCVT.{W,WU}.{S,D} results are sign-extended to
  XLEN on RV64, including unsigned conversions.
- **`lowerFCvtToI` / `lowerFCvtFromI` I16/I8 paths** (`ir/lower_amd64.go:1824,
  1828,1852,1856`). RISC-V FCVT only targets W (32-bit) and L (64-bit); I16/I8
  is dead code and never reached from `jit_emit_ir.go`.
- **Map iteration in `jit_emit.go:1558`**. Generates bail-label declarations in
  C source — order is irrelevant to compiled semantics. `testIterStart` exists
  to catch ordering bugs in the IR pipeline, not the C-source pipeline.
- **`extendLoopLiveRanges` bounds check** (`ir/regalloc.go:551`). `result` is
  pre-sized to `maxVReg(b)+1` at line 403; `touched` only contains def VRegs,
  which contribute to `maxVReg`. The skip is unreachable, defensive only.

## Fix

### Fix 1 — Per-call-site fault exits in IR JIT

Mirror the TCC C-source emitter: each load/store gets its own tail that returns
its own PC and the actual computed address VReg. The shared
`loadFaultLabel` / `storeFaultLabel` scheme is replaced with a deferred-exit
list, modelled on the existing `deferredExits` machinery used for chain exits
(`jit_emit_ir.go:61,428–431`).

#### Files to modify

| File | Change |
|------|--------|
| `/Users/jaten/ris/jit_emit_ir.go` | Replace shared fault label with per-call-site deferred fault exits. |
| `/Users/jaten/ris/ir/highlevel.go` | Have `MaskedLoad` / `GuestStore` expose (or accept) the computed `addr` VReg so the caller can pass it to its own fault tail. |

#### Implementation sketch

In `jit_emit_ir.go`:

1. Add a per-emitter slice analogous to `deferredExits` (line 61):
   ```go
   type deferredFault struct {
       label  ir.Label
       pc     uint64        // faulting instruction PC
       addrVR ir.VReg       // live VReg holding the faulting address
       status int           // jitLoadFault or jitStoreFault
   }
   var deferredFaults []deferredFault
   ```
2. Remove the `loadFaultLabel` / `storeFaultLabel` / `hasLoadFault` /
   `hasStoreFault` fields (lines 57–60) and their initialization (lines 609–610).
3. In `emitLoad` / `emitStore` / `emitFPLoad` / `emitFPStore`, before each
   `MaskedLoad` / `GuestStore` call:
   - Allocate a per-call-site label with `irEm.NewLabel()`.
   - Pass it as `faultLabel` (current API).
   - Append `{label, e.pc, addr, jit{Load,Store}Fault}` to `deferredFaults`.
4. Replace the shared-fault block in `finalize()` (lines 433–443) with:
   ```go
   for _, df := range e.deferredFaults {
       e.irEm.PlaceLabel(df.label)
       e.irEm.WriteBackAll()
       e.irEm.Ret(df.pc, df.status, df.addrVR)
   }
   ```
5. Update `emitMisalignedLoad` / `emitMisalignedStore` (lines 1408,1488) to use
   their own deferred fault exits for the OOB branch on lines 1419 / 1499 (they
   currently still target the shared `loadFaultLabel`/`storeFaultLabel`).

In `ir/highlevel.go`:

- Either change `MaskedLoad` / `GuestStore` (lines 15,42) to return the
  computed `addr` VReg so the caller can capture it for the fault tail, **or**
  inline the OOB-check logic in `jit_emit_ir.go` so the address VReg is in
  scope at the call site (cleaner; keeps `MaskedLoad` signature stable for
  other callers if any).

#### Why a per-call-site exit and not a single shared label that reads PC+addr from a register?

`ir.Emitter.Ret` takes the PC as a constant `uint64`, not a VReg
(`ir.go` / lowerer assumes constant immediate). Adding a `RetIndirect` would
touch the lowerer, register allocator, and the `Status`/`PC`/`FaultAddr`
materialization in `lower_amd64.go:508`. Per-call-site fault tails are the
narrower change and match the TCC emitter's approach exactly.

#### Code-size impact

Each load/store now has a ~4-instruction fault tail (label + WriteBackAll +
3-field MOV + RET). For a typical 30-instruction block with 5 loads/stores,
this is ~20 extra native instructions, all in the cold path. The JIT's region
size cap (2048 PCs / 16 KB) is unaffected.

### Fix 2 — Byte-by-byte FP misalign path

Mirror the integer `emitMisalignedLoad` / `emitMisalignedStore`
(`jit_emit_ir.go:1408,1488`) for FP loads (FLW/FLD) and stores (FSW/FSD).

#### Files to modify

| File | Change |
|------|--------|
| `/Users/jaten/ris/jit_emit_ir.go` | Add `emitMisalignedFPLoad` / `emitMisalignedFPStore`; replace the misalign-fault branches at 1541,1573 with branch-to-misalign-label + byte-by-byte tail. |

#### Implementation sketch

For FLW (4-byte, NaN-boxed) and FLD (8-byte):
- After computing `addr`, branch to a `misalignLabel` if `addr & (width-1) != 0`
  (mirrors integer pattern at lines 1388–1395).
- Aligned path: existing `MaskedLoad` + `boxF32` (FLW) or direct load to f-reg
  (FLD).
- Misaligned path: byte-by-byte little-endian load into a temp VReg (mirroring
  `emitMisalignedLoad` at lines 1422–1442), then `boxF32` for FLW or `Mov` to
  the destination f-reg for FLD.
- For FSW (extract low 32 bits, store), reuse the integer
  `emitMisalignedStore` with `width=4` after extracting low 32 bits via `Zext`.
- For FSD, use `emitMisalignedStore` with `width=8` directly on the f-reg.

The OOB branch in the byte-by-byte path uses a per-call-site fault tail from
Fix 1.

## Critical files to read before implementing

- `/Users/jaten/ris/jit_emit_ir.go` lines 50–80 (emitter state), 416–443
  (finalize), 1380–1520 (load/store), 1521–1584 (FP load/store).
- `/Users/jaten/ris/ir/highlevel.go` lines 8–62 (`MaskedLoad`, `GuestStore`).
- `/Users/jaten/ris/jit_emit.go` lines 690–780, 800–845 (TCC reference for
  correct fault PC/addr).
- `/Users/jaten/ris/jit.go` lines 286–376 (dispatch loop — confirm fault
  delivery still behaves correctly with corrected PC/addr).
- `/Users/jaten/ris/plans/PLAN025_fix_fault_label_placement.md` (historical
  context for the current shared-label design).

## Existing utilities to reuse

- `e.irEm.NewLabel()`, `e.irEm.PlaceLabel()` — per-call-site labels.
- `e.irEm.WriteBackAll()` (`ir/highlevel.go:67`) — register flush before exit.
- `e.irEm.Ret(pc, status, addrVR)` — block exit with status code.
- Existing `deferredExits` / deferredExit struct (`jit_emit_ir.go:61`) as a
  template for `deferredFaults`.
- `emitMisalignedLoad` / `emitMisalignedStore` (`jit_emit_ir.go:1408,1488`) as
  templates for the FP misalign path.

## Verification

```bash
cd /Users/jaten/ris

# 1. Unit tests: IR + JIT
go test -v -count=1 ./ir/...
go test -v -count=1 -run 'TestJIT_' .

# 2. Official RISC-V ISA tests via the JIT path
go test -v -count=1 -timeout 120s -run 'TestRISCVTests_UI_JIT' .
# ma_data subtest must complete (was the original infinite-loop trigger)

# 3. Lockstep V1 vs V2 (catches divergence between the two lowerers)
go test -v -count=1 -run 'TestLockstep' .

# 4. Confirm fault-PC/addr in a targeted test
# Add or extend an OOB-load test in jit_test.go that asserts:
#   - cpu.pc points to the faulting instruction (not the block start)
#   - MemFault.Addr equals the actual faulting guest address (not 0)

# 5. FP misalign coverage
go test -v -count=1 -run 'TestJIT_FLW|TestJIT_FLD|TestJIT_FSW|TestJIT_FSD' .

# 6. Fuzz oracle (regression catch — requires `make bench-setup` first)
go test -v -count=1 -run 'TestOracle' ./fuzzoracle/

# 7. Benchmarks — confirm no regression on the hot path
go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=1x ./bench/
```

End-to-end success criteria:
- `TestRISCVTests_UI_JIT/ma_data` passes without timeout.
- A new `MemFault.Addr` assertion test reports the correct guest address.
- A new `cpu.pc` assertion test on JIT fault confirms the faulting instruction's
  PC, not the block-start PC.
- No regressions in the existing JIT/lockstep/oracle suites.
- MIPS metric in `BenchmarkCPU_FullExecution` is within ±2% of pre-fix.

## Out of scope (per user)

- Refactor/ folder promotion review (FixedAllocation, AMD64PoolNormal/DivMul,
  LowerAMD64Fixed) — defer.
- Stale `log.hang*`, `log.red`, `*.go~`, `*.test` cleanup — defer.
- The dispatch loop's interpreter-fallback-on-fault behavior described in
  PLAN025 — superseded by this fix; the fault path now reports correct PC/addr
  directly from the JIT, so interpreter replay is unnecessary.
