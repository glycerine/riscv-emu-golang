# Plan: Fully sandbox JIT via CGO trampoline

## Context

The JIT has two sandbox violations:
1. Passes Go heap pointers (`&cpu.x`, `&cpu.f`, `&cpu.fcsr`) to native code
2. JIT native code runs on the Go goroutine stack (spill slots, sret, callee saves)

Both must be eliminated. Additionally, switching RSP away from the goroutine stack in Go assembly is unsafe — the GC's `gentraceback` will panic if RSP falls outside `g.stack` bounds. The correct boundary is **CGO**: when Go calls into C, the runtime marks the goroutine as GC-quiescent and stops scanning its stack.

This plan replaces the Go assembly trampoline (`internal/jitcall/call_amd64.s`) with a C function + assembly helper called via CGO. The JIT code runs entirely in C mmap memory (guest memory allocation).

## Guest memory layout

```
Page 1  (0x0000-0x0FFF):  Guard — null pointer segfault catching
Page 2  (0x1000-0x1FFF):  Host reply (tohost/fromhost)
Page 3  (0x2000-0x2FFF):  Shadow register file (520 bytes, rest reserved)
Page 4+ (0x3000+):        Heap grows up — ELF code + data loaded here
...
Top of mmap:              Sandbox stack grows down — sret, JIT spill slots, scratch
```

### Shadow register file (guest offset 0x2000)

```
+0     x[0..31]    256 bytes
+256   f[0..31]    256 bytes
+512   fcsr        8 bytes (uint32, 8-aligned)
```

## Architecture

```
Go code                    CGO boundary              C mmap (sandbox)
────────                   ────────────              ──────────────────
jit.RunJIT()          ──>  C.jit_sandbox_call()  ──> jit_trampoline_asm()
  passes &cpu.x/f/fcsr      copies regs to shadow     switches RSP to sandbox stack
  passes regFile, stackTop   sets up sret in mmap      calls JIT fn (System V ABI)
                             calls asm helper           JIT runs: [RBP+r*8] = shadow
                             copies shadow back         spills on sandbox stack
                             returns JitResult          chains via JMP (stays in mmap)
                                                        returns via RET
```

## Files to modify

### 1. `guestmem.go` — constants and accessors

```go
const (
    GuestPageSize      = 4096
    GuestGuardOffset   = 0x0000
    GuestTohostOffset  = 0x1000
    GuestRegFileOffset = 0x2000
    GuestHeapOffset    = 0x3000
)

func (m *GuestMemory) RegFileBase() uintptr { return m.base + GuestRegFileOffset }
func (m *GuestMemory) StackTop() uintptr    { return m.base + m.guestSize }
```

### 2. New file: `jit_sandbox.go` — CGO declarations

```go
package riscv

/*
#include "jit_sandbox.h"
*/
import "C"
import "unsafe"

func sandboxCall(fn uintptr, cpu *CPU, regFile, stackTop uintptr,
    dcBase uintptr, dcMask, vBegin, segSize uint64) jitcall.Result {

    r := C.jit_sandbox_call(
        C.uintptr_t(fn),
        (*C.uint64_t)(unsafe.Pointer(&cpu.x[0])),
        (*C.uint64_t)(unsafe.Pointer(&cpu.f[0])),
        (*C.uint32_t)(unsafe.Pointer(&cpu.fcsr)),
        C.uintptr_t(cpu.mem.Base()), C.uint64_t(cpu.mem.Mask()),
        C.uintptr_t(regFile), C.uintptr_t(stackTop),
        C.uintptr_t(dcBase), C.uint64_t(dcMask),
        C.uint64_t(vBegin), C.uint64_t(segSize),
    )
    return jitcall.Result{PC: uint64(r.pc), IC: uint64(r.ic),
        Status: uint64(r.status), FaultAddr: uint64(r.fault_addr)}
}
```

### 3. New file: `jit_sandbox.h` — C header

```c
#ifndef JIT_SANDBOX_H
#define JIT_SANDBOX_H

#include <stdint.h>

typedef struct {
    uint64_t pc;
    uint64_t ic;
    uint64_t status;
    uint64_t fault_addr;
} JitResult;

JitResult jit_sandbox_call(
    uintptr_t fn,
    uint64_t *go_x, uint64_t *go_f, uint32_t *go_fcsr,
    uintptr_t mem_base, uint64_t mem_mask,
    uintptr_t reg_file, uintptr_t sandbox_stack_top,
    uintptr_t dc_base, uint64_t dc_mask,
    uint64_t vaddr_begin, uint64_t seg_size);

#endif
```

