# abjit: Aaron Balke JIT trampoline for riscv-emu-golang

## Context

Bridge2 SPSC ring: 186ns (cross-core cache bounce). Plain CGO: 34ns (same
thread). The Balke gojit approach runs JIT code on the same goroutine with
zero CGO and allows GC-safe callbacks into Go mid-execution. Expected
trampoline overhead: ~5-15ns.

**Goal**: New `~/ris/abjit/` package providing an alternative call path.
All existing JIT machinery stays untouched.

---

## 1. Parallel Stacks Design

JIT code maintains **two stacks simultaneously**:

| Stack | Register | Location | Purpose |
|-------|----------|----------|---------|
| Go stack | RSP | Goroutine stack | Trampoline frame, gocall resume slot, Go callbacks |
| Sandbox stack | R12 | mmap'd memory with guard pages | JIT-internal function calls between blocks, temporaries |

### Why two stacks

- **Containment**: JIT code's own stack operations (push/pop of temporaries,
  inter-block calls) stay in sandbox memory. A bug in JIT code can't corrupt
  the Go stack because JIT never writes to RSP-relative addresses (except
  the single `[RSP+0]` resume slot for gocall).
- **Depth**: Sandbox stack can be 512KB+ (mmap'd). Go stack stays small
  (64KB frame just for callbacks and runtime safety).
- **Control**: Since we generate all JIT code, we choose which "stack pointer"
  each instruction uses. Guest RISC-V SP (x2) is just a register value
  mapping to guest virtual addresses in GuestMemory — a third, completely
  separate address space.

### RSP rules

JIT code may ONLY touch RSP in one way: `MOV [RSP], R10` to store the gocall
resume address. All other stack-like operations use R12 (sandbox stack) or
direct CPU struct offsets via R9.

### R12 sandbox stack operations (for future inter-block JIT calls)

```asm
; JIT "push" (save return address for intra-JIT calls)
SUB R12, 8
MOV [R12], return_addr

; JIT "pop" (return from intra-JIT call)
MOV RAX, [R12]
ADD R12, 8
JMP RAX
```

Initially unused — the dispatch loop handles block transitions. But the
register is pinned now so the design supports it from day one.

---

## 2. Register Convention

### Reserved registers (NOT in allocation pool)

| Register | Role | Notes |
|----------|------|-------|
| R14 | Go goroutine pointer (`g`) | **NEVER TOUCHED** by JIT code. Go runtime crashes if clobbered. |
| R9  | CPU state pointer (pinned) | Set by trampoline. Restored by gocall handler after callbacks. |
| R12 | Sandbox stack pointer | Set by trampoline. Callee-saved, survives callbacks. |
| RSP | Go stack pointer | Only `[RSP+0]` used for gocall resume slot. |
| RAX | Staging register A | Scratch for spill/reload, DIV results, temporaries. |
| RCX | Staging register B | Scratch for shifts, MUL upper half, temporaries. |

### Allocation pool: 8 integer registers

**Callee-saved** (survive Go callbacks — no save/restore needed):
- RBX, RBP, R13, R15 = 4 registers

**Caller-saved** (clobbered by Go callbacks — must spill before, reload after):
- RDX, RSI, RDI, R8 = 4 registers

**Total: 8 allocatable integer registers** (down from 12 in current rv8 pool).

R10 and R11 are also usable between callbacks (giving 10 total), but since
the callback emit sequence clobbers them, treating them as "staging" for the
callback mechanism is simpler initially. Can promote to pool later.

### Pinned register map (replaces `RV8Pinned`)

```go
func ABJITPinned() map[VReg]int16 {
    return map[VReg]int16{
        VRRegFile: goasm.REG_AMD64_BP,  // register file base (same as rv8)
    }
}
```

RBP is pinned to VRRegFile just like current rv8. It's callee-saved per
System V ABI so it survives Go callbacks (Go function prologue pushes/pops
its own RBP without disturbing the caller's value).

R9 and R12 are loaded by the trampoline before entering JIT code. They're
not tracked by the IR allocator — they're implicit context, like RSP.

### Why R10/R11 are NOT permanently reserved

R10 and R11 are only clobbered by the 5-instruction callback emit sequence.
Between callbacks, JIT code can use them freely. The register allocator
can treat them as caller-saved registers (spill around callbacks) or leave
them out of the pool for simplicity. Phase 1 excludes them; Phase 2 can
add them back as caller-saved with proper spill logic.

### FP registers: unchanged

XMM0-XMM13 pool, XMM14-XMM15 staging (same as current rv8).

---

## 3. Trampoline Assembly

### `abjit/trampoline_amd64.s`

```asm
#include "funcdata.h"
#include "textflag.h"

// func callJIT(code, cpuState, sandboxSP uintptr)
TEXT ·callJIT(SB), 0, $65528-24
    NO_LOCAL_POINTERS
    //
    // Frame layout:
    //   SP+0      resume address slot (JIT writes here for gocall)
    //   SP+8      saved RBX
    //   SP+16     saved RBP
    //   SP+24     saved R12
    //   SP+32     saved R13
    //   SP+40     saved R15
    //   SP+48..   65KB of stack for Go callbacks
    //
    MOVQ BX,  8(SP)
    MOVQ BP,  16(SP)
    MOVQ R12, 24(SP)
    MOVQ R13, 32(SP)
    MOVQ R15, 40(SP)

    MOVQ cpuState+8(FP), R9       // pin CPU state pointer
    MOVQ sandboxSP+16(FP), R12    // pin sandbox stack pointer
    MOVQ code+0(FP), AX
    JMP AX                         // enter JIT code

gocall:
    // R10 = Go function address (set by JIT code)
    // [RSP+0] = resume address (set by JIT code)
    CALL R10                        // return addr points into callJIT → GC-safe
    MOVQ cpuState+8(FP), R9        // restore R9 (clobbered by Go callee)
    JMP (SP)                        // resume JIT at address stored at [RSP]

TEXT ·callJITImplAddr(SB), 0, $0-8
    NO_LOCAL_POINTERS
    MOVQ $·callJIT(SB), AX
    MOVQ AX, ret+0(FP)
    RET
```

### Frame size rationale

$65528 ≈ 64KB. This is NOT for JIT code's own stack (that's the sandbox
stack in R12). It's so the Go runtime pre-grows the goroutine stack before
entering callJIT, ensuring `//go:nosplit` callback functions have ample
room without triggering stack growth. Matches the existing
`internal/jitcall` approach.

### Exit sequence (emitted at end of every JIT block)

JIT code writes results (PC, status) to CPU struct via `[R9+offset]`, then:

```asm
; Restore callee-saved registers
MOV RBX, [RSP+8]
MOV RBP, [RSP+16]
MOV R12, [RSP+24]
MOV R13, [RSP+32]
MOV R15, [RSP+40]
; Undo Go's frame (framesize + 8 for BP push by Go prologue)
ADD RSP, <framesize>       ← exact value TBD by disassembly verification
RET
```

**VERIFIED (Phase 0):** Go inserts `PUSH BP; MOV BP, SP; SUB SP, 0xFFF8`.
Total frame = 65528 (declared) + 8 (BP push) = 65536 = 0x10000 bytes.

Exit sequence:
```asm
MOV RBX, [RSP+8]       ; restore callee-saves
MOV R12, [RSP+0x18]
MOV R13, [RSP+0x20]
MOV R15, [RSP+0x28]
ADD RSP, 0xFFF8         ; undo SUB SP, 0xFFF8
POP BP                  ; undo PUSH BP
RET
```
Note: we do NOT restore RBP from our save slot. Go's `PUSH BP` already
preserved the caller's BP. We just `POP BP` to get it back.

FP offsets (verified by disassembly):
- `code+0(FP)`      = `SP + 0x10008`
- `cpuState+8(FP)`  = `SP + 0x10010`
- `sandboxSP+16(FP)` = `SP + 0x10018`

CALL R10 byte pattern: `{0x41, 0xFF, 0xD2}` — confirmed at offset +0x59.

Our callee-save offsets in the frame:
- `SP+0x08` = RBX
- `SP+0x10` = RBP (Go's frame pointer, NOT caller's original BP)
- `SP+0x18` = R12
- `SP+0x20` = R13
- `SP+0x28` = R15

---

## 4. Callback Mechanism

### `getCallAddr()` — find the gocall label at runtime

Scans the first 0x80 bytes of callJIT's machine code for the byte pattern
`{0x41, 0xFF, 0xD2}` = `CALL R10`. Returns the address of that instruction.

### Callback emit sequence (5 instructions, ~34 bytes)

```asm
MOVABS gocallAddr, R11         ; 10B — address of gocall label
LEA    R10, [RIP+offset]       ;  7B — compute resume address
MOV    [RSP], R10              ;  4B — store resume in callJIT's frame
MOVABS goFuncAddr, R10         ; 10B — Go function to call
JMP    R11                     ;  3B — → gocall: CALL R10 → Go → JMP (SP)
```

### Callback functions (all `//go:nosplit`)

```go
var cpuPtr *State  // set once before JIT entry

//go:nosplit
func goLoad64(addr uint64) uint64

//go:nosplit
func goStore64(addr uint64, val uint64)

//go:nosplit
func goEcall() uint64
```

These use ABIInternal convention: args arrive in RAX, RBX; result in RAX.
The `CALL R10` at gocall calls through the ABI0 wrapper that Go generates,
which converts between ABI0 and ABIInternal. System V callee-saved registers
(RBX, RBP, R12, R13, R15) are preserved across the call.

---

## 5. Implementation Phases

### Phase 0: Verification tests (before writing any JIT code)

**Must resolve uncertainties first.**

1. **Frame offset verification**: Assemble `callJIT`, disassemble with
   `go tool objdump`, and record the exact prologue sequence. Determine:
   - Does Go insert `PUSH RBP; MOV RBP, RSP`?
   - What is the exact `ADD RSP, N` needed for the exit sequence?
   - Where is `FP` relative to `SP` (for `cpuState+8(FP)` access)?

2. **getCallAddr byte pattern**: Verify `CALL R10` assembles to
   `{0x41, 0xFF, 0xD2}`. If Go's assembler uses a different encoding
   (e.g., `{0xFF, 0xD2}` without REX), update the scan pattern.

3. **R14 liveness test**: Write a trivial JIT block that does NOT modify
   R14, calls a Go function via gocall, forces `runtime.GC()` inside
   the callback, and verifies no crash. Then write one that DOES modify
   R14 and verify it DOES crash (confirms we understand the constraint).

4. **Callee-save verification**: Write a JIT block that puts known values
   in RBX, RBP, R12, R13, R15, calls a Go function, and checks the values
   survive. Verify R9 is correctly restored by the gocall handler.

### Phase 1: Minimal trampoline + benchmark

- `trampoline_amd64.s` + `trampoline.go`
- `callfunc.go` (getCallAddr)
- `emit.go` (minimal: movabs, mov, add, ret)
- `mmap_unix.go` (mmapExec, munmapExec)
- `abjit.go` (State struct, RunBlock)
- `abjit_test.go`:
  - `TestTrampolineRoundTrip` — JIT loads X[1], adds 1, stores X[2], exits
  - `BenchmarkTrampolineOverhead` — JIT immediately exits (measures entry/exit cost)
  - `BenchmarkCGO` — baseline comparison

### Phase 2: Callbacks

- Callback emit helper (`EmitCallFunc`)
- Memory load/store callbacks (`goLoad64`, `goStore64`)
- `TestCallback` — JIT calls goLoad64, verifies result
- `TestGCSafety` — forces runtime.GC() during callback
- `BenchmarkCallbackRoundTrip` — measures callback overhead

### Phase 3: Register pool integration

- `ABJITPool()` and `ABJITPinned()` functions (can live in `ir/` or `abjit/`)
- Verify allocator excludes R14, R9, R12
- Verify spill/reload around callbacks for caller-saved registers
- Integration with existing IR emitter (new lowering target)

---

## 6. Files to create

| File | Purpose |
|------|---------|
| `~/ris/abjit/trampoline_amd64.s` | Assembly: callJIT + gocall + callJITImplAddr |
| `~/ris/abjit/trampoline.go` | Go stubs for asm functions |
| `~/ris/abjit/callfunc.go` | getCallAddr, EmitCallFunc |
| `~/ris/abjit/emit.go` | Minimal x86_64 byte-level emitter |
| `~/ris/abjit/mmap_unix.go` | Executable memory allocation (mmap/munmap) |
| `~/ris/abjit/abjit.go` | State struct, callbacks, RunBlock entry point |
| `~/ris/abjit/abjit_test.go` | Tests + benchmarks (Phase 0-2) |

## 7. Files to modify (Phase 3 only)

| File | Change |
|------|--------|
| `~/ris/ir/lower_amd64.go` | Add `ABJITPool()` and `ABJITPinned()` |

---

## 8. Verification

```bash
# Phase 0: disassemble trampoline to verify frame layout
cd ~/ris/abjit && go test -c -o /dev/null && go tool objdump -S abjit.test | grep -A 50 callJIT

# Phase 1: basic tests + benchmark
cd ~/ris/abjit && go test -v
cd ~/ris/abjit && go test -run='^$' -bench=. -benchtime=3s

# Phase 2: GC stress
cd ~/ris/abjit && GOGC=1 go test -v -run TestGCSafety -count=100

# Phase 3: verify R14 exclusion
cd ~/ris/abjit && go test -v -run TestR14Never
```

---

## 9. What stays unchanged

- `internal/jitcall/` trampoline and Call/CallAOT functions
- `ir/` pipeline (RV8Pool, RV8Pinned, lower_amd64_rv8.go)
- `jit.go` RunJIT dispatch loop
- TCC/C code generation path
- AOT segment management
- bridge2 SPSC approach
