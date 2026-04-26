# abjit: Aaron Balke JIT trampoline for riscv-emu-golang

## Context

Bridge2 SPSC ring: 186ns (cross-core cache bounce). Plain CGO: 34ns (same
thread). The Balke gojit approach runs JIT code on the same goroutine with
zero CGO and allows GC-safe callbacks into Go mid-execution.

**Goal**: New `~/ris/abjit/` package providing an alternative call path.
All existing JIT machinery stays untouched.

---

## Phase 0 Results (verified)

### Performance
| Metric | Latency | vs CGO |
|--------|---------|--------|
| abjit trampoline entry/exit | **2.8 ns** | 12x faster |
| abjit + one Go callback | **4.0 ns** | 8.5x faster |
| CGO call | 34 ns | baseline |
| bridge2 SPSC ring | 186 ns | 5.5x slower |

### Frame layout (verified by disassembly)

Go inserts `PUSH BP; MOV BP,SP; SUB SP,0xFFF8` in the prologue.
Total frame = 65528 (declared) + 8 (BP push) = 65536 = 0x10000 bytes.

```
SP+0x00     resume address slot (JIT writes [RSP] for gocall)
SP+0x08     saved RBX
SP+0x10     saved RBP (Go's frame pointer value)
SP+0x18     saved R12
SP+0x20     saved R13
SP+0x28     saved R15
SP+0x30..   available stack for Go callbacks (~65KB)
...
SP+0xFFF8   Go's pushed BP (caller's original)
SP+0x10000  return address
SP+0x10008  code+0(FP)       — first arg
SP+0x10010  cpuState+8(FP)   — second arg
SP+0x10018  sandboxSP+16(FP) — third arg
```

Exit sequence (verified):
```asm
MOV RBX, [RSP+0x08]
MOV R12, [RSP+0x18]
MOV R13, [RSP+0x20]
MOV R15, [RSP+0x28]
ADD RSP, 0xFFF8          ; undo SUB SP
POP RBP                  ; undo PUSH BP
RET
```

### CALL R10 byte pattern: `{0x41, 0xFF, 0xD2}` at offset +0x59

### Register preservation across Go callbacks (empirically verified)

**Preserved** by Go functions (callee-saved):
- RBX, RBP, R12, R13, R15 — all sentinels survived callbacks ✓
- R14 — goroutine pointer, always maintained by Go ✓

**Clobbered** by Go functions (caller-saved):
- RAX, RCX, RDX, RSI, RDI, R8, R9, R10, R11

**R9 restoration**: gocall handler reloads R9 from `cpuState+8(FP)` after
every callback. Verified: R9 is identical before and after callbacks ✓

### GC safety: verified

100 iterations of `runtime.GC()` during JIT callbacks — no crash ✓

### Critical: state must be heap-allocated

