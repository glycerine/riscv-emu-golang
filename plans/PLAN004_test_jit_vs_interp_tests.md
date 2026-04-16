# Comprehensive JIT Test Suite — Foundation for Instruction Budget

## Context

The JIT currently has only 2 unit tests (TestJIT_ADD, TestJIT_Fib). Before adding the instruction budget (semi-cooperative preemption at backward branches), we need thorough coverage of:
- Register writeback correctness (budget exit depends on this)
- Block exit → dispatch → re-entry path (budget triggers this on every backward branch)
- Memory operations through JIT (stores must be visible after writeback)
- Cycle counter accuracy (budget decision is `ic >= limit`)
- Fault handling (all RunJIT status code paths)
- Mixed JIT/interpreter transitions (budget exit falls to dispatch, may hit interpreter)
- JIT vs interpreter correctness parity (the fundamental invariant)

## Files to Modify

| File | Changes |
|------|---------|
| `jit.go` | Add `StepBlock` method for lockstep testing |
| `jit_test.go` | All new tests and helpers (same package, accesses internals) |
| `riscv_test.go` | Add `runRISCVTestJIT`, lockstep tests, `TestRISCVTests_*_JIT` |

## Shared Helpers

### `runJITWithOS` — JIT equivalent of RunWithOS

Goes in `jit_test.go`. Mirrors `RunWithOS` from os.go but calls `jit.RunJIT` instead of `RunWithChain`:

```go
func runJITWithOS(cpu *CPU) (exitCode int, err error) {
    o := NewOS()
    o.HandleSyscall(93, LinuxExit)
    o.HandleSyscall(94, LinuxExit)
    o.HandleEcall(RiscvTestsEcall)
    cpu.Notes.Push(o.Handle)
    defer cpu.Notes.Pop()

    defer func() {
        if r := recover(); r != nil {
            if ex, ok := r.(*ExitError); ok {
                exitCode = ex.Code
                err = nil
                return
            }
            panic(r)
        }
    }()

    jit := NewJIT()
    err = jit.RunJIT(cpu)
    return
}
```

### `cpuSnapshot` — full CPU state capture

```go
type cpuSnapshot struct {
    x    [32]uint64
    f    [32]uint64
    pc   uint64
    fcsr uint32
}

func takeCPUSnapshot(cpu *CPU) cpuSnapshot { ... }

func (a cpuSnapshot) compare(t *testing.T, b cpuSnapshot, label string) {
    t.Helper()
    for i := 0; i < 32; i++ {
        if a.x[i] != b.x[i] {
            t.Errorf("%s: x[%d] mismatch: 0x%x vs 0x%x", label, i, a.x[i], b.x[i])
        }
    }
    for i := 0; i < 32; i++ {
        if a.f[i] != b.f[i] {
            t.Errorf("%s: f[%d] mismatch: 0x%x vs 0x%x", label, i, a.f[i], b.f[i])
        }
    }
    if a.pc != b.pc {
        t.Errorf("%s: PC mismatch: 0x%x vs 0x%x", label, a.pc, b.pc)
    }
    if a.fcsr != b.fcsr {
        t.Errorf("%s: FCSR mismatch: 0x%x vs 0x%x", label, a.fcsr, b.fcsr)
    }
}
```

### Instruction encoding helpers

Reuse existing `renc` and `benc` from jit_test.go. Add:

```go
// ienc encodes an I-type instruction (ADDI, loads, JALR, etc.)
func ienc(opcode, funct3, rd, rs1 uint32, imm int32) uint32 {
    return uint32(imm)<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

// senc encodes an S-type instruction (stores)
func senc(opcode, funct3, rs1, rs2 uint32, imm int32) uint32 {
    u := uint32(imm)
    return (u>>5)<<25 | rs2<<20 | rs1<<15 | funct3<<12 | (u&0x1F)<<7 | opcode
}

// uenc encodes a U-type instruction (LUI, AUIPC)
func uenc(opcode, rd uint32, imm int32) uint32 {
    return uint32(imm)&0xFFFFF000 | rd<<7 | opcode
}

// jenc encodes a J-type instruction (JAL)
func jenc(opcode, rd uint32, offset int32) uint32 {
    u := uint32(offset)
    return ((u>>20)&1)<<31 | ((u>>1)&0x3FF)<<21 | ((u>>11)&1)<<20 | ((u>>12)&0xFF)<<12 | rd<<7 | opcode
}
```

Common instruction constants:
```go
const (
    opLOAD   = 0x03
    opSTORE  = 0x23
    opOPIMM  = 0x13
    opOP     = 0x33
    opBRANCH = 0x63
    opJAL    = 0x6F
    opJALR   = 0x67
    opLUI    = 0x37
    opSYSTEM = 0x73
    instrECALL  = uint32(0x00000073)
    instrEBREAK = uint32(0x00100073)
)
```

### `storeProgram` — write instruction sequence to memory

