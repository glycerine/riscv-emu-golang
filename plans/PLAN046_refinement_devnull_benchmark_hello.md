# Plan: Correctness-Verified Benchmarks + Fair Dispatch Comparison

## Background (completed)

Phases 1 and 2 delivered, on master:

- **Phase 1** — `cd ~/ris && make hello` harness, guest ELFs for libriscv
  and GoCPU strings, GoCPU interpreter + libriscv runners. Files:
  `bench/hello_guest/`, `bench/hellobench/main.go`, `os.go`
  (`InstallLinuxOS`/`RunWithLinuxOS`), `hello_test.go`.
- **Phase 2** — native ECALL fast path: `internal/syscalls/` (Go-asm
  SysV dispatcher + tests), `IRSyscall` op (`ir/ir.go`, `ir/emit.go`,
  `ir/regalloc.go`, `ir/lower_amd64.go`, `ir/lower_amd64_v2.go`),
  `jit_syscall.go`, ECALL emission in `jit_emit_ir.go`, third runner
  in `hellobench`, dup2-capture test `TestHelloGoCPU_JIT_DirectSyscall`.

Current `make hello`:

```
  libriscv             21.0 +/- 0.95 ns/call   Hello, libriscv!
  GoCPU interpreter    82.9 +/- 5.20 ns/call   Hello, Go CPU!
  GoCPU direct syscall 717.2 +/- 42.40 ns/call   Hello, Go CPU!
```

---

## Context for this plan

Two issues with today's benchmark, raised in conversation:

1. **No correctness check.** All three runners discard guest output
   (`null_stdout` for libriscv, `io.Discard` for GoCPU interpreter,
   `dup2`-to-`/dev/null` for GoCPU direct-syscall). If the dispatcher
   silently writes wrong bytes, skips writes, or the JIT emits bad
   register values, `make hello` still returns a plausible timing. A
   regression becomes a mysterious performance change rather than a
   loud test failure. The user wants regressions caught immediately.

2. **Not apples-to-apples.** libriscv's 21 ns never enters the kernel
   — `opts.output` (registered as `null_stdout` in
   `bench/libriscv/bridge.go:23-24`) discards bytes in C userspace.
   Confirmed via `xendor/libriscv/lib/libriscv/linux/system_calls.cpp:308-314`:
   for fd 1/2, libriscv's write handler routes to
   `machine.print()` → `opts.output()`, never `write(2)`. Our
   direct-syscall path does a real `SYSCALL` to the kernel, paying
   ~400–500 ns of kernel entry on darwin. Decomposing our 717 ns: ~500
   ns kernel + ~217 ns dispatch, vs libriscv's 21 ns of pure dispatch.

These are independent problems but can be fixed together.

---

## Phase 3 — Correctness verification in every benchmark run

**Goal:** every runner on every repeat captures and verifies the exact
`"Hello, <tag>!\n" × 10000` output, and `die()`s loudly on mismatch.
Silent regressions become impossible.

### Design

Each runner already has a natural capture point; we just stop throwing
bytes away.

**libriscv.** Replace `null_stdout` in `bench/libriscv/bridge.go` with
a capturing callback that appends each `(buf, len)` chunk to a
reusable C buffer. Expose it to Go as a `[]byte` after each run.
Keep `null_stdout` available for callers that explicitly don't want
capture. Concretely: add a new bridge entry point
`libriscv.NewMachineCapturing(elf, memBytes) → *Machine` and a
`(*Machine).CapturedOutput() []byte` getter that copies out the
accumulated bytes. Register a C callback that does
`memcpy` into a contiguous buffer (amortized O(1) per chunk with
realloc-by-doubling).

**GoCPU interpreter.** `runGoCPU(elf, jit=false, verify=true)`
already supports `bytes.Buffer` capture — just flip the flag on. The
existing code is at `bench/hellobench/main.go:runGoCPU`.

**GoCPU direct syscall.** Replace `withStdoutToDevNull` in
`bench/hellobench/main.go` with a `withStdoutToTempFile` that
`dup2`s fd=1 to a `os.CreateTemp` file, runs the closure, restores
fd=1, reads the tempfile contents, and returns them. Same
infrastructure as `hello_test.go:captureStdout`. Truncate between
repeats so memory doesn't balloon across 5 runs.

All three: after each run, `bytes.Equal(captured, expected)` →
`die()` on mismatch with a short diff (first-differing-byte
position, length comparison, last 32 bytes of each).

### Expected characteristics

- Cost of capture: per run, libriscv adds one C memcpy of 150 KB
  (~30 µs) plus callback overhead; GoCPU interp is unchanged;
  GoCPU direct syscall adds one file-seek + read of 150 KB (~100
  µs). Per-ECALL impact is dominated by kernel-write to a file vs
  /dev/null: roughly equal on darwin (both go through the kernel's
  VFS layer and end up nowhere "real"). Expect numbers to drift by
  <5% from today.
- The `--verify` flag on `hellobench` becomes redundant and can be
  removed.

### Files to touch

- `bench/libriscv/bridge.go` — new capturing callback, new
  `NewMachineCapturing` + `CapturedOutput` methods. Keep existing
  `NewMachine` for callers (fuzz oracle, etc.) that don't need capture.
- `bench/hellobench/main.go` — new `withStdoutToTempFile`, call
  the capturing APIs, verify in each runner closure, remove
  `--verify` flag.

### Verification

- `make hello` succeeds and all three lines still print.
- Artificial regression test: temporarily break the direct-syscall
  stub (e.g., replace `SYSCALL` with `XOR RAX,RAX; RET` so it returns 0
  without writing) and confirm `make hello` loudly fails with a byte
  mismatch.
- Existing tests still pass: `go test -run TestHello . && go test
  ./internal/syscalls/`.

