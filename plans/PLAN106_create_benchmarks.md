# Plan: AOT vs Lazy JIT Benchmarks for Profiling

## Context

AOT is 18-148x slower than lazy JIT on the RISC-V test ELFs, which is unexpected. We need reproducible benchmarks across four workloads so we can profile both paths and find the bottleneck.

## File: `bench/jit_aot_bench_test.go`

The file already has `benchJITAOTELF` and three AOT benchmarks (CoreMark, Dhrystone, BenchGuest). Rename existing `benchJITAOTELF` → `benchAotJITELF` and `BenchmarkJITAOT_*` → `BenchmarkAotJIT_*`, then add:

### 1. Lazy JIT helper (mirrors `benchAotJITELF`)

```go
func benchLazyJITELF(b *testing.B, elfData []byte) {
    // Same as benchAotJITELF but:
    // - No InstallAOT call
    // - jit.DisableAutoAOT = true
}
```

### 2. Lazy benchmarks for existing workloads

```go
func BenchmarkLazyJIT_CoreMark(b *testing.B)   { benchLazyJITELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf")) }
func BenchmarkLazyJIT_Dhrystone(b *testing.B)  { benchLazyJITELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf")) }
func BenchmarkLazyJIT_BenchGuest(b *testing.B) { benchLazyJITELF(b, loadCPUELF(b)) }
```

### 3. RISC-V test ELF helper

The riscv-tests need different setup than bench ELFs (Size1MB, watchAddr, RiscvTestsEcall). Add a helper:

```go
func newRVTestCPU(tb testing.TB, elfData []byte) (*riscv.CPU, *riscv.GuestMemory) {
    mem := NewGuestMemory(Size1MB)
    elf := LoadELFBytes(mem, elfData)
    cpu := NewCPU(*mem)
    cpu.SetPC(elf.Entry)
    cpu.SetWatchAddr(elf.TohostAddr)
    // OS: exit(93,94) + RiscvTestsEcall
}
```

And a runner that uses `runJITBenchGuestWith` (already exists, calls `jit.RunJIT`).

### 4. RISC-V test suite benchmarks (all UI ELFs per iteration)

Run all `rv64ui-p-*` ELFs sequentially as one benchmark iteration — matches the test suite and gives meaningful total timing:

```go
func benchRVTestSuite_AotJIT(b *testing.B)  { /* for each ELF: newRVTestCPU + InstallAOT + RunJIT */ }
func benchRVTestSuite_LazyJIT(b *testing.B) { /* for each ELF: newRVTestCPU + DisableAutoAOT + RunJIT */ }

func BenchmarkRVTests_UI_AotJIT(b *testing.B)  { benchRVTestSuite_AotJIT(b) }
func BenchmarkRVTests_UI_LazyJIT(b *testing.B) { benchRVTestSuite_LazyJIT(b) }
```

### Summary of benchmarks

| Benchmark | Workload | JIT mode |
|-----------|----------|----------|
| `BenchmarkAotJIT_CoreMark` | CoreMark | AOT (existing, renamed) |
| `BenchmarkLazyJIT_CoreMark` | CoreMark | Lazy (new) |
| `BenchmarkAotJIT_Dhrystone` | Dhrystone | AOT (existing, renamed) |
| `BenchmarkLazyJIT_Dhrystone` | Dhrystone | Lazy (new) |
| `BenchmarkAotJIT_BenchGuest` | fib/sieve | AOT (existing, renamed) |
| `BenchmarkLazyJIT_BenchGuest` | fib/sieve | Lazy (new) |
| `BenchmarkRVTests_UI_AotJIT` | all rv64ui ELFs | AOT (new) |
| `BenchmarkRVTests_UI_LazyJIT` | all rv64ui ELFs | Lazy (new) |

## Verification

```bash
cd ~/ris/bench && go test -run='^$' -bench='Benchmark(AotJIT|LazyJIT)_BenchGuest' -benchtime=1x
cd ~/ris/bench && go test -run='^$' -bench='BenchmarkRVTests_UI' -benchtime=1x
```

## Critical Files

| File | Change |
|------|--------|
| `bench/jit_aot_bench_test.go` | Add lazy helper, 3 lazy benchmarks, RV test helpers, 2 RV test benchmarks |
