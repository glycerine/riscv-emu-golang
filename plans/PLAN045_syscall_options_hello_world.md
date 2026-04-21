# Plan: Fast Guest Syscalls — "Hello, World!" Without Losing JIT Speed

## Context

This is the first real OS-interaction milestone for the emulator: getting guest
code to do `write(1, "Hello, World!\n", 14)` and see it appear on our terminal,
while preserving the JIT hot-path throughput we have today.

The motivating constraint: **libriscv reports ~3 ns per host↔guest boundary
crossing**. That number is the linchpin of the whole project. If a syscall
costs microseconds, an I/O-heavy guest pays thousands of times more per call
than on libriscv. The emulator's value proposition evaporates. So syscalls
must be cheap.

Today our ECALL path exits the JIT block, returns through `jitcall.Call`,
walks the NoteChain, and invokes a Go `SyscallHandler` under the OS
personality. Estimated cost: **~2–12 µs per ECALL** — dominated by the
JIT→Go transition and map-based handler lookup. That's ~1000× libriscv's
number. The user wants to know if we can close that gap, and if so, how.

The user explicitly asked to:

1. Verify the claim that libriscv dispatches syscalls **directly from JIT code**
   (not via a trampoline back to the C++ interpreter).
2. Brainstorm options — not commit to one — so this plan is for discussion.

---

## How libriscv Achieves ~3 ns (verified)

The claim is correct. When the binary-translator (TCC mode) emits an ECALL,
it does **not** return to the C++ emulator. It emits an inline call:

```c
// libriscv/tr_emit.cpp:744  (binary-translated block, TCC-compiled)
max_ic = api.system_call(cpu, 0x<pc>, ic, max_ic, sysno);
```

`api.system_call` is a plain C function pointer (registered via
`tcc_add_symbol` equivalent) pointing at a lambda in
`libriscv/tr_translate.cpp:1348`:

```cpp
.system_call = [](CPU<W>& cpu, addr_t pc, uint64_t ic, uint64_t max_ic, int sysno) -> uint64_t {
    cpu.machine().set_instruction_counter(ic);
    cpu.registers().pc = pc;
    cpu.machine().system_call(sysno);   // → syscall_handlers[sysno](*this)
    ...
}
```

Which reduces to a **single indirect call through an array**:

```cpp
// libriscv/machine_inline.hpp:79
syscall_handlers[sysnum](*this);
```

The handler itself (e.g., `syscall_write` in `linux/system_calls.cpp`) is a
plain C++ function that:

1. Reads guest args directly from `cpu.reg(10..15)` (plain array access).
2. Calls the host's libc `write()`.
3. Writes the return value to `cpu.reg(10)`.
4. Returns.

Control never leaves native code. Everything is one process, one address
space, pure C/C++ — no language boundary. **The ~3 ns is the cost of an
indirect function call plus a couple of array loads.** The actual
`write(2)` syscall itself adds the normal kernel-entry cost (~100–300 ns
on Linux) on top of that 3 ns dispatch.

Key point: **libriscv is not doing magic.** It's just avoiding language
boundaries. The 3 ns is simply "what a C function pointer call costs when
nothing else is in the way."

---

## Our Current State (verified)

- JIT emits ECALL as `return (JITResult){pc, ic, 1, 0};` — exits the block.
- `jitcall.Call` returns to Go with `Status=jitEcall`.
- `RunJIT` constructs a `Note` and calls `cpu.Notes.Deliver(cpu, n)`.
- `OS.Handle` does `o.syscalls[args.Num]` — **map lookup** — then calls the
  Go `SyscallHandler`, which copies guest memory and calls a `WriteFunc`.
- All of this is Go. Cost ≈ 2–12 µs.

We already have **one-way** zero-CGO efficiency in the right direction:
`internal/jitcall/call_amd64.s` calls JIT code from Go in ~5–10 ns with no
CGO. We also already use `tcc_add_symbol` to inject C helpers (`jit_sqrt`,
`jit_sqrtf`, `jit_trace`) into the JIT's address space. **`jit_trace`
itself already calls raw `write(2)` from inside JIT code** (jit_tcc.go:20–34).
We are one architectural step away from production-quality syscalls.

