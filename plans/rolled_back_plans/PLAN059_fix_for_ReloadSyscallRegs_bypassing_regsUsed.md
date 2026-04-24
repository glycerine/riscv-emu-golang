# Fix: Lockstep x[11] divergence — ReloadSyscallRegs bypasses regsUsed

## Context

`TestRISCVTests_Lockstep_UI/beq` fails at block 72 (entry pc=0x138, exit pc=0x4, IC=255):
```
x[11] mismatch: jit=0x2ad3fa768270 interp=0x0
```

The JIT writes a stale host pointer (the fcsr pointer from the SysV ABI) into guest register x[11]. Pre-existing bug affecting all branch-test lockstep comparisons.

## Root Cause

`ir.Emitter.ReloadSyscallRegs()` (`ir/highlevel.go:182-187`) directly calls `e.Load(vr, ...)` and `e.MarkDirty(vr)` for VRegs 10 and 11. This bypasses `emitter.xreg()`/`emitter.xregDst()` in `jit_emit_ir.go` — the ONLY place where `regsUsed` bits are set.

Result: VReg(11) appears in the IR (allocator assigns it CX), but no `IRLoad` is prepended at block entry (bit 11 not in `regsUsed`). CX retains the stale fcsr pointer from the SysV ABI. The syscall writeback `store [xBase+88] = x11` stores this stale CX value into x[11].

## Fix

Move the syscall reload logic from `ir.Emitter` methods up into `emitter` methods in `jit_emit_ir.go` so they go through `xreg`/`xregDst` which sets `regsUsed`.

### 1. `jit_emit_ir.go` — Replace `ir.Emitter` calls with `emitter`-level equivalents

In `emitSyscall` (line 309), replace:

```go
e.irEm.WriteBackAll()
e.irEm.ClearDirtySyscallRegs()
e.irEm.Syscall(resumePC, dispatcherAddr)
e.irEm.ReloadSyscallRegs()
```

with:

```go
e.irEm.WriteBackAll()
e.irEm.ClearDirtySyscallRegs()
e.irEm.Syscall(resumePC, dispatcherAddr)
// Reload a0/a1 through xregDst so regsUsed is set and the
// prologue prepends IRLoads for these registers.
for _, r := range []uint32{10, 11} {
    dst := e.xregDst(r)
    e.irEm.Load(dst, e.irEm.XBase(), int64(r)*8, ir.I64, false)
}
```

`xregDst(r)` sets `regsUsed |= 1<<r`, calls `MarkDirty`, and returns the VReg. The `Load` then emits the IR instruction using that VReg. This is the same sequence as `ReloadSyscallRegs` but routed through `xregDst`.

### 2. `ir/highlevel.go` — Delete `ReloadSyscallRegs`

Delete the method (lines 178-187). It's only called from `jit_emit_ir.go:318` (replaced above) and `ir/highlevel_test.go`.

### 3. `ir/highlevel_test.go` — Update or remove tests for `ReloadSyscallRegs`

The tests at lines 350 and 401 (`TestReloadSyscallRegs`, `TestReloadSyscallRegs_DoesNotAffectOtherRegs`) test the deleted method. Either delete them or rewrite them to test the new `emitter`-level reload path.

Since the reload is now in `jit_emit_ir.go` (package `riscv`, not `ir`), the unit tests in `ir/highlevel_test.go` can't test it directly. Delete the two test functions. The lockstep integration tests verify the behavior end-to-end.

### 4. `ClearDirtySyscallRegs` — Keep or move

`ClearDirtySyscallRegs` (lines 170-176) clears `dirty[10]` and `dirty[11]` before the syscall. This is fine — it operates on the `dirty` flag, not `regsUsed`. The dirty flags are managed by `ir.Emitter` throughout. Keep it in `ir/highlevel.go`.

## Critical files

- `/Users/jaten/ris/jit_emit_ir.go` — replace `ReloadSyscallRegs()` call with `xregDst`-based reload
- `/Users/jaten/ris/ir/highlevel.go` — delete `ReloadSyscallRegs` method
- `/Users/jaten/ris/ir/highlevel_test.go` — delete tests for deleted method

## Verification

1. `cd ~/ris && go test -v -run 'TestRISCVTests_Lockstep_UI/beq' .` — passes
2. `go test -v -run 'TestRISCVTests_Lockstep_UI' .` — all subtests pass
3. `go test -v -run 'TestJIT_|TestAOT_|TestBloat' .` — no regressions
4. `go test -v -run 'TestRISCVTests_UI_JIT' .` — all 55 pass
5. `go test ./ir/` — ir package tests pass
