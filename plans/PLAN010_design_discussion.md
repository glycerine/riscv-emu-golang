# Notes: JIT Runtime Architecture — Discussion Capture

## Context

The previous plan (audit and disposition of `/Users/jaten/ris/goasm/`) is
**complete**. All P0/P1 issues fixed; deferred items 15 and 16 explicitly
postponed pending profile data or concrete consumer demand.

The current conversation has shifted from "audit goasm" to a deep design
discussion about the JIT runtime architecture: where our current design
sits relative to libriscv, what trade-offs apply when running pre-compiled
opaque binaries as a security sandbox, and what Go's runtime allows or
forbids.

This file captures the design insights so they survive context loss and
can inform a future planning session if/when the user decides to start
real implementation work on Phase 2 (replacing TCC with goasm) or beyond.

It is **not** an actionable plan — there is no current implementation
ask. It is a design-notes document.

---

## Design insights (current discussion thread)

### 1. SCC analysis is missing today

`scanRegion` (jit_emit.go:116) does bounded BFS reachability, not
loop/SCC analysis. Hot loops emerge accidentally because the BFS visits
both header and back-edge source. SCCs that span a function call
(recursion, mutual recursion, calls into hot helpers) are split across
regions, fragmented by the dispatch loop. Multi-symbol Ctx is the cheap
upgrade path that absorbs the call-graph SCCs into compiled code without
needing real Tarjan-style analysis.

### 2. Workloads that benefit from cross-region linking

- ✅ Function-call-heavy: parsers, compression, recursion, GC code
- ✅ Programs with hot helpers: `memcpy`/`memset`/`strlen` called from
  many regions
- ✅ VM-in-VM (interpreter as guest)
- ✅ Cold-tail splitting (future scanRegion improvement)
- ❌ Matrix mult, FFT, BLAS kernels (one tight loop, scanRegion swallows it)
- ❌ DFS/BFS within one function (recursive call lands in same region)

### 3. Three JIT design families and where they fit

- **PGTR** (LuaJIT): tight loops with one dominant path. Loses on JS-style
  polymorphism. Not applicable to opaque binary sandbox (no interpreter
  to record from).
- **Method-based with dominator analysis** (HotSpot, V8): dominant
  industry approach; works for irregular code; multi-tier setup is huge.
- **Superblock with profiling** (Dynamo, Rosetta 2): wins when no source
  is available. Closest fit for binary translation sandbox.

### 4. Single-digit-overhead binary translator (security sandbox)

Confirmed achievable (NaCl: 5–7%, Rosetta 2: ~20% but for x86→ARM cross-arch).
The recipe for our case:

1. **AOT translate at load time** (not lazy JIT) — avoid warmup cost
2. **Single-pass translator, no IR, no opts** — input is already optimized
3. **Pin guest registers to host registers** — biggest single perf lever
4. **Memory sandbox: guard pages with fixed reservation** — single
   instruction per access, MMU enforces bounds
5. **Direct cross-region call linking** — multi-symbol Ctx
6. **Return-address stack pairing for JALR=RET** — host RAS does the work
7. **PIC for non-RET JALR** — 1-2 cached targets, dispatch as cold path
8. **Page-aligned RX translation cache** — preserve compiler's layout

### 5. The syscall path is the hidden second front

Real workloads vary 10⁶× in syscall density. Naive dispatch-loop
handling can blow the budget on syscall-heavy workloads. Three-tier
syscall handling needed:

- **Tier 1**: assembly stubs for trivial syscalls (getpid, gettid,
  clock_gettime, sched_yield) — ~30 cycles. Matches libriscv.
- **Tier 2**: `//go:nosplit` non-allocating Go stubs for moderate
  syscalls — ~50–100 cycles. Fragile (must avoid stack-grow / alloc
  chains transitively).
- **Tier 3**: existing dispatch path for complex syscalls — ~200–500
  cycles. Acceptable because complex syscalls usually do real I/O that
  dominates anyway.

