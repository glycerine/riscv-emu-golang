# Phase F + PGO: Investigate interpreter switch-dispatch cost and apply PGO

## Context

Phases A–C of the interpreter tuning plan at `/Users/jaten/ris/interpreter_performance_tuning.md` are complete. Current baseline from `make bench` on the i7-1068NG7:

```
Go interpreter (no JIT):     430.5 MIPS   (bench_guest.elf: fib + memstress + sieve)
```

Phase F (verbatim from the plan): *"Only if, after A+B+C, `pprof -list RunCached` shows switch dispatch itself as a top cost and reordering cases by observed frequency demonstrably helps. Skip otherwise."*

Two reasons the literal Phase F may not move the needle at 430.5 MIPS:

1. **Go's switch lowering.** Go 1.21+ (we use 1.25.6 per `go.mod`) lowers reasonably dense integer switches to jump tables. `slot.op` is `uint8` (`decoder_cache.go:16`) with ~30 values in [0, 0x9C]. If the top-level switch at `run_cached.go:83` is a jump table, case order is irrelevant.
2. **We're already at the plan's stated ~400–500 MIPS Go ceiling.** Libriscv's threaded-dispatch advantage (~1.3–1.7×) is not reproducible in portable Go.

Per user direction, this plan:
- **Keeps Phase F's diagnose-first spirit** (measure before touching).
- **Adds Go PGO as a parallel track** (the modern equivalent of what F proposed — let the compiler do frequency-driven case reordering, BB layout, and inlining from a real profile).
- **Pivots to PGO-only if the switch itself isn't hot** (user's direction when asked about the no-go path).

## Outcome we want

A reproducible number: does **Phase F (manual)** and/or **PGO** demonstrably move `BenchmarkCPU_FullExecution_Cached` above 430.5 MIPS on the baseline machine, with no regression to CoreMark/Dhrystone, the JIT, or the test suites?

The investigation itself (step 1) is valuable even if both results are null, because it updates the plan document's assumptions for future work.

---

## Step 1 — Diagnose (read-only)

All read-only. No source edits. Expected to take ~1 hour.

### 1a. Flat-time breakdown inside `RunCached`

```bash
go test -run=xxx -bench='^BenchmarkCPU_FullExecution_Cached$' \
    -benchtime=10s -count=3 -cpuprofile=/tmp/rc.prof ./bench/
go tool pprof -list 'RunCached' /tmp/rc.prof > /tmp/rc.list
go tool pprof -top -cum /tmp/rc.prof | head -40
```

Record:
- Flat % on the `switch slot.op` header (`run_cached.go:83`) and on the first few cycles of each case block.
- Combined flat % on the sub-switches at `run_cached.go:283` (OP, funct7) and `run_cached.go:390` (OP-32, funct7) — these are the sparse ones most likely to be compare-chains.
- Top-5 functions by flat and by cum.

### 1b. Inspect generated assembly

```bash
go test -c -o /tmp/bench.test ./bench/
go tool objdump -s 'riscv\.RunCached' /tmp/bench.test > /tmp/rc.s
```

Identify the lowering of each switch:
- **Jump table**: bounds-check + `LEAQ <table>` + `JMP *AX/…` (indirect jump). Order-independent.
- **Compare chain / binary search**: cascade of `CMPQ …, imm ; JE label`. Order-sensitive.

Expected observations (to be confirmed):
- `switch slot.op` (30 cases, range ~0x9D): likely jump table.
- `switch slot.funct3` (6–8 dense cases in 0..7): jump table.
- `switch slot.funct7` in cases 0x33 / 0x3B (sparse: {0x00, 0x01, 0x20, …}): likely compare chain.

### 1c. Opcode-frequency check

