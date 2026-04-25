# JIT Sandbox — Go/C Shared-Memory IPC, claude's take.

Zero-CGo-per-call interpreter dispatch for a JIT sandbox, communicating
between a permanently-parked C thread and Go goroutines via a lock-free
SPSC ring buffer backed by `mmap`.

## Architecture

```
Go goroutine                         C interpreter thread
     │                                       │
     │  ring_push() — atomic store           │
     │──────────────────────────────────────►│
     │                                       │  dispatch(item)
     │  poll state == DONE — atomic load     │
     │◄──────────────────────────────────────│
     │                                       │
```

The CGo tax is paid **exactly once** at startup (`Init()`), which locks
an OS thread and hands it permanently to `interpreter_thread_main()`.
All subsequent communication is via shared memory — no CGo, no syscall,
no Go scheduler involvement on the hot path.

## Files

```
c/
  sandbox.h           — types, platform abstractions (futex, CPU_RELAX), API
  sandbox.c           — ring buffer, guard-page mmap, interpreter loop

go/
  sandbox.go          — CGo bridge; Go-side Ring and SandboxMem handles;
                        zero-CGo Call() using unsafe atomic ops on shared ring

asm/
  trampoline.go       — Go declarations for assembly functions (no body)
  trampoline_amd64.s  — x86-64: SwitchStack, GetG, CPURelax, fences
  trampoline_arm64.s  — arm64:  SwitchStack, GetG, CPURelax, fences
```

## Key Design Decisions

### SPSC Ring Buffer
- Head and tail on **separate 64-byte cache lines** — eliminates false sharing
- Work items are exactly **64 bytes** (one cache line) — no torn reads
- Power-of-2 capacity — index wrapping is a bitmask, not a modulo

### Adaptive Back-off (C thread)
1. **Spin** (<1000 iters): `PAUSE`/`YIELD` hint — ~1 ns/iter, zero latency
2. **Yield** (<5000 iters): `sched_yield()` — gives up timeslice
3. **Sleep**: `futex_wait` / `__ulock_wait` — woken by Go side on push

### Guard Pages
Every memory region (data, stack) is flanked by `PROT_NONE` pages.
Any out-of-bounds read or write triggers `SIGSEGV` immediately, with
`si_code == SEGV_ACCERR` (distinguishable from unmapped-page faults).

### W^X Code Pages
Code is mapped `PROT_READ|PROT_WRITE` during JIT compilation, then
`sandbox_seal_code()` flips it to `PROT_READ|PROT_EXEC`. The page is
never simultaneously writable and executable.

### Stack Switch Safety
Before calling `SwitchStack()`, the goroutine MUST update `g.stack.lo`
and `g.stack.hi` to reflect the new stack bounds. Failure to do so will
cause the Go GC to panic during stack scanning (`gentraceback` validates
that frame pointers remain within `[g.stack.lo, g.stack.hi]`).

## Portability

| Feature          | Linux                  | Darwin (macOS/iOS)          |
|------------------|------------------------|-----------------------------|
| Anonymous mmap   | `MAP_ANONYMOUS`        | `MAP_ANONYMOUS`             |
| Sleep primitive  | `futex(FUTEX_WAIT)`    | `__ulock_wait`              |
| Wake primitive   | `futex(FUTEX_WAKE)`    | `__ulock_wake`              |
| Spin hint x86    | `PAUSE`                | `PAUSE`                     |
| Spin hint arm64  | `YIELD`                | `YIELD`                     |
| ICache flush     | `__builtin___clear_cache` | `__builtin___clear_cache` |

## Approximate Latencies

| Mechanism              | Round-trip latency  |
|------------------------|---------------------|
| CGo call (each time)   | ~100–200 ns         |
| SPSC ring (hot/spin)   | ~10–30 ns           |
| Ring + futex sleep     | ~1–4 µs             |
| Local function call    | ~1 ns               |

The spinning ring gets you within one order of magnitude of a local
function call — the irreducible cost is cache coherency propagation
across cores (~10 ns on modern hardware).

## Building

```bash
# C only (for testing the ring/guard page logic standalone)
clang -std=c11 -O2 -o sandbox_test c/sandbox.c -lpthread   # Linux
clang -std=c11 -O2 -o sandbox_test c/sandbox.c              # Darwin

# As part of a Go module (CGo picks up sandbox.c automatically via
# the cgo directives in go/sandbox.go)
go build ./...
```

## Go Version Note

`GetG()` returns R14 (amd64) or R28 (arm64) which is the dedicated `g`
register introduced in Go 1.17's register-based calling convention.
The `g.stack` field offsets are **runtime-internal and not stable**.
Pin your Go toolchain version (`go.toolchain` in `go.mod`) if you
access `g` fields directly via unsafe pointer arithmetic.
