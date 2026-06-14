**Findings**

1. **Phase 13 is not at the planned end-state yet.** The plan asks for JIT ECALLs to use a personality-aware direct `jea9linux` callout, and for the C fixture corpus to pass under that path. The current code disables the native host direct-syscall dispatcher while `jea9linux` is installed, then JIT ECALL returns as `jitEcall` and is delivered through the note chain. That proves no host syscall and no rewind, but it is not the direct personality callout described in the plan. See [plans/os_impl_arch.md](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/plans/os_impl_arch.md:1216), [jea9linux.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:3150), [jit_emit_ir.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_emit_ir.go:380), [jit.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit.go:1003).

2. **Replay trace acceptance is not implemented.** The plan requires byte-identical syscall trace, schedule trace, random observations, and clock observations. I do not see trace/log infrastructure for those observations in `jea9linux`; the Go replay test compares only exit/stdout/stderr. See [plans/os_impl_arch.md](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/plans/os_impl_arch.md:1454) and [jea9linux_phase14_test.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux_phase14_test.go:184).

3. **One planned Go acceptance test is explicitly skipped.** `TestJea9Linux_GoManualClockTimerBlocksUntilAdvance` remains pending because Go manual-clock timers need resumable sleep/futex scheduling beyond the current blocked-deadline model. See [jea9linux_phase14_test.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux_phase14_test.go:137) and [plans/os_impl_arch.md](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/plans/os_impl_arch.md:1389).

4. **Two Phase 14 planned tests are absent.** I found no `TestJea9Linux_GoNetpollEventfdEpoll` and no `TestJea9Linux_GoSIGURGPreemption`. Lower-level eventfd/epoll and signal/SIGURG coverage exists, but those Go-runtime acceptance tests have not been added. See [plans/os_impl_arch.md](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/plans/os_impl_arch.md:1346).

5. **Several Phase 13 planned tests are absent.** Present JIT tests cover host-write avoidance, virtual tid, deterministic clock/random, no rewind, and policy restoration. Missing from the planned list are JIT budget state preservation, JIT futex fixture parity, JIT replay trace parity, and the exact “fresh after syscall mode change” coverage. See [plans/os_impl_arch.md](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/plans/os_impl_arch.md:1254) and [jea9linux_phase13_test.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux_phase13_test.go:36).

6. **The plan’s completion-note line numbers are stale in places.** Example: the plan says `Jea9LinuxOptions` is at line 137, but it is currently at [jea9linux.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:278). The implementation has moved; the doc is still useful architecturally, but not reliable as a line-number reference.

I ran the focused suite:

```bash
GOCPU_VIZJIT_OFF=1 go test -count=1 -timeout 240s -run 'TestJea9Linux_|TestRunDefaultBudget|TestJITStepBlockBudget' .
```

It passed in 54.434s.

**Acceptance Criteria 16**

1. **Deterministic control tests:** Met. Budget, clock modes, seeded entropy, manual clock basics, and JIT budget smoke coverage are present and passed.

2. **All checked-in tiny C ELF fixtures under cached interpreter:** Met for the current fixture set. The 27 fixtures under `testvectors/jea9linux/elf/` are covered by split phase suites. Caveat: there is no inventory test that automatically fails if a new `.elf` is checked in but not added to a suite.

3. **Same fixtures under JIT with direct personality-aware ECALL:** Not met. There are synthetic JIT ECALL tests, but not the whole C fixture corpus under JIT, and the current path is `jitEcall` note delivery after disabling host direct syscalls, not the planned personality-aware direct callout.

4. **Byte-identical replay including traces/observations:** Not met. Output/exit replay exists; syscall trace, schedule trace, random observation log, and clock observation log do not appear to exist yet.

5. **`sched_getaffinity` one CPU and no host scheduler for guest threads:** Met. `sched_getaffinity` writes only CPU 0, and clone-created guest threads are saved contexts in the single `Jea9Linux` scheduler, not host threads. See [jea9linux.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:387) and [jea9linux.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:2255).

6. **No host fallthrough by default:** Mostly met for installed `jea9linux`. Unknown syscalls return `-ENOSYS`, random/clock/fd state is virtualized, and `InstallJea9Linux` disables the host direct dispatcher for future JIT blocks. Caveat: that process-global toggle is still a bridge, not the final per-JIT/per-run policy.

7. **Page-zero, futex, clone, eventfd, epoll, signal unit + ELF tests:** Met. I found dedicated unit tests and ELF fixture suites for each of these areas.

8. **Initial Go hello/time/random/goroutine-futex/timer-select/nil-panic:** Met for the listed initial set. Those tests are present and passed. The extra manual-clock timer acceptance remains skipped, and Go netpoll/SIGURG preemption acceptance tests are still missing from the broader Phase 14 plan.