Already exists in cpu_branch_load_test.go; reuse pattern:
```go
func storeInsns(mem *GuestMemory, addr uint64, insns []uint32) {
    for i, insn := range insns {
        mem.Store32(addr+uint64(i*4), insn)
    }
}
```

---

## Test 1: JIT vs Interpreter — riscv-tests Compliance Suite

**File: `riscv_test.go`**

Add `runRISCVTestJIT` (mirrors `runRISCVTest` but uses JIT) and parallel test functions.

```go
func runRISCVTestJIT(t *testing.T, elfPath string) {
    t.Helper()
    data, err := os.ReadFile(elfPath)
    if err != nil { t.Skipf(...); return }

    mem, _ := NewGuestMemory(Size4GB)
    defer mem.Free()
    entry, _ := LoadELFBytes(mem, data)
    cpu := NewCPU(*mem)
    cpu.SetPC(entry)

    exitCode, err := runJITWithOS(cpu)
    if err != nil { t.Fatalf("JIT run error: %v", err) }
    if exitCode != 0 {
        testNum := exitCode >> 1
        t.Errorf("JIT FAILED: test %d (exit code %d)", testNum, exitCode)
    }
}
```

Note: `runJITWithOS` is defined in jit_test.go (same package).

Add test functions for each ISA extension:
- `TestRISCVTests_UI_JIT` — 48 integer tests (ADD, branches, loads, stores, shifts, etc.)
- `TestRISCVTests_UM_JIT` — 13 multiply/divide tests
- `TestRISCVTests_UA_JIT` — 19 atomic tests (will fall back to interpreter, but tests mixed path)
- `TestRISCVTests_UF_JIT` — 11 float-single tests
- `TestRISCVTests_UD_JIT` — 12 float-double tests
- `TestRISCVTests_UC_JIT` — 1 compressed instruction test

Each follows the same pattern as existing TestRISCVTests_UI etc., but calls `runRISCVTestJIT`.

**Why this matters:** 123 compliance tests running through JIT. Every JIT-translatable instruction gets tested against the official spec. Untranslatable instructions (AMO, FCLASS, CSR) exercise the mixed JIT/interpreter fallback path automatically.

---

## Test 2: Register State Comparison — JIT vs Interpreter Identical

**File: `jit_test.go`**

Run the same instruction sequence through both interpreter and JIT, snapshot all CPU state, compare.

```go
func TestJIT_vs_Interp_Registers(t *testing.T) {
    tests := []struct {
        name  string
        insns []uint32
        init  func(cpu *CPU) // register setup
    }{
        {
            name: "ALU_mix",
            insns: []uint32{
                // ADDI x1, x0, 100
                // ADDI x2, x0, 42
                // ADD  x3, x1, x2
                // SUB  x4, x1, x2
                // SLL  x5, x1, x2  (only low 6 bits of x2 used)
                // XOR  x6, x3, x4
                // ECALL
            },
        },
        {
            name: "load_store",
            insns: []uint32{
                // LUI x10, 0x2     (x10 = 0x2000, data area)
                // ADDI x11, x0, 0x55
                // SW x11, 0(x10)
                // LW x12, 0(x10)
                // SB x11, 8(x10)
                // LB x13, 8(x10)
                // LBU x14, 8(x10)
                // SD x11, 16(x10)
                // LD x15, 16(x10)
                // ECALL
            },
        },
        {
            name: "branch_skip",
            insns: []uint32{
                // ADDI x1, x0, 5
                // ADDI x2, x0, 5
                // BEQ x1, x2, +8     (skip next insn)
                // ADDI x3, x0, 999   (should be skipped)
                // ADDI x3, x0, 42    (branch target: x3 = 42)
                // ECALL
            },
        },
        {
            name: "lui_auipc",
            insns: []uint32{
                // LUI x1, 0xDEADB
                // ADDI x1, x1, 0xEEF (x1 = 0xDEADBEEF with sign extension)
                // AUIPC x2, 0
                // ECALL
            },
        },
    }

    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            // --- Interpreter ---
            mem1, _ := NewGuestMemory(Size64MB)
            defer mem1.Free()
            storeInsns(mem1, 0x1000, tc.insns)
            cpu1 := NewCPU(*mem1)
            cpu1.SetPC(0x1000)
            if tc.init != nil { tc.init(cpu1) }
            // install ECALL → NoteFatal handler
            cpu1.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
                if IsEcall(n) { return NoteFatal }
                return NoteForward
            })
            RunWithChain(cpu1, &cpu1.Notes)
            snap1 := takeCPUSnapshot(cpu1)

            // --- JIT ---
            mem2, _ := NewGuestMemory(Size64MB)
            defer mem2.Free()
            storeInsns(mem2, 0x1000, tc.insns)
            cpu2 := NewCPU(*mem2)
            cpu2.SetPC(0x1000)
            if tc.init != nil { tc.init(cpu2) }
            cpu2.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
                if IsEcall(n) { return NoteFatal }
                return NoteForward
            })
            jit := NewJIT()
            jit.RunJIT(cpu2)
            snap2 := takeCPUSnapshot(cpu2)

            // --- Compare ---
            snap1.compare(t, snap2, tc.name)
        })
    }
}
```

