# Plan: Replace IC with InfiniteLoopStopperPage guard page

## Context

IC (instruction counter) causes 5-6x performance loss: occupies a register allocation slot, emits `ic++` per instruction, emits a budget check at every backward branch. Replace with a guard page: at backward branches, emit a load from a stopper page. Normally readable (L1 hit, ~free). To preempt, `mprotect(PROT_NONE)` — the load faults, and the existing `defer/recover` in `RunJIT()` (jit.go:655) catches the panic. No custom signal handler needed.

## Part 1: Allocate InfiniteLoopStopperPage

### 1a. New file: `jit_stopper.go`

Standalone C mmap, independent of GuestMemory (works for both Rv8 and Abjit).

C helpers (inline in cgo preamble):
- `stopper_alloc()` → `mmap(NULL, 4096, PROT_READ|PROT_WRITE, MAP_PRIVATE|MAP_ANON, -1, 0)`
- `stopper_free(p)` → `munmap(p, 4096)`
- `stopper_arm(p)` → `mprotect(p, 4096, PROT_NONE)`
- `stopper_disarm(p)` → `mprotect(p, 4096, PROT_READ|PROT_WRITE)`

Add to JIT struct (`jit.go`):
```go
stopperPage uintptr
```

Go methods on JIT:
- `initStopperPage()` — allocate via `stopper_alloc()`, called during JIT init
- `freeStopperPage()` — called during JIT cleanup
- `RequestPreemption()` — calls `stopper_arm()` → PROT_NONE
- `ClearPreemption()` — calls `stopper_disarm()` → PROT_READ|PROT_WRITE

**Recovery path**: When the JIT loads from the armed page, SIGSEGV occurs. Go's runtime converts this to a panic. The existing `defer/recover` at `jit.go:655` catches it. `RunJIT` returns an error (e.g. `ErrPreempted`). The caller calls `ClearPreemption()` to disarm the page before re-entering JIT.

**Files**: `jit_stopper.go` (new), `jit.go` (add field + init/cleanup)

## Part 2: Emit stopper load at backward branches

### 2a. Stopper probe in emitted native code

At each backward branch, instead of BudgetCheck, emit a TEST from the stopper page. This is a single native instruction that reads memory but **does not dirty any GP register** — it only touches EFLAGS (SF, ZF, PF), which are volatile across branches anyway.

On the fast path (page readable), it costs ~1 cycle (L1 hit). When armed (PROT_NONE), the read phase faults and `defer/recover` catches the panic.

In the IR, replace `BudgetCheck` with a new `IRStopperLoad`:
```
IRStopperLoad  Imm=stopperPageAddr
```

Semantics: probe (read) the qword at `Imm`. No GP register modified. If the page is PROT_NONE, execution never reaches the next instruction.

### 2b. Lowering IRStopperLoad

**Rv8 lowerer** (`lower_amd64_ops.go`):
```asm
MOVQ stopperAddr, RAX    ; MOV imm64 → RAX (10 bytes, uses staging reg)
TESTQ RAX, (RAX)         ; read from page, result → EFLAGS only; faults if armed
```

Uses RAX (stgA) only to hold the address. The TESTQ reads `[RAX]` and ANDs it with RAX, writing only EFLAGS. Since this is emitted right before an unconditional JMP to the loop target, EFLAGS are immediately dead.

Note: RAX is the staging register — it's always scratch and never holds a live value across instructions in both lowerers.

**Abjit lowerer** (`lower_amd64_abjit.go`): same pattern using stgA.

**Total native cost per backward branch**: 13 bytes (10-byte MOV imm64 + 3-byte TESTQ). Zero GP register impact.

**ARM64 note** (for when we port): Use `LDR XZR, [Xn]` — loads into the zero register which discards the value. The CPU still performs the full memory read (TLB walk, page table check), so PROT_NONE faults as expected. Zero change to X0-X30. Add this as a comment next to the amd64 TESTQ emission.

### 2c. Replace BudgetCheck calls in jit_emit_ir.go

Add `stopperAddr int64` field to `emitter` struct. Passed from `JIT.stopperPage` at block emission time.

In `emitBranch()` (~line 2624, backward branch path):
```go
// Before:
e.irEm.BudgetCheck(targetLabel, target)

// After:
e.irEm.StopperLoad(e.stopperAddr)
e.irEm.Jump(targetLabel)
```

In `emitJAL()` (~line 2543, backward unconditional jump):
```go
// Before:
e.irEm.BudgetCheck(targetLabel, target)

// After:
e.irEm.StopperLoad(e.stopperAddr)
e.irEm.Jump(targetLabel)
```

