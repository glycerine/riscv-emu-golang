# Plan: Fast Guest Syscalls — "Hello, World!" via Direct SYSCALL

## Context

This is the emulator's first real OS-interaction milestone: get guest code
to do `write(1, "Hello, World!\n", 14)` and see it on the host terminal,
while preserving — and extending — the JIT hot-path throughput.

The motivating constraint: **libriscv reports ~3 ns per host↔guest
boundary crossing**. If a syscall costs microseconds, I/O-heavy guests
pay thousands of times more per call than libriscv. The project's value
proposition erodes. So syscalls must be cheap.

Today our ECALL path exits the JIT block, returns through `jitcall.Call`,
walks the NoteChain, and invokes a Go `SyscallHandler` under the OS
personality. Estimated cost: **~2–12 µs per ECALL**. That's ~1000×
libriscv's number.

### Decisions already made (from discussion)

- **No TCC on the syscall path.** TCC stays as a legacy per-block
  compiler; the syscall fast path uses the **Go-only native IR JIT
  backend** (goasm-based, no cgo at runtime, no LGPL).
- **Direct `SYSCALL` instruction on both linux/amd64 and darwin/amd64.**
  Speed is the priority. Apple's "unstable ABI" concern is a manageable
  risk (common syscall numbers have been stable for a decade; a recompile
  restores the emulator if Apple ever breaks us; Apple breaking would
  affect a huge amount of software, so it's unlikely).
- **Exit semantics**: native stub sets a flag + returns `jitEcall`
  status; the existing Go exit path takes over. No panic from stubs.

---

## How libriscv Achieves ~3 ns (verified)

When the binary-translator (TCC mode) emits an ECALL, it does **not**
return to the C++ emulator. It emits an inline call:

```c
// libriscv/tr_emit.cpp:744
max_ic = api.system_call(cpu, 0x<pc>, ic, max_ic, sysno);
```

`api.system_call` is a registered C function pointer; in
`libriscv/tr_translate.cpp:1348` it's a lambda that reduces to:

```cpp
// libriscv/machine_inline.hpp:79
syscall_handlers[sysnum](*this);    // one indirect array-call
```

The handler is a plain C++ function that reads `cpu.reg(10..15)`, calls
libc `write`, writes `cpu.reg(10)`, returns. **Control never leaves
native code.** The 3 ns is just "what an indirect function call costs
when nothing else is in the way." The kernel-entry cost of the actual
`write(2)` is on top (~100–300 ns) and identical across any emulator.

Key point: libriscv is not doing magic. It avoids language boundaries.
We will match it by also avoiding language boundaries — **native
code calling native code, no cgo crossing, no Go scheduler involvement**.

---

## Current State (verified)

- JIT emits ECALL as `return (JITResult){pc, ic, jitEcall, 0};` — exits.
- `jitcall.Call` returns to Go with `Status=jitEcall`.
- `RunJIT` builds a `Note`, runs `cpu.Notes.Deliver(cpu, n)`.
- `OS.Handle` does a map lookup, calls Go handler, copies guest bytes,
  calls a Go `WriteFunc`.
- All of this is Go. Cost ≈ 2–12 µs per ECALL.

What we already have in our favor:

- `internal/jitcall/call_amd64.s` — pure Go asm, zero cgo; calls JIT
  blocks in ~5–10 ns. This is our **precedent** for the asm-based
  fast path we will build for syscalls.
- `jit_tcc.go:20–34` — `jit_trace` already calls raw `write(2)` from
  inside JIT code, proving the pattern works from TCC-compiled code.
- `ir/emit.go:213` — `ir.Emitter.Call(sym, addr)` registers a C-ABI
  function and emits `IRCall`.
- `ir/lower_amd64.go:1945` — `lowerCall` emits a native `CALL` with
  live-register save. This is our **hook point**: the syscall stub
  is just a specially-registered `CTab` entry.

---

## The Approach: Direct `SYSCALL` via Go-asm Stubs, Dispatched by the IR JIT

The syscall fast path is one new file plus two small changes:

1. **`internal/syscalls/stubs_<goos>_amd64.s`** — per-syscall leaf
   stubs in Go assembly. Each stub takes the System V AMD64 ABI
   (matching what `ir.lowerCall` emits), reads args from the guest
   `CPU` by offset, translates guest pointers via `mem_base`, sets
   `RAX` to the host syscall number, issues `SYSCALL`, writes the
   result back to `x[10]`, and returns.
2. **Dispatch table** — a `[NR_SYSCALLS]uintptr` on `JIT` populated
   at init with the stub addresses. Unimplemented slots point at a
   fallback thunk that returns `jitEcall` so the existing Go path
   handles them.
3. **IR emission change** — at ECALL, instead of emitting a return
   with `jitEcall`, emit:
   - Load `x[17]` (guest syscall number, a7)
   - Bounds-check against `NR_SYSCALLS`
   - `MOVQ dispatch_table(,a7,8), scratch`
   - `IRCall scratch` (through existing `lowerCall`)
   - After the call, either continue in the block (for syscalls like
     write that return normally) or observe the `exit_flag` in `CPU`
     and return `jitEcall` to Go (for exit).

This structure mirrors libriscv's `api.system_call` one-to-one, but
implemented in Go asm + goasm-emitted native code instead of
C++/TCC. No cgo, no TCC, no LGPL.

### Stub shape (pseudocode)

For linux/amd64, `sys_write_linux_amd64.s`:

```
// Entry: SysV AMD64 ABI
//   RDI = *CPU     (guest register file base)
//   RSI = mem_base (host address of guest memory)
//   RDX = mem_mask (size-1 of guest memory)
// Reads:  CPU.x[10]=fd, CPU.x[11]=guest_buf, CPU.x[12]=count
// Writes: CPU.x[10] = ssize_t result
TEXT ·sys_write(SB), NOSPLIT|NOFRAME, $0-0
    MOVQ  8*10(DI), AX          // fd (to be in RDI for syscall)
    MOVQ  8*11(DI), R10         // guest buf
    ANDQ  DX, R10               // mask bounds-check (host-offset)
    ADDQ  SI, R10               // translate to host ptr
    MOVQ  8*12(DI), R11         // count
    // Set up SysV syscall ABI: RAX=nr, RDI=a0, RSI=a1, RDX=a2
    MOVQ  AX, DI                // fd
    MOVQ  R10, SI               // buf (host)
    MOVQ  R11, DX               // count
    MOVQ  $1, AX                // SYS_write on linux/amd64
    SYSCALL
    MOVQ  AX, 8*10(DI_original) // store result to x[10]  (need to restash DI; see real impl)
    RET
```

Real implementation needs to save the incoming `RDI` (CPU ptr) before
clobbering it to set up the syscall. Trivial — stash to `R11` or a
local slot.

`sys_write_darwin_amd64.s` is identical except `MOVQ $0x2000004, AX`
(darwin syscall-4-with-class-2 encoding) and runs on the same kernel
interface. Both files compile under the same Go build tags.

### Exit stub

`sys_exit` and `sys_exit_group` don't issue SYSCALL — we want the emulator
to cleanly return to Go, not terminate the host process.

```
TEXT ·sys_exit(SB), NOSPLIT|NOFRAME, $0-0
    MOVQ  8*10(DI), AX          // a0 = exit code
    MOVQ  AX, CPU_exit_code(DI) // store in a new CPU field
    MOVB  $1, CPU_exit_flag(DI) // set flag
    RET
```

The IR's ECALL emission tests `CPU.exit_flag` after the call; if set,
returns `{pc, ic, jitEcall, 0}` immediately. The existing Go side sees
`jitEcall`, reads `CPU.exit_code`, returns `*ExitError`. This preserves
the current Go exit semantics without any `panic`.

---

## Benchmark Harness (up front)

Before implementing either phase, stand up `cd ~/ris && make hello`
so we can see progress quantitatively at each step. The target runs
a tight inner loop inside a RISC-V guest ELF, times it externally,
and prints per-call ECALL+write cost. Stdout is redirected to
`/dev/null` so the ~10K prints don't scroll the terminal.

**Guest ELFs (hand-written RV64 assembly, no libc):**

- `hello_libriscv.elf` — writes `"Hello, libriscv!\n"` in a 10,000-
  iteration loop, then exits 0.
- `hello_gocpu.elf` — writes `"Hello, Go CPU!\n"` in a 10,000-
  iteration loop, then exits 0.

Both are structurally identical (same loop count, same ECALL
pattern, a single fixed-length write per iteration). Only the
data-section string differs. Different strings let us visually
confirm which emulator produced the output if we ever unredirect.

**Shape (pseudocode):**

```
.rodata: msg: "Hello, ...!\n"
        msg_len = <constant>

_start:
    li   s0, 10000           # iteration count
loop:
    li   a7, 64              # SYS_write
    li   a0, 1               # fd = stdout
    la   a1, msg             # buf
    li   a2, msg_len         # count
    ecall
    addi s0, s0, -1
    bnez s0, loop
    li   a7, 93              # SYS_exit
    li   a0, 0
    ecall
```

**Harness (`~/ris/Makefile`, `hello` target):**

For each of three runners, record wall-clock time, divide by 10K,
print one line.

```
make hello
── prints three lines once both phases are done ─────────────
  libriscv             <X> ns/call   Hello, libriscv!
  GoCPU interpreter    <Y> ns/call   Hello, Go CPU!
  GoCPU direct syscall <Z> ns/call   Hello, Go CPU!
```

After Phase 1 only, the "GoCPU direct syscall" line is absent (2
lines total). After Phase 2, all three lines are present.

**Runners:**

- `libriscv`: the CLI binary built by `make bench-setup` (already
  in the build).
- `GoCPU interpreter`: our emulator with `JIT.InterpOnly = true`
  (already a supported flag — every instruction, including ECALL,
  goes through the Go step path).
- `GoCPU direct syscall`: our emulator with JIT enabled and the
  Phase 2 native-SYSCALL fast path active.

All three redirect stdout to `/dev/null`. Timing is measured in the
host driver (a small Go program `~/ris/cmd/hellobench/` that
`exec`s libriscv and dispatches the two Go variants in-process).
Using a Go driver for all three keeps the timing methodology
identical and lets us report to the same precision.

**Why wall-clock over `go test -bench`:** the libriscv side is a
separate process. A unified Go driver that times `cmd.Run()` for
libriscv and direct `cpu.Run()` for our two variants gives one
consistent number per runner. 10K is plenty of iterations — at
~50–500 ns/call, total runtime is 0.5–5 ms, much larger than
process startup (~1 ms for libriscv; near zero in-process).

---

## Phase Plan

Each phase produces measurable harness output we can compare.

### Phase 1 — Benchmark harness + "Hello, World!" via Go path (2 lines)

**Goal:** bring `make hello` online. Prove end-to-end plumbing.
Emit the first two lines: libriscv and GoCPU-interpreter.

1. Add a `RunWithLinuxOS(cpu, stdout io.Writer)` convenience in
   `os.go` that installs `LinuxExit` (93/94) and
   `LinuxWriteHandler` (64).
2. Build the two guest ELFs under `testdata/hello/` from hand-
   written `.S` files. Use a minimal linker script or
   `riscv64-unknown-elf-gcc -nostdlib -static -T link.ld` to
   produce tiny static ELFs.
3. Write `~/ris/cmd/hellobench/main.go` — the unified timing
   driver. It:
   - exec's libriscv on `hello_libriscv.elf` (stdout to /dev/null),
     times with `time.Now` bracket.
   - in-process: runs `hello_gocpu.elf` through our emulator with
     `InterpOnly = true`, stdout piped to `io.Discard`.
   - prints two aligned lines: `<runner>  <ns/call>  <last-line-of-
     actual-output>` (captured-but-discarded).
4. Add `~/ris/Makefile` target `hello:` that builds the ELFs
   (lazily if needed) and runs the driver.
5. Add a unit test `hello_test.go` in the main repo that runs the
   GoCPU ELF through `RunJIT` with `RunWithLinuxOS`, captures the
   buffer, and asserts it equals `"Hello, Go CPU!\n"` × 10000.
   (Correctness gate for the ELF; separate from the harness.)

**Files touched/new:** `os.go` (new `RunWithLinuxOS`),
`testdata/hello/hello_libriscv.S`,
`testdata/hello/hello_gocpu.S`, `testdata/hello/link.ld`,
built `testdata/hello/hello_libriscv.elf` +
`testdata/hello/hello_gocpu.elf`, `~/ris/cmd/hellobench/main.go`,
`~/ris/Makefile` (new `hello` target), `hello_test.go`.

### Phase 2 — Native SYSCALL fast path (adds the 3rd line)

**Goal:** drop ECALL cost into the 10–50 ns range. Populate the
"GoCPU direct syscall" line in `make hello`.

1. **Freeze the `CPU` layout** the stubs depend on. Add a comment
   block in `cpu.go` locking the offsets of `x[0]`, `exit_code`,
   `exit_flag`. Add the two new fields (`exit_code uint64`,
   `exit_flag uint8`) where padding allows. Add a test using
   `unsafe.Offsetof` to lock offsets and catch accidental shifts.
2. **Write `internal/syscalls/stubs_linux_amd64.s` and
   `stubs_darwin_amd64.s`.** Start with four stubs: `sys_write`,
   `sys_read`, `sys_exit`, `sys_exit_group`. NULL/fallback thunk
   for everything else.
3. **Add `internal/syscalls/table.go`** — a Go package that exposes
   `Table() [NR_SYSCALLS]uintptr` using `//go:linkname` /
   `FuncPC` to obtain each stub's address (same pattern as
   `jitcall`).
