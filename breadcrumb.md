# Breadcrumb Trace Plan

Goal: add a deterministic-execution breadcrumb system for the interpreted CPU paths, so two executions of the same guest can be compared and bisected to the first instruction where their control flow diverges.

Default builds must keep the RV64 CPU as fast as possible. Breadcrumbing should be compiled in only when explicitly requested with:

```bash
go test -tags breadcrumb ...
go build -tags breadcrumb ./cmd/emu
```

In non-breadcrumb builds, the trace hooks should compile away to no-ops. Do not add a runtime `if cpu.breadcrumb != nil` branch to the default interpreter hot path.

This plan intentionally targets the interpreter only:

- The `cmd/emu -run` non-JIT path uses the decoder-cached interpreter:
  `RunEmu -> RunWithJea9LinuxInterp -> Jea9Linux.Run -> RunDefaultDualBudget -> runCachedDualBudget`.
- The `cmd/emul` and `cmd/emu -bios` path boots OpenSBI/Linux and uses the machine-mode interpreter:
  `RunEmu -> runEmuBios -> RunBiosMachineBudget -> runMachineBudget -> cpu.Step`.
- The uncached reference path uses:
  `RunWithChain -> cpu.step`.
- JIT/native paths are out of scope for the first implementation.

## Basic Idea

Maintain a streaming BLAKE3 hash over the sequence of guest PCs attempted by the CPU. For guest Linux, hash only user-mode attempted PCs by default. Kernel, OpenSBI, interrupt, scheduler, and trap-handler PCs are ignored unless the tracer is explicitly configured to include privileged modes.

At fixed breadcrumb-sequence intervals, write a checkpoint containing the number of PCs actually hashed, the current PC, and the current hash digest. The checkpoint cadence must be based on the hashed sequence count, not the absolute CPU attempt count, so nondeterministic kernel bookkeeping does not shift checkpoint boundaries.

For a first version, hash only the pre-execution PC of each attempted instruction:

```text
seq=1000 pc=0x00000000000123f0 hash=...
seq=2000 pc=0x0000000000012458 hash=...
```

The pre-execution PC is important because faulting instructions, ECALL, EBREAK, illegal instructions, and page faults should still contribute the PC that caused the attempt.

For the user-mode filter, use the privilege mode before executing the instruction. That includes a user-mode `ECALL` instruction in the hash, but excludes the S-mode/M-mode instructions that service it. It also excludes `SRET`/`MRET`; the first instruction hashed after a return from the kernel is the next actual user-mode instruction.

The hash/checkpoint logic only exists in `-tags breadcrumb` builds. Normal builds should not allocate a tracer, should not carry tracer state on `CPU`, and should not execute a per-instruction enabled check.

## Hash Package

Use the already vendored/local module dependency:

```go
import "github.com/glycerine/blake3"
```

The local API supports:

```go
h := blake3.New(32, nil)
h.Write(buf[:])
digest := h.Sum(nil)
```

Use a 32-byte digest. Encode PCs as fixed 8-byte little-endian values before writing to the hasher. Fixed binary encoding avoids textual formatting ambiguity and keeps per-instruction overhead small.

## Build Tags

Use paired implementation files:

```text
breadcrumb_on.go   //go:build breadcrumb
breadcrumb_off.go  //go:build !breadcrumb
```

`breadcrumb_off.go` provides compile-time constants and no-op helpers:

```go
const breadcrumbEnabled = false

func breadcrumbRecordPC(_ *CPU, _, _, _, _ uint64, _ PrivilegeMode) error {
	return nil
}

func breadcrumbBeforeAttempt(_ *CPU, _, _, _, _ uint64, _ PrivilegeMode) (bool, error) {
	return false, nil
}

func breadcrumbAfterAttempt(_ *CPU, _, _, _, _ uint64, _ PrivilegeMode) error {
	return nil
}

func breadcrumbFlush(_ *CPU) error {
	return nil
}
```

`breadcrumb_on.go` provides the real implementation:

```go
const breadcrumbEnabled = true
```

Then hot loops use a compile-time guard:

```go
if breadcrumbEnabled {
	if err := breadcrumbRecordPC(cpu, attempt, retired, pc, satp, priv); err != nil {
		return err
	}
}
```

In default builds, `breadcrumbEnabled` is a constant false, so the compiler should delete the whole block. In breadcrumb builds, the guard is a constant true, so only the real record call remains.

