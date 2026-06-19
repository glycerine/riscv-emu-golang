I read through the SoftFloat/TestFloat docs under `xendor/ucb-bar` and the right shape is: use SoftFloat as the in-process oracle, and optionally use `testfloat_gen` for larger generated corpora.

**Recommended Test Design**

Create SoftFloat oracle tests behind a non-default build tag, e.g. `softfloat`, so normal `go test .` does not require the vendored C archive to already be built.

Files:

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cpu_softfloat_test.go`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/softfloat_oracle_test.go`

Build tag:

```go
//go:build softfloat && cgo && amd64
```

Reason: our current Makefile builds `xendor/ucb-bar/SoftFloat-3e/build/Linux-x86_64-GCC/softfloat.a`, so the first pass should target amd64.

**Oracle Rules**

SoftFloat maps very cleanly to RISC-V for rounding and flags:

```text
RISC-V rm 0 RNE -> softfloat_round_near_even   0
RISC-V rm 1 RTZ -> softfloat_round_minMag      1
RISC-V rm 2 RDN -> softfloat_round_min         2
RISC-V rm 3 RUP -> softfloat_round_max         3
RISC-V rm 4 RMM -> softfloat_round_near_maxMag 4
```

SoftFloat exception flags also match RISC-V `fflags` bit-for-bit:

```text
NX = 1
UF = 2
OF = 4
DZ = 8
NV = 16
```

So tests can compare `cpu.FCSR() & 0x1f` directly against `softfloat_exceptionFlags`.

One important normalization: SoftFloat may propagate NaN payloads, while RISC-V arithmetic generally expects canonical NaNs. So for arithmetic, FMA, sqrt, and FP conversion results, the oracle should canonicalize NaN outputs to:

```text
f32 canonical NaN = 0x7fc00000
f64 canonical NaN = 0x7ff8000000000000
```

Do not canonicalize bit-move/sign-injection operations like `FMV`, `FSGNJ`, etc.

**Operation Coverage**

The core SoftFloat-backed matrix should cover:

- `FADD.S/D`, `FSUB.S/D`, `FMUL.S/D`, `FDIV.S/D`, `FSQRT.S/D`
- `FMADD.S/D`, `FMSUB.S/D`, `FNMSUB.S/D`, `FNMADD.S/D`
- `FCVT.S.D`, `FCVT.D.S`
- `FCVT.W/WU/L/LU.S`
- `FCVT.W/WU/L/LU.D`
- `FCVT.S/D.W/WU/L/LU`
- `FEQ.S/D`, `FLT.S/D`, `FLE.S/D`

For FMA, use SoftFloat’s `f32_mulAdd` / `f64_mulAdd` and transform signs:

```text
FMADD  a*b+c  -> mulAdd(a, b, c)
FMSUB  a*b-c  -> mulAdd(a, b, -c)
FNMSUB -a*b+c -> mulAdd(-a, b, c)
FNMADD -a*b-c -> mulAdd(-a, b, -c)
```

For invalid FP-to-int conversions, compare flags against SoftFloat, but compare integer results against RISC-V’s specified saturation behavior rather than blindly trusting SoftFloat’s implementation-defined invalid integer return.

**RISC-V-Specific Tests**

Some instructions should be table-driven rather than SoftFloat-oracled:

- `FSGNJ`, `FSGNJN`, `FSGNJX`
- `FMV.X.W`, `FMV.W.X`, `FMV.X.D`, `FMV.D.X`
- `FCLASS.S/D`
- `FMIN.S/D`, `FMAX.S/D`

For `FMIN/FMAX`, include signed zero ordering, one-NaN cases, both-NaN cases, and signaling NaN invalid-flag behavior.

Also include NaN-boxing tests for single-precision sources: malformed upper 32 bits should be treated as canonical NaN by the CPU before the operation.

**Input Sets**

Use two tiers:

1. Deterministic edge tables for every operation:
   - `+0`, `-0`
   - min/max subnormal
   - min normal
   - `1`, `-1`
   - max finite
   - `+Inf`, `-Inf`
   - quiet NaNs
   - signaling NaNs
   - invalid f32 NaN-boxed values
   - known invalid combinations like `Inf - Inf`, `0 * Inf`, `0/0`, `sqrt(-1)`

2. Generated corpus from `testfloat_gen`:
   - Use `-level 1 -seed 1` for normal oracle tests.
   - Keep `level 2` for an explicit longer target later.
   - Generate per rounding mode with `-rnear_even`, `-rminMag`, `-rmin`, `-rmax`, `-rnear_maxMag`.

**Expected Early Finding**

The design should include RMM (`rm=4`) from the start. Current `cpu.go` delegates many FP operations to host rounding via `internal/fenv`, and `fenv.SetRoundingMode` does not appear to support RISC-V RMM. So the SoftFloat tests will probably expose real failures there. That is good signal, not a harness problem.

**Suggested Make Target Later**

After the test files exist, add:

```make
test-softfloat: softfloat
    GOCPU_VIZJIT_OFF=1 go test -tags softfloat -run 'TestCPU_FPSoftFloat' .
```

That gives us a clean path: `make softfloat`, then run the CPU-vs-SoftFloat oracle suite directly against `cpu.go`.