---

## The Core Insight

The user asked "can we call Go directly from JIT like libriscv does?"
Answer: **we shouldn't try.** The honest observation is:

> libriscv is fast because it doesn't cross language boundaries. We can
> match its speed by also not crossing language boundaries — but the
> other way. **Don't ask C to call Go. Ask C to call C.**

Syscall handlers are just libc/kernel wrappers. They don't need to be Go
functions. If we write them in C and register them with `tcc_add_symbol`
(the pattern we already use for `jit_sqrt`), then the JIT's ECALL site
becomes an ordinary indirect call — identical in shape to libriscv's
`api.system_call`. The Go runtime is never involved on the hot path.

The Go-side `OS`/`SyscallHandler` machinery remains — but as a **fallback**
for syscalls that are too complex to implement in C (anything needing
goroutines, channels, maps, or Go-managed state).

This gets us to **~10–30 ns per ECALL** on the C fast path (function
pointer + dispatch + libc syscall entry), and keeps ~2 µs as the graceful
fallback for cold/complex syscalls. That is the libriscv shape, matched.

---

## Options (brainstorm — for discussion)

Each option is an independent choice for the "fast syscall path." They
can be combined.

### Option A — C syscall table, TCC-compiled handlers (recommended base)

- Add `jit_syscall.c` next to `jit_trace`/`jit_sqrt` in `jit_tcc.go`.
- Define a static C array `syscall_handler_t handlers[NR_SYSCALLS]`.
- Each supported syscall is a C function: `void sys_write(CPU*)`,
  `void sys_exit(CPU*)`, etc. They read guest regs from `cpu->x[]`,
  call libc, write back to `cpu->x[10]`.
- Register the table base with `tcc_add_symbol(s, "jit_syscall_table", ...)`.
- JIT emits for ECALL:
  ```c
  jit_syscall_table[cpu->x[17]](cpu);  // 1 load + 1 indirect call
  ```
- **Speed:** ~10–30 ns per ECALL, cross-platform, no CGO.
- **Complexity:** moderate — need to factor a stable `CPU*` layout for C.
- **Limitation:** handlers run on the JIT goroutine's stack; they must not
  call back into Go, must not block indefinitely, must not allocate via Go.
  libc calls are fine.

### Option B — Direct `SYSCALL` instruction (Linux-only, fastest)

- For syscalls with identical Linux-host and RISC-V-guest calling conventions
  (write, read, close, exit, clock_gettime, …), emit raw `SYSCALL` inline:
  ```c
  register long rax asm("rax") = 1;   // SYS_write
  register long rdi asm("rdi") = cpu->x[10];
  register long rsi asm("rsi") = cpu->x[11] + (uint64_t)mem_base;
  register long rdx asm("rdx") = cpu->x[12];
  asm volatile("syscall" : "+r"(rax) : "r"(rdi),"r"(rsi),"r"(rdx) : "rcx","r11","memory");
  cpu->x[10] = rax;
  ```
- **Speed:** ~15 ns dispatch + syscall entry. No function call at all.
- **Limitation:** Linux-only. Darwin has no stable syscall ABI; you must go
  through libSystem.
- **Use case:** Linux production deployments. On darwin, Option A naturally
  falls back to libc wrappers for the same speed class.

### Option C — Hybrid: C fast path + Go fallback

- Option A's table has NULL slots for unimplemented syscalls.
- On NULL, the dispatcher returns a `jitEcall` status the JIT trampoline
  already understands — existing NoteChain path handles it.
- Best of both: implement 10–20 hot syscalls in C; everything else works
  via the existing Go layer.
- This is the **minimum viable path** and the recommended first cut.

### Option D — "Batched" / deferred writes

- For fd 1/2, ring-buffer the bytes inside C memory and flush lazily
  (on newline, on buffer fill, on block boundary).
