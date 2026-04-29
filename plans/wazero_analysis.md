# Wazero Architecture: How It Achieves Near-Native Performance

## Context

Our RISC-V emulator's abjit JIT achieves ~4100 MIPS — roughly 20% of native (21041 MIPS). The libriscv JIT (TCC-based) is similar at ~4235 MIPS. Meanwhile, wazero — a pure-Go WebAssembly runtime — achieves 19336 MIPS (~92% of native) on the same benchmark. This document analyzes wazero's architecture, accurately compares it to our abjit architecture, and assesses the feasibility of adapting wazero's backend for RISC-V.

---

## 1. Wazero Overview: Two-Engine Architecture

Wazero ships two execution engines:

- **Interpreter** (`internal/engine/interpreter/`): Pure Go, stack-based, runs everywhere.
- **wazevo AOT compiler** (`internal/engine/wazevo/`): A proper SSA-based optimizing compiler that emits native x86-64 or ARM64 machine code directly — no intermediate C, no TCC, no LLVM, no CGO. This is the engine that achieves 92% of native.

Platform detection (`internal/platform/`) auto-selects the compiler on supported targets (linux/darwin/freebsd on amd64 with SSE4.1; arm64 on linux/darwin/freebsd/netbsd/windows).

---

## 2. The Wazevo Compilation Pipeline

```
Wasm bytecode
     │
     ▼
┌─────────────────────────────────┐
│  Stage 1: FRONTEND              │
│  Wasm bytecodes → SSA IR        │
│  (frontend/)                    │
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
│  (backend/)                     │
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

### 2.1 Frontend (Wasm → SSA IR)

- Uses "block argument" SSA variant (more efficient than traditional PHI nodes)
- `Value`: 64-bit entity — ID + instruction ID + type packed into 64 bits
- `Instruction`: Flat union with opcode + up to 3 operands + additional data
- 110+ SSA opcodes, **all general-purpose** — no Wasm-specific opcodes in the IR
- Optimization passes: dead block elimination, redundant PHI elimination, dead code elimination, reverse post-order block layout, loop nesting forest construction

### 2.2 Backend (SSA → Machine IR)

**Instruction selection via pattern matching:**
- Recognizes `Iadd(Ishl(base, 2), offset)` → single `[base*4 + offset]` x86-64 addressing mode
- Multiple SSA instructions collapse into one machine instruction via `MarkLowered()`
- Constant folding and strength reduction during lowering

**Register allocation — Chaitin's algorithm:**
- Interference graph with liveness analysis at instruction-level granularity
- 14 allocatable integer registers, 16 float/SIMD registers on AMD64
- Spills to stack with locality-aware slot assignment
- Caller-saved registers preferred (cheaper than callee-saved save/restore)

### 2.3 Bounds Check Elimination

Each SSA `Value` representing a memory address carries a `knownSafeBound`. If a subsequent access falls within the already-proven range, the bounds check is eliminated entirely. At block merge points, safe bounds take the minimum across predecessors. After any function call, bounds reset (callee may grow memory). In memory-heavy loops, one check at the loop header can prove the entire body safe — a 1.5-2x speedup over naive checking.

---

## 3. Wazero's Zero-CGO Native Code Execution

### 3.1 The Entry Point

Go assembly (`abi_entry_amd64.s`), marked `NOSPLIT|NOFRAME`:
```asm
TEXT ·entrypoint(SB), NOSPLIT|NOFRAME, $0-48
    MOVQ preambleExecutable+0(FP), R11
    MOVQ functionExecutable+8(FP), R14
    MOVQ executionContextPtr+16(FP), AX
    MOVQ moduleContextPtr+24(FP), BX
    MOVQ paramResultSlicePtr+32(FP), R12
    MOVQ goAllocatedStackSlicePtr+40(FP), R13
    JMP  R11   // jump to generated preamble