After implementation, verify this assumption with at least one of:

- `go test -gcflags='-m=2'` on a narrow package target to see the no-op path inline/disappear.
- `go tool objdump` on the built package/test binary to confirm the default interpreter loop has no breadcrumb call/branch.
- `GOCPU_VIZJIT_OFF=1 go test -bench='^BenchmarkCPU_FullExecution_Cached$' -benchtime=10s ./bench/` to check the cached interpreter did not regress.

## Proposed Types

In `breadcrumb_on.go`, add a small tracing facility:

```go
type BreadcrumbConfig struct {
	Path          string
	Outfile       *os.File
	Interval      uint64
	StartAt       uint64
	StopAt        uint64
	AfterInterval uint64
	AfterAt       uint64
	StartPaused   bool
	GuestControl  bool
	IncludePrivileged bool
}

type BreadcrumbTracer struct {
	w             *bufio.Writer
	outfile       *os.File
	path          string
	h             *blake3.Hasher
	interval      uint64
	startAt       uint64
	stopAt        uint64
	afterAt       uint64
	afterInterval uint64
	nextCheckpoint uint64
	lastPC        uint64
	active        bool
	guestControl  bool
	includePrivileged bool
	filterAddressSpace bool
	targetSATP    uint64
	targetSATPValid bool
	targetPID     uint32
	seq           uint64
	epoch         uint64
	buf           [8]byte
}
```

`Interval` is the normal checkpoint interval, measured in hashed PCs. `StartAt` and `StopAt` allow focused runs over the hashed sequence. `StopAt` records the target PC after that instruction has executed, flushes/syncs the trace, raises the breadcrumb MMIO interrupt, then pauses hashing. With the guest Linux breadcrumb driver enabled, that interrupt sends `SIGSTOP` to the registered guest PID before the next user instruction executes. `AfterAt` plus `AfterInterval` allows a run to begin coarse, then automatically switch to finer checkpoints around a suspected sequence region.

`Path` is the requested breadcrumb output path. `Outfile` is the open file the tracer writes to. Use `*os.File`, not a generic `io.Writer`, so the implementation can own file creation, flushing, optional syncing, and close behavior explicitly. If a caller passes `Path` without `Outfile`, `NewBreadcrumbTracer` should create/truncate that file. If a caller passes `Outfile`, the tracer should write to that file and use `Path` only for diagnostics.

`StartPaused` creates the tracer but does not hash PCs until activation. In breadcrumb builds, this should be the default for guest Linux: host flags configure the output path, cadence, and sequence window, but the tracer waits for guest MMIO activation before hashing anything. `GuestControl` exposes a guest-visible control path for BIOS/Linux runs so userspace inside the guest can start, stop, and reset breadcrumbs after boot.

`IncludePrivileged` opt-ins to hashing all privilege modes. Leave it false for guest Linux program tracing so random kernel timer ticks and scheduler work do not affect the breadcrumb stream.

In guest-control mode, target address-space filtering is enabled by default. The emulator cannot infer a Linux PID from architectural CPU state without guest-kernel cooperation, so the MMIO `target_pid` register is guest-provided metadata for logs and optional tooling. The tracer captures `satp` when it activates in user mode, then hashes only future user-mode attempted PCs with the same pre-instruction `satp`. That keeps unrelated guest user processes out of the hash when Linux schedules them during target sleeps, syscalls, or timeslice gaps. Threads that share an address space also share `satp`; exact per-thread or per-PID filtering would require guest cooperation.

Keep the first implementation simple:

- `NewBreadcrumbTracer(cfg BreadcrumbConfig) (*BreadcrumbTracer, error)`
- `func (b *BreadcrumbTracer) RecordPC(attempt, retired, pc uint64, priv PrivilegeMode) error`
- `func (b *BreadcrumbTracer) Activate(attempt, retired, pc uint64, reset bool) error`
- `func (b *BreadcrumbTracer) Pause(attempt, retired, pc uint64) error`
- `func (b *BreadcrumbTracer) Flush() error`
- `func (b *BreadcrumbTracer) Close() error`
- `func (c *CPU) SetBreadcrumbTracer(b *BreadcrumbTracer)`
- `func (c *CPU) BreadcrumbTracer() *BreadcrumbTracer`

Do not add a tracer pointer to `CPU` in the default build. This preserves the CPU struct layout and avoids perturbing cache locality in the normal emulator.

