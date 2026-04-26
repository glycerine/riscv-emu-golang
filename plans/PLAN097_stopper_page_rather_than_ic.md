# Plan: Replace IC with InfiniteLoopStopperPage guard page

## Context

IC (instruction counter) causes 5-6x performance loss: pinned register, `ic++` per instruction, budget check at every backward branch. Replace with a guard page: at backward branches, emit a load from a stopper page. Normally readable (L1 hit, ~free). To preempt, `mprotect(PROT_NONE)` — the load faults, SIGSEGV handler redirects execution to a return stub.

## Part 1: Allocate InfiniteLoopStopperPage

### 1a. New file: `jit_stopper.go`

Standalone C mmap, independent of GuestMemory (works for both Rv8 and Abjit).

```go
/*
#include <sys/mman.h>
#include <signal.h>
#include <string.h>

static void* stopper_alloc() {
    return mmap(NULL, 4096, PROT_READ|PROT_WRITE,
                MAP_PRIVATE|MAP_ANON, -1, 0);
}
static void stopper_free(void* p) { munmap(p, 4096); }
static void stopper_arm(void* p)  { mprotect(p, 4096, PROT_NONE); }
static void stopper_disarm(void* p) { mprotect(p, 4096, PROT_READ|PROT_WRITE); }
*/
```

Add to JIT struct:
```go
stopperPage uintptr // address of the guard page
```

Methods:
- `NewStopperPage() uintptr` — alloc via `stopper_alloc()`
- `FreeStopperPage()` — free via `stopper_free()`
- `RequestPreemption()` — `stopper_arm()` → PROT_NONE
- `ClearPreemption()` — `stopper_disarm()` → PROT_READ|PROT_WRITE

**File**: `jit_stopper.go` (new)
**File**: `jit.go` — add `stopperPage` field, allocate in JIT init, free in cleanup

### 1b. SIGSEGV handler

Install a C signal handler via `sigaction` that intercepts SIGSEGV when the faulting address is the stopper page. The handler modifies the ucontext to redirect RIP to a preemption return stub.

In `jit_stopper.go` (C preamble) or a new `jit_stopper.c`:

```c
#include <signal.h>
#include <ucontext.h>

static void* g_stopper_page = NULL;
static void* g_preempt_stub = NULL;  // address of return thunk

static void stopper_sigaction(int sig, siginfo_t* info, void* ctx) {
    if (info->si_addr >= g_stopper_page &&
        info->si_addr < g_stopper_page + 4096) {
        // Fault is on stopper page — redirect to preempt stub.
        ucontext_t* uc = (ucontext_t*)ctx;
#ifdef __APPLE__
        uc->uc_mcontext->__ss.__rip = (uint64_t)g_preempt_stub;
#else
        uc->uc_mcontext.gregs[REG_RIP] = (uint64_t)g_preempt_stub;
#endif
        // Disarm the page so the stub doesn't re-fault.
        stopper_disarm(g_stopper_page);
        return;
    }
    // Not our page — chain to previous handler or re-raise.
}

static struct sigaction g_old_sigaction;

static void stopper_install_handler(void* page, void* stub) {
    g_stopper_page = page;
    g_preempt_stub = stub;
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_sigaction = stopper_sigaction;
    sa.sa_flags = SA_SIGINFO | SA_RESTART;
    sigaction(SIGSEGV, &sa, &g_old_sigaction);
    // Also SIGBUS on macOS (mprotect faults can be SIGBUS).
    sigaction(SIGBUS, &sa, NULL);
}
```

**Critical**: Go's runtime also uses SIGSEGV. We must chain: if the fault isn't on our page, forward to the previous handler (`g_old_sigaction`). Go installs its handler at runtime init, so our handler installed later takes priority and chains back.

**Preempt stub**: A small piece of native code (emitted once per JIT or as a static assembly stub) that:
1. Writes `jitPreempted` status to the appropriate location (sret for Rv8, State for Abjit)
2. Writes the current PC (tricky — see below)
3. Returns via RET (pops back through the trampoline)

The stub doesn't need to write back registers — the dispatch loop re-enters the interpreter which re-loads from x[]/f[]. Actually, **the stub must write back dirty registers**. But dirty regs are in host registers, and we don't know which ones are dirty at the point of the fault.

**Simpler approach**: The signal handler sets a flag. The preempt stub is placed right after the stopper load in the generated code:

```asm
; At each backward branch:
    MOV RAX, [stopperPageAddr]   ; faults if armed
    ; execution continues here (stopper was readable)
    JMP loop_target
```