### 4. New file: `jit_sandbox.c` — C wrapper

The C function runs on the system stack (g0) after the CGO transition. It copies registers, sets up the sret buffer in guest mmap, calls the assembly trampoline, reads the result, copies back.

```c
#include "jit_sandbox.h"
#include <string.h>

// Sret buffer layout (144 bytes) — must match ir/lower_amd64_rv8.go expectations
typedef struct __attribute__((aligned(8))) {
    uint64_t pc;           // [0]
    uint64_t ic;           // [8]
    uint64_t status;       // [16]
    uint64_t fault_addr;   // [24]
    uint8_t  _pad1[56];    // [32-79]
    uint64_t fcsr_ptr;     // [80]
    uint64_t dc_base;      // [88]
    uint64_t dc_mask;      // [96]
    uint64_t vaddr_begin;  // [104]
    uint64_t seg_size;     // [112]
    uint64_t _pad2;        // [120]
    uint64_t mem_base;     // [128]
    uint64_t mem_mask;     // [136]
} Sret;                    // 144 bytes

// Assembly helper: switches RSP to sandbox stack, calls JIT fn, switches back.
// Defined in jit_sandbox_amd64.S
extern void jit_trampoline_asm(
    void *fn, void *sret, void *regfile,
    uintptr_t memBase, uint64_t memMask,
    void *sandbox_sp);

JitResult jit_sandbox_call(
    uintptr_t fn,
    uint64_t *go_x, uint64_t *go_f, uint32_t *go_fcsr,
    uintptr_t mem_base, uint64_t mem_mask,
    uintptr_t reg_file, uintptr_t sandbox_stack_top,
    uintptr_t dc_base, uint64_t dc_mask,
    uint64_t vaddr_begin, uint64_t seg_size)
{
    void *rf = (void*)reg_file;

    // ── Copy Go registers → shadow register file (C mmap) ──
    memcpy(rf, go_x, 256);
    memcpy((char*)rf + 256, go_f, 256);
    *(uint32_t*)((char*)rf + 512) = *go_fcsr;

    // ── Set up sret buffer on sandbox stack ──
    Sret *sret = (Sret*)((char*)sandbox_stack_top - sizeof(Sret));
    memset(sret, 0, sizeof(*sret));
    sret->fcsr_ptr    = reg_file + 512;
    sret->dc_base     = dc_base;
    sret->dc_mask     = dc_mask;
    sret->vaddr_begin = vaddr_begin;
    sret->seg_size    = seg_size;
    // mem_base/mem_mask: published by JIT prologue from R8/R9 into sret[128..136]

    // ── Call JIT via assembly trampoline ──
    // sandbox_sp = sret address: CALL pushes return addr below, JIT frame below that
    jit_trampoline_asm((void*)fn, sret, rf, mem_base, mem_mask, sret);

    // ── Read result from sret (still in guest mmap) ──
    JitResult result;
    result.pc         = sret->pc;
    result.ic         = sret->ic;
    result.status     = sret->status;
    result.fault_addr = sret->fault_addr;

    // ── Copy shadow register file → Go registers ──
    memcpy(go_x, rf, 256);
    memcpy(go_f, (char*)rf + 256, 256);
    *go_fcsr = *(uint32_t*)((char*)rf + 512);

    return result;
}
```

### 5. New file: `jit_sandbox_amd64.S` — assembly trampoline

Compiled by the C compiler (gcc/clang). Runs on the system stack (via CGO). Switches RSP to sandbox stack for the JIT call, then switches back.

