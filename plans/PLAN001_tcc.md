# JIT Binary Translation for the Go RISC-V Emulator

## Context

Our Go CPU interpreter runs at ~150 MIPS. libriscv's C++ interpreter (threaded dispatch, no JIT) runs at ~823 MIPS. libriscv with JIT (TCC binary translation) runs at ~3400 MIPS. The JIT achieves a ~4x speedup over the interpreter by translating hot RISC-V basic blocks into native C, compiling with TCC (Tiny C Compiler), and calling the result via function pointer.

We will use the same strategy: translate RISC-V basic blocks to C source, compile to native code via TCC's in-memory compiler, cache compiled blocks, and call them from Go via cgo.

## How libriscv Does It

1. **Detect basic block**: Walk from a PC until branch/jump/ecall/ebreak or size limit (~5000 insns)
2. **Emit C**: For each instruction, emit equivalent C operating on a `CPU*` struct — register reads/writes become `cpu->r[N]`, memory ops become `*(type*)(arena + addr)`. Frequently-used RISC-V registers are cached in C local variables (`uint64_t r1 = cpu->r[1]`) and written back on exit.
3. **Compile**: Call `tcc_compile_string()` → `tcc_relocate()` → `tcc_get_symbol()` to get a function pointer. All in memory, no files.
4. **Dispatch**: When PC hits a translated block, call the native function directly instead of interpreting.

## Architecture

```
RunJIT loop:
  ┌──────────────────────────────────────────┐
  │  pc = cpu.PC()                           │
  │  if block = cache[pc]; block != nil:     │
  │      result = cgo_call(block, &cpu.x,    │
  │               mem.base, mem.mask, ...)   │
  │      cpu.pc = result.pc                  │
  │      cpu.cycle += result.ic              │
  │      handle result.status (ecall/fault)  │
  │  else:                                   │
  │      emit C for basic block at pc        │
  │      compile with TCC → fn pointer       │
  │      cache[pc] = fn                      │
  │      (fall through to call it next iter) │
  └──────────────────────────────────────────┘
```

## Files

| File | Purpose |
|------|---------|
| `jit.go` | JIT manager: block cache, `RunJIT` dispatch loop, integration with NoteChain |
| `jit_emit.go` | C code emitter: translates RISC-V instructions → C source strings |
| `jit_tcc.go` | cgo bridge to libtcc: compile C string → native function pointer (cold path only) |
| `jit_call_amd64.s` | Go assembly trampoline: call JIT'd blocks with no cgo overhead (hot path) |
| `jit_call.go` | Go function signature matching the assembly trampoline |
| `jit_test.go` | Tests: single-block translation, fib loop, comparison with interpreter |
| `Makefile` | Build libtcc from vendored TCC source |

## Step 1: Build libtcc

TCC source is already vendored at `vendor/libriscv/lib/libriscv/`. The libtcc library can be built standalone. We need:
- `libtcc.h` — TCC's public C API
- `libtcc.a` — compiled library

Add a Makefile target that builds libtcc from the vendored TCC source (or use a separately vendored tcc).
The key TCC API calls:
```c
TCCState *s = tcc_new();
tcc_set_output_type(s, TCC_OUTPUT_MEMORY);
tcc_compile_string(s, c_source);
tcc_relocate(s, TCC_RELOCATE_AUTO);
void *fn = tcc_get_symbol(s, "block_entry");
// fn is now a callable function pointer
```

## Step 2: Compilation (cgo, cold path) and Invocation (Go asm, hot path)

Two separate layers — cgo is only used for TCC compilation (once per block), never on the hot execution path.

### 2a: TCC Compilation (`jit_tcc.go`) — cold path, uses cgo

```go
// #cgo CFLAGS: -I${SRCDIR}/vendor/tcc
// #cgo LDFLAGS: -L${SRCDIR}/vendor/tcc -ltcc
// #include <libtcc.h>
import "C"

// compileBlock takes C source, compiles it with TCC, returns a native function pointer.
// Called once per block (~1ms), amortized over billions of executions.
func compileBlock(csrc string) (*compiledBlock, error) {
    s := C.tcc_new()
    C.tcc_set_output_type(s, C.TCC_OUTPUT_MEMORY)
    C.tcc_compile_string(s, C.CString(csrc))
    C.tcc_relocate(s, C.TCC_RELOCATE_AUTO)
    fn := C.tcc_get_symbol(s, C.CString("block_entry"))
    return &compiledBlock{fn: uintptr(fn), tcc: s}, nil
}

type compiledBlock struct {
    fn   uintptr     // native function pointer (passed to Go asm trampoline)
    tcc  *C.TCCState // prevents GC — owns the compiled code memory
}
```

### 2b: Block Invocation (`jit_call_amd64.s` + `jit_call.go`) — hot path, zero cgo

The JIT'd block is called via Go assembly, avoiding the ~50ns `runtime.cgocall` overhead entirely. The generated native code uses minimal stack (just register shuffling), so running on the Go goroutine stack is safe.