4. **Wire into the JIT.** In `jit.go`, during `NewJIT`, call
   `syscalls.Table()` and stash the base pointer on the JIT object.
5. **Emit native ECALL dispatch** in `jit_emit_ir.go`:
   - Emit IR to load `a7` from `x[17]`.
   - Emit IR to bounds-check (guard: if `a7 >= NR_SYSCALLS` or
     slot is NULL, return `jitEcall` to fall back).
   - Emit IR to load the stub address from `dispatch_base[a7*8]`.
   - Set up System V args (CPU ptr, mem_base, mem_mask) via `IRMov`.
   - `IRCall scratch` — `lowerCall` already handles live-register save.
   - After the call, test `CPU.exit_flag`; if set, return `jitEcall`.
   - Otherwise, continue the block.
6. **Extend the harness.** Add a third runner to
   `~/ris/cmd/hellobench/main.go`: same GoCPU ELF, but with
   `InterpOnly = false` (JIT on) and the new fast-path table
   installed. `make hello` now prints 3 lines.
7. **Leave the TCC path alone** for now. TCC blocks still exit on
   ECALL via the old `jitEcall` path. Only the native IR path gets
   the fast syscall. A later phase can port this to TCC if that path
   remains important.

**Success criteria:** the third line in `make hello` is <100 ns/call
on linux/amd64 and darwin/amd64, within 2–3× of libriscv's line.
Correctness: all existing riscv-tests and oracle fuzzing stay green.

