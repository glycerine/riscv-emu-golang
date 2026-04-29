# Wazero Architecture: How It Achieves Near-Native Performance

## Context

Our RISC-V emulator's JIT (via TCC) achieves ~4100 MIPS — roughly 20% of native (21041 MIPS). The libriscv JIT is similar at ~4235 MIPS. Both use TCC to compile C source to native code. Meanwhile, wazero — a pure-Go WebAssembly runtime — achieves 19336 MIPS (~92% of native) on the same benchmark. This document analyzes wazero's architecture to understand why it's 4-5x faster than TCC-based JITs and what techniques could be applied to our emulator.

---

## 1. Overview: Two-Engine Architecture

Wazero ships two execution engines:

- **Interpreter** (`internal/engine/interpreter/`): Pure Go, stack-based, runs everywhere. Analogous to our `step()` loop.
- **wazevo AOT compiler** (`internal/engine/wazevo/`): A proper SSA-based optimizing compiler that emits native x86-64 or ARM64 machine code directly — no intermediate C, no TCC, no LLVM, no CGO. This is the engine that achieves 92% of native.

Platform detection (`internal/platform/`) auto-selects the compiler on supported targets (linux/darwin/freebsd on amd64 with SSE4.1; arm64 on linux/darwin/freebsd/netbsd/windows).

**Key insight #1:** The performance gap is not Go vs C. It's "proper optimizing compiler" vs "template-based C emission compiled by an unoptimizing compiler (TCC)."

---

## 2. The Compilation Pipeline: Three Stages

Wazevo uses a classic compiler architecture — nearly identical in structure to LLVM but specialized for WebAssembly:

```
Wasm bytecode
     │
     ▼
┌─────────────────────────────────┐
│  Stage 1: FRONTEND              │
│  Wasm bytecodes → SSA IR        │
│  (internal/engine/wazevo/       │
│   frontend/)                    │
│                                 │
│  • Stack-to-SSA conversion      │
│  • Bounds check analysis        │
│  • Dead code elimination        │
│  • Redundant PHI elimination    │
└────────────┬────────────────────┘
             │ SSA IR (architecture-independent)
             ▼
┌─────────────────────────────────┐
│  Stage 2: BACKEND               │
│  SSA IR → Machine IR            │
│  (internal/engine/wazevo/       │
│   backend/)                     │
│                                 │
│  • Instruction selection        │
│  • Address mode synthesis       │
│  • Register allocation          │
│  • Instruction scheduling       │
└────────────┬────────────────────┘
             │ Machine instructions
             ▼
┌─────────────────────────────────┐
│  Stage 3: ISA ENCODING          │
│  Machine IR → bytes             │
│  (backend/isa/amd64/ or arm64/) │
│                                 │
│  • Binary instruction encoding  │
│  • Relocation resolution        │
│  • Executable memory emission   │
└────────────┬────────────────────┘
             │ executable []byte (mmap'd RX)
             ▼
        Native execution
```

### 2.1 Stage 1: Frontend (Wasm → SSA IR)

**SSA IR Design:**
- Uses "block argument" SSA variant (more efficient than traditional PHI nodes)
- `Value`: 64-bit entity — lower 32 bits = ID, bits 32-59 = instruction ID, bits 60-63 = type
- `Instruction`: Flat union type with opcode + up to 3 operands + additional data
- `BasicBlock`: Contains parameters (block arguments) instead of PHI nodes
- Supported types: I32, I64, F32, F64, V128 (SIMD)

**Wasm stack → SSA conversion:**
The frontend maintains a conceptual Wasm value stack. Stack operations become SSA operations:
- `i32.const 5` → `v1 = Iconst(5)` (push SSA value)
- `i32.add` → pop v1,v2; `v3 = Iadd(v1,v2)` (push v3)
- `block`/`loop`/`if` → create SSA BasicBlocks; push control frames
- `br` → jump with block arguments (for values that cross the branch)

**Optimization passes:**
1. Dead block elimination (unreachable code)
2. Redundant PHI elimination (coalesce identical block arguments)
3. Dead code elimination (unused instructions)
4. Block layout in reverse post-order (cache-friendly)
5. Loop nesting forest construction (for future loop optimizations)

Instructions carry an `InstructionGroupID` — instructions in the same group have no intervening side effects and can be safely reordered or merged.

