# PLAN117: Deterministic Scheduler Testing and Chaos Scheduler

## Context

Jea9Linux currently mixes two concepts that must be separated:

1. How often the host emulator returns to Go control.
2. When the simulated guest OS scheduler makes a semantic scheduling decision.

The first is an implementation detail. It depends on interpreter/JIT chunking,
single stepping, benchmark harnesses, lockstep probes, and host-side budget
choices. The guest must not be able to observe it.

The second is part of the simulated execution. It must be deterministic,
replayable, seed-controlled, and comparable between interpreter, lazy JIT, AOT,
and lockstep runs.

This plan introduces deterministic scheduler testing (DST) and a seeded chaos
scheduler inspired by Robert O'Callahan's rr chaos mode. The key requirement is
that the thread scheduling path be independent of host instruction budget sizes.
Running with budget 1, budget 1000, or one large JIT block must produce the same
guest thread schedule for a fixed seed.

Manual clock mode was removed because it exposed a half-finished blocked/resume
model. The new design keeps virtual time deterministic while avoiding a separate
manual resumption API.

## Goals

- Make host execution budgets invisible to the guest scheduler.
- Use a single deterministic scheduler seed to produce reproducible schedules.
- Use `math/rand/v2.ChaCha8` through the local import alias `mathrand2`.
- Keep scheduler PRNG state on `Jea9Linux`, not `CPU`.
- Expose cached scheduler PRNG state for comparison without advancing the PRNG.
- Support deterministic two-priority scheduling: high and low.
- Support deterministic randomized run quantum sizes.
- Support deterministic chaos windows that temporarily starve low-priority
  threads, bounded to at most 20 percent of simulated run time.
- Support three deterministic virtual-time advancement policies:
  `ClockPolicyOnlyDeadlockAdvances`, `ClockPolicyPRNG`, and
  `ClockPolicyFixed`.
- Provide bottom-up TDD coverage so each invariant is red/green before the next
  layer depends on it.

## Non-Goals

- Do not make CPU carry OS scheduler state.
- Do not use host wall time.
- Do not use Go map iteration order for scheduling decisions.
- Do not draw scheduler randomness during debug/trace comparison.
- Do not couple guest `getrandom` output to scheduler draws.
- Do not make manual clock mode come back as an exposed feature.

## Core Invariant

Host budget expiry is not a scheduler event.

If a run stops because the host poll budget expired:

- do not switch threads;
- do not draw scheduler randomness;
- do not increment scheduler event ID;
- do not reshuffle priorities;
- do not start or end chaos windows;
- do not advance virtual time just because the host budget ended.

The next run continues the same guest context unless a true guest-visible event
has happened.

## State Ownership

### CPU

`CPU` remains hart/ISA state only:

- PC, registers, F registers, CSRs;
- guest memory handle;
- instruction counters;
- trap metadata;
- LR/SC reservation;
- watch address and exit code.

The CPU should not know about scheduler priorities, chaos windows, or scheduler
PRNG state.

### Jea9Linux

`Jea9Linux` owns all scheduler policy state:

- current TID and context table;
- waiting/runnable/exited context states;
- virtual clock;
- scheduler mode/config;
- live scheduler PRNG;
- cached scheduler PRNG snapshot;
- scheduler event ID and draw count;
- priorities;
- next quantum deadline;
- priority reshuffle deadlines;
- chaos starvation windows.

Lockstep compares both CPU state and OS state.

## Scheduler PRNG Design

Import:

```go
import mathrand2 "math/rand/v2"
```

Add to `Jea9Linux`:

```go
schedRNG          *mathrand2.ChaCha8
schedPRNGSnapshot []byte
schedDraws        uint64
schedEventID      uint64
```

Derive the scheduler seed from the existing root seed:

```text
schedSeed = sha256(rootSeed || "jea9linux-scheduler-chacha8-v1")
```

Initialize with:

```go
jos.schedRNG = mathrand2.NewChaCha8(schedSeed)
```

Guest randomness remains a separate stream derived from the same root seed. This
keeps a guest `getrandom` call from perturbing future scheduling choices.

### Cached PRNG Snapshot

The live PRNG must never be inspected by drawing from it. Comparisons read a
cached snapshot:

```go
func (jos *Jea9Linux) SchedulerPRNGState() []byte
func (jos *Jea9Linux) SchedulerPRNGDraws() uint64
func (jos *Jea9Linux) SchedulerEventID() uint64
```

`SchedulerPRNGState` returns a copy of `schedPRNGSnapshot`.

The snapshot is updated only by scheduler code:

- after scheduler RNG initialization;
- after every scheduler RNG draw;
- after deterministic scheduler state changes that should be visible to
  lockstep comparison.

Debug reads, trace snapshots, and lockstep comparisons must not call
`schedRNG.Uint64`, `schedRNG.Read`, or `schedRNG.MarshalBinary` directly.

### RNG Wrapper Discipline

All scheduler randomness goes through wrappers:

```go
func (jos *Jea9Linux) schedUint64(cpu *CPU, why string) uint64
func (jos *Jea9Linux) schedN(cpu *CPU, n uint64, why string) uint64
func (jos *Jea9Linux) commitSchedulerPRNGState(cpu *CPU)
```

Rules:

- no direct `schedRNG.Uint64` calls outside these wrappers;
- no RNG draw on host budget expiry;
- no RNG draw in `Blocked`, `TraceSnapshot`, formatting helpers, or accessors;
- every draw increments `schedDraws`;
- semantic scheduler decisions increment `schedEventID`;
- trace records include enough state to reproduce the decision.

## Scheduler Configuration

Add a scheduler config struct instead of scattered booleans:

```go
type Jea9LinuxSchedulerMode uint8

const (
    Jea9SchedulerRoundRobin Jea9LinuxSchedulerMode = iota
    Jea9SchedulerDST
    Jea9SchedulerChaos
)

type Jea9LinuxSchedulerConfig struct {
    Mode Jea9LinuxSchedulerMode

    Seed [32]byte // optional; zero means derive from root seed

    MinQuantumRetired uint64
    MaxQuantumRetired uint64

    LowPriorityNumerator   uint64 // default 1
    LowPriorityDenominator uint64 // default 10

    PriorityShuffleMinRetired uint64
    PriorityShuffleMaxRetired uint64

    ChaosWindowProbNumerator   uint64
    ChaosWindowProbDenominator uint64
    ChaosWindowMaxNS           int64
    ChaosBudgetNumerator       uint64 // default 1
    ChaosBudgetDenominator     uint64 // default 5
}
```

`RoundRobin` remains the default deterministic simple policy, but it is now a
semantic scheduler mode: it rotates only at retired-instruction scheduler
quantum boundaries or explicit guest-visible scheduling events. It must not
rotate on host poll budget expiry. `DST` enables seeded quanta/priorities
without starvation windows. `Chaos` enables starvation windows.

## Clock Policy Configuration

Clock policy is separate from scheduler mode. The scheduler decides which guest
context may run. The clock policy decides how virtual time advances when the OS
needs to move time forward.

```go
type ClockPolicy uint8

const (
    ClockPolicyOnlyDeadlockAdvances ClockPolicy = iota
    ClockPolicyPRNG
    ClockPolicyFixed
)
```

`ClockPolicyOnlyDeadlockAdvances` is the current idle-jump behavior renamed:
virtual time advances to wait deadlines only when no runnable context can make
progress.

`ClockPolicyPRNG` advances virtual time by a deterministic pseudorandom delta,
sampled uniformly from 1 millisecond through 500 milliseconds. The sample comes
from the OS scheduler PRNG wrappers, so it is seed-controlled and replayable.

`ClockPolicyFixed` advances virtual time by the exact value stored in another
Jea9Linux OS state variable:

```go
clockFixedAdvanceNS int64
```

This fixed policy is used to implement chaos-mode "squeeze" behavior where the
scheduler deliberately advances time in a controlled, deterministic amount.

Clock policy state belongs on `Jea9Linux`:

```go
clockPolicy        ClockPolicy
clockFixedAdvanceNS int64
clockPRNGMinNS      int64 // default 1 * time.Millisecond
clockPRNGMaxNS      int64 // default 500 * time.Millisecond
```

Clock advancement must obey the same determinism discipline as scheduling:

- no clock-policy PRNG draw on host budget expiry;
- no clock-policy PRNG draw during comparison or trace formatting;
- randomized clock deltas are drawn only at semantic OS clock-advance events;
- every PRNG clock draw updates the cached scheduler PRNG snapshot;
- trace records include the policy, delta, pre-time, post-time, and reason.

## Context Scheduler Metadata

Add to each `jea9LinuxContext`:

```go
schedPriority jea9LinuxSchedPriority
```

Priority enum:

```go
type jea9LinuxSchedPriority uint8

const (
    jea9LinuxSchedHigh jea9LinuxSchedPriority = iota
    jea9LinuxSchedLow
)
```

New contexts get high priority until the next deterministic priority shuffle, or
draw priority at creation if the scheduler mode is already active. The decision
must be stable and traced.

## Scheduler Deadlines

Use retired guest instructions for scheduler quanta:

```go
nextScheduleAtRetired uint64
currentQuantumRetired uint64
nextPriorityShuffleAtRetired uint64
```

Draw:

```text
quantum = random in [MinQuantumRetired, MaxQuantumRetired]
nextScheduleAtRetired = cpu.RiscvInstrRetired() + quantum
```

`RiscvInstrRetired` is preferred over instruction attempts because synchronous
exceptions such as ECALL and EBREAK do not retire in RISC-V. Faulting attempts
also do not retire. Scheduling on retired instructions is closer to the
architectural model and avoids counting retried attempts as progress.

Implementation decision: keep host poll budgets and retired scheduler deadlines
as separate first-class APIs. Existing `RunDefaultBudget` and
`JIT.StepBlockBudget` remain attempt/countdown budget APIs for host polling and
low-level countdown tests. Scheduler deadlines use retired-budget APIs:

```go
RunDefaultRetiredBudget(cpu, notes, retiredBudget)
JIT.StepBlockRetiredBudget(cpu, retiredBudget)
RunDefaultDualBudget(cpu, notes, attemptBudget, retiredBudget)
JIT.StepBlockDualBudget(cpu, attemptBudget, retiredBudget)
```

These APIs expire only after the requested number of RISC-V instructions have
retired. Non-retiring ECALL/EBREAK/fault attempts may be begun and handled
inside the call, but do not consume the retired budget.

`RunDefaultDualBudget` and `JIT.StepBlockDualBudget` report which budget limit
expired. Jea9Linux uses that result to keep host polling and semantic
scheduling independent: an attempt-budget expiry returns to the host without a
scheduler event, while a retired-budget expiry performs exactly one scheduler
event.

## Host Budget vs Scheduler Deadline

Every Jea9Linux run computes two independent limits:

```text
hostPollRemaining
schedulerRemainingRetired
```

The execution engine runs until the earlier of:

- host poll budget expires;
- scheduler retired-instruction deadline expires;
- ECALL/trap/fault/exit;
- tohost/watch exit;
- explicit guest-visible yield/blocking syscall.

If host poll budget expires first:

- return `ErrJea9LinuxBudget` or equivalent host-poll result;
- do not schedule;
- do not draw RNG;
- do not change scheduler event ID.

If scheduler deadline expires first:

- save current context;
- clear LR/SC reservation through the normal context-switch path;
- increment scheduler event ID;
- perform due priority reshuffles and chaos-window transitions;
- choose next runnable context;
- draw/install the next quantum.

## Thread Choice

Thread selection must be stable:

- iterate `contextOrder`, never maps;
- skip exited/waiting contexts;
- during chaos windows, skip low-priority contexts;
- preserve round-robin order when scheduler mode is `RoundRobin`, while keeping
  host poll expiry non-semantic;
- record event ID, from TID, to TID, priorities, quantum, and PRNG draw count.

When chaos is active and only low-priority contexts are runnable:

- no guest thread runs;
- virtual time may advance to the earliest of chaos-window end or a high-priority
  wake deadline;
- if no progress is possible, return the blocked state cleanly.

## Priority Reshuffling

At `nextPriorityShuffleAtRetired`:

- walk live contexts in `contextOrder`;
- draw one priority decision per context;
- probability low defaults to 0.1;
- record old/new priority in trace when tracing is enabled;
- draw and install the next reshuffle deadline.

