# Plan: Fix Two AOT Compilation Bugs in jit_aot.go — SIGSEGV Crashes

## Context

Both the `TestJIT_BenchGuest_Smoke` crash (log.red) and the `TestJIT_CoreMark_ChainReference` crash (log.red3) are caused by **two missing steps in `jit_aot.go:jitCompileAOTSegment`** that are present in the lazy `jitCompile` path but were never added to the AOT batch-compilation path.

`RunJIT` auto-calls `InstallAOTFromMem` on first entry (jit.go:711), so even tests that don't explicitly request AOT still go through `jitCompileAOTSegment`. Both bench tests hit this.

## Root Cause Analysis

### Bug 1: R15 Not Excluded from Register Pool (log.red3)

**`jit_aot.go:55-57`** — the AOT path allocates registers without removing R15:

```go
pool := j.regPolicy.Pool(res.block)
pinned := j.regPolicy.Pinned()
alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)  // R15 still in pool!
```

Meanwhile, `jitCompile` (jit_native.go:33-37) correctly removes it:

```go
pool := j.regPolicy.Pool(res.block)
pinned := j.regPolicy.Pinned()
if j.UseR15InstructionCounter || j.DebugOneBlockLockstepMode {
    pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
}
```

**Effect**: The allocator assigns VRMemBase (guest memory base pointer) to R15. The IR emitter still emits `load_ic` (MOVQ 600(BP), R15) and `inc_ic` (INCQ R15), which overwrite R15 with the instruction counter. Later stores use R15 (now containing IC value ~3000) as the memory base address → SIGSEGV at a non-canonical address.

**Proof**: VizJit dump of block at guest PC 0x1e50 shows:
```
[  49]  MOVQ  520(BP), R15     ← allocator put memBase in R15
[ 136]  MOVQ  600(BP), R15     ← load_ic OVERWRITES with IC
[ 606]  MOVQ  R15, AX          ← uses IC value as memBase → crash
[ 624]  MOVQ  DX, (AX)         ← SIGSEGV here (0x15a171434)
```

### Bug 2: Gocall Resume Addresses Never Backpatched (log.red)

**`jit_aot.go`** calls `aotBackpatchJalrICs` (line 142) but never calls any gocall resume backpatching. The lazy path calls `backpatchGocallResumes` (jit_native.go:104).

**Effect**: Syscall gocall sequences retain the sentinel `0x7CAFFE7CAFFE7CAF` as the resume address. When a syscall returns, the trampoline does `JMP (SP)` which jumps to `0x7CAFFE7CAFFE7CAF` — a non-canonical x86-64 address. macOS misreports the resulting #GP as `_SI_USER` and Go's `fixsigcode` (signal_darwin_amd64.go:93) replaces the fault address with `0xb01dfacedebac1e`.

**Proof**: VizJit dump of syscall block at PC 0x2e8e shows the un-patched sentinel:
```
15a1b8ef8  [  110]  48 b8 af 7c fe af 7c fe af 7c   MOVQ $0x7caffe7caffe7caf, AX
15a1b8f02  [  120]  48 89 04 24                     MOVQ AX, 0(SP)  ← writes sentinel to resume slot
```

## Fix

### Change 1: `jit_aot.go:55-57` — Add R15 pool exclusion

```go
pool := j.regPolicy.Pool(res.block)
pinned := j.regPolicy.Pinned()
if j.UseR15InstructionCounter || j.DebugOneBlockLockstepMode {
    pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
}
alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)
```

### Change 2: `jit_aot.go` after line 142 — Add gocall resume backpatching

Add a new function `aotBackpatchGocallResumes` (similar to `aotBackpatchJalrICs`) and call it:

```go
aotBackpatchGocallResumes(execMem, blockBase, bc.baseOffset, bc.lowerResult)
```

The function:
```go
func aotBackpatchGocallResumes(execMem []byte, blockBase uintptr, baseOffset int, lr *LowerResult) {
    for _, gr := range lr.GocallResumes {
        patchOff := int(gr.AddrMov.Pc) + 2
        resumeAddr := blockBase + uintptr(gr.ResumeProg.Pc)
        binary.LittleEndian.PutUint64(execMem[baseOffset+patchOff:], uint64(resumeAddr))
    }
}
```

## Files

| File | Change |
|------|--------|
| `jit_aot.go:55-57` | Add `removeReg(pool.IntRegs, R15)` when UseR15InstructionCounter is set |
| `jit_aot.go` after line 142 | Add `aotBackpatchGocallResumes(...)` call |
| `jit_aot.go` (new func) | Add `aotBackpatchGocallResumes` function |

## Verification

```bash
# The two crashing tests:
cd ~/ris && go test -v -run TestJIT_BenchGuest_Smoke ./bench/
cd ~/ris && go test -v -run TestJIT_CoreMark_ChainReference ./bench/

# Full bench test suite:
cd ~/ris && go test -v ./bench/

# Safety test (zero CALL/RET in JIT code):
cd ~/ris && go test -v -run TestABJIT_NoJITtoJIT_CALL .

# RISC-V test suite:
cd ~/ris && go test -v -run TestRISCVTests .
```