When the SIGSEGV fires, the handler advances RIP past the `MOV` to a **bail-out sequence** that does writeback + return. But this bail-out must be emitted per backward branch site, which is what BudgetCheck already does.

**Better approach**: Each backward branch site already has the BudgetCheck pattern:
```
BudgetCheck: if (ic >= MaxIC) { writeback; return }
             goto target
```

Replace with:
```
StopperCheck: MOV scratch, [stopperPageAddr]   ; L1 hit or SIGSEGV
              TEST scratch, scratch             ; always 0 when readable
              JNZ preempt_exit                  ; never taken on fast path
              JMP target
preempt_exit: writeback; return jitPreempted
```

And the signal handler, instead of redirecting RIP, simply **writes a non-zero value to the first word of the stopper page and disarms it** (restores PROT_READ|PROT_WRITE). The faulting MOV instruction is retried by the kernel, this time it succeeds and reads the non-zero value, the TEST/JNZ takes the preempt_exit path cleanly.

This is much simpler: no RIP manipulation in the signal handler, no need for a separate stub, and the writeback happens normally through the emitted code.

```c
static void stopper_sigaction(int sig, siginfo_t* info, void* ctx) {
    if (info->si_addr >= g_stopper_page &&
        info->si_addr < g_stopper_page + 4096) {
        // Disarm: make readable again, but write a poison value.
        stopper_disarm(g_stopper_page);
        *(volatile uint64_t*)g_stopper_page = 1;  // non-zero = preempt
        // The kernel retries the faulting instruction, which now
        // succeeds and reads 1. The JIT's TEST+JNZ takes the exit path.
        return;
    }
    // Chain to Go's handler.
    if (g_old_sigaction.sa_flags & SA_SIGINFO) {
        g_old_sigaction.sa_sigaction(sig, info, ctx);
    } else if (g_old_sigaction.sa_handler != SIG_DFL &&
               g_old_sigaction.sa_handler != SIG_IGN) {
        g_old_sigaction.sa_handler(sig);
    }
}
```