**Files**: `ir.go`, `highlevel.go` (or `emit.go`), `lower_amd64_ops.go`, `lower_amd64_abjit.go`, `jit_emit_ir.go`

## Part 3: Remove IC entirely

### 3a. Simplify advancePC (`jit_emit_ir.go`)

```go
func (e *emitter) advancePC(size uint64) {
    e.numInsns++
    e.pc += size
}
```

Delete:
- `emitICplusplus()` function (line 274-276)
- `icEmitted` field from `emitter` struct
- IC logic in `emitBranch()`: remove `e.emitICplusplus()` (line 2607) and `e.icEmitted = true` (line 2608)

### 3b. Remove IC VReg allocation (`emit.go`, `lower_amd64.go`)

**`emit.go`**: Remove `ic` field, `IC()` method, and the `e.Tmp()` call that allocated VRIC (line 46: `e.ic = e.Tmp()`).

Currently NewEmitter allocates 6 parameter VRegs via `e.Tmp()`:
```
t64 = VRXBase
t65 = VRFBase
t66 = VRIC        ← DELETE this one
t67 = VRMemBase
t68 = VRMemMask
t69 = VRRegFile
```

After removal, only 5 calls to `e.Tmp()`:
```
t64 = VRXBase
t65 = VRFBase
t66 = VRMemBase   (was t67)
t67 = VRMemMask   (was t68)
t68 = VRRegFile   (was t69)
```

**`lower_amd64.go`**: Update constants:
```go
const (
    VRXBase   = VReg(VRegTempStart + 0) // t64
    VRFBase   = VReg(VRegTempStart + 1) // t65
    VRMemBase = VReg(VRegTempStart + 2) // t66 (was +3)
    VRMemMask = VReg(VRegTempStart + 3) // t67 (was +4)
)
const VRRegFile = VReg(VRegTempStart + 4) // t68 (was +5)
```

Delete `VRIC` entirely.

### 3c. Return freed register to allocator pools

Currently VRIC competes with guest registers for a host register in the allocator. With VRIC gone, the allocator automatically has one more VReg fewer to assign, meaning more host registers available for guest regs.

**No pool changes needed** — the pools (`RV8Pool` returns 12 int regs, `ABJITPool` returns 11 int regs) define *available host registers*, which haven't changed. What changes is *demand*: one fewer VReg (VRIC) competing for those host registers. The allocator will naturally assign the freed slot to a guest register that was previously spilled.

To verify this is working: after the change, check that blocks which previously spilled a guest register now keep it in a host register. Can be verified by dumping `Allocation.Kind` for a test block before/after.

**Files**: `emit.go`, `lower_amd64.go`

### 3d. Remove IC from Rv8 lowerer (`lower_amd64_rv8.go`)

