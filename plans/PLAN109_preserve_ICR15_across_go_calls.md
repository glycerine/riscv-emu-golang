# Plan: Save/Restore R15 IC Across Go Calls + Interpreter Verification

## Context

R15 holds the cumulative guest instruction count. Two JIT code paths call into Go via x86 CALL: `abjitSyscall` (ECALL dispatcher, lower_amd64_abjit.go:424-430) and `abjitCall` (Go callback, lower_amd64_abjit.go:692-697). Go functions freely clobber R15, destroying the IC. The syscall path even has a placeholder comment at line 400: "Stage IC to State BEFORE the CALL" with no implementation.

Additionally, the dispatch stats test shows `18446744073642442760` (negative R15) for the AOT bench_guest — R15 was clobbered during an inline ECALL that panicked (LinuxExit).

## Changes

### 1. `lower_amd64_abjit.go` — abjitSyscall: save/restore R15

Before the `CALL dispatcher` at line 424, spill R15 to State.IC. After CALL returns, reload R15 from State.IC and increment by 1 (the ECALL instruction itself):

```go
// Save R15 (IC) to State before CALL (Go clobbers R15).
lc.opsSpillIC()  // MOV [RBP+600], R15

// CALL dispatcher.
lc.loadImm(int64(sym.Addr), stgA)
...CALL stgA...

// Restore R15 (IC) from State after CALL.
lc.opsLoadIC()   // MOV R15, [RBP+600]
```

The ECALL instruction's own IncIC was already emitted by `emitBudgetCheck()` before the syscall IR was emitted, so no extra +1 is needed here.

### 2. `lower_amd64_abjit.go` — abjitCall: save/restore R15

Same pattern. The `callerSavedInt` list at line 660 only covers SysV caller-saved registers (DX, SI, DI, R8-R11). R15 is not in it because it's SysV callee-saved — but Go doesn't honor SysV conventions. Add explicit R15 save/restore around the CALL:

Before the CALL at line 692: `lc.opsSpillIC()`
After the CALL returns (before restoring other live regs): `lc.opsLoadIC()`

### 3. Interpreter verification

The interpreter's `step()` (cpu.go:133) executes one instruction and returns. The caller does `cpu.cycle++` (jit.go:653, etc.). With the new R15 IC model, `cpu.cycle` is the authoritative counter. When the interpreter runs (JIT fallback), `cpu.cycle++` correctly counts the interpreted instruction. Before the next JIT dispatch, `abjitDispatch` copies `cpu.cycle → s.IC`, and the JIT loads `R15 = s.IC`. So interpreter-executed instructions ARE included in R15's cumulative count. **No interpreter changes needed.**

### 4. Guard the save/restore on `useICR15`

The spill/load should only be emitted when R15 is used for IC counting. The lowerer doesn't currently have access to the `useICR15` flag. Two options:

**Option A (simple)**: Always emit the spill/load. R15 is excluded from the allocator pool when `UseR15InstructionCounter` is true (which is the default). When false, R15 is in the pool and might be allocated — but then `opsSpillIC`/`opsLoadIC` would clobber an allocated value. So we must guard.

**Option B (correct)**: Pass `useICR15` to the lowerer context. The `lowerCtxABJIT` already has access to `lc.blk` — but the flag lives on the JIT, not the block. Add a bool to the IR Block or pass through the LowerResult.

**Recommendation**: Option A is safe because `UseR15InstructionCounter` defaults to true and R15 is always excluded from the pool in practice. But to be defensive, add a `HasIC bool` field to the IR `Block` struct (set during emission), and check it in the lowerer.

Actually, simplest: just always emit the save/restore. If R15 is not used for IC, the extra MOVs are harmless (they read/write State.IC which is unused). The cost is 2 MOVs per Go call — negligible.

## Files

| File | Change |
|------|--------|
| `lower_amd64_abjit.go:400-430` | Add `opsSpillIC()` before and `opsLoadIC()` after CALL in `abjitSyscall` |
| `lower_amd64_abjit.go:682-697` | Add `opsSpillIC()` before and `opsLoadIC()` after CALL in `abjitCall` |

## Verification

```bash
# IC accuracy test (small ELF, exact match with interpreter)
cd ~/ris && go test -v -run TestR15IC_MatchesInterpreter -count=1 .

# AOT bench — should now show sane MIPS (~2000-3000, not trillions)
cd ~/ris && go test -run='^$' -bench='BenchmarkAotJIT_BenchGuest' -benchtime=1x ./bench/

# Dispatch stats — retired insns should be ~2.5M, not 18 quintillion  
cd ~/ris/bench && go test -v -run TestJIT_DispatchStats -count=1

# Full suite
cd ~/ris && go test -count=1 .
```
