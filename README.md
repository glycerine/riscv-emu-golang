riscv-emulator
==============

macOS uses a different RISC-V toolchain from linux.

# Prerequisites (one-time)

brew install riscv64-elf-gcc cmake

# Extract

tar -xzf riscv-emulator.tar.gz
cd riscv

# Unit tests — works immediately

go test -v ./...

# One-time setup (clones libriscv ~400MB, builds static libs, compiles guest ELF)

make bench-setup

# Full benchmark comparison

make bench

# Quick targets (no libriscv needed)

make bench-ours       # our GuestMemory benchmarks only

make test             # unit tests

# benchmark results 2026 April 20

JIT-ed, we are ballpark of libriscv now. Sometimes a little slower
slower, sometimes faster. We are not measuring system call latency.
This is a pure compute benchmarks at the moment.

* linux: make bench

~~~
$ make bench

══════════════════════════════════════════════════════════════════
  JIT ALLOCATOR COMPARISON — 2026-04-20 16:29  [linux]
  cpu: AMD Ryzen Threadripper 3960X 24-Core Processor
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go interpreter (no JIT):                     407.8 MIPS
  Go JIT — ELS allocator (native):           4189 MIPS
  Go JIT — Fixed Static Mapping (native):    3987 MIPS
  Go JIT — TCC backend:                      2215 MIPS
  libriscv — JIT (TCC):                      2960 MIPS
  libriscv — interpreter (no JIT):           843.6 MIPS
  native x86-64 (-O3 -march=native):           22954 MIPS  (110.0 ms)
~~~

# darwin: make bench

~~~
$ make bench

══════════════════════════════════════════════════════════════════
  JIT ALLOCATOR COMPARISON — 2026-04-20 18:29  [macos]
  cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go interpreter (no JIT):                     430.5 MIPS
  Go JIT — ELS allocator (native):           3316 MIPS
  Go JIT — Fixed Static Mapping (native):    3357 MIPS
  Go JIT — TCC backend:                      1966 MIPS
  libriscv — JIT (TCC):                      3562 MIPS
  libriscv — interpreter (no JIT):           569.8 MIPS
  native x86-64 (-O3 -march=native):           18035 MIPS  (140.0 ms)
~~~
