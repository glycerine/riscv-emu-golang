# Beat TCC: Larger Blocks + Eliminate Dispatch Overhead

## Context

The native IR JIT runs at 194 MIPS vs TCC's 1961 MIPS (10x slower). CPU profiling shows:
- **45%** — `map[uint64]` dispatch lookup in `RunJIT`
- **21%** — actual JIT code execution (`jitcall.Call`)
- **8%** — OS/runtime overhead

The map lookup dominates because every block exit does: native RET → restore 6 callee-saves → copy 32-byte Result to Go stack → Go dispatch loop → map lookup → setup 6 args → save 6 callee-saves → CALL. TCC has this same overhead but compiles larger blocks (fewer dispatches).

**Two-tier strategy:**
1. **Match TCC**: handle the same instructions so blocks are equally large
2. **Beat TCC**: block chaining — compiled blocks jump directly to each other, bypassing the dispatch loop entirely

## What TCC Handles That IR Bails On

### High Impact (common in real code)

1. **DIV/DIVU/REM/REMU (64-bit)** — TCC emits ternary checks for divide-by-zero and signed overflow:
   ```c
   // DIV:  (b==0) ? -1 : (a==INT64_MIN && b==-1) ? a : a/b
   // DIVU: (b==0) ? MAX_UINT : a/b
   // REM:  (b==0) ? a : (a==INT64_MIN && b==-1) ? 0 : a%b
   // REMU: (b==0) ? a : a%b
   ```

2. **DIVW/DIVUW/REMW/REMUW (32-bit)** — Same pattern with 32-bit casts + sign-extend.

3. **CLZ/CTZ/CPOP** (Zbb) — TCC uses inline C helper functions (`jit_clz64`, etc.). These appear in compiler-generated code.

### Medium Impact

4. **FMIN.S/FMAX.S/FMIN.D/FMAX.D** — TCC uses custom NaN-aware helpers.

5. **ORC.B** — OR-combine bytes. TCC emits inline byte loop.

6. **REV8** — Byte-reverse. TCC emits byte-swap code.

7. **RORIW** — Word rotate right immediate.

8. **CLZW/CTZW/CPOPW** — 32-bit versions of Zbb helpers.

### Low Impact (TCC also bails)

9. **FCLASS.S/FCLASS.D** — Both TCC and IR bail. Leave as-is.
10. **MULH/MULHSU/MULHU** — TCC bails (no `__int128`). IR already handles these. IR is ahead.

## Implementation Plan

### File: `jit_emit_ir.go`

#### 1. DIV/DIVU/REM/REMU (64-bit) — replace bail with guarded IR

For each, emit a branch-based guard matching RISC-V spec:

```
DIV rd, rs1, rs2:
  if rs2 == 0:  rd = -1
  elif rs1 == INT64_MIN && rs2 == -1:  rd = rs1  (overflow)
  else: rd = rs1 / rs2

DIVU rd, rs1, rs2:
  if rs2 == 0:  rd = UINT64_MAX
  else: rd = rs1 /u rs2

REM rd, rs1, rs2:
  if rs2 == 0:  rd = rs1
  elif rs1 == INT64_MIN && rs2 == -1:  rd = 0  (overflow)
  else: rd = rs1 % rs2

REMU rd, rs1, rs2:
  if rs2 == 0:  rd = rs1
  else: rd = rs1 %u rs2
```

IR pattern (example for DIV):
```
  zeroLabel = NewLabel(); ovfLabel = NewLabel(); doneLabel = NewLabel()
  Branch(b, VRegZero, EQ, zeroLabel)         // b == 0?
  t = Tmp(); Const(t, INT64_MIN)
  Branch(a, t, NE, normalLabel)              // a != INT64_MIN?
  t2 = Tmp(); Const(t2, -1)
  Branch(b, t2, NE, normalLabel)             // b != -1?
  // Overflow: rd = a
  PlaceLabel(ovfLabel); Mov(dst, a); Jump(doneLabel)
  // Zero: rd = -1
  PlaceLabel(zeroLabel); Const(dst, -1); Jump(doneLabel)
  // Normal
  PlaceLabel(normalLabel); DivS(dst, a, b); Jump(doneLabel)
  PlaceLabel(doneLabel)
```

#### 2. DIVW/DIVUW/REMW/REMUW (32-bit) — same pattern with 32-bit bounds

Same approach but with `INT32_MIN` / 32-bit comparisons and sign-extension of result.

#### 3. CLZ/CTZ/CPOP — implement via IR bit manipulation

These can be implemented as IR helper sequences, or more efficiently by adding new IR opcodes (`IRClz`, `IRCtz`, `IRPopcount`) and lowering them to `LZCNTQ`/`TZCNTQ`/`POPCNTQ` x86 instructions (available on all modern x86).

**Recommended**: Add `IRClz`, `IRCtz`, `IRPopcount` opcodes to `ir/ir.go` and lower via `LZCNTQ`/`TZCNTQ`/`POPCNTQ`. This is simpler and faster than emitting a loop in IR.

