# jea9linux OS Implementation Architecture and Test Plan

This document is the implementation and testing sequence for `jea9linux`, the
Linux-like OS personality used to run `riscv64/linux` guest programs, especially
Go binaries, on this emulator. The target execution model is deliberately not a
host-threaded Linux clone. It is a deterministic, single-hart simulation: many
guest Linux threads may exist as saved register contexts, but only one guest
context is ever loaded into the `CPU` and executing at a time.

The implementation strategy is TDD-first. Each feature starts with failing unit
tests against the OS state machine, then failing checked-in RISC-V ELF fixture
tests, then implementation. The Go runtime acceptance tests come last. They are
the proof that the personality is complete enough, not the first diagnostic tool
used to discover missing behavior.

The name of the personality is `jea9linux`.

Line-reference policy, 2026-06-14: implementation-status notes in this document
now prefer file names, symbol names, and test names over numeric line
references. Numeric line references in older completion notes went stale as the
implementation evolved; use `rg` on the named symbols for the current location.

## 1. Determinism Controls

Determinism controls are implemented first because every later subsystem depends
on them: clock syscalls, random syscalls, futex timeouts, epoll timeouts, timer
delivery, signal preemption, and context switching. The initial public API should
be explicit and small:

```go
type Jea9LinuxClockMode uint8

const (
	Jea9ClockIdleJump Jea9LinuxClockMode = iota
	Jea9ClockICTick
	Jea9ClockManual
)

type Jea9LinuxOptions struct {
	EntropySeed       []byte
	ClockMode         Jea9LinuxClockMode
	MonotonicStartNS  int64
	RealtimeOffsetNS  int64
	NSPerInstruction  int64
	InstructionBudget uint64
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
}

func NewJea9Linux(opts Jea9LinuxOptions) *Jea9Linux
func InstallJea9Linux(cpu *CPU, os *Jea9Linux) func()
func RunWithJea9Linux(cpu *CPU, os *Jea9Linux) (exitCode int, err error)
func (os *Jea9Linux) AdvanceTime(d time.Duration)
func (os *Jea9Linux) SetMonotonicNS(ns int64)
func (os *Jea9Linux) MonotonicNS() int64
```

`EntropySeed == nil` means use a stable built-in default seed, computed as
`SHA256("jea9linux default deterministic seed v1")`. A non-nil seed is copied
and hashed into an internal 32-byte root seed with
`SHA256("jea9linux entropy root v1" || EntropySeed)`. This avoids host entropy
after construction and lets tests use arbitrary seed byte strings, including an
empty but explicitly supplied seed.

The deterministic PRNG should use only the Go standard library. Use a
SHA-256 counter stream: each block is
`SHA256(root || streamLabel || littleEndian(counter))`. `AT_RANDOM` uses label
`"auxv-random-v1"` and does not consume bytes from the syscall random stream.
`getrandom`, `/dev/urandom`, and `/dev/random` use label
`"sys-random-v1"` and consume one shared sequential stream. This makes startup
randomness stable regardless of later `getrandom` chunking, while still making
normal random reads sequential and reproducible.

`ClockMode` defaults to `Jea9ClockIdleJump`. In idle-jump mode, guest instruction
execution does not advance time by itself. `CLOCK_MONOTONIC` returns the current
logical monotonic time. `CLOCK_REALTIME` returns monotonic time plus
`RealtimeOffsetNS`. When at least one guest context is runnable, the scheduler
runs guest work without advancing time. When all contexts are blocked and at
least one timeout or timer deadline can unblock a context, the OS advances
logical monotonic time directly to the earliest such deadline and wakes the
eligible context or contexts.

`Jea9ClockICTick` is also implemented in the first phase. In this mode,
monotonic time advances at instruction-budget boundaries by
`retiredInstructionDelta * NSPerInstruction`. If `NSPerInstruction` is zero, it
defaults to `1`. This mode is deterministic but intentionally sensitive to guest
instruction count. It is useful for tests that want simulated CPU time to move
with work rather than with blocked deadlines.

`Jea9ClockManual` never advances automatically. `nanosleep`, futex timeouts,
epoll timeouts, and timer deadblock until the caller explicitly advances
time with `AdvanceTime` or `SetMonotonicNS`. If all contexts are blocked in
manual mode and no external time advance occurs, the run loop should return a
distinguishable blocked/deadlock error rather than silently inventing time.

`InstructionBudget` is the deterministic scheduler heartbeat. A value of zero
means the default budget of `65536` retired guest instructions. Every context is
allowed to execute at most one budget slice before `jea9linux` regains control,
saves CPU state into the current guest thread context, accounts time according
to the selected clock mode, handles pending deadlines/signals, and selects the
next runnable context. This budget boundary exists from the first implementation
step, before `clone` or futex support, because it is the foundation for
deterministic simulation testing.

The cached interpreter should gain a budgeted entry point in `run_cached.go`,
for example:

```go
type RunBudgetResult uint8

const (
	RunBudgetContinue RunBudgetResult = iota
	RunBudgetExpired
	RunBudgetExit
)

func RunDefaultBudget(cpu *CPU, nc *NoteChain, budget uint64) (RunBudgetResult, error)
```

The exact helper can be adjusted to match local naming, but it must live in
`run_cached.go` so the repository rule is preserved: no direct `runCached` call
sites outside that file. The JIT should reuse the precise instruction counter
and budget-return machinery already present for lockstep/debug work, but expose
it as a production scheduler budget for `jea9linux`.

Implementation status, 2026-06-14: this phase is complete for the initial
single-context scheduler slice. Added `jea9linux.go` with `Jea9LinuxClockMode`
and clock mode constants, `Jea9LinuxOptions`,
`Jea9Linux` state, `NewJea9Linux`, deterministic seed
derivation, clock/budget/blocking accessors,
deterministic PRNG helpers, the budgeted `Jea9Linux.Run`
loop, IC-tick accounting, `Jea9Linux.Handle`, and install/run helpers. Changed `run_cached.go` to
add `RunBudgetResult`, `RunDefaultBudget`, and the
internal `runCachedBudget` entry point so budgeted execution remains
inside the `run_cached.go` call-site boundary. Changed `jit.go` to add
`JIT.StepBlockBudget`, bridging the existing lockstep budget gate
into a scheduler-facing "N retired instructions" API. Added
`jea9linux_phase1_test.go`, with deterministic option/entropy tests,
cached-interpreter budget tests, IC-tick/manual-clock
tests, JIT budget coverage, and install/default
writer coverage.

### Determinism tests

1. `TestJea9Linux_DefaultOptions` constructs `NewJea9Linux(Jea9LinuxOptions{})`
   and verifies idle-jump clock mode, a nonzero default instruction budget, a
   stable default entropy root, and safe default stdin/stdout/stderr behavior.

2. `TestJea9Linux_EntropySeedCopied` passes a mutable seed slice, mutates it
   after construction, then verifies later random output does not change.

3. `TestJea9Linux_PRNGRepeatable` constructs two OS instances with identical
   seeds and verifies identical byte streams from the syscall random stream.

4. `TestJea9Linux_PRNGDifferentSeedsDiffer` verifies two different seeds produce
   different `AT_RANDOM` bytes and different syscall random bytes.

5. `TestJea9Linux_ATRandomSeparateFromSysRandom` verifies reading `AT_RANDOM`
   does not consume from the `getrandom` stream.

6. `TestJea9Linux_IdleJumpClockStableWhileRunnable` runs a tiny guest loop that
   reads `clock_gettime` repeatedly without blocking and verifies time remains
   unchanged.

7. `TestJea9Linux_ICTickClockAdvancesByInstructionDelta` runs a budgeted loop
   with `NSPerInstruction=7` and verifies monotonic time advances by exactly
   retired instructions times seven.

8. `TestJea9Linux_ManualClockDoesNotAdvance` parks a context on a timeout and
   verifies the run loop reports that all contexts are blocked until the test
   explicitly advances time.

9. `TestJea9Linux_BudgetBoundaryInterpreter` runs a counter loop through the
   cached interpreter with a small budget and verifies the scheduler regains
   control at the expected retired-instruction counts.

10. `TestJea9Linux_BudgetBoundaryJIT` runs the same counter loop through JIT and
    verifies final state and budget-yield count match the interpreter.

11. `TestJea9Linux_BudgetPreservesCPUState` forces several budget yields while
    all integer registers, floating registers, FCSR, PC, and LR/SC reservation
    state hold nontrivial values, then verifies exact restoration.

12. `TestJea9Linux_ReplayIdenticalTrace` runs the same fixture twice with the
    same seed, clock mode, and budget, then compares exit code, stdout, stderr,
    syscall trace, schedule trace, time reads, and random reads byte-for-byte.

## 2. Checked-In ELF Fixture Policy