**Sub-tests (instruction mixes):**
1. `ALU_mix` — ADD, SUB, SLL, XOR, ADDI with various register combos
2. `load_store` — LUI + SW/LW/SB/LB/LBU/SD/LD verifying memory roundtrip
3. `branch_skip` — BEQ taken (forward), verifying skipped instruction doesn't execute
4. `lui_auipc` — upper-immediate instructions
5. `shifts` — SLL, SRL, SRA, SLLI, SRLI, SRAI with edge values (shift by 0, 63)
6. `mul_div` — MUL, DIV, REM (if JIT-translatable), verify against interpreter
7. `bitmanip` — Zbb instructions: CLZ, CTZ, CPOP, REV8, ORC.B, RORI

---

## Test 3: Load/Store Through JIT

**File: `jit_test.go`**

### 3a: Basic load/store roundtrip
```go
func TestJIT_LoadStore(t *testing.T) {
    // Program: store 0xDEADBEEF to addr 0x2000, load it back, ECALL
    // LUI x10, 0x2          (x10 = 0x2000)
    // LUI x11, 0xDEADB      (x11 = 0xDEADB000)
    // ADDI x11, x11, 0xEEF  (sign-extends! need to handle this carefully)
    // SW x11, 0(x10)
    // LW x12, 0(x10)
    // ECALL
    // Verify: x12 == x11 (store then load same address)
    // Also verify: mem.Load32(0x2000) == value (memory actually written)
}
```

### 3b: All widths
```go
func TestJIT_LoadStore_AllWidths(t *testing.T) {
    // Sub-tests for each width: byte, halfword, word, doubleword
    // Each: store known value, load it back, verify
    // Also test sign extension: LB of 0xFF → -1, LBU of 0xFF → 255
}
```

### 3c: Load fault
```go
func TestJIT_LoadFault(t *testing.T) {
    mem, _ := NewGuestMemory(Size64MB) // 64MB = mask 0x03FFFFFF
    // Set x10 = 0x04000000 (just past end of 64MB)
    // LW x11, 0(x10) → should fault
    // Verify: NoteChain receives load fault with correct address
    // Verify: cpu.pc is set to the faulting instruction's PC
    var gotFault *MemFault
    cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
        if n.Cause == CauseLoadFault {
            gotFault = &MemFault{...}
            return NoteFatal
        }
        return NoteForward
    })
    jit := NewJIT()
    jit.RunJIT(cpu)
    // Assert gotFault != nil, gotFault.Addr == 0x04000000
}
```

### 3d: Store fault
```go
func TestJIT_StoreFault(t *testing.T) {
    // Same as load fault but with SW to out-of-bounds address
    // Verify jitStoreFault status delivered through NoteChain
}
```

---

## Test 4: Cycle Counter Accuracy

**File: `jit_test.go`**

```go
func TestJIT_CycleCount(t *testing.T) {
    // Straight-line program: 5 ADDIs followed by ECALL
    // Expected: cpu.Cycle() == 6 (5 ADDIs + 1 ECALL)
    insns := []uint32{
        ienc(opOPIMM, 0, 1, 0, 1),  // ADDI x1, x0, 1
        ienc(opOPIMM, 0, 2, 0, 2),  // ADDI x2, x0, 2
        ienc(opOPIMM, 0, 3, 0, 3),  // ADDI x3, x0, 3
        ienc(opOPIMM, 0, 4, 0, 4),  // ADDI x4, x0, 4
        ienc(opOPIMM, 0, 5, 0, 5),  // ADDI x5, x0, 5
        instrECALL,
    }
    // Run through JIT
    // Assert cpu.Cycle() == 6

    // Also test with a loop:
    // Fib loop with n=5: 5 insns per iteration × 5 iterations + setup + ECALL
    // Compare JIT cycle count vs interpreter cycle count
}
```

### Cycle count with loop
```go
func TestJIT_CycleCount_Loop(t *testing.T) {
    // Same fib program as TestJIT_Fib but verify exact cycle count
    // Run through interpreter: get cycle count
    // Run through JIT: get cycle count
    // Assert they match exactly
}
```

---

## Test 5: Block Exit and Re-entry (Dispatch Transition)

**File: `jit_test.go`**

### 5a: Two-block dispatch
```go
func TestJIT_TwoBlockDispatch(t *testing.T) {
    // Block A at 0x1000: ADDI x1, x0, 10 → JAL x1(ra), +16 (call block B)
    //   JAL rd=1 terminates block A, returns target=0x1010
    // Block B at 0x1010: ADDI x2, x0, 20 → ECALL
    //   Separate block, compiled on demand
    // Verify: x1 = 0x1008 (return address), x2 = 20
    // Verify: both blocks in jit.blocks cache
}
```