```

### 3.2 The Entry Preamble (generated per-signature)

1. Save Go's RSP/RBP into `executionContext`
2. Switch RSP to Go-allocated wasm stack
3. Copy arguments from param slice into registers per calling convention
4. Zero RBP (stack unwinding marker)
5. `CALL` the compiled function
6. Copy results back to param slice
7. Restore Go's RSP/RBP; `RET`

### 3.3 The Dispatch Loop

When native code needs Go services, it writes an exit code to `executionContext.exitCode` and returns to Go. The Go dispatch loop handles 30+ exit codes (memory growth, host function calls, stack growth, exceptions, etc.), then calls `afterGoFunctionCallEntrypoint` to resume native execution.

---

## 4. Our abjit Architecture — Accurate Description

### 4.1 What abjit Actually Is

Our JIT uses **NO CGO and NO TCC**. It is a Go-native JIT that:
- Emits x86-64 machine code directly (raw bytes, no C intermediate)
- Uses a Go assembly trampoline (`abjit/trampoline_amd64.s`) — 2.8ns overhead
- Has an IR intermediate representation with 75+ opcodes
- Uses fixed static register allocation (priority-based, not liveness-based)
- Supports Go callbacks from JIT code (4.0ns round-trip)

### 4.2 The abjit Trampoline

Go assembly entry (`trampoline_amd64.s`):
```asm
TEXT ·callJIT(SB), NOSPLIT|NOFRAME, $0
    // Go prologue: PUSH BP; MOV BP,SP; SUB SP,0xFFF8 (65KB frame)
    // Save callee-saved: RBX, RBP, R12, R13, R15
    // Load RBP = regFileBase (State pointer)
    // Load R15 = State.IC
    // JMP RAX (= native code address)
```

**Go callback from JIT code:**
```asm
MOVABS R11, <gocallAddr>     ; 10 bytes
LEA    R10, [RIP+17]         ; 7 bytes  (resume point)
MOV    [RSP], R10            ; 4 bytes  (store resume at [RSP+0])
MOVABS R10, <goFunc>         ; 10 bytes
JMP    R11                   ; → gocall: CALL R10; JMP (SP)
```

**GC safety invariant**: JIT code never emits `RET`. Instead it jumps to `retStubAddr` — a known Go code address — because Go's GC scans return addresses on the stack and panics if one points into mmap'd memory.

### 4.3 Register Mapping

**Pinned registers (outside allocator):**
- RBP → `abjit.State` base (guest register file pointer)
- R14 → Go goroutine pointer `g` (must never touch)
- R15 → Instruction counter (relative per-block)
- R12 → Reserved (future sandbox stack)
- RAX/RCX → Staging registers (scratch, used by lowerer)

**Allocatable pool (11 integer, 14 FP):**
- Integer: RBX, RDX, RSI, RDI, R8, R9, R10, R11, R13 + 2 more
- FP: XMM0–XMM13

**Fixed static priority mapping** — RISC-V registers x0–x31 are assigned to host registers by a static priority table (most-used first: ra, sp, t0, t1, a0–a7, ...). Registers beyond pool size spill to stack. No liveness analysis — every allocated register is live for the entire block.

### 4.4 IR and Compilation Pipeline

```
RISC-V instructions
     │ (jit_emit_ir.go — decode + emit IR)
     ▼
IR Block (75+ opcodes: IRLoad, IRStore, IRAdd, IRBranch, IRChainExit, ...)
     │
     ▼ Peephole pass (4-instruction window, ~10 patterns)
     │
     ▼ Fixed static register allocation (regalloc_fixed.go)
     │
     ▼ Lowering to x86-64 (lower_amd64_abjit.go)
     │
     ▼ Assembly (goasm.Ctx.Assemble() → bytes)
     │
     ▼ mmap + sentinel backpatching
     │
     ▼ compiledBlock {fn, chainExits[], jalrICs[]}