Tiny C integration fixtures must be checked into `testvectors/jea9linux/`.
Do not use `testdata/`: the Go fuzzer treats it specially, and this repository
should not rely on fixture directories that tooling may delete or rewrite. Also
avoid names that conflict with the existing `testvectors/gc_riscv64` binary.

Use this layout:

```text
testvectors/jea9linux/
  README.md
  build.sh
  src/
    clock_gettime_basic.c
    getrandom_repeat.c
    futex_wait_wake.c
    ...
  elf/
    clock_gettime_basic.elf
    getrandom_repeat.elf
    futex_wait_wake.elf
    ...
```

Normal `go test` loads the checked-in `.elf` files directly and must not require
Zig, GCC, Clang, or any cross compiler. `build.sh` is the intentional
regeneration path and may require Zig. Each source file should be tiny,
single-purpose, and statically linked for `riscv64-linux-musl`. The build script
should use deterministic flags and should fail if an output ELF would not be
placed under `testvectors/jea9linux/elf/`.

Each feature gets both unit tests and ELF tests. Unit tests call syscall
handlers or the run loop directly and assert exact OS state transitions. ELF
tests exercise the real RISC-V Linux syscall ABI by running guest code. Go
runtime tests come after the C fixture tests because Go brings many subsystems
online at once.

### Fixture policy tests

1. `TestJea9Linux_TestVectorFilesExist` verifies every ELF fixture referenced
   by tests exists under `testvectors/jea9linux/elf/`.

2. `TestJea9Linux_TestVectorNoTestdataPath` verifies jea9linux tests do not
   read from `testdata/`.

3. `TestJea9Linux_TestVectorSourcesHaveMatchingELFs` verifies every source file
   under `testvectors/jea9linux/src/` has a same-stem ELF under
   `testvectors/jea9linux/elf/`.

4. `TestJea9Linux_TestVectorELFLoads` iterates all fixture ELFs and verifies
   `LoadELFBytes` succeeds.

5. `TestJea9Linux_TestVectorELFMachine` verifies all fixture ELFs are ELF64,
   little-endian, RISC-V machine type, executable, and statically loadable by
   the current loader.

## 3. OS Personality Skeleton And Syscall Dispatch

`Jea9Linux` is a stateful note handler installed above or alongside the current
minimal OS personality. The current `OS` map-based ECALL dispatcher is useful
as a starting shape, but `jea9linux` needs state: process IDs, thread contexts,
futex queues, virtual file descriptors, VM mappings, signal state, deterministic
clock, deterministic random source, and exit state.

Use Linux/RISC-V syscall numbers from the current Go runtime, not the archived
numbers in `plans/os_needs.md`. The core set is:

```text
19  eventfd2
20  epoll_create1
21  epoll_ctl
22  epoll_pwait
25  fcntl
56  openat
57  close
59  pipe2
62  lseek
63  read
64  write
67  pread64
72  pselect6
93  exit
94  exit_group
96  set_tid_address
98  futex
99  set_robust_list
101 nanosleep
103 setitimer
107 timer_create
110 timer_settime
111 timer_delete
113 clock_gettime
123 sched_getaffinity
124 sched_yield
129 kill
130 tkill
131 tgkill
132 sigaltstack
134 rt_sigaction
135 rt_sigprocmask
139 rt_sigreturn
160 uname
163 getrlimit
167 prctl
169 gettimeofday
172 getpid
178 gettid
179 sysinfo
214 brk
215 munmap
220 clone
222 mmap
226 mprotect
232 mincore
233 madvise
258 riscv_hwprobe
261 prlimit64
278 getrandom
```

The ECALL handler reads `a7` for the syscall number and `a0` through `a5` for
arguments. It writes the Linux return value to `a0`, using two's-complement
negative errno values for errors. Unknown syscalls return `-ENOSYS` and resume
at the post-ECALL PC. No syscall in `jea9linux` should pass through to the host
kernel by default.

The run loop should treat syscall boundaries, instruction-budget boundaries,
blocking syscalls, and signal delivery as scheduler decision points. The default
scheduler policy is deterministic round-robin among runnable contexts. If a
context blocks, the scheduler immediately chooses the next runnable context. If
no context is runnable, the clock policy determines whether time can advance or
the run loop must return blocked/deadlock.

### Skeleton and dispatch tests

1. `TestJea9Linux_UnknownSyscallENOSYS` executes an ECALL with an unused syscall
   number and verifies `a0 == -ENOSYS`, PC is post-ECALL, and no OS state changes
   except the syscall trace.

2. `TestJea9Linux_EcallArgumentDecode` installs a spy syscall handler and
   verifies `a0` through `a5` and `a7` are decoded exactly.

3. `TestJea9Linux_EcallReturnEncoding` returns success, zero, and several
   negative errno values from a test handler and verifies guest-visible `a0`.

4. `TestJea9Linux_SyscallTraceDeterministic` runs the same syscall sequence
   twice and compares trace records exactly.

5. `TestJea9Linux_RunLoopStopsOnExitError` verifies `exit` and `exit_group`
   return a normal `ExitError` path with the expected exit code.

6. `TestJea9Linux_SchedulerRoundRobinSingleContext` verifies a single context
   remains current across budget yields and syscalls.

7. ELF fixture `unknown_syscall.elf` performs an unknown syscall and prints the
   returned errno.

8. ELF fixture `syscall_args.elf` passes recognizable argument values to a test
   syscall reserved for tests and verifies they arrive intact.

## 4. Linux Initial Stack And Auxv

Real `riscv64/linux` programs need a Linux process stack, not only an ELF entry
PC and a manually assigned stack pointer. Add a startup helper that loads an
ELF, lays out argc, argv strings, env strings, auxv entries, and random bytes,
then sets guest `sp` and entry PC. The stack must be aligned as Linux/RISC-V
expects.

The auxv must include deterministic values for entries the Go runtime and musl
expect: page size, program headers, program header entry size/count when known,
entry point, UID/GID values if needed, secure mode, hardware capability values,
platform string when useful, and `AT_RANDOM`. Do not expose VDSO entries such as
`AT_SYSINFO_EHDR`; time and random behavior must go through the personality.

The initial environment should be caller-controlled. The default environment
should be small and deterministic. A temporary bootstrap option may inject
`GODEBUG=asyncpreemptoff=1`, but that option must be explicit and treated as a
bridge until Linux signal-frame delivery is complete.

### Initial stack tests

1. `TestJea9Linux_InitialStackArgv` starts a fixture with several argv values
   and verifies the guest observes exact argc/argv strings in order.

2. `TestJea9Linux_InitialStackEnvp` verifies configured environment strings are
   present, ordered deterministically, and null-terminated.

3. `TestJea9Linux_InitialStackAlignment` verifies the initial guest stack
   pointer meets Linux/RISC-V ABI alignment requirements.

4. `TestJea9Linux_AuxvContainsATRandom` verifies the auxv contains a valid
   `AT_RANDOM` pointer to 16 deterministic nonzero bytes.

5. `TestJea9Linux_AuxvOmitsVDSO` verifies no `AT_SYSINFO_EHDR` or equivalent
   VDSO pointer is exposed.

6. `TestJea9Linux_AuxvPageSize` verifies the page size reported to the guest is
   the personality's page size.

7. `TestJea9Linux_AuxvProgramHeaders` verifies `AT_PHDR`, `AT_PHENT`,
   `AT_PHNUM`, and `AT_ENTRY` match the loaded ELF.

8. ELF fixture `startup_argv_envp.elf` prints argc, argv, and selected env
   values.

9. ELF fixture `startup_dump_auxv.elf` walks auxv and prints selected tags in a
   stable order.

Implementation status, 2026-06-14: this phase is complete for the initial
Linux process stack and deterministic auxv contract. `jea9linux.go` now defines
the auxv tags needed for startup, exposes `ExecPath` in
`Jea9LinuxStartOptions`, adds `jea9LinuxAuxEntry` and the
`jea9LinuxStackBuilder` helper type, implements stack
byte/string/vector construction through `newJea9LinuxStackBuilder`,
`pushBytes`, `pushString`, `pushStrings`,
and `writeInitialVector`, implements `InitELFStack`,
builds the deterministic Linux auxv in `buildJea9LinuxAuxv`, and
discovers the loaded program-header address in `elfProgramHeaderVA`. The refactor pass split the previous monolithic stack routine into those
helpers, added deterministic identity/security/platform auxv defaults
(`AT_UID`, `AT_EUID`, `AT_GID`, `AT_EGID`, `AT_SECURE`, `AT_HWCAP`,
`AT_HWCAP2`, `AT_CLKTCK`, `AT_PLATFORM`, and `AT_EXECFN`), and preserved the
rule that `AT_RANDOM` uses a separate labeled stream from syscall randomness.
Added `jea9linux_phase4_test.go`, with stack/argv/env/auxv coverage,
repeatable `AT_RANDOM` coverage, syscall-random separation coverage,
Linux auxv personality defaults, and input/stack error
coverage.

