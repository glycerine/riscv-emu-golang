# PLAN115: Decreasing JIT Budget Details

## Purpose

This note records the agreed instruction-budget design for the native JIT after
the discussion around `emux -jit=true`, Go runtime execution, lockstep testing,
and macro-op fusion. The key decision is that emitted native code must have one
budget meaning, not a proliferation of "precise IC", "lockstep", "debug", and
"production" modes.

The JIT budget register is a decreasing counter of guest RISCV64 instructions (NOT native instructions). The host can still compute and
publish total retired guest instructions, but native code should not switch
between count-up and count-down semantics depending on who called it.

## Core Invariant

`R15` always means **remaining guest instructions for this dispatch**.

On entry to native code, Go writes the requested remaining budget into the JIT
state field loaded by the trampoline into `R15`. For an effectively unbounded
run, Go supplies `^uint64(0)` (`math.MaxUint64`). That value is large enough for
ordinary execution to treat as "forever" without introducing a second emitted
code mode.

Native code consumes budget before executing guest work:

```text
if remaining budget is insufficient:
    return to Go with status=jitBudget and PC=current guest PC
otherwise:
    subtract the number of guest instructions about to be executed
    execute the native implementation
```

On every native exit, generated code spills `R15` back into the JIT state. Go
computes retired guest instructions as:

```text
retired = initial_budget - remaining_budget
```

Go then updates whichever host-side accounting needs total retired
instructions, such as `cpu.riscvInstrBegun` or OS clock accounting. The emitted
code never needs a separate count-up path.

## Native Return Status

Budget exits must return a distinct status, not `jitOK`.

Add a new JIT status constant, for example:

```go
const jitBudget = 8
```

The exact numeric value should avoid existing statuses:

- `0`: `jitOK`
- `1`: `jitEcall`
- `2`: `jitEbreak`
- `3`: `jitLoadFault`
- `4`: `jitStoreFault`
- `5`: `jitIllegal`
- `6`: `jitOKJalrMiss`
- `7`: `jitMisalign`

For the ABJIT path, generated native code already communicates status through
`abjit.State`. On amd64, `State.Status` is at offset `544` from the state base
held in `RBP`; `State.PC` is at offset `536`, `State.FaultAddr` is at offset
`552`, and `State.IC` is at offset `600`. A budget return should therefore:

1. Store the current guest PC in `State.PC`.
2. Store `jitBudget` in `State.Status`.
3. Store zero or diagnostic information in `State.FaultAddr`.
4. Spill `R15` to `State.IC`.
5. Return through the existing trampoline.

This does not require a new ABI register. The trampoline returns to Go exactly
as it already does for ordinary JIT exits; Go reads `s.Status` and `s.IC` in
`abjitDispatch`.

For ARM64 and the older RV8 path, the same logical result should be used:
status is written to the existing result/status slot, and remaining budget is
written to the existing IC/result slot before returning.

## Single Instruction Preflight

For an ordinary non-fused guest instruction, the generated preflight is:

```text
if R15 == 0:
    return jitBudget at current PC
R15 -= 1
execute one guest instruction
```

This ordering is important. The zero test comes first so a budget of zero does
not execute anything and does not need compensation in Go. If a budget of one is
provided, exactly one guest instruction can execute and the return PC is the next
unexecuted guest PC.

This eliminates the old compensation shim in `StepBlockBudget` that decremented
the cumulative IC and then interpreted one boundary instruction. With
count-down semantics, native code returns at the correct PC with correct
remaining budget, so Go does not need to rewind or repair accounting.

## Fused Group Preflight

Existing macro-op fusion must be preserved. The interpreter does not implement
these fusions; it executes one guest instruction at a time. Fusion exists only
in the JIT emitter.

The fused groups currently known in the emitter are bounded and small:

- `AUIPC + ADDI`: 2 guest instructions.
- `AUIPC + JALR`: 2 guest instructions.
- `AUIPC + LOAD`: 2 guest instructions.
- `AUIPC + STORE` to watched `tohost`: 2 guest instructions.
- `SLLI rd,rs1,32 + SRLI rd,rd,32`: 2 guest instructions.
- `ADDIW + SLLI + SRLI`: 3 guest instructions.

