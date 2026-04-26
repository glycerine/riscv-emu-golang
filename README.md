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

latest

~~~
Profile-driven optimization pass is complete. Summary:

Results (benchstat, 20 samples, same machine state):

┌─────────────┬───────────────┬────────────┬────────┬───────┐
│    bench    │ Phase 2b base │ Opt 1+2+3  │   Δ    │   p   │
├─────────────┼───────────────┼────────────┼────────┼───────┤
│ coremark    │    886.4 MIPS │ 938.1 MIPS │ +5.84% │ 0.001 │
├─────────────┼───────────────┼────────────┼────────┼───────┤
│ dhrystone   │         759.2 │      785.5 │ +3.46% │ 0.000 │
├─────────────┼───────────────┼────────────┼────────┼───────┤
│ bench_guest │          3233 │       3264 │  +1.0% │ 0.180 │
└─────────────┴───────────────┴────────────┴────────┴───────┘

What landed (user committed as ddaed1c + f5ec0ee):
- Opt 1 soleSegment fast-path: skip findSegment + blk.segment null-chain when len(aotSegments)==1
- Opt 2 hygiene: drop the redundant defensive findSegment inside nextExecuteSegment
- Opt 3 pre-computed vaddrSize on DecodedExecuteSegment — removes vaddrEnd-vaddrBegin subtraction per CallAOT

ExecRegion finesse (your question): The check is only on block-cache miss, not the steady-state hot path — so for coremark's tight inner loop it contributes ~0 MIPS. Opt 2 still cleaned up the redundant re-scan inside nextExecuteSegment.

Regression sweep: root tests / ir / bench / chain-ref all green. fuzz-oracle / fuzz-rvc / fuzz-amo / fuzz-bitmanip 60s each — all PASS. fuzz-fd fails on a pre-existing NaN-payload seed corpus entry unrelated to this work (verified by running against HEAD~3; same failure).

Coremark reached 938 MIPS — within 0.2% of the ≥940 target and within 2.2% of Phase 2a's 959. Bonus win on dhrystone.


~/ris (master) $ go test -v -run=xxx -bench='^BenchmarkJITAOT_' -benchtime=1x ./bench/
goos: darwin
goarch: amd64
pkg: riscv/bench
cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
BenchmarkJITAOT_CoreMark
BenchmarkJITAOT_CoreMark-8     	       1	 379747218 ns/op	      1026 MIPS	 1839696 B/op	    1129 allocs/op
BenchmarkJITAOT_Dhrystone
BenchmarkJITAOT_Dhrystone-8    	       1	 340511799 ns/op	       828.2 MIPS	  210296 B/op	      55 allocs/op
BenchmarkJITAOT_BenchGuest
BenchmarkJITAOT_BenchGuest-8   	       1	 750860305 ns/op	      3363 MIPS	   15016 B/op	      10 allocs/op
PASS
ok  	riscv/bench	1.621s

~~~

# libriscv assembly dumps to debug_libriscv_dir

~~~
The libriscv dump facility is implemented and working end-to-end.

  What landed:

  - New files: xendor/libriscv/lib/libriscv/tr_dump.{hpp,cpp} — env-gated diagnostic dumper
  - Edited: tr_translate.cpp (two hooks), lib/CMakeLists.txt, bench/hellobench/main.go, jit_vizjit.go
  (exported GetVizJitTag), .gitignore

  Output format — mirrors GoCPU's VizJit, one file per block:
  ~/ris/debug_libriscv_dir/<tag>.libriscv.asm.pc_0x<basepc:08x>.asm
  with sections:
  1. Header (run tag, entry PC, byte range, symbol)
  2. == Guest RISC-V == — raw hex per instruction
  3. == libriscv bintr C == — the f_<pc> function body extracted from the generated C
  4. == Host x86-64 (from TCC) == — Intel-syntax disassembly via llvm-mc, trimmed at the last ret

  Usage — single-switch activation:
  GOCPU_VIZJIT=~/ris/debug_vizjit_dir \
    go run -tags libriscv ./bench/hellobench/ -only=libriscv
  hellobench auto-sets LIBRISCV_DUMP_DIR to a sibling path and propagates GoCPU's 16-hex run tag, so
  diff ~/ris/debug_vizjit_dir/<tag>.gocpu.asm.pc_<X>.asm
  ~/ris/debug_libriscv_dir/<tag>.libriscv.asm.pc_<X>.asm just works.

  Zero cost when LIBRISCV_DUMP_DIR is unset — TestLibriscvSmokeTest still passes unchanged.