## 5. Clock And Sleep Syscalls

Implement `clock_gettime(113)`, `gettimeofday(169)`, and `nanosleep(101)`.
`clock_gettime` must support at least `CLOCK_REALTIME`, `CLOCK_MONOTONIC`, and
the coarse variants that Go or musl may ask for. Unsupported clock IDs return
`-EINVAL`. `gettimeofday` returns realtime seconds and microseconds derived from
the logical clock.

`nanosleep` validates the requested `timespec`, parks the current context until
the deadline, and returns success when the sleep completes. Before signal support
exists, interrupted sleeps do not occur. After signal support, an interrupted
sleep returns `-EINTR` and writes the remaining time if the guest provided a
remaining-time pointer.

Idle-jump behavior is exact: if a single context sleeps for 10 ms and no other
context is runnable, monotonic time jumps by exactly 10 ms. If another context
is runnable, time does not advance merely because one context sleeps; the other
context runs until it blocks, exits, or consumes its budget.

Implementation status, 2026-06-14: this phase is complete for the
single-context clock/sleep model. `jea9linux.go` now defines Linux errno,
syscall, and clock constants, routes `clock_gettime(113)`,
`gettimeofday(169)`, and `nanosleep(101)` through `Jea9Linux.Handle`,
implements `sysClockGettime`, `sysGettimeofday`,
`sysNanosleep`, manual-clock blocked-state refresh,
clock selection, and Linux timespec/nanosecond splitting
helpers. Manual clock sleeps now mark the OS blocked until
explicit `AdvanceTime` or `SetMonotonicNS` reaches the deadline. Added
`jea9linux_phase2_test.go`, with syscall helper scaffolding,
`clock_gettime` tests, `gettimeofday`,
idle-jump and invalid `nanosleep` tests, manual-clock blocking
tests, and ELF fixture execution.
Added checked-in fixture infrastructure under `testvectors/jea9linux/`: the
regeneration script `build.sh`, source fixtures
`src/clock_gettime_basic.c` and `src/nanosleep_idle_jump.c`, and generated ELF
fixtures `elf/clock_gettime_basic.elf` and `elf/nanosleep_idle_jump.elf`.

### Clock and sleep tests

1. `TestJea9Linux_ClockGettimeMonotonic` verifies `CLOCK_MONOTONIC` writes a
   correct Linux `timespec` to guest memory.

2. `TestJea9Linux_ClockGettimeRealtimeOffset` verifies `CLOCK_REALTIME` equals
   monotonic plus `RealtimeOffsetNS`.

3. `TestJea9Linux_ClockGettimeInvalidClock` verifies unsupported clock IDs
   return `-EINVAL`.

4. `TestJea9Linux_Gettimeofday` verifies seconds and microseconds conversion
   from realtime nanoseconds.

5. `TestJea9Linux_NanosleepIdleJumpSingleThread` verifies a single sleeping
   context wakes exactly at its requested deadline.

6. `TestJea9Linux_NanosleepDoesNotAdvanceWhileOtherRunnable` creates two
   contexts, sleeps one, and verifies time stays fixed while the other remains
   runnable.

7. `TestJea9Linux_NanosleepManualClockBlocks` verifies manual mode blocks until
   `AdvanceTime` crosses the deadline.

8. `TestJea9Linux_NanosleepInvalidTimespec` verifies negative nanoseconds or
   nanoseconds >= 1e9 return `-EINVAL`.

9. ELF fixture `clock_gettime_basic.elf` prints monotonic and realtime values.

10. ELF fixture `gettimeofday_basic.elf` prints realtime as sec/usec.

11. ELF fixture `nanosleep_idle_jump.elf` sleeps twice and prints observed
    monotonic timestamps.

12. ELF fixture `nanosleep_invalid.elf` verifies invalid timespec errno values.

## 6. Deterministic Randomness Syscalls And Devices

Implement `getrandom(278)`, virtual `/dev/urandom`, and virtual `/dev/random`.
These all read from the deterministic syscall random stream described in the
determinism section. `getrandom` supports normal flags, `GRND_NONBLOCK`, and
`GRND_RANDOM`. Unsupported flag bits return `-EINVAL`. Since the entropy source
is deterministic and always initialized, supported calls do not block.

`openat("/dev/urandom")` and `openat("/dev/random")` return virtual readable
file descriptors. Reads from those descriptors consume the same stream as
`getrandom`. Closing and reopening the device must not rewind the stream. This
matches the process-level deterministic entropy model: the seed defines one
ordered stream of entropy observations for the process.

`AT_RANDOM` is separate from this stream and is produced from the seed with the
`"auxv-random-v1"` label. This prevents startup layout or Go runtime startup
from perturbing later explicit random reads.

Implementation status, 2026-06-14: this phase is complete for `getrandom(278)`
and the minimal virtual random-device path. `jea9linux.go` now has fd kinds and
fd state-170, initializes the fd table in
`NewJea9Linux`, routes `openat(56)`, `close(57)`, `read(63)`,
and `getrandom(278)` through `Jea9Linux.Handle`, implements
`sysGetrandom`, implements virtual random-device `openat`, `read`, `close`, and guest C-string path loading
at . Added `jea9linux_phase3_test.go`, with
repeatability coverage, chunking, zero-length and
invalid-flag handling, `/dev/urandom` open/read/close/reopen
coverage, and ELF fixture execution. Added
`testvectors/jea9linux/src/getrandom_repeat.c` and generated
`testvectors/jea9linux/elf/getrandom_repeat.elf`.

### Randomness tests

1. `TestJea9Linux_GetRandomExactBytes` verifies the exact first 64 bytes for a
   fixed seed.

2. `TestJea9Linux_GetRandomChunking` verifies four 16-byte reads equal one
   64-byte read from a fresh OS with the same seed.

3. `TestJea9Linux_GetRandomZeroLength` verifies zero-length requests succeed
   and do not advance the stream.

4. `TestJea9Linux_GetRandomInvalidFlags` verifies unknown flags return
   `-EINVAL`.

5. `TestJea9Linux_GetRandomNonblock` verifies `GRND_NONBLOCK` succeeds and
   returns deterministic bytes.

6. `TestJea9Linux_DevURandomReadConsumesSysStream` verifies `/dev/urandom`
   reads consume the same stream as `getrandom`.

7. `TestJea9Linux_DevRandomReadSupported` verifies `/dev/random` behaves
   deterministically and does not block.

8. `TestJea9Linux_RandomReopenDoesNotRewind` reads, closes, reopens, and reads
   again, verifying the stream continues.

9. `TestJea9Linux_ATRandomDoesNotConsumeGetRandom` verifies auxv random reads
   do not change the first `getrandom` bytes.

10. ELF fixture `getrandom_repeat.elf` prints a fixed-size hex sample.

11. ELF fixture `getrandom_flags.elf` checks supported and unsupported flags.

12. ELF fixture `dev_urandom_read.elf` opens `/dev/urandom`, reads bytes, and
    prints hex output.

13. ELF fixture `dev_random_reopen.elf` verifies close/reopen stream behavior.

## 7. Basic Process And File Descriptor Syscalls

Implement a deterministic fd table before adding Go runtime acceptance tests.
FDs 0, 1, and 2 map to configured `Stdin`, `Stdout`, and `Stderr`. If `Stdin`
is nil, reads from fd 0 return EOF. If stdout/stderr are nil, they discard.
Writes should accept partial writes only if the configured writer reports them;
otherwise they return the full byte count.

Implement `read(63)`, `write(64)`, `close(57)`, `openat(56)`, `fcntl(25)`,
`lseek(62)`, and `pread64(67)` enough for virtual files and checked-in fixtures.
`openat` initially supports virtual paths required by the personality:
`/dev/urandom`, `/dev/random`, and any explicitly configured read-only fixture
files. Unsupported paths return `-ENOENT` or `-EACCES` consistently. Do not open
host files by default.

Implement process identity and simple resource syscalls: `exit(93)`,
`exit_group(94)`, `getpid(172)`, `gettid(178)`, `uname(160)`, `getrlimit(163)`,
`prlimit64(261)`, `sysinfo(179)`, and `prctl(167)`. The responses should be
small, Linux-compatible, and deterministic. `sched_getaffinity(123)` belongs to
threading, but its default behavior is already decided: expose exactly one CPU.

### Process and fd tests

1. `TestJea9Linux_WriteStdout` writes to fd 1 and verifies configured stdout
   receives exact bytes.

