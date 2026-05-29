# CLAUDE.md

## Project Overview

Go RISC-V emulator (RV64IMAFDC + Zicsr/Zba/Zbb/Zbs) with a JIT compiler that translates RISC-V assembly to native x86-64, and calls through a zero-CGO-overhead Go assembly trampoline. Module name: `riscv`.

## Build & Test Commands

```bash
# Unit tests (no external deps needed)
go test -v .
go test -v ./...              # includes fuzzoracle (one-time setup: make bench-setup first)

# Run a single test
go test -v -run TestJIT_ADD .

# JIT smoke test via bench package
go test -v -run TestJIT_BenchGuest_Smoke ./bench/

# Full benchmark comparison
make bench-setup              # one-time: clone libriscv (~400MB), build, compile guest ELF
make bench                    # full comparison
make bench-quick              # fast head-to-head (<1s)
make bench-ours               # our benchmarks only (no libriscv needed)
make bench-cpu                # CPU execution throughput (MIPS metric)

# Benchmark single run
go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=1x ./bench/

# Fuzzing (oracle-based, validates against libriscv)
make fuzz-oracle              # ALU instructions
make fuzz-fd                  # floating-point F+D
make fuzz-rvc                 # compressed instructions
make fuzz-amo                 # atomics
make fuzz-bitmanip            # Zba/Zbb/Zbs

# Profiling
make prof                     # CPU profile
make mem                      # memory profile
```

## Architecture

### Execution Pipeline

1. **ELF loading** (`elf.go`): Parses ELF64, loads PT_LOAD segments into guest memory
2. **Interpreter** (`cpu.go`): Switch-based decode/execute, one instruction at a time via `step()`
3. **JIT** (`jit.go` + `jit_emit.go` + `jit_tcc.go`):
   - `emitBlock()`: Scans a region of RISC-V code via BFS (`scanRegion`), emits C source for the whole region
   - `tccCompile()`: CGO call to TCC to compile C → native machine code in memory
   - `jitcall.Call()`: Go assembly trampoline (`internal/jitcall/`) invokes the native block with zero CGO overhead
   - `RunJIT()`: Dispatch loop with last-block cache, falls back to interpreter for untranslatable instructions

### Key Design Decisions

- **GuestMemory** (`guestmem.go`): Power-of-two mmap with mask-based bounds checking. Security invariant: `hostPtr = base + (addr & mask)`. All access is a single branch.
- **NaN-boxing** (`float.go`): Single-precision FP values have upper 32 bits = `0xFFFFFFFF` (differs from libriscv which uses zeros). JIT emission must match this convention.
- **Exception delivery** (`note.go`): Plan 9-inspired NoteChain — stack of handlers, innermost-first. `NoteHandled` / `NoteForward` / `NoteFatal` dispositions.
- **OS personality** (`os.go`): Pluggable syscall table. `RunWithOS()` installs handler on NoteChain.
- **TCC limitations**: No `__int128` (affects MULH emission), no libm (sqrt/sqrtf injected via `tcc_add_symbol`).

### JIT Region Scanning

`emitBlock` uses two-phase approach:
1. `scanRegion()` BFS discovers reachable PCs (up to 2048 PCs, 16KB range)
2. Emitter walks the region sequentially, emitting all instructions
3. Forward branch targets within the region use `goto`; external targets exit the block
4. Bail labels in `finalize()` catch goto targets that weren't emitted (early termination)

### Package Structure

- **Root** (`package riscv`): CPU, memory, JIT, ELF, OS, exception system
- **`internal/jitcall`**: Go assembly trampoline for calling JIT-compiled native blocks
- **`internal/fenv`**: Host FP exception flag access (MXCSR on x86-64)
- **`bench/`**: Benchmarks (CPU throughput, memory ops, JIT). Guest ELF in `bench/libriscv_guest/`
- **`fuzzoracle/`**: Oracle fuzzing against libriscv — requires `make bench-setup`
- **`xendor/tcc`**: Precompiled TinyCC library (libtcc.a)
- **`xendor/libriscv`**: Reference emulator (cloned by `make bench-setup`, ~400MB)
- **`riscv-elf-tests/`**: Official RISC-V test suite ELFs

### Test Organization

- `*_test.go` in root: Unit tests for CPU instructions, memory, ELF, OS, JIT
- `bench/*_bench_test.go`: Throughput benchmarks (report MIPS metric)
- `fuzzoracle/*_test.go`: Oracle tests (`*_oracle_test.go`) and fuzz targets (`fuzz_*_test.go`) — both need libriscv via `make bench-setup`
- `riscv_test.go`: Runs official riscv-tests ELF suite

### CGO Build Tags

- Default build: JIT + TCC (xendor/tcc must be present)
- `libriscv` build tag: Required for `bench/libriscv/`
- `fuzzoracle/` no longer needs any build tag.

### ignore the plans/ sub-folder.

These are user's archives and not for CLAUDE.

### goasm assembler targets

goasm/ is an extraction of the Go assembler backend. All targets
must be preserved.
