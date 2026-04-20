# Go Interpreter: Match or Exceed libriscv Interpreter

## Context

Our Go RV64 interpreter runs at **192 MIPS** (CoreMark cached), **250 MIPS** (bench_guest cached), **208 MIPS** (Dhrystone cached). libriscv's C++ interpreter runs at ~**828 MIPS** on the same workload — a 3.3–4.3× gap.

The original initiative (decoder cache + branchless x0) moved us from 172 → 192-250 MIPS. A fresh profile now shows where the remaining gap lives and what the next set of changes should target. This plan covers the **next wave** of optimization, driven by the v2 profile written to `prof_findings_v2.md`.

### What's already landed

| Phase | Scope | Result |
|---|---|---|
| Step 1 (harness) | Vendored CoreMark (`xendor/coremark`), Dhrystone (`xendor/dhrystone`); added `BenchmarkCPU_{CoreMark,Dhrystone}` + Makefile targets | ✅ |
| Step 2 (profile) | Captured Go pprof; `prof_findings.md` written | ✅ |
| Phase A+B (decoder cache, single fetch) | `decoder_cache.go`, `decode.go`, `exec_slot.go`, `exec_slot32.go`, `run_cached.go` | ✅ 172→192-250 MIPS |
| Phase C (branchless x0) | `SetReg`/`Reg` in `cpu.go:49-53` | ✅ |
| Phase D (memory inlining) | **Not done** — Load*/Store* are already inlined by Go but still go through `check + hostPtr` |
| Phase E+G (block batching) | Attempted, regressed. Landed `pollBatch=1024` watchAddr polling in `run_cached.go:14` instead | ⚠️ partial |

### Where the remaining time goes — v2 profile (CoreMark cached, 10s sample)

```
flat%  cum%    function
34.65% 38.14%  (*CPU).execRVCSlot
27.22% 99.50%  RunCached           ← driver loop itself
19.01% 22.62%  (*CPU).exec32Slot
 9.92% 10.20%  (*DecoderCache).lookup
 2.49%  2.49%  (*GuestMemory).check
 ~5%           Load64/Load32/Store*/hostPtr
```

54% is instruction bodies (healthy). The remaining ~46% is **driver + dispatch + lookup overhead** — that's the target surface.

### Concrete per-line waste in the hot loop

From `pprof -list RunCached` and `pprof -list execRVCSlot`:

| Waste | ns/insn | Root cause |
|---|---|---|
| `cpu.pc` read twice (driver + executor) | ~6% flat | executor reads `c.pc`, driver just read `cpu.pc` 2 instructions ago |
| `slot.len` loaded **twice** in dispatch | ~3% flat | Compiler can't hoist across the `exec*Slot` call (aliasing) |
| `cache.lookup` on every insn | ~10% flat | 85-90% of instructions are non-branch — successor is predictable |
| `exec*Slot` function prologue | ~5% flat | Method call crosses stack-frame boundary |
| `slot != nil` check | ~1-2% flat | OOB PCs are rare — could use sentinel slot |

## Goal

Same as before: match libriscv on a RISC-V benchmark suite without breaking the JIT, fuzzoracle, riscv-elf-tests, RVC/FP/AMO coverage, or the `GuestMemory` security invariant.

**Realistic ceiling in pure Go**: ~400-500 MIPS on CoreMark. libriscv's computed-goto threaded dispatch is worth ~1.3-1.7× on its own and is unavailable in Go; our switch is the floor. Past ~500 MIPS likely requires a language-level option (Go+asm, separate engine written in Go assembly, or a second JIT tier for hot interpreter loops).

## Plan

Each phase has a single-sentence theory, a concrete file diff scope, a measurable gate, and a stop condition if the gain doesn't appear.

### Phase A — micro-wins (expected +10-15%)

Single goal: reduce per-instruction driver overhead without changing layout or interfaces.

**A1. Hoist `slot.len` into a local before dispatch.**
- File: `run_cached.go:28-36`
- Change: one `slotLen := slot.len` above the `if`, switch on it with three arms (`2`, `4`, default = slow path).
- Gate: `slot.len` line in `pprof -list RunCached` drops from 560ms → ~0.

**A2. Pass `pc` as a parameter to `execRVCSlot` / `exec32Slot`, return `newPC`.**
- Files: `run_cached.go` (callsite), `exec_slot.go` (signature + `pc := c.pc` removed), `exec_slot32.go` (signature + same removal).
- New signature: `func (c *CPU) execRVCSlot(slot *DecodedInsn, pc uint64) (uint64, error)`. Executors return `(pc+len, nil)` or `(target, nil)` for branches. `c.pc` only written at inner-loop break or fault delivery.
- Gate: `pc := c.pc` line in `pprof -list execRVCSlot` drops from 610ms → ~0.

**A3. Sentinel slot for OOB PCs — eliminate the nil check.**
- File: `decoder_cache.go:66-72`
- Change: `lookup` returns a pointer to a pre-allocated sentinel slot (`len=0, op=opFallback`) instead of nil when `off >= size`. Driver falls through to slow path via the same `slotLen == 0` case.
- Gate: `slot != nil` branch disappears from RunCached listing.

**Combined gate for Phase A**: CoreMark MIPS ≥ 210 (≈+10%). If below, stop and re-profile before Phase B.

### Phase B — slot chaining (expected +10-15%)

Single goal: skip `cache.lookup` on the 85-90% of instructions that are non-branches.

**B1. Add `next *DecodedInsn` to `DecodedInsn`.**
- File: `decoder_cache.go` (struct grows 16 → 24 bytes; cache-density drops 4 → ~2.7 slots per 64-byte line).
- `populateSlot` in `run_cached.go:90` sets `next = &cache.slots[(pc+len-base)>>1]` for non-branch slots whose successor is in-range; leaves `next = nil` otherwise (branch / jump / OOB successor).