### 5b: JALR indirect exit and re-entry
```go
func TestJIT_JALR_IndirectJump(t *testing.T) {
    // Block A at 0x1000:
    //   ADDI x1, x0, 10
    //   LUI  x5, 0x1       (x5 = 0x1000, but we want 0x1010)
    //   ADDI x5, x5, 0x10  (x5 = 0x1010)
    //   JALR x0, x5, 0     (jump to x5, rd=0 means no link)
    // Block B at 0x1010:
    //   ADDI x2, x0, 20
    //   ECALL
    // Verify: x1 = 10, x2 = 20
    // Key: JALR exits block A, dispatch compiles block B at runtime target
}
```

### 5c: Block re-entry with modified state
```go
func TestJIT_BlockReentry(t *testing.T) {
    // This simulates what the instruction budget will do:
    // Block at 0x1000: ADDI x1, x1, 1 → ECALL
    //
    // First call: x1 starts at 0 → block runs → x1=1, writeback, ECALL stops
    // Modify x1 via cpu.SetReg → restart at same PC
    // Second call: x1 starts at 100 → block runs → x1=101
    // Verify: x1 == 101 (block re-read from x[] on entry, not stale cached value)

    mem, _ := NewGuestMemory(Size64MB)
    defer mem.Free()
    storeInsns(mem, 0x1000, []uint32{
        ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1
        instrECALL,
    })

    cpu := NewCPU(*mem)
    cpu.SetPC(0x1000)
    cpu.SetReg(1, 0)
    // ECALL handler: stop, record state
    stopped := false
    cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition {
        if IsEcall(n) { stopped = true; return NoteFatal }
        return NoteForward
    })

    jit := NewJIT()
    jit.RunJIT(cpu)
    assert(cpu.Reg(1) == 1) // first run: 0+1=1

    // Re-enter with modified state
    cpu.SetPC(0x1000)
    cpu.SetReg(1, 100)
    stopped = false
    jit.RunJIT(cpu)
    assert(cpu.Reg(1) == 101) // second run: 100+1=101, proves re-read from x[]
}
```

---

## Test 6: Memory Consistency Across JIT/Interpreter Boundary

**File: `jit_test.go`**

### 6a: JIT store → interpreter load
```go
func TestJIT_MemConsistency_JIT_to_Interp(t *testing.T) {
    // Program:
    //   0x1000: LUI  x10, 0x2          (x10 = 0x2000)
    //   0x1004: ADDI x11, x0, 42
    //   0x1008: SW   x11, 0(x10)        (store 42 to 0x2000)
    //   0x100C: CSRRS x0, cycle, x0     (untranslatable! falls to interpreter)
    //   0x1010: LW   x12, 0(x10)        (interpreter loads from 0x2000)
    //   0x1014: ECALL
    //
    // The CSR instruction forces a JIT→interpreter transition.
    // x12 must equal 42 — the JIT store must be visible to the interpreter load.
    // This works because JIT stores go directly to guest memory (no write buffer).
}
```

### 6b: Interpreter store → JIT load
```go
func TestJIT_MemConsistency_Interp_to_JIT(t *testing.T) {
    // Pre-store a value via interpreter (cpu.step or mem.Store32)
    // Then run JIT block that loads it
    // Verify: JIT load sees the interpreter's store
    mem.Store32(0x2000, 0xCAFEBABE)
    // JIT block: LUI x10, 0x2; LW x11, 0(x10); ECALL
    // Verify x11 == 0xCAFEBABE (sign-extended to 64 bits: 0xFFFFFFFFCAFEBABE)
}
```

---

## Test 7: EBREAK Through JIT

**File: `jit_test.go`**

```go
func TestJIT_EBREAK(t *testing.T) {
    // Program: ADDI x1, x0, 42 → EBREAK
    insns := []uint32{
        ienc(opOPIMM, 0, 1, 0, 42), // ADDI x1, x0, 42
        instrEBREAK,
    }
    // Install handler that catches EBREAK
    var gotBreak bool
    cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
        if n.Cause == CauseBreakpoint {
            gotBreak = true
            return NoteFatal
        }
        return NoteForward
    })
    jit := NewJIT()
    jit.RunJIT(cpu)
    assert(gotBreak == true)
    assert(cpu.Reg(1) == 42) // instruction before EBREAK executed
}
```

---

## Test 8: Mixed JIT/Interpreter Execution

**File: `jit_test.go`**

```go
func TestJIT_MixedExecution(t *testing.T) {
    // Program with untranslatable instruction in the middle:
    //   0x1000: ADDI x1, x0, 10       (JIT'd)
    //   0x1004: ADDI x2, x0, 20       (JIT'd)
    //   0x1008: CSRRS x3, cycle, x0   (NOT JIT'd — terminates block)
    //   0x100C: ADDI x4, x0, 30       (new JIT block starts here)
    //   0x1010: ADD  x5, x1, x2       (JIT'd)
    //   0x1014: ECALL
    //
    // Verify: x1=10, x2=20, x3=<cycle value>, x4=30, x5=30
    // Key: registers from first JIT block (x1, x2) survive through
    //       interpreter step and are visible to second JIT block (x5=x1+x2)
}
```