**Files touched:** `cpu.go` (new fields + offset-lock test), new
`internal/syscalls/stubs_linux_amd64.s`, new
`internal/syscalls/stubs_darwin_amd64.s`, new
`internal/syscalls/table.go`, `jit.go` (register table on init),
`jit_emit_ir.go` (ECALL emission), `~/ris/cmd/hellobench/main.go`
(third runner).

### Phase 3 (optional, deferred) — Broaden the stub set

Extend `internal/syscalls/stubs_*.s` with more syscalls as guest
workloads demand them: `brk`, `close`, `clock_gettime` (candidate for
vDSO fast path), `open`, `fstat`, `mmap`, `rt_sigprocmask`, etc. Each
stub is ~10 lines of asm. Darwin number translation is mechanical.
For complex syscalls (anything touching file descriptors the emulator
manages, multi-threading primitives, signal state) leave the slot NULL
and let the existing Go `SyscallHandler` pick it up — that's the whole
point of the hybrid design.

---

## Verification

- **Primary:** `cd ~/ris && make hello`
  - After Phase 1 — 2 lines, libriscv and GoCPU-interpreter, both
    printing the correct strings (verified via tee of stdout before
    /dev/null redirection in a one-off manual check).
  - After Phase 2 — 3 lines, GoCPU-direct-syscall line <100 ns/call
    on both linux/amd64 and darwin/amd64, within 2–3× of libriscv.