Reshuffle decisions must be independent of host budgets. A host budget that
stops before the reshuffle deadline must not draw anything.

## Chaos Starvation Windows

Chaos mode periodically starts short starvation windows:

- window start decisions occur only at scheduler semantic events;
- window duration is seeded and bounded by `ChaosWindowMaxNS`;
- low-priority contexts cannot run while the window is active;
- high-priority contexts can run normally;
- if all runnable contexts are low priority, guest progress pauses.

Bound total starvation:

```text
chaosBlockedNS <= simulatedElapsedNS * ChaosBudgetNumerator / ChaosBudgetDenominator
```

Default fraction is 1/5, matching the desired "no more than 20 percent of total
run time" cap.

If starting or extending a chaos window would exceed the cap, truncate it or do
not start it.

## Virtual Time

Virtual time remains deterministic and controlled by `ClockPolicy`.

### ClockPolicyOnlyDeadlockAdvances

This is the current idle-jump behavior with a clearer name. Virtual time
advances only when the scheduler cannot make guest progress because every
context is waiting, exited, or filtered out by scheduler policy. The clock jumps
to the earliest deterministic deadline that can make progress possible.

This policy is the conservative default because it minimizes clock movement.

### ClockPolicyPRNG

At semantic OS clock-advance points, draw a pseudorandom duration uniformly from
1 millisecond through 500 milliseconds and advance virtual time by that delta.
The draw uses the scheduler PRNG wrappers, updates the cached PRNG snapshot, and
is recorded in trace when tracing is enabled.

The random advance may or may not reach a pending wait deadline. If it does not,
the waiting context remains waiting and the scheduler records that no context
became runnable. If no context can run after the advance, the OS may perform
another semantic clock-advance event; each such event is visible in scheduler
trace and consumes exactly one deterministic PRNG draw.

### ClockPolicyFixed

Advance virtual time by exactly `clockFixedAdvanceNS`. There is no random draw.
This is used by chaos mode to implement deliberate squeeze intervals where the
scheduler controls time pressure precisely.

The fixed amount is part of Jea9Linux OS state and must appear in lockstep state
comparison. Changing it is a scheduler semantic event, not a host-budget effect.

Chaos mode adds one more reason no context can run: all runnable contexts are
low priority during a starvation window. In that case the scheduler may advance
virtual time to the earlier of:

- starvation window end;
- timeout that wakes a high-priority context;
- other deterministic wake event.

No host wall-clock time is used.

## Trace Additions

Extend `Jea9LinuxScheduleTraceEntry` or add scheduler-specific trace entries:

```go
SchedEventID       uint64
SchedPRNGDraws     uint64
SchedPRNGState     []byte // trace/debug only
RiscvInstrRetired  uint64
QuantumRetired     uint64
Reason             string
FromPriority       string
ToPriority         string
ChaosActive        bool
ChaosUntilNS       int64
ClockPolicy        string
ClockAdvanceNS     int64
ClockBeforeNS      int64
ClockAfterNS       int64
```

Keep normal runs cheap. Full PRNG state should be included only when tracing is
enabled or a debug/lockstep mode requests it.

## Bottom-Up TDD Coverage

### 1. PRNG Initialization and Snapshot Tests

Red tests first:

- `TestJea9Linux_SchedulerPRNGStateInitializesDeterministically`
  - same entropy seed gives same scheduler PRNG snapshot;
  - different entropy seed gives different snapshot.

- `TestJea9Linux_SchedulerPRNGStateReadDoesNotAdvance`
  - repeated calls to `SchedulerPRNGState` return identical bytes;
  - draw count and event ID do not change.

- `TestJea9Linux_SchedulerPRNGDrawUpdatesSnapshot`
  - one scheduler draw changes snapshot and increments draw count.

Implementation:

- add scheduler PRNG fields;
- add seed derivation;
- add copy-only accessors;
- add RNG wrapper.

### 2. Scheduler Event Accounting Tests

Red tests:

- `TestJea9Linux_HostBudgetDoesNotAdvanceSchedulerEvent`
  - run with tiny host budget until budget expiry;
  - event ID, draw count, PRNG snapshot unchanged.

- `TestJea9Linux_SchedulerDecisionAdvancesEvent`
  - force a scheduler quantum expiry;
  - event ID increments exactly once.