- Reduces ECALL-per-byte scenarios (e.g., character-at-a-time stdio).
- **Risk:** changes observable semantics — errors appear late, `fsync`
  becomes lossy. Don't default to this; offer it as an opt-in for benchmarks.

### Option E — vDSO fast paths

- `clock_gettime`, `gettimeofday`, `getcpu` can resolve via the Linux vDSO
  with no kernel entry — ~5 ns total.
- Low priority for hello-world but a natural extension for real workloads.

### Option F — Memory-mapped I/O via fault handler (exotic, not recommended)

- Reserve a guest physical range; writes there trap to a host handler that
  emits bytes. Avoids ECALL entirely.
- Fast but invasive, non-POSIX, not how real guests are built. Listed for
  completeness only — not a serious candidate.

### Option G — Keep Go handlers, optimize the NoteChain (baseline)

- Replace the map lookup with a dense array, cache the handler pointer,
  skip note construction for known-handled syscalls.
- Gets us maybe 3–5× faster (~400 ns–1 µs), still 100× libriscv.
- Useful as a **sanity check** before committing to the C-handler path
  — if this gets us "good enough," the C-handler work isn't worth it.
- Recommended as a quick prerequisite benchmark step.

---

## Recommended Approach

A two-phase plan. Each phase produces a measurable outcome the user can
evaluate before committing to the next.

### Phase 1 — "Hello, World!" via the existing Go path (fast to ship, establishes baseline)

**Goal:** prove the plumbing end to end. Produce the baseline ECALL cost
for comparison.

1. Add a `RunWithLinuxOS(cpu, stdout io.Writer)` convenience in `os.go`
   that installs `LinuxExit` (93/94) and `LinuxWriteHandler` (64).
2. Build a minimal hello-world guest ELF (RV64, static, no libc — just
   inline asm for `li a7, 64; ecall; li a7, 93; ecall`). Put it under
   `testdata/hello/`.
3. Add a unit test that runs the ELF through `RunJIT` with
   `RunWithLinuxOS` and asserts the captured buffer equals
   `"Hello, World!\n"`.
4. Add a benchmark `BenchmarkECall_Go` that runs a tight ECALL loop
   (write of 0 bytes to fd 3) and reports ns/op.

**Files touched:** `os.go`, new `testdata/hello/hello.S`+`hello.elf`,
new `hello_test.go`, new `bench/ecall_bench_test.go`.

### Phase 2 — C syscall fast path (matches libriscv shape)

**Goal:** get ECALL cost into the 10–50 ns range on both Linux and
darwin.

1. **Define a stable CPU ABI** the C side can rely on. Add a
   `// go:generate` or a hand-maintained comment in `cpu.go` that freezes
   the offsets of `x[0]`, `f[0]`, `fcsr`, `pc` in `CPU`. The JIT already
   treats these as raw pointers; we now need C to dereference them by
   offset.
2. **Write `jit_syscall.c`**, included in the `jit_tcc.go` C preamble
   alongside `jit_trace`. It defines:
   ```c
   typedef struct CPU CPU;  // opaque — access via cpu_reg(cpu, n)
   typedef void (*sys_fn)(CPU*, char *mem_base, uint64_t mem_mask);
   extern sys_fn jit_syscalls[512];   // installed via tcc_add_symbol
   ```
   Plus concrete handlers:
   - `sys_write` — reads a0/a1/a2 from cpu, bounds-checks via `mem_mask`,
     calls libc `write`, stores result in a0.
   - `sys_exit` / `sys_exit_group` — stores exit code somewhere the Go
     side can pick up, sets a flag that causes the JIT to exit cleanly.
     (This is the trickiest piece: exit needs to unwind to Go.)
   - `sys_brk`, `sys_close` (probably stubbed for now).
3. **Install the table** from Go at `JIT` init. Add a field
   `syscallTable [512]uintptr` on `JIT`, populate from a C-ABI list, pass
   its base into TCC via `tcc_add_symbol(s, "jit_syscalls", ...)`.