2. `TestJea9Linux_WriteStderr` writes to fd 2 and verifies configured stderr
   receives exact bytes.

3. `TestJea9Linux_WriteBadFD` verifies writing to an unopened fd returns
   `-EBADF`.

4. `TestJea9Linux_ReadStdinEOF` verifies nil stdin returns EOF.

5. `TestJea9Linux_ReadConfiguredStdin` verifies configured stdin bytes are read
   in order.

6. `TestJea9Linux_CloseThenReadBadFD` verifies closed fd behavior.

7. `TestJea9Linux_OpenAtUnsupportedPath` verifies stable errno for unsupported
   host paths.

8. `TestJea9Linux_FcntlGetFlags` verifies the minimal `F_GETFL` behavior needed
   by fixtures and Go runtime code.

9. `TestJea9Linux_LseekVirtualFile` verifies seek behavior for seekable virtual
   files and `-ESPIPE` for non-seekable streams.

10. `TestJea9Linux_GetpidGettidInitialThread` verifies stable pid/tid values
    for the initial context.

11. `TestJea9Linux_ExitCode` verifies `exit` sets process exit code.

12. `TestJea9Linux_ExitGroupTerminatesAllContexts` verifies process-wide exit
    stops all contexts once threading exists.

13. `TestJea9Linux_UnameDeterministic` verifies fixed sysname, release,
    version, machine, and nodename strings.

14. ELF fixture `write_stdout.elf` writes a fixed message.

15. ELF fixture `read_stdin.elf` echoes configured stdin.

16. ELF fixture `openat_urandom.elf` opens and reads `/dev/urandom`.

17. ELF fixture `pid_tid.elf` prints pid and tid.

18. ELF fixture `exit_codes.elf` exits with several configured codes.

Implementation status, 2026-06-14: this phase is complete for deterministic
stdio, read-only virtual files, and the first process/resource syscall surface.
`jea9linux.go` now defines fd/process syscall constants and errno values,
fcntl/seek/resource/prctl constants, fd kinds and
fd records, `Files`, `PID`, and `TID` options in
`Jea9LinuxOptions`, and fd/process state in `Jea9Linux`. `NewJea9Linux` now initializes fds 0, 1, and 2, default pid/tid
identity, default discard writers, and cloned read-only virtual file contents.
`Jea9Linux.Handle` routes the Phase 5 syscalls, with fd
cases and process/resource cases. The fd
implementation lives in `sysOpenat`, `sysRead`,
`sysWrite`, `sysClose`, `sysFcntl`,
`sysLseek`, `sysPread64`, and the refactored shared
read-only-file range helper `readJea9LinuxFileRange`. The
process/resource implementation lives in `sysUname`,
`sysGetrlimit`, `sysPrlimit64`, `sysSysinfo`, `sysPrctl`, and `readLinuxThreadName`. Added
`jea9linux_phase5_test.go`, with syscall assertion helpers,
stdout/stderr/partial-write coverage, bad-fd write coverage,
stdin EOF/configured-input coverage, close/read behavior,
unsupported path coverage, configured read-only
file/read/seek/pread/fcntl coverage, virtual-file option-copy
coverage, fd error-edge coverage, nonseekable lseek
coverage, pid/tid coverage, uname coverage,
resource/sysinfo coverage, prctl thread-name coverage,
and ELF fixture execution. Added checked-in fixture sources
`testvectors/jea9linux/src/write_stdout.c`,
`testvectors/jea9linux/src/read_stdin_echo.c`, and
`testvectors/jea9linux/src/pid_tid.c`, plus generated ELF fixtures
`testvectors/jea9linux/elf/write_stdout.elf`,
`testvectors/jea9linux/elf/read_stdin_echo.elf`, and
`testvectors/jea9linux/elf/pid_tid.elf`.

## 8. Guest VM Map, Brk, Mmap, And Protection Overlay

The current guest memory is a flat mmap-backed power-of-two slab with a mask
sandbox invariant. `jea9linux` must preserve that invariant while adding a
Linux VM metadata overlay. The overlay tracks mapped pages and permissions for
Linux personality runs. Bare-metal tests and riscv-tests should not see this
overlay unless `jea9linux` is installed.

Implement `brk(214)`, `mmap(222)`, `munmap(215)`, `mprotect(226)`,
`madvise(233)`, and `mincore(232)`. Anonymous mappings are zero-filled.
`mmap` chooses deterministic page-aligned addresses when the guest does not use
`MAP_FIXED`. `MAP_FIXED` validates alignment and range. `munmap` removes whole
pages. `mprotect` updates permissions. `madvise` accepts common advice values
as deterministic no-ops unless a fixture proves a stronger behavior is needed.
`mincore` reports residency for mapped pages deterministically.

Page zero must be unmapped or protected under `jea9linux` so Go nil-pointer
checks and C null accesses produce Linux-style faults. This is a personality
feature. Do not globally guard page zero because existing ELF tests may depend
on low addresses.

Executable mappings must update executable-region metadata and coordinate with
JIT invalidation. For the first implementation, it is acceptable to disable
JIT execution from dynamically executable mappings until explicit tests require
it. The important rule is that writable-to-executable transitions cannot keep
running stale translated code.

### VM and memory tests

1. `TestJea9Linux_BrkInitialValue` verifies the initial brk is page-aligned and
   above loaded ELF data.

2. `TestJea9Linux_BrkGrowZeroFilled` grows brk, writes/reads memory, and
   verifies newly exposed bytes are zero.

3. `TestJea9Linux_BrkShrink` shrinks brk and verifies accesses beyond the new
   brk fault under the VM overlay.

4. `TestJea9Linux_MmapAnonymous` maps anonymous memory and verifies page
   alignment, zero fill, and read/write permission.

5. `TestJea9Linux_MmapFixed` maps at a fixed address and verifies exact address
   or correct errno.

6. `TestJea9Linux_MmapNoOverlap` verifies two non-fixed mappings do not overlap.

7. `TestJea9Linux_MunmapFaultsAfterUnmap` verifies load/store after unmap
   produces a memory fault path.

8. `TestJea9Linux_MprotectReadOnlyRejectsStore` verifies store to read-only
   mapping faults.

9. `TestJea9Linux_MprotectExecMetadata` verifies adding execute permission
   updates executable-region metadata or records a deliberate unsupported path.

10. `TestJea9Linux_PageZeroFaults` verifies null load and null store do not
    silently access guest memory.

11. `TestJea9Linux_MincoreMappedUnmapped` verifies mapped pages are reported
    resident and unmapped pages return the expected errno.

12. `TestJea9Linux_MadviseCompatibilityNoop` verifies common advice values
    return success without changing deterministic contents.

13. ELF fixture `brk_basic.elf` exercises brk growth and writes.

14. ELF fixture `mmap_rw.elf` maps, writes, reads, and unmaps anonymous memory.

15. ELF fixture `mprotect_ro.elf` verifies read-only protection.

16. ELF fixture `null_fault.elf` intentionally touches null and expects signal
    behavior once signals are implemented; before signal support, the unit test
    should assert the raw fault path.

Implementation status, 2026-06-14: this phase is complete for the first
jea9linux VM overlay and anonymous-memory syscall surface. `guestmem.go` now
has an optional personality access overlay field, the
`guestMemoryAccessOverlay` interface, install/cleanup helpers
`setAccessOverlay` and `clearAccessOverlay`, and
`checkAccessOverlay`. Normal memory operations now consult the
overlay only when installed: scalar loads, scalar stores, instruction fetch,
bulk `ReadBytes`, and bulk `WriteBytes`.
`note.go` maps `FaultFetch` to an instruction-fault note in
`faultCauseAndText`.

`jea9linux.go` defines the VM syscall numbers and prot/map
constants, adds `jea9LinuxVM` state, creates and
attaches the overlay through `newJea9LinuxVM`,
`jea9LinuxDefaultMmapBase`, and `ensureVM`, and
implements the access policy in `CheckGuestAccess`. Page/range
helpers live. `InitELFStack` adjusts the initial program break
from the loaded ELF using `elfProgramBreak`.
`Jea9Linux.Handle` routes the VM syscalls. The syscall bodies
are `sysBrk`, `sysMmap`, `sysMunmap`,
`sysMprotect`, `sysMincore`, and `sysMadvise`.
`InstallJea9Linux` attaches the overlay and removes it
on cleanup, keeping bare-metal memory behavior unchanged.