```go
var breadcrumbTracers sync.Map // map[*CPU]*BreadcrumbTracer
```

In `-tags breadcrumb` builds, `SetBreadcrumbTracer` stores into this side table and `SetBreadcrumbTracer(nil)` deletes from it. A package-level hook counter gates the side-table lookup so an idle guest-controlled BIOS tracer does not pay a `sync.Map` lookup on every Linux boot instruction while it is merely waiting for the guest MMIO arm command. Once a tracer is active, armed, or has pending control, the diagnostic build pays the side-table lookup because the user explicitly requested tracing.

In `!breadcrumb` builds, either omit the public breadcrumb API entirely or provide stubs that return a clear disabled error when callers try to construct/attach a tracer. Prefer not to make a default `emul` silently accept a breadcrumb flag and produce no trace.

## Guest-Controlled Activation

For real guest Linux (`cmd/emul` / `cmd/emu -bios`), a guest userspace ECALL is not visible to the emulator as a custom host syscall. The ECALL is handled by the guest kernel. Therefore, guest-controlled breadcrumb activation should use a tiny MMIO control device.

Add a breadcrumb MMIO page only in `-tags breadcrumb` builds and only when the host enables breadcrumbing in config:

```text
biosBreadcrumbBase = 0x10002000
biosBreadcrumbSize = 0x1000
compatible = "glycerine,riscv-breadcrumb-v1"
```

Advertise it in the generated FDT, alongside `hostio@10001000`, so Linux can discover it. A root userspace helper in the initramfs can use `/dev/mem` to map `0x10002000` and write the control registers. Precision stop-at-divergence uses the guest Linux platform driver in `drivers/misc/riscv-breadcrumb.c`; the driver binds to `glycerine,riscv-breadcrumb-v1`, handles the MMIO interrupt, reads the hit registers, and sends the configured signal to the registered PID.

Suggested registers:

```text
0x00 magic       read  u32 "BCR1"
0x04 version     read  u32 1
0x08 status      read  u32 0=idle 1=active 2=paused 3=error
0x10 control     write u32 1=start/resume, 2=pause, 3=reset-and-start, 4=flush, 5=arm-next-user-reset, 6=arm-next-user-resume, 7=arm-next-address-space-reset, 8=arm-next-address-space-resume
0x18 interval    read/write u64 checkpoint interval
0x20 after_at    read/write u64 dynamic interval switch point
0x28 after_int   read/write u64 dynamic interval after after_at
0x30 trip_seq    read/write u64 one-shot trip at hashed breadcrumb seq
0x38 trip_attempt read/write u64 one-shot trip at absolute attempted instruction count
0x40 trip_signal read/write u32 guest signal, default 19/SIGSTOP
0x44 trip_pid    read/write u32 guest PID metadata; defaults to target_pid if unset
0x48 trip_mode   read/write u32 0=disabled, 1=trip_seq, 2=trip_attempt
0x4c irq_status  read/write u32 bit 0=pending; write 1 to acknowledge
0x50 hit_seq     read  u64 breadcrumb seq that tripped
0x58 hit_attempt read  u64 absolute attempted instruction count that tripped
0x60 hit_pc      read  u64 attempted PC that tripped
0x68 target_pid  read/write u32 guest-provided target PID for logs/tooling
0x70 target_satp read/write u64 captured or guest-overridden target address space
0x78 target_status read/write u32 bit 0=filter enabled, bit 1=target_satp valid
```

The guest should not choose the host output path. The host should still provide `BreadcrumbConfig.Path` or `Outfile` at emulator startup. The host also configures interval, start/stop sequence bounds, and privilege filtering. The guest controls only when the already-configured tracer becomes active.

Default guest-Linux behavior:

1. `-breadcrumb path` creates/truncates the output file and attaches a tracer.
2. The tracer starts inactive and armed-for-control. It writes only the header initially.
3. The MMIO device is exposed in the generated FDT.
4. No PCs are hashed during OpenSBI, Linux boot, init, shell setup, or any other pre-test work.
5. A guest write to the MMIO control register activates or arms the tracer.
6. For a launcher that arms and then `execve`s a program, prefer `arm-next-address-space-reset`; it waits for a privileged interlude and then the first user-mode return whose `satp` differs from the arming process.

Activation should take effect after the MMIO store instruction completes. That gives intuitive guest code:

```c
breadcrumb_start();
run_the_test();
breadcrumb_pause();
```

The first hashed PC is the instruction after `breadcrumb_start()`. If tracing is active, `breadcrumb_pause()` itself may be included as the final active instruction; document and test the exact boundary.

On activation with reset, reset the BLAKE3 hasher and increment an `epoch`. Checkpoint lines stay in breadcrumb sequence coordinates, while event lines keep the absolute instruction-attempt context. The hash only covers PCs since the most recent reset/start:

```text
# activation epoch=1 seq=0 attempt=123456 retired=123400 pc=0xffffffff8001234c mode=reset scope=user
seq=1000 pc=0xffffffff80013000 hash=...
```

This isolates the program-under-test from nondeterministic Linux boot and setup. Boot can vary; the breadcrumb hash begins only when the guest says the experiment starts.

## Exact Program-Start Activation

Starting breadcrumbs from a guest helper with `start; execve(program)` is still fuzzy: the helper can execute user instructions after arming, and a guest timer interrupt can return to the helper before it reaches the `execve` trap. If the goal is "start when this guest program begins running", use an armed activation mode instead of immediate activation.

Recommended launcher mode: `arm-next-address-space-reset`.

Semantics:

1. Guest writes its PID to `target_pid` and `trip_pid`, then writes `control=7` to the breadcrumb MMIO page.
2. The tracer resets its hash, increments `epoch`, and becomes armed but inactive.
3. The emulator remembers the arming process's current `satp`.
4. The emulator waits until the CPU has entered privileged mode after the arm request.
5. Returns to the same user `satp` are ignored, which covers timer interrupts landing between the MMIO write and the `execve` trap.
6. The emulator activates only after a later transition back to `PrivUser` with a different `satp`.
7. The first hashed PC is the next attempted instruction in that newly installed address space.

This enables a tiny guest launcher:

```c
breadcrumb_arm_next_address_space_reset();
raw_execve("/path/to/test-program", argv, envp);
```

The launcher should make the MMIO write and then issue the raw `execve` syscall directly. If `execve` succeeds, it never returns to the launcher; the next different user address space should be the launched image. If `execve` fails and execution returns to the launcher's original address space, the tracer stays armed instead of capturing the launcher failure path.

This requires a tiny guest helper binary or shell builtin that can map the breadcrumb MMIO page, write the control register, and then immediately perform `execve`.

The repository includes `cmd/breadcrumbexec` as this launcher. It is intentionally C, not Go, so no launcher runtime threads or signal machinery survive before the target `execve`. Build and pack it with `make bread`, or directly with `zig cc -target riscv64-linux-musl -static -O2 -fno-stack-protector -fno-sanitize=all -o ... cmd/breadcrumbexec/breadcrumbexec.c`. Inside the guest, run `breadcrumbexec [--] program [args...]`. The optional `--` is accepted for compatibility with tools that use it as an argument separator.

The launcher cannot see the host breadcrumb output path. It only writes the MMIO control registers. The emulator owns the host file lifecycle. In guest-controlled BIOS mode, the output file is not opened during Linux boot; the first reset-style arm command opens it. Each later reset-style command closes any owned open handle, rotates an existing path to the first free suffix such as `.01` or `.02`, and opens a fresh file at the configured path. If the operator has already moved the previous trace out of the way, the close still releases the old inode and the new trace is created at the original path.

`arm-next-user-reset` remains useful when the guest can guarantee there is no user-mode return to the arming process before the target starts. In practice, the address-space-change variant is more robust against guest timer interrupts and scheduler activity.

## Exact Divergence Tripwire

Once two breadcrumb logs identify a divergence window, the guest should be able to re-run with a one-shot trip point. The emulator-side trip should be exact in breadcrumb coordinates.

Use `trip_seq` for the usual deterministic case. It is counted in hashed PCs, so ignored kernel instructions do not move the target. `trip_attempt` is also available when debugging the raw machine interpreter timeline. After the configured instruction executes, the tracer stores `hit_seq`, `hit_attempt`, and `hit_pc`, disables `trip_mode`, flushes/syncs the trace file, raises the breadcrumb IRQ, and pauses hashing. Host `StopAt` behaves like an automatic `trip_seq` boundary for the trace window.

