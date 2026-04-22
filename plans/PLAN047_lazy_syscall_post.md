# Phase 5 — Inline ECALL to close the 33 ns libriscv gap

## Context

`make hello` today:

```
  libriscv              21.0 ns/call   Hello, libriscv!     (dispatch only)
  libriscv real write  656.0 ns/call   Hello, libriscv!     (+ kernel)
  GoCPU interpreter     79.6 ns/call   Hello, Go CPU!       (dispatch only)
  GoCPU direct syscall 709.5 ns/call   Hello, Go CPU!       (+ kernel)
  GoCPU direct callback 54.4 ns/call   Hello, Go CPU!       (dispatch only)
```

Kernel-exclusive gap: **33 ns**. Cause: every ECALL terminates the
JIT block, forcing a round-trip per loop iteration (RET → `jitcall.Call`
trampoline copy-out → `RunJIT` lookupBlock → `jitcall.Call` re-entry
→ block prologue).

Goal: drive `GoCPU direct callback` to ≤ 21 ns/call.

## What libriscv does (`xendor/libriscv/lib/libriscv/tr_emit.cpp:720`)

```c
STORE_REGS_<func>();                                   // spill cached regs to cpu->r[]
max_ic = api.system_call(cpu, pc, ic, max_ic, a7);     // inline C call — no block exit
ic = INS_COUNTER(cpu);
if (!max_ic) return (ReturnValues){ic, MAX_COUNTER(cpu)};   // bail only if syscall exited
// …fall through; block keeps executing…
```

Post-call reload is lazy: `from_reg(n)` re-reads `cpu->r[n]` on the
**next use** (`tr_emit.cpp:192-207`). A guest reg that's written by the
syscall but never read again pays zero reload cost. The C compiler
handles caller-saved clobbers via ordinary C call semantics.

## Our analog (allocator-agnostic)

This design works with both `ir/regalloc_fixed.go` (fixed static, one
host reg per VReg for the whole block) and `ir/regalloc.go` (ELS, with
interval splitting) — because the reload is emitted as a standard
`IRLoad` op, which both allocators handle as a new def (creating a
fresh interval post-ECALL).

- **Fixed static**: every used VReg is pinned to one host reg for the
  block. Because RBX/RBP/R12-R15 are pinned to sret/IC/xBase/fBase/
  memBase/memMask, every `AllocReg` VReg lives in a caller-saved host
  reg — guaranteed clobbered by the dispatcher CALL. The IRLoad after
  the CALL reassigns a fresh value to the same host reg, so subsequent
  uses read correct data.
- **ELS**: IRSyscall stays modeled as "no uses, no defs"
  (`ir/regalloc.go:299,314`), but the IRLoad we emit at first
  post-ECALL use creates a new def point, starting a new live
  interval. The pre-ECALL interval naturally ends at the last use
  before the call (dirty VRegs end at the WriteBack store; clean
  VRegs end at their last read). If a pre-ECALL VReg has no
  post-ECALL use, no reload is emitted — the caller-saved clobber
  harms nothing because the interval already ended.

Temps (`e.Tmp()`) are instruction-local in the current emitter pattern
and don't span ECALL under either allocator, so we only need to worry
about X1..X31 and F0..F31 (the guest register VRegs exposed via
`XReg`/`FRegV`).

To be lazy like libriscv: after ECALL, mark every touched X/F VReg
`needsReload`. On the next reference through `XReg(i)` / `FRegV(i)`,
emit an `IRLoad` from `x[i]` / `f[i]` *once*, clear the flag, and
return the VReg. Subsequent uses of the same VReg in the same
post-ECALL window reuse the reloaded host reg.

Guest regs that are touched pre-ECALL but never read post-ECALL pay
zero reload cost — matching libriscv exactly. For hellobench's hot
loop, this means we reload only the loop counter (if used after the
branch-target wraps around), not a0/a1/a2/a7 which get re-materialized
by `li` at the top of the next iteration.

## Design

Four small edits.

**1. `jit_decode.go:classifyFlow`** — split SYSTEM by funct7. Today
opcode 0x73 unconditionally returns `flowTerm`. Change ECALL only:

```go
case 0x73:
    if (insn >> 20) == 0 { // ECALL (funct7=0, rs2=0)
        return flowSeq, 0, 4
    }
    return flowTerm, 0, 4   // EBREAK, CSR*
```

**2. `jit_emit_ir.go` at `case 0x00000073`** — terminate only when no
fast dispatcher is installed:

```go
case 0x00000073: // ECALL
    e.advancePC(4)
    e.emitSyscall(e.pc, currentSyscallDispatcherAddr())
    if currentSyscallDispatcherAddr() == 0 {
        e.terminated = true
    }
```

**3. `ir/emit.go` — lazy reload via `touched[]` + `needsReload[]`.**

Add to `Emitter`:

```go
touched     [64]bool // VRegs ever referenced in this block
needsReload [64]bool // VRegs currently stale (reload on next use)
```

Hook `XReg(i)` / `FRegV(i)` — the only entry points that return a
guest-register VReg:

```go
func (e *Emitter) XReg(i uint32) VReg {
    if i > 31 { panic("ir.Emitter.XReg: index > 31") }
    vr := VReg(i)
    if i != 0 && e.needsReload[i] {
        e.Load(vr, e.xBase, int64(i)*8, I64, false)
        e.needsReload[i] = false
    }
    e.touched[i] = true
    return vr
}

func (e *Emitter) FRegV(i uint32) VReg {
    if i > 31 { panic("ir.Emitter.FRegV: index > 31") }
    vr := VReg(32 + i)
    if e.needsReload[32+i] {
        e.Load(vr, e.fBase, int64(i)*8, F64, false)
        e.needsReload[32+i] = false
    }
    e.touched[32+i] = true
    return vr
}
```

In `Syscall`:

```go
func (e *Emitter) Syscall(resumePC uint64, dispatcherAddr uintptr) {
    // (CTab dispatcher lookup — unchanged) …
    e.emit(IRInstr{Op: IRSyscall, Imm: int64(resumePC), Imm2: int64(idx)})

    // Mark touched guest regs as needing reload; host regs are clobbered
    // by the dispatcher CALL. Actual IRLoad emits at next XReg/FRegV.
    for i := 0; i < 64; i++ {
        if e.touched[i] {
            e.needsReload[i] = true
        }
    }
    // WriteBackAll ran before Syscall; dirty bits already covered.
    // Clear dirty — nothing is owed until further writes.
    for i := range e.dirty {
        e.dirty[i] = false
    }
}
```

Caller contract unchanged: `emitSyscall` in `jit_emit_ir.go` still
pre-emits `e.irEm.WriteBackAll()`.

**4. `ir/lower_amd64.go:lowerSyscall`** — remove the final
`emitEpilogue()`; add inline status check + cold fallback stub:

```asm
; existing: MOV R12/R14/R15 → RDI/RSI/RDX, MOVABS dispatcher, CALL
TESTQ  RAX, RAX
JZ     L_continue
; cold fallback (dispatcher returned 1 = unknown syscall):
MOVABS $resumePC, R10
MOVQ   R10, 0(RBX)         ; sret.PC
MOVQ   RBP, 8(RBX)         ; sret.IC
MOVQ   $1, 16(RBX)         ; jitEcall
MOVQ   $0, 24(RBX)         ; sret.FaultAddr
[ADDQ  frameSize, RSP]
RET
L_continue:
; fall through to next IR op (Loads emitted lazily on next guest-reg use)
```

Mirror in `ir/lower_amd64_v2.go` (replace the current `v2Ret` fallback).

## Rollback

Gate behind `syscalls.InlineEcallEnabled` in `jit_syscall.go`; default
`true` post-validation, flip to `false` to restore today's behavior.

## Files

| File | Edit |
|---|---|
| `jit_decode.go` | `classifyFlow`: ECALL → flowSeq |
| `jit_emit_ir.go` | conditional terminate at ECALL |
| `jit_syscall.go` | `InlineEcallEnabled` toggle |
| `ir/emit.go` | `touched[]`/`needsReload[]` + hook XReg/FRegV + Syscall |
| `ir/lower_amd64.go` | `lowerSyscall`: TESTQ+JZ+fallback stub |
| `ir/lower_amd64_v2.go` | `lowerSyscallV2`: same as V1 |

## Verification

- `go test ./internal/syscalls/` — dispatcher contract unchanged.
- `go test -run 'TestSRL_RealBlock' ./ir/` — V1/V2 parity.
- `go test -run TestHello .` — hello correctness.
- `go test -run 'TestJIT_' .` — broad regression.
- `go test ./fuzzoracle/...` (after `make bench-setup`) — libriscv oracle.
- `make hello` — all 5 lines pass byte-for-byte verification; expect
  `GoCPU direct callback` ≤ 21 ns (from 54 ns) and `GoCPU direct
  syscall` drops ~33 ns (from 710 to ~677).
- Artificial regression: break dispatcher (return wrong count) →
  `make hello` fails loudly.