Added `jea9linux_phase8_test.go`, with brk grow/shrink/zero-fill/fault coverage
coverage, anonymous/fixed/non-overlap mmap coverage, munmap fault
coverage, mprotect read-only and exec metadata coverage, page-zero load/store/fetch fault coverage, mincore/madvise
coverage, overlay install cleanup coverage,
syscall-buffer permission coverage, invalid-range coverage, and ELF fixture execution. Added checked-in fixture sources
`testvectors/jea9linux/src/brk_basic.c`,
`testvectors/jea9linux/src/mmap_rw.c`, and
`testvectors/jea9linux/src/mprotect_ro.c`, plus generated ELF fixtures
`testvectors/jea9linux/elf/brk_basic.elf`,
`testvectors/jea9linux/elf/mmap_rw.elf`, and
`testvectors/jea9linux/elf/mprotect_ro.elf`.

## 9. Clone, Guest Thread Contexts, Futex, And Scheduling

`clone(220)` creates a guest Linux thread context, not a host goroutine. The Go
runtime's RISC-V clone assembly expects the parent to return the child tid in
`a0`, and the child to resume at the post-ECALL PC with `a0 == 0` and `sp` set
to the requested child stack. The child stack already contains Go runtime values
written by the parent before the syscall. `jea9linux` should not invent a new
entry function ABI; it should resume exactly as Linux would after clone.

Supported clone flags for the Go runtime include the usual thread-sharing set:
`CLONE_VM`, `CLONE_FS`, `CLONE_FILES`, `CLONE_SIGHAND`, `CLONE_SYSVSEM`, and
`CLONE_THREAD`. Unsupported flag combinations should return `-EINVAL` until a
test requires them. All guest threads share guest memory, VM mappings, fd table,
process ID, and signal-handler table. They have separate register files,
signal masks, alternate signal stacks, tid values, clear-child-tid addresses,
and robust-list pointers.

Implement `futex(98)` for `FUTEX_WAIT`, `FUTEX_WAKE`, and their private
variants. `WAIT` reads the guest futex word under the single scheduler. If the
word differs from the expected value, return `-EAGAIN`. If it matches, park the
current context on the futex address, optionally with a deterministic timeout.
`WAKE` marks up to `n` waiters runnable in FIFO order. The scheduler decides
when woken contexts run.

Implement `sched_yield(124)` as a deterministic voluntary scheduler boundary.
Implement `sched_getaffinity(123)` to report exactly one CPU. Implement
`set_tid_address(96)` and `set_robust_list(99)` with enough state for the Go
runtime and tests. On thread exit, clear the clear-child-tid address and wake
waiters if Linux semantics require it for the tested path.

### Threading and futex tests

1. `TestJea9Linux_CloneParentReturn` verifies parent receives a positive child
   tid and remains at the post-ECALL PC.

2. `TestJea9Linux_CloneChildReturn` verifies the child context has `a0 == 0`,
   the requested stack pointer, copied/shared process state, and post-ECALL PC.

3. `TestJea9Linux_CloneUnsupportedFlags` verifies unsupported flag combinations
   return `-EINVAL` without creating a context.

4. `TestJea9Linux_SingleHartInvariant` instruments the scheduler and verifies
   exactly one guest context is loaded into `CPU` at any time.

5. `TestJea9Linux_SchedYieldRoundRobin` creates three runnable contexts and
   verifies yield rotates in deterministic order.

6. `TestJea9Linux_SchedYieldSingleContextNoop` verifies yielding with no other
   runnable contexts returns success and continues.

7. `TestJea9Linux_SchedGetAffinityOneCPU` verifies the guest affinity mask has
   exactly one bit set.

8. `TestJea9Linux_FutexWaitBlocksWhenValueMatches` verifies matching value
   parks the current context.

9. `TestJea9Linux_FutexWaitEAGAINWhenValueDiffers` verifies nonmatching value
   returns immediately.

10. `TestJea9Linux_FutexWakeOne` verifies waking one waiter marks exactly one
    context runnable.

11. `TestJea9Linux_FutexWakeFIFO` verifies multiple waiters wake in stable FIFO
    order.

12. `TestJea9Linux_FutexWakeNoWaiters` verifies waking an empty queue returns
    zero.

13. `TestJea9Linux_FutexTimeoutIdleJump` verifies a futex timeout wakes at the
    exact logical deadline in idle-jump mode.

14. `TestJea9Linux_FutexTimeoutManualClock` verifies manual mode requires an
    explicit time advance.

15. `TestJea9Linux_SetTidAddressClearOnExit` verifies thread exit clears the
    configured child tid address.

16. ELF fixture `clone_child_stack.elf` creates a child and prints parent/child
    observed values.

17. ELF fixture `yield_pingpong.elf` uses `sched_yield` to interleave two
    contexts and prints the deterministic order.

18. ELF fixture `futex_wait_wake.elf` parks one context and wakes it from
    another.

19. ELF fixture `futex_timeout.elf` waits with a timeout and prints timestamps.

20. ELF fixture `sched_affinity.elf` prints the count of CPUs in the affinity
    mask and must print one.

Implementation status, 2026-06-14: this phase is complete for the first
single-hart deterministic guest-thread scheduler, clone, futex, and affinity
surface. The red tests were added first in `jea9linux_phase9_test.go`, then the
implementation was made green, then the refactor pass removed an unused helper
and added extra edge coverage for budget-boundary rotation, empty futex wakes,
faulting/unaligned futex waits, affinity faults, and `Blocked()` behavior
around manual futex deadlines. The broader jea9linux gate was rerun after the
refactor.

`jea9linux.go` now defines the new errno/syscall constants and
the futex/clone flag constants. Scheduler state was added to
`Jea9Linux`. Guest context state, CPU snapshots, and per-thread
fields are defined. The single-hart scheduler helpers are
`ensureScheduler`, `snapshotJea9LinuxCPU`,
`restoreJea9LinuxCPU`, `loadContext`,
`nextRunnableAfterCurrent`, `hasRunnableContext`,
`markRunnable`, and `removeFutexWaiter`. `Run` now uses
the instruction budget as a deterministic scheduling boundary.
`Handle` routes the new syscalls and `gettid` through the
current guest context. `sysClone` starts,
`jea9LinuxCloneFlagsSupported`, `sysSchedYield`,
`sysSchedGetAffinity`, `sysSetTidAddress`,
`sysSetRobustList`, `sysExit`,
`exitCurrentThread`, `sysFutex`, `futexWait`, `futexDeadline`, and `wakeFutex`.
`refreshBlocked` now refreshes futex timeouts using
`refreshFutexTimeouts`.

`jea9linux_phase9_test.go` covers clone parent/child return and copied register
state, unsupported clone flags, deterministic
round-robin yield and the single-hart loaded-context invariant,
single-context yield, budget-boundary rotation,
one-CPU affinity, affinity faults, `set_tid_address`
and `set_robust_list` state, futex `-EAGAIN`, empty
wake, futex fault/alignment errors, wait/wake resume, FIFO wake order,
idle-jump timeout,
manual-clock timeout, clear-child-tid on thread exit,
and checked-in ELF fixtures. Added fixture sources
`testvectors/jea9linux/src/sched_affinity.c`,
`testvectors/jea9linux/src/clone_child_stack.c`,
`testvectors/jea9linux/src/yield_pingpong.c`,
`testvectors/jea9linux/src/futex_wait_wake.c`, and
`testvectors/jea9linux/src/futex_timeout.c`, with generated binaries under
`testvectors/jea9linux/elf/` for the same basenames.

## 10. Eventfd, Epoll, Pipe, And Polling Primitives

Go's Linux netpoller uses `eventfd2(19)`, `epoll_create1(20)`, `epoll_ctl(21)`,
and `epoll_pwait(22)` on current RISC-V Linux. Implement these as virtual kernel
objects in the fd table. They must be deterministic and must not use host epoll.

An eventfd stores a uint64 counter and flags such as nonblocking. Writes add to
the counter with Linux-compatible overflow behavior. Reads return the counter
and reset it, or decrement by one if semaphore mode is implemented. Empty
nonblocking reads return `-EAGAIN`; blocking reads park the current context.

An epoll instance tracks registered fd interests. `epoll_ctl` supports add,
modify, and delete. `epoll_pwait` returns ready events in deterministic order,
preferably registration order then fd number as a stable tie-breaker. If no
events are ready, `epoll_pwait` blocks until an event arrives or the timeout
expires according to the selected clock mode.

Implement `pipe2(59)` and `pselect6(72)` enough for C fixtures and any Go
fallback path encountered. Pipes are virtual bounded byte queues. Reads block
when empty unless nonblocking; writes block or return `-EAGAIN` when full
depending on flags.

### Event and polling tests

1. `TestJea9Linux_EventfdInitialValue` verifies `eventfd2` initializes the
   counter correctly.

2. `TestJea9Linux_EventfdReadWrite` verifies write increments and read consumes
   the counter.

3. `TestJea9Linux_EventfdNonblockEmptyRead` verifies `-EAGAIN`.

4. `TestJea9Linux_EventfdPollReadiness` verifies eventfd readiness changes
   after write and read.

