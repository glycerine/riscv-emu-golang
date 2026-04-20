# Go Interpreter: Match or Exceed libriscv Interpreter (891 MIPS)

## Context

Our Go RISC-V interpreter runs at ~172 MIPS while libriscv's C++ interpreter runs at ~891 MIPS on the same workload — a 5× gap. We want to close it. The JIT already beats libriscv, so this is interpreter-only work (the JIT path stays untouched).

The gap is explained by concrete, verifiable structural differences between the two interpreters, not a generic "Go is slow" story:

| Area | Our interpreter | libriscv |
|---|---|---|
| Decode | Fresh bit-shift decode every instruction (`cpu.go:84-91`) | Pre-decoded bytecode cache keyed by PC (`decoder_cache.hpp`) |
| Fetch | Double fetch — `Fetch16` probe then `Fetch32` (`cpu.go:68,76`) | Single read from decoder slot |
| Dispatch | Go `switch` on opcode | Computed-goto threaded dispatch (`cpu_dispatch.cpp`) — unavailable in Go, switch is our ceiling |
| Counter / watchAddr | Every instruction (`note.go:239-247`) | Once per block |
| Register x0 | Branch on every SetReg / Reg (`cpu.go:45-46`) | Implicit zero via array layout |
| Memory | `go:nosplit` method call per load/store returning `*MemFault` | Flat arena, inlined bounds check |

Before writing code we will profile both interpreters so the optimization order is driven by measured hot spots, not just hypothesis. Static analysis is strong — but the user wants profile data, and profile data is cheap to collect.

## Goal

Match or exceed libriscv's 891 MIPS on the same benchmark ELF, without breaking: the JIT, the fuzzoracle suite, the riscv-elf-tests suite, RVC / FP / AMO tests, or the GuestMemory security invariant.

## Step 1 — Benchmark harness and workloads

The current `bench_guest.elf` (fib + memstress + sieve) stays as the primary regression signal. In addition, add CoreMark and Dhrystone so we can publish numbers comparable to libriscv's own.

1. **CoreMark (RV64)**. libriscv ships an `examples/coremark/build_and_run.sh` that downloads `coremark-rv32g_b.elf` — RV32, not RV64. We need an RV64 build. Fetch the CoreMark source (`https://github.com/eembc/coremark`) into `bench/coremark/`, add a Makefile target `make coremark-elf` that cross-compiles with `riscv64-unknown-elf-gcc -O2 -march=rv64gc` using the same newlib setup the existing `bench/libriscv_guest/` uses. Also build `xendor/libriscv/examples/coremark/` with `-DRISCV_64I=ON -DRISCV_EXT_C=ON` so libriscv can run the same ELF.
2. **Dhrystone (RV64)**. Not included in libriscv. Add `bench/dhrystone/` with the public Dhrystone 2.1 source (`dhry_1.c`, `dhry_2.c`, `dhry.h`), cross-compile the same way. Both emulators can run the resulting ELF through their standard ECALL/exit hooks.
3. **Benchmark runner**: add `BenchmarkCPU_CoreMark` and `BenchmarkCPU_Dhrystone` in `bench/cpu_bench_test.go` that mirror `BenchmarkCPU_FullExecution` but take `BENCH_ELF` pointing to the new ELFs. Add Makefile targets `bench-coremark` and `bench-dhrystone` that run both our emulator and libriscv for head-to-head MIPS.

## Step 2 — Profile both interpreters

1. **Our Go interpreter (pprof)**.
   - `make prof` already runs `go test -run=xxx -bench=BenchmarkCPU_FullExecution -benchtime=1x -cpuprofile cpu.prof ./bench/` and opens pprof at `:8080` (Makefile:721-723). Run this and capture a text listing:
     - `go tool pprof -list 'step|RunWithChain' cpu.prof > prof_our_step.txt`
     - `go tool pprof -top -cum cpu.prof | head -40 > prof_our_top.txt`
   - Repeat with `-benchtime=10s` for a longer sample so per-opcode lines show up with meaningful counts.
2. **libriscv C++ interpreter**. On macOS use `sample`:
   - Build libriscv's CLI in interpreter-only mode (`-DRISCV_BINARY_TRANSLATION=OFF`, the existing build in `xendor/libriscv/build*` may already be interpreter-only).
   - Run it against the same ELF in a backgrounded process, then `sample <pid> 10 -file prof_libriscv.txt`.
   - Alternatively use `instruments -t "Time Profiler"` for a callgraph.
3. **Write up** `prof_findings.md` (NOT checked in) listing top-10 functions for each interpreter, confirming or revising the pre-implementation ranking below. This file lives in the working tree but is gitignored.

## Step 3 — Optimization phases (ranked by expected ROI)

Each phase runs `make bench-cpu && make bench-coremark && make bench-dhrystone && go test ./...` before moving to the next. If a phase fails to deliver within ±30% of its expected gain, re-profile before continuing.

### Phase A + B — Decoded instruction cache + single fetch
**Expected gain: 172 → ~500 MIPS (≈3×).**

Replace the per-step bit-shift decode with a flat per-PC cache. Each executable PT_LOAD segment gets a `[]DecodedInsn` slab indexed by `(pc - segBase) >> 1`.

```go
type DecodedInsn struct {
    handler    uint8  // opcode class index
    rd, rs1, rs2, rs3 uint8
    funct3, funct7    uint8
    imm        int32
    len        uint8  // 2 for RVC, 4 otherwise
    blockEnd   bool   // set by decoder for branches/JAL/JALR/ECALL/EBREAK/FENCE.I/MRET — used in Phase E
    decoded    bool   // false means first-time execution path decodes and fills
}
```