Implementation:

- separate host poll result from scheduler event result;
- stop calling the scheduler from raw host budget expiry.

### 3. Retired-Instruction Deadline Tests

Red tests:

- `TestJea9Linux_SchedulerQuantumUsesRetiredNotBegun`
  - program includes ECALL/EBREAK or a handled trap attempt;
  - scheduler quantum does not count non-retired trap instruction as retired
    progress.

- `TestJea9Linux_SchedulerDeadlineStopsAtRetiredCount_Interpreter`
  - interpreter stops exactly at requested retired count.

- `TestJea9Linux_SchedulerDeadlineStopsAtRetiredCount_LazyJIT`
  - lazy JIT matches interpreter.

Implementation:

- add retired-deadline execution support or a proven bridge around existing
  budget mechanics;
- verify JIT retirement accounting before enabling scheduler quanta on JIT.

### 4. Budget Independence Tests

Red tests:

- `TestJea9Linux_ScheduleTraceIndependentOfHostBudget_Interpreter`
  - same program, same seed;
  - compare host budgets 1, 3, 1000;
  - schedule trace TID sequence, event IDs, draw counts, and PRNG snapshots match.

- `TestJea9Linux_ScheduleTraceIndependentOfHostBudget_LazyJIT`
  - same as above for lazy JIT.

- `TestJea9Linux_ScheduleTraceInterpreterMatchesLazyJIT`
  - same seed and same scheduler config;
  - interpreter and lazy JIT produce same scheduler trace.

Implementation:

- run engines with `min(hostPollRemaining, schedulerRemaining)`;
- host poll expiry returns without scheduling;
- scheduler expiry schedules exactly once.

### 5. Priority Assignment Tests

Red tests:

- `TestJea9Linux_PriorityShuffleStableByContextOrder`
  - create contexts in a known order;
  - reshuffle with fixed seed;
  - priorities are deterministic and independent of map iteration.

- `TestJea9Linux_PriorityShuffleSameSeedSameResult`

- `TestJea9Linux_PriorityShuffleDifferentSeedUsuallyDiffers`

Implementation:

- add priority field to context;
- add stable reshuffle over `contextOrder`;
- add trace coverage.

### 6. Random Quantum Tests

Red tests:

- `TestJea9Linux_RandomQuantumWithinBounds`

- `TestJea9Linux_RandomQuantumSameSeedSameSequence`

- `TestJea9Linux_RandomQuantumDoesNotDrawOnHostBudget`

Implementation:

- add quantum draw/install helpers;
- record quantum in trace;
- enforce min/max.

### 7. Thread Choice Tests

Red tests:

- `TestJea9Linux_RoundRobinModeMatchesOldOrder`

- `TestJea9Linux_DSTSkipsWaitingAndExitedContexts`

- `TestJea9Linux_DSTChoosesRunnableInStableOrder`

Implementation:

- centralize thread choice into one scheduler helper;
- remove ad hoc `nextRunnableAfterCurrent` calls from semantic paths where
  policy matters;
- default round-robin mode is now a semantic scheduler mode too; it preserves
  stable context order but does not depend on host-budget expiration.

### 8. Clock Policy Tests

Red tests:

- `TestJea9Linux_ClockPolicyOnlyDeadlockAdvances`
  - with at least one runnable context, virtual time does not advance just
    because a wait deadline exists;
  - when no context can run, virtual time advances to the earliest deadline.

- `TestJea9Linux_ClockPolicyPRNGAdvanceWithinBounds`
  - each semantic clock advance is between 1ms and 500ms inclusive.

- `TestJea9Linux_ClockPolicyPRNGSameSeedSameDeltas`
  - same seed produces the same sequence of clock deltas and PRNG snapshots.

- `TestJea9Linux_ClockPolicyPRNGDoesNotDrawOnHostBudget`
  - host poll expiry does not change clock delta sequence, draw count, or PRNG
    snapshot.

- `TestJea9Linux_ClockPolicyFixedUsesStateValue`
  - clock advances by exactly `clockFixedAdvanceNS`;
  - changing `clockFixedAdvanceNS` is visible in scheduler/lockstep OS state.

