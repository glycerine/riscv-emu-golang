# GoCPU: Balke-style JIT trampoline for riscv-emu-golang

## Context

The bridge2 SPSC ring achieves 186ns round-trip, but cross-core cache-line
bounces are the fundamental bottleneck. A plain CGO call is 34ns (same thread).
The Balke gojit approach runs JIT code on the same goroutine with zero CGO,
and critically allows JIT code to **call back into Go functions** mid-execution
(GC-safe, stack-safe). This enables inline memory access, ecall handling, etc.
without block-exit/re-enter overhead.

**Goal**: Build a new `~/ris/gocpu/` package that provides an alternative
call path using the Balke trampoline. Leaves all existing JIT machinery intact.

---

## Core Mechanism (from Balke's gojit)

### The `callJIT` trampoline trick

```
TEXT ·callJIT(SB), 0, $framesize-argsize
    NO_LOCAL_POINTERS          ← GC won't scan this frame
    save callee-saved regs
    load CPU state ptr → R9
    MOVQ code+0(FP), AX
    JMP AX                     ← enter JIT code (NOT call)
gocall:                        ← JIT jumps here when it wants to call Go
    CALL R10                   ← R10 = Go function addr; return addr is inside
                                  callJIT, so Go runtime sees a valid Go frame
    JMP (SP)                   ← resume JIT at address JIT stored at [RSP]
```

### JIT→Go callback sequence (emitted in JIT machine code)

```asm
MOVABS gocallAddr, R11         ; address of gocall label
LEA    R10, [RIP+offset]       ; compute resume address (after JMP R11)
MOV    [RSP], R10              ; save resume address in callJIT's frame
MOVABS goFuncAddr, R10         ; Go function to call
JMP    R11                     ; → gocall: CALL R10 → Go func → JMP (SP) → resume
```

### Why it's GC-safe

- `CALL R10` at `gocall:` pushes a return address that points inside `callJIT`
  (a real Go function). The GC sees a valid stack frame chain.
- `NO_LOCAL_POINTERS` tells the GC this frame has no managed pointers to scan.
- Callback functions use `//go:nosplit` to prevent stack growth during the call.

---

## Register Convention

| Register | Usage | Notes |
|----------|-------|-------|
| R9 | CPU state pointer (pinned) | Set by trampoline, restored after callbacks |
| R14 | goroutine pointer (`g`) | **NEVER TOUCHED** by JIT code |
| R10 | Go function addr (callbacks) | Clobbered by each callback |
| R11 | gocall label addr (callbacks) | Clobbered by each callback |
| RAX-RDI, R8 | Scratch / guest regs | Free for JIT computation |
| R12, R13, R15 | Callee-saved / guest regs | Saved/restored by trampoline |
| RBX, RBP | Callee-saved | Saved/restored by trampoline |
| RSP | Stack pointer | [RSP+0] = resume slot for gocall |

R14 is off-limits because Go requires it to hold the goroutine pointer at all
times. Violating this crashes the runtime on the next GC scan or stack growth.

---

## Package Structure: `~/ris/gocpu/`

### `trampoline_amd64.s` — Assembly trampoline

```asm
TEXT ·callJIT(SB), 0, $65528-16
    NO_LOCAL_POINTERS
    // Frame layout:
    //   SP+0      resume address slot (JIT writes here for gocall)
    //   SP+8      saved RBX
    //   SP+16     saved RBP
    //   SP+24     saved R12
    //   SP+32     saved R13
    //   SP+40     saved R15
    //   SP+48..   available stack for JIT code and callbacks
    MOVQ BX,  8(SP)
    MOVQ BP,  16(SP)
    MOVQ R12, 24(SP)
    MOVQ R13, 32(SP)
    MOVQ R15, 40(SP)
    MOVQ cpuState+8(FP), R9     // pin CPU state pointer
    MOVQ code+0(FP), AX
    JMP AX
gocall:
    CALL R10
    MOVQ cpuState+8(FP), R9     // restore R9 (callback may clobber)
    JMP (SP)

TEXT ·callJITImplAddr(SB), 0, $0-8
    NO_LOCAL_POINTERS
    MOVQ $·callJIT(SB), AX
    MOVQ AX, ret+0(FP)
    RET
```

The $65528 frame (≈64KB) ensures enough stack space for JIT code and
`//go:nosplit` callbacks without triggering stack growth. Matches the
existing `internal/jitcall` approach.

### `trampoline.go` — Go function stubs

```go
package gocpu

func callJIT(code uintptr, cpuState uintptr)
func callJITImplAddr() uintptr
```

### `callfunc.go` — Runtime discovery of gocall label + emit helper

- `getCallAddr()`: scans callJIT's bytes for `{0x41, 0xFF, 0xD2}` (CALL R10
  encoding) to find the `gocall:` label address at runtime.
- `getExitAddr()`: scans for the callee-save restore sequence to find the
  exit point (or we emit exit inline in every block).
- `EmitCallFunc(buf, goFunc)`: emits the 5-instruction callback sequence
  (MOVABS R11, LEA R10, MOV [RSP], MOVABS R10, JMP R11) into a byte buffer.

### `emit.go` — Minimal x86_64 code emitter

