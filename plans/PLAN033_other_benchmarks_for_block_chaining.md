# Add JIT CoreMark/Dhrystone benchmarks with chain-counter reporting

## Context

Block chaining is wired and measured on `bench_guest.elf` (via
`TestJIT_ChainReference` in `bench/jit_chain_reference_test.go`), but
there is no JIT benchmark for CoreMark or Dhrystone — only interpreter
variants (`BenchmarkCPU_CoreMark`, `BenchmarkCPU_Dhrystone`). We want
standard-benchmark numbers under the production path (Fixed Static
Mapping JIT) and per-workload chain counters, so we can tell whether a
different workload shape changes how often chaining fires. If chaining
helps `bench_guest` little but helps CoreMark/Dhrystone a lot (or vice
versa), that is itself diagnostic.

### Why this plan is motivated by recent data

One reference run on `bench_guest.elf` (Fixed, post-chaining):

```
  retired insns     : 2,524,935,201
  DispatchOK        :       615,845
  ChainPatched      :         2,071
  insns/DispatchOK  :       4,100.0
  MIPS              :       3,378.6
```

`insns/DispatchOK = 4100` ≈ `MaxIC = 4096` (GC safepoint budget). Almost
every jitOK return is a BudgetCheck round-trip, not a chain-eligible
exit. Chaining is working (2,071 patches eliminated back-edges) — but
BudgetCheck is the dominant exit on this workload. Adding
CoreMark/Dhrystone tells us whether 4100 is universal or bench_guest
shape-specific, which affects whether to invest in raising `MaxIC` /
chaining across safepoint polls vs. moving to IC-per-insn batching or
regalloc work.

Both guest ELFs already exist and already exit via ecall-93 (same
syscall set `newBenchCPU` installs: 93, 94, 96, 214). No guest-side
changes are required — this is purely new test/bench wiring.

## What already exists we can reuse

- `bench/cpu_bench_test.go:43` `newBenchCPU(tb, elfData)` — shared guest
  bring-up for any ELF. Already used by CoreMark/Dhrystone interp tests.
- `bench/cpu_bench_test.go:231` `loadELFFrom(tb, envVar, defaultPath)` —
  already loads `coremark.elf` / `dhrystone.elf` with env override
  (`CM_ELF` / `DHRY_ELF`).
