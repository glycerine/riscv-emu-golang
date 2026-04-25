# JIT Sandbox — Go/C Shared-Memory IPC. from claude.

Zero-CGo-per-call interpreter dispatch. The CGo tax is paid **once** at
`NewRing()`. After that, all Go↔C communication uses atomic operations on
a lock-free SPSC ring buffer backed by `mmap` — no CGo, no syscall on the
hot path.

## Files (flat layout)

```
sandbox.h             C types, futex abstraction (Linux/Darwin), API declarations
sandbox.c             Ring buffer, guard-page mmap, interpreter loop w/ adaptive sleep
sandbox.go            CGo bridge + Go-side Ring/SandboxMem API (owns all CGo)
sandbox_test.go       Pure-Go tests and benchmarks (no CGo)
trampoline.go         Go declarations for assembly functions (no body)
trampoline_amd64.s    x86-64: SwitchStack, GetG, CPURelax, memory fences
trampoline_arm64.s    arm64:  SwitchStack, GetG, CPURelax, memory fences
```

## Why the split between sandbox.go and sandbox_test.go?

Go forbids `import "C"` in `_test.go` files. All CGo lives in `sandbox.go`
(a regular package file). The test file imports the package normally and
sees only Go types — `Ring`, `SandboxMem` — with no C types leaking through.

## Building

```bash
# Run all tests
go test -v -count=1 .

# Run benchmarks (5 s each)
go test -bench=. -benchtime=5s -benchmem .

# Build the package (checks compilation only)
go build .
```

No special flags needed. CGo picks up `sandbox.c` automatically via the
`#include "sandbox.c"` directive in the preamble of `sandbox.go`.

## Architecture

```
Go goroutine                      C interpreter thread (locked OS thread)
     │                                        │
     │  ring.Push()  — atomic store + futex   │
     │────────────────────────────────────────►│
     │                                        │  dispatch(item)
     │  ring.WaitResult() — atomic poll       │
     │◄────────────────────────────────────────│
```

### Adaptive idle back-off (C thread)

```
Work arrives → reset to SPINNING
      │
      ▼
  SPINNING   spin < 1000       PAUSE/YIELD hint, ~1–5 ns/iter
      │
      ▼
  YIELDING   spin 1000–5000    sched_yield(), ~1 µs/iter
      │
      ▼
  DEADLINE?  every ~1000 yields
      ├─ < 100 ms → back to YIELDING
      └─ ≥ 100 ms → SLEEPING (futex_wait indefinitely)
                         │
                    woken by Push() futex_wake
```

### Guard pages

Data and stack regions are flanked by `PROT_NONE` pages. Out-of-bounds
access triggers `SIGSEGV` with `si_code == SEGV_ACCERR`.

### W^X enforcement

Code pages are `PROT_READ|PROT_WRITE` during JIT compilation. `SealCode()`
flips them to `PROT_READ|PROT_EXEC`. Never simultaneously writable and
executable.

### Stack switching (trampoline)

`SwitchStack()` swaps RSP. The caller **must** update `g.stack.lo` /
`g.stack.hi` before calling, or the Go GC will panic. See comments in
`trampoline.go` and `trampoline_amd64.s`.

## Approximate latencies

| Mechanism              | Round-trip  |
|------------------------|-------------|
| CGo call each time     | ~100–200 ns |
| SPSC ring (hot/spin)   | ~10–30 ns   |
| Ring + futex wake      | ~1–4 µs     |
| Local function call    | ~1 ns       |

## Portability

| Feature          | Linux                     | Darwin                    |
|------------------|---------------------------|---------------------------|
| Anonymous mmap   | `MAP_ANONYMOUS`           | `MAP_ANONYMOUS`           |
| Futex sleep      | `SYS_futex`               | `__ulock_wait/wake`       |
| Spin hint x86    | `PAUSE`                   | `PAUSE`                   |
| Spin hint arm64  | `YIELD`                   | `YIELD`                   |
| ICache flush     | `__builtin___clear_cache` | `__builtin___clear_cache` |

## Go version note

Pin your Go toolchain (`go.toolchain` in `go.mod`) if you access `g` struct
fields directly — their offsets are runtime-internal and not stable across
versions. `GetG()` returns R14 (amd64) or R28 (arm64), valid for Go ≥ 1.17.

## Measurement

~~~
go test -run=xxx -bench='BenchmarkRoundTrip$|BenchmarkCGO$' -benchtime=3s
~~~

The story thus far:

CGO is 34 ns, ring round-trip is 188 ns. CGO is 5.6x 
faster for a simple call. The ring's cross-thread wake 
latency dominates — even with pure atomic spin, the 
producer stores to head and the consumer's cache line 
has to see it, plus the consumer writes the result back.
      
The ring approach only wins when you're amortizing 
the CGO entry/exit across many batched
operations, or when the C-side work is substantial 
enough to overlap with the cross-thread
transit. For a single trivial call, CGO's same-thread 
function call can't be beat.
