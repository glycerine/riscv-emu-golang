# go_regalloc: Register Allocation for Go-Compatible JIT

## Context

The abjit trampoline achieves 2.8ns entry/exit and 4ns with Go callbacks,
12x faster than CGO. Now we must design the register allocation that maps
31 live RISC-V guest registers onto x86_64 host registers, accounting for:

1. **Go compatibility**: No callee-saved registers in Go ABIInternal. After
   any Go callback, every register except RSP, RBP (frame pointer protocol),
   R14 (g) is destroyed.
2. **Sandbox containment**: JIT code uses a parallel sandbox stack (R12),
   never writes to RSP-relative memory (except [RSP+0] resume slot).
3. **Maximum amd64 execution speed**: Minimize spills to memory. Register
   file access ([RBP+r*8]) is ~5 cycles per load/store; keeping hot values
   in host registers saves this on every use.

---

## 1. x86_64 Register Map (16 registers)

### Fixed by Go runtime (3 registers — UNTOUCHABLE)

| Register | Role | Why |
|----------|------|-----|
| RSP | Go stack pointer | Frame, callbacks, runtime |
| R14 | Goroutine pointer (g) | GC, stack growth, scheduler |
| RBP | Frame pointer | Go's PUSH/POP protocol preserves caller's RBP across calls; we exploit this to pin the register file base here |

### Pinned by abjit (2 registers)

| Register | Role | Lifetime |
|----------|------|----------|
| R9 | CPU state pointer (`&CPU`) | Set by trampoline; explicitly restored by gocall handler after every Go callback |
| R12 | Sandbox stack pointer | Set by trampoline; survives Go callbacks via frame-pointer-like push/pop protocol (callee-saved by convention in most generated code, but NOT guaranteed by Go — see below) |

**R12 safety note**: Go does NOT guarantee R12 preservation. Our Phase 0
test saw it preserved, but that was luck. For correctness, the gocall
handler must restore R12 from the stack (like it does for R9), OR we accept
that R12 is destroyed by callbacks and reload it from `sandboxSP+16(FP)`.

**Decision**: Add R12 restoration to the gocall handler:
```asm
gocall:
    CALL R10
    MOVQ cpuState+8(FP), R9      ; restore R9
    MOVQ sandboxSP+16(FP), R12   ; restore R12
    JMP (SP)
```

### Staging (2 registers — scratch, never hold guest state across instructions)

| Register | Role | Why dedicated |
|----------|------|---------------|
| RAX | Staging A | Implicit in MUL/DIV/MOVABS; used as temporary for address computation, spill/reload shuttle |
| RCX | Staging B | Implicit in SHL/SHR/SAR (CL); used as shift amount, secondary scratch |

### Allocatable pool (9 registers — for guest RISC-V register values)

| Register | Notes |
|----------|-------|
| RDX | Clobbered by MUL/DIV (RDX:RAX); allocator saves/restores around these ops |
| RBX | General purpose |
| RSI | General purpose |
| RDI | General purpose |
| R8 | General purpose |
| R10 | Also used by callback emit sequence; spilled before callbacks |
| R11 | Also used by callback emit sequence; spilled before callbacks |
| R13 | General purpose |
| R15 | General purpose |

**Total: 9 integer registers** for mapping 31 live RISC-V registers.

### FP registers (unchanged from rv8)

| Registers | Role |
|-----------|------|
| XMM0-XMM13 | Allocatable pool (14 FP registers for f0-f31) |
| XMM14 | FP staging A |
| XMM15 | FP staging B |

---

## 2. Three Calling Conventions

### Convention 1: JIT→JIT block transition (HOT PATH)

Both sides are our generated code. We define the rules.

**Register state at block boundary:**
- RBP = register file base (always valid)
- R9 = CPU state pointer (always valid)
- R12 = sandbox stack pointer (always valid)
- All 9 pool registers: **dirty guest regs written back to [RBP+r*8]**
- The next block loads whatever guest regs it needs in its prologue

**Transition sequence (direct chain):**
```asm
; Block A epilogue — write back dirty guest regs
MOV [RBP+rs1*8], host_reg_1    ; ~1 cycle per store
MOV [RBP+rs2*8], host_reg_2
...
; Update PC in register file or sret
JMP block_B_entry               ; 0 cycles (predicted)
```