- Delete `stageICToScratch()` function entirely
- Remove all `icStaged := lc.stageICToScratch()` calls (~5 sites: rv8Ret, rv8RetDyn, rv8ChainExit, rv8Syscall, rv8JalrIC)
- Remove IC load from prologue (lines ~189-205: loading sret.IC into VRIC's host reg)
- Remove IC writes to sret buffer (stores to `sretOffset+8`)
- Note: `sretOffset+8` is also used as scratch for RDX spill during DIV (`lower_amd64_ops.go:767`). That usage is unrelated to IC and stays — just add a comment clarifying it's a scratch slot.

### 3e. Remove IC from Abjit lowerer (`lower_amd64_abjit.go`)

- Delete `stageICToState()` function
- Remove all calls to it (~5 sites: abjitRet, abjitRetDyn, abjitChainExit, abjitSyscall, abjitJalrIC)
- Remove IC load from prologue (lines ~168-180)
- Update hardcoded offset constants after State.IC removal (Part 3h shifts offsets)

### 3f. Delete BudgetCheck + MaxIC (`highlevel.go`)

- Delete `BudgetCheck()` method (lines 162-173)
- Delete `MaxIC` constant (line 20)

### 3g. Remove IC from Result struct + trampolines

**`internal/jitcall/call.go`**: Remove `IC` field. Result becomes 24 bytes:
```go
type Result struct {
    PC        uint64 // offset 0
    Status    uint64 // offset 8  (was 16)
    FaultAddr uint64 // offset 16 (was 24)
}
```

**`internal/jitcall/call_amd64.s`**: In both `Call` and `CallAOT`:
- Copy 3 qwords from sret to Go return area (not 4)
- Adjust return field offsets: `ret_Status` and `ret_FaultAddr` shift by -8
- sret buffer layout: offset 0=PC, 8=Status (was IC), 16=FaultAddr (was Status), slot at 24 becomes unused

**`jit_sandbox_amd64.S`**: Same sret layout adjustment for sandbox trampoline.

**`jit_sandbox.c`**: Update sret layout comments.

**Rv8 lowerer** (`lower_amd64_rv8.go`): All writes of Status/FaultAddr to sret must use new offsets (shift by -8):
- Status: `sretOffset+8` (was `sretOffset+16`)
- FaultAddr: `sretOffset+16` (was `sretOffset+24`)

**Abjit lowerer** (`lower_amd64_abjit.go`): Offset constants for State.Status, State.FaultAddr shift after IC removal.

### 3h. Remove IC from Abjit State (`abjit/abjit.go`)

Remove `IC uint64` field from State struct. This shifts all subsequent field offsets by -8:
```
PC:         536 (unchanged)
Status:     544 (was 552)
FaultAddr:  552 (was 560)
DCBase:     560 (was 568)
DCMask:     568 (was 576)
VAddrBegin: 576 (was 584)
SegSize:    584 (was 592)
```

Update all hardcoded offset constants in `lower_amd64_abjit.go`.

### 3i. Update dispatch loop (`jit.go`, `jit_abjit.go`)

**`jit.go`**:
- Remove all `res.IC` references
- Remove `cpu.cycle += res.IC` (or replace with estimate from `blk.numInsns`)
- Change all `return res.IC, err` → `return 0, err` (or remove IC from return type if desired)
- In `RunJIT` recover block: detect stopper page fault (check error message or add a sentinel) and return `ErrPreempted`

**`jit_abjit.go`**:
- Remove `s.IC = 0` (line 30)
- Remove `IC: s.IC` from Result construction (line 36)

## Execution order

1. Add stopper page allocation + arm/disarm API (Part 1)
2. Add IRStopperLoad IR op + lowering for both Rv8 and Abjit (Part 2a, 2b)
3. Replace BudgetCheck → StopperLoad+Jump at backward branches (Part 2c)
4. Remove emitICplusplus, simplify advancePC (Part 3a)
5. Remove IC VReg allocation, renumber constants (Part 3b)
6. Verify freed register helps allocator (Part 3c)
7. Remove IC from Rv8 lowerer (Part 3d)
8. Remove IC from Abjit lowerer (Part 3e)
9. Delete BudgetCheck + MaxIC (Part 3f)
10. Remove IC from Result struct + trampolines (Part 3g) — most invasive
11. Remove IC from Abjit State + update offsets (Part 3h)
12. Update dispatch loop (Part 3i)

Steps 3-12 are tightly coupled — do as one atomic change.

## Files to modify

| File | Changes |
|------|---------|
| `jit_stopper.go` (new) | C mmap page, arm/disarm API |
| `jit.go` | stopperPage field, init/cleanup, dispatch loop, recover handling |
| `jit_emit_ir.go` | Remove emitICplusplus, simplify advancePC, replace BudgetCheck |
| `ir.go` | Add IRStopperLoad opcode |
| `highlevel.go` | Add StopperLoad emitter, remove BudgetCheck + MaxIC |
| `emit.go` | Remove IC Tmp() alloc, IC() method, ic field |
| `lower_amd64.go` | Delete VRIC, renumber VRMemBase/VRMemMask/VRRegFile |
| `lower_amd64_rv8.go` | Remove stageICToScratch, IC prologue, IC sret writes, update sret offsets |
| `lower_amd64_abjit.go` | Remove stageICToState, IC prologue, update all State offsets |
| `lower_amd64_ops.go` | Add IRStopperLoad lowering |
| `abjit/abjit.go` | Remove IC from State, shifts all subsequent offsets |
| `jit_abjit.go` | Remove IC references |
| `internal/jitcall/call.go` | Remove IC from Result (24 bytes now) |
| `internal/jitcall/call_amd64.s` | Update sret copy (3 qwords), adjust offsets |
| `jit_sandbox_amd64.S` | Update sret layout |
| `jit_sandbox.c` | Update sret comments |

## Verification

```bash
go test -v -run TestJIT .
go test -v ./...
make bench-quick            # expect major speedup
make bench-cpu              # MIPS metric improvement
```

Test preemption: guest infinite loop + goroutine calls `RequestPreemption()` after timeout → `RunJIT` returns via `recover` with preemption error. Caller calls `ClearPreemption()` before re-entering.

Test register pressure improvement: compare `Allocation.Kind` dump for a register-heavy block before/after — one fewer spill expected.