#### 4. FMIN/FMAX — implement via IR comparisons

```
FMIN.D rd, rs1, rs2:
  if rs1 is NaN: rd = rs2
  elif rs2 is NaN: rd = rs1
  elif rs1 < rs2: rd = rs1
  else: rd = rs2
```

Use `FCmp` with ordered comparisons + NaN check via `FCmp(a, a, EQ)` (NaN != NaN).

#### 5. ORC.B — byte-wise OR propagation

Each byte of rd is 0xFF if the corresponding byte of rs1 is nonzero, else 0x00. Implement with 8 iterations of shift/mask/OR in IR. ~24 IR instructions.

#### 6. REV8 — byte reversal

Swap bytes of a 64-bit value. Implement with shifts and ORs. ~14 IR instructions. Or add `IRBswap` opcode → `BSWAPQ`.

#### 7. RORIW — word rotate right immediate

```
t = Zext(a, I32)
t = (t >> shamt) | (t << (32-shamt))
dst = Sext(t, I32)
```

#### 8. CLZW/CTZW/CPOPW — 32-bit versions

If we add IRClz/IRCtz/IRPopcount with I32 type, the lowerer uses `LZCNTL`/`TZCNTL`/`POPCNTL`.

### File: `ir/ir.go` — new opcodes

Add:
```go
IRClz      // Dst = count leading zeros of A (type T)
IRCtz      // Dst = count trailing zeros of A (type T)
IRPopcount // Dst = population count of A (type T)
IRBswap    // Dst = byte-reverse of A
```

### File: `ir/emit.go` — new emitter methods

```go
func (e *Emitter) Clz(dst, a VReg, t Type)
func (e *Emitter) Ctz(dst, a VReg, t Type)
func (e *Emitter) Popcount(dst, a VReg, t Type)
func (e *Emitter) Bswap(dst, a VReg)
```

### File: `ir/regalloc.go` — register uses/defs for new opcodes

Add new opcodes to `instrDefs` (returns Dst) and `instrUses` (uses A).

### File: `ir/lower_amd64.go` — lower new opcodes

```go
case IRClz:    lc.lowerUnary(ins, x86.ALZCNTQ)  // or ALZCNTL for I32
case IRCtz:    lc.lowerUnary(ins, x86.ATZCNTQ)
case IRPopcount: lc.lowerUnary(ins, x86.APOPCNTQ)
case IRBswap:  lc.lowerUnary(ins, x86.ABSWAPQ)
```

### File: `ir/lower_amd64_v2.go` — same for V2

Mirror the new opcodes.

## Implementation Order

1. **DIV/REM guards** (biggest impact — division is common in real code, and currently every DIV fragments the block)
2. **CLZ/CTZ/CPOP + CLZW/CTZW/CPOPW** (new IR opcodes + lowering, used by compiler-generated code)
3. **REV8 + ORC.B** (new IR opcode for BSWAP, inline sequence for ORC.B)
4. **RORIW** (simple IR sequence)
5. **FMIN/FMAX** (IR comparison sequences)

## Verification

```bash
# All ELF tests pass
go test -count=1 -run 'TestRISCVTests_Lockstep_UI' -timeout 120s -v .

# M-extension tests pass (DIV/REM)
go test -count=1 -run 'TestRISCVTests_UM_JIT' -timeout 30s -v .

# JIT unit tests
go test -count=1 -run 'TestJIT_' -timeout 30s .

# Benchmark comparison
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/

# V1/V2 lockstep
go test -count=1 -run 'TestLockstep_V1V2' -timeout 30s .
```

## Tier 2: Beat TCC — Block Chaining

### The Problem

Every block exit currently does this round-trip:

```
native RET
  → jitcall.Call epilogue (restore BX,BP,R12-R15, copy 32-byte Result)
    → Go dispatch loop (read Result, update cpu.pc/cycle, map lookup)
      → jitcall.Call prologue (save BX,BP,R12-R15, setup args)
        → native CALL
```

That's ~40 instructions of overhead per block transition. TCC pays this too. We can eliminate it.

### Block Chaining Design

When a compiled block exits to a known PC (not ECALL/fault), instead of returning to Go, jump directly to the target block's native code:

```
Block A (ends with branch to PC 0x200):
  ... compute ...
  JMP [chain_slot]       ; patched to Block B's entry

Block B (at PC 0x200):
  ... compute ...
```

**No RET, no Go dispatch, no map lookup, no CALL.** Just a single JMP.

### How It Works

1. **Chain table**: Each compiled block has an array of exit slots. Each slot holds a function pointer (initially pointing to a "miss" trampoline).

2. **Miss trampoline**: On first execution, the slot points to a small stub that:
   - Writes the Result (PC, IC, status) to the sret buffer
   - RETs to Go dispatch (the normal slow path)
   - Go dispatch compiles the target block (if needed), then **patches** the slot to point directly to the target block's entry