```asm
.globl jit_trampoline_asm
jit_trampoline_asm:
    // System V ABI args:
    //   RDI = fn
    //   RSI = sret (in guest mmap)
    //   RDX = regfile (in guest mmap)
    //   RCX = memBase
    //   R8  = memMask
    //   R9  = sandbox_sp (= sret address)

    // Save callee-saved registers (on system stack — safe, this is C's stack)
    pushq   %rbx
    pushq   %rbp
    pushq   %r12
    pushq   %r13
    pushq   %r14
    pushq   %r15

    // Save system RSP in callee-saved register
    movq    %rsp, %rbx

    // Switch RSP to sandbox stack
    movq    %r9, %rsp

    // Shuffle registers for JIT calling convention:
    //   RDI = sret, RSI = regfile, R8 = memBase, R9 = memMask
    movq    %rdi, %rax          // save fn
    movq    %rsi, %rdi          // RDI = sret
    movq    %rdx, %rsi          // RSI = regfile → becomes RBP in JIT prologue
    movq    %rcx, %r11          // save memBase temporarily
    xorq    %rdx, %rdx          // RDX = 0 (unused by JIT prologue)
    xorq    %rcx, %rcx          // RCX = 0 (unused; fcsr via sret[80])
    movq    %r8, %r9            // R9 = memMask
    movq    %r11, %r8           // R8 = memBase

    // Call JIT-compiled native code
    callq   *%rax

    // Restore system RSP
    movq    %rbx, %rsp

    // Restore callee-saved registers
    popq    %r15
    popq    %r14
    popq    %r13
    popq    %r12
    popq    %rbp
    popq    %rbx

    retq
```

### 6. `jit.go` — use CGO trampoline

Replace all `jitcall.Call` / `jitcall.CallAOT` with `sandboxCall`:

```go
// Before (in RunJIT):
res = jitcall.CallAOT(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
    cpu.mem.Base(), cpu.mem.Mask(),
    seg.decoderCacheBase, seg.decoderCacheMask,
    seg.vaddrBegin, seg.vaddrSize)

// After:
res = sandboxCall(blk.fn, cpu,
    cpu.mem.RegFileBase(), cpu.mem.StackTop(),
    seg.decoderCacheBase, seg.decoderCacheMask,
    seg.vaddrBegin, seg.vaddrSize)
```

Same for `StepBlock` and the non-AOT `Call` path (pass zeros for dc params).

### 7. `ir/lower_amd64_rv8.go` — no changes

- `RBP = RSI` in prologue: RSI now points to guest offset 0x2000 (C mmap). Unchanged.
- `[RBP+r*8]` for x, `[RBP+256+r*8]` for f: layout matches shadow. Unchanged.
- `[RSP+slot*8]` for spill slots: RSP is on sandbox stack (C mmap). Unchanged.
- Sret buffer via stashed RDI pointer: on sandbox stack (C mmap). Unchanged.
- Chain exits: RBP persists, sret persists, both in C mmap. Unchanged.

### 8. `internal/jitcall/` — deprecate or remove

The Go assembly trampoline is no longer used. Can be removed or kept behind a build tag for debugging.

### 9. ELF loading — enforce minimum VA

Code must load at guest VA >= `GuestHeapOffset` (0x3000).

### 10. Test updates

- Tests using `codeVA = 0x1000` → `0x3000`
- `BuildELF` default VA → `0x3000`
- Riscv-tests ELFs: may need relinking if VAs < 0x3000

## Why CGO is safe here

When Go calls into C via CGO, the runtime:
1. Saves goroutine state and switches to g0 (system stack)
2. Marks the goroutine as `_Gsyscall` / in-CGO-call
3. GC scanning of this goroutine is suspended
4. The C function runs on the system stack — free to switch RSP

On return, the runtime restores everything. The GC never sees the sandbox stack or the shadow register file. Go heap pointers (`&cpu.x`, etc.) are passed as C arguments, used for memcpy, then forgotten — no retention beyond the call, satisfying CGO pointer rules.

## Performance

- **CGO overhead**: ~100-200ns per dispatch cycle. Acceptable for blocks executing hundreds+ of RISC-V instructions.
- **Copy overhead**: 516 bytes in + 516 bytes out (~16 cache lines). Same as before.
- **Zero overhead during chaining**: chained blocks jump directly to each other in C mmap, never crossing the CGO boundary.
- **Net impact**: the CGO overhead only applies at Go↔JIT transitions. AOT-chained workloads (dhrystone, coremark) will see minimal impact since most dispatch stays in-sandbox.

## Verification

1. `go test -v -run TestJIT_ADD .` — basic JIT
2. `go test -v -run TestJIT_vs_Interp_Registers .` — register writeback parity
3. `go test -v -run TestRISCVTests_UI_JIT .` — riscv-tests under JIT
4. `go test -v -run TestAOTInstall_RunDhrystone .` — AOT with chaining
5. `go test -v .` — full suite
6. Remote Linux box: `make test` — verify GC crash gone
7. `go test -gcflags=all=-d=checkptr .` — no unsafe.Pointer violations