---

## Phase 4 — Apples-to-apples dispatch comparison

**Goal:** expose our true dispatch overhead (not confounded by
kernel-entry cost) and make the libriscv vs GoCPU numbers directly
comparable.

### Design

Add two new benchmark variants so each emulator has one dispatch-only
line and one kernel-inclusive line. With Phase 3 active, all four also
verify output.

**libriscv (kernel write).** Add a `real_write_stdout` callback that
does `write(1, buf, len)` and use it in a second bridge entry point
`NewMachineRealWrite`. Output goes to fd=1 (captured via Phase 3's
tempfile). Expected cost: ~520 ns (20 dispatch + 500 kernel).

**GoCPU direct dispatch callback.** Add an output callback hook to the
Go-asm dispatcher in `internal/syscalls/`. Current stub issues
`SYSCALL`; a new variant — `dispatch_callback.s` or a flag — calls a
registered C-ABI function pointer with `(buf_ptr, count)` instead. The
callback is a Go-asm or cgo thunk that appends to a Go `bytes.Buffer`
(or captures into the tempfile by issuing its own `write(2)`, but only
once per N calls — unnecessary complication for first cut). Expected
cost: ~50 ns (dispatch + Go/C callback), target < 100 ns.

**Harness.** `hellobench` grows from 3 lines to 5:

```
  libriscv null_stdout        <~21 ns>   dispatch only (no kernel)
  libriscv real write fd=1    <~520 ns>  dispatch + kernel
  GoCPU interpreter           <~83 ns>   dispatch only (io.Discard)
  GoCPU direct syscall        <~717 ns>  dispatch + kernel
  GoCPU direct callback       <target>   dispatch only
```

The two "dispatch only" lines are directly comparable. The gap between
`GoCPU direct callback` and `libriscv null_stdout` is our *real*
optimization target; the gap between `GoCPU direct syscall` and
`libriscv real write` is the practical-use performance.

### What this will reveal

If `GoCPU direct callback` lands at ~50 ns, we're within 2.5× of
libriscv's pure dispatch — respectable, and the residual gap is
mostly block termination at ECALL.

If it lands at ~200 ns, kernel entry wasn't the big cost; the big
cost is our JIT block round-trip (jitcall.Call + block cache lookup
+ prologue on every ECALL). That feeds the decision in Phase 5 below.

### Files to touch

- `bench/libriscv/bridge.go` — `real_write_stdout` callback,
  `NewMachineRealWrite` entry.
- `internal/syscalls/dispatch_{linux,darwin}_amd64.s` — new stub
  variant (or a single stub with a mode switch driven by a global
  function-pointer, null = use SYSCALL, non-null = call pointer).
- `internal/syscalls/dispatch.go` — Go-side registration API:
  `SetWriteCallback(cb WriteCallback)` where
  `WriteCallback = func(fd int, buf []byte) int`.
- `bench/hellobench/main.go` — two new runners, callback
  registration.

### Verification

- All 5 lines print; all 5 pass correctness check from Phase 3.
- `GoCPU direct callback` line's measured bytes match expected.
- Existing `TestHelloGoCPU_JIT_DirectSyscall` still passes (that test
  exercises the real-kernel path).

---

## Phase 5 (deferred) — close the dispatch gap

Only worth attempting *after* Phase 4 measures how big the real gap is.

Candidate optimizations, roughly in order of expected payoff:

1. **Don't terminate the JIT block at ECALL.** Today `emitSyscall`
   ends the block and returns to Go; the next iteration re-enters
   via `jitcall.Call` + block cache lookup. For a 3-instruction loop,
   that's most of the dispatch cost. Emit the dispatcher call inline
   and continue the block, invalidating only `x[10]` (and `x[11..15]`
   if conservative) after the call. Requires machinery the IR doesn't
   currently have for mid-block register-file invalidation.
2. **Cache the dispatcher address in a pinned reg.** Today we emit a
   `MOVABS $addr, scratch` at each ECALL site. Loading once into a
   pinned register at block entry saves 10 bytes per ECALL site and
   a few cycles.
3. **Specialize on compile-time-known `a7`.** If the block preceding
   ECALL contains `li a7, 64` (common pattern), constant-propagate
   and emit a direct call to the write stub skipping the dispatcher
   switch. Saves ~5 ns but amplifies inlining.

Don't scope this work until Phase 4 data is in hand.

---

## Critical files (at-a-glance index)

Already-modified by earlier phases:

- `bench/hello_guest/hello.c`, `bench/hello_guest/link.ld` (guest ELFs)
- `os.go` — `InstallLinuxOS`, `RunWithLinuxOS`
- `bench/hellobench/main.go` — harness
- `hello_test.go` — correctness tests + `captureStdout`
- `internal/syscalls/dispatch.go`, `dispatch_{linux,darwin}_amd64.s`,
  `dispatch_test.go`
- `ir/ir.go`, `ir/emit.go`, `ir/regalloc.go`, `ir/lower_amd64.go`,
  `ir/lower_amd64_v2.go` — `IRSyscall` op + lowering
- `jit_syscall.go` — dispatcher address registration and toggle
- `jit_emit_ir.go` — ECALL emission

Phase 3 adds/touches:

- `bench/libriscv/bridge.go` — capturing callback, new bridge entry
- `bench/hellobench/main.go` — `withStdoutToTempFile`, verification

Phase 4 adds/touches:

- `bench/libriscv/bridge.go` — real-write callback, new bridge entry
- `internal/syscalls/dispatch_{linux,darwin}_amd64.s` — callback
  dispatcher variant
- `internal/syscalls/dispatch.go` — `SetWriteCallback` API
- `bench/hellobench/main.go` — two new runners