callJIT's 65KB frame triggers `morestack` on entry, which copies the
goroutine stack to a larger allocation. Any `uintptr` pointing to the
old stack becomes stale. **All data passed as `uintptr` to callJIT must
be heap-allocated** (or in global/mmap'd memory).

This is NOT a bug — it's the correct design. The CPU state struct will be
heap-allocated in production. The existing `internal/jitcall` ($65536 frame)
has the same behavior; it works because it passes pointers to the heap-
allocated CPU struct, not stack locals.

**Why not NOSPLIT?** NOSPLIT would skip morestack and keep stack pointers
valid. But NOSPLIT functions have an ~800 byte stack budget (StackNosplit).
Our $65528 frame wouldn't even compile as NOSPLIT. With a tiny NOSPLIT frame
($48), callbacks would have almost no stack space — any Go callback that
isn't itself nosplit (e.g., runtime.GC(), memory access with error handling)
would overflow. NOSPLIT is incompatible with the callback design.

---

## 1. Parallel Stacks Design

JIT code maintains **two stacks simultaneously**:

| Stack | Register | Location | Purpose |
|-------|----------|----------|---------|
| Go stack | RSP | Goroutine stack | Trampoline frame, gocall resume slot, Go callbacks |
| Sandbox stack | R12 | mmap'd memory with guard pages | JIT-internal function calls between blocks, temporaries |

### Why two stacks

- **Containment**: JIT code's own stack operations stay in sandbox memory.
  JIT never writes to RSP-relative addresses except `[RSP+0]` (resume slot).
- **Depth**: Sandbox stack can be 512KB+ (mmap'd). Go stack's 65KB frame
  is just for callbacks and runtime safety.
- **Control**: Since we generate all JIT code, we choose which "stack pointer"
  each instruction uses. Guest RISC-V SP (x2) is a third, completely
  separate address space (guest virtual addresses in GuestMemory).

### RSP rules

JIT code may ONLY touch RSP in one way: `MOV [RSP], R10` to store the gocall
resume address. All other stack-like operations use R12 (sandbox stack) or
direct CPU struct offsets via R9.

---

## 2. Register Convention (verified)

### Reserved registers (NOT in allocation pool)

| Register | Role | Verified |
|----------|------|----------|
| R14 | Go goroutine pointer (`g`) | Must never touch — Go runtime crashes |
| R9  | CPU state pointer (pinned) | Restored by gocall handler ✓ |
| R12 | Sandbox stack pointer | Survives callbacks (callee-saved) ✓ |
| RSP | Go stack pointer | Only `[RSP+0]` for gocall resume |
| RAX | Staging register A | Clobbered by callbacks |
| RCX | Staging register B | Clobbered by callbacks |

### Allocation pool: 8 integer registers

**Callee-saved** (survive Go callbacks — verified):
- RBX, RBP, R13, R15 = 4 registers

**Caller-saved** (clobbered by Go callbacks — must spill/reload):
- RDX, RSI, RDI, R8 = 4 registers

**Total: 8 allocatable** (down from 12 in current rv8 pool; lost R14/R9).

R10/R11 usable between callbacks (10 total), but excluded from pool
initially for simplicity. Can promote in Phase 3.

### Pinned registers

- RBP → VRRegFile (register file base, same as current rv8)
- R9 → CPU state pointer (loaded by trampoline, restored by gocall)
- R12 → sandbox stack pointer (loaded by trampoline)

### FP registers: unchanged from rv8

XMM0-XMM13 pool, XMM14-XMM15 staging.

---

## 3. Trampoline (implemented, verified)

File: `~/ris/abjit/trampoline_amd64.s`

```asm
TEXT ·callJIT(SB), 0, $65528-24
    NO_LOCAL_POINTERS
    [Go prologue: stack check, PUSH BP, SUB SP 0xFFF8]
    save RBX, RBP, R12, R13, R15 at SP+8..40
    load R9 = cpuState, R12 = sandboxSP, AX = code
    JMP AX
gocall:
    CALL R10
    reload R9 from cpuState+8(FP)
    JMP (SP)
```

### Callback emit sequence (34 bytes, verified):
```asm
MOVABS gocallAddr, R11     ; 10B
LEA    R10, [RIP+17]       ;  7B — resume point
MOV    [RSP], R10          ;  4B
MOVABS goFuncAddr, R10     ; 10B
JMP    R11                 ;  3B
; <resume here>
```

---

## 4. Implementation Phases (detailed)

### Phase 1: Full package structure + benchmarks

**Files to create:**

`~/ris/abjit/emit.go` — x86_64 emitter (promote codeBuilder from test):
- `type CodeBuilder struct` with exported API
- `Movabs(reg, imm)`, `StoreToR9(reg, disp)`, `Callback(goFunc)`, `Exit()`
- `LoadFromR9(reg, disp)` for loading guest registers
- `Add/Sub/Cmp` on registers
- `Addr() uintptr` — returns code page base address

`~/ris/abjit/abjit.go` — public API:
- `type State struct` wrapping CPU state fields (heap-allocated)
- `func NewState() *State`
- `func Run(code *CodeBuilder, state *State)`
- Memory callback functions (`//go:nosplit`)

**Tests:**
- `TestAddInstruction` — emit ADD, verify result in state
- `TestLoadStore` — emit load from state, compute, store back
- `BenchmarkTrampolineOverhead` (already done: 2.8ns)
- `BenchmarkCallbackRoundTrip` (already done: 4.0ns)
- `BenchmarkVsCGO` — side-by-side with bridge2 CGO benchmark

### Phase 2: Memory callbacks + sandbox stack

- Allocate sandbox stack via mmap with guard pages
- Pass sandbox stack top as third arg to callJIT
- `goLoad8/16/32/64(addr)` and `goStore8/16/32/64(addr, val)` callbacks
- Test: JIT code calls goLoad64, verifies returned value
- Test: JIT code uses R12 for push/pop operations on sandbox stack
- Benchmark: callback cost for memory operations

### Phase 3: Register pool integration with IR pipeline

- Add `ABJITPool()` to `~/ris/ir/lower_amd64.go`:
  ```go
  func ABJITPool(_ *Block) RegPool {
      intRegs := []int16{
          REG_AMD64_DX, REG_AMD64_BX, REG_AMD64_SI, REG_AMD64_DI,
          REG_AMD64_R8, REG_AMD64_R13, REG_AMD64_R15, REG_AMD64_BP,
      }
      // same FP pool as RV8
  }
  ```
- Add `ABJITPinned()` — pin RBP to VRRegFile
- New lowering pass: `LowerAMD64_ABJIT` that emits callback sequences
  for memory access instead of inline mask-based loads/stores
- Spill/reload logic around callbacks for caller-saved registers
- Test: `TestR14NeverUsed` — verify generated code never touches R14
- Benchmark: full RISC-V block execution via abjit vs existing jitcall

---

## 5. Files (current state)

| File | Status | Purpose |
|------|--------|---------|
| `~/ris/abjit/trampoline_amd64.s` | ✅ done | Assembly trampoline |
| `~/ris/abjit/trampoline.go` | ✅ done | Go stubs |
| `~/ris/abjit/callfunc.go` | ✅ done | getCallAddr, funcAddr |
| `~/ris/abjit/mmap_unix.go` | ✅ done | Executable memory |
| `~/ris/abjit/abjit_test.go` | ✅ done | Phase 0 tests (11 pass) |
| `~/ris/abjit/emit.go` | Phase 1 | Code emitter |
| `~/ris/abjit/abjit.go` | Phase 1 | State struct, public API |

## 6. Files to modify (Phase 3 only)

| File | Change |
|------|--------|
| `~/ris/ir/lower_amd64.go` | Add `ABJITPool()` and `ABJITPinned()` |

---

## 7. What stays unchanged

- `internal/jitcall/` trampoline and Call/CallAOT
- `ir/` pipeline (RV8Pool, RV8Pinned, lower_amd64_rv8.go)
- `jit.go` RunJIT dispatch loop
- TCC/C code generation path
- AOT segment management
- bridge2 SPSC approach
