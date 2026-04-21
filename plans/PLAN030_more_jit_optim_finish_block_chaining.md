# Closing the 5× Gap Between Our JIT and Native

## Context

On the bench_guest workload (fib(500M) + memstress + sieve, 2.52B retired insns), measured MIPS:

| Engine                                   | Linux (Ryzen 3960X) | macOS (i7-1068NG7) |
| ---------------------------------------- | ------------------- | ------------------ |
| Go interp                                | 443                 | 443                |
| **Go JIT — Fixed Static Mapping (baseline)** | **4174**            | **3322**           |
| Go JIT — ELS                             | 4169                | 3345               |
| Go JIT — TCC                             | 2223                | 1973               |
| libriscv JIT (TCC)                       | 4304                | 3617               |
| wazero wasm AOT (Go)                     | 19738               | 8344               |
| native x86-64 (-O3 -march=native)        | 22954               | 18035              |

The baseline here is the **Fixed Static Mapping** backend — our production setting. All proposals below are measured against and built on that path.

Two observations:
- **We're at parity with libriscv** (within ~3%). The C++ JIT isn't doing anything structurally fancier than us.
- **Wazero, also in Go, runs ~5× faster** than our JIT. That proves the gap is *code-quality and architecture*, not Go-vs-C++. The ceiling is native (~5.5× away on Linux, ~5.4× on macOS).

Workload shape matters: fib dominates (tight ALU loop, zero memory traffic), memstress is sequential 64-bit load/store, sieve is branchy byte access. Different optimizations help each.

## Where the 5× gap lives (root-cause analysis)

The Fixed-Mapping native JIT does real x86-64 codegen — register-allocated (14 int + 16 FP bound), pinned R12=x[], R13=f[], R14=membase, R15=memmask, RBP=IC, peephole optimizer, masked-addr memory (no bounds branch). That's decent. Yet we give up 5× vs native. Ordered by likely magnitude:

**1. Dispatch cost per block exit (~30–45% of wall time).**
`jitcall.Call` crosses the Go boundary on every block exit: 6 callee-save restores, 32-byte Result copy, return to Go, map/array lookup, setup args, re-enter native. At 4000 MIPS with ~50 insns/block we dispatch ~80M times/sec. The infrastructure for block chaining exists (`chainExits`, `tryPatchChain`, `patchChainTarget` in `jit.go:418–442`), but `emitChainableReturn` in `jit_emit_ir.go:204` still emits a plain `Ret` — the TODO says "re-enable chaining after fixing MOVABS offset calculation." So chaining only works for **post-patched** hits, and every block still returns to Go once before being patched. Backward branches in hot loops are the clear win here.