5. `TestJea9Linux_EpollCreateClose` verifies epoll fds allocate and close.

6. `TestJea9Linux_EpollCtlAddModDel` verifies registration lifecycle.

7. `TestJea9Linux_EpollPwaitReadyImmediate` verifies ready events return
   without clock advancement.

8. `TestJea9Linux_EpollPwaitBlocksUntilEvent` verifies a blocked context wakes
   when another context writes an eventfd.

9. `TestJea9Linux_EpollPwaitTimeoutIdleJump` verifies idle-jump advances to the
   epoll timeout exactly.

10. `TestJea9Linux_EpollPwaitReadyOrder` verifies deterministic event order
    when multiple fds are ready.

11. `TestJea9Linux_Pipe2ReadWrite` verifies byte ordering through a pipe.

12. `TestJea9Linux_Pipe2NonblockEmptyRead` verifies nonblocking empty read
    returns `-EAGAIN`.

13. `TestJea9Linux_Pselect6Timeout` verifies timeout behavior through the
    deterministic clock.

14. ELF fixture `eventfd_basic.elf` writes and reads an eventfd.

15. ELF fixture `epoll_eventfd.elf` registers an eventfd and observes readiness.

16. ELF fixture `epoll_timeout.elf` verifies timeout timestamps.

17. ELF fixture `pipe2_basic.elf` writes and reads a pipe.

18. ELF fixture `pselect_timeout.elf` verifies timeout behavior.

Implementation status, 2026-06-14: this phase is complete for the first
virtual eventfd, epoll, pipe, and pselect surface. Red syscall-level tests were
added first in `jea9linux_phase10_test.go`; implementation then made them
green; the refactor pass shared timespec deadline parsing and added additional
coverage for eventfd overflow/short reads, epoll duplicate/error paths, and
pipe readiness through epoll. Checked-in tiny C fixtures were added and rebuilt
under `testvectors/jea9linux/elf/`. The Phase 10 focused suite and the broader
jea9linux gate both pass.

`jea9linux.go` defines the Phase 10 syscall numbers and eventfd,
epoll, and fd flag constants. The fd kinds now include
eventfd, epoll, and pipe endpoints. `jea9LinuxFD` carries
eventfd counters, epoll state, and pipe pointers.
`jea9LinuxEpollRegistration`, `jea9LinuxEpoll`, and `jea9LinuxPipe` carry the
eventfd, epoll, and pipe state. Epoll wait state is represented through
`jea9LinuxWaitEpoll` and the per-context wait fields.

`Jea9Linux.Handle` routes `eventfd2`, `epoll_create1`, `epoll_ctl`,
`epoll_pwait`, `pipe2`, and `pselect6`. FD allocation is
centralized in `allocFD`. The new syscall bodies and helpers are
`sysEventfd2`, `sysEpollCreate1`, `sysEpollCtl`, packed `epoll_event`
loaders/storers,
`sysEpollPwait`, `epollDeadline`,
`epollCollectReady`, `fdReadyEvents`,
`wakeEpollWaitersForFD`, `sysPipe2`,
`sysPselect6`, and shared `timespecDeadline`.
Existing `sysRead` and `sysWrite` dispatch to eventfd and pipe handlers. The fd
bodies are `sysEventfdRead`, `sysEventfdWrite`, `sysPipeRead`, and
`sysPipeWrite`. `refreshBlocked` invokes epoll timeout refresh through
`refreshEpollTimeouts`.

`jea9linux_phase10_test.go` covers eventfd initial/read/write/nonblocking and
overflow paths; epoll create/close, add/modify/delete, error handling,
immediate readiness, blocking wake, timeout, and deterministic order; pipe
readiness through epoll and pipe read/write/nonblocking empty reads; pselect
timeout; and checked-in ELF fixtures. Added fixture sources
`testvectors/jea9linux/src/eventfd_basic.c`,
`testvectors/jea9linux/src/epoll_eventfd.c`,
`testvectors/jea9linux/src/epoll_timeout.c`,
`testvectors/jea9linux/src/pipe2_basic.c`, and
`testvectors/jea9linux/src/pselect_timeout.c`, with generated binaries under
`testvectors/jea9linux/elf/` for the same basenames.

## 11. Signals Over Plan 9 Notes

The emulator's internal exception mechanism remains the Plan 9-style note
chain. `jea9linux` must translate guest-visible Linux signals onto that
mechanism rather than replacing notes with host signals. The note chain is the
delivery trigger; Linux signal frames are the guest ABI.

Implement `rt_sigaction(134)`, `rt_sigprocmask(135)`, `sigaltstack(132)`,
`tgkill(131)`, `tkill(130)`, `kill(129)`, and `rt_sigreturn(139)`. Each guest
thread has a signal mask and alternate signal stack. The process has a shared
signal-handler table. Pending process-directed and thread-directed signals are
tracked in deterministic queues.

For delivery, build a RISC-V Linux signal frame on the guest stack or altstack,
populate `siginfo` and `ucontext`, set registers according to the Linux ABI
(`a0=sig`, `a1=&siginfo`, `a2=&ucontext`), and set PC to the registered handler
or trampoline. `rt_sigreturn` restores the saved context. This is required for
Go's signal handling to perform async preemption and convert memory faults into
panics.

`SIGURG` is important for Go async preemption. A temporary explicit bootstrap
option may ignore `SIGURG` while `GODEBUG=asyncpreemptoff=1` is injected, but
the final implementation should deliver `SIGURG` to Go's registered handler.
Memory faults under the VM overlay should become `SIGSEGV` or `SIGBUS` when a
handler is installed; otherwise they remain fatal notes.

### Signal tests

1. `TestJea9Linux_RtSigactionInstall` verifies installing a handler stores the
   action exactly.

2. `TestJea9Linux_RtSigactionReadBack` verifies old action readback.

3. `TestJea9Linux_RtSigprocmaskBlockUnblock` verifies mask changes are
   per-thread.

4. `TestJea9Linux_SignalPendingWhileMasked` verifies a blocked signal remains
   pending until unmasked.

5. `TestJea9Linux_SigaltstackInstall` verifies alternate stack state is stored.

6. `TestJea9Linux_SignalUsesAltstack` verifies delivery frame is placed on the
   altstack when enabled.

7. `TestJea9Linux_TgkillTargetsTid` verifies thread-directed delivery to a
   specific guest tid.

8. `TestJea9Linux_KillTargetsProcess` verifies process-directed delivery uses a
   deterministic eligible thread.

9. `TestJea9Linux_RtSigreturnRestoresRegisters` verifies all saved registers,
   PC, SP, FCSR, and signal mask are restored.

10. `TestJea9Linux_SiginfoForUserSignal` verifies signal number, code, pid, and
    uid fields for `tgkill`.

11. `TestJea9Linux_SiginfoForSegv` verifies fault address and code for a page
    fault.

12. `TestJea9Linux_SIGURGDelivery` verifies `tgkill(SIGURG)` invokes the
    installed handler rather than silently succeeding.

13. `TestJea9Linux_NullFaultDeliversSIGSEGV` verifies page-zero access becomes
    guest-visible `SIGSEGV`.

14. ELF fixture `sigaction_basic.elf` installs a handler and sends itself a
    signal.

15. ELF fixture `sigmask_pending.elf` blocks, sends, unblocks, and observes
    delivery order.

16. ELF fixture `sigaltstack_frame.elf` verifies handler stack address.

17. ELF fixture `tgkill_self.elf` targets the current tid.

18. ELF fixture `sigsegv_null.elf` handles a null access and exits cleanly.

Phase 11 completion note: completed the Plan 9 note to Linux signal bridge with
red tests first, a green implementation, and a refactor/coverage pass before
moving on. `jea9linux.go` now defines signal syscall constants, signal
action/info/frame types, shared signal/frame maps, per-context masks, pending
queues, altstack state, and signal-state initialization. The ECALL router
dispatches signal syscalls, while the production signal implementation lives in
`handleFaultSignal`, `sysRtSigaction`, `sysRtSigprocmask`, `sysSigaltstack`,
`sysKill`, `sysTkill`, `sysTgkill`, `signalTIDSyscall`, `sysRtSigreturn`,
compiled-restorer frame lookup in `findSignalFrame`, delivery/pending helpers,
guest frame construction in `writeSignalFrame`, `storeJea9LinuxSignalInfo`,
and wait cancellation. `sysClone` now copies the parent signal mask. The
refactor pass also found and fixed an older VM-overlay hole in
the cached interpreter: aligned LD/SD fast paths in `run_cached.go` now require
`accessOverlay == nil`, and the RVC slot helper mirrors that guard in
`exec_slot.go`. Added `jea9linux_phase11_test.go`, with signal action helpers,
unit coverage for actions/masks/pending signals/altstack/tgkill/siginfo and
`rt_sigreturn`, and checked-in ELF fixture execution. Added cached overlay
regression tests in `jea9linux_phase8_test.go`. Added fixture sources
`testvectors/jea9linux/src/jea9linux_signal_common.h` with syscall/restorer
helpers, plus
`sigaction_basic.c`, `sigmask_pending.c`, `sigaltstack_frame.c`,
`tgkill_self.c`, and `sigsegv_null.c`; their generated ELFs live under
`testvectors/jea9linux/elf/`. Verified with the Phase 11 focused suite, the
cached VM-overlay regression suite, and the broader jea9linux gate.