### 2.2 Stage 2: Backend (SSA → Machine IR)

**Lowering (`Lower()`):**
Pattern-matches SSA instructions into ISA-specific machine instructions. This is where the real optimization happens:

- **Address mode synthesis**: Recognizes patterns like `Iadd(Ishl(base, 2), offset)` and lowers to a single `[base*4 + offset]` x86-64 addressing mode. Multiple SSA instructions collapse into one machine instruction.
- **Instruction merging**: When multiple SSA instructions can be represented by one machine instruction, marks predecessors with `MarkLowered()` so they don't emit redundant code.
- **Constant folding / strength reduction**: Applied during lowering.

**Register Allocation (`RegAlloc()`):**
Chaitin's algorithm with liveness analysis:
- One `VReg` (virtual register) per SSA Value
- Liveness computed at instruction-level granularity
- Interference graph built; physical registers assigned
- Spills to stack when pressure exceeds available registers
- **Register preference**: Caller-saved registers preferred (cheaper than callee-saved save/restore)

**AMD64 register assignment:**
- 14 allocatable integer registers: rax, rcx, rdx, rbx, rsi, rdi, r8-r15
- 16 allocatable float/SIMD registers: xmm0-xmm15
- rbp = frame pointer, rsp = stack pointer (reserved)
- Caller-saved: rax, rcx, rbx, rsi, rdi, r8-r11, xmm0-7
- Callee-saved: rdx, r12-r15, xmm8-15

### 2.3 Stage 3: ISA Encoding

Translates machine IR to raw bytes. For AMD64 this means encoding ModR/M, SIB, REX prefixes, etc. Each instruction type has its own encoder. The final output is a `[]byte` of native machine code.

---

## 3. Memory Management and Executable Code

### 3.1 Code Allocation

```
platform.MmapCodeSegment(size)  // mmap(PROT_READ|PROT_WRITE, MAP_ANON|MAP_PRIVATE)
copy(executable[offset:], functionBody)  // copy compiled code
resolveRelocations(executable)            // fix up call targets
platform.MprotectCodeSegment(executable)  // mprotect(PROT_READ|PROT_EXEC)
```

On Linux, wazero tries huge pages first (2MB/1GB) and falls back to 4KB pages. All functions within a module are packed into a single contiguous executable segment.

Go finalizers handle cleanup (`MunmapCodeSegment`), so compiled modules are GC-friendly.

### 3.2 Compilation Caching

Two-level cache:
1. **In-memory**: `map[wasm.ModuleID]*compiledModule` (guarded by `sync.RWMutex`)
2. **File cache**: SHA256(moduleID + magic + cpuFeatures) → serialized compiled code on disk

Cache key includes CPU feature flags, so recompilation happens when hardware differs.

### 3.3 Parallel Compilation

Functions within a module are compiled in parallel using worker goroutines. Each worker gets its own `Machine`, `SSABuilder`, and `backend.Compiler` instances — no shared mutable state during compilation.

---

## 4. Zero-CGO Native Code Execution

**This is the single most critical architectural decision.** Wazero calls native code from Go without CGO, eliminating the ~200ns CGO call overhead entirely.

### 4.1 The Entry Point Mechanism

Go assembly file (`abi_entry_amd64.s`):
```asm
TEXT ·entrypoint(SB), NOSPLIT|NOFRAME, $0-48
    MOVQ preambleExecutable+0(FP), R11    // preamble code pointer
    MOVQ functionExecutable+8(FP), R14     // function code pointer
    MOVQ executionContextPtr+16(FP), AX    // execution context
    MOVQ moduleContextPtr+24(FP), BX       // module context
    MOVQ paramResultSlicePtr+32(FP), R12   // args/results buffer
    MOVQ goAllocatedStackSlicePtr+40(FP), R13  // wasm stack
    JMP  R11   // jump to preamble
```

Key flags: `NOSPLIT|NOFRAME` — Go runtime won't preempt this function or insert stack checks. The `JMP R11` transfers control to dynamically-generated machine code.

The Go-side declaration uses `//go:linkname` to bind:
```go
//go:linkname entrypoint github.com/.../amd64.entrypoint
func entrypoint(preambleExecutable, functionExecutable *byte,
    executionContextPtr uintptr, moduleContextPtr *byte,
    paramResultStackPtr *uint64, goAllocatedStackSlicePtr uintptr)
```

