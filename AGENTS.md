# AGENTS.md

## Read This First

This repository is a performance-oriented RV64 emulator and native JIT, not
just a simple interpreter. The Go module is `riscv`. The root package
implements guest memory, ELF loading, a cached interpreter, a native AMD64 JIT,
an AOT segment system, OS/syscall personalities, and a forkable `Machine`
wrapper. The surrounding directories hold benchmarks, oracle tests, vendored
reference projects, and a local copy of the Go assembler backend.

The current JIT path is native IR -> goasm -> executable mmap. The default
register policy is `PolicyABJIT`; `PolicyRV8` is still available for the older
rv8-style trampoline/register layout. The old C-source/TCC emitter still exists
as legacy/reference code, but it is not the dispatch path in `NewJIT`/`RunJIT`.

Treat comments in hot-path files as part of the design. Several "odd" choices
are load-bearing for correctness or speed: CPU field layout, the `runCached`
call-site restriction, ABJIT/RV8 register conventions, NaN boxing, decoder
cache layout, AOT segment lifetime, and the guest-memory mask invariant.

`plans/` is historical/user archive material. Do not treat it as live guidance
unless the user explicitly asks about a plan. `attic/` is disabled code.
`xendor/`, `riscv-elf-tests/`, and `goasm/` are vendored/reference trees; edit
them only when the task specifically targets them.

## Quick Commands

```bash
# Common doc/code sanity checks
GOCPU_VIZJIT_OFF=1 go test -count=1 .
GOCPU_VIZJIT_OFF=1 go test -count=1 ./bench/

# The Makefile's default unit-test target currently runs bench then root tests
make test

# Focused examples
GOCPU_VIZJIT_OFF=1 go test -count=1 -run 'TestJIT_' .
GOCPU_VIZJIT_OFF=1 go test -count=1 -run TestCPU_BenchGuest_Smoke ./bench/
GOCPU_VIZJIT_OFF=1 go test -count=1 -run TestAOT .

# Bench setup and benchmark families
make bench-setup       # requires cmake, zig, C++ toolchain; uses vendored xendor/libriscv
make bench             # rv8 vs abjit vs interpreter vs libriscv vs native/wasm
make bench-cpu
make bench-coremark
make bench-dhrystone
make bench-jit-coremark
make bench-jit-dhrystone
make bench-chain-ref
make quad              # AOT vs lazy JIT on several workloads

# Fuzzing
make fuzz              # pure-Go CPU/IR fuzz target
make fuzz-oracle       # libriscv oracle; requires make bench-setup
make fuzz-stores
make fuzz-rvc
make fuzz-amo
make fuzz-fd
make fuzz-bitmanip
make fuzz-cfloat
```

`go test ./...` is not the usual first pass: it includes packages such as
`fuzzoracle` that expect the libriscv C API to have been built. Use it only
after `make bench-setup` or when you intentionally want that broader sweep.

Set `GOCPU_VIZJIT_OFF=1` for routine tests. Without it, VizJit may write
per-block assembly dumps under `debug_vizjit_dir`. To inspect generated code,
set `GOCPU_VIZJIT=/path/to/dir` and run a narrow test or benchmark.

## Architecture Map

### Guest State and Interpreter

- `guestmem.go` owns the mmap-backed guest address space. Its critical
  sandbox invariant is `hostPtr = base + (addr & mask)`, with a power-of-two
  memory size. Bounds checks report guest `MemFault`s; the mask prevents host
  escape even if a check is wrong.
- `guestmem_exec.go` tracks executable guest-VA ranges. ELF loading and future
  mmap/mprotect hooks use this metadata to drive AOT segment creation.
- `cpu.go` is the canonical instruction implementation and CPU state. The
  `init` offset assertions are intentional: native lowerers depend on the
  register-file layout.
- `run_cached.go` is the fast cached interpreter. Do not add direct
  `runCached(...)` call sites outside `run_cached.go`; this has measured
  performance effects due to Go compiler code generation.
- `decode.go`, `decoder_cache.go`, and `exec_slot.go` implement predecode and
  slot execution for the cached interpreter.

### Exceptions, ELF, and OS Personality

- `elf.go` loads ELF64 RISC-V executables, records executable regions, and
  discovers the `tohost` symbol used by riscv-tests.