For guest Linux, the breadcrumb platform driver handles the IRQ before the emulator returns to the next user instruction. The driver reads the registered `trip_pid`/`target_pid` and sends the configured guest signal, `SIGSTOP` by default. That leaves the target process stopped under Linux, ready for `dlv attach PID` or `gdb -p PID`, without running the debugger around `breadcrumbexec` and without relying on synthetic `EBREAK`.

The MMIO trip registers expose the configured/hit sequence, attempt, PC, target PID, and signal number as metadata. The emulator syncs the host trace before asserting the interrupt, so the comparison log should contain the final trip checkpoint/event even if the guest process is inspected or killed immediately after stopping.

If the guest kernel lacks the breadcrumb driver, the trace still records and syncs the trip event, but no guest signal is delivered. In that case the target can continue running because no kernel handler acknowledges and acts on the pending device interrupt.

The precision boundary is: the trip instruction has retired and contributed its PC to the hash; the next interpreter loop notices the pending external interrupt and vectors into the guest kernel; the driver sends `SIGSTOP`; Linux stops the target before returning to user mode. That is intentionally after instruction `N`, before instruction `N+1`.

Implementation detail:

- Track pending arm state in the breadcrumb MMIO/tracer side table, not in `CPU` for default builds.
- In `runMachineBudget`, `RunWithChain`, and `runCachedDualBudget`, capture the pre-execution PC/SATP/privilege mode before executing the instruction.
- In `runMachineBudget`, call a breadcrumb after-attempt hook after `cpu.Step()` and after `cpu.riscvInstrBegun++`.
- The after-attempt hook observes the post-instruction `cpu.priv` and `cpu.pc`, applies pending guest-control commands, records the attempted PC if active, and raises the trip IRQ after the configured trip instruction has actually executed.
- If armed while still in `PrivUser`, do not activate immediately. First require seeing `cpu.priv != PrivUser`.
- Once the hook later sees `cpu.priv == PrivUser`, activate the tracer but do not record the just-finished privileged instruction. The next interpreter iteration records the first user instruction.

This activation mode is mainly for `cmd/emul` / `cmd/emu -bios`. It should be the default activation path when `-breadcrumb` is used with guest Linux. Immediate `start/resume` remains useful for hand-written tests and special diagnostics, but normal program tracing should wait for the guest MMIO arm command and the following kernel-to-user transition.

## Checkpoint Format

Start with a stable, line-oriented text format:

```text
# riscv-breadcrumb-v1 kind=pc-hash digest=blake3-256 endian=little scope=user guest_control=true filter_address_space=true
seq=1000 pc=0x00000000000123f0 hash=0123...
```

Reasons to prefer text first:

- Easy to diff.
- Easy to grep.
- Easy to compare with small scripts.
- Fast enough because only checkpoints are textual; per-instruction hashing remains binary.

The first line should identify the format and hash scheme. Each checkpoint line should include only deterministic comparison fields:

- `seq`: number of PCs actually hashed in this epoch.
- `pc`: the pre-execution PC just hashed.
- `hash`: hex-encoded BLAKE3-256 digest of all hashed PCs through `seq`.

Event lines begin with `#` and may include nondeterministic context such as absolute attempted-instruction count, retired count, epoch, privilege scope, `satp`, target PID, and trip metadata. Comparison tools should ignore event lines unless they are explicitly debugging activation/trip behavior.

## Instrumentation Points

### Uncached Interpreter

In `RunWithChain`, record `cpu.pc` before `cpu.step()`:

```go
attemptPC := cpu.pc
attemptSATP := cpu.satp
attemptPriv := cpu.priv
if breadcrumbEnabled {
	handled, berr := breadcrumbBeforeAttempt(cpu, cpu.riscvInstrBegun+1, cpu.riscvInstrRetired, attemptPC, attemptSATP, attemptPriv)
	if berr != nil {
		return berr
	}
	if handled {
		cpu.riscvInstrBegun++
		continue
	}
}
err := cpu.step()
cpu.riscvInstrBegun++
if breadcrumbEnabled {
	if berr := breadcrumbAfterAttempt(cpu, cpu.riscvInstrBegun, cpu.riscvInstrRetired, attemptPC, attemptSATP, attemptPriv); berr != nil {
		return berr
	}
}
```

The before-attempt hook is currently idle; it exists so the default build keeps the same compile-time shape if a future pre-execution diagnostic needs it. The after-attempt hook captures ordinary attempted instructions, including a final ECALL/EBREAK/faulting instruction, and trips only after the configured instruction has executed.