### 6. libriscv's syscall design (worth borrowing)

From `vendor/libriscv/lib/libriscv/tr_emit.cpp:720` and
`vendor/libriscv/lib/libriscv/linux/system_calls.cpp`:

1. **ECALL emitted as in-block function call**, not block exit
2. **Syscall number propagated as compile-time immediate** when the
   `MOV $imm, a7` is statically visible
3. **Per-syscall clobbering metadata** — only TKILL/EXIT/EXIT_GROUP
   marked clobbering; everything else preserves the host-cached
   guest registers
4. **Direct array dispatch** in runtime — no NoteChain overhead
5. **Pinned register caching across the call** — only `a0/a1` round-trip

Current gap vs. libriscv on syscalls: **~10× per-syscall boundary
cost**. Closing it is mostly about adopting these five techniques on top
of the broader register-pinning + direct-linking design.

### 7. Go-runtime constraint that limits us vs. libriscv

libriscv enjoys: no GC, no stack relocation, free C++ callbacks from
JIT'd code, no async preemption to worry about.

We have: Go runtime that requires stack maps for any PC the GC walks,
async preemption that can fire at unsafe-point PCs, stack growth that
can move frames.

The thought experiment "make JIT'd code act like Go code by emitting
stack maps" has limited legs:

- Our JIT frames have **no Go pointers by construction** (guest regs in
  `*[32]uint64`, guest mem in `[]byte`), so the stack map is empty
  everywhere.
- Registering JIT'd PCs with the runtime requires **unsafe moduledata
  manipulation** (goloader-style) that breaks across Go versions —
  perpetual maintenance tax.
- The realistic interpretation: **the existing `jitcall.Call`
  trampoline IS the Go-friendly model**. Treat the JIT block as a giant
  inline-asm body inside `Call`'s pre-reserved 64KB frame. GC stops at
  the trampoline; nothing below has roots; runtime is happy.

### 8. Why the trampoline (`internal/jitcall/call_amd64.s`) exists

Four jobs, all driven by Go ↔ foreign-code mismatches:

1. **ABI bridge**: Go ABI → System V AMD64 (arg/return register shuffle).
2. **Callee-saved preservation**: save BX/BP/R12-R15 around the call so
   the foreign code can't violate Go's caller assumptions.
3. **Pre-reserved 64KB frame**: declared `$65536-80`. Go's runtime grows
   the goroutine stack before entering Call; the JIT'd code uses that
   frame; GC walks Call's frame and stops there. This is the
   mechanism that lets foreign code run on a goroutine stack without
   blowing up the runtime.
4. **sret return**: 32-byte `Result` struct returned via System V hidden
   pointer; trampoline copies into Go ABI return slots.

The trampoline survives the move from TCC to goasm — its job is
"calling foreign code from Go," which is independent of the foreign
code's source.

### 9. Hard ceiling on what we can do

We can match libriscv on:

- Trivial syscalls (Tier 1 assembly stubs)
- Hot guest code (with register pinning + direct linking)
- Cross-region branches (with multi-symbol Ctx and PIC)

We **cannot** match libriscv on:

- Cold/complex syscall handling (Go runtime structurally won't allow it)
- Async preemption inside JIT blocks (would need runtime hacks)
- Calling normal Go functions from JIT'd code (nosplit constraint)

The honest budget: 4–8% overhead for compute-bound workloads,
8–15% for moderately syscall-heavy, can match libriscv on the syscall
mix that frequency-dominates real workloads.

---

## What this notes file is NOT

- Not an implementation plan — there is no current ask.
- Not a commitment to any of the design choices above — they are options
  the user is evaluating.
- Not a replacement for the goasm audit plan, which was completed and
  superseded this file.

## What it IS

A capture of design discussion threads so future planning sessions can
start from a shared understanding rather than re-deriving the
trade-offs from scratch. When the user decides to start real
implementation work on Phase 2 or beyond, this file is the input;
that planning session will produce a focused executable plan replacing
this notes file.