```

### 4.5 Block Chaining and Inline Caches

- **Chain patching**: When block A branches to block B, the MOVABS sentinel in A is patched to jump directly to B's entry — eliminating the Go dispatch loop on subsequent executions.
- **JALR inline cache**: 2-slot shift-policy IC. On miss, slot 0 → slot 1, new target → slot 0. After 16 misses, the site is deopted (polymorphic).
- **Decoder cache**: Per-segment read-only array indexed by `(pc - vAddrBegin) >> 1` for fast block lookup.

### 4.6 Memory Model

- `GuestMemory`: Power-of-two mmap slab, mask-based bounds checking
- All access: `hostPtr = base + (addr & mask)` (one AND, one ADD)
- Alignment check inline; misaligned access falls back to byte-by-byte
- Faults exit to Go dispatcher, which re-executes via interpreter

### 4.7 What Our IR Has

- 75+ opcodes covering integer, FP, memory, control flow, syscall, JALR-IC, chain exits
- Dedicated opcodes for budget/IC management (IRMemBudget, IRIncIC, etc.)
- IRSyscall with hot-path inline dispatch
- IRJalrIC with decoder-cache + 2-slot fallback

### 4.8 What Our IR Does NOT Have

- **No SSA form** — IR is linear, imperative, register-machine style
- **No global optimization passes** — no constant folding, no DCE, no CSE, no LICM
- **No instruction scheduling**
- **No address mode synthesis** — `base + index*scale + offset` patterns not recognized
- **No liveness-based register allocation** — every VReg lives for the full block
- **No cross-block optimization** — each block compiled independently
- **Peephole only** — 4-instruction window, ~10 hand-written patterns

---

## 5. Head-to-Head Comparison

| Feature | Wazero (wazevo) | Our abjit |
|---------|----------------|-----------|
| **MIPS achieved** | 19,336 (92% native) | 4,103 (20% native) |
| **CGO?** | No | No |
| **Trampoline** | Go asm, ~5ns | Go asm, ~2.8ns |
| **IR form** | SSA (block arguments) | Linear/imperative |
| **IR opcodes** | 110+ general-purpose | 75+ (RISC-V-specific) |
| **Optimization passes** | DCE, dead block elim, redundant PHI elim, block layout | Peephole only (4-insn window) |
| **Register allocator** | Chaitin (interference graph + liveness) | Fixed static priority (no liveness) |
| **Allocatable int regs** | 14 | 11 |
| **Instruction selection** | Pattern-matching with address mode synthesis | Direct per-op translation |
| **Bounds check elim** | SSA-level knownSafeBound analysis | None (every access checked) |
| **Block size** | Entire Wasm functions | 50–100 RISC-V instructions |
| **Cross-block optimization** | Yes (SSA spans function) | No (each block independent) |
| **Block chaining** | N/A (AOT compiles whole function) | Runtime patching of MOVABS targets |
| **Compilation model** | AOT (ahead of time, whole module) | JIT (lazy, per-block on first hit) |
| **Code caching** | In-memory + file cache (SHA256-keyed) | In-memory block cache (direct-mapped) |
| **Parallel compilation** | Yes (worker goroutines per function) | No |

---

## 6. Where the 5x Performance Gap Comes From

Both systems use Go assembly trampolines with similar overhead (~3-5ns). The gap is in **code quality**, not calling convention.

### 6.1 Register Allocation Quality (est. 35-45% of the gap)

**Wazero**: Chaitin's algorithm with liveness analysis. Values stay in registers across their entire live range. Spills only when register pressure exceeds 14 integer / 16 float registers.

**abjit**: Fixed static priority mapping. Every allocated VReg is assumed live for the entire block (`Start=0, End=N-1`). This means:
- Registers that are written once and read once still occupy a register for the whole block
- No register reuse within a block (a temporary used at instruction 3 blocks a register through instruction 99)
- More spills than necessary when >11 guest registers are active

**Impact**: For a block with 50 RISC-V instructions touching 15 guest registers, abjit must spill 4+ registers to stack. Wazero would likely keep all values in registers (many are short-lived temporaries).

### 6.2 Instruction Selection and Address Modes (est. 15-25% of the gap)

**Wazero**: Pattern-matches across multiple SSA instructions. `base + index*4 + offset` becomes one x86-64 instruction with a complex addressing mode. Multiple SSA ops collapse into one machine instruction.

**abjit**: Direct per-op translation. Each IR instruction becomes one or more x86-64 instructions. `IRLoad` and `IRLoadX` emit simple addressing but don't merge surrounding address arithmetic.

**Impact**: Wazero emits fewer instructions for the same computation, reducing both code size and execution time.

### 6.3 Compilation Unit Size (est. 10-20% of the gap)

**Wazero**: Compiles entire Wasm functions at once (potentially hundreds of instructions). The SSA builder sees the complete control flow graph — all blocks, all branches, all PHIs — enabling cross-block optimization.

**abjit**: Compiles 50-100 instruction blocks independently. Cross-block values must be spilled to the `State` struct at block boundaries and reloaded at the next block's prologue. Every block entry loads registers from `[RBP+offset]`; every exit stores them back.

**Impact**: The load/store overhead at block boundaries is significant for hot inner loops. A loop body that wazero compiles as one function with values in registers, abjit compiles as multiple blocks with full register file load/store at each boundary.

### 6.4 Optimization Passes (est. 10-15% of the gap)

**Wazero**: Dead code elimination removes unreachable instructions. Redundant PHI elimination reduces register pressure. Block layout in reverse post-order improves cache behavior and branch prediction.

**abjit**: Peephole pass catches ~10 trivial patterns (self-moves, identity operations). No constant folding (`const 5; const 3; add` is not folded to `const 8`). No dead code elimination. No common subexpression elimination.

### 6.5 Bounds Check Elimination (est. 5-10% of the gap)

**Wazero**: SSA-level analysis eliminates redundant bounds checks. In loops, one check can prove the entire body safe.

**abjit**: Every memory access performs `(addr & mask) + base` inline. The mask-based approach is efficient (one AND + one ADD), but alignment checks add an extra branch. No hoisting of invariant checks out of loops.

**Note**: This factor is smaller than it would be for a naive bounds-checking scheme because our mask-based approach is already quite cheap. The gap here is more about alignment check overhead than bounds checks per se.

---

## 7. Feasibility: Adapting Wazero's Backend for RISC-V

### 7.1 The Key Insight

Wazero's SSA IR is **completely general-purpose** — no Wasm-specific opcodes. The frontend (Wasm → SSA) and backend (SSA → machine code) are cleanly separated by the `Builder` interface. We could replace the Wasm frontend with a RISC-V frontend and reuse the entire backend stack.

### 7.2 What's Reusable As-Is (80% of wazevo)

| Component | Lines (approx) | Reusable? |
|-----------|---------------:|-----------|
| SSA IR (`ssa/`) | ~5,000 | 100% |
| SSA Builder | ~2,000 | 100% |
| Optimization passes | ~1,500 | 100% |
| Backend compiler | ~2,000 | 100% |
| Register allocator | ~3,000 | 100% |
| AMD64 ISA backend | ~10,000 | 100% |
| ARM64 ISA backend | ~10,000 | 100% |
| Entry trampoline | ~500 | 95% (minor adaptation) |
| Dispatch loop | ~600 | 80% (different exit codes) |

### 7.3 What Needs to Be Written (~3,000-5,000 lines)

1. **RISC-V Instruction Decoder** (~800 lines)
   - Parse RV64IMAFDC + Zba/Zbb/Zbs
   - Handle compressed instructions (RVC)
   - We already have a complete decoder in our emulator

2. **RISC-V → SSA Lowering Handlers** (~2,500 lines)
   - Map each RISC-V instruction to SSA opcodes
   - RISC-V's 32 integer + 32 FP registers become SSA Variables
   - Example: `ADDI x1, x1, 10` →
     ```
     old_x1 = MustFindValue(x1Var)
     ten = Iconst64(10)
     new_x1 = Iadd(old_x1, ten)
     DefineVariable(x1Var, new_x1)
     ```

3. **Basic Block Detection** (~500-1000 lines)
   - Scan RISC-V code for branch targets, build CFG
   - Our `scanRegion()` BFS already does this

4. **Calling Convention / ABI Adapter** (~200 lines)
   - Map RISC-V standard ABI (a0-a7 args, fa0-fa7 float args) to wazero's `FunctionABI`

5. **Execution Context Adaptation** (~500 lines)
   - Adapt `executionContext` for RISC-V state (PC, CSRs, privilege mode)
   - Adapt exit codes for RISC-V traps (ECALL, page fault, etc.)

### 7.4 Architectural Challenges

**Block boundaries vs. whole-function compilation:**
Wazero compiles entire Wasm functions because Wasm has explicit function boundaries. RISC-V binaries don't always have clear function boundaries (stripped binaries, hand-written assembly, computed jumps). Options:
- Use symbol table / DWARF info when available
- Fall back to region-based compilation (like our current `scanRegion`)
- Use trace compilation for hot paths across function boundaries

**JIT vs AOT tradeoffs:**
Wazero is AOT — it compiles everything upfront. Our emulator is JIT — it compiles on first execution. The SSA pipeline is more expensive than our current IR pipeline, so compilation latency per block will increase. However:
- Wazero compiles Wasm functions in parallel (worker goroutines) — we could do the same
- The code quality improvement should more than compensate for slower compilation
- Wazero supports file-based compilation caching — amortizes cost across runs

**Self-modifying code:**
RISC-V programs can modify their own code (rare but legal). Wasm cannot. Our current JIT handles this by invalidating blocks when stores hit code regions. A wazero-based approach would need the same invalidation mechanism.

### 7.5 Expected Performance

If we achieve wazero-quality code generation for RISC-V, we could reasonably expect:
- Current: 4,103 MIPS (20% of native)
- With SSA + proper regalloc + instruction selection: **12,000-16,000 MIPS** (57-76% of native)
- The gap from 76% to wazero's 92% would come from: RISC-V's richer register set (32 vs Wasm's stack-based model creates more register pressure), cross-block overhead if we can't compile whole functions, and the inherent overhead of dynamic translation (address mapping, privilege checks)

### 7.6 Implementation Strategy

**Phase 1: Proof of concept (~2 weeks)**
- Fork wazevo frontend
- Implement RV64I subset (no F/D/C extensions)
- Compile a simple loop, measure MIPS vs abjit
- Validate against our interpreter

**Phase 2: Full ISA coverage (~3 weeks)**
- Add IMAFDC + Zba/Zbb/Zbs
- Handle compressed instructions
- Pass riscv-elf-tests suite

**Phase 3: Integration with emulator (~2 weeks)**
- Replace abjit backend with wazevo-based backend
- Adapt dispatch loop and block caching
- Handle self-modifying code invalidation
- Benchmark against libriscv and native

**Phase 4: Optimization (~ongoing)**
- Tune compilation unit size (region vs function)
- Add RISC-V-specific SSA optimizations (e.g., x0 is always zero)
- Profile and optimize hot paths

---

## 8. Summary

### What wazero does that we don't:
1. **SSA-based IR** with proper dataflow analysis (we use linear/imperative IR)
2. **Chaitin register allocation** with liveness intervals (we use fixed static priority)
3. **Instruction selection with pattern matching** and address mode synthesis (we do direct per-op translation)
4. **Cross-block optimization** within functions (we compile blocks independently)
5. **Dead code elimination, constant propagation** (we have peephole only)
6. **Bounds check elimination** via knownSafeBound tracking (we check every access)

### What we already do right:
1. **Zero-CGO trampoline** — actually faster than wazero's (2.8ns vs ~5ns)
2. **Go callback support** from native code (4.0ns round-trip)
3. **GC-safe design** — no mmap pointers on Go stack
4. **Block chaining** with runtime patching
5. **JALR inline cache** with 2-slot shift policy
6. **Mask-based memory sandbox** — efficient, branchless base case

### The path forward:
The most promising approach is **replacing our IR/regalloc/lowering pipeline with wazero's SSA backend**, keeping our trampoline, block chaining, JALR-IC, and memory model. This gives us wazero-quality code generation with our existing runtime infrastructure. Estimated effort: 3,000-5,000 lines of new frontend code + integration work, achievable in 6-8 weeks.
