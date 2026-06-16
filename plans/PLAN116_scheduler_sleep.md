Q: great. back to the scheduler work. I don't think we really fleshed out the scheduler much, it probably is only half-done. let's review the scheduler for all of the fixes it will need in order to deschedule sysmon and then pick it up later at later clocks. above we identified two issues we want to address: I think we are okay to address 1 now:

1. OS scheduler bug: nanosleep should block/deschedule the current guest context until its deadline, not immediately return while leaving it runnable.

while still keeping an eye out for accounting problems that were 2:

2. JIT amplifier: lazy JIT is still producing way too many budget yields/instruction count, which makes the bad scheduler behavior explode.

so plan out how to actually deschedule the sysmon.

A:
Here’s the concrete plan.

The bug is not really “sysmon” by name. It is that `nanosleep` does not enter the scheduler wait model. Today [sysNanosleep](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:3861) just advances `monotonicNS` and returns runnable in idle-jump mode. So the Go runtime sysmon thread calls `clock_gettime`, calls `nanosleep`, immediately resumes, and repeats. Lazy JIT then amplifies that by bouncing through ECALL/budget paths millions of times.

**Design**
1. Add a new wait kind: `jea9LinuxWaitNanosleep`.
2. Change `nanosleep` success with `delta > 0` to:
   - snapshot the current CPU context,
   - mark current context `waiting`,
   - set `waitKind = jea9LinuxWaitNanosleep`,
   - set `waitDeadlineNS = monotonicNS + delta`,
   - do **not** advance time immediately if another context is runnable,
   - wake with return value `0` when the deadline expires.
3. If another guest context is runnable, load it immediately and return `NoteHandled`. That is the sysmon fix: sysmon stops being runnable, and the useful guest thread runs.
4. If no guest context is runnable:
   - in `Jea9ClockManual`, return `ErrJea9LinuxBlocked` and wait for external `AdvanceTime` / `SetMonotonicNS`;
   - in `Jea9ClockIdleJump`, jump `monotonicNS` to the earliest blocked deadline, refresh waiters, load the newly runnable context, and continue;
   - for `Jea9ClockICTick`, I would initially treat all-blocked-with-deadline like idle-jump too, otherwise no instructions can retire and time can never reach the deadline.
5. Add a scheduler helper that can load **any** runnable context, not just `nextRunnableAfterCurrent()`. The current helper skips the current TID, which is wrong after an all-blocked deadline wake where the sleeping context may be the only one runnable again.
6. Generalize deadline refresh:
   - futex deadline -> runnable with `-ETIMEDOUT`,
   - epoll deadline -> runnable with `0`,
   - nanosleep deadline -> runnable with `0`.
   This should scan contexts or maintain counters correctly. The existing timed futex/epoll counters are easy to make stale, so I’d prefer a simple scan unless profiling says otherwise.

**Where This Lands**
- Main logic in [jea9linux.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:456) wait kinds, scheduler helpers around [ensureScheduler](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:944), and [sysNanosleep](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jea9linux.go:3861).
- `RunWithJea9Linux` and `RunWithJea9LinuxJIT` should handle auto idle-jump blocked deadlines instead of returning an error for default runs.
- Lockstep needs to drive blocked auto-deadlines identically on both sides, otherwise it may stop on legitimate all-sleep states.

**Tests**
- Update `TestJea9Linux_NanosleepIdleJumpSyscall`: direct syscall with one context should block or require idle-jump resume, not simply return runnable immediately.
- Add a two-context nanosleep test: TID 1 sleeps, TID 2 becomes current and runs; TID 1 remains waiting until deadline.
- Add all-blocked idle-jump test: one context sleeps, wrapper jumps to deadline and resumes it with `a0=0`.
- Add JIT parity for the same behavior.
- Then rerun `make lazy-bench`; expected signal is nanosleep/clock syscall count collapsing hard, not just a small timing improvement.

This fixes issue 1 without hiding issue 2: after sysmon deschedules, if lazy JIT still yields too often, it should show up as normal dispatch/budget overhead rather than millions of fake sleep loops.