~~~

wedged. rolled back. on rv8inspired branch now, a bit slower
after some of this. but tests were faster for a while; top level was 25 sec,
now back to 67s.
  
~~~
with cgo, on darwin now:

ok  	riscv	119.905s  (about 2x slow down from pre-CGO and smaller memory sizes)
ok  	riscv/ir	0.323s
ok  	riscv/bench	18.235s

darwin benchmarks:

  Go JIT — Fixed Static Mapping (native):    3172 MIPS
  Go interpreter (no JIT):                    427 MIPS

linux:  cpu: AMD Ryzen Threadripper 3960X 24-Core Processor

  Go JIT — Fixed Static Mapping (native):    3394 MIPS
  Go interpreter (no JIT):                     391.6 MIPS

still on default rv8, about to compare to ABJIT

  cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go JIT — Fixed Static Mapping (native):    3415 MIPS
  Go interpreter (no JIT):                     461.7 MIPS
  libriscv — JIT (TCC):                      3842 MIPS
  libriscv — interpreter (no JIT):           902.0 MIPS
  native x86-64 (-O3 -march=native):           18035 MIPS  (140.0 ms)
  wazero wasm aot-and-run                    8628.9 MIPS

~~~

### performance note

the main client caller should do

`runtime.LockOSThread()` before invoking the emulator.
This pins the goroutine to one thread and thus avoids
re-scheduling and keep caches hot.

also scheduler affinity.

~~~
make bench

darwin

  Go JIT — rv8 Fixed Static Mapping (native): 3462 MIPS
  Go JIT — abjit (native):                    3367 MIPS
  Go interpreter (no JIT):                     464.7 MIPS

linux
cpu: AMD Ryzen Threadripper 3960X 24-Core Processor

  Go JIT — rv8 Fixed Static Mapping (native): 3218 MIPS
  Go JIT — abjit (native):                    3348 MIPS
  Go interpreter (no JIT):                     378.8 MIPS
  libriscv — JIT (TCC):                       3409 MIPS
  libriscv — interpreter (no JIT):            795.8 MIPS
  native x86-64 (-O3 -march=native):         21041 MIPS  (120.0 ms)
  wazero wasm aot-and-run                    18384.9 MIPS
~~~

~~~
make bench-cpu

goos: darwin
goarch: amd64
pkg: riscv/bench
cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz

BenchmarkCPU_FullExecution_JIT_Rv8-8     	       1	 837820510 ns/op	      3014 MIPS	 2369224 B/op	   17590 allocs/op
BenchmarkCPU_FullExecution_JIT_ABJIT-8   	       1	 829903924 ns/op	      3042 MIPS	 2356184 B/op	   17311 allocs/op

goos: linux
goarch: amd64
pkg: riscv/bench
cpu: AMD Ryzen Threadripper 3960X 24-Core Processor 

BenchmarkCPU_FullExecution_JIT_Rv8-48                  1         985023960 ns/op
              2563 MIPS  2389736 B/op      17726 allocs/op
BenchmarkCPU_FullExecution_JIT_ABJIT-48                1         843590039 ns/op
              2993 MIPS  2374776 B/op      17441 allocs/op
~~~

without any fake IC 

~~~
for 2524935201 riscv instuctions:

darwin 9399 MIPS (269 msec) 
Linux  7129 MIPS (354 msec)
~~~