**`jit_call.go`** — Go function signature:
```go
// JITResult is the return value from a JIT'd block.
// Matches the C struct layout: {pc, ic, status, fault_addr}.
type JITResult struct {
    PC        uint64
    IC        uint64
    Status    int32  // 0=ok, 1=ecall, 2=ebreak, 3=load_fault, 4=store_fault
    _         int32  // padding
    FaultAddr uint64
}

// callJIT calls a JIT-compiled block via direct function pointer.
// Implemented in jit_call_amd64.s — no cgo overhead.
func callJIT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
             memBase uintptr, memMask uint64) JITResult
```

**`jit_call_amd64.s`** — System V ABI trampoline:
```asm
#include "textflag.h"

// func callJIT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
//              memBase uintptr, memMask uint64) JITResult
//
// Go ABI0: args on stack. SysV ABI: args in DI, SI, DX, CX, R8, R9.
TEXT ·callJIT(SB), NOSPLIT, $64
    // Load Go args from stack into SysV calling convention registers
    MOVQ  x+8(FP), DI           // arg1: x (register file)
    MOVQ  f+16(FP), SI          // arg2: f (float regs)
    MOVQ  fcsr+24(FP), DX       // arg3: fcsr
    MOVQ  memBase+32(FP), CX    // arg4: mem_base
    MOVQ  memMask+40(FP), R8    // arg5: mem_mask

    // Save callee-saved registers (Go expects these preserved)
    MOVQ  BX, 0(SP)
    MOVQ  BP, 8(SP)
    MOVQ  R12, 16(SP)
    MOVQ  R13, 24(SP)
    MOVQ  R14, 32(SP)
    MOVQ  R15, 40(SP)

    // Call the JIT'd function
    MOVQ  fn+0(FP), AX
    CALL  AX

    // Restore callee-saved registers
    MOVQ  0(SP), BX
    MOVQ  8(SP), BP
    MOVQ  16(SP), R12
    MOVQ  24(SP), R13
    MOVQ  32(SP), R14
    MOVQ  40(SP), R15

    // SysV returns struct in RAX:RDX (small struct) or via hidden pointer.
    // JITResult is 32 bytes → returned via hidden pointer in RDI (set by caller).
    // TCC will return it however the ABI dictates; we copy from the return area.
    MOVQ  AX, ret+48(FP)        // pc
    MOVQ  DX, ret+56(FP)        // ic
    // status + fault_addr returned in memory (details depend on ABI struct return)
    RET
```

*Note: exact register mapping and struct return convention need validation during implementation. The key point is that this is a plain `CALL` — no cgo overhead.*

## Step 3: C Code Emitter (`jit_emit.go`)

Walk RISC-V instructions from a starting PC, emit C source for each, stop at block boundaries.