**Block B prologue — load needed guest regs:**
```asm
; Load guest regs this block uses
MOV host_reg_1, [RBP+rd1*8]    ; ~4-5 cycles per load (L1 hit)
MOV host_reg_2, [RBP+rd2*8]
...
; Execute instructions
```

**Cost**: N stores + M loads, where N = dirty regs in block A, M = regs
needed by block B. Typically 3-7 stores + 3-7 loads = 6-14 instructions,
~10-25 cycles. At 2.3GHz: ~4-11ns per block transition.

**Future optimization — trace linking**: When blocks A and B are linked in a
hot trace, the allocator can arrange for shared guest regs to stay in the
same host registers, eliminating the store-load pairs. This requires
cross-block allocation (not per-block). Deferred to Phase 3+.

### Convention 2: JIT→JIT function call via sandbox stack

For RISC-V JAL/JALR that we want to execute as native calls:

```asm
; Caller — Block A does RISC-V JAL:
MOV [RBP+1*8], return_pc        ; save ra (x1) to register file
; Write back all dirty guest regs
MOV [RBP+rs1*8], host_1
...
; Push return address on sandbox stack
LEA RAX, [RIP+resume_offset]
SUB R12, 8
MOV [R12], RAX
; Jump to callee block
JMP block_callee_entry

; ... callee executes, then does RISC-V JALR x1 (return):
; Write back dirty regs
...
; Pop return from sandbox stack
MOV RAX, [R12]
ADD R12, 8
JMP RAX                          ; returns to caller's resume point

resume_point:
; Reload guest regs needed after the call
MOV host_1, [RBP+rs1*8]
...
```

**Cost**: Same as block transition + 2 sandwich stack ops (~2 cycles).
Approximately same as Convention 1.

### Convention 3: JIT→Go callback (SLOW PATH)

All pool registers are destroyed. Full spill and reload.

```asm
; === SPILL: write ALL live guest regs back ===
MOV [RBP+g1*8], host_reg_1
MOV [RBP+g2*8], host_reg_2
... (up to 9 stores)

; === CALLBACK: 34-byte emit sequence ===
MOVABS gocallAddr, R11
LEA    R10, [RIP+17]
MOV    [RSP], R10
MOVABS goFuncAddr, R10
JMP    R11
; --- gocall: CALL R10; restore R9,R12; JMP (SP) ---

; === RELOAD: load guest regs needed after callback ===
MOV host_reg_1, [RBP+g1*8]
MOV host_reg_2, [RBP+g2*8]
... (up to 9 loads)
```

**Cost**: 9 stores + callback(~4ns) + 9 loads = ~18 instructions + 4ns
≈ 12-18ns total. Still 2-3x faster than CGO (34ns).

**Optimization**: The allocator knows which regs are actually live at the
callback point. Dead regs don't need spilling. If only 3 regs are live:
3 stores + callback + 3 loads ≈ 7-10ns.

### Convention 4: Go→JIT exported function call

Go calls a JIT function via closure. The closure marshals through state:

```go
func ExportRISCVFunc(block uintptr, state *State) func(a0, a1 uint64) uint64 {
    return func(a0, a1 uint64) uint64 {
        state.X[10] = a0   // RISC-V a0 = x10
        state.X[11] = a1   // RISC-V a1 = x11
        callJIT(block, uintptr(unsafe.Pointer(state)), state.SandboxSP)
        return state.X[10] // result in a0
    }
}
```

**Cost**: 2 memory writes + callJIT(2.8ns) + block execution + 1 memory
read ≈ 5-8ns overhead. The Go closure is a normal Go function value —
passes type checks, works with interfaces, can be stored in maps, etc.

---

## 3. Memory Access Strategy

### Primary: Inline mask-based (no callback, no Go involvement)

```asm
; Load64: result = *(memBase + (guestAddr & memMask))
MOV  RAX, guestAddr_host_reg     ; RAX = guest address
AND  RAX, [RBP+528]              ; RAX &= memMask (at rfMemMaskOff)
ADD  RAX, [RBP+520]              ; RAX += memBase (at rfMemBaseOff)
MOV  dest_host_reg, [RAX]        ; load from host address
```

memBase and memMask are stored at fixed offsets from RBP:
- `[RBP+520]` = memBase (rfMemBaseOff, set in block prologue)
- `[RBP+528]` = memMask (rfMemMaskOff, set in block prologue)