### Decoder-Cached Interpreter

In `runCachedDualBudget`, `pc`, `instrBegun`, and `instrRetired` are held in locals and flushed to the CPU periodically. The breadcrumb hook must use the logical post-attempt counters, not only the stale CPU fields.

At the bottom of the inner dispatch loop, before advancing to the next slot, compute:

```go
attemptPC := pcBeforeThisInstruction
attemptSATP := satpBeforeThisInstruction
attemptPriv := privBeforeThisInstruction
logicalAttempt := cpu.riscvInstrBegun + instrBegun + 1
logicalRetired := cpu.riscvInstrRetired + instrRetired
if err == nil && inlineRetired {
	logicalRetired++
}
```

The before-attempt hook is kept in the cached loop as a no-op compatibility hook:

```go
if breadcrumbEnabled {
	handled, berr := breadcrumbBeforeAttempt(cpu, logicalAttempt, logicalRetired, attemptPC, attemptSATP, attemptPriv)
	if berr != nil {
		// return the hook error after flushing logical counters
	}
	if handled {
		// count the attempt, reload pc from cpu.pc, and continue
	}
}
```

After dispatching a normal instruction, call:

```go
if breadcrumbEnabled {
	if berr := breadcrumbAfterAttempt(cpu, logicalAttempt, logicalRetired, attemptPC, attemptSATP, attemptPriv); berr != nil {
		cpu.riscvInstrBegun += instrBegun
		cpu.riscvInstrRetired += instrRetired
		cpu.pc = pc
		return RunBudgetContinue, RunBudgetLimitNone, berr
	}
}
```

To make this work, save the pre-instruction PC at the start of each inner-loop iteration:

```go
var attemptPC uint64
var attemptSATP uint64
var attemptPriv PrivilegeMode
if breadcrumbEnabled {
	attemptPC = pc
	attemptSATP = cpu.satp
	attemptPriv = cpu.priv
}
```

This handles inline cached ops, `opDelegate`, and `slowStep` paths consistently. For `slowStep`, the local `pc` may be updated by `cpu.step()`, but the breadcrumb should still hash the original attempted PC.

Because `breadcrumbEnabled` is a build-tagged constant, the default build should not retain the temporary `attemptPC` variable or the record block in generated code.

### BIOS / Machine-Mode Interpreter

In `runMachineBudget`, record `cpu.pc` before `cpu.Step()` exactly like `RunWithChain`:

```go
attemptPC := cpu.pc
attemptSATP := cpu.satp
attemptPriv := cpu.priv
if breadcrumbEnabled {
	handled, berr := breadcrumbBeforeAttempt(cpu, cpu.riscvInstrBegun+1, cpu.riscvInstrRetired, attemptPC, attemptSATP, attemptPriv)
	if berr != nil {
		return RunBudgetContinue, berr
	}
	if handled {
		cpu.riscvInstrBegun++
		continue
	}
}
err := cpu.Step()
cpu.riscvInstrBegun++
if breadcrumbEnabled {
	if berr := breadcrumbAfterAttempt(cpu, cpu.riscvInstrBegun, cpu.riscvInstrRetired, attemptPC, attemptSATP, attemptPriv); berr != nil {
		return RunBudgetContinue, berr
	}
}
```

`breadcrumbAfterAttempt` should both apply any pending guest-control command and record the PC if the tracer is active and the privilege filter accepts `attemptPriv`. Guest-control commands set by the MMIO device during `cpu.Step()` should take effect after the current instruction attempt, so `start` begins at the next guest instruction.

This hook is required for `cmd/emul` because that path uses `RunBiosMachineBudget`, not `runCachedDualBudget`.

## Error Handling

In `-tags breadcrumb` builds, if the breadcrumb writer fails, return that error from the run loop. A trace that silently stops is worse than a failed run.

In default builds, breadcrumb helpers compile away and cannot fail.

Potential polish:

- Wrap errors as `fmt.Errorf("breadcrumb: %w", err)`.
- Flush on normal returns from top-level helpers, or provide an explicit `defer cpu.BreadcrumbTracer().Flush()` in callers/tests.

## CLI / Config

The `cmd/emu` flags are:

```text
-breadcrumb path
-breadcrumb-interval 1000
-breadcrumb-start 0
-breadcrumb-stop 0
-breadcrumb-after-at 0
-breadcrumb-after-interval 0
-breadcrumb-privileged=false
```

