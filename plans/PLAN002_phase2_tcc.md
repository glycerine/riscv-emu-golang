# Phase 2b/2c: FP JIT + Zbb/Zbs/Zba Gap Fill + Block Chaining

## Context

Phase 2a (RVC integer) is complete. JIT now handles all integer compressed instructions.
- **Performance**: 1249 MIPS JIT vs 153 MIPS interpreter (8.2x speedup)
- **Coverage**: RV64I, M-extension, Zbb (partial), Zicond, full RVC integer

This plan covers: (1) FP instruction emission, (2) Zbb/Zbs/Zba gap fill, (3) block chaining, (4) C output optimizations. All following libriscv's `tr_emit.cpp` as correctness oracle.

## Audit Summary: JIT vs Interpreter Gaps

### Missing in emit32 (jit_emit.go)

| Opcode | Category | Instructions |
|--------|----------|-------------|
| 0x07 | FP Load | FLW, FLD |
| 0x27 | FP Store | FSW, FSD |
| 0x43/47/4B/4F | FP Fused | FMADD/FMSUB/FNMADD/FNMSUB (.S/.D) |
| 0x53 | FP Arith | FADD/FSUB/FMUL/FDIV/FSQRT, FSGNJ/N/X, FMIN/FMAX, FEQ/FLT/FLE, FCVT, FMV |

### Missing Zbb/Zbs/Zba in emitOp (opcode 0x33)

| funct7 | Instructions | C Pattern |
|--------|-------------|-----------|
| 0x04 | ZEXT.H | `rd = rs1 & 0xFFFF` |
| 0x10 | SH1ADD/SH2ADD/SH3ADD | `rd = rs2 + (rs1 << N)` |
| 0x14 | BSET | `rd = rs1 \| (1 << (rs2 & 63))` |
| 0x24 | BCLR, BEXT | `rd = rs1 & ~(1<<(rs2&63))`, `rd = (rs1>>(rs2&63)) & 1` |
| 0x30 | ROL, ROR | rotate left/right |
| 0x34 | BINV | `rd = rs1 ^ (1 << (rs2 & 63))` |
| 0x35 | REV8 | byte swap |
| 0x60 | CLZ, CTZ, CPOP | count leading/trailing zeros, popcount |

### Missing Zbb/Zbs in emitOpImm (opcode 0x13)

Currently only handles SLLI (funct3=1) and SRLI/SRAI (funct3=5). Missing:
- **funct3=1**: BSETI, BCLRI, BINVI, CLZ/CTZ/CPOP/SEXT.B/SEXT.H
- **funct3=5**: BEXTI, RORI, ORC.B, REV8, ZEXT.H

### Missing Zba/Zbb in emitOp32 (opcode 0x3B)

| funct7 | Instructions |
|--------|-------------|
| 0x04 | ADD.UW |
| 0x10 | SH1ADD.UW, SH2ADD.UW, SH3ADD.UW |
| 0x30 | ROLW, RORW |
| 0x60 | CLZW, CTZW, CPOPW |

### Missing in emitOpImm32 (opcode 0x1B)

- SLLI.UW (funct7=0x04, funct3=1)
- RORIW (funct7=0x30>>1, funct3=5)

### RVC FP (currently bail)

C.FLD, C.FSD, C.FLDSP, C.FSDSP — trivial once FP load/store works.

---

## Step 1: C Output Audit — Quick Wins

**File: `jit_emit.go`**

### 1a. MV / LI special cases in emitOpImm funct3=0

Current: `r5 = r10 + 0LL;` for MV. Change to:
```go
case 0: // ADDI
    if imm == 0 {
        if rs1 == 0 {
            e.emit("    %s = 0;\n", d)           // LI rd, 0
        } else {
            e.emit("    %s = %s;\n", d, s)        // MV rd, rs1
        }
    } else if rs1 == 0 {
        e.emit("    %s = %dLL;\n", d, imm)        // LI rd, imm
    } else {
        e.emit("    %s = %s + %dLL;\n", d, s, imm) // ADDI
    }
```

### 1b. Same for ADDIW (emitOpImm32 funct3=0)

---

## Step 2: Zbb/Zbs/Zba Gap Fill

**File: `jit_emit.go`**

### 2a. Inline math helpers in finalize() header