3. **Patched fast path**: After patching, the JMP goes directly from block A to block B. The pinned registers (R12-R15 = x[], f[], memBase, memMask; RBP = IC) are already set up — no prologue needed.

4. **IC budget**: Each block increments RBP (IC). The budget check at backward branches ensures preemption. Forward chains don't need budget checks.

### Implementation

#### Compiled block layout

```go
type compiledBlock struct {
    fn      uintptr   // native function pointer
    backing []byte    // mmap'd memory
    exits   []uintptr // chain slots: exit[i] → target block fn or miss stub
}
```

#### IR changes

Add `IRChainExit` — like `IRRet` but instead of writing sret + RET, it does:
```asm
  MOV [chain_slot_addr], RAX    ; load chain target
  ADD $1, RBP                   ; IC++
  JMP RAX                       ; direct jump (no CALL/RET)
```

On first execution, RAX points to the miss stub. After patching, RAX points to the target block.

#### Miss stub (one per exit slot)

```asm
miss_stub:
  ; Write Result to sret buffer (RBX)
  MOV $target_pc, R10
  MOV R10, 0(RBX)        ; Result.PC
  MOV RBP, 8(RBX)        ; Result.IC
  MOV $0, 16(RBX)        ; Result.Status = jitOK
  MOV $0, 24(RBX)        ; Result.FaultAddr = 0
  ; Deallocate spill frame if needed
  RET                     ; back to jitcall.Call
```

#### Patching (in Go dispatch loop)

```go
case jitOK:
    // Check if the previous block has an unchained exit for this PC
    if prevBlk != nil && exitIdx >= 0 {
        if target, ok := j.blocks[cpu.pc]; ok {
            // Patch: make prev block jump directly to target
            prevBlk.exits[exitIdx] = target.fn
        }
    }
    continue
```

#### Benefits

- **Eliminates map lookup** for all chained transitions (the hot path)
- **Eliminates jitcall.Call overhead** (save/restore callee-saves, copy Result)
- **Pinned registers stay loaded** across block boundaries
- Chaining is incremental — blocks start unchained and get patched on first execution

#### What stays the same

- ECALL/EBREAK/faults still RET to Go (they need Go-side exception handling)
- First execution of any block still goes through dispatch (cold path)
- Interpreter fallback unchanged

### Expected Performance

With chaining, the hot loop (fib benchmark) becomes:
```
Block A: add, branch-taken → JMP Block A  (tight loop, no dispatch)
```

The 45% map lookup overhead drops to near zero for hot paths. Expected MIPS: **3000-5000+** (limited by native code quality, not dispatch).

### Complexity

Block chaining is an independent optimization that can be added AFTER Tier 1 (instruction coverage). It only touches:
- `jit.go` — dispatch loop patching
- `jit_native.go` — compiled block struct
- `ir/lower_amd64.go` — new `IRChainExit` lowering
- `internal/jitcall/call_amd64.s` — no changes needed (chained blocks bypass jitcall entirely)

## Implementation Order

### Tier 1: Match TCC (instruction coverage)
1. DIV/REM guards (64-bit + 32-bit)
2. CLZ/CTZ/CPOP + CLZW/CTZW/CPOPW (new IR opcodes)
3. REV8 + ORC.B (BSWAP opcode + inline sequence)
4. RORIW (IR sequence)
5. FMIN/FMAX (IR comparison sequences)

### Tier 2: Beat TCC (block chaining)
6. Add chain exit slots to compiledBlock
7. Emit IRChainExit instead of IRRet for jitOK exits
8. Implement miss stubs
9. Add patching to dispatch loop
10. Benchmark and tune

## Verification

```bash
# All ELF tests pass
go test -count=1 -run 'TestRISCVTests_Lockstep_UI' -timeout 120s -v .

# M-extension tests pass (DIV/REM)
go test -count=1 -run 'TestRISCVTests_UM_JIT' -timeout 30s -v .

# JIT unit tests
go test -count=1 -run 'TestJIT_' -timeout 30s .

# Benchmark: target > 2000 MIPS after Tier 1, > 4000 MIPS after Tier 2
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/

# Compare with TCC
go test -tags tcc -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=1x ./bench/

# V1/V2 lockstep
go test -count=1 -run 'TestLockstep_V1V2' -timeout 30s .
```

## Critical Files

- `jit_emit_ir.go` — remove bails, add guarded sequences
- `ir/ir.go` — new opcodes (CLZ, CTZ, POPCNT, BSWAP, ChainExit)
- `ir/emit.go` — new emitter methods
- `ir/regalloc.go` — new opcode uses/defs
- `ir/lower_amd64.go` — lower new opcodes + chain exit
- `ir/lower_amd64_v2.go` — same for V2
- `jit.go` — chain patching in dispatch loop
- `jit_native.go` — exit slots in compiledBlock
- `jit_decode.go` — no changes (scanRegion already does BFS)