**Generated C function structure** (following libriscv's pattern):

```c
#include <stdint.h>
typedef struct { uint64_t pc; uint64_t ic; int status; uint64_t fault_addr; } JITResult;

JITResult block_entry(uint64_t *x, uint64_t *f, uint32_t *fcsr,
                      char *mem_base, uint64_t mem_mask) {
    // Cache registers used in this block
    uint64_t r10 = x[10];
    uint64_t r11 = x[11];
    uint64_t r12 = x[12];
    uint64_t r13 = x[13];
    uint64_t r5  = x[5];
    uint64_t ic = 0;

    // Instruction sequence with labels for branch targets
b_0x10400:
    r5 = r10 + r11;                    // add t0, a0, a1
    ic++;
b_0x10404:
    r10 = r11 + 0;                     // addi a0, a1, 0  (mv)
    ic++;
b_0x10408:
    r11 = r5 + 0;                      // addi a1, t0, 0  (mv)
    ic++;
b_0x1040c:
    r12 = r12 + 1;                     // addi a2, a2, 1
    ic++;
b_0x10410:
    ic++;
    if ((int64_t)r12 < (int64_t)r13) goto b_0x10400;  // blt a2, a3, -16

    // Write back cached registers
    x[5] = r5; x[10] = r10; x[11] = r11; x[12] = r12;
    return (JITResult){0x10414, ic, 0, 0};  // fall through
}
```

**Emitter handles these instruction classes (Phase 1 — integer only):**

| Opcode | Instructions | C emission |
|--------|-------------|------------|
| 0x33 | ADD/SUB/SLL/SRL/SRA/AND/OR/XOR/SLT/SLTU + M-ext (MUL/DIV) + Zbb/Zbc/Zicond | `rd = rs1 OP rs2;` |
| 0x13 | ADDI/SLTI/SLTIU/XORI/ORI/ANDI/SLLI/SRLI/SRAI + Zbb/Zbs imm | `rd = rs1 OP imm;` |
| 0x3B | ADDW/SUBW/SLLW/SRLW/SRAW/MULW/DIVW etc. | `rd = (int64_t)(int32_t)(rs1 OP rs2);` |
| 0x1B | ADDIW/SLLIW/SRLIW/SRAIW | `rd = (int64_t)(int32_t)(rs1 OP imm);` |
| 0x37 | LUI | `rd = imm << 12;` |
| 0x17 | AUIPC | `rd = pc + (imm << 12);` |
| 0x03 | LB/LH/LW/LD/LBU/LHU/LWU | bounds check + `rd = *(type*)(mem_base + (addr & mask));` |
| 0x23 | SB/SH/SW/SD | bounds check + `*(type*)(mem_base + (addr & mask)) = rs2;` |
| 0x63 | BEQ/BNE/BLT/BGE/BLTU/BGEU | `if (rs1 cmp rs2) goto label;` or exit block |
| 0x6F | JAL | `rd = pc+4; return {target, ic, 0, 0};` |
| 0x67 | JALR | `rd = pc+4; return {(rs1+imm)&~1, ic, 0, 0};` |
| 0x73 | ECALL/EBREAK | `return {pc, ic, 1/2, 0};` |
| RVC | Compressed equivalents | Same logic, 2-byte PC increment |

**Memory access pattern:**
```c
// Load (e.g., LD)
{ uint64_t addr = rRS1 + IMM;
  if (__builtin_expect((addr & 7) | ((addr | (addr+7)) & ~mem_mask), 0))
      { x[RS1_WRITEBACK]; return (JITResult){PC, ic, 3, addr}; }
  rRD = *(uint64_t*)(mem_base + (addr & mem_mask)); }

// Store (e.g., SD)
{ uint64_t addr = rRS1 + IMM;
  if (__builtin_expect((addr & 7) | ((addr | (addr+7)) & ~mem_mask), 0))
      { x[RS1_WRITEBACK]; return (JITResult){PC, ic, 4, addr}; }
  *(uint64_t*)(mem_base + (addr & mem_mask)) = rRS2; }
```

**Block termination rules:**
- Branch with backward target inside block → emit `goto` label (keeps loops JIT'd)
- Branch with forward target inside block → emit `goto` label
- Branch to outside block → write back registers, return with target PC
- JAL/JALR → always exit block (indirect jumps are unpredictable)
- ECALL/EBREAK → exit with status code
- Unknown/FP instruction → exit block (fall back to interpreter)
- Max block size (512 instructions) → exit

**Register caching:**
- Scan the block to find which registers are read/written
- Cache those in C local variables at entry, write back on every exit path
- x0 is never cached (hardwired zero, emit `0` literal)

## Step 4: JIT Manager (`jit.go`)

```go
type JIT struct {
    blocks map[uint64]*compiledBlock  // PC → compiled block
}

func (j *JIT) RunJIT(cpu *CPU) error {
    // Like RunWithChain but tries JIT blocks first
    for {
        pc := cpu.pc
        if blk, ok := j.blocks[pc]; ok {
            result := callJIT(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
                              cpu.mem.base, cpu.mem.mask)
            cpu.pc = result.pc
            cpu.cycle += result.ic
            switch result.status {
            case 0: continue  // normal block exit
            case 1: // ecall
                n := noteFromStepErr(ErrEcall, cpu.pc)
                if cpu.Notes.Deliver(cpu, n) != NoteHandled { return ErrEcall }
                continue
            case 2: // ebreak — similar
            case 3, 4: // memory fault — return MemFault
            }
        } else {
            // Translate and compile this block
            csrc, meta := emitBlock(cpu, pc)
            blk := compile(csrc)
            j.blocks[pc] = blk
            // Next iteration will hit the cache
        }
    }
}
```

## Step 5: Integration

- Add `RunJIT` as an alternative to `Run`/`RunWithChain`
- The benchmark and `RunWithOS` can opt into JIT mode
- FP instructions are not JIT'd in Phase 1 — they cause a block exit and the interpreter handles them, then the next block is JIT'd from the instruction after
- Add a `BenchmarkCPU_FullExecution_JIT` benchmark

## Phase 2 (future): FP JIT, Compressed Instructions

- Add FP instruction emission (calls to fenv assembly helpers)
- Add RVC emission (already decoded in emitter, just 2-byte PC increments)
- Add block chaining: when one block exits to another known block, patch the exit to jump directly (avoid Go dispatch overhead)

## Verification

```bash
# Unit test: translate a single ADD, compare result with interpreter
go test -run TestJIT_ADD -v

# Fib loop: compare JIT vs interpreter for fib(1000)
go test -run TestJIT_Fib -v

# Full benchmark:
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/

# Regression: interpreter still passes all tests
go test -timeout 60s ./...
```

## Expected Performance

- Compilation cost: ~1ms per block with TCC (amortized over billions of executions)
- Block call cost: ~2ns via Go assembly (vs ~50ns if we used cgo — 25x cheaper)
- Target: 500-1500 MIPS (3-10x over current 150 MIPS interpreter)
- libriscv JIT achieves ~4x over its own interpreter (823 → 3400 MIPS); we should see similar ratio
- Overhead vs libriscv: Go dispatch loop between blocks, slightly larger struct passing. No cgo overhead on hot path.
