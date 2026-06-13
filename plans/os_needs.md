Question:

I am interested in doing Deterministic Simulation Testing on Go, and so I am looking at how to get Go to run on a single thread. Since the Go runtime resists this, and I want to minimize the number and size of runtime changes, I am thinking about how the Go code could be run on single CPU or single CPU emulator like my interpreter for RISCV64. It seems easy to me to simple allocate a thread with a distinct stack of memory. What operating system primitives does Go rely on to a) create new threads; and b) switch between them? These are the OS calls that my emulator would need to provide.

Answer:
This is a well-targeted question. Let me break down exactly what Go's runtime does at the OS boundary for thread creation and context switching, which maps cleanly onto what your RISC-V emulator needs to intercept/emulate.

## Thread Creation

Go creates OS threads via the `clone(2)` syscall on Linux (not `pthread_create` — Go avoids libc entirely). The relevant call is in `runtime/os_linux.go` and the assembly stubs:

```
clone(
    CLONE_VM | CLONE_FS | CLONE_FILES | CLONE_SIGHAND |
    CLONE_SYSVSEM | CLONE_THREAD,
    newstack,    // new SP
    mp,          // *m, passed via ctid/tls
    g0,          // *g, for the new thread's g0
    fn           // thread start function
)
```

Key flags: `CLONE_VM` (shared address space) and `CLONE_THREAD` (same thread group). The new thread starts executing `runtime/tls_linux_amd64.s:settls` then falls into `runtime.mstart`.

On RISC-V 64 Linux, the syscall number is **220** (`__NR_clone`). The ABI passes the start function and stack in a way that the kernel directly jumps to it after cloning.

For your emulator: you need to handle `clone` with at least `CLONE_VM|CLONE_THREAD` and spin up a new emulated hart sharing the same memory image. Since you're targeting single-thread determinism, you could intercept `clone` and instead schedule the new "thread" cooperatively on your single hart.

## Context Switching (M:N Scheduler)

Go's goroutine switching does **not** use OS context-switch primitives — it's entirely in userspace via `runtime·mcall` and `runtime·gogo` (in `runtime/asm_riscv64.s`). These save/restore the set of callee-saved registers + PC + SP into `g.sched` (a `gobuf`):

```go
type gobuf struct {
    sp   uintptr   // stack pointer
    pc   uintptr   // program counter
    g    guintptr  // the g itself
    ctxt unsafe.Pointer
    ret  uintptr
    lr   uintptr   // link register (for arm/riscv)
    bp   uintptr   // frame pointer
}
```

On RV64, callee-saved registers are `s0`–`s11` (plus `ra`, `sp`). `mcall` saves these into `g.sched` and switches to `g0`'s stack; `gogo` restores them. **No OS involvement whatsoever** — this is pure register save/restore in assembly.

The OS *is* involved in two preemption mechanisms, however:

### 1. Signal-based Preemption (`SIGURG`)
Go sends `SIGURG` to a thread (via `tgkill(2)`, syscall **234** on RV64) to preempt goroutines at safe points. The signal handler (`runtime/signal_unix.go`) injects an async preemption by rewriting the PC in the `ucontext` to `asyncPreempt`. For your emulator:
- You need `tgkill` (or can stub it if you intercept the scheduler loop)
- You need signal delivery — `rt_sigaction` (**134**), `sigaltstack` (**132**), and the `ucontext_t` structure for RV64

### 2. `sched_yield` / Futex (OS thread parking)
When an M has no work, it parks via `futex(2)` (**422** on RV64) in `runtime.notesleep`/`notewakeup`. This is the **only** true OS blocking primitive the Go scheduler uses for M-parking. You also see `sched_yield` (**124**) in some spin loops.

## Minimal Syscall Surface for Single-Threaded DST

If your goal is to collapse everything onto one hart and drive scheduling yourself, here's the minimal set:

