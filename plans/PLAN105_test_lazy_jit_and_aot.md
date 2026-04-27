# Plan: Close RISC-V Test ELF Coverage Gap (Lazy JIT + AOT)

## Context

The standard RISC-V test ELFs (rv64ui, rv64um, rv64ua, rv64uc) are run through three modes, but there's a gap:

| Test group | Runner | Mode |
|---|---|---|
| `TestRISCVTests_UI` etc. | `RunWithOS` | **Interpreter** |
| `TestRISCVTests_UI_JIT` etc. | `runJITWithOS` → `RunJIT` | **AOT** (auto-installed by RunJIT:715) |
| `TestRISCVTests_Lockstep_UI` etc. | `StepBlock` loop | **Lazy JIT** (per-block, no RunJIT dispatch) |

**Missing**: Lazy JIT via RunJIT (`DisableAutoAOT=true`). This exercises the full RunJIT dispatch loop with lazy compilation — the 2-slot JALR IC, lazy block cache, and interpreter fallback. The lockstep tests use `StepBlock` (single-step), not `RunJIT`.

## Changes

### 1. Add `runRISCVTestJITLazy` helper (riscv_test.go)

Clone `runRISCVTestJIT` but set `DisableAutoAOT=true`:

```go
func runRISCVTestJITLazy(t *testing.T, elfPath string) {
    // Same as runRISCVTestJIT but:
    jit := NewJIT()
    jit.DisableAutoAOT = true
    // ... install OS, RunJIT, check exit code ...
}
```

### 2. Add `TestRISCVTests_*_JIT_Lazy` test functions (riscv_test.go)

For each active instruction category (UI, UM, UA, UC):

```go
func TestRISCVTests_UI_JIT_Lazy(t *testing.T) { ... runRISCVTestJITLazy ... }
func TestRISCVTests_UM_JIT_Lazy(t *testing.T) { ... }
func TestRISCVTests_UA_JIT_Lazy(t *testing.T) { ... }
func TestRISCVTests_UC_JIT_Lazy(t *testing.T) { ... }
```

UF and UD remain skipped (fflags issue, same as existing `_JIT` tests).

### 3. Rename existing `_JIT` tests for clarity (optional)

The existing `_JIT` tests are actually AOT. Renaming to `_JIT_AOT` makes the coverage matrix self-documenting. This is optional — the user may prefer to keep the names stable.

## Coverage Matrix After Changes

| Test | Interpreter | Lazy JIT (RunJIT) | AOT (RunJIT) | Lockstep (StepBlock) |
|------|:-----------:|:-----------------:|:------------:|:-------------------:|
| UI (integer) | yes | **new** | yes | yes |
| UM (mul/div) | yes | **new** | yes | yes |
| UA (atomics) | yes | **new** | yes | yes |
| UC (compressed) | yes | **new** | yes | yes |
| UF (float) | yes | skip | skip | skip |
| UD (double) | yes | skip | skip | skip |

## Performance Comparison

The lazy tests will naturally be slower than AOT (every JALR goes through 2-slot IC or Go round-trip vs decoder cache). The test log can report timing to quantify the gap without a separate benchmark.

## Verification

```bash
cd ~/ris && go test -v -run 'TestRISCVTests_UI_JIT_Lazy' .
cd ~/ris && go test -v -run 'TestRISCVTests_U._JIT_Lazy' .
cd ~/ris && go test -count=1 .  # full suite, no regressions
```

## Critical Files

| File | Change |
|------|--------|
| `riscv_test.go` | Add `runRISCVTestJITLazy` + 4 new `_JIT_Lazy` test functions |