---

## Test 9: Forward Branch Within Region

**File: `jit_test.go`**

```go
func TestJIT_ForwardBranch(t *testing.T) {
    // Tests region-aware forward goto (the Phase 2 feature):
    //   0x1000: ADDI x1, x0, 1
    //   0x1004: BEQ  x0, x0, +12      (always taken, jump to 0x1010)
    //   0x1008: ADDI x2, x0, 999      (should be SKIPPED)
    //   0x100C: ADDI x3, x0, 999      (should be SKIPPED)
    //   0x1010: ADDI x2, x0, 42       (branch target)
    //   0x1014: ECALL
    //
    // Region scan finds 0x1000-0x1014. Forward BEQ to 0x1010 uses goto.
    // Verify: x1=1, x2=42, x3=0 (never written)
}
```

### Forward branch not-taken path
```go
func TestJIT_ForwardBranch_NotTaken(t *testing.T) {
    // Same layout but branch condition is false:
    //   0x1000: ADDI x1, x0, 1
    //   0x1004: ADDI x2, x0, 2
    //   0x1008: BNE  x1, x1, +8       (never taken, x1==x1)
    //   0x100C: ADDI x3, x0, 42       (falls through here)
    //   0x1010: ECALL
    // Verify: x3 == 42 (fall-through executed)
}
```

---

## Test 10: Unconditional Jump (J / JAL rd=0)

**File: `jit_test.go`**

```go
func TestJIT_J_Forward(t *testing.T) {
    // JAL x0, +12 (J forward, skip 2 instructions)
    //   0x1000: ADDI x1, x0, 1
    //   0x1004: JAL  x0, +12           (jump to 0x1010)
    //   0x1008: ADDI x2, x0, 999       (SKIPPED)
    //   0x100C: ADDI x3, x0, 999       (SKIPPED)
    //   0x1010: ADDI x2, x0, 42
    //   0x1014: ECALL
    //
    // Phase 1 chaining: emitter follows J without exiting block.
    // Entire sequence is one compiled block.
    // Verify: x1=1, x2=42, x3=0
}
```

---

## Test 11: Translation Failure and noJIT Fallback

**File: `jit_test.go`**

```go
func TestJIT_TranslationFailure(t *testing.T) {
    // Start with untranslatable instruction:
    //   0x1000: CSRRS x1, cycle, x0   (CSR — emitBlock returns nil or 0 insns)
    //   0x1004: ECALL
    //
    // Verify: jit.noJIT[0x1000] == true after first attempt
    // Verify: interpreter handles it correctly (x1 gets cycle value)
    // Verify: execution completes (doesn't hang)

    jit := NewJIT()
    jit.RunJIT(cpu)
    // Check noJIT map
    if !jit.noJIT[0x1000] {
        t.Error("expected noJIT[0x1000] to be set")
    }
}
```

---

## Test 12: Bail Label Safety Net

**File: `jit_test.go`**

```go
func TestJIT_BailLabel(t *testing.T) {
    // Construct a region where a forward branch targets a PC that
    // has an untranslatable instruction. The region scan includes
    // it, but emission terminates early. The bail label catches the goto.
    //
    //   0x1000: ADDI x1, x0, 1
    //   0x1004: BEQ  x0, x0, +8        (always taken → 0x100C)
    //   0x1008: ADDI x2, x0, 999       (skipped by branch)
    //   0x100C: CSRRS x3, cycle, x0    (untranslatable! block terminates here)
    //   0x1010: ECALL
    //
    // Region scan finds 0x1000-0x1010. emitBranch sees target 0x100C < regionEnd → goto.
    // But when emission reaches 0x100C, CSR terminates the block.
    // Bail label at b_100C catches the goto, writes back, returns pc=0x100C.
    // Dispatch falls to interpreter for the CSR, then JIT resumes at 0x1010 (or interp ECALL).
    //
    // Verify: x1=1, x2=0, x3=<cycle value>, execution completes
}
```

---

## Test 13: Last-Block Cache Correctness

**File: `jit_test.go`**

```go
func TestJIT_LastBlockCache(t *testing.T) {
    // Run same block 3 times, verify last-block cache is used:
    // Block at 0x1000: ADDI x1, x1, 1 → ECALL
    // Run 1: x1 = 0 → 1
    // Run 2: x1 = 1 → 2 (should use lastBlk cache, no map lookup)
    // Run 3: x1 = 2 → 3

    // Verify: jit.lastPC == 0x1000, jit.lastBlk != nil
    // Verify: len(jit.blocks) == 1 (only one block compiled)
    // Verify: x1 == 3 after all runs
}
```

---

## Test 14: Fault Address Correctness

**File: `jit_test.go`**

