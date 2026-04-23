# Plan: Full rv8-Faithful Design — Incremental TDD Stages

## Design Scope

This plan covers the full rv8 design from the CARRV 2017 paper, broken into incremental stages. Each stage is independently committable with all tests green.

### rv8 features and GoCPU status

| rv8 Feature | Paper Section | GoCPU Status | Plan Stage |
|------------|---------------|-------------|------------|
| Static register allocation (12 regs) | §3.1, Table 1 | 7 regs currently | Stages 1-9 |
| RBP = register file base | §3.1, Table 2 | R12/R13/R14/R15 pinned | Stages 1-9 |
| RAX/RCX translator temps | §3.3, Table 2 | R10/R11 scratch | Stages 1-9 |
| CISC memory operands | §3.2, Figure 2 | Not implemented | Stage 12 |
| Macro-op fusion | §3.7, Table 3 | Peephole exists, no fusion | Stage 13 |
| L1 translation cache | §3.5, Figure 3 | JALR IC exists (2-way) | Stage 14 |
| Inline caching (call/return) | §3.6, Figure 4 | Block chaining exists | Stage 15 |
| Return address stack | §3.6 | Not implemented | Stage 15 |
| Branch tail dynamic linking | §3.8 | Already implemented (IRChainExit) | Done |
| Sign ext vs zero ext | §3.9 | Already implemented (IRSext/IRZext) | Done |
| Bit manipulation intrinsics | §3.10 | Already implemented (IRClz/IRCtz/IRBswap) | Done |
| Lazy sign extension | §3.9 future | Not implemented | Future |
| CISC load-op coalescing | §3.2 future | Not implemented | Future |

---

## Phase A: Register Layout (Stages 0-11)

### Stage 0: Baseline commit

Tag current commit. Verify: `go test ./ir/` and `go test ./...` green.

---

### Stage 1: New pool & pinned constants (parallel, no behavior change)

**Red test:**
```go
func TestRV8Pool(t *testing.T) {
    b := makeBlock()
    pool := RV8Pool(b)
    assert len(pool.IntRegs) == 12  // RDX,RBX,RSI,RDI,R8-R15
    assert no RAX,RCX,RBP,RSP in pool
}
func TestRV8Pinned(t *testing.T) {
    p := RV8Pinned()
    assert len(p) == 1   // only VRRegFile → RBP
}
```

**Green:** Add `RV8Pool()`, `RV8Pinned()`, `VRRegFile` constant. Old code untouched.

---

### Stage 2: intPriority reorder

**Red test:**
```go
func TestRV8Priority_Top12(t *testing.T) {
    // With 12-reg pool, ra,sp,t0,t1,a0-a7 get host regs; rest spill
    verify top 12 in priority are {1,2,5,6,10,11,12,13,14,15,16,17}
}
```

**Green:** Change `intPriority` to rv8 order. Existing structural tests still pass.

---

### Stage 3: Sret-on-stack prologue/epilogue (new lowerer, parallel)

**Red test:**
```go
func TestRV8Prologue_SretOnStack(t *testing.T) {
    // Trivial block through LowerAMD64_RV8 produces non-empty code
}
```

**Green:** Add `LowerAMD64_RV8()` with:
- `MOV RBP, RSI; SUB RSP, frame; MOV [RSP], RDI` (stash sret)
- Load 12 RISC-V regs from `[RBP+r*8]`
- Epilogue: store regs, load sret from stack, write results, RET
- Staging via RAX/RCX instead of R10/R11

---

### Stage 4: Basic ALU ops in new lowerer

**Red tests:** `TestRV8Lower_Add`, `_Sub`, `_Const`, `_Mov`, `_Sext`, `_Zext`

**Green:** Port ALU lowering to new context (RAX/RCX staging, RBP-relative spill).

---

### Stage 5: Memory, shifts, DIV/MUL in new lowerer

**Red tests:** `TestRV8Lower_Load`, `_Store`, `_Shl`, `_Div`, `_Rem`

**Green:**
- Guest memory: load mem_base/mem_mask from `[sret+128/136]` via stack-stashed sret pointer
- Shifts: RCX is temp, shift amount goes directly into CL
- DIV/MUL: save/restore RDX (RISC-V ra) locally around IDIVQ