| Syscall | Number (RV64) | Purpose | DST strategy |
|---|---|---|---|
| `clone` | 220 | Create OS thread (new M) | Intercept → register cooperative fiber |
| `futex` | 422 | Park/unpark M threads | Intercept → your scheduler's wait queue |
| `tgkill` | 234 | Deliver SIGURG for preemption | Intercept → inject preemption directly |
| `rt_sigaction` | 134 | Register signal handlers | Passthrough (store handler table) |
| `sigaltstack` | 132 | Alternate signal stack | Passthrough or no-op |
| `sched_yield` | 124 | Yield hint in spin loops | Intercept → yield to your scheduler |

The goroutine-level switching (`mcall`/`gogo`) runs entirely in your emulated RV64 instruction stream with no syscalls — you get that for free.

## The Deeper Insight

What makes this tractable for DST is that Go's M:N model means **goroutine scheduling is already in userspace**. You only need to control the M-level (OS thread) primitives. If you intercept `clone` to run all Ms on your single hart sequentially, and replace `futex` with cooperative yield points in your emulator, you effectively have a deterministic cooperative scheduler where you control the interleaving. The goroutine scheduler on top of that remains unmodified.

This is essentially what `gosimnet`-style approaches do in software — you're just doing it one level lower, at the ISA emulation boundary, which gives you much stronger guarantees because you don't need to modify the Go runtime at all.

-----------
How could my "linux like OS personality" on my single emulator Riscv64 hart do "thread scheduling", or would it not really need to?

This is the key insight worth unpacking carefully.

## You Don't Really Need Thread Scheduling

On a single hart emulator, "thread scheduling" in the traditional sense dissolves. Here's why:

**True parallelism is gone by construction.** There is only one instruction stream. What looks like "threads" to the Go runtime are just multiple register-file + stack contexts that your emulator knows about. You are the scheduler — you decide which context runs next, and nothing runs without your explicit say-so.

Compare this to a real OS on real hardware: the OS scheduler exists because the CPU runs independently and you need to multiplex it. On your emulator, the CPU *is* your emulator loop. It cannot "run away" from you.

## What You Actually Need: Context Multiplexing

When Go calls `clone`, instead of creating a new OS thread, you:

1. Allocate a new register file (the 32 RV64 integer + float registers + PC)
2. Set its `sp` to the new stack you've carved out of emulated memory
3. Set its `pc` to the thread start function
4. Put it in a table of **runnable contexts**

That's it. No OS scheduler needed. You have a table of `{regfile, status}` structs.

```
contexts[] = [
  { regs: {...}, pc: 0x..., status: RUNNING  },  // current
  { regs: {...}, pc: 0x..., status: RUNNABLE },  // parked M
  { regs: {...}, pc: 0x..., status: WAITING  },  // futex-blocked
]
```

## The Only Question: When Do You Switch?

Since you control the fetch-decode-execute loop, you switch contexts whenever *you* decide to. The natural switch points are exactly your intercepted syscalls:

- **`futex(WAIT)`** — save current register file → mark context WAITING → pick another RUNNABLE context → restore its register file → continue execution. This is the big one; it's how Go parks an M that has no goroutines to run.
- **`futex(WAKE)`** — mark the target context RUNNABLE. It won't actually run until you switch to it.
- **`clone`** — allocate new context as described above, mark RUNNABLE.
- **`sched_yield`** — optionally switch to next RUNNABLE context, or just return immediately (legitimate no-op).
- **`tgkill(SIGURG)`** — instead of actually delivering a signal, you can directly inject the preemption into the target context by rewriting its PC to `asyncPreempt`, since you have direct register file access.

You can also switch on a cycle budget — every N emulated instructions, rotate to the next runnable context — which gives you control over interleaving granularity for DST purposes.

## The DST Superpower This Gives You

On real hardware with a real OS scheduler, thread interleaving is nondeterministic. On your emulator:

- You choose *which* context runs next (round-robin, priority, or a recorded schedule)
- You choose *when* to switch (cycle count, syscall boundary, or explicit yield)
- You can **replay** an exact interleaving by recording your switch decisions as a log
- You can **enumerate** interleavings systematically (à la DPOR) by forking emulator state at decision points — since your entire "OS" state is just that context table plus emulated memory

This is strictly stronger than what user-space DST frameworks like `Antithesis` or `go-deadlock` can achieve, because they still depend on the real OS scheduler for actual execution and can only observe, not control, preemption timing.

## What Your "OS Personality" Actually Looks Like

It's surprisingly thin:

```
clone(flags, stack, ...) → allocate context, return tid
futex(addr, FUTEX_WAIT, val) → if *addr == val: park self, switch context
futex(addr, FUTEX_WAKE, n) → mark n waiters on addr as RUNNABLE
tgkill(tid, SIGURG) → rewrite target context PC to asyncPreempt
sched_yield() → no-op or voluntary switch
exit_group() → terminate all contexts
```

The signal machinery (`rt_sigaction`, `sigaltstack`) you need to handle enough to satisfy Go's startup assertions, but for SIGURG-based preemption you can shortcut it entirely by doing the PC rewrite directly since you own the register file.

**You don't need a scheduler. You need a context switcher triggered by a handful of syscalls.** The "scheduling policy" is entirely in your hands as the emulator author, which is precisely the point.
--------


I am hoping that by running on a single hart (emulated riscv64 cpu) that I can avoid having to implement the synchronization instructions in the Riscv64 ISA, because there is no point. Currently they just no-op. But I suspect I would need to implement them to stall certain Go threads / goroutines to force them to block on locked mutexes... or can I just grab scheduling at that point?

This is a sharp observation and the answer is: **you can get away with no-ops, but only if you also intercept the right futex calls — and the tricky part is that Go's mutexes have two layers.**

## Go's Two-Layer Mutex

Go's `sync.Mutex` (and the internal `runtime.mutex`) are not purely futex-based. They use a **spin-then-block** strategy:

```
Layer 1: Atomic CAS on the mutex word (pure userspace, no syscall)
Layer 2: futex(WAIT) when spinning gives up (OS involvement)
```

The atomic CAS on layer 1 is where your no-op problem surfaces.

## What Happens With No-Op Atomics on a Single Hart

On a real multicore machine, atomics (LR/SC, AMO*) provide mutual exclusion across harts. On your single hart, **mutual exclusion is already guaranteed** because only one context runs at a time, and you only switch at your chosen points. So no-op atomics are *semantically correct* for the mutual exclusion property.

The specific sequence for `sync/atomic.CompareAndSwap` compiled to RV64:

```asm
loop:
    lr.w.aq  t0, (a0)       ; load-reserved
    bne      t0, a1, fail   ; check expected
    sc.w.rl  t1, a2, (a0)  ; store-conditional
    bnez     t1, loop       ; retry if SC failed
    ; success
fail:
```

With no-op LR/SC, the SC never spuriously fails, which means **CAS always succeeds on first try**. On a single hart this is fine — there is no concurrent writer, so it *should* always succeed.

## The Real Problem: Spin Loops That Never Yield

Here is where it gets subtle. Go's runtime mutex fast path looks roughly like:

```go
func lock(l *mutex) {
    for {
        v := atomic.LoadUintptr(&l.key)
        if v == mutex_unlocked {
            if atomic.Casuintptr(&l.key, mutex_unlocked, gp) {
                return  // got it
            }
        }
        // spin a bit, then eventually...
        if spin count exhausted {
            futex(FUTEX_WAIT)  // block here
        }
    }
}
```

If context A holds a mutex and context B is spinning on it, context B will spin in a tight atomic loop. With no-op atomics and no preemption, **context B will spin forever** — it never yields, never hits `futex(WAIT)`, and you never get to switch back to context A to release the lock.

So the question is: **when does Go give up spinning and call futex?**