4. **Change JIT emission for ECALL** in `jit_emit.go:291`. Instead of
   `emitReturn(pc, jitEcall)`, emit (TCC path only for now):
   ```c
   jit_syscalls[x[17] & 0x1FF](cpu_ptr, mem_base, mem_mask);
   ```
   Table entries for unhandled numbers are a thunk that returns
   `jitEcall` → NoteChain fallback (Option C hybrid).
5. **Handle "exit" specially.** `sys_exit` can't just `return` — the JIT
   block needs to stop executing and propagate to Go. Cleanest path:
   set a flag in the CPU struct that the JIT checks after the syscall
   call, and if set, returns `jitEcall` (so the existing Go exit handler
   still owns the exit semantics).
6. **Extend the native IR path** to emit the same call via
   `ir.Emitter.Call` + `CTab` (already supported per `ir/emit.go:213`
   and `ir/lower_amd64.go:1945`). This gives us no-CGO native-code
   dispatch at matching speed.
7. **Benchmark:** add `BenchmarkECall_FastC` mirroring Phase 1's test.
   Expected: 10–50 ns/op. Compare against libriscv via the existing
   `bench-setup` infrastructure.

**Files touched:** `jit_tcc.go` (add C preamble), `jit_emit.go` (ECALL
emission), `jit_emit_ir.go` (same for IR path), `jit_native.go` /
`ir/emit.go` (table registration), `cpu.go` (ABI freeze comment, exit
flag), `bench/ecall_bench_test.go`.

### Decision point between phases

After Phase 1 benchmark ships: if baseline ECALL cost is (say) 500 ns
and the guest workload does one ECALL per 10k instructions, the total
overhead is negligible and Phase 2 is not worth the architectural weight.
If it's >1 µs or the guest is I/O heavy, Phase 2 pays for itself.

**The user explicitly wants to know before committing.** That's what
this plan is for.

---

## Open Questions for the User

1. **Scope of the fast path.** Do we care about matching libriscv on
   Linux specifically, cross-platform (including darwin, where the
   fastest path is slightly slower), or both?
2. **Exit semantics.** Today `LinuxExit` `panic`s and `RunWithOS`
   recovers. The C fast path can't panic. Is a flag-in-CPU + clean JIT
   exit acceptable, or do we want to preserve the panic idiom?
3. **Guest ABI.** Should the hello-world ELF use the Linux RISC-V
   convention (a7=syscall number, 64=write, 93=exit) or the riscv-tests
   convention we already support? Recommended: Linux, because that's
   what real guests will use.
4. **Which JIT path gets this first — TCC, native IR, or both?** Both
   matter, but the TCC path is a quicker prototype. Proposal: ship TCC
   first (Phase 2 steps 1–5), then port to native IR (step 6).

---

## Verification

- `go test -run TestHello ./...` prints `"Hello, World!\n"` and exits 0.
- `go test -bench BenchmarkECall -benchtime=1s ./bench/` reports ns/op
  for both the Go path and (after Phase 2) the C fast path.
- Sanity: run an existing riscv-tests ELF through both paths; no
  regressions.
- Optional: compare against `xendor/libriscv` using the same hello-world
  ELF. `make bench-setup` already clones and builds libriscv.

---

## Critical files

- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_tcc.go`
  — C preamble, `tcc_add_symbol` registration point.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit.go:291`
  — ECALL emission in TCC path.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit_ir.go`
  — ECALL emission in native IR path (needs the same change).
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/os.go`
  — Go syscall layer (keep as fallback; add `RunWithLinuxOS`).
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cpu.go`
  — freeze `CPU` field offsets for the C ABI; add exit flag.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/emit.go:213`
  — `Call(sym, addr)` already supports external-symbol calls in IR.
- `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/ir/lower_amd64.go:1945`
  — `lowerCall` already emits the indirect CALL with live-register save.
- `~/ris/xendor/libriscv/lib/libriscv/tr_emit.cpp:744` — reference pattern
  for syscall emission in translated blocks.
- `~/ris/xendor/libriscv/lib/libriscv/tr_translate.cpp:1348` — reference
  pattern for the `api.system_call` callback.
