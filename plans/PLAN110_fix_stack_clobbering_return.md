# Plan: Fix JIT CALL Panic Stack Unwinding Crash

## Context

`TestJIT_CoreMark_ChainReference` crashes with `runtime: g 7: unexpected return pc ... called from 0x157f32b86` — a JIT mmap address on the Go stack. This happens when:

1. JIT code calls the syscall dispatcher via x86 `CALL RAX` (lower_amd64_abjit.go:428)
2. The dispatcher calls `LinuxExit` which `panic(&ExitError{...})`
3. Go's stack unwinder walks the CALL frame and finds the return PC is `0x157f32b86` — JIT mmap memory, not Go code
4. Go runtime panics: "unknown caller pc"

R14 (`g`) is NOT clobbered — it's correctly excluded from the pool and never touched by JIT code. The issue is purely the JIT return address on the Go stack.

This is NOT a new bug — it exists whenever JIT code does `CALL` to Go and Go panics. But it's now exposed because CoreMark's syscall (exit) always panics.

## Root Cause

The `abjitSyscall` lowerer emits `CALL RAX` to invoke the syscall dispatcher. This pushes a return address (pointing into JIT mmap) onto RSP. If the Go function panics, the stack unwinder can't trace through the JIT frame.

## Fix: Use the gocall trampoline instead of direct CALL

The ABJIT trampoline already has a `gocall` mechanism (trampoline_amd64.s:27-29):
```asm
gocall:
    CALL R10
    JMP (SP)
```

This is designed for exactly this scenario: JIT code writes the resume address to `[SP+0]`, loads the Go function address into R10, then JMPs to the `gocall` label. The `CALL R10` executes from within the Go trampoline frame (which has a valid Go return PC). If the callee panics, the unwinder sees the trampoline's return PC — valid Go code.

### Change `abjitSyscall` (lower_amd64_abjit.go ~line 424-430)

Replace:
```go
// CALL dispatcher.
lc.loadImm(int64(sym.Addr), stgA)
p := lc.c.NewProg()
p.As = obj.ACALL
p.To.Type = obj.TYPE_REG
p.To.Reg = stgA
lc.c.Append(p)
```

With gocall sequence:
```go
// Write resume address to [SP+0] (trampoline reads it after CALL returns).
// The resume address is the next instruction after the JMP to gocall.
resumeNop := lc.c.NewProg()  // placeholder — will be placed after JMP
lc.emitMR(x86.ALEAQ, resumeNop_addr, goasm.REG_AMD64_SP, 0)  // [SP+0] = &resume

// Load dispatcher address into R10.
lc.loadImm(int64(sym.Addr), goasm.REG_AMD64_R10)

// JMP to gocall label in trampoline.
lc.loadImm(int64(abjit.GocallAddr()), stgA)
jp := lc.c.NewProg()
jp.As = obj.AJMP
jp.To.Type = obj.TYPE_REG
jp.To.Reg = stgA
lc.c.Append(jp)

// Resume point (JMP (SP) in gocall lands here):
lc.c.Append(resumeNop)
```

**Problem**: We need the address of `resumeNop` at emit time, but it's not known until assembly. We need a forward reference.

**Simpler approach**: Use the existing `gocall` mechanism as designed — the JIT code stores its own resume PC at `[SP+0]`. After `JMP gocall`, the trampoline does `CALL R10; JMP (SP)` which jumps back to the resume point. Since the CALL happens within the trampoline (Go code), its return PC is valid.

The challenge is computing the resume address. The lowerer emits Progs, not final bytes. We need a relocation. The goasm assembler supports branch target resolution — we can use a label-like mechanism.

**Actually simplest**: Since we can't easily compute the absolute resume address at lowering time, emit the resume address as a MOVABS with a sentinel that gets backpatched after assembly, similar to how chain exits work.

**Or**: Just accept the current CALL approach but catch the panic at a higher level. The `runJITBenchGuestWith` already has `defer/recover` that catches `ExitError`. The issue is that Go's unwinder crashes BEFORE the recover fires.

**Real fix needed**: The gocall trampoline is the correct solution. Let me check how it's currently used.

### How gocall works

1. JIT code stores resume PC at `[RSP+0]` (the trampoline's frame slot 0)
2. JIT loads function address into R10
3. JIT does `JMP gocallAddr` (absolute address of `CALL R10` in trampoline)
4. Trampoline: `CALL R10` — return address is inside the trampoline (Go code) ✓
5. Trampoline: `JMP (SP)` — reads `[RSP+0]` and jumps back to JIT resume point

If the Go function panics in step 4, the return PC on the stack points to the trampoline's `JMP (SP)` instruction — valid Go code. The unwinder is happy.

### Same fix for `abjitCall` (lower_amd64_abjit.go ~line 692-697)

Same pattern: replace direct `CALL RAX` with gocall trampoline.

## Also: Save/Restore R15 (IC) across the Go call

The gocall mechanism doesn't change the R15 issue — Go can still clobber R15. Add SpillIC before the JMP-to-gocall and LoadIC at the resume point (guarded by `UseICR15` from RegPolicy).

## Files

| File | Change |
|------|--------|
| `lower_amd64_abjit.go` | `abjitSyscall`: replace CALL with gocall JMP sequence |
| `lower_amd64_abjit.go` | `abjitCall`: same |
| `lower_amd64_abjit.go` | Add R15 spill/load around gocall (guarded by policy flag) |

## Verification

```bash
cd ~/ris/bench && go test -v -run TestJIT_CoreMark_ChainReference -count=1
cd ~/ris && go test -v -run TestR15IC_MatchesInterpreter -count=1 .
cd ~/ris && go test -count=1 .
```