Add these (only when `e.usesZbbHelpers` is true):
```c
static int jit_clz64(uint64_t x) {
    if (!x) return 64; int n=0;
    if (!(x&0xFFFFFFFF00000000ULL)){n+=32;x<<=32;}
    if (!(x&0xFFFF000000000000ULL)){n+=16;x<<=16;}
    if (!(x&0xFF00000000000000ULL)){n+=8;x<<=8;}
    if (!(x&0xF000000000000000ULL)){n+=4;x<<=4;}
    if (!(x&0xC000000000000000ULL)){n+=2;x<<=2;}
    if (!(x&0x8000000000000000ULL)){n+=1;}
    return n;
}
// Similarly: jit_ctz64, jit_cpop64, jit_clz32, jit_ctz32, jit_cpop32
```

### 2b. emitOp additions (funct7 cases)

Add cases for 0x04, 0x10, 0x14, 0x24, 0x30, 0x34, 0x35, 0x60 matching the interpreter's `cpu.go:238-311`.

### 2c. emitOpImm refactor

Replace the simple funct3=1 and funct3=5 handlers with full Zbb/Zbs dispatch using `funct7 &^ 1` (bits[31:26]) as the discriminator, matching `cpu.go:155-186`.

### 2d. emitOp32 additions

Add funct7 cases 0x04, 0x10, 0x30, 0x60 matching `cpu.go:345-371`.

### 2e. emitOpImm32 additions

Add SLLI.UW (funct7=0x04) and RORIW (funct7>>1=0x30) matching `cpu.go:200-214`.

---

## Step 3: FP Infrastructure

**File: `jit_emit.go`**

### 3a. Emitter struct additions

```go
type emitter struct {
    // ... existing fields ...
    usesFP         bool  // any FP instruction emitted
    usesZbbHelpers bool  // CLZ/CTZ/CPOP emitted
}
```

### 3b. FP register access (NO caching for FP regs)

FP registers accessed directly through `uint64_t *f` parameter — no local caching.
```go
func (e *emitter) frd(r uint32) string { e.usesFP = true; return fmt.Sprintf("f[%d]", r) }
func (e *emitter) frs(r uint32) string { e.usesFP = true; return fmt.Sprintf("f[%d]", r) }
```

### 3c. FP header in finalize() (only when usesFP)

```c
typedef union { int32_t i32[2]; float f32[2]; int64_t i64; double f64; uint64_t u64; } fp64reg;

// NaN-boxing: upper 32 = 0xFFFFFFFF (matching our interpreter's convention)
static uint64_t box_f32(uint32_t bits) { return 0xFFFFFFFF00000000ULL | (uint64_t)bits; }
static uint32_t unbox_f32(uint64_t r) { return (r>>32)==0xFFFFFFFF ? (uint32_t)r : 0x7FC00000u; }

// Read/write helpers
static float rd_f32(uint64_t *f, int r)  { fp64reg t; t.i32[0]=unbox_f32(f[r]); return t.f32[0]; }
static double rd_f64(uint64_t *f, int r) { fp64reg t; t.u64=f[r]; return t.f64; }
static void wr_f32(uint64_t *f, int r, float v)  { fp64reg t; t.f32[0]=v; f[r]=box_f32(t.i32[0]); }
static void wr_f64(uint64_t *f, int r, double v) { fp64reg t; t.f64=v; f[r]=t.u64; }

// Math helpers (TCC-compatible, no libm)
static float  jit_fminf(float a, float b)  { if(a!=a)return b; if(b!=b)return a; if(a<b)return a; if(b<a)return b; fp64reg u,v; u.f32[0]=a; v.f32[0]=b; return (u.i32[0]&0x80000000)?a:b; }
static float  jit_fmaxf(float a, float b)  { if(a!=a)return b; if(b!=b)return a; if(a>b)return a; if(b>a)return b; fp64reg u,v; u.f32[0]=a; v.f32[0]=b; return (v.i32[0]&0x80000000)?a:b; }
static double jit_fmin(double a, double b)  { if(a!=a)return b; if(b!=b)return a; if(a<b)return a; if(b<a)return b; fp64reg u,v; u.f64=a; v.f64=b; return (u.u64>>63)?a:b; }
static double jit_fmax(double a, double b)  { if(a!=a)return b; if(b!=b)return a; if(a>b)return a; if(b>a)return b; fp64reg u,v; u.f64=a; v.f64=b; return (v.u64>>63)?a:b; }
```