---

### Stage 6: FP ops, comparisons, branches in new lowerer

**Red tests:** `TestRV8Lower_FAdd`, `_Branch`, `_Set`, `_Ret`, `_RetDyn`

**Green:** Port remaining ops. FP uses XMM0-XMM13 pool (XMM14/15 staging for V2 compat).

---

### Stage 7: Exhaustive register-pair tests

**Red tests:** `TestRV8Exhaust_ADD`, `_SUB`, `_SHL` — all O(N³) combos with 12-reg pool

**Green:** Adapt `execBlock` helper to use new lowerer. This is the "no aliasing" gate.

---

### Stage 8: Chain exit/entry with sret-on-stack

**Red tests:**
```go
func TestRV8Chain_SretCarriedInRAX(t *testing.T) {
    // Chain exit: load sret into RAX, MOVABS sentinel to RCX, JMP RCX
    // Chain entry: MOV [RSP], RAX (re-stash sret)
}
```

**Green:** Implement chain exit/entry in new lowerer. JALR IC uses same staging regs (RAX/RCX).

---

### Stage 9: New trampoline + wire up

**Red test:**
```go
func TestRV8Trampoline_RoundTrip(t *testing.T) {
    // Compile trivial block with LowerAMD64_RV8, call via CallRV8, verify results
}
```

**Green:**
- Add `CallRV8` trampoline in `call_rv8_amd64.s`
- Publishes memBase/memMask to sret buffer at [SP+128]/[SP+136]
- Saves/restores callee-saved BX,BP,R12-R15

**Then wire up:** Change `AMD64Pool`→`RV8Pool`, `AMD64Pinned`→`RV8Pinned`, `LowerAMD64`→`LowerAMD64_RV8`, `Call`→`CallRV8`.

**Gate:** ALL existing tests pass with new pipeline.

---

### Stage 10: Remove old code

Remove old `AMD64Pool`, `AMD64Pinned`, old trampolines, R10/R11 scratch constants, `BlockHasDivMul` pool-shrinking.

**Gate:** All tests pass, clean diff.

---

### Stage 11: CPU struct layout assertion

```go
func init() {
    assert unsafe.Offsetof(CPU{}.f) == unsafe.Offsetof(CPU{}.x) + 256
    assert unsafe.Offsetof(CPU{}.fcsr) == unsafe.Offsetof(CPU{}.x) + 512
}
```

**Gate:** Program starts without panic.

---

## Phase B: CISC Memory Operands (Stage 12)

### Stage 12: Embed spilled register references in ALU ops

rv8 §3.2: "rv8 makes use of CISC memory operands to access registers residing in the memory backed register spill area." Instead of:
```asm
MOV RAX, [RBP+0xF8]   ; load t4 from spill
XOR RAX, [RBP+0xF0]   ; XOR with t3 (also spilled)
MOV [RBP+0xF8], RAX   ; store result
```
Emit:
```asm
MOV RAX, qword ptr [RBP+0xF8]
XOR RAX, qword ptr [RBP+0xF0]
MOV qword ptr [RBP+0xF8], RAX
```
Or even better, when dst==src1 (both spilled):
```asm
XOR r13, qword ptr [RBP+0x50]  ; a5 (mapped) XOR s1 (spilled)
```

**Red tests:**
```go
func TestCISCMemOp_SpilledSrcInALU(t *testing.T) {
    // XOR mapped_reg, spilled_reg → single XOR with [RBP+offset] operand
    // Verify code is shorter than load+XOR+store sequence
}
```

**Green:** In the lowerer, when source operand is spilled, emit `[RBP+offset]` memory operand instead of staging through temp. This works for commutative ops (ADD, AND, OR, XOR) and loads.

**Why this matters:** Reduces instruction count and I-cache pressure. The paper notes this "helps the translator maintain instruction density which lowers instruction cache pressure and helps improve throughput."

**Compatibility:** This requires RBP to be the register file base (Stage 3 prerequisite). Without that, we can't use `[RBP+offset]` for spilled registers.

---

## Phase C: Macro-Op Fusion (Stage 13)

### Stage 13: RISC-V instruction fusion patterns

