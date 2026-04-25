# Plan: Flatten interpreter dispatch to single-level switch

## Context

The RunCached megaswitch (`run_cached.go`) dispatches every instruction via `switch slot.op`. For RV32 instructions, `slot.op` is the raw 7-bit opcode (0x03, 0x13, 0x33, etc.), requiring nested sub-switches on `funct3` and `funct7` to reach the actual handler. This means ~60% of instructions pay 2-3 indirect branches per dispatch instead of 1.

RVC instructions already use flat synthetic opcodes (`opC_ADDI`, `opC_LW`, etc. starting at 0x80) — only `opC_MISC_ALU` still has internal nesting (9 sub-handlers for C.SRLI/SRAI/ANDI/SUB/XOR/OR/AND/SUBW/ADDW).

**Goal:** Resolve all nesting at decode time (paid once per instruction) so the megaswitch is a single flat jump table — one indirect branch per dispatch, zero nesting.

**Critical constraint:** RunCached is extremely sensitive to Go compiler codegen (see lines 31-61 of `run_cached.go` — moving a call site between files caused a 25% regression). All changes stay in `run_cached.go`. No changes to `decode.go`, `decoder_cache.go`, or `jit_vizjit.go`.

## Design

### Flat opcode constants (in `run_cached.go`)

Define ~73 constants covering every RV32 instruction the megaswitch handles inline, plus 9 for flattened MISC_ALU, plus 1 delegate marker:

```
 0       — sentinel (uninitialized/OOB slot)
 1-7     — LOAD: LB LH LW LD LBU LHU LWU
 8-11    — STORE: SB SH SW SD
12-17    — BRANCH: BEQ BNE BLT BGE BLTU BGEU
18-26    — OP-IMM: ADDI SLTI SLTIU XORI ORI ANDI SLLI SRLI SRAI
27-30    — OP-IMM-32: ADDIW SLLIW SRLIW SRAIW
31-38    — OP funct7=0x00: ADD SLL SLT SLTU XOR SRL OR AND
39-40    — OP funct7=0x20: SUB SRA
41-48    — OP funct7=0x01 (M): MUL MULH MULHSU MULHU DIV DIVU REM REMU
49-51    — OP-32 funct7=0x00: ADDW SLLW SRLW
52-53    — OP-32 funct7=0x20: SUBW SRAW
54-58    — OP-32 funct7=0x01 (M): MULW DIVW DIVUW REMW REMUW
59-63    — singles: FENCE AUIPC LUI JAL JALR
64-72    — MISC_ALU: C.SRLI C.SRAI C.ANDI C.SUB C.XOR C.OR C.AND C.SUBW C.ADDW
73       — opDelegate (FP/AMO/SYSTEM/Zb* → delegateInsn)
0x80+    — existing opC_* RVC values (unchanged)
```

Total: ~96 flat cases. Go generates a ~165-entry jump table (dense from 0-73, gap to 0x80, dense to ~0xA4). All fits in one cache line worth of pointer table.

### `flattenSlotOp(slot *DecodedInsn)` function (in `run_cached.go`)

Called once from `populateSlot` after `decodeInsn32`/`decodeRVC`. Converts raw opcode to flat dispatch ID using slot's pre-decoded `funct3`, `funct7`, and `insn` fields.

- **RVC (slot.op >= 0x80):** Only transforms `opC_MISC_ALU` → one of 9 flat opcodes. Also pre-decodes rd/rs1/rs2/imm for MISC_ALU sub-variants (currently re-extracted inline every execution). All other opC_* values pass through unchanged.
- **RV32:** Nested switch on `slot.op` (raw opcode) → `slot.funct3` → `slot.funct7` resolves to flat ID. Unrecognized combinations → `opDelegate`.
- Shortcut for OP funct7=0x00 and funct7=0x01: all 8 funct3 values are valid, so `slot.op = baseOp + slot.funct3`.

### Refactor `case 0:` (sentinel/first-visit)

Current flow: `case 0:` → `slowStep()` → `populateSlot()` + `exec32Slot()`/`execRVCSlot()`.