### 3d. sqrtf/sqrt via tcc_add_symbol

**File: `jit_tcc.go`**

Add `tcc_add_symbol` calls before `tcc_relocate`:
```c
#include <math.h>
// In compile_block(), after tcc_compile_string and before tcc_relocate:
tcc_add_symbol(s, "jit_sqrtf", sqrtf);
tcc_add_symbol(s, "jit_sqrt", sqrt);
```

Declare in emitted header (when usesFP):
```c
extern float jit_sqrtf(float);
extern double jit_sqrt(double);
```

---

## Step 4: FP Load/Store (opcodes 0x07, 0x27)

**File: `jit_emit.go`**

### 4a. FP Load (opcode 0x07)

Add to emit32 switch:
```go
case 0x07: // FP LOAD
    e.emitFPLoad(rd, rs1, iimm, funct3)
    e.advancePC(4)
```

New function:
```go
func (e *emitter) emitFPLoad(rd, rs1 uint32, imm int64, funct3 uint32) {
    switch funct3 {
    case 2: // FLW
        // bounds check, then: f[rd] = box_f32(*(uint32_t*)(mem_base + (addr & mem_mask)));
    case 3: // FLD
        // bounds check, then: f[rd] = *(uint64_t*)(mem_base + (addr & mem_mask));
    default:
        e.terminated = true
    }
}
```

Memory access pattern reuses existing load bounds-check approach (alignment + mask check), but writes to `f[rd]` instead of integer register.

### 4b. FP Store (opcode 0x27)

```go
case 0x27: // FP STORE
    simm := sImm(insn)
    e.emitFPStore(rs1, rs2, simm, funct3)
    e.advancePC(4)
```

New function:
```go
func (e *emitter) emitFPStore(rs1, rs2 uint32, imm int64, funct3 uint32) {
    switch funct3 {
    case 2: // FSW — *(uint32_t*)(mem) = (uint32_t)f[rs2];
    case 3: // FSD — *(uint64_t*)(mem) = f[rs2];
    default:
        e.terminated = true
    }
}
```

### 4c. RVC FP Un-bail

Replace bail stubs in emitRVC_Q0 and emitRVC_Q2:
- C.FLD (Q0, funct3=1): decode compressed regs + CSD immediate, call `emitFPLoad(rd, rs1, uimm, 3)`
- C.FSD (Q0, funct3=5): decode compressed regs + CSD immediate, call `emitFPStore(rs1, rs2, uimm, 3)`
- C.FLDSP (Q2, funct3=1): decode CSFSD immediate, call `emitFPLoad(rd, 2, uimm, 3)`
- C.FSDSP (Q2, funct3=5): decode CSFSD immediate, call `emitFPStore(2, rs2, uimm, 3)`

Immediate extraction (from cpu.go stepRVC, verified against spec):
- CSD: `imm[5:3] = insn[12:10], imm[7:6] = insn[6:5]`
- CSFSD-load: `imm[5] = insn[12], imm[4:3] = insn[6:5], imm[8:6] = insn[4:2]`
- CSFSD-store: `imm[5:3] = insn[12:10], imm[8:6] = insn[9:7]`

---

## Step 5: FP Arithmetic (opcode 0x53)

**File: `jit_emit.go`**

Add to emit32:
```go
case 0x53:
    funct5 := insn >> 27
    fpfmt := (insn >> 25) & 0x3
    e.emitFPOp(rd, rs1, rs2, funct3, funct5, fpfmt)
    e.advancePC(4)
```

New function dispatches on fpfmt (0=single, 1=double) then funct5:

### 5a. Basic arithmetic (funct5 = 0x00-0x03, 0x0B)

| funct5 | .S pattern | .D pattern |
|--------|-----------|-----------|
| 0x00 FADD | `wr_f32(f, rd, rd_f32(f,rs1) + rd_f32(f,rs2))` | `wr_f64(f, rd, rd_f64(f,rs1) + rd_f64(f,rs2))` |
| 0x01 FSUB | same with `-` | same with `-` |
| 0x02 FMUL | same with `*` | same with `*` |
| 0x03 FDIV | same with `/` | same with `/` |
| 0x0B FSQRT | `wr_f32(f, rd, jit_sqrtf(rd_f32(f,rs1)))` | `wr_f64(f, rd, jit_sqrt(rd_f64(f,rs1)))` |

### 5b. Sign injection (funct5 = 0x04)