**Decision**: Keep memBase/memMask in memory, NOT in dedicated registers.
This preserves all 9 pool registers for guest register mapping. The memory
access pattern (AND + ADD from [RBP+offset]) hits L1 cache on every use
since the register file region is hot. Cost: ~1-2 extra cycles per
load/store vs having them in registers. Worth it for 2 more guest registers
in the pool.

### Exceptional: Callback for faults and MMIO

When the inline access detects a fault condition (alignment error, address
outside guest range), the JIT code branches to a slow path that:
1. Spills live regs
2. Calls a Go fault handler via callback
3. Reloads regs and continues (or exits block with fault status)

---

## 4. Register Allocator Interface

### ABJITPool (new function in ir/lower_amd64.go)

```go
func ABJITPool(_ *Block) RegPool {
    intRegs := []int16{
        goasm.REG_AMD64_DX,   // RDX (save/restore around MUL/DIV)
        goasm.REG_AMD64_BX,   // RBX
        goasm.REG_AMD64_SI,   // RSI
        goasm.REG_AMD64_DI,   // RDI
        goasm.REG_AMD64_R8,   // R8
        goasm.REG_AMD64_R10,  // R10 (clobbered by callback sequence)
        goasm.REG_AMD64_R11,  // R11 (clobbered by callback sequence)
        goasm.REG_AMD64_R13,  // R13
        goasm.REG_AMD64_R15,  // R15
    }
    fpRegs := []int16{
        goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2,
        goasm.REG_AMD64_X3, goasm.REG_AMD64_X4, goasm.REG_AMD64_X5,
        goasm.REG_AMD64_X6, goasm.REG_AMD64_X7, goasm.REG_AMD64_X8,
        goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
        goasm.REG_AMD64_X12, goasm.REG_AMD64_X13,
    }
    return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}
```

### ABJITPinned (new function in ir/lower_amd64.go)

```go
func ABJITPinned() map[VReg]int16 {
    return map[VReg]int16{
        VRRegFile: goasm.REG_AMD64_BP,  // register file base
    }
}
```

R9 and R12 are NOT in the pinned map because they're not tracked as VRegs
by the allocator. They're implicit context, like RSP. The trampoline sets
them; the gocall handler restores them. JIT code never assigns guest
register values to them.

### Excluded registers (NOT in pool, NOT pinned — just absent)

- RAX (staging A)
- RCX (staging B)
- RSP (Go stack)
- RBP (pinned to VRRegFile — appears in pinned map, not pool)
- R9 (CPU state pointer — implicit)
- R12 (sandbox stack — implicit)
- R14 (goroutine pointer — Go runtime)

### Allocation strategy

The existing `FixedStaticAllocator` in `ir/regalloc_fixed.go` handles:
- Liveness analysis per block
- Assigning VRegs to host registers from the pool
- Spilling to stack slots when pressure exceeds pool size
- RegMove insertion at control flow edges

This allocator works with ABJITPool unchanged. The pool is just smaller
(9 vs 12 integer registers). The allocator automatically spills the
least-used VRegs when pressure exceeds 9.

---

## 5. Block Prologue and Epilogue

### Prologue (emitted at block entry)

```asm
; RBP = register file base (set by trampoline or previous block)
; R9 = CPU state pointer
; R12 = sandbox stack pointer

; Load memBase/memMask into register file cache slots
; (already there from previous block or trampoline setup)

; Load allocated RISC-V integer regs from register file
MOV host_reg_1, [RBP + vreg1 * 8]
MOV host_reg_2, [RBP + vreg2 * 8]
...
; Load allocated FP regs
MOVSD xmm_reg, [RBP + 256 + fvreg * 8]
...
```

### Epilogue (emitted at block exit)

```asm
; Write back dirty guest regs to register file
MOV [RBP + vreg1 * 8], host_reg_1
MOV [RBP + vreg2 * 8], host_reg_2
...
; Write back dirty FP regs
MOVSD [RBP + 256 + fvreg * 8], xmm_reg
...

; Write exit info
; Option A: return to Go via exit sequence
;   MOV [R9+pc_offset], next_pc
;   MOV [R9+cycle_offset], cycle_count
;   <exit sequence: restore callee-saves, ADD RSP, POP BP, RET>

; Option B: chain to next block
;   JMP next_block_entry
```

---

## 6. Trampoline Update (R12 restoration)

The gocall handler must restore R12 after callbacks, since Go does NOT
guarantee R12 preservation:

```asm
gocall:
    CALL R10
    MOVQ cpuState+8(FP), R9        ; restore R9
    MOVQ sandboxSP+16(FP), R12     ; restore R12
    JMP (SP)
```