New flow:
```go
case 0:
    pc, err = slowStep(cpu, cache, slot, pc)
    if err == nil && slot.op != 0 {
        // Slot just populated with flat opcode. Re-dispatch immediately.
        continue
    }
```

`slowStep` changes: after `populateSlot`, return `(pc, nil)` without calling `exec*Slot`. The `continue` re-enters the inner loop; the switch dispatches to the flat handler. One extra switch evaluation on first visit only — negligible.

`exec_slot.go` and `exec_slot32.go` become dead code (only called from `slowStep`). Leave the files for now — they're useful reference implementations.

### Flatten the megaswitch

Replace every nested case with flat top-level cases. Example:

**Before** (2 indirect branches):
```go
case 0x03: // LOAD
    addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
    var v uint64
    var f *MemFault
    switch slot.funct3 {
    case 0x0: // LB
        var u uint8
        u, f = (&cpu.mem).Load8(addr)
        v = uint64(int64(int8(u)))
    case 0x3: // LD
        // ... aligned fast path ...
    // ... 5 more cases ...
    }
    if f != nil { err = f; break inner }
    cpu.x[slot.rd] = v
    cpu.x[0] = 0
    pc += 4
```

**After** (1 indirect branch per case):
```go
case opLB:
    addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
    u, f := (&cpu.mem).Load8(addr)
    if f != nil { err = f; break inner }
    cpu.x[slot.rd] = uint64(int64(int8(u)))
    cpu.x[0] = 0
    pc += 4

case opLD:
    addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
    if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
        cpu.x[slot.rd] = *(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask)))
    } else {
        v, f := (&cpu.mem).Load64U(addr)
        if f != nil { err = f; break inner }
        cpu.x[slot.rd] = v
    }
    cpu.x[0] = 0
    pc += 4
```

The `delegate` bool pattern (used in OP/OP-IMM/OP-32) disappears entirely — each handler is self-contained.

For MISC_ALU, the 9 handlers become trivial since rd/rs1/rs2/imm are now pre-decoded:
```go
case opCSRLI:
    cpu.x[slot.rd] >>= uint(slot.imm)
    pc += 2
case opCSUB:
    cpu.x[slot.rd] -= cpu.x[slot.rs2]
    pc += 2
```

### Update `populateSlot` references

Line 847: `slot.op == 0x0F` → runs BEFORE `flattenSlotOp`, so stays as raw opcode check. Fine.

Line 869: `slot.op == 0x6F || slot.op == opC_J` → runs AFTER `flattenSlotOp`, must change to `slot.op == opJAL || slot.op == opC_J`.

## Files to modify

| File | Change |
|------|--------|
| `run_cached.go` | Add flat opcode constants (1-73). Add `flattenSlotOp` + `flattenMiscALU`. Call from `populateSlot`. Flatten megaswitch (~96 top-level cases). Refactor `case 0:` with `continue` re-dispatch. Remove exec*Slot calls from `slowStep`. Update JAL check in `populateSlot`. |

**No changes to:** `decode.go`, `decoder_cache.go`, `cpu.go`, `jit_vizjit.go`, `exec_slot.go`, `exec_slot32.go`.

## Verification

```bash
# Primary benchmark gate (must stay ≥400 MIPS):
go test -run='^$' -bench='BenchmarkCPU_FullExecution_Cached' -benchtime=3s ./bench/

# Compare before/after (run on same machine, no load):
# git stash && run benchmark → baseline
# git stash pop && run benchmark → new
# Expect 2-5% improvement on interpreter-bound workloads

# Full test suite:
go test -v -timeout 120s .
go test -v -timeout 120s ./bench/

# Specific interpreter-path tests:
go test -v -run TestJIT_ADD .
go test -v -run TestInlineEcall_HelloEndToEnd .
go test -v -run 'TestRV64' .

# RISC-V official test suite (exercises all instruction variants):
go test -v -run TestRISCVTests .
```

If MIPS drops below 400, the Go compiler codegen shifted. Remedies:
1. Check if adding `//go:nosplit` to `flattenSlotOp` helps
2. Try reordering cases (hot cases first: opLD, opSD, opC_ADDI, opADD, opBNE)
3. Worst case: revert and investigate compiler-generated assembly