Bit manipulation — no FP arithmetic, no rounding. Uses unbox_f32 for .S:
```c
// FSGNJ.S (funct3=0): f[rd] = box_f32((unbox_f32(f[rs1]) & 0x7FFFFFFFu) | (unbox_f32(f[rs2]) & 0x80000000u));
// FSGNJN.S (funct3=1): ... | (~unbox_f32(f[rs2]) & 0x80000000u)
// FSGNJX.S (funct3=2): f[rd] = box_f32(unbox_f32(f[rs1]) ^ (unbox_f32(f[rs2]) & 0x80000000u));
```
For .D, use 64-bit masks directly on f[rs1]/f[rs2].

### 5c. FMIN/FMAX (funct5 = 0x05)

```c
// FMIN.S: wr_f32(f, rd, jit_fminf(rd_f32(f,rs1), rd_f32(f,rs2)));
// FMAX.S: wr_f32(f, rd, jit_fmaxf(rd_f32(f,rs1), rd_f32(f,rs2)));
```

### 5d. FCVT format conversion (funct5 = 0x08)

```c
// FCVT.S.D (fpfmt=0, rs2=1): wr_f32(f, rd, (float)rd_f64(f, rs1));
// FCVT.D.S (fpfmt=1, rs2=0): wr_f64(f, rd, (double)rd_f32(f, rs1));
```

### 5e. FP Comparisons (funct5 = 0x14) — write to integer x[rd]

```go
// FLE.S (funct3=0): rN = (rd_f32(f,rs1) <= rd_f32(f,rs2)) ? 1 : 0;
// FLT.S (funct3=1): rN = (rd_f32(f,rs1) < rd_f32(f,rs2)) ? 1 : 0;
// FEQ.S (funct3=2): rN = (rd_f32(f,rs1) == rd_f32(f,rs2)) ? 1 : 0;
```
Uses `e.rd(rd)` for integer register write. C NaN semantics (NaN comparisons return false) match RISC-V.

### 5f. FCVT float-to-int (funct5 = 0x18) — write to integer x[rd]

Two approaches:
- **Simple**: Use C cast with truncation. `x[rd] = (int64_t)(int32_t)rd_f32(f,rs1);` for FCVT.W.S
- **Correct**: Include saturation checks (NaN→max, overflow→saturate)

Go with **simple first** (C truncation matches RTZ rounding mode, the most common). Saturation edge cases will be caught by fuzz oracle and can be added as needed. We skip fflags in JIT.

| rs2 | Instruction | .S pattern |
|-----|-------------|-----------|
| 0 | FCVT.W.S | `rd = (int64_t)(int32_t)rd_f32(f,rs1);` |
| 1 | FCVT.WU.S | `rd = (int64_t)(int32_t)(uint32_t)rd_f32(f,rs1);` |
| 2 | FCVT.L.S | `rd = (int64_t)rd_f32(f,rs1);` |
| 3 | FCVT.LU.S | `rd = (uint64_t)rd_f32(f,rs1);` |

Same pattern for .D variants using `rd_f64`.

### 5g. FCVT int-to-float (funct5 = 0x1A) — read integer, write FP

```c
// FCVT.S.W:  wr_f32(f, rd, (float)(int32_t)rN);    // read cached integer reg
// FCVT.S.WU: wr_f32(f, rd, (float)(uint32_t)rN);
// FCVT.S.L:  wr_f32(f, rd, (float)(int64_t)rN);
// FCVT.S.LU: wr_f32(f, rd, (float)rN);
```

### 5h. FMV raw bit moves (funct5 = 0x1C, 0x1E)

```c
// FMV.X.W (0x1C, funct3=0): rN = (int64_t)(int32_t)(uint32_t)f[rs1];  // sign-extend lower 32
// FMV.X.D (0x1C, funct3=0, fpfmt=1): rN = f[rs1];
// FCLASS.S (0x1C, funct3=1): bail to interpreter
// FCLASS.D (0x1C, funct3=1, fpfmt=1): bail to interpreter
// FMV.W.X (0x1E): f[rd] = box_f32((uint32_t)rN);
// FMV.D.X (0x1E, fpfmt=1): f[rd] = rN;
```

---

## Step 6: FMADD Family (opcodes 0x43, 0x47, 0x4B, 0x4F)