- `bench/cpu_bench_test.go:67` `runJITBenchGuestWith(cpu, jit)` —
  generic: takes any CPU + JIT, returns `(exitCode, insns)`. Name is a
  misnomer (it's not specific to `bench_guest`) — rename in Part 4.
- `bench/jit_bench_test.go:74` `benchJITWith(b, strategy)` — helper for
  JIT benchmarks with a given alloc strategy. Currently hardcoded to
  `loadCPUELF` (bench_guest). We generalize in Part 1.
- `jit.ChainPatched`, `jit.DispatchOK`, `jit.DispatchOther`,
  `jit.DispatchInterp`, `jit.DispatchCompile` — already populated; read
  after each run.

## Part 1 — Generalize the JIT bench helper

In `bench/jit_bench_test.go`, factor `benchJITWith` so it accepts an ELF
loader closure (or the ELF bytes directly). Signature target:

```go
func benchJITELF(b *testing.B, elfData []byte, strategy string) {
    // unchanged body: for i:=0..b.N { newBenchCPU; NewJIT; SetAllocStrategy;
    //                 runJITBenchGuestWith; accumulate insns }
    // report MIPS as today
}
```

Keep `benchJITWith(b, strategy)` as a thin wrapper that loads
`bench_guest.elf` and forwards to `benchJITELF`, so existing benchmarks
(`BenchmarkCPU_FullExecution_JIT`, `..._JIT_Fixed`) are unchanged.

## Part 2 — Add JIT CoreMark/Dhrystone benchmarks

Append to `bench/jit_bench_test.go`:

```go
func BenchmarkJIT_CoreMark_Fixed(b *testing.B) {
    benchJITELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf"), "fixed")
}

func BenchmarkJIT_CoreMark_ELS(b *testing.B) {
    benchJITELF(b, loadELFFrom(b, "CM_ELF", "coremark.elf"), "els")
}

func BenchmarkJIT_Dhrystone_Fixed(b *testing.B) {
    benchJITELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf"), "fixed")
}

func BenchmarkJIT_Dhrystone_ELS(b *testing.B) {
    benchJITELF(b, loadELFFrom(b, "DHRY_ELF", "dhrystone.elf"), "els")
}
```

`loadELFFrom` already skips (not fails) if the ELF is missing — so
running `go test ./bench/...` on a fresh checkout without
`make coremark-elf` / `make dhrystone-elf` does not break.

Fixed Static Mapping is the production path and the comparison point;
ELS is kept so we can detect if one strategy benefits more from
chaining on these workloads.

## Part 3 — Generalize the chain reference harness

Today `bench/jit_chain_reference_test.go:27` hardcodes `bench_guest`.
Refactor the body into a shared helper:

```go
func runChainReference(t *testing.T, elfData []byte, workload string) {
    cpu, mem := newBenchCPU(t, elfData)
    defer mem.Free()
    jit := riscv.NewJIT()
    jit.SetAllocStrategy("fixed")
    t0 := time.Now()
    exitCode, insns := runJITBenchGuestWith(cpu, jit)
    elapsed := time.Since(t0)
    if exitCode != 0 { t.Fatalf("%s exited %d", workload, exitCode) }
    // ... existing logging, prefixed with workload name
}
```

Then:

```go
func TestJIT_ChainReference(t *testing.T) {
    runChainReference(t, loadCPUELF(t), "bench_guest")
}
func TestJIT_CoreMark_ChainReference(t *testing.T) {
    runChainReference(t, loadELFFrom(t, "CM_ELF", "coremark.elf"), "coremark")
}
func TestJIT_Dhrystone_ChainReference(t *testing.T) {
    runChainReference(t, loadELFFrom(t, "DHRY_ELF", "dhrystone.elf"), "dhrystone")
}
```

Keep the log format identical across workloads so three `-v` runs can be
eyeballed side-by-side. The `workload` string goes in the header line
only: `─── Chain reference (coremark, Fixed Static Mapping) ───`.

## Part 4 — Rename `runJITBenchGuestWith` → `runJITWith`

Not required for correctness. The name is misleading now that
`coremark.elf` and `dhrystone.elf` go through the same helper. Rename
in-place via a single search/replace across `bench/`. The function
already takes an arbitrary CPU; there was nothing bench-guest-specific
about it. One-liner — defer to a follow-up commit if it complicates
review.

## Part 5 — Makefile targets

Add to `Makefile` near line 435 (next to `bench-coremark`):

```make
bench-jit-coremark: coremark-elf
	@echo "── CoreMark (JIT, Fixed vs ELS) ────────────────────────────────"
	@cd $(ROOT) && CM_ELF=$(CM_ELF) \
	    $(GO) test $(GOTAGS) -count=1 -benchtime=3x \
	        -run='^$$' -bench='^BenchmarkJIT_CoreMark' \
	        ./bench/

bench-jit-dhrystone: dhrystone-elf
	@echo "── Dhrystone (JIT, Fixed vs ELS) ───────────────────────────────"
	@cd $(ROOT) && DHRY_ELF=$(DHRY_ELF) \
	    $(GO) test $(GOTAGS) -count=1 -benchtime=3x \
	        -run='^$$' -bench='^BenchmarkJIT_Dhrystone' \
	        ./bench/

bench-chain-ref: coremark-elf dhrystone-elf
	@echo "── Chain-counter reference (all workloads, Fixed) ──────────────"
	@cd $(ROOT) && CM_ELF=$(CM_ELF) DHRY_ELF=$(DHRY_ELF) \
	    $(GO) test $(GOTAGS) -count=1 -v \
	        -run='^TestJIT_(ChainReference|CoreMark_ChainReference|Dhrystone_ChainReference)$$' \
	        ./bench/
```

Add each to `.PHONY` near line 29 and include a help line near line
172 (`bench-jit-coremark`, `bench-jit-dhrystone`, `bench-chain-ref`).

## Files to touch

- **Modify:**
  - `bench/jit_bench_test.go` — new helper `benchJITELF`, four new
    `BenchmarkJIT_{CoreMark,Dhrystone}_{Fixed,ELS}`. Keep existing
    `BenchmarkCPU_FullExecution_JIT*` names stable.
  - `bench/jit_chain_reference_test.go` — refactor body to
    `runChainReference`; add two new test functions.
  - `Makefile` — three new targets and help strings; `.PHONY` update.
- **No changes:** `jit.go`, `jit_emit_ir.go`, anything outside `bench/`
  and `Makefile`.

## Verification

1. `go test -run TestJIT_.*ChainReference -v ./bench/` — all three tests
   pass, each prints `ChainPatched`, `DispatchOK`, `insns/DispatchOK`,
   `MIPS`. This is the primary diagnostic output.
2. `go test -run='^$' -bench='^BenchmarkJIT_' -benchtime=3x ./bench/` —
   six JIT benchmarks run (2 bench_guest + 2 CoreMark + 2 Dhrystone),
   each reports MIPS.
3. `make bench-chain-ref` — runs all three chain-reference tests.
4. `make bench-jit-coremark` and `make bench-jit-dhrystone` — each
   produces a MIPS number without requiring `make bench-setup`
   (libriscv).
5. `go test ./...` — no regressions; existing CoreMark/Dhrystone interp
   benchmarks and `TestJIT_BenchGuest_Smoke` unchanged.

## Expected outputs and what they would tell us

For each of the three workloads we will get (from Part 3):

```
─── Chain reference (coremark, Fixed Static Mapping) ───
  retired insns     : N
  DispatchOK        : D
  ChainPatched      : C
  insns/DispatchOK  : R = N/D
  MIPS              : M
```

Interpretation tree (matches what we'd look at in the diagnostic
question that prompted this plan):

- `R < ~50` → chaining is not firing for this workload's hot loop.
  Candidates: backward branches go through JALR (dynamic), or the hot
  block is falling out via a non-chainable exit (BudgetCheck, fcsr
  read, interp fallback).
- `R > ~1000` → chaining fires heavily. If MIPS is still far below
  native, the bottleneck is elsewhere (IC-per-insn stores, regalloc
  spills, per-insn `BudgetCheck`).
- Big spread of R across workloads → we have a workload-shape reason
  for the small bench_guest delta; next plan should target whichever
  shape dominates real use.

## Non-goals

- No guest-side changes (CoreMark / Dhrystone sources untouched).
- No JIT emitter / chain infrastructure changes.
- No libriscv comparison benchmarks (those live under `bench/libriscv/`
  and require the `libriscv` build tag / `make bench-setup`).
- No changes to the existing interpreter benchmarks
  (`BenchmarkCPU_CoreMark`, `BenchmarkCPU_Dhrystone`).

## Risks

- **CoreMark ITERATIONS**: whatever value was baked into the ELF at
  build time controls total insns. A very small setting (e.g., 1) would
  leave chaining cold and R artificially low. Check by inspecting the
  `retired insns` line — if it's under ~1e7, rebuild
  `bench/coremark.elf` with a larger ITERATIONS.
- **Dhrystone default runs**: same concern. The vendored main runs
  `Number_Of_Runs` as set; confirm that the value is high enough to
  amortize startup.
- **FP in CoreMark final score computation**: briefly touches floats.
  With F/D + fcsr in the sret buffer, this is already supported — but
  if we trip an interp fallback on any FP path, it'll show as
  `DispatchInterp > 0`. Not a correctness issue, just noise in the
  chaining ratio.

## Recommended execution order

1. Part 1 — refactor helper. Run existing benchmarks; confirm unchanged
   behavior.
2. Part 3 — refactor chain-reference test + add two new variants. Run
   `go test -run TestJIT_.*ChainReference -v ./bench/` to see numbers.
3. Part 2 — add the four new `BenchmarkJIT_*`. Run `-bench`.
4. Part 5 — Makefile targets.
5. Part 4 — rename (optional, can be a follow-up).
6. Regression: `go test ./...`.