**B2. Chain-walk in the driver.**
- File: `run_cached.go:17-43`
- Change the tail of the inner loop: if `slot.next != nil`, take it directly; else `cache.lookup(cpu.pc)`. Only taken branches / JAL / JALR / ECALL / traps force a lookup.

**Gate for Phase B**: `cache.lookup` flat time drops from ~9.9% → ≤ 2%. CoreMark MIPS ≥ 230. Cache-density regression acceptable if MIPS still improves net.

### Phase C — megaswitch (expected +20-30%, biggest single phase)

Single goal: remove the function-call boundary between the driver and the opcode bodies.

**C1. Inline both `execRVCSlot` and `exec32Slot` bodies into `RunCached`.**
- File: `run_cached.go` grows to ~1500 lines; `exec_slot.go` and `exec_slot32.go` become thin wrappers that call into shared helpers (or are deleted if the driver owns the logic outright).
- Dispatch: one switch over `slot.op` covering both RV32 opcodes (0x03..0x7F) and synthetic RVC classes (opC_* ≥ 0x80). Compiler emits a single jump table.
- Keep `stepFromInsn` / `stepRVC` as-is for the slow path (uncached PCs, unrecognized ops, fuzzing). This is the correctness net.

**Gate for Phase C**: CoreMark MIPS ≥ 280 (≈+30% from 210). `exec*Slot` disappears from the profile; `RunCached` flat% rises but absolute MIPS goes up.

### Phase D — memory fast path (expected +3-5%)

Single goal: strip the generic-purpose bounds-check wrapper on LOAD/STORE for in-hot-path sizes.

**D1. Specialized aligned 64-bit LOAD/STORE in the megaswitch.**
- File: `run_cached.go` (LOAD 0x03 funct3=0x3, STORE 0x23 funct3=0x3, RVC LD/SD).
- Change: direct `*(*uint64)(unsafe.Pointer(c.mem.base + (addr & c.mem.mask)))` for aligned cases. Fault cases (unaligned, OOB) fall back to `(&c.mem).Load64U(addr)`.
- Security invariant preserved: `addr & mask` still bounds the access. `unsafe.Pointer` indexing replaces the method call only.

**Gate for Phase D**: `GuestMemory.check` / `Load64` combined flat% drops from ~4% → ≤ 1%. CoreMark MIPS ≥ 300.

### Phase E — pre-resolve direct branch targets (expected +2-5%, optional)

Single goal: skip re-lookup on unconditional static-target branches.

**E1. Store target slot pointer in the slot for JAL and C.J.**
- File: `decode.go:39-50` (JAL), `decode.go:202-205` (C.J).
- When the target is in-cache, write `slot.next = target_slot` at decode time. Driver already chases `slot.next`.

**Gate for Phase E**: CoreMark `cache.lookup` flat% drops further. Only pursue if E1 is cheap and the profile still shows lookup as a top-5 cost.

### Phase F — opcode switch reshuffling (contingent)

Only if, after A+B+C, `pprof -list RunCached` shows switch dispatch itself as a top cost and reordering cases by observed frequency demonstrably helps. Skip otherwise.

## Verification (run after each phase)

1. `go test ./...` — unit tests (CPU, mem, ELF, OS, JIT, RVC, AMO, FP)
2. `go test ./fuzzoracle` — oracle fuzzing vs libriscv
3. `go test -run=. ./riscv-elf-tests/...` — official RISC-V test suite
4. `make bench-cpu` — MIPS on bench_guest.elf
5. `make bench-coremark` and `make bench-dhrystone`
6. `go test -bench=BenchmarkCPU_CoreMark -benchtime=10s -cpuprofile=/tmp/cm.prof ./bench/` + `go tool pprof -list RunCached /tmp/cm.prof` — confirm the targeted line dropped
7. **Cycle-count bit-exactness**: cycle counts from `RunCached` must match `RunWithChain` on all three benchmark ELFs. This is the strongest correctness invariant for refactoring the hot path; compare `cpu.Cycle()` after each run.
8. JIT regression: `make bench` — JIT MIPS unchanged within ±2%.

Success criteria for the whole plan:
- CoreMark MIPS ≥ 400 (our realistic Go ceiling; libriscv parity is aspirational).
- All tests pass.
- JIT MIPS unchanged.
- No new unsafe-pointer corners outside of `run_cached.go` LOAD/STORE fast paths.

## Critical files

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/run_cached.go` — driver loop; Phase A/B/C/D all touch this
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/decoder_cache.go` — slot layout, lookup, sentinel (A3, B1)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/decode.go` — `populateSlot` successor wiring (B1, E1)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/exec_slot.go` — RVC executor; A2 signature change, C absorbs or deletes
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/exec_slot32.go` — 32-bit executor; A2 signature change, C absorbs or deletes
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cpu.go` — unchanged (slow path kept), except if Phase C requires shared helpers
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/guestmem.go` — unchanged (Load*/Store* preserved for slow path and JIT)

## Expected outcome

| Phase | CoreMark MIPS | Δ | Notes |
|---|---|---|---|
| Baseline (today) | 192 | — | decoder cache + branchless x0 |
| +A1/A2/A3 | ~210-220 | +10-15% | hoist len, pc-param, sentinel |
| +B | ~230-250 | +10-15% | slot chaining |
| +C | ~280-320 | +20-30% | megaswitch |
| +D | ~290-340 | +3-5% | unsafe mem fast path |
| +E / +F if justified | ~300-400 | +3-15% | only if profile demands |

Stop condition: if any phase's actual gain is < 50% of the expected range, re-profile before continuing. The profile drives the plan, not the reverse.