- `note.go` is the Plan 9-style synchronous exception system. Handlers compose
  through `NoteChain`; unrecognized notes should be forwarded.
- `os.go` defines syscall/ecall handling, Linux-style helpers, `RunWithOS`,
  and exit-code behavior.
- `jit_syscall.go` and `internal/syscalls/` provide the direct ECALL fast path.
  Toggle functions affect future block emissions only; already-compiled blocks
  keep the path they were emitted with.

### Native JIT and AOT

- `jit_emit_ir.go`, `emit.go`, `emit_impl.go`, `highlevel.go`, and
  `peephole.go` translate guest instructions into the local IR.
- `ir.go` defines IR operations, VRegs, types, predicates, and VizJit globals.
- `regalloc.go` and `regalloc_fixed.go` implement the current allocation path.
- `lower_amd64.go` selects register policies. `PolicyABJIT` is the default;
  `PolicyRV8` remains available for comparison/debugging.
- `lower_amd64_ops.go`, `lower_amd64_abjit.go`, `lower_amd64_rv8.go`, and
  shared lowerer files convert IR to goasm programs.
- `jit_native.go` assembles goasm output, mmaps executable memory, backpatches
  chain exits/JALR slots, and writes VizJit dumps.
- `jit.go` owns dispatch, lazy block caching, block chaining, AOT install,
  JALR miss handling, preemption, counters, and segment lifetime.
- `aot.go`, `jit_aot.go`, and `jit_segment.go` implement whole-region AOT:
  collect block ranges, compile one native slab per executable segment, build a
  read-only decoder cache, and create dynamic segments on demand.

### Trampolines and Support Packages

- `abjit/` is the default same-goroutine trampoline path. Its `State` layout
  must match the offsets in `lower_amd64_abjit.go`.
- `internal/jitcall/` is the older direct function-pointer trampoline used by
  the RV8 path and AOT-aware calls.
- `internal/fenv/` exposes FP exception flags on amd64; non-amd64 fallbacks
  currently return zero flags.
- `internal/syscalls/` is the assembly syscall dispatcher called from JIT code.
- `goasm/` is a local extraction of Go's assembler backend. Preserve target
  coverage and avoid broad cleanup there.

### Tests, Benchmarks, and Reference Code

- Root `*_test.go` files cover interpreter, memory, ELF, JIT, AOT, lowering,
  lockstep, and RISC-V ELF test execution.
- `bench/` contains throughput benchmarks and guest programs for bench_guest,
  CoreMark, Dhrystone, hello/ecall, wazero comparison, and libriscv calibration.
- `fuzzoracle/` compares against libriscv through its C API; build it with
  `make bench-setup` before running the oracle fuzz targets.
- `bridge/` and `bridge2/` are experiments around CGo/ring-buffer sandbox
  dispatch. They are useful context, but not the main JIT path.
- `xendor/` holds vendored/reference projects such as libriscv, LuaJIT,
  CoreMark, Dhrystone, aabalke_gojit, and guac.

## Maintainer Notes

- Keep changes tightly scoped. This code has many microarchitectural and Go
  compiler-sensitive paths; small-looking edits can move benchmark results a
  lot.
- Preserve `CPU` and `abjit.State` layouts unless you also update every offset
  consumer and add/adjust tests.
- RISC-V `x0` must remain hardwired to zero. Many fast paths use the "write,
  then zero x0" trick to avoid branches.
- RV64F single-precision values are NaN-boxed with upper 32 bits set to all
  ones. Interpreter and JIT behavior must match.
- AOT segments are immutable after install and reference-counted. `Close`
  releases their native-code and decoder-cache mmaps. Segment invalidation has
  documented dangling-reference caveats; do not expand it casually.
- ABJIT must not clobber Go runtime-critical registers such as R14. R15 may be
  reserved for instruction counting; register-pool changes need focused tests
  and benchmarks.
- JIT syscall toggles, direct syscall callbacks, and inline ECALL settings are
  process-level knobs for future emissions. Create a fresh JIT when comparing
  modes.
- For performance-sensitive changes, pair correctness tests with at least one
  relevant benchmark (`make bench`, `make quad`, or a narrow `go test -bench`
  target). Use `GOCPU_VIZJIT` when inspecting generated code.