No current fusion is known to consume more than 3 guest instructions.

Each fused group should emit a fused preflight based on its exact guest
instruction count:

```text
if R15 < fused_count:
    return jitBudget at current PC with R15 unchanged
R15 -= fused_count
execute fused native sequence
```

This preserves fusion while avoiding partial architectural state inside a fused
sequence. If the remaining budget is too small for a fused group, native code
returns before the group. Go can then choose to interpret one or more guest
instructions from that PC.

The preflight must be atomic. It is not correct to emit N repeated
single-instruction preflights before a fused native sequence, because that would
partially consume budget and then discover too late that the fused native
sequence cannot be entered.

## Go-Side Interpretation Of `jitBudget`

On return from native code, Go has:

```text
initial_budget
remaining_budget = State.IC
retired = initial_budget - remaining_budget
status = State.Status
pc = State.PC
```

There are two important `jitBudget` cases:

```text
status == jitBudget && retired == initial_budget
```

The requested dispatch budget was exhausted exactly. The scheduler or OS
personality should report a normal budget yield.

```text
status == jitBudget && retired < initial_budget
```

The JIT returned before consuming the entire requested budget. The most likely
reason is that the current PC starts a fused group whose `fused_count` is larger
than the remaining budget. Go should not spin. It should execute one guest
instruction with the interpreter, decrement/account the budget accordingly, and
continue or yield as appropriate.

No extra host-side fusion table is needed for control flow. The native return
status identifies a budget gate return, and the difference between initial and
remaining budget tells Go whether progress was made. `FaultAddr` may optionally
carry `fused_count` for diagnostics, but correctness does not depend on it.

## Lockstep Consequences

The current lockstep harness runs the JIT first, then runs the interpreter for
the number of guest instructions reported by the JIT. In `riscv_test.go`,
`runLockstep` sets `DebugOneBlockLockstepMode = true`, but the active budget is
currently `17_731`; the `LockstepModeBudget = 1` single-step setting is present
only as a commented tuning option.

The agreed count-down design still supports future single-step lockstep, but
single-step with fusion requires the `jitBudget` fallback behavior above. If a
budget of one reaches a two-instruction fusion:

```text
R15 = 1
R15 < 2, so native code returns jitBudget at the same PC
Go sees retired = 0 and remaining = 1
Go interprets one guest instruction from that PC
remaining becomes 0
the exact single-step boundary is preserved
```

This works because the interpreter is the exact one-instruction fallback and
because native code explicitly signals that a budget gate, not a normal block
exit, caused the return.

## Implementation Shape

The implementation should proceed bottom-up and test-first:

1. Add tests for `StepBlockBudget` with exact count-down behavior, including
   budget one, budget changes without recompilation, and repeated slices.
2. Add tests for a two-instruction fusion with budget one:
   the JIT must not execute the fused group, Go must make progress by
   interpreting exactly one guest instruction, and total accounting must be
   exact.
3. Add tests for a two-instruction fusion with budget two:
   the fused native path should execute and retire two guest instructions.
4. Add tests for the three-instruction `ADDIW + SLLI + SRLI` fusion with
   budgets two and three.
5. Add `jitBudget` status handling in the dispatcher before changing generated
   code to return it.
6. Add an IR helper for fused reservation, such as `IRBudgetReserve` or
   `IRBudgetNeed`, with `Imm` or `Imm2` holding the required guest instruction
   count and `Dst` holding the cold budget label.
7. Lower the helper on amd64 and arm64 as:

   ```text
   CMP required, R15
   JB/JLT budget_exit
   SUB required, R15
   ```

   The exact signed/unsigned branch mnemonic should match the backend's
   compare operand order. Budgets are unsigned quantities, so prefer unsigned
   comparison semantics.
8. Replace ordinary instruction preflight with the same reserve helper using
   `required = 1`, or keep a specialized zero-check plus decrement if it is
   clearer. The fused preflight must be atomic.
9. Change `IRRetBudget` / budget cold paths to write `jitBudget`, not `jitOK`.
10. Change `abjitDispatch` to load `State.IC` as remaining budget, compute
    `ICdelta = initial - remaining`, and update host-side retired-instruction
    accounting from that delta.
