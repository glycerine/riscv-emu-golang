# Plan: Wire UseICR15 Through RegPolicy + Save/Restore R15 Across Go Calls

## Context

R15 holds the cumulative guest IC but gets clobbered by Go calls (syscall dispatcher, Go callbacks). The lowerer needs to spill R15 to State.IC before and reload after each CALL, but only when R15 IC is active. The flag must flow from JIT → RegPolicy → lowerer.

## Changes

### 1. `lower_amd64.go:32-37` — Add `UseICR15` to RegPolicy

```go
type RegPolicy struct {
    Name     string
    Pool     func(*Block) RegPool
    Pinned   func() map[VReg]int16
    Lower    func(*goasm.Ctx, *Block, *Allocation) (*LowerResult, error)
    UseICR15 bool
}
```

### 2. `jit.go` — Set `UseICR15` on policy when configuring JIT

In `SetRegPolicy` or wherever the policy is configured, copy the JIT flag:

```go
func (j *JIT) SetRegPolicy(p RegPolicy) {
    j.regPolicy = p
    j.regPolicy.UseICR15 = j.UseR15InstructionCounter
    ...
}
```

Also update any place that creates/copies the policy (e.g., `NewJIT`, AOT clone path at ~line 387).

### 3. `lower_amd64_abjit.go` — Store policy flag in `lowerCtxABJIT`

The `lowerCtxABJIT` is created inside `LowerAMD64_ABJIT`. It doesn't currently receive the RegPolicy. But `LowerAMD64_ABJIT` is called via the function pointer `RegPolicy.Lower` — so it doesn't have access to the policy struct.

**Fix**: Pass the flag through the `Block` or through a closure. Simplest: `LowerAMD64_ABJIT` is assigned as `PolicyABJIT.Lower`. We can make `PolicyABJIT` a package-level var and read `PolicyABJIT.UseICR15` directly from the lowerer. But that's fragile.

**Better**: Since `RegPolicy.Lower` is a function pointer, make it a closure that captures the policy:

```go
var PolicyABJIT = RegPolicy{
    Name:   "abjit",
    Pool:   ABJITPool,
    Pinned: ABJITPinned,
    Lower:  LowerAMD64_ABJIT, // will be wrapped below
}
```

Change to store `UseICR15` on `lowerOps` (the shared struct embedded in both `lowerCtxABJIT` and `lowerCtxRV8`). Add a `useICR15 bool` field to `lowerOps` (lower_amd64_ops.go:45). Set it in `LowerAMD64_ABJIT` — but how does it get the value?

**Cleanest approach**: Change `Lower` signature to accept RegPolicy (or just the bool). But this changes the function pointer type.

**Actually simplest**: Add `UseICR15 bool` to the `Allocation` struct — it's already passed to `Lower`. Set it in `jitCompile` before calling `Lower`.

Wait, the user chose Option D (RegPolicy). Let me make it work without changing the Lower signature. The policy is stored on `j.regPolicy`. The function `jitCompile` has access to `j`. It calls `j.regPolicy.Lower(ctx, res.block, alloc)`. We can set a field on `Block` from `jitCompile`:

Actually — even simpler. The Lower function is `LowerAMD64_ABJIT` which takes `(ctx, b, alloc)`. We can just set `b.UseICR15` from jitCompile before calling Lower. But the user rejected adding it to Block.

OK, let me just read `UseICR15` from the global `PolicyABJIT` variable. In `jit.go`, `SetRegPolicy` copies the JIT's `UseR15InstructionCounter` to `regPolicy.UseICR15`. The lowerer function `LowerAMD64_ABJIT` is a package-level function — it can't access the policy. But we can make it a method or use a package-level variable.

**Final approach**: Add a package-level `var abjitUseICR15 bool`. Set it in `SetRegPolicy` when the policy name is "abjit". Read it in `LowerAMD64_ABJIT`. Simple, no signature changes, no struct pollution.

### 4. `lower_amd64_abjit.go` — Spill/load R15 around CALLs

In `abjitSyscall` (~line 424), before CALL:
```go
if abjitUseICR15 {
    lc.opsSpillIC()  // MOV [RBP+600], R15
}
```
After CALL returns (~line 430):
```go
if abjitUseICR15 {
    lc.opsLoadIC()   // MOV R15, [RBP+600]
}
```

Same in `abjitCall` (~line 692/697).

### 5. Interpreter — no change needed

`cpu.step()` returns, caller does `cpu.cycle++`. Before next JIT dispatch, `abjitDispatch` sets `s.IC = cpu.cycle`, and JIT loads `R15 = s.IC`. Interpreter-counted instructions are included.

## Files

| File | Change |
|------|--------|
| `lower_amd64.go:32` | Add `UseICR15 bool` to `RegPolicy` |
| `lower_amd64_abjit.go` | Add `var abjitUseICR15 bool`; spill/load R15 in `abjitSyscall` and `abjitCall` |
| `jit.go` | Copy `UseR15InstructionCounter` to `regPolicy.UseICR15` in `SetRegPolicy`; set `abjitUseICR15` |

## Verification

```bash
cd ~/ris && go test -v -run TestR15IC_MatchesInterpreter -count=1 .
cd ~/ris/bench && go test -v -run TestJIT_DispatchStats -count=1  # should show ~2.5M insns, not trillions
cd ~/ris && go test -run='^$' -bench='Benchmark(AotJIT|LazyJIT)_BenchGuest' -benchtime=1x ./bench/
```