```go
case 0x43, 0x47, 0x4B, 0x4F:
    rs3 := insn >> 27
    fpfmt := (insn >> 25) & 0x3
    e.emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt)
    e.advancePC(4)
```

Emitted C patterns (single-precision):

| Opcode | Instruction | C expression |
|--------|------------|-------------|
| 0x43 | FMADD.S | `rd_f32(f,rs1) * rd_f32(f,rs2) + rd_f32(f,rs3)` |
| 0x47 | FMSUB.S | `rd_f32(f,rs1) * rd_f32(f,rs2) - rd_f32(f,rs3)` |
| 0x4B | FNMSUB.S | `-(rd_f32(f,rs1) * rd_f32(f,rs2)) + rd_f32(f,rs3)` |
| 0x4F | FNMADD.S | `-(rd_f32(f,rs1) * rd_f32(f,rs2)) - rd_f32(f,rs3)` |

Note: These are NOT true fused multiply-add (separate multiply and add, with intermediate rounding). True FMA would need `fmaf()`/`fma()`. Acceptable for now — most programs don't depend on FMA precision. Can upgrade to true FMA via `tcc_add_symbol("jit_fmaf", fmaf)` later.

---

## Step 7: Block Chaining — Last-Block Cache

**File: `jit.go`**

Add single-entry "last block" cache to avoid map lookup for tight loops:

```go
type JIT struct {
    blocks     map[uint64]*compiledBlock
    noJIT      map[uint64]bool
    InterpOnly bool
    lastPC     uint64
    lastBlk    *compiledBlock
}
```

In RunJIT dispatch, before `j.blocks[pc]`:
```go
var blk *compiledBlock
if pc == j.lastPC && j.lastBlk != nil {
    blk = j.lastBlk
} else if b, ok := j.blocks[pc]; ok {
    blk = b
    j.lastPC = pc
    j.lastBlk = blk
}
```

Also update `jit.go:116` comment — remove "RVC" from exclusion list (now handled).

---

## Step 8: fflags Strategy

**No fflags tracking in JIT.** Rationale:
- The interpreter captures MXCSR flags per-op via custom x86 assembly (`internal/fenv`)
- Most programs don't read fflags
- If a program reads FCSR, it uses a CSR instruction → block terminates → falls back to interpreter where flags are tracked correctly
- libriscv also skips fflags in JIT path

---

## Implementation Order

| # | Task | Files | Risk |
|---|------|-------|------|
| 1 | MV/LI special case (audit) | jit_emit.go | None |
| 2 | Zbb/Zbs/Zba gap fill (emitOp, emitOpImm, emitOp32, emitOpImm32) | jit_emit.go | Low |
| 3 | Last-block cache | jit.go | None |
| 4 | FP infrastructure (struct, header, helpers) | jit_emit.go | Low |
| 5 | FP load/store + RVC FP un-bail | jit_emit.go | Low |
| 6 | sqrt via tcc_add_symbol | jit_tcc.go | Medium (cgo change) |
| 7 | FP arithmetic (FADD-FDIV, FSGNJ, FMIN/FMAX) | jit_emit.go | Low |
| 8 | FP comparisons + FMV | jit_emit.go | Medium (int←→FP reg) |
| 9 | FP conversions (FCVT) | jit_emit.go | Medium (saturation) |
| 10 | FMADD family | jit_emit.go | Low |
| 11 | Conditional header emission | jit_emit.go | None |

## Key Files

| File | Changes |
|------|---------|
| `jit_emit.go` | All emission: FP opcodes, Zbb/Zbs/Zba, MV optimization, conditional headers |
| `jit_tcc.go` | `tcc_add_symbol` for sqrtf/sqrt |
| `jit.go` | Last-block cache, update comment |
| `jit_test.go` | Unit tests per instruction category |
| `float.go` | Reference only (NaN-boxing convention: upper32 = 0xFFFFFFFF) |
| `cpu.go` | Reference only (interpreter FP dispatch at lines 540-741) |

## Verification

```bash
# After each step:
go test -run TestJIT -v                          # JIT unit tests
go test -run TestJIT_BenchGuest_Smoke -v ./bench/ # full ELF smoke test

# After FP:
go test -run TestJIT_FP -v                        # FP-specific tests

# Full regression:
go test -timeout 120s ./...

# Benchmark:
go test -run='^$' -bench='BenchmarkCPU_FullExecution' -benchtime=1x ./bench/
```
