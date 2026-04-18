# Minimal M-Mode Trap Support for riscv-tests

## Context

The riscv-tests binaries install their own M-mode trap handler at address 0x0000. When a test passes or fails, it calls ECALL, which on real hardware traps through `mtvec` to the handler, which writes the result to the `tohost` memory address. Our emulator currently intercepts ECALL at the Go level (`ErrEcall` → NoteChain), so the binary's trap handler never runs and `tohost` is never written.

We already implemented tohost polling (previous plan, done). Now we need the trap handler to actually execute so it can write to tohost.

**This won't fix the JIT block-progression hang at 0x3ce** (that's a separate issue), but it's the correct foundation and may fix tests where ECALL IS reached but the exit path doesn't work properly.

## What riscv-tests Do

The `reset_vector` (at 0x48) sets up M-mode CSRs:
```asm
csrw mtvec, t0        # multiple times, for "skip on trap" pattern
csrwi mstatus, 0x0    # clear machine status
csrw mepc, t0         # set return address = test code entry
mret                   # jump to test code via mepc
```

The trap handler (at 0x0000):
```asm
csrr t5, mcause       # what caused the trap?
beq  t5, 8, write_tohost   # ECALL? → write result and halt
beq  t5, 9, write_tohost
beq  t5, 11, write_tohost
...                    # other exceptions: skip via mtvec table
```

Pass/fail both call ECALL → trap handler → `write_tohost` → stores to `tohost` → our polling detects it.

## CSRs Needed

| CSR | Address | Purpose |
|-----|---------|---------|
| mtvec | 0x305 | Trap handler base address |
| mepc | 0x341 | Exception PC (saved on trap, restored by MRET) |
| mcause | 0x342 | Cause code (8=user ECALL, 2=illegal insn, etc.) |
| mstatus | 0x300 | Machine status bits (just store/return, no full semantics) |
| mtval | 0x343 | Trap value (faulting address, etc. — set to 0 for ECALL) |

Other CSRs written by reset_vector (satp, pmpaddr0, pmpcfg0, mie, medeleg, mideleg, stvec, mnstatus) can be silently accepted — our `writeCSR` already ignores unknown CSRs, which is the correct behavior (the "skip on trap" pattern never triggers because we don't fault on unknown CSRs).

## Implementation Plan

### Step 1: Add M-mode CSR Storage (`cpu.go`)

Add fields to `CPU` struct:

```go
// M-mode trap CSRs
mtvec   uint64 // 0x305: trap vector base
mepc    uint64 // 0x341: exception PC
mcause  uint64 // 0x342: trap cause
mstatus uint64 // 0x300: machine status
mtval   uint64 // 0x343: trap value
```

### Step 2: Add CSR Read/Write Cases (`cpu.go`)

In `readCSR`:
```go
case 0x300: return c.mstatus
case 0x305: return c.mtvec
case 0x341: return c.mepc
case 0x342: return c.mcause
case 0x343: return c.mtval
```

In `writeCSR`, add the same cases writing to the fields. Also add sink cases for the CSRs that reset_vector writes but we don't need to track (so they don't show up as "unknown"):
```go
case 0x300: c.mstatus = val
case 0x302: // medeleg — accept, ignore
case 0x303: // mideleg — accept, ignore
case 0x304: // mie — accept, ignore
case 0x305: c.mtvec = val
case 0x341: c.mepc = val
case 0x342: c.mcause = val
case 0x343: c.mtval = val
// Supervisor CSRs that reset_vector touches:
case 0x105: // stvec — accept, ignore
case 0x180: // satp — accept, ignore
// PMP CSRs:
case 0x3A0: // pmpcfg0 — accept, ignore
case 0x3B0: // pmpaddr0 — accept, ignore
// Other:
case 0x744: // mnstatus (non-standard) — accept, ignore
```

### Step 3: Change ECALL to Trap Through mtvec (`cpu.go`)

Replace the current ECALL handling:

```go
case insn == 0x00000073: // ECALL
    if c.mtvec != 0 {
        c.mepc = c.pc       // save PC of ECALL instruction
        c.mcause = 8        // user ECALL (CauseEcallU)
        c.mtval = 0
        c.pc = c.mtvec      // jump to trap handler
        return nil           // no error — trap handled internally
    }
    c.pc = nextPC
    return ErrEcall          // legacy path when no trap handler installed
```

When `mtvec != 0`: the ECALL doesn't produce an error. The CPU simply jumps to `mtvec` and continues executing the guest's trap handler as normal code. The trap handler writes to `tohost`, and our polling detects it.

When `mtvec == 0`: existing behavior preserved (ErrEcall → NoteChain → OS personality).

### Step 4: Fix MRET to Jump to mepc (`cpu.go`)

Currently MRET is a no-op that advances PC. Fix it:

```go
case insn == 0x30200073: // MRET
    c.pc = c.mepc    // jump to saved exception PC
    return nil
```

### Step 5: Handle ECALL Trap in JIT Dispatch (`jit.go`)

In `RunJIT`, the `jitEcall` handler currently delivers to NoteChain. Add mtvec check first:

```go
case jitEcall:
    if cpu.mtvec != 0 {
        cpu.mepc = cpu.pc   // save ECALL PC
        cpu.mcause = 8
        cpu.mtval = 0
        cpu.pc = cpu.mtvec  // redirect to trap handler
        continue            // dispatch loop runs trap handler
    }
    // Legacy path: deliver to NoteChain
    n := noteFromStepErr(ErrEcall, cpu.pc)
    switch cpu.Notes.Deliver(cpu, n) {
    ...
    }
```

Similarly update `StepBlock`'s jitEcall handler.

### Step 6: Handle MRET in JIT Emitter (`jit_emit_ir.go`)

MRET (0x30200073) is currently caught by the `default` case in SYSTEM handling, which sets `e.terminated = true` (ends block before the instruction). This means MRET falls to the interpreter, which is fine. **No change needed** — the interpreter handles MRET correctly after Step 4.

## Files to Modify

| File | Change |
|------|--------|
| `cpu.go` | Add 5 CSR fields, update `readCSR`/`writeCSR`, fix ECALL and MRET |
| `jit.go` | Add mtvec check in `RunJIT` and `StepBlock` jitEcall handlers |

## What Does NOT Change

- `note.go` — no changes (tohost polling already added)
- `os.go` — no changes (tohostExitCode already added)
- `riscv_test.go` — no changes (tohost watch already wired up)
- `jit_emit_ir.go` — no changes (MRET already terminates block)
- NoteChain / OS personality — still works when mtvec == 0

## Verification

```bash
# Basic interpreter tests — should still pass
go test -count=1 -run 'TestRISCVTests_UI$' -timeout 60s -v .

# Check that tohost is actually written now (add logging if needed)
go test -count=1 -run 'TestDispatchTrace_sraw' -timeout 30s -v .

# JIT tests — ECALL now traps through handler, writes tohost, polling catches it
# (tests that reach ECALL should now exit cleanly via tohost)
go test -count=1 -run 'TestRISCVTests_UI_JIT/^add$' -timeout 10s -v .

# Full interpreter suite
go test -count=1 -run 'TestRISCVTests_UI$' -timeout 60s -v .

# ELF symbol tests
go test -count=1 -run 'TestFindSymbolAddr' -timeout 10s -v .
```

## Design Notes

- **Backward compatible**: When `mtvec == 0` (default), all behavior is unchanged. OS personality, NoteChain, LinuxExit — all work as before.
- **Minimal scope**: We implement just enough M-mode to run riscv-tests. No interrupts, no privilege levels, no delegation. Just mtvec/mepc/mcause/mstatus/mtval + trap vectoring for ECALL + MRET.
- **Unknown CSRs**: Already silently ignored by `readCSR` (returns 0) and `writeCSR` (no-op). This makes the reset_vector's "skip on trap" pattern work correctly — the risky CSR writes succeed silently, no trap occurs, and the mtvec redirect is never triggered.
- **ECALL PC**: `mepc = c.pc` (the ECALL instruction itself, not PC+4). This matches RISC-V spec. The trap handler is responsible for advancing mepc if it wants to return past the ECALL.