rv8 §3.7, Table 3 defines 10 fusion patterns. These are applied at the RISC-V→IR translation stage (in `jit_emit_ir.go`), NOT in the peephole optimizer. The peephole works on IR instructions; fusion works on RISC-V instruction pairs/triples before they become IR.

**Fusion patterns from Table 3:**

| Pattern | Fused Result | x86 Expansion |
|---------|-------------|---------------|
| AUIPC r1, imm20; ADDI r1, r1, imm12 | `la r1, addr` | Single MOV |
| AUIPC r1, imm20; JALR ra, imm12(r1) | `call addr` | Single MOV (direct call) |
| AUIPC ra, imm20; JALR ra, imm12(ra) | `call addr` (write elided) | Elide target reg write |
| AUIPC r1, imm20; LW r1, imm12(r1) | `lw r1, [addr]` | MOV with imm addressing |
| AUIPC r1, imm20; LD r1, imm12(r1) | `ld r1, [addr]` | MOV with imm addressing |
| SLLI r1, r1, 32; SRLI r1, r1, 32 | `zext.w r1` | MOVZX (32→64) |
| ADDIW r1, imm12; SLLI r1, r1, 32; SRLI r1, r1, 32 | `addiwz` | 32-bit zero-extending ADD |
| SRLI+SLLI+OR (rotate patterns) | `ror`/`rol` | ROR/ROL with residual SHL/SHR |

**Red tests (one per pattern):**
```go
func TestFusion_AUIPC_ADDI(t *testing.T) {
    // Two RISC-V instructions fused into single IR Const+Mov
    code := []uint32{auipc(x5, 0x12345), addi(x5, x5, 0x678)}
    // Emit IR, verify single IRConst instead of AUIPC+ADDI pair
}
func TestFusion_SLLI_SRLI_ZextW(t *testing.T) {
    code := []uint32{slli(x10, x10, 32), srli(x10, x10, 32)}
    // Verify fused to single IRZext I32
}
// ... etc for each pattern
```

**Green:** Add a fusion pass in `jit_emit_ir.go` that looks ahead 1-2 RISC-V instructions. When a fusable pattern is detected, emit the fused IR instead of the individual instruction IRs.

**Why this is separate from the peephole:** The peephole operates on IR instructions (which are already lowered from RISC-V). Fusion operates on RISC-V instruction pairs BEFORE IR emission. The peephole can't see the original RISC-V encoding, so it can't recognize AUIPC+ADDI as a load-address pattern.

**Compatibility:** Fusion is independent of the register layout — it works at the IR emission level. Stages 1-11 are not prerequisites, but the CISC memory operands (Stage 12) make fused patterns more efficient because the fused constant can be embedded in the addressing mode.

---

## Phase D: Translation Cache & Inline Caching (Stages 14-15)

### Stage 14: L1 translation cache (sparse direct-mapped)

rv8 §3.5, Figure 3: A 1024-entry direct-mapped cache indexed by `bits[10:1]` of the guest PC. This accelerates indirect branches (JALR) by providing O(1) lookup of guest PC → native code address.

GoCPU already has a 2-way JALR inline cache (`IRJalrIC`). The L1 translation cache is a broader mechanism — it covers ALL indirect branches, not just specific JALR sites.

**Red test:**
```go
func TestTranslationCache_HitAfterCompile(t *testing.T) {
    // Compile a block at guest PC 0x1000
    // Lookup 0x1000 in translation cache → hit, returns native addr
}
func TestTranslationCache_MissFallback(t *testing.T) {
    // Lookup unknown PC → miss, returns sentinel/zero
}
func TestTranslationCache_IndexBits(t *testing.T) {
    // Verify index = pc[10:1] (1024 entries, ignoring bit 0)
}
```

**Green:** Add `TranslationCache` struct (1024-entry array of `{guestPC, nativeAddr}` pairs). Integrate with block compilation — when a block is compiled, insert into cache. JALR fallback path consults cache before falling back to hashtable.

**Compatibility:** Independent of register layout. Can be done before or after Phase A.

---

### Stage 15: Return address stack optimization