- `TestJea9Linux_ClockPolicyFixedChaosSqueeze`
  - chaos mode can set a fixed squeeze delta and produce deterministic trace.

Implementation:

- add clock policy config/state;
- implement a single `advanceVirtualClockForSchedulerEvent` helper;
- route deadlock, PRNG, and fixed advancement through that helper;
- route timeout/deadline fast-forwards through a deadline helper instead of
  assigning `monotonicNS` directly from syscall paths;
- deadline fast-forwards refresh waiters and chaos state, but do not leave
  pending schedule-trace clock metadata unless they occur inside a scheduler
  event;
- ensure all PRNG clock draws go through scheduler RNG wrappers.

### 9. Chaos Window Tests

Red tests:

- `TestJea9Linux_ChaosWindowSkipsLowPriority`

- `TestJea9Linux_ChaosWindowBlocksWhenOnlyLowPriorityRunnable`

- `TestJea9Linux_ChaosWindowEndsDeterministically`

- `TestJea9Linux_ChaosStarvationBudgetCappedAtTwentyPercent`

- `TestJea9Linux_ChaosSameSeedSameTrace`

Implementation:

- add chaos window state;
- add deterministic start/end decisions;
- add virtual-time advance for all-low-priority blocked windows;
- enforce starvation budget cap.

### 10. Lockstep Integration Tests

Red tests:

- `TestJea9Linux_LockstepSchedulerStateCompareDoesNotAdvancePRNG`
  - compare scheduler state repeatedly;
  - no draw/event changes.

- `TestJea9Linux_ZygoLockstep_SchedulerIndependentOfBudget`
  - run the existing Zygo lockstep workload with multiple host budgets;
  - scheduler trace remains identical.

- `TestJea9Linux_ZygoLockstep_ChaosReplay`
  - fixed chaos seed reproduces exactly.

Implementation:

- extend lockstep OS comparison to include scheduler event ID, draw count, PRNG
  snapshot, current TID, priorities, clock policy state, fixed clock delta, and
  pending scheduler deadlines.

## Implementation Order

### Phase 0: Rename Concepts Without Behavior Change

- Document that current `InstructionBudget` is a host poll budget.
- Add comments around current `expireBudget` behavior explaining it will change.
- No behavior change yet.

Tests:

- existing suite stays green.

### Phase 1: Add OS-Owned Scheduler PRNG

- Add `mathrand2 "math/rand/v2"` import.
- Add scheduler PRNG fields to `Jea9Linux`.
- Derive seed from root seed.
- Add snapshot and accessors.
- Add RNG wrappers.

Tests:

- PRNG initialization/snapshot tests.

### Phase 2: Trace Scheduler PRNG Metadata

- Add event ID, draw count, and optional PRNG snapshot to trace.
- Keep full state trace gated by `Trace`.

Tests:

- trace deep copy;
- trace does not advance PRNG;
- trace contains event/draw metadata after a scheduler decision.

### Phase 3: Make Host Budget Non-Scheduling

- Change host budget expiry so it does not choose another thread.
- Preserve outer run-loop behavior: returning to caller for polling is still OK.
- Add explicit scheduler event path for real scheduling decisions.

Tests:

- host budget does not change TID, event ID, draw count, or PRNG snapshot.
- old budget tests updated to expect no scheduler decision on budget alone.

### Phase 4: Add First-Class Retired-Budget Execution Support

- Add interpreter support via `RunDefaultRetiredBudget`.
- Add lazy JIT support via `JIT.StepBlockRetiredBudget`.
- Add dual-budget support via `RunDefaultDualBudget` and
  `JIT.StepBlockDualBudget` so Jea9Linux can run with both host-poll and
  scheduler-retired limits at once.
- Preserve existing attempt/countdown budget APIs for host polling.
- Verify non-retired ECALL/EBREAK/fault behavior.

Tests:

- direct retired-budget API tests for interpreter and lazy JIT;
- retired deadline tests for interpreter and lazy JIT.
- JIT/interpreter accounting equivalence.

### Phase 5: Add Deterministic Scheduler Quantum

- Add scheduler quantum config and state.
- Draw quantum only on scheduler events.
- Install `nextScheduleAtRetired`.
- Run engines with host poll and scheduler retired deadlines as independent
  limits.