```go
func TestJIT_FaultAddress(t *testing.T) {
    // Verify that the fault address in the NoteChain note matches
    // the actual address that faulted.
    //
    // Load fault: x10 = 0x7FFFFFF0, LW x11, 8(x10) → addr 0x7FFFFFF8
    // (past 64MB mask = 0x03FFFFFF)
    // Verify note.Tval or MemFault.Addr == 0x7FFFFFF8 (the computed address)

    // Store fault: same pattern with SW
}
```

---

## Implementation Order

1. Add encoding helpers (`ienc`, `senc`, `uenc`, `jenc`, `storeInsns`) and `cpuSnapshot` to jit_test.go
2. Add `runJITWithOS` helper to jit_test.go
3. **Test 5c** (BlockReentry) — most critical for budget, simplest to implement
4. **Test 4** (CycleCount) — budget depends on ic accuracy
5. **Test 3a/3b** (LoadStore) — proves memory operations work
6. **Test 2** (JIT vs Interp Registers) — comprehensive state comparison
7. **Test 7** (EBREAK) — tests fault status path
8. **Test 3c/3d** (Load/Store Faults) — tests fault delivery
9. **Test 6** (Memory Consistency) — cross-boundary store/load
10. **Test 8** (Mixed Execution) — JIT/interpreter interleaving
11. **Test 9** (Forward Branch) — region scanning correctness
12. **Test 10** (J Forward) — Phase 1 chaining
13. **Test 11** (Translation Failure) — noJIT path
14. **Test 12** (Bail Label) — safety net
15. **Test 13** (Last-Block Cache) — cache correctness
16. **Test 14** (Fault Address) — fault metadata
17. **Test 1** (riscv-tests JIT) — the big one, in riscv_test.go

## Verification

```bash
# Run all JIT tests
go test -v -run 'TestJIT' -timeout 120s .

# Run riscv-tests through JIT (requires ELF binaries)
go test -v -run 'TestRISCVTests_.*_JIT' -timeout 120s .

# Run everything
go test -v -timeout 120s .

# Verify bench smoke still passes
go test -v -run TestJIT_BenchGuest_Smoke ./bench/
```

Expected: all tests pass. Any failure indicates a JIT correctness bug that must be fixed BEFORE adding the instruction budget.

---

## TEST 15: LOCKSTEP — Per-Block JIT vs Interpreter with Full State Comparison

**The crown jewel.** Runs every ELF from the `elfs` slice through a lockstep dispatch loop that compares ALL registers and guest memory after every single block boundary.

### `StepBlock` — single-dispatch-cycle method

**File: `jit.go`** — new public method on JIT, used by the lockstep test harness.

Executes exactly one dispatch cycle: either a compiled JIT block or one interpreter instruction. Returns the number of instructions retired and any error (ErrEcall, ErrEbreak, MemFault).

```go
// StepBlock executes one dispatch cycle and returns.
// If a compiled block exists for cpu.pc, it runs the full block.
// Otherwise it attempts compilation; if that fails, interprets one instruction.
// Returns (instructionsRetired, error). Error is nil for jitOK.
func (j *JIT) StepBlock(cpu *CPU) (ic uint64, err error) {
    pc := cpu.pc

    // Check cache (last-block + map)
    var blk *compiledBlock
    if pc == j.lastPC && j.lastBlk != nil {
        blk = j.lastBlk
    } else if b, ok := j.blocks[pc]; ok {
        blk = b
        j.lastPC = pc
        j.lastBlk = blk
    }

    if blk != nil {
        res := jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
            cpu.mem.Base(), cpu.mem.Mask())
        cpu.pc = res.PC
        cpu.cycle += res.IC

        switch int(res.Status) {
        case jitOK:
            return res.IC, nil
        case jitEcall:
            return res.IC, ErrEcall
        case jitEbreak:
            return res.IC, ErrEbreak
        case jitLoadFault:
            return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
        case jitStoreFault:
            return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
        default:
            // Unknown status — interpret one instruction
            err = cpu.step()
            cpu.cycle++
            return res.IC + 1, err
        }
    }

    // Try to translate
    if !j.InterpOnly && !j.noJIT[pc] {
        res := emitBlock(&cpu.mem, pc)
        if res != nil && res.numInsns > 0 {
            compiled, cerr := tccCompile(res.csrc)
            if cerr == nil {
                j.blocks[pc] = compiled
                j.lastPC = pc
                j.lastBlk = compiled
                return j.StepBlock(cpu) // retry with compiled block
            }
        }
        j.noJIT[pc] = true
    }

    // Interpreter fallback
    err = cpu.step()
    cpu.cycle++
    return 1, err
}
```

### Lockstep runner

**File: `jit_test.go`** (or `riscv_test.go` since it needs `elfs` slice and `rvTestsDir`)

Actually place in `riscv_test.go` since it references `elfs` and `rvTestsDir`.
The helpers (`cpuSnapshot`, `runJITWithOS`, etc.) are in `jit_test.go` (same package).