### 4.2 The Entry Preamble

Each function signature gets a generated **preamble** — a machine code thunk that:
1. Saves Go's RSP and RBP into `executionContext`
2. Switches RSP to the Go-allocated wasm stack
3. Copies arguments from the param/result slice into registers per the calling convention
4. Zeros RBP (wasm stack unwinding marker)
5. `CALL` to the actual compiled wasm function
6. On return: copies results back to the param/result slice
7. Restores Go's RSP/RBP
8. `RET` back to Go

### 4.3 The Dispatch Loop

When native code needs Go runtime services (memory growth, host function calls, etc.), it:
1. Writes an exit code to `executionContext.exitCode`
2. Saves its stack pointer in `executionContext`
3. Returns to Go via `afterGoFunctionCallEntrypoint`

Go's dispatch loop (`callEngine.callWithStack`) handles 30+ exit codes:

```go
for {
    switch ec := c.execCtx.exitCode & ExitCodeMask {
    case ExitCodeOK:           return nil
    case ExitCodeGrowStack:    newsp, newfp = c.growStack(); resume(...)
    case ExitCodeGrowMemory:   mem.Grow(pages); resume(...)
    case ExitCodeCallGoFunc:   hostFunc.Call(ctx, args); resume(...)
    case ExitCodeTableOutOfBounds:  return ErrRuntimeInvalidTableAccess
    // ... etc
    }
}
```

After handling, Go calls `afterGoFunctionCallEntrypoint` to resume native execution at the saved return address.

### 4.4 Why This Is Fast

| Factor | CGO approach (our JIT) | Wazero approach |
|--------|----------------------|-----------------|
| Call overhead | ~200ns per CGO call (save/restore all regs, signal mask, etc.) | ~5ns (plain function call + JMP) |
| Stack switch | C stack ↔ Go stack via runtime | Direct RSP swap in preamble |
| Register save | CGO saves ALL registers | Only callee-saved; caller-saved live across calls only |
| Preemption | CGO boundary is a preemption point | NOSPLIT — no preemption in hot path |
| Memory access | Through function args/struct | Direct register access to execution context |

---

## 5. Bounds Check Elimination — The Other Big Win

WebAssembly requires bounds checking on every memory access. Naively, this doubles the instruction count for memory-heavy code. Wazero's frontend performs aggressive bounds check analysis:

### 5.1 Known-Safe-Bound Tracking

Each SSA `Value` that represents a memory address carries a `knownSafeBound` — the maximum offset that has already been proven in-bounds for this value.

```
// Conceptual:
v1 = load(memBase + offset, size=4)   // bounds check: memLen >= offset + 4
                                       // v1.knownSafeBound = offset + 4
v2 = load(memBase + offset + 2, size=2)  // offset+2+2 = offset+4 <= knownSafeBound
                                          // BOUNDS CHECK ELIMINATED
```

### 5.2 Cross-Block Propagation

At block merge points, the safe bound is the *minimum* across all predecessors (intersection). Within a straight-line block, bounds increase monotonically.

### 5.3 Invalidation

After any function call, all `knownSafeBound` values reset — the callee might have grown memory, changing the base pointer and length.

### 5.4 Impact

In memory-heavy loops (the typical case for benchmarks), a single bounds check at loop entry can prove all accesses within the loop body safe. This alone can account for a 1.5-2x speedup over naive bounds checking.

---

## 6. Execution Context and Module Context

### 6.1 Execution Context

A Go-allocated struct shared with native code at known offsets:

```go
type executionContext struct {
    exitCode                    ExitCode   // how/why native code returned to Go
    callerModuleContextPtr      *byte      // for nested cross-module calls
    originalFramePointer        uintptr    // Go's RBP (to restore on exit)
    originalStackPointer        uintptr    // Go's RSP (to restore on exit)
    goReturnAddress             uintptr    // where to resume in Go
    stackBottomPtr              *byte      // wasm stack bounds (for growth)
    goCallReturnAddress         *byte      // resume point after Go function
    stackPointerBeforeGoCall    *uint64    // saved SP when calling Go
    stackGrowRequiredSize       uintptr    // how much stack to grow
    // Trampoline addresses (shared across all calls):
    memoryGrowTrampolineAddress              *byte
    stackGrowCallTrampolineAddress           *byte
    tableGrowTrampolineAddress               *byte
    // ... 10+ more trampoline addresses
    savedRegisters              [64][2]uint64  // callee-saved register spill area
}
```