And `ClearPreemption()` resets the word to 0 (it's already PROT_READ|PROT_WRITE after the handler disarmed it).

**File**: `jit_stopper.go` (new) or `jit_stopper.c` (new) + `jit_stopper.go` (Go wrapper)

## Part 2: New IR op + lowering

### 2a. Add IRStopperCheck

**File**: `ir.go` — add opcode:
```go
IRStopperCheck // Imm = stopper page addr, Imm2 = targetPC; jumps to Label(A) or exits
```

**File**: `highlevel.go` — add emitter method:
```go
func (e *Emitter) StopperCheck(target Label, targetPC uint64, stopperAddr int64) {
    preempt := e.NewLabel()
    tmp := e.Tmp()
    e.Const(tmp, stopperAddr)
    e.LoadAbsolute(tmp, tmp, I64)         // tmp = *stopperAddr
    e.BranchImm(tmp, 0, NE, preempt)     // if non-zero, preempt
    e.Jump(target)
    e.PlaceLabel(preempt)
    e.WriteBackAll()
    e.Ret(targetPC, jitPreempted, VRegZero)
}
```

Or implement as a single IR op that the lowerer handles directly (avoids allocating a temp VReg for the address):

**File**: `lower_amd64_ops.go` — add lowering for IRStopperCheck:
```asm
MOV RAX, imm64           ; stopperPageAddr
MOV RAX, [RAX]           ; load from page (faults if armed)
TEST RAX, RAX
JNZ preempt_label
JMP target_label
preempt_label:
  <writeback>
  <ret with jitPreempted>
```

**File**: `lower_amd64_abjit.go` — same lowering for Abjit path

### 2b. Replace BudgetCheck with StopperCheck

**File**: `jit_emit_ir.go`:
- `emitBranch()` (~line 2631): replace `e.irEm.BudgetCheck(targetLabel, target)` with `e.irEm.StopperCheck(targetLabel, target, stopperAddr)`
- `emitJAL()` (~line 2543): same replacement
- Add `stopperAddr int64` field to `emitter` struct, passed from JIT

### 2c. Add jitPreempted status

**File**: `highlevel.go` or `jit.go`:
```go
const jitPreempted = 8
```

## Part 3: Remove IC entirely

### 3a. Simplify advancePC (jit_emit_ir.go)

```go
func (e *emitter) advancePC(size uint64) {
    e.numInsns++
    e.pc += size
}
```

Delete: `emitICplusplus()`, `icEmitted` field, the IC logic in `emitBranch()` (lines 2607-2608).

### 3b. Remove IC VReg (emit.go, lower_amd64.go)

- `emit.go`: Remove `ic` field, `IC()` method, the `e.Tmp()` call for IC
- `lower_amd64.go`: Delete `VRIC`, renumber:
  ```
  VRXBase   = VRegTempStart + 0  // t64
  VRFBase   = VRegTempStart + 1  // t65
  VRMemBase = VRegTempStart + 2  // t66 (was +3)
  VRMemMask = VRegTempStart + 3  // t67 (was +4)
  VRRegFile = VRegTempStart + 4  // t68 (was +5)
  ```

### 3c. Remove IC from Rv8 lowerer (lower_amd64_rv8.go)

- Delete `stageICToScratch()` function
- Remove all `icStaged` calls (~5 sites)
- Remove IC load from prologue (lines ~189-205)

### 3d. Remove IC from Abjit lowerer (lower_amd64_abjit.go)

- Delete `stageICToState()` function
- Remove all calls (~5 sites)
- Remove IC load from prologue (lines ~168-180)
- Update all hardcoded offsets after removing IC from State struct

### 3e. Delete BudgetCheck + MaxIC (highlevel.go)

Delete `BudgetCheck()` method and `MaxIC` constant.

### 3f. Remove IC from Result struct + trampolines

- `internal/jitcall/call.go`: Remove `IC` field, Result becomes 24 bytes:
  ```go
  type Result struct {
      PC        uint64 // offset 0
      Status    uint64 // offset 8
      FaultAddr uint64 // offset 16
  }
  ```
- `internal/jitcall/call_amd64.s`: Copy 3 qwords, adjust offsets
- `jit_sandbox_amd64.S`: Same
- `jit_sandbox.c`: Update comments

### 3g. Remove IC from Abjit State (abjit/abjit.go)

Remove `IC uint64` field. Update all offset constants in `lower_amd64_abjit.go`.

### 3h. Update dispatch loop (jit.go, jit_abjit.go)

- Remove `cpu.cycle += res.IC`
- Change `return res.IC, err` → `return 0, err` (or estimate from numInsns)
- Handle `jitPreempted` status: return to Go dispatch loop
- `jit_abjit.go`: Remove `s.IC = 0` and `IC: s.IC`

## Execution order

1. Add stopper page allocation + signal handler (Part 1)
2. Add IRStopperCheck + lowering (Part 2a)
3. Replace BudgetCheck → StopperCheck at backward branches (Part 2b)
4. Remove emitICplusplus / simplify advancePC (Part 3a)
5. Remove IC VReg (Part 3b)
6. Remove IC from both lowerers (Part 3c, 3d)
7. Delete BudgetCheck + MaxIC (Part 3e)
8. Remove IC from Result + trampolines (Part 3f)
9. Remove IC from Abjit State (Part 3g)
10. Update dispatch loop (Part 3h)

Steps 3-10 are tightly coupled — do as one atomic change.

## Files to modify

| File | Changes |
|------|---------|
| `jit_stopper.go` (new) | C mmap page, signal handler, arm/disarm/clear API |
| `jit.go` | stopperPage field, jitPreempted const, dispatch loop |
| `jit_emit_ir.go` | Remove emitICplusplus, simplify advancePC, replace BudgetCheck |
| `ir.go` | Add IRStopperCheck opcode |
| `highlevel.go` | Add StopperCheck, remove BudgetCheck + MaxIC |
| `emit.go` | Remove IC Tmp() alloc + IC() method |
| `lower_amd64.go` | Delete VRIC, renumber VRMemBase/VRMemMask/VRRegFile |
| `lower_amd64_rv8.go` | Remove stageICToScratch, IC prologue, IC sret writes |
| `lower_amd64_abjit.go` | Remove stageICToState, IC prologue, update offsets |
| `lower_amd64_ops.go` | Add IRStopperCheck lowering |
| `abjit/abjit.go` | Remove IC from State |
| `jit_abjit.go` | Remove IC references |
| `internal/jitcall/call.go` | Remove IC from Result (24 bytes now) |
| `internal/jitcall/call_amd64.s` | Update sret copy offsets |
| `jit_sandbox_amd64.S` | Update sret layout |
| `jit_sandbox.c` | Update sret comments |

## Verification

```bash
go test -v -run TestJIT .
go test -v ./...
make bench-quick            # expect major speedup from IC removal
make bench-cpu              # MIPS should improve dramatically
```

Test preemption: run guest infinite loop, goroutine calls `RequestPreemption()` after timeout, verify JIT returns with `jitPreempted`.