```go
// runLockstep loads the same ELF into two separate CPU+Memory pairs,
// then runs them in lockstep: JIT executes one block, interpreter executes
// the same number of instructions, then all state is compared.
func runLockstep(t *testing.T, elfPath string) {
    t.Helper()
    data, err := os.ReadFile(elfPath)
    if err != nil {
        t.Skipf("ELF not found: %s", elfPath)
        return
    }

    // --- JIT side ---
    jitMem, err := NewGuestMemory(Size4GB)
    if err != nil { t.Fatal(err) }
    defer jitMem.Free()
    jitEntry, err := LoadELFBytes(jitMem, data)
    if err != nil { t.Fatal(err) }
    jitCPU := NewCPU(*jitMem)
    jitCPU.SetPC(jitEntry)

    // --- Interpreter side ---
    interpMem, err := NewGuestMemory(Size4GB)
    if err != nil { t.Fatal(err) }
    defer interpMem.Free()
    interpEntry, err := LoadELFBytes(interpMem, data)
    if err != nil { t.Fatal(err) }
    interpCPU := NewCPU(*interpMem)
    interpCPU.SetPC(interpEntry)

    jit := NewJIT()
    maxCycles := uint64(10_000_000) // safety limit
    blockNum := 0

    for jitCPU.Cycle() < maxCycles {
        // INVARIANT: PCs must match at start of each dispatch cycle
        if jitCPU.pc != interpCPU.pc {
            t.Fatalf("block %d: PC desync BEFORE dispatch: jit=0x%x interp=0x%x",
                blockNum, jitCPU.pc, interpCPU.pc)
        }

        // JIT side: one dispatch cycle
        jitIC, jitErr := jit.StepBlock(jitCPU)

        // Interpreter side: run same number of instructions
        var interpErr error
        for i := uint64(0); i < jitIC; i++ {
            interpErr = interpCPU.step()
            interpCPU.cycle++
            if interpErr != nil {
                // Exception on last instruction — should match JIT
                break
            }
        }

        // Handle ECALL exit on both sides
        jitExit := isExitEcall(jitCPU, jitErr)
        interpExit := isExitEcall(interpCPU, interpErr)
        if jitExit || interpExit {
            if jitExit != interpExit {
                t.Errorf("block %d: exit mismatch: jit=%v interp=%v",
                    blockNum, jitExit, interpExit)
            }
            break // program complete
        }

        // Handle non-exit ECALL/EBREAK: advance PC past the instruction
        // (mirrors what NoteChain + OS handler does)
        if jitErr != nil {
            advancePastException(jitCPU, jitErr)
        }
        if interpErr != nil {
            advancePastException(interpCPU, interpErr)
        }

        // COMPARE ALL REGISTERS
        for r := 0; r < 32; r++ {
            if jitCPU.x[r] != interpCPU.x[r] {
                t.Errorf("block %d (startPC=0x%x): x[%d] mismatch: jit=0x%x interp=0x%x",
                    blockNum, jitCPU.pc, r, jitCPU.x[r], interpCPU.x[r])
            }
        }
        for r := 0; r < 32; r++ {
            if jitCPU.f[r] != interpCPU.f[r] {
                t.Errorf("block %d: f[%d] mismatch: jit=0x%x interp=0x%x",
                    blockNum, r, jitCPU.f[r], interpCPU.f[r])
            }
        }
        if jitCPU.pc != interpCPU.pc {
            t.Fatalf("block %d: PC mismatch AFTER dispatch: jit=0x%x interp=0x%x",
                blockNum, jitCPU.pc, interpCPU.pc)
        }
        if jitCPU.fcsr != interpCPU.fcsr {
            t.Errorf("block %d: FCSR mismatch: jit=0x%x interp=0x%x",
                blockNum, jitCPU.fcsr, interpCPU.fcsr)
        }

        blockNum++
    }

    // FINAL: Compare guest memory (ELF segment regions)
    compareGuestMemory(t, jitMem, interpMem, data)
}
```

### Memory comparison helper

```go
// compareGuestMemory compares the ELF's PT_LOAD segment regions in both memories.
// Reads ELF headers to find segment addresses, then compares byte-for-byte.
// This catches JIT store bugs that register comparison might miss.
func compareGuestMemory(t *testing.T, a, b *GuestMemory, elfData []byte) {
    t.Helper()
    // Parse ELF headers to find PT_LOAD segments
    segments := elfPTLoadSegments(elfData) // returns []struct{VAddr, MemSz}

    for _, seg := range segments {
        // Compare segment region + 4KB padding (catches stack/BSS writes near segment)
        size := seg.MemSz
        if size > 1<<20 { size = 1<<20 } // cap at 1MB per segment

        bufA := make([]byte, size)
        bufB := make([]byte, size)
        a.ReadBytes(seg.VAddr, bufA)
        b.ReadBytes(seg.VAddr, bufB)

        for i := range bufA {
            if bufA[i] != bufB[i] {
                addr := seg.VAddr + uint64(i)
                t.Errorf("memory mismatch at 0x%x: jit=0x%02x interp=0x%02x",
                    addr, bufA[i], bufB[i])
                // Report first few differences only
                break
            }
        }
    }
}

// elfPTLoadSegments parses ELF headers and returns PT_LOAD segment info.
func elfPTLoadSegments(data []byte) []struct{ VAddr, MemSz uint64 } {
    // Parse elf64Header, iterate PhNum program headers, collect PT_LOAD entries
    // Return slice of {VAddr, MemSz} for each
}
```