## 12. RISC-V Linux Capability And Misc Runtime Syscalls

Implement `riscv_hwprobe(258)` deterministically. The simplest acceptable
policy is to return `-ENOSYS`, because the Go runtime tolerates absence of the
syscall and leaves optional feature bits disabled. If a stronger fixed response
is desired later, it must be added with tests and must not depend on host CPU
features.

Timer syscalls are included as runtime-discovered compatibility items:
`setitimer(103)`, `timer_create(107)`, `timer_settime(110)`, and
`timer_delete(111)`. Implement them only as needed by red tests. They should
use the same deterministic deadline queue as `nanosleep`, futex timeouts, and
epoll timeouts.

`set_robust_list(99)`, `prctl(167)`, `getrlimit(163)`, `prlimit64(261)`, and
`sysinfo(179)` should return narrow deterministic Linux-compatible responses.
Do not overbuild them. Add fields only when a fixture or Go acceptance binary
proves the need.

### Capability and misc tests

1. `TestJea9Linux_RiscvHwprobeENOSYS` verifies the chosen initial response.

2. `TestJea9Linux_RiscvHwprobeNoHostDependency` verifies the response does not
   vary across host platforms.

3. `TestJea9Linux_GetrlimitStack` verifies deterministic stack/resource limits.

4. `TestJea9Linux_Prlimit64Read` verifies `prlimit64` readback for supported
   resources.

5. `TestJea9Linux_PrctlSupportedNoops` verifies supported `prctl` operations
   used by fixtures return stable values.

6. `TestJea9Linux_SysinfoDeterministic` verifies uptime, memory, and process
   counts are deterministic.

7. `TestJea9Linux_TimerCreateSetDelete` verifies timer lifecycle if timer
   syscalls are implemented.

8. `TestJea9Linux_TimerExpirationIdleJump` verifies virtual timers wake or
   signal at exact logical deadlines.

9. ELF fixture `riscv_hwprobe.elf` calls the syscall and prints errno/result.

10. ELF fixture `resource_limits.elf` prints selected rlimit/prlimit values.

11. ELF fixture `sysinfo_basic.elf` prints deterministic sysinfo fields.

12. Timer ELF fixtures are added only when timer syscalls are implemented.

Phase 12 completion note: completed the capability and miscellaneous runtime
syscall pass with red tests first, a small green implementation, and a refactor
survey before moving on. `jea9linux.go` now explicitly owns timer compatibility
syscall numbers and `riscv_hwprobe(258)`. The ECALL
router dispatches timer compatibility syscalls and
`riscv_hwprobe`. The narrow deterministic implementations
are `sysTimerCompatibility` and `sysRiscvHwprobe`;
both return `-ENOSYS` deliberately so the guest observes stable Linux-compatible
absence rather than host-dependent timer or CPU capability state. Existing
resource-limit, `prctl`, and `sysinfo` helpers remain the implementation for
that part of the phase, with additional error-edge coverage added in
`jea9linux_phase12_test.go`.

Added `jea9linux_phase12_test.go`, with `riscv_hwprobe` deterministic ENOSYS
coverage, timer compatibility ENOSYS coverage,
resource-limit error edges, `prctl`/`sysinfo` deterministic edge
coverage, and checked-in ELF fixture execution. Added
fixture sources `testvectors/jea9linux/src/riscv_hwprobe.c` with the raw
`riscv_hwprobe` call, `resource_limits.c` with getrlimit/prlimit
checks, and `sysinfo_basic.c` with sysinfo validation
starting. Generated fixture binaries
`testvectors/jea9linux/elf/riscv_hwprobe.elf`,
`testvectors/jea9linux/elf/resource_limits.elf`, and
`testvectors/jea9linux/elf/sysinfo_basic.elf`. Verified with the Phase 12
focused suite and the broader jea9linux gate.

## 13. JIT Integration And Direct Syscall Policy

The desired `jea9linux` JIT behavior is still a direct ECALL path. The direct
path must trap directly into the active `jea9linux` OS personality rather than
falling back to the interpreter, rewinding, restarting at the top of a compiled
function, or issuing host syscalls. The incompatible behavior is the current
direct-host-syscall shortcut for a small set of guest syscall numbers, not
direct ECALL dispatch itself.

Implement a personality-aware direct ECALL target for JIT-emitted ECALLs. A
compiled block should be able to leave native guest code, call the installed
`jea9linux` syscall/note handler with the current CPU state, receive the handler
disposition/result, and continue or exit according to the same rules as the
cached interpreter. This callout must preserve the existing compiled-frame
assumptions: no "restart" or "rewind" path that requires re-entering at a
compiled function's top, and no new requirement that arbitrary guest state be
reconstructed outside the normal JIT spill/return convention.

The direct ECALL policy should be per-run or per-JIT where possible. Avoid
process-global toggles except as a temporary bridge. If a global toggle is used
temporarily, tests must create a fresh JIT after changing it because
already-compiled blocks retain the syscall path they were emitted with. The
end-state should make the active OS personality, not the host process, the
authority for fd state, tid state, brk, time, randomness, futex scheduling,
signals, and all other guest-visible kernel behavior.

JIT instruction-budget support must be production-ready for `jea9linux`. Budget
returns should preserve CPU state, spill all guest-visible changes, update
`riscvInstrBegun`, and return control to the deterministic scheduler. JIT and
interpreter should produce the same syscall traces, schedule traces, clock
observations, random observations, and final guest state for the same seed and
budget.

### JIT integration tests

1. `TestJea9Linux_JITDoesNotUseHostWrite` verifies guest `write(1, ...)` goes
   directly to the configured `jea9linux` `Stdout`, not host fd 1.

2. `TestJea9Linux_JITGettidUsesVirtualTid` verifies `gettid` returns the
   personality's tid, not a host thread id.

3. `TestJea9Linux_JITClockUsesDeterministicClock` verifies JIT
   `clock_gettime` matches interpreter output for the same clock configuration.

4. `TestJea9Linux_JITRandomUsesDeterministicRNG` verifies JIT `getrandom`
   matches interpreter output.

5. `TestJea9Linux_JITBudgetReturnPreservesState` verifies budget return from
   compiled code saves all guest-visible state.

6. `TestJea9Linux_JITFutexWaitWake` verifies a futex fixture behaves the same
   under JIT and interpreter.

7. `TestJea9Linux_JITReplayMatchesInterpreterTrace` runs a deterministic ELF
   under both engines and compares syscall/schedule trace after normalizing
   expected engine labels.

8. `TestJea9Linux_JITDirectEcallDoesNotRewind` verifies a JIT ECALL callout
   resumes at the post-ECALL PC and does not re-enter the top of the compiled
   function or fall back through the interpreter to complete the syscall.

9. `TestJea9Linux_JITFreshAfterSyscallModeChange` verifies changing direct
   syscall policy cannot affect already-compiled blocks silently if any
   temporary process-global bridge remains.

Phase 13 completion note: completed the initial JIT integration pass with red
tests first, a green implementation, and a refactor/coverage pass. The key
behavior is that jea9linux suppresses the native host-syscall shortcut for
future JIT block emissions while it is installed, so JIT ECALLs trap through
the existing `jitEcall`/NoteChain path at the post-ECALL PC and are handled by
the jea9linux personality. This keeps ECALL handling direct in the JIT sense
(no interpreter execution of the ECALL, no rewind, no re-entry at a compiled
function top) while preventing guest syscalls from reaching host fd, tid,
clock, random, or resource state through `internal/syscalls`.

`jea9linux.go` now saves the prior host-direct-syscall flag in
`InstallJea9Linux`, disables only the host dispatcher while
the personality is installed, and restores the exact previous flag value. Added
`jea9linux_phase13_test.go`, with direct/inline ECALL test
setup, a JIT-with-jea9linux helper,
`TestJea9Linux_JITDoesNotUseHostWrite` proving guest `write(1, ...)`
goes to configured jea9linux stdout rather than host fd 1,
`TestJea9Linux_JITGettidUsesVirtualTid`,
`TestJea9Linux_JITClockUsesDeterministicClock`,
`TestJea9Linux_JITRandomMatchesInterpreter`,
`TestJea9Linux_JITDirectEcallDoesNotRewind`, and
`TestJea9Linux_InstallRestoresDirectSyscallPolicy`. Verified with
the Phase 13 focused suite and the broader jea9linux gate. The remaining
Phase 13 budget/AOT/direct-callout refinements should build on this guarded
policy, especially if the temporary process-global bridge is replaced with a
per-JIT or per-run dispatcher selection.