11. Make normal unbounded JIT dispatch enter native code with `^uint64(0)`.
12. Remove the `StepBlockBudget` compensation shim.
13. Refactor comments and field names so `State.IC` is documented as the JIT
    budget/remaining counter at the native boundary, not as a native count-up
    instruction counter.

## Notes On Existing Public Knobs

Existing fields such as `DebugOneBlockLockstepMode`, `LockstepModeBudget`, and
`UseR15InstructionCounter` should not create emitted-code semantic modes. If
they remain for compatibility, they should be treated as host-side dispatch
configuration:

- what budget value Go passes into native code;
- whether a harness asks to stop at a budget boundary;
- whether benchmarks want host-side accounting reported.

They should not cause the emitter to switch between count-up and count-down IR.

## Acceptance Criteria

The change is complete when:

- R15 has one emitted-code meaning: remaining guest instruction budget.
- Native code uses `^uint64(0)` for effectively unbounded execution.
- Budget returns use a distinct `jitBudget` status.
- Go computes retired instructions from `initial_budget - remaining_budget`.
- `StepBlockBudget` has no compensation shim.
- Macro-op fusion remains enabled.
- Each fused group uses its actual `fused_count` for preflight.
- A too-small budget for a fused group makes progress through interpreter
  fallback instead of spinning.
- Current lockstep tests remain green.
- Focused tests cover non-fused, 2-instruction fused, and 3-instruction fused
  budget boundaries.

## Implementation Notes

Implemented the decreasing-budget contract in the native JIT path. `jit.go:179`
defines the distinct `jitBudget` status, `jit.go:190` defines `jitMaxBudget`
as `^uint64(0)`, and `jit.go:430` / `jit.go:437` route `StepBlock` and
`StepBlockBudget` through a caller-provided countdown budget. The dispatcher
handles `jitBudget` at `jit.go:796` and `jit.go:1000`; when a fused group
cannot enter because the remaining budget is too small and no native guest
instruction retired, it interprets one guest instruction to make progress.

The emitted IR now uses a single active budget primitive. `ir.go:283` defines
`IRBudgetReserve`, `highlevel.go:210` exposes `BudgetReserve`, and the emitter
uses it for ordinary instructions at `jit_emit_ir.go:303`. Fused groups reserve
their actual fused width before executing: AUIPC-based two-instruction fusions
start at `jit_emit_ir.go:1152`, and the OP-IMM / OP-IMM-32 fusion helpers cover
the two- and three-instruction patterns later in the same file. The legacy
count-up and zero-check ops remain for older tests/helpers, but normal emission
does not switch modes.

The native boundary now treats R15 as remaining budget. ABJIT sets
`State.IC = budget` and computes `ICdelta = budget - State.IC` in
`jit_abjit.go:56`. The older RV8 trampoline path also accepts an explicit
budget: `internal/jitcall/call_amd64.go` and `internal/jitcall/call_arm64.go`
include the new argument, while the assembly initializes R15 and writes
`initial - remaining` back to `Result.ICdelta` in
`internal/jitcall/call_amd64.s` and `internal/jitcall/call_arm64.s`.
The C sandbox wrapper mirrors that contract through `jit_sandbox.c`.

Compatibility knobs no longer create emitted-code modes. `jit.go:346` keeps
`SetInstructionCounterMode` as a validating shim, and `jit.go:359` always
reports `JITICPrecise` as the effective mode. `UseR15InstructionCounter` is
retained for API compatibility, but R15 is always reserved by JIT compilation
because it is the budget register.

Focused coverage was added in `jea9linux_phase1_test.go` for exact budget
expiry, repeated budget slices, changing budget values without recompilation,
too-small fused-pair fallback, fitted fused-pair execution, too-small
fused-triple fallback, and fitted fused-triple execution. The emux regression
test in `cmd/emux/emux_test.go` remains a bounded completion test for the Go
`timenow.elf` fixture; its guard is intentionally a hang boundary rather than a
tight performance assertion because the current default path lazy-compiles many
Go-runtime blocks.