### Exception handling helpers

```go
// isExitEcall checks if the error is an ECALL and the CPU is requesting exit.
func isExitEcall(cpu *CPU, err error) bool {
    if err != ErrEcall { return false }
    return cpu.x[17] == 93 || cpu.x[17] == 94 // a7 = exit or exit_group syscall
}

// advancePastException handles non-exit exceptions by advancing PC.
// For riscv-tests, the only mid-program exception is the riscv-tests
// ECALL convention where a7!=93 means "unknown syscall" → advance PC by 4.
func advancePastException(cpu *CPU, err error) {
    if err == ErrEcall {
        cpu.pc += 4 // skip the ECALL instruction
    }
    // EBREAK, faults: for riscv-tests these shouldn't occur mid-program
}
```

### Test functions using lockstep

**File: `riscv_test.go`**

```go
func TestRISCVTests_Lockstep_UI(t *testing.T) {
    entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
    if err != nil || len(entries) == 0 {
        t.Skip("rv64ui ELFs not found")
    }
    for _, path := range entries {
        name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
        t.Run(name, func(t *testing.T) {
            runLockstep(t, path)
        })
    }
}

func TestRISCVTests_Lockstep_UM(t *testing.T) { ... } // same pattern
func TestRISCVTests_Lockstep_UA(t *testing.T) { ... }
func TestRISCVTests_Lockstep_UF(t *testing.T) { ... }
func TestRISCVTests_Lockstep_UD(t *testing.T) { ... }
func TestRISCVTests_Lockstep_UC(t *testing.T) { ... }
```

This runs ALL 123 ELFs in lockstep mode, comparing every register and memory after every block.

### What lockstep catches that exit-code-only doesn't

| Bug type | Exit-code test | Lockstep test |
|----------|---------------|---------------|
| Wrong register value (used later) | Maybe (if it affects exit code) | **Always** (immediate detection) |
| Wrong register value (dead) | Never | **Always** |
| Wrong store data | Maybe (if loaded later) | **Always** (memory comparison) |
| Store to wrong address | Maybe | **Always** |
| ic count off by 1 | Never | **Detectable** (cycle count diverges) |
| JIT block exits at wrong PC | Eventually (program goes off rails) | **Immediately** (PC mismatch at start of next block) |
| FCSR flags wrong | Never (tests don't check) | **Always** |

---

## Implementation Order (updated)

1. Add `StepBlock` method to `jit.go`
2. Add encoding helpers + `cpuSnapshot` + `storeInsns` to `jit_test.go`
3. Add `runJITWithOS` helper to `jit_test.go`
4. **Test 5c** (BlockReentry) — most critical for budget
5. **Test 4** (CycleCount) — budget depends on ic accuracy
6. **Test 3a/3b** (LoadStore) — memory operations
7. **Test 2** (JIT vs Interp Registers) — comprehensive state comparison
8. **Test 7** (EBREAK) — fault status path
9. **Test 3c/3d** (Load/Store Faults) — fault delivery
10. **Test 6** (Memory Consistency) — cross-boundary store/load
11. **Test 8** (Mixed Execution) — JIT/interpreter interleaving
12. **Test 9** (Forward Branch) — region scanning
13. **Test 10** (J Forward) — Phase 1 chaining
14. **Test 11** (Translation Failure) — noJIT path
15. **Test 12** (Bail Label) — safety net
16. **Test 13** (Last-Block Cache) — cache correctness
17. **Test 14** (Fault Address) — fault metadata
18. Add `elfPTLoadSegments` + `compareGuestMemory` + `runLockstep` to `riscv_test.go`
19. Add `TestRISCVTests_*_JIT` (exit-code-only, fast) to `riscv_test.go`
20. **Test 15** `TestRISCVTests_Lockstep_*` — all 123 ELFs in lockstep mode

## Verification

```bash
# Run all JIT unit tests
go test -v -run 'TestJIT' -timeout 120s .

# Run riscv-tests through JIT (exit-code only, fast)
go test -v -run 'TestRISCVTests_.*_JIT$' -timeout 120s .

# Run lockstep comparison on all ELFs (the thorough one)
go test -v -run 'TestRISCVTests_Lockstep' -timeout 300s .

# Run everything
go test -v -timeout 300s .

# Verify bench smoke still passes
go test -v -run TestJIT_BenchGuest_Smoke ./bench/
```

Expected: all tests pass. Any failure indicates a JIT correctness bug that must be fixed BEFORE adding the instruction budget.