- New file `decoder_cache.go` holding `DecoderCache`, the slab, and a `Decode(insn uint32) DecodedInsn` function split out from today's `step()` body.
- `cpu.go:65` — rewrite `step()` as `stepCached(slot *DecodedInsn)`; the outer driver does the cache lookup.
- Eliminate the double fetch: replace the `Fetch16` probe + `Fetch32` pair at `cpu.go:68-82` with a single `Fetch32U` (the existing byte-granular fetcher handles the rare end-of-segment case). The low 16 bits tell us RVC vs 32-bit; if RVC, pass the low half into the RVC expander.
- RVC stays in `stepRVC` for correctness, but the *expanded* 32-bit form plus `len: 2` is stored in the cache so dispatch is unified after first execution.
- First execution at a PC populates the slot (`decoded = true`) under the same call site; subsequent visits skip decode entirely.
- Self-modifying code support: leave unimplemented for now (our benchmark workloads never write to .text). If required later, add `InvalidateRange(addr, len)` hooked into any `WriteBytes` that targets the cached region.

Files: `decoder_cache.go` (new), `cpu.go` (step rewrite, each opcode case refactored to read from slot), `note.go` (RunWithChain becomes the cache-driven driver), `elf.go` (feed executable segment base/length into the cache builder).

### Phase E + G — Block batching + inlined driver loop
**Expected gain: 500 → ~800 MIPS (≈1.5×).**

With `blockEnd` already in the slot, the driver can run an inner loop that accumulates cycles locally and breaks at block end:

```go
func (c *CPU) RunCached(nc *NoteChain) error {
    for {
        var cycles uint64
        slot := &c.cache[(c.pc - c.cacheBase) >> 1]
        for !slot.blockEnd {
            if err := c.execSlot(slot); err != nil { c.cycle += cycles; return deliver(...) }
            cycles++
            slot = &c.cache[(c.pc - c.cacheBase) >> 1]
        }
        c.execSlot(slot); cycles++
        c.cycle += cycles
        if c.watchAddr != 0 { ... }  // once per block, not per instruction
    }
}
```

Correctness note: CSR reads of `cycle`/`instret` must flush the in-flight accumulator first. Add `flushCycles()` called from `readCSR` when the CSR is one of the counters. Exception delivery pre-flushes cycles before invoking `nc.Deliver`.

Files: `cpu.go` (inline driver), `note.go` (fold `RunWithChain` into CPU method or gut it to a thin wrapper).

### Phase C + D — Inline memory access + branchless x0
**Expected gain: 800 → ~950 MIPS (≈1.15×).**

- Inline the `check` + `hostPtr` pair into LOAD/STORE cases in `cpu.go`. Keep the existing `GuestMemory.Load*` methods for external callers and the JIT — only the interpreter hot path bypasses them.
- Replace `c.Reg(rs1)` / `c.SetReg(rd, v)` with direct `c.x[rs1]` / `c.x[rd] = v` in `step()`; add `c.x[0] = 0` at the top of each iteration of the driver's inner loop.
- Keep `Load8`/`Store8` as-is (no alignment concern). For 16/32/64 loads and stores, keep the misalign fallback to `Load*U`/`Store*U`.

Files: `cpu.go` only.

### Phase F (optional, post-measurement) — Opcode switch reshuffling
If the profile after C+D shows the switch dispatch itself as hot, reorder cases so the most frequent opcodes (per profile) sit at the top of the switch. Also consider splitting the switch into a fast path that handles the top-8 opcodes first. Only attempt if profile data clearly justifies it.

## Step 4 — Verification

Run after each phase:
1. `go test ./...` — unit tests for CPU instructions, memory, ELF, OS, JIT, RVC, AMO, FP
2. `go test -v ./fuzzoracle` — oracle fuzzing vs libriscv (requires `make bench-setup`)
3. `go test -run=. ./riscv-elf-tests/...` — official RISC-V test suite (via `riscv_test.go`)
4. `make bench-cpu` — MIPS on bench_guest.elf
5. `make bench-coremark` and `make bench-dhrystone` — MIPS and official CoreMark/Dhrystone scores, compared head-to-head with libriscv
6. `BENCH_ELF=<elf> go test -bench=. -cpuprofile=cpu.prof ./bench/` + `go tool pprof -list stepCached cpu.prof` — confirm the hot function has shifted or flattened as expected
7. JIT regression: `make bench` (full comparison) to confirm JIT MIPS unchanged

Success criteria:
- `make bench-cpu` interpreter MIPS ≥ 891 (libriscv parity)
- All tests pass
- JIT MIPS unchanged within noise (±2%)

## Critical files

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cpu.go` — step, run loop, opcode switch, register access
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/note.go` — RunWithChain driver
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/guestmem.go` — Load*/Store* to inline
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/elf.go` — feed executable segment bounds to the decoder cache
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/bench/cpu_bench_test.go` — add CoreMark + Dhrystone benchmarks
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/Makefile` — add `coremark-elf`, `dhrystone-elf`, `bench-coremark`, `bench-dhrystone` targets
- new `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/decoder_cache.go` — decoder cache slab and builder
- new `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/bench/coremark/` and `bench/dhrystone/` — source and build scripts

## Expected outcome

| Phase | MIPS (our) | Notes |
|---|---|---|
| Baseline | 172 | today |
| A + B | ~500 | decoded cache + single fetch |
| E + G | ~800 | block batching |
| C + D | ~950 | inlined mem + branchless x0 — parity with libriscv |
| F if needed | 950-1100 | switch reshuffle based on profile |