## 14. Go Runtime Acceptance Tests

Only after the unit tests and tiny C ELF fixtures pass should Go binaries be
used as acceptance tests. Go exercises many features at once: clone, futex,
signals, stack/auxv, random startup, time, netpoll, memory mapping, and page
fault behavior. A failure in a Go binary should point back to a missing lower
level test, not become the primary debugging surface.

If checked-in Go `linux/riscv64` fixtures are small enough, place them under
`testvectors/jea9linux/go/elf/` with source under
`testvectors/jea9linux/go/src/`. If they are too large for the repository,
document an explicit regeneration target and keep the regular root tests focused
on C fixtures. The acceptance target can then be run separately in CI or by
developers with the Go cross-compiler available.

The initial Go acceptance suite should use `GOMAXPROCS` only as an observation,
not as the core determinism mechanism. `sched_getaffinity` returning one CPU is
the OS-level mechanism that makes Go see one CPU. Temporary
`GODEBUG=asyncpreemptoff=1` is allowed only until signal delivery is complete
and should be removed from final acceptance runs.

### Go acceptance tests

1. `TestJea9Linux_GoHello` runs a static Go hello-world binary and verifies
   exact stdout and exit code zero.

2. `TestJea9Linux_GoSchedAffinityOneP` runs a Go binary that prints
   `runtime.GOMAXPROCS(0)` and verifies it observes one CPU by default.

3. `TestJea9Linux_GoTimeNowDeterministic` verifies `time.Now` and monotonic
   readings are deterministic under idle-jump mode.

4. `TestJea9Linux_GoManualClockTimerBlocksUntilAdvance` verifies manual clock
   mode requires external advancement for Go timers.

5. `TestJea9Linux_GoCryptoRandDeterministic` verifies `crypto/rand.Read`
   produces exact seeded output.

6. `TestJea9Linux_GoMathRandStartupUnaffectedByHost` verifies runtime startup
   random seeding does not read host random or host time.

7. `TestJea9Linux_GoGoroutineFutexWake` starts goroutines that park and wake,
   proving clone/futex scheduling is sufficient.

8. `TestJea9Linux_GoTimerSelectIdleJump` verifies timers/select wake at exact
   logical times.

9. `TestJea9Linux_GoNetpollEventfdEpoll` runs a small netpoll-using program if
   the supported fd model is sufficient.

10. `TestJea9Linux_GoNilPointerPanic` verifies nil pointer panic uses the
    signal path and reports a normal Go panic.

11. `TestJea9Linux_GoSIGURGPreemption` verifies signal-based preemption no
    longer requires `asyncpreemptoff`.

12. `TestJea9Linux_GoReplayIdentical` runs the same Go binary twice with the
    same seed, clock, and budget, then compares output, exit code, random log,
    clock log, syscall trace, and schedule trace.

Phase 14 completion note: completed the first Go runtime acceptance pass with
checked-in `linux/riscv64` Go ELF fixtures under `testvectors/jea9linux/go/elf/`
and source under `testvectors/jea9linux/go/src/`. The acceptance harness lives in
`jea9linux_phase14_test.go`, with `TestJea9Linux_GoHello`,
`TestJea9Linux_GoSchedAffinityOneP`,
`TestJea9Linux_GoTimeNowDeterministic`,
`TestJea9Linux_GoCryptoRandDeterministic`,
`TestJea9Linux_GoGoroutineFutexWake`,
`TestJea9Linux_GoTimerSelectIdleJump`,
`TestJea9Linux_GoNilPointerPanic`,
`TestJea9Linux_GoMathRandStartupUnaffectedByHost`,
`TestJea9Linux_GoManualClockTimerBlocksUntilAdvance`, and
`TestJea9Linux_GoReplayIdentical`. Shared fixture setup is in
`runJea9LinuxGoFixture` and `newJea9LinuxGoMachine`.
The red tests exposed three lower level Linux personality gaps that are now
covered bottom-up: initial stack reservation is implemented by
`reserveInitialStackMapping` in `jea9linux.go` and tested by
`TestJea9Linux_InitELFStackReservesStackMapping` in `jea9linux_phase8_test.go`;
`PROT_NONE` reservations are represented separately from unmapped holes by
`mapRange`, `unmapRange`, and `rangeUnmapped` in `jea9linux.go` and tested by
`TestJea9Linux_MmapProtNoneReserveCanBeMprotected`; Linux/riscv64
signal action, `siginfo`, `ucontext`, modified-`rt_sigreturn`, and synthetic
restorer behavior are implemented by `loadJea9LinuxSignalAction`,
`sysRtSigreturn`, `ensureSignalRestorer`,
`storeJea9LinuxSignalUContext`, and
`loadJea9LinuxSignalUContext`. The signal compatibility tests are
`TestJea9Linux_RtSigactionAcceptsLinuxRiscv64Layout`,
`TestJea9Linux_RtSigreturnRestoresModifiedLinuxUContext`,
`TestJea9Linux_SignalWithoutRestorerUsesSyntheticRtSigreturn`, and
`TestJea9Linux_SignalFrameHasLinuxRiscv64UContext`. Manual-clock Go
timer acceptance remains present but skipped because Go needs resumable
sleep/futex scheduling beyond the current blocked-deadline model. Verified with
the Phase 8 VM suite, the Phase 11 signal suite, the full Phase 14 Go acceptance
batch, and the broader jea9linux gate.

## 15. Implementation Order

Implement in this exact order. Do not move to the next phase until the current
phase has red tests written first, then green unit tests, green checked-in ELF
tests where applicable, and interpreter/JIT expectations documented.

1. Add `plans/os_impl_arch.md`, then add the testvector directory skeleton and
   fixture build script. Do not add feature code before tests.

2. Add `Jea9LinuxOptions`, deterministic entropy root, deterministic PRNG,
   clock modes, and instruction-budget scheduler loop. Add budgeted interpreter
   entry point inside `run_cached.go`. Add JIT budget-run support or the narrow
   hook needed by `jea9linux`.

3. Add the stateful ECALL dispatcher and single-context run loop. Unknown
   syscalls return `-ENOSYS`; exit syscalls work.

4. Add Linux initial stack and auxv builder, including deterministic
   `AT_RANDOM` and no VDSO.

5. Add clock and sleep syscalls.

6. Add random syscalls and random devices.

7. Add basic fd table and process/resource syscalls.

8. Add VM overlay, brk, mmap, munmap, mprotect, madvise, mincore, and page-zero
   fault behavior under `jea9linux`.

9. Add clone, guest thread contexts, futex, sched_yield, sched_getaffinity,
   set_tid_address, and set_robust_list.

10. Add eventfd, epoll, pipe, and pselect support.

11. Add Linux signal tables, signal-frame delivery over notes, and
    rt_sigreturn.

12. Add riscv_hwprobe and any red-test-discovered runtime misc syscalls.

13. Make JIT direct ECALLs personality-aware for `jea9linux`, with no host
    syscall bypass and no interpreter restart/rewind fallback, then require
    interpreter/JIT parity tests for every fixture.

14. Add Go runtime acceptance fixtures and remove any temporary
    `asyncpreemptoff` dependency once signal delivery is complete.

## 16. Acceptance Criteria

The first complete `jea9linux` milestone is accepted when:

1. All deterministic control tests pass.

2. All checked-in tiny C ELF fixtures under `testvectors/jea9linux/elf/` pass
   under the cached interpreter.

3. The same fixtures pass under JIT with direct ECALLs going to the
   personality-aware `jea9linux` callout, not host syscalls and not an
   interpreter restart/rewind path.

4. Re-running the same fixture with the same seed, clock mode, instruction
   budget, argv, env, and stdin produces byte-identical stdout, stderr, exit
   code, syscall trace, schedule trace, random observations, and clock
   observations.

5. `sched_getaffinity` exposes one CPU, and no host thread or host scheduler
   decision is used for guest thread execution.

6. No random source, clock read, fd operation, or syscall in `jea9linux` falls
   through to the host by default.

7. Page-zero faults, futex blocking/waking, clone-created contexts, eventfd,
   epoll, and signal delivery all have dedicated unit tests and ELF tests.

8. At least the initial Go hello, deterministic time, deterministic random,
   goroutine/futex, timer/select, and nil-panic acceptance tests pass or have
   documented red-test blockers tied to a specific unimplemented lower-level
   feature.