This adds one instruction (~1ns) to the callback path. Total callback
cost: ~5ns (was ~4ns).

---

## 7. Comparison: rv8 vs go_regalloc

| Aspect | rv8 (current) | go_regalloc (new) |
|--------|---------------|-------------------|
| Integer pool | 12 regs | 9 regs |
| Excluded | RAX, RCX, RBP, RSP | RAX, RCX, RBP, RSP, R9, R12, R14 |
| Pinned | RBP→VRRegFile | RBP→VRRegFile |
| Callbacks | N/A (block exits for all Go interaction) | JIT→Go: full spill/reload (~12-18ns) |
| Block transitions | RET to trampoline, re-enter via CALL | JMP to next block (0 cycles) or sandbox stack |
| Go→JIT | Via CGO + trampoline (~34ns) | Via closure + callJIT (~6ns) |
| Memory access | Inline mask-based | Same inline mask-based |
| R14 (g) | In pool (safe because no callbacks) | Excluded (required for callbacks) |
| GC safety | Relies on CGO boundary | NO_LOCAL_POINTERS + frame pointer |

**Net register loss**: 3 registers (R9, R12, R14). But we gain:
- 12x faster entry/exit (2.8ns vs 34ns CGO)
- Direct Go callbacks from JIT (4-5ns)
- Go-exported JIT functions via closures
- Sandbox stack for inter-block calls
- GC safety during JIT execution

---

## 8. Implementation Steps

### Step 1: Update trampoline (add R12 restoration to gocall)

File: `~/ris/abjit/trampoline_amd64.s`
- Add `MOVQ sandboxSP+16(FP), R12` after the R9 restore in gocall
- Re-run Phase 0 tests to verify

### Step 2: Add ABJITPool and ABJITPinned

File: `~/ris/ir/lower_amd64.go`
- Add the two functions as defined in Section 4
- Add unit tests verifying pool size, exclusions, pinned map

### Step 3: Create ABJIT lowering pass

File: `~/ris/ir/lower_amd64_abjit.go` (new)
- Fork from `lower_amd64_rv8.go`
- Modify prologue: no sret buffer, just load regs from [RBP+r*8]
- Modify epilogue: write back to [RBP+r*8], then exit sequence
- Add callback emit support: spill live regs → callback sequence → reload
- Use ABJITPool instead of RV8Pool

### Step 4: Create CodeBuilder with callback-aware emission

File: `~/ris/abjit/emit.go`
- Promote codeBuilder from test to exported API
- Add `SpillBeforeCallback()` and `ReloadAfterCallback()`
- Add block prologue/epilogue emission
- Track which host registers hold which guest regs (for spill)

### Step 5: Wire up to existing IR pipeline

File: `~/ris/abjit/abjit.go`
- `State` struct wrapping CPU fields (heap-allocated)
- `Run(state, pc)` — emit IR → allocate → lower with ABJIT → assemble → callJIT
- `Export(block, state)` — return Go closure wrapping a JIT block

### Step 6: Benchmarks

- `BenchmarkABJIT_ADD` — tight loop of RISC-V ADD instructions
- `BenchmarkABJIT_LoadStore` — memory access intensive block
- `BenchmarkABJIT_Callback` — block with Go callbacks
- `BenchmarkABJIT_BlockChain` — multiple blocks chained via JMP
- Compare all against existing rv8 path

---

## 9. Verification

```bash
# Unit tests for pool/pinned
cd ~/ris && go test -v -run TestABJIT ./ir/

# Phase 0 re-verification with R12 restoration
cd ~/ris/abjit && go test -v

# Full benchmark comparison
cd ~/ris/abjit && go test -run='^$' -bench=. -benchtime=3s
```

---

## 10. Files to create/modify

| File | Action | Purpose |
|------|--------|---------|
| `~/ris/abjit/trampoline_amd64.s` | Modify | Add R12 restore to gocall |
| `~/ris/ir/lower_amd64.go` | Modify | Add ABJITPool, ABJITPinned |
| `~/ris/ir/lower_amd64_abjit.go` | Create | ABJIT lowering pass |
| `~/ris/abjit/emit.go` | Create | CodeBuilder with callback support |
| `~/ris/abjit/abjit.go` | Create | State, Run, Export API |
| `~/ris/abjit/abjit_test.go` | Modify | Phase 1-3 tests + benchmarks |
