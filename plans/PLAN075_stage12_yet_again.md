# Plan: Complete Stage 12 — CISC Memory Operands

## Context

Stage 12 CISC Memory Operands is ~30% complete. The CMP direction bug is fixed, `rv8Binop`/`rv8Set`/`rv8Branch` use `spilledMemOp`, but the highest-impact operations are untouched. Current benchmark: host=1502 bytes (bloat test), MIPS ~2940. All infrastructure helpers (`spilledMemOp`, `directReg`, `emitRM`/`emitMR`/`emitMI`) are ready — the operations just don't call them yet.

**File**: `ir/lower_amd64_rv8.go` (all changes)

---

## Step 1: rv8BinopImm — `OP [mem], imm` when dst==A spilled (CRITICAL)

`AddImm` is the most frequent IR opcode. Currently always stages through RAX when operands are spilled.

**Current** (line 935-967): Has fast path for `dstHR >= 0 && aHR >= 0`, otherwise stages A to RAX, applies op, writes back.

**Add** before the slow path (before line 954):

```go
// CISC: dst==A both spilled → operate directly on memory.
if ins.Dst == ins.A {
    if base, off, ok := lc.spilledMemOp(ins.Dst); ok {
        imm := ins.Imm
        if imm >= -(1<<31) && imm < (1<<31) {
            lc.emitMI(op, imm, base, off)
            return
        }
    }
}
```

Also add a CISC path for dst in register, A spilled (common):
```go
if dstHR >= 0 {
    if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
        lc.emitRM(x86.AMOVQ, aBase, aOff, dstHR)
        imm := ins.Imm
        if imm >= -(1<<31) && imm < (1<<31) {
            lc.emitRI(op, imm, dstHR)
        } else {
            lc.loadImm(imm, rv8StgB)
            lc.emit2(op, rv8StgB, dstHR)
        }
        lc.commitDst(ins.Dst, dstHR)
        return
    }
}
```

**Savings**: 14 bytes per call (eliminates stage-A MOV + commit MOV).

---

## Step 2: rv8Mov — Direct spill↔reg moves (HIGH)

**Current** (line 821-838): Fast path for both in register, otherwise always stages.

**Add** between the fast-path return and the slow path (after line 830):

```go
// CISC: dst in register, A spilled → load directly.
if dstHR >= 0 {
    if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
        lc.emitRM(x86.AMOVQ, aBase, aOff, dstHR)
        lc.commitDst(ins.Dst, dstHR)
        return
    }
}
// CISC: A in register, dst spilled → store directly.
if aHR >= 0 {
    if dBase, dOff, ok := lc.spilledMemOp(ins.Dst); ok {
        lc.emitMR(x86.AMOVQ, aHR, dBase, dOff)
        return
    }
}
```

**Savings**: 7 bytes per call (eliminates staging MOV).

---

## Step 3: rv8Sext / rv8Zext — Load-with-extend from spill (HIGH)

**Current** (lines 846-896): Fast path for both in register, otherwise stages.

**Add** between the fast-path return and the slow path in each function:

```go
// CISC: dst in register, A spilled → load-with-extend directly.
if dstHR >= 0 {
    if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
        lc.emitRM(op, aBase, aOff, dstHR)
        lc.commitDst(ins.Dst, dstHR)
        return
    }
}
```

**Savings**: 7 bytes per call.

---

## Step 4: rv8ShiftImm — `SHL [mem], imm` when dst==A spilled (MEDIUM)

**Current** (line 1016-1035): Same structure as BinopImm.

**Add** before the slow path:

```go
if ins.Dst == ins.A {
    if base, off, ok := lc.spilledMemOp(ins.Dst); ok {
        lc.emitMI(op, ins.Imm, base, off)
        return
    }
}
```

Also add dst-in-reg, A-spilled path:
```go
if dstHR >= 0 {
    if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
        lc.emitRM(x86.AMOVQ, aBase, aOff, dstHR)
        lc.emitRI(op, ins.Imm, dstHR)
        lc.commitDst(ins.Dst, dstHR)
        return
    }
}
```

---

## Step 5: rv8LoadX / rv8StoreX — directReg for base/index (MEDIUM)

**Current** (lines 1259-1300): Always stages both base and index.

**rv8LoadX fix** — replace first two lines:
```go
base := lc.directReg(ins.A)
if base < 0 {
    base = lc.stageInt(ins.A, 0)
}
idx := lc.directReg(ins.B)
if idx < 0 {
    idx = lc.stageInt(ins.B, 1)
}
```

**rv8StoreX fix** — same pattern for base and index. Additionally check if the value register (`ins.Dst` for StoreX) can use directReg:
```go
base := lc.directReg(ins.A)
if base < 0 {
    base = lc.stageInt(ins.A, 0)
}
idx := lc.directReg(ins.B)
if idx < 0 {
    idx = lc.stageInt(ins.B, 1)
}
```

**Caution**: If both base and idx come from directReg and happen to be RAX or RCX (the staging registers), we might have a problem. But `directReg` excludes parameter VRegs and staging regs aren't in the pool, so this should be safe.

---

## Step 6: rv8Binop gate fix — handle dstHR==bHR (LOW)

**Current** (line 905): `if dstHR >= 0 && aHR >= 0 && dstHR != bHR`

The `dstHR != bHR` guard prevents clobbering B via `MOV aHR, dstHR`. But when `dstHR == aHR`, no MOV is emitted, so the guard is unnecessary.

**Fix**: Relax the gate:
```go
if dstHR >= 0 && aHR >= 0 && (dstHR != bHR || dstHR == aHR) {
```

---

## Verification

After each step:
```bash
cd ~/ris && go test ./ir/ -v -run 'TestRV8' 2>&1 | grep -E '=== RUN|PASS|FAIL'
```

After all steps:
```bash
cd ~/ris && go test ./...
cd ~/ris && go test -run TestBloat_BenchGuest_0x10de -v .
cd ~/ris && make bench
```

**Targets**: host bytes < 1400 (from 1502), MIPS > 3100 (from 2940)

## Helpers (all exist, just need to be called)

| Helper | Line | Signature |
|--------|------|-----------|
| `spilledMemOp(v)` | 495 | `(base int16, off int64, ok bool)` |
| `directReg(v)` | 666 | `int16` (-1 if unavailable) |
| `emitRM(op, base, off, dst)` | 721 | load/ALU from memory |
| `emitMR(op, src, base, off)` | 732 | store/ALU to memory |
| `emitMI(op, imm, base, off)` | 743 | immediate to memory |