Simple byte-level emitter for:
- `MOVABS imm64, reg` (10 bytes)
- `MOV reg, [R9+offset]` / `MOV [R9+offset], reg` (memory access via CPU ptr)
- `ADD`, `SUB`, basic ALU on registers
- `LEA reg, [RIP+off]` (for resume address)
- `MOV [RSP], reg` (store resume address)
- `JMP reg` (for gocall jump)
- Callee-save restore + `ADD RSP, framesize` + `RET` (exit sequence)

This is NOT the full IR pipeline. It's a minimal emitter for the proof-of-concept.
Later phases will integrate with the existing `ir/` pipeline.

### `mmap_unix.go` — Executable memory allocation

```go
func mmapExec(size int) ([]byte, error)    // mmap RWX
func munmapExec(b []byte) error            // munmap
```

Uses `syscall.Mmap` with `PROT_READ|PROT_WRITE|PROT_EXEC, MAP_PRIVATE|MAP_ANON`.

### `gocpu.go` — CPU state and callbacks

**State struct** (wraps existing `riscv.CPU` or mirrors its layout):
```go
type State struct {
    X     [32]uint64   // guest integer registers
    F     [32]uint64   // guest FP registers
    FCSR  uint32       // FP control/status
    PC    uint64       // program counter
    Cycle uint64       // instruction count
    Mem   *riscv.GuestMemory
    // ... status fields for block exit
}
```

**Callback functions** (all `//go:nosplit`):
```go
var cpuPtr *State  // global, set once before JIT entry

//go:nosplit
func goLoad64(addr uint64) uint64 {
    return cpuPtr.Mem.Load64Unchecked(addr)
}

//go:nosplit
func goStore64(addr uint64, val uint64) {
    cpuPtr.Mem.Store64Unchecked(addr, val)
}
```

These use ABIInternal convention: args in RAX, RBX; return in RAX.

**Entry point**:
```go
func RunBlock(code []byte, state *State) {
    cpuPtr = state
    callJIT(Addr(code), uintptr(unsafe.Pointer(state)))
}
```

### `gocpu_test.go` — Tests and benchmarks

**Tests**:
- `TestTrampolineRoundTrip`: JIT code loads a value from State.X[1], adds 1,
  stores to State.X[2], exits. Verify X[2] == X[1]+1.
- `TestCallback`: JIT code calls goLoad64 via gocall mechanism, verifies
  returned value. Exercises the full callback path.
- `TestGCSafety`: Run JIT code that calls back into Go, force `runtime.GC()`
  inside the callback. Verify no crash (the whole point of Balke's approach).

**Benchmarks**:
- `BenchmarkTrampolineCall`: Measure callJIT overhead (JIT code immediately exits).
  Expected: ~5-15ns.
- `BenchmarkCallbackRoundTrip`: JIT code calls goLoad64 and exits.
  Compare with CGO (34ns) and bridge2 (186ns).
- `BenchmarkCGO`: Existing CGO baseline for comparison (already in bridge2).

---

## Critical Invariants (must be verified)

1. **R14 untouched**: JIT code must never modify R14. Allocator must exclude it.
2. **NO_LOCAL_POINTERS**: Required on callJIT so GC doesn't scan the JIT frame.
3. **`//go:nosplit` on all callbacks**: Prevents stack growth during JIT→Go calls.
   Stack growth would relocate the frame and corrupt the resume address at [RSP].
4. **Frame offset verification**: After assembling, disassemble callJIT to confirm
   exact byte offsets of callee-save slots and resume slot. Go may insert BP
   save/restore automatically, shifting offsets.
5. **CALL R10 byte pattern**: Must be `{0x41, 0xFF, 0xD2}`. If Go's assembler
   emits a different encoding, getCallAddr() fails silently.
6. **R9 restore after callback**: Go functions follow ABIInternal and may clobber
   R9. The gocall handler must restore R9 from `cpuState+8(FP)` after each call.
7. **Exit sequence must match frame**: The ADD RSP value in the exit sequence
   must exactly match the frame size that Go's prologue allocated. Verify by
   examining the assembled function bytes.

---

## Files to create

| File | Purpose |
|------|---------|
| `~/ris/gocpu/trampoline_amd64.s` | Assembly: callJIT + gocall + callJITImplAddr |
| `~/ris/gocpu/trampoline.go` | Go stubs for asm functions |
| `~/ris/gocpu/callfunc.go` | getCallAddr, EmitCallFunc |
| `~/ris/gocpu/emit.go` | Minimal x86_64 byte emitter |
| `~/ris/gocpu/mmap_unix.go` | Executable memory allocation |
| `~/ris/gocpu/gocpu.go` | State struct, callbacks, RunBlock |
| `~/ris/gocpu/gocpu_test.go` | Tests + benchmarks |

---

## Verification

```bash
# Unit tests — correctness + GC safety
cd ~/ris/gocpu && go test -v

# Benchmark — trampoline overhead vs CGO vs bridge2
cd ~/ris/gocpu && go test -run='^$' -bench=. -benchtime=3s

# GC stress test — force GC during callbacks
cd ~/ris/gocpu && GOGC=1 go test -v -run TestGCSafety -count=100
```

---

## What this does NOT change

- Existing `internal/jitcall/` trampoline (untouched)
- Existing IR pipeline (`ir/`, `jit_emit_ir.go`, `jit_native.go`)
- Existing `RunJIT` dispatch loop
- Existing TCC/C code generation path
- Existing AOT segment management
- Existing bridge2 SPSC approach