Native code accesses fields at compile-time-known offsets (e.g., `exitCode` at offset 0, `originalFramePointer` at offset 24, etc.).

### 6.2 Module Context

An opaque byte buffer containing per-module-instance state, accessed by native code via offsets:
- `moduleInstance` pointer
- Local memory base pointer and length (cached — reloaded after any call)
- Imported memory instance pointer
- Imported function table (executable pointers + module context pointers)
- Global variable storage (if engine owns globals)
- Type ID table (for indirect call type checking)
- Table instance pointers

The offset layout is computed at compile time by `wazevoapi.NewModuleContextOffsetData` based on the module's import/export structure.

---

## 7. Function Call Mechanics

### 7.1 Direct Wasm-to-Wasm Calls

Compiled as direct `CALL` instructions to the target function's offset within the executable segment. Arguments passed in registers per the wazevo calling convention.

### 7.2 Indirect Calls

```
1. Load function pointer from table[index]
2. Null check (exit ExitCodeIndirectCallNullPointer if null)
3. Type check: load actualTypeID, compare to expectedTypeID
   (exit ExitCodeIndirectCallTypeMismatch on mismatch)
4. Load executable pointer and module context from function instance
5. CALL indirect through executable pointer
```

### 7.3 Host (Go) Function Calls

Native code uses a **Go function trampoline** — a per-signature machine code thunk that:
1. Marshals wasm registers into a stack-based format
2. Sets exit code to `ExitCodeCallGoFunction | (funcIndex << 8)`
3. Stores stack pointer in execution context
4. Returns to Go dispatch loop
5. (Go calls the host function)
6. After return: restores registers, loads results, resumes

---

## 8. What Makes Wazero 4-5x Faster Than TCC-Based JITs

### 8.1 No CGO Overhead (est. 10-30% of the gap)

Our JIT pays ~200ns per CGO call to invoke TCC-compiled blocks. For small blocks (which dominate), this overhead is significant. Wazero's Go-assembly trampoline costs ~5ns.

### 8.2 Proper Register Allocation (est. 30-40% of the gap)

TCC performs almost no register allocation — it's essentially a one-pass compiler that loads/stores through memory for every operation. Wazero's Chaitin allocator keeps values in registers across entire basic blocks and beyond, dramatically reducing memory traffic.

**Example — `a + b + c`:**
- TCC: load a → reg, load b → reg, add → reg, store → mem, load → reg, load c → reg, add → reg, store → mem (8 memory ops)
- Wazero: load a → r1, load b → r2, add r1,r2 → r1, load c → r3, add r1,r3 → r1 (3 memory ops)

### 8.3 Instruction Selection and Address Mode Synthesis (est. 15-25% of the gap)

Wazero recognizes patterns like `base + index*scale + offset` and emits single x86-64 instructions with complex addressing modes. TCC compiles C source literally — `ptr[i]` becomes separate shift, add, and load instructions.

### 8.4 Bounds Check Elimination (est. 10-20% of the gap)

TCC-compiled code checks bounds on every memory access. Wazero's SSA-level analysis eliminates redundant checks, often proving entire loop bodies safe with a single check at the loop header.

### 8.5 Block Layout and Branch Optimization (est. 5-10% of the gap)

Wazero lays out blocks in reverse post-order for optimal branch prediction. Fall-through paths are the common case. TCC has no such optimization.

### 8.6 No C Compilation Step

TCC must parse C source, build an AST, and generate code. Wazero goes directly from Wasm bytecode to SSA to machine code — no intermediate text format. This doesn't affect runtime performance but makes compilation faster.

---

## 9. Lessons for Our RISC-V Emulator

### 9.1 The Core Problem

We emit C source and compile with TCC. TCC is a **non-optimizing** compiler. It produces correct code, but:
- No register allocation (everything goes through stack)
- No instruction scheduling
- No address mode optimization
- No cross-instruction analysis

### 9.2 What Wazero's Architecture Tells Us

To reach near-native performance, we would need to either:

**Option A: Replace TCC with an optimizing C compiler (GCC/Clang -O2)**
- Pro: Minimal architectural change
- Con: Compilation latency jumps from ~1ms to ~100ms per block
- Con: Still have CGO overhead per block invocation
- Con: GCC/Clang are huge dependencies

**Option B: Emit machine code directly (the wazero approach)**
- Build an SSA-based compiler backend in Go
- Emit x86-64 (and optionally ARM64) machine code directly
- Use Go assembly trampolines to call compiled code (no CGO)
- Pro: Near-native performance achievable
- Con: Massive engineering effort (wazevo is ~30K+ lines of compiler code)
- Con: Per-ISA backend needed for each target

**Option C: Emit machine code directly but without SSA — a "macro assembler" approach**
- Write a Go-native x86-64 assembler (or use an existing one)
- Translate RISC-V instructions to x86-64 instruction sequences with a fixed register mapping
- Apply a few key optimizations: register pinning, bounds check hoisting, branch threading
- Pro: Much less code than a full SSA compiler (~5-10K lines)
- Con: Won't match full SSA quality, but can close most of the gap
- This is essentially what rv8 does, and it achieves ~4000 MIPS — so the register mapping alone isn't enough

**Option D: Use wazero itself as the backend**
- Compile RISC-V → Wasm → native via wazero
- Pro: Get wazero's optimizations for free
- Con: Double translation overhead
- Con: Wasm's stack machine model may not map efficiently from RISC-V's register machine
- Worth benchmarking — if wasm overhead is low, this could be the easiest path to 15000+ MIPS

**Option E: Use Go's own compiler backend**
- Emit Go SSA or even Go source, compile with `go build`
- Pro: Leverage Go's own optimizing compiler
- Con: Go compilation is slow (~seconds per package)
- Con: Not suitable for JIT (too slow to compile at runtime)
- Could work for AOT compilation of known ELFs

### 9.3 Quick Wins (applicable regardless of approach)

1. **Eliminate CGO overhead**: Use Go assembly trampolines like wazero's `entrypoint`. Even with TCC-compiled code, avoiding CGO per-block saves 10-30%.

2. **Larger compilation regions**: Our current 2048-PC / 16KB limit means many small blocks. Larger regions give the compiler (even TCC) more to work with and amortize call overhead.

3. **Pin guest registers to host registers**: Even in C emission, we could use `register` keyword hints or TCC-specific extensions to encourage register allocation.

4. **Hoist bounds checks out of loops**: Analyze loop bounds at JIT time and emit a single check before the loop.

---

## 10. Key Files in Wazero (for reference)

| Area | Path |
|------|------|
| Runtime entry | `runtime.go`, `config.go` |
| Engine selection | `internal/platform/platform.go` |
| SSA IR | `internal/engine/wazevo/ssa/` |
| Frontend | `internal/engine/wazevo/frontend/frontend.go`, `lower.go` |
| Backend | `internal/engine/wazevo/backend/compiler.go` |
| AMD64 ISA | `internal/engine/wazevo/backend/isa/amd64/` |
| Entry trampoline | `backend/isa/amd64/abi_entry_amd64.s` |
| Preamble gen | `backend/isa/amd64/abi_entry_preamble.go` |
| Go call trampoline | `backend/isa/amd64/abi_go_call.go` |
| Dispatch loop | `internal/engine/wazevo/call_engine.go` |
| Memory mapping | `internal/platform/mmap_linux.go` |
| Bounds check elim | `internal/engine/wazevo/frontend/lower.go` (knownSafeBound) |
| Compilation cache | `internal/engine/wazevo/engine_cache.go` |
| Module context | `internal/engine/wazevo/module_engine.go` |

---

## Summary

Wazero achieves 92% of native speed through five reinforcing techniques:

1. **Proper SSA-based optimizing compiler** — not a template emitter
2. **Zero-CGO native code invocation** — Go assembly trampoline, ~5ns overhead
3. **Aggressive bounds check elimination** — SSA-level analysis proves most checks redundant
4. **Full register allocation** — Chaitin's algorithm keeps values in registers
5. **Instruction selection with address mode synthesis** — multiple SSA ops → one x86-64 instruction

The gap between our 4100 MIPS and wazero's 19300 MIPS is almost entirely explained by TCC's lack of optimization + CGO call overhead. Closing it requires either replacing TCC with an optimizing backend or emitting machine code directly.