The answer is in `runtime/lock_futex.go`: after a bounded number of `procyield` / `osyield` calls. `osyield` calls `sched_yield`. So you have a hook:

```
sched_yield → your scheduler switches context
```

If you intercept `sched_yield` and actually switch to another runnable context, the spin loop naturally resolves: A runs, releases the lock, B's next CAS succeeds.

## So Your Options Are

**Option 1: Intercept `sched_yield` + keep no-op atomics**

This works if Go always eventually calls `sched_yield` before `futex(WAIT)` in its spin path. In practice it does — `runtime.osyield()` is called in the active spinning phase. You get context switches there, lock holder runs, releases, waiter succeeds. No atomic implementation needed.

The risk: some spin loops in Go's runtime are tighter and may reach `futex(WAIT)` without an intervening `sched_yield`. In that case you fall through to option 2 naturally.

**Option 2: Intercept `futex(WAIT)` + keep no-op atomics (recommended)**

This is the robust path. When a goroutine exhausts its spin budget and calls `futex(FUTEX_WAIT, addr, expected)`:

1. Check `*addr == expected` (the mutex is still locked)
2. Park this context (mark WAITING on `addr`)
3. Switch to another RUNNABLE context
4. Eventually lock holder calls `futex(FUTEX_WAKE, addr)` → you mark the waiter RUNNABLE
5. Waiter resumes, retries CAS, succeeds

This requires no atomic implementation at all. The CAS in the retry after wakeup succeeds trivially because you only run one context at a time and the lock is genuinely free at that point.

**Option 3: Implement LR/SC with reservation tracking**

You'd track a reservation address per hart (you only have one hart, so it's one global slot). SC fails if another AMO touched that address since LR. This is correct but **unnecessary** for your use case — it buys you nothing on a single hart that you don't already get from the scheduling approach.

## The Verdict

You do **not** need to implement the synchronization instructions beyond no-ops, provided you correctly implement `futex(WAIT/WAKE)` and either:

- intercept `sched_yield` to do a real context switch, or
- rely on `futex(WAIT)` as your primary park point (which you need anyway)

The atomic instructions on a single hart with cooperative scheduling at syscall boundaries are semantically vacuous — the "synchronization" they provide is already guaranteed by the fact that only one context runs at a time. Your only obligation is ensuring that spinning contexts eventually yield, which `futex` and `sched_yield` interception gives you.

The no-op atomics are not a hack — they are *correct* for this execution model.

-------------
what other operating system calls does my "linux personality OS" on top of the riscv64 hart emulator need in order for the Go standard library to run?

Good question. Let me think through this systematically by category. Go's runtime does a fairly minimal but specific set of syscalls at startup and during normal operation.

## Startup / Process Initialization

These happen before `main` and must succeed or the runtime panics:

| Syscall | RV64 # | Purpose |
|---|---|---|
| `mmap` | 222 | All heap allocation — Go never uses `brk` |
| `munmap` | 215 | Return memory to OS |
| `mprotect` | 226 | Stack guard pages, executable JIT memory |
| `madvise` | 233 | `MADV_FREE`/`MADV_DONTNEED` on scavenged heap spans |
| `getrlimit` | 163 | Check stack size limit at startup |
| `getpid` | 172 | Runtime uses this for signal targeting |
| `gettid` | 178 | Each M records its own tid |
| `mincore` | 232 | Used by scavenger to check if pages are resident |
| `prctl` | 167 | Sets thread name (`PR_SET_NAME`), optional but called |

`mmap` is by far the most critical — Go's entire allocator is built on it. You need MAP_ANON|MAP_PRIVATE at minimum, and MAP_FIXED for specific placement requests.

## Signal Infrastructure

Go's runtime installs signal handlers very early:

| Syscall | RV64 # | Purpose |
|---|---|---|
| `rt_sigaction` | 134 | Install handlers for SIGURG, SIGSEGV, SIGBUS, etc. |
| `rt_sigprocmask` | 135 | Block/unblock signals on thread creation |
| `sigaltstack` | 132 | Alternate stack for SIGSEGV handler (stack overflow detection) |
| `tgkill` | 234 | Send SIGURG for async preemption |

For your purposes you can stub `rt_sigaction` to record the handler table but not actually deliver signals, then intercept `tgkill` to inject preemption directly into the target context's register file as discussed. `sigaltstack` can be a no-op as long as you never actually deliver SIGSEGV.

## Time

Go's runtime uses time pervasively — timers, scheduler, GC pacing:

| Syscall | RV64 # | Purpose |
|---|---|---|
| `clock_gettime` | 113 | `CLOCK_REALTIME` and `CLOCK_MONOTONIC` — used everywhere |
| `nanosleep` | 101 | Used in `timeSleep`, fallback timer path |
| `timer_create` / `timer_settime` | 107/110 | Not used by Go runtime directly |

`clock_gettime` is called extremely frequently. For DST this is actually your **first major control point** — if you return a deterministic logical clock instead of wall time, you eliminate a huge source of nondeterminism. Your emulator controls time completely.

## File Descriptors / I/O

Go uses the `netpoller` (epoll on Linux) for all async I/O including timers:

| Syscall | RV64 # | Purpose |
|---|---|---|
| `epoll_create1` | 20 | Create epoll instance at runtime init |
| `epoll_ctl` | 21 | Register fds with epoll |
| `epoll_pwait` | 22 | Block waiting for I/O events |
| `pipe2` | 59 | Self-pipe for waking epoll from scheduler |
| `read` | 63 | Pipe reads, stdin |
| `write` | 64 | stdout/stderr — needed immediately |
| `close` | 57 | fd cleanup |
| `fcntl` | 25 | Set O_NONBLOCK on fds |
| `open` / `openat` | 56 | File access |

`write` to fd 1/2 is needed almost immediately for any panic or print output. `epoll_create1` is called during runtime init — if it fails the runtime will fall back but may behave oddly. For DST, epoll is another control point: you control when I/O events are "delivered."

## Memory / GC Support

| Syscall | RV64 # | Purpose |
|---|---|---|
| `mmap` | 222 | (already listed — primary allocator) |
| `madvise` | 233 | Scavenger returns pages |
| `mincore` | 232 | Scavenger residency check |
| `userfaultfd` | 282 | Not used by default Go runtime |

## Process / Thread Identity

| Syscall | RV64 # | Purpose |
|---|---|---|
| `getpid` | 172 | Startup |
| `gettid` | 178 | Per-M identity |
| `set_tid_address` | 96 | Called by runtime for thread-local cleanup |
| `exit` | 93 | Goroutine/thread exit |
| `exit_group` | 94 | Process exit |

## futex (already discussed but complete picture)

| Syscall | RV64 # | Ops used |
|---|---|---|
| `futex` | 422 | `FUTEX_WAIT`, `FUTEX_WAKE`, `FUTEX_WAIT_PRIVATE`, `FUTEX_WAKE_PRIVATE` |

The `_PRIVATE` variants are identical in behavior for your purposes — treat them the same.

## What You Can Safely Stub / No-Op

| Syscall | Reason safe to stub |
|---|---|
| `prctl(PR_SET_NAME)` | Thread naming, purely cosmetic |
| `sigaltstack` | Only matters if you deliver SIGSEGV |
| `getrlimit` | Return a large sane value |
| `mincore` | GC scavenger works without it, just less efficient |
| `madvise` | Return 0, pages stay mapped, GC still correct |
| `rt_sigaction` | Record handlers but don't deliver (except SIGURG which you intercept directly) |

## The Minimal Viable Set

If you want to prioritize, implement these first and everything else second:

```
mmap / munmap / mprotect   — nothing works without these
write                       — you need output immediately  
clock_gettime               — called constantly, must not crash
clone / exit / exit_group   — thread lifecycle
futex                       — M parking
epoll_create1 / epoll_ctl / epoll_pwait  — netpoller init
pipe2                       — netpoller self-wake
rt_sigprocmask              — called during thread init
gettid / getpid             — identity
```

That's roughly 15-20 syscalls to get a Go program to `main`. The long tail of file I/O, network, etc. you can add on demand as your test programs exercise them.

## DST-Specific Observations

Since your goal is deterministic simulation, note that several of these are **nondeterminism injection points** you want to control rather than just implement faithfully:

- `clock_gettime` → return logical time you advance explicitly
- `epoll_pwait` → return events only when your simulator decides to deliver them  
- `getrandom` (RV64 **278**) → return deterministic bytes (Go uses this for map seed and `crypto/rand`)
- `mmap` → deterministic addresses if you control the allocator base

`getrandom` is worth adding to the list — Go calls it at startup to seed the hash map randomization and it will block or fail ungracefully if you don't handle it.

------------
how does Go query for how many CPU's the operating system has?

Go uses two mechanisms, and which one fires depends on the path through runtime init.

## Primary: `sched_getaffinity`

The first thing Go tries is:

```go
// runtime/os_linux.go
func osinit() {
    ncpu = getproccount()
}
```

`getproccount()` calls `sched_getaffinity(0, sizeof(cpumask), &cpumask)` (RV64 syscall **122**) and counts the set bits in the returned CPU affinity mask. This gives the number of CPUs the process is *allowed* to use, which may be less than the physical count (e.g. under cgroups or `taskset`).

For your emulator: return a bitmask with exactly **1 bit set**. Go will then set `GOMAXPROCS=1` by default, which is precisely what you want — it tells the scheduler to use only one P (processor), which means only one goroutine runs at a time at the Go scheduler level. This is a gift: it naturally suppresses a lot of the concurrency you'd otherwise have to manage.

## Fallback: `/proc/stat` or `sysinfo`

If `sched_getaffinity` fails, Go falls back to parsing `/proc/stat` and counting `cpu0`, `cpu1`, ... lines, or on some paths calls `sysinfo` (RV64 **179**). Since you probably don't want to implement a `/proc` filesystem, just make `sched_getaffinity` succeed and return 1.

## The `GOMAXPROCS` Consequence

It's worth being precise about what returning 1 CPU buys you at each layer:

```
sched_getaffinity returns 1 CPU
  → ncpu = 1
    → default GOMAXPROCS = 1
      → only 1 P created
        → at most 1 goroutine running at Go-scheduler level
          → goroutine switches are all cooperative (mcall/gogo)
            → no goroutine-level races possible
```

Combined with your single hart, this means goroutine scheduling is **fully deterministic** without any further intervention at that layer. The remaining nondeterminism sources are M-level (OS thread) events, which you handle via `futex`/`clone` interception.

## Also: `auxv` (ELF Auxiliary Vector)

Go's runtime also reads the ELF auxiliary vector passed by the kernel on process startup, which includes `AT_HWCAP` and related fields. It doesn't get CPU count from here, but it does check for ISA extensions. On RV64 you'd want to populate a minimal `auxv` with:

| Tag | Value |
|---|---|
| `AT_HWCAP` | 0 (or appropriate extension bits) |
| `AT_PAGESZ` | 4096 |
| `AT_PHDR` / `AT_PHENT` etc. | ELF headers if you load a real binary |
| `AT_NULL` | terminator |

Go reads `auxv` via the stack at process entry (not a syscall) — it's the array above `envp` in the initial stack layout. If it's absent or malformed Go will proceed with defaults, but you'll want it correct for a clean init.

## Summary for Your Emulator

Add `sched_getaffinity` (RV64 **122**) to your syscall table, return a single-byte affinity mask of `0x01`, and you're done. This is probably the highest-leverage single syscall in your list because it shapes the entire Go scheduler topology before `main` even runs.

---------