rv8 §3.6: When a procedure call is inlined, the return (`JALR zero, ra`) compares `ra` (RDX in rv8 layout) against the expected return address. If it matches, control flows directly to the return site without consulting the translation cache.

**Red test:**
```go
func TestReturnAddrStack_FastReturn(t *testing.T) {
    // Compile caller (JAL ra, func) + callee (JALR zero, ra)
    // Verify the return generates CMP RDX, <expected_addr>; JE <return_site>
}
```

**Green:** In the emitter, when emitting JALR zero,ra after a known call site, emit:
```asm
CMP RDX, <expected_return_addr>   ; ra == expected?
JNE slow_path                      ; no → translation cache lookup
; yes → fall through to return site
```

**Compatibility:** Requires ra mapped to a host register (RDX in our layout). Stage 9 is prerequisite.

---

## Phase E: Future Optimizations (not staged yet)

These are mentioned in rv8 §5 as future work. Not planned for implementation now, but the architecture must not prevent them.

| Optimization | rv8 Section | Compatibility Notes |
|-------------|-------------|-------------------|
| Dynamic register allocation (live range splitting) | §5 | RBP-relative spill addressing supports this — spill slots are at known offsets |
| CISC load-op coalescing (ALU with memory source) | §3.2, §5 | Stage 12 lays groundwork; full coalescing folds loads INTO ALU instructions |
| Register write elision (dead writes to temps) | §3.7 | Peephole already does some of this; fusion pass can elide AUIPC temp writes |
| Deoptimization (restore state on fault) | §3.7 | Requires register mapping to be reversible — our layout supports this via [RBP+offset] |
| Hardware MMU shadow paging | §5 | Independent of register layout |
| Lazy sign extension | §3.9 | Independent of register layout; type tracking in IR |

**Key architectural invariant:** All spilled RISC-V registers are at `[RBP + r*8]` (integer) or `[RBP + 256 + r*8]` (FP). This is the foundation that enables:
- CISC memory operands (embed spill address in ALU instructions)
- Dynamic register allocation (spill/reload at known offsets)
- Deoptimization (reconstruct full register state from memory + host regs)
- Fusion with write elision (known spill locations for dead-write detection)

---

## Summary: Stage Dependencies

```
Stage 0 (baseline)
  └─ Stage 1 (new pool/pinned)
       └─ Stage 2 (intPriority)
            └─ Stage 3 (new lowerer prologue/epilogue)
                 ├─ Stage 4 (ALU ops)
                 ├─ Stage 5 (mem/shift/div)
                 ├─ Stage 6 (FP/branch/ret)
                 │    └─ Stage 7 (exhaustive combos)
                 └─ Stage 8 (chain exit/entry)
                      └─ Stage 9 (trampoline + wire up)
                           └─ Stage 10 (remove old code)
                                └─ Stage 11 (layout assertion)

Stage 12 (CISC mem operands) ← requires Stage 9 (RBP = reg file base)
Stage 13 (macro-op fusion)   ← independent, can parallelize with Phase A
Stage 14 (translation cache) ← independent
Stage 15 (return addr stack)  ← requires Stage 9 (ra in host register)
```

## Test Gate Summary

| Stage | Gate |
|-------|------|
| 0 | `go test ./...` green (baseline) |
| 1 | TestRV8Pool, TestRV8Pinned + all existing |
| 2 | TestRV8Priority + all existing |
| 3 | TestRV8Prologue + all existing |
| 4 | TestRV8Lower_{Add,Sub,Const,Mov} + all existing |
| 5 | TestRV8Lower_{Load,Store,Shl,Div} + all existing |
| 6 | TestRV8Lower_{FAdd,Branch,Set,Ret} + all existing |
| 7 | TestRV8Exhaust_{ADD,SUB,SHL} + all existing |
| 8 | TestRV8Chain + all existing |
| 9 | TestRV8Trampoline + ALL existing (swap gate) |
| 10 | All tests pass (cleanup) |
| 11 | All tests pass + init() guard |
| 12 | TestCISCMemOp + all existing |
| 13 | TestFusion_{AUIPC_ADDI, SLLI_SRLI, ...} + all existing |
| 14 | TestTranslationCache + all existing |
| 15 | TestReturnAddrStack + all existing |