Use pprof sample counts per `case 0xNN:` line as the first-pass frequency signal (it's free — samples under each arm approximate execution frequency × body cost). If pprof is ambiguous, add a temporary `[256]uint64` counter next to `slot.op` read at `run_cached.go:83`, dump at benchmark end, revert before merge.

### 1d. Codegen-sensitivity snapshot

Capture the current `objdump` of `RunCached` as a reference. The comment block at `run_cached.go:29-62` documents a 25% regression from cross-file code motion; any subsequent change must be diffed against this snapshot.

---

## Step 2 — Go/No-go gate for Phase F (manual reshuffle)

**Proceed with manual reshuffle only if ALL hold:**
- `objdump` shows at least one top-level or sub-switch lowered to a compare chain, not a jump table.
- That chain has a clear frequency skew in the pprof data (one or two cases carrying >60% of samples in the arm).
- Combined flat % attributable to dispatch selection in compare-chain switches is ≥ 3%.

**Otherwise: skip Phase F manual.** Document the finding in a short note and move on — Step 3 (PGO) runs regardless.

---

## Step 3 — Apply PGO (unconditional)

Runs regardless of the Step 2 outcome.

### 3a. Capture a PGO profile

```bash
# Capture on the real workload, longer sample for stability.
go test -run=xxx -bench='^BenchmarkCPU_FullExecution_Cached$' \
    -benchtime=20s -cpuprofile=./bench/default.pgo ./bench/
```

Place the profile at `bench/default.pgo` so `go test ./bench/` picks it up automatically (Go 1.21+ convention: a `default.pgo` next to the package's main / test entry).

### 3b. Rebuild and measure

```bash
# Verify PGO is being used (should see "-pgo=default.pgo" in the action graph).
go test -run=xxx -bench='^BenchmarkCPU_FullExecution_Cached$' \
    -benchtime=10s -count=10 -cpuprofile=/tmp/rc.pgo.prof ./bench/ \
    -x 2>&1 | grep -i pgo | head -5

# Full measurement.
go test -run=xxx -bench='^BenchmarkCPU_FullExecution_Cached$' \
    -benchtime=10s -count=10 ./bench/ | tee /tmp/pgo.bench
```

Compare MIPS median+IQR against the pre-PGO baseline captured at Step 1.

### 3c. Inspect PGO-produced assembly

Rerun `objdump` on the PGO-built binary. Note any BB reordering in `RunCached` (hot arms promoted to fall-through, cold arms demoted). This tells us whether PGO is actually doing useful work here or just inlining differently elsewhere.

---

## Step 4 — Implement manual reshuffle (conditional — only if Step 2 green)

For each compare-chain switch identified in 1b with a clear frequency skew in 1c:

- Reorder its `case` clauses so the highest-frequency case appears first, descending.
- Leave the default case at the end.
- Re-run Step 1b `objdump` after each edit to confirm the chain order flipped and no jump-table regression was triggered.

Candidate switches (subject to diagnosis):
- `switch slot.funct7` at `run_cached.go:283` (OP / R-type, 0x33) — if it's a compare chain, hottest `funct7` on integer workloads will be 0x00; may already be first.
- `switch slot.funct7` at `run_cached.go:390` (OP-32 / R-type, 0x3B) — same pattern.
- Any other chain discovered at 1b.

**Stop condition:** if any reorder fails to move the median MIPS by ≥ 1% (on `-count=10`) or shifts register allocation in a way that reduces downstream cases, revert and move on.

---

## Step 5 — Verification

Run after every code change (Step 4) and after PGO rebuild (Step 3):

1. `go test ./...` — unit tests (CPU, mem, ELF, OS, JIT, RVC, AMO, FP).
2. `go test ./fuzzoracle` — oracle fuzzing vs libriscv (requires `make bench-setup` to have run).
3. `go test -run=. ./riscv-elf-tests/...` — official RISC-V test suite.
4. `make bench-cpu` — MIPS on bench_guest.elf (primary).
5. `make bench-coremark` and `make bench-dhrystone` — confirm no regression on workloads we didn't profile with.
6. `make bench` — full comparison incl. JIT MIPS (±2% tolerance on the JIT numbers).
7. Cycle-count bit-exactness: `cpu.Cycle()` on bench_guest.elf must match the pre-change count (plan §Verification.7).
8. pprof delta: `go tool pprof -list RunCached` before vs after, confirm the targeted switch flat% actually dropped.

---

## Measurement protocol (non-negotiable)

- `-count=10` minimum on `BenchmarkCPU_FullExecution_Cached`. Discard min and max, report median and IQR. One-run bench numbers are meaningless for expected 1–4% deltas.
- Always measure through the `RunDefault` entry point (which `BenchmarkCPU_FullExecution` uses via `cpu.Run()`). Micro-benches lie; see `run_cached.go:29-62`.
- Keep a snapshot `objdump` of `RunCached` before any change. Any register-allocation drift in the hot loop is a yellow flag independent of the measured MIPS.
- Machine conditions: same CPU governor, same thermal state, background quiet. The baseline 430.5 MIPS was captured on `i7-1068NG7 @ 2.30GHz` / macOS — match that.

---

## Critical files

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/run_cached.go` — megaswitch at line 83; OP funct7 switch ~line 283; OP-32 funct7 switch ~line 390. The only file Step 4 will modify.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/decoder_cache.go` — `DecodedInsn.op uint8` layout confirms uint8 range for lowering analysis.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/decode.go:72-100` — `opC_*` synthetic constants (0x80..0x9C).
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/bench/cpu_bench_test.go` — `BenchmarkCPU_FullExecution_Cached` at lines ~183–203; used for all measurements.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/bench/default.pgo` — to be created in Step 3a. Not a source file; may want to gitignore.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/Makefile` — existing `prof`, `bench-cpu`, `bench-coremark`, `bench-dhrystone`, `bench` targets used during verification.

---

## Expected outcome matrix

| Scenario | Likelihood | Action | Expected Δ MIPS |
|---|---|---|---|
| Main `switch slot.op` is a jump table, sub-switches also jump tables, dispatch flat% low | High | Skip Step 4; PGO still runs | PGO: +1 to +4% |
| Sub-switches are compare-chains with skew; manual reorder helps | Medium | Run Step 4 reorder + PGO | Manual: +0.5 to +2% · PGO: +1 to +4% (overlap) |
| Switch dispatch dominates AND is a compare chain at top level | Low | Run Step 4 aggressive reorder + PGO | Manual: +3 to +8% · PGO: +2 to +5% |
| Nothing helps (already at ceiling) | Possible | Document findings; plan update | 0% |

Honest read: I expect Step 2 to return "skip" and PGO to yield ~1–3% on bench_guest. That would bring us to ~440–445 MIPS — modest but within reach at no code-complexity cost.

---

## Non-goals

- Do not touch Phase D (memory unsafe fast path) or Phase E (pre-resolved branch targets) as part of this work. They remain separate open items on the original plan.
- Do not touch the JIT path. The JIT does not go through `RunCached`.
- Do not pursue computed-goto / threaded dispatch alternatives in this cycle. Those require a different plan.