- If host poll expires first, snapshot and return with no scheduling.
- If scheduler retired budget expires first, perform exactly one scheduler
  event.
- Do not infer retired deadlines from attempts-used deltas; use the first-class
  retired-budget result from the execution engine.

Tests:

- quantum bound and sequence tests;
- budget independence tests.

### Phase 6: Centralize Thread Choice

- Add a single scheduler decision helper.
- Route yield, timeout wake, blocking syscall switch, exit switch, and quantum
  expiry through that helper.
- Keep default mode equivalent to current round-robin behavior.

Tests:

- round-robin compatibility;
- stable context order;
- no map-order dependence.

### Phase 7: Add Priorities and Reshuffle

- Add priority field to context.
- Add deterministic reshuffle deadline.
- Draw priorities in stable context order.

Tests:

- priority reshuffle tests;
- same seed/same result;
- different seed changes schedule in a bounded test.

### Phase 8: Add Clock Policies

- Add clock policy state and config.
- Implement `ClockPolicyOnlyDeadlockAdvances`.
- Implement `ClockPolicyPRNG` using scheduler RNG wrappers.
- Implement `ClockPolicyFixed` using `clockFixedAdvanceNS`.
- Add trace fields for clock policy and clock delta.

Tests:

- clock policy tests;
- PRNG clock advance does not draw on host budget;
- fixed clock advance appears in lockstep OS state.

### Phase 9: Add Chaos Windows

- Add chaos state and config.
- Add window start/end decisions.
- Skip low-priority contexts during active windows.
- Bound starvation to 20 percent of simulated run time.
- Use `ClockPolicyFixed` for chaos squeeze intervals.

Tests:

- low-priority skip;
- all-low-priority blocked behavior;
- chaos window end;
- starvation cap;
- same seed replay.

### Phase 10: Integrate With Zygo Lockstep and Benchmarks

- Add scheduler state to lockstep comparison.
- Run Zygo interpreter/lazy lockstep under multiple host budgets, including
  tagged Zygo tests that compare real scheduler traces.
- Run with chaos mode seed and record trace.
- Keep normal benchmarks on deterministic round-robin unless chaos/DST is
  explicitly configured, but round-robin still uses semantic retired quanta and
  never host-budget scheduling.

Tests:

- Zygo budget independence;
- interpreter vs lazy JIT scheduler trace match;
- chaos replay.

## Risks and Open Questions

### Retired Deadline Support

Scheduler quanta must never depend on host poll budget size. The retired-budget
APIs are the contract for this. It is acceptable for the JIT implementation to
use native countdown dispatch internally, but the exported retired-budget API
must not report expiration until retired instructions, not attempts, reach the
requested deadline.

### ECALL and Retired Count

RISC-V ECALL does not retire. A guest that performs many syscalls still makes
progress through surrounding retired instructions, but syscall-heavy loops need
coverage so the scheduler cannot starve event decisions unexpectedly.

### PRNG Snapshot Cost

`ChaCha8.MarshalBinary` is small, but still not free. Snapshot after scheduler
draws/events only, never per guest instruction.

### Trace Size

Full PRNG state in every schedule trace entry can be noisy. Gate it behind
`Trace` or a dedicated scheduler debug flag.

### Backwards Compatibility

Default round-robin mode preserves stable round-robin order, but intentionally
does not preserve the old host-budget scheduling side effect. Host poll expiry
is an implementation detail in every scheduler mode.

## Success Criteria

- Same seed and same guest produce identical scheduler traces under host budgets
  1, 3, 1000, and large JIT chunks.
- Interpreter and lazy JIT match scheduler event IDs, PRNG draw counts, PRNG
  snapshots, current TID, priorities, and wake deadlines.
- Repeated scheduler-state comparison does not advance the scheduler PRNG.
- Clock policy behavior is deterministic, traceable, and independent of host
  budget expiry.
- `ClockPolicyPRNG` produces reproducible 1ms-to-500ms deltas for a fixed seed.
- `ClockPolicyFixed` advances virtual time by exactly the OS state value and can
  be used by chaos squeeze intervals.
- Chaos mode reproduces exactly for a fixed seed.
- Chaos mode finds starvation-sensitive bugs without compromising deterministic
  replay.