- `go test -run TestHello ./...` asserts the captured guest output is
  `"Hello, Go CPU!\n"` repeated 10,000 times and exit code is 0.
- Sanity: `make test` — existing riscv-tests ELFs must still pass
  through both JIT paths.
- `make fuzz-oracle` — oracle fuzzing still green.

---

## Critical files

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cpu.go`
  — add `exit_code`, `exit_flag`; freeze layout for stub offsets.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/os.go`
  — add `RunWithLinuxOS`; keep Go fallback handlers.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit.go`
  — call `syscalls.Table()` in `NewJIT`, stash base pointer.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit_ir.go`
  — ECALL emission: dispatch through table, handle exit flag.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/emit.go:213`
  — `Call(sym, addr)` already supports external-symbol calls.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/lower_amd64.go:1945`
  — `lowerCall` already emits the live-register-saved CALL.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/internal/jitcall/call_amd64.s`
  — reference for Go-asm + System V ABI patterns we will mirror.

New files:

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/internal/syscalls/stubs_linux_amd64.s`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/internal/syscalls/stubs_darwin_amd64.s`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/internal/syscalls/table.go`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/testdata/hello/hello_libriscv.S`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/testdata/hello/hello_gocpu.S`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/testdata/hello/link.ld`
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/testdata/hello/hello_libriscv.elf` (built)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/testdata/hello/hello_gocpu.elf` (built)
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/hello_test.go`
- `/Users/jaten/ris/cmd/hellobench/main.go`
- `/Users/jaten/ris/Makefile` — new `hello` target (edit if file exists, create if not)

Reference (read-only):

- `~/ris/xendor/libriscv/lib/libriscv/tr_emit.cpp:744` — syscall
  emission pattern in translated blocks.
- `~/ris/xendor/libriscv/lib/libriscv/tr_translate.cpp:1348` —
  `api.system_call` callback shape.