Semantics:

- `-breadcrumb`: configures breadcrumb tracing and creates/truncates this output file.
- `-breadcrumb-interval`: checkpoint cadence; default `1000`.
- `-breadcrumb-start`: hash PCs only at or after this breadcrumb sequence count; default `0`.
- `-breadcrumb-stop`: stop hashing, flush/sync the host trace, and raise the breadcrumb trip IRQ at this breadcrumb sequence count; default `0` means no stop.
- `-breadcrumb-after-at`: switch cadence after this breadcrumb sequence count.
- `-breadcrumb-after-interval`: finer cadence after `-breadcrumb-after-at`.
- `-breadcrumb-privileged`: include S-mode/M-mode PCs; default false.

For `emu -run`, there is no guest Linux kernel and no guest MMIO control device. The tracer attaches after ELF stack setup and starts hashing immediately when `RunWithJea9LinuxInterp` begins executing the guest program. This is the preferred first debugging mode when the goal is to avoid real guest-kernel timer ticks and scheduler work.

For `emu -bios` guest Linux, these flags set up the tracer parameters ahead of time and expose the MMIO control page. The tracer remains inactive until the guest writes the MMIO control register. For launcher-driven program tracing, the guest should use `arm-next-address-space-reset`; only after the next kernel-to-user transition into a different address space do `-breadcrumb-interval`, `-breadcrumb-start`, `-breadcrumb-stop`, and the hash stream begin to matter. For a process that can call the MMIO helper itself at the exact point of interest, use `start/resume`; the first hashed PC is the next user instruction in that same address space.

If CLI flags are present in a non-breadcrumb build, fail fast with an explicit error such as:

```text
breadcrumb tracing requires rebuilding with -tags breadcrumb
```

Do not silently ignore a requested breadcrumb file.

## Comparing Two Runs

Workflow:

1. Run the same guest twice with the same checkpoint interval.
2. Compare files line by line.
3. The first mismatching checkpoint gives a hashed-PC sequence interval:
   `(previous_matching_seq, first_mismatching_seq]`.
4. Re-run with `StartAt` near the previous matching sequence number and a smaller interval.
5. Repeat until the interval is small enough.
6. For final diagnosis, enable an optional exact trace mode over only the suspect window.

For exact final diagnosis, add a separate mode later that writes:

```text
seq=123456 pc=0x...
```

for every accepted attempted instruction in a small bounded window. In the default user-only scope, that means every user-mode attempted instruction. Do not make full-run exact logging the default.

## What PC-Only Hashing Catches

PC-only breadcrumbs catch the first control-flow divergence. They do not immediately catch silent state divergence if both executions continue down the same PCs.

That is still valuable because:

- It is cheap enough to leave on for long runs.
- Most state bugs eventually affect a branch, syscall, memory address, trap, or exit path.
- Once a control-flow interval is known, heavier tracing can be enabled only there.

Possible later extensions:

- Include syscall number and args at ECALL checkpoints.
- Include trap cause and `tval` when an instruction faults.
- Include a checkpoint-time CPU state digest every N checkpoints.
- Include selected memory write breadcrumbs: `(pc, addr, width, value)`.

Keep these as opt-in modes. The baseline should remain PC-only and low overhead.

## Tests

Add focused tests before wiring CLI flags.

Suggested test cases:

- `TestBreadcrumbRunWithChain`: a tiny hand-written program with two NOPs then ECALL, interval 1, verifies three checkpoint lines and the expected PCs.
- `TestBreadcrumbRunCached`: same tiny program through `RunDefaultBudget` or `RunDefaultDualBudget`, interval 1, verifies the same PCs and hash sequence as `RunWithChain`.
- `TestBreadcrumbRunMachineBudget`: same tiny program through `RunMachineBudget`, interval 1, verifies the same PC hash sequence.
- `TestBreadcrumbGuestControlMMIO`: BIOS MMIO control write sets a pending activation command; the first hashed PC is the instruction after the control write.
- `TestBreadcrumbArmNextUser`: a supervisor-mode sequence arms breadcrumbs, executes an `sret` to a user PC, and verifies the first hashed PC is the user instruction, not the MMIO store or `sret`.
- `TestBreadcrumbArmNextUserRequiresPrivilegedInterlude`: arming while already in `PrivUser` does not activate on the next user instruction until the CPU first leaves user mode and returns.
- `TestBreadcrumbGuestControlCapturesTargetAddressSpace`: guest-control mode captures target `satp`, records guest-provided `target_pid`, ignores returns to the arming address space, and refuses to hash a different user process's PCs.
- `TestBreadcrumbHandBuiltLinuxSleepTraceDeterministic`: under the hand-built guest Linux fixture, build a tiny self-arming RISC-V process, run it twice while a background user process is available to pollute the stream, include a real guest `nanosleep`, and verify the same target-only hash trace is produced.
- `TestBreadcrumbUserModeOnly`: when `IncludePrivileged` is false, user-mode PCs are hashed and S/M-mode PCs are ignored.
- `TestBreadcrumbIncludePrivileged`: when `IncludePrivileged` is true, privileged PCs enter the same hash stream.
- `TestBreadcrumbInterval`: interval 2 emits only sequence numbers 2, 4, ...
- `TestBreadcrumbStartStop`: verifies hashing/checkpointing can focus on a window.
- `TestBreadcrumbAfterInterval`: coarse interval switches to finer interval after a configured sequence count.
- `TestBreadcrumbFaultingInstruction`: illegal instruction or EBREAK contributes its attempted PC before the run returns the fault.

Most important invariants:

The same guest PC stream must produce identical checkpoint hashes through the uncached interpreter and decoder-cached interpreter.

For guest Linux, changes in privileged-mode interrupt/scheduler work must not affect the breadcrumb hash or checkpoint cadence when `IncludePrivileged` is false.

## Implementation Order

1. Add `breadcrumb_off.go` with `//go:build !breadcrumb`, `breadcrumbEnabled = false`, and no-op internal helpers.
2. Add `breadcrumb_on.go` with `//go:build breadcrumb`, `BreadcrumbTracer`, config validation, PC hashing, checkpoint formatting, side-table attachment, and flush support.
3. Add breadcrumb MMIO stubs/off implementation for default builds and real MMIO/FDT support for `-tags breadcrumb`.
4. Hook `RunWithChain` behind `if breadcrumbEnabled`.
5. Hook `runCachedDualBudget` behind `if breadcrumbEnabled`.
6. Hook `runMachineBudget` behind `if breadcrumbEnabled` for `cmd/emul` and `cmd/emu -bios`.
7. Add breadcrumb tests that run only with `//go:build breadcrumb`.
8. Add a default-build test or benchmark guard that confirms breadcrumb hooks do not affect normal interpreter behavior/performance.
9. Add optional `EmuConfig` and `cmd/emul` flags once the core behavior is stable.
10. In non-breadcrumb CLI builds, reject breadcrumb flags explicitly rather than ignoring them.

## Open Questions

- Should checkpoints use absolute instruction attempts, retired instructions, or hashed sequence numbers? Recommendation: checkpoint by hashed sequence number, include absolute attempts and retired count as metadata.
- Should `StartAt` mean "begin hashing at this sequence number" or "hash from the beginning but begin writing at this sequence number"? Recommendation: use two separate concepts if needed later. For now, `StartAt` should begin hashing at that sequence number, because it supports focused re-runs.
- Should a dynamic interval switch happen before or after checkpointing at `AfterAt`? Recommendation: apply the new interval after recording attempt `AfterAt`, so the boundary is easy to reason about.
- Should the trace include the initial entry PC before any instruction attempt? Recommendation: no. The first attempted instruction already records it at `attempt=1`.
- Should the default build expose the public breadcrumb API as no-op stubs? Recommendation: avoid this unless an external caller needs source compatibility. Silent no-op tracing is dangerous; explicit build-tag failure is better.
- Is a compile-time constant guard enough to avoid perturbing the cached interpreter? Recommendation: start there, then confirm with objdump and the cached interpreter benchmark. If the giant `runCached` function still regresses, consider a generated/tagged duplicate for the breadcrumb build, keeping the default source path completely unchanged.
- Should guest `pause` include or exclude the pause MMIO store instruction? Recommendation: start commands take effect after the current instruction, and pause commands also take effect after the current instruction. This gives simple instruction-boundary semantics and avoids changing CPU state mid-instruction.
- Should `-breadcrumb` start hashing immediately? Recommendation: no for guest Linux. It should only configure the inactive tracer and expose the MMIO device. Hashing starts only after guest activation, either `start/resume` from inside the target process or `arm-next-address-space-reset` from a launcher that immediately `execve`s the target.