**2. Per-instruction codegen quality inside blocks.**
Native code for fib's inner loop is ~5 x86 instructions/iteration; we emit noticeably more because each RISC-V op becomes a standalone sequence with little cross-op optimization. Missing:
- Constant propagation / local value numbering across the whole block (today's peephole is a 4-insn sliding window).
- LUI+ADDI fusion into a single 64-bit `MOVABSQ`.
- Dead-store and redundant-load elimination for frame reloads.
- `LEA` for add-shift-add patterns and for `base + idx*scale + disp` loads.
- Folding of 32-bit-signed immediates into x86 `ADDQ $imm, r/m` is already present; the misses are in longer chains.

**3. Register pool is narrow.**
Int pool = 7 (RAX, RCX, RDX, RSI, RDI, R8, R9), shrinking to 5 when DIV/MUL is present. x86-64 has 16 GPRs; we're using roughly half. R10/R11 are scratch only; the Fixed Mapping pins R12–R15+RBP+RBX for architectural roles. On any block with >7 concurrently live RISC-V regs we spill to stack, which is the hot path in memstress and sieve. Note: expanding the pool inside Fixed Static Mapping means revising the hard-coded priority table in `ir/regalloc_fixed.go`, not swapping allocators.

**4. Block size cap (2048 insns / 16KB).**
Fine for fib, probably leaves sieve with extra block boundaries. Every extra exit is a chain-patch dependency.

**5. IC-increment emitted every instruction.**
`advancePC` emits an `AddImm(IC, IC, 1)` per op. For pure counter purposes we can accumulate once at block exit (or per-exit path). Small but per-insn.

**6. Memory ops, on memstress.**
Each guest load/store is `MOV [R14 + (addr & R15)]`. Native gets address-mode folding with base+index+disp. We don't pattern-match `ADD`+`LW` into a single addressed load when both ops live in one block.

**7. Indirect branch cost for JALR.**
Every JALR exits through the slow stub (can't chain a moving target). The bench has limited JALR so this is minor for this workload, but it hurts general programs.

## Recommended path forward (build on Fixed Static Mapping)

Grouped by expected MIPS gain per engineering week. Numbers are estimates; all should be **measurement-gated** before going further.

### Tier 1 — Finish what's already wired (expected 1.5–2.5× total)

**T1.1 — Finish block chaining (`jit_emit_ir.go:204`, `ir/lower_amd64.go` chain exit lowering).**
The plumbing exists. Resolve the MOVABS offset calculation and make `emitChainableReturn` actually emit the chain-exit sequence (MOVABS R10, <sentinel>; JMP R10) on every `jitOK` return. Verify `tryPatchChain` succeeds on the first exit. The fib loop is one block chained to itself — once patched, zero Go-boundary crossings.
- Gate: fib MIPS rises ≥2× on its own; `ChainPatched / DispatchOK` ratio should approach 1.
- Fallback: if MOVABS patching is too fragile, use a `JMP rel32` that points to a per-block patch site, same effect.

**T1.2 — Emit IC increment once per basic-block edge, not per instruction.**
Track `numInsns` statically in the block; emit a single `ADDQ $n, RBP` per exit path. Already done in emit epilogue for some paths — make it universal.
- Gate: `ADDQ $1, RBP` instances go from N to ≤ #exits. Expect +3–5%.

**T1.3 — Extend chaining to indirect dispatch (JALR) via inline cache.**
One-entry per-site PIC: `cmp last_target, r_target; je cached_block; jmp slow`. Cheap win for sieve and any future non-bench workload. Skip for this bench if fib+memstress dominate.

### Tier 2 — Better codegen inside blocks (expected 1.5–2× on top of Tier 1)

**T2.1 — Block-scope constant propagation / copy propagation.**
Replace the 4-insn peephole window with a single-block forward pass that tracks constant values and copies. Folds `LUI + ADDI → MOVABSQ $imm`, `ADDI t0, zero, 5; ADD t1, t2, t0 → ADDI t1, t2, 5`, etc. Scope: one block at a time (no cross-block reasoning needed).

**T2.2 — Redundant-load / dead-store elimination within a block.**
Track last value stored/loaded at each `[sp+k]` slot. Eliminate the reload if sp is constant (almost always) and no intervening store aliases. Biggest win in sieve and anywhere the guest reloads spilled args.

**T2.3 — LEA for add-shift and load-address folding.**
Pattern match `SLLI rX, rA, k; ADD rY, rB, rX; LW rZ, 0(rY)` → one `MOV rZ, [rB + rA*scale]`. Scales up memstress specifically. Use x86 LEA for any 3-operand add-shift that doesn't need flags.

**T2.4 — Widen the integer regalloc pool within Fixed Static Mapping.**
In `ir/regalloc_fixed.go`, the priority table currently stops at 7 regs. R10 and R11 are reserved as scratch / chain-exit targets, but they're only used at block boundaries — they can be full regalloc candidates in the block interior, with a block-end writeback that frees them. Potentially gain 2 more int regs (9 total), which eliminates most spills in the memstress inner loop. Keep DIV/MUL carveout logic intact.
- Gate: spill-slot count drops; memstress MIPS rises. Correctness: every existing JIT test passes.

### Tier 3 — Larger translation units (expected 1.3–1.6×)

**T3.1 — Superblock / multi-entry region compilation.**
Today's BFS region stops at JAL/ECALL/big jump. Extend to trace-style superblocks: follow the taken path of conditional branches when profiled-hot, emit side exits for the cold path. This is what libriscv does implicitly via larger regions and what wazero does trivially because WASM has structured control flow. For RISC-V, needs a profiling pass or a naive heuristic ("extend at all branches up to a budget").

**T3.2 — Inter-block register hints.**
When block A chains to block B, if A ends with `a0` in RAX and B enters expecting `a0` in RAX (Fixed Mapping guarantees this), we already save the spill. But write-back-all at every exit still reloads on entry. Add a chain-exit mode that skips write-back when the target is known and shares the register convention.
- Gate: spill traffic around chained loops drops. Risk: correctness if a fault intervenes between blocks — keep a "checkpoint" fallback.

### Tier 4 — Architectural bets (expected 2–3× combined but weeks of work)

**T4.1 — AOT mode: compile reachable guest code up-front.**
At ELF load, disassemble + translate all reachable code into one contiguous machine-code image with all direct branches pre-resolved. This is what wazero does and why it's at 86% of native. For RISC-V we'd need a static discovery pass (harder than WASM because control flow isn't structured — indirect jumps, computed gotos, vtables). Could gate behind an `AOT: true` config for binaries with well-behaved control flow.

**T4.2 — Full SSA IR with standard optimizations.**
Current IR is a linear op list. A real SSA form enables global value numbering, loop-invariant code motion, dead-code elimination, and sparse conditional constant propagation. This is a several-week investment with big upside for complex guests. For the fib/memstress/sieve bench, T2 local passes probably capture most of the easy gain.

**T4.3 — Translate guest → Go source, compile with `gc`.**
An option we haven't tried: emit Go code for each function, run `go build`, `plugin.Open` the result, call into it. Leverages Go's optimizer. Downsides: huge compile time, cold-start cost, plugin restrictions. Only makes sense in AOT mode.

**T4.4 — Out-of-scope for this bench but worth noting:**
LLVM backend (compile to bitcode, run LLC); LuaJIT-style trace compilation with guards and deopt; real hardware hypervisor if present. These are 3–6 month efforts.

## Recommended sequencing

Ship Tier 1 first — it's all wiring we already started. On fib alone, finishing chaining (T1.1) should be visibly multiplicative. Then measure. Tier 2 follows (T2.1–T2.4) for in-block quality — also well-scoped and local. Reassess before Tier 3+.

Order I'd bet on:

1. **T1.1** — finish block chaining. *Biggest single expected win.*
2. **T1.2** — fold IC increments. Easy measurable.
3. **T2.4** — widen the regalloc pool (R10, R11 reclaim). Small change, probably helps memstress most.
4. **T2.1, T2.2** — block-scope constprop + load/store elim. Real codegen improvement.
5. Re-profile. Decide whether T3.1 (superblocks) or T2.3 (LEA patterns) pays more.
6. Consider T4.1 (AOT) only if we've run out of in-block gains and the bench is still short of native.

## Critical files

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit.go` — dispatch loop (lines 286–442), chain-patch helpers.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit_ir.go` — `emitChainableReturn` (L204), `advancePC` (L177) for IC increment.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/lower_amd64.go` — native emission, chain-exit MOVABS lowering, binop/load/store lowering, LEA patterns.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/regalloc_fixed.go` — Fixed Static Mapping priority table; widening the pool lives here.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/peephole.go` — current 4-insn window; extend to full-block pass for T2.1/T2.2.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_decode.go` — `scanRegion` (L104); region-size and superblock logic for T3.1.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/internal/jitcall/` — trampoline; touch only if chaining needs new entry conventions.

## Verification methodology

Every change must:

1. Pass `go test ./...` (CPU, mem, ELF, OS, JIT, RVC, AMO, FP).
2. Pass `make fuzz-oracle`, `make fuzz-fd`, `make fuzz-rvc`, `make fuzz-amo`, `make fuzz-bitmanip` against libriscv.
3. Pass `go test -run=. ./riscv-elf-tests/...`.
4. Keep MIPS on **Fixed Static Mapping** the metric of record: `make bench-quick` and `make bench-ours` for Linux/macOS split.
5. Report `DispatchOK`, `DispatchCompile`, `DispatchInterp`, `ChainPatched`, and `insns/dispatch` from `bench/jit_bench_test.go:37–42` before/after.
6. Per-phase stop rule: if measured gain is <50% of predicted, re-profile with `make prof` before continuing to the next tier. The profile drives the plan, not the reverse.

Target: ~10,000 MIPS from Tier 1+2 (2–2.5× from 4174). Beyond that likely requires Tier 3 or T4.1 AOT.
