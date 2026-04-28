# Plan: Move RET to Trampoline retStub — Fix SIGSEGV Crash

## Context

The ABJIT exit thunk emits `RET` inside JIT-compiled mmap'd code (lower_amd64_abjit.go:229). When this RET executes, its PC is in JIT memory — unknown to Go. If Go's signal handler fires at or near this instruction, or the stack unwinder walks through, it sees an unknown PC and crashes: `runtime: unexpected return pc ... called from 0x157f32b86`.

**Rule**: JIT-compiled code must NEVER execute CALL or RET. All control flow to/from Go must go through the trampoline (Go-known code). The JIT uses JMP exclusively.

## Fix

### 1. `abjit/trampoline_amd64.s` — Add `retStub` label

After the `gocall` label, add a `retStub` that just does `RET`:

```asm
gocall:
    CALL R10
    JMP (SP)
retStub:
    RET
```

### 2. `abjit/callfunc.go` — Export `retStub` address

Add `RetStubAddr()` that scans for the `RET` byte (0xC3) after the `CALL R10; JMP (SP)` sequence, similar to how `GocallAddr()` finds the `CALL R10`.

Or simpler: `retStub` is exactly 5 bytes after `gocall` (CALL R10 = 3 bytes, JMP (SP) = 2 bytes). So `RetStubAddr() = GocallAddr() + 5`.

### 3. `lower_amd64_abjit.go` — Replace RET with JMP retStub

In `emitExitThunk` (line 228-230), replace:
```go
ret := lc.c.NewProg()
ret.As = obj.ARET
lc.c.Append(ret)
```

With:
```go
lc.loadImm(int64(abjit.RetStubAddr()), stgA)
jp := lc.c.NewProg()
jp.As = obj.AJMP
jp.To.Type = obj.TYPE_REG
jp.To.Reg = stgA
lc.c.Append(jp)
```

Now every exit from JIT code goes through the trampoline's retStub for the actual RET. Go sees the RET at a known PC.

## ABJIT CALL/RET Audit After Fix

| Instruction | Location | Purpose | Status |
|---|---|---|---|
| `RET` | `trampoline_amd64.s:retStub` | Return to Go caller of callJIT | In Go code ✓ |
| `CALL R10` | `trampoline_amd64.s:gocall` | Call Go function from JIT | In Go code ✓ |
| ~~`RET`~~ | ~~`lower_amd64_abjit.go:229`~~ | ~~Exit thunk~~ | **Removed** → JMP retStub |
| ~~`CALL RAX`~~ | ~~`lower_amd64_abjit.go:428`~~ | ~~Syscall dispatch~~ | **Already removed** → gocall JMP |

**Zero CALL/RET in JIT-compiled code** after this fix.

## Files

| File | Change |
|------|--------|
| `abjit/trampoline_amd64.s` | Add `retStub: RET` after gocall |
| `abjit/callfunc.go` or `abjit/abjit.go` | Add `RetStubAddr()` |
| `lower_amd64_abjit.go:228-230` | Replace `obj.ARET` with JMP to retStubAddr |

## Verification

```bash
cd ~/ris/bench && go test -v -run TestJIT_CoreMark_ChainReference -count=1
cd ~/ris && go test -v -run TestR15IC_MatchesInterpreter -count=1 .
cd ~/ris && go test -v -run TestABJIT_NoJITtoJIT_CALL -count=1 .
```
