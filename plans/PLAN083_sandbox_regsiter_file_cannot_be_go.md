# Plan: Fully sandbox JIT — register file + stack in C mmap

## Context

The JIT trampoline currently passes Go heap pointers (`&cpu.x`, `&cpu.f`, `&cpu.fcsr`) to native code, and the JIT code's stack (spill slots, sret stash, scratch) runs on the Go goroutine stack. Both are sandbox violations: guest code must only touch C mmap memory. This plan moves the register file backing store AND the JIT call stack into the guest memory mmap.

## Guest memory layout

Using the C mmap allocation (`guest_alloc`), with 4096-byte pages:

```
Page 1  (0x0000-0x0FFF):  Guard — null pointer segfault catching
Page 2  (0x1000-0x1FFF):  Host reply (tohost/fromhost)
Page 3  (0x2000-0x2FFF):  Shadow register file (520 bytes used, rest reserved)
Page 4+ (0x3000+):        Heap, grows up — ELF code + data loaded here
...
Top of mmap:              Stack, grows down
```

### Shadow register file layout (at guest offset 0x2000)

```
+0     x[0..31]    256 bytes  (32 × uint64)
+256   f[0..31]    256 bytes  (32 × uint64)
+512   fcsr        8 bytes    (uint32, 8-byte aligned)
```

### Sandbox stack

Starts at the top of the mmap, grows downward. The trampoline switches RSP to this address before `CALL AX` and restores the Go RSP after return.

## Files to modify

### 1. `guestmem.go` — add constants and accessors

```go
const (
    GuestPageSize       = 4096
    GuestGuardOffset    = 0x0000  // page 1: null guard
    GuestTohostOffset   = 0x1000  // page 2: tohost/fromhost
    GuestRegFileOffset  = 0x2000  // page 3: shadow register file
    GuestHeapOffset     = 0x3000  // page 4+: heap start
)

func (m *GuestMemory) RegFileBase() uintptr {
    return m.base + GuestRegFileOffset
}

func (m *GuestMemory) StackTop() uintptr {
    return m.base + m.guestSize  // top of mmap, stack grows down
}
```

No change to allocation size — these pages are already within the existing mmap. Code/data loading must respect `GuestHeapOffset` as the minimum VA (existing ELFs may need adjustment; see verification section).

### 2. `internal/jitcall/call.go` — add regFile and stackTop parameters

```go
func Call(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
    memBase uintptr, memMask uint64,
    regFile uintptr, sandboxStack uintptr) Result

func CallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
    memBase uintptr, memMask uint64,
    decoderCacheBase uintptr, decoderCacheMask uint64,
    vaddrBegin uint64, segSize uint64,
    regFile uintptr, sandboxStack uintptr) Result
```

### 3. `internal/jitcall/call_amd64.s` — full sandbox isolation

The trampoline does three new things:
1. **Copy-in**: cpu.x/f/fcsr → shadow register file (C mmap)
2. **Switch RSP**: Go stack → sandbox stack (C mmap)
3. **Copy-out + restore**: after return, copy shadow → cpu.x/f/fcsr, restore Go RSP

Pseudocode for `Call`:

```asm
TEXT ·Call(SB), $65536-??
    // ── Save callee-saved registers (on Go stack) ──
    MOVQ    BX,  32(SP)
    MOVQ    BP,  40(SP)
    MOVQ    R12, 48(SP)
    MOVQ    R13, 56(SP)
    MOVQ    R14, 64(SP)
    MOVQ    R15, 72(SP)

    // ── Save original Go pointers for copy-back ──
    MOVQ    x+8(FP), AX
    MOVQ    AX, 80(SP)             // stash &cpu.x
    MOVQ    f+16(FP), AX
    MOVQ    AX, 88(SP)             // stash &cpu.f
    MOVQ    fcsr+24(FP), AX
    MOVQ    AX, 96(SP)             // stash &cpu.fcsr

    // ── Copy cpu.x → shadow x (256 bytes) ──
    MOVQ    x+8(FP), SI
    MOVQ    regFile+??(FP), DI
    MOVQ    $32, CX
    REP; MOVSQ

    // ── Copy cpu.f → shadow f (256 bytes) ──
    MOVQ    f+16(FP), SI
    MOVQ    regFile+??(FP), DI
    ADDQ    $256, DI
    MOVQ    $32, CX
    REP; MOVSQ

    // ── Copy fcsr → shadow (4 bytes) ──
    MOVQ    fcsr+24(FP), AX
    MOVL    (AX), AX
    MOVQ    regFile+??(FP), DI
    MOVL    AX, 512(DI)

    // ── Save Go RSP, switch to sandbox stack ──
    MOVQ    SP, 104(SP)            // stash Go SP (must be before switch!)
    // Actually: save Go SP to a callee-saved register since SP is about to change
    MOVQ    SP, R12                // R12 = Go SP (callee-saved, restored later)
    MOVQ    sandboxStack+??(FP), SP  // RSP = top of sandbox stack (C mmap)

    // ── Build sret buffer on sandbox stack ──
    SUBQ    $144, SP               // allocate sret area on sandbox stack
    MOVQ    $0, 0(SP)              // zero Result area
    MOVQ    $0, 8(SP)
    MOVQ    $0, 16(SP)
    MOVQ    $0, 24(SP)

    // Publish fcsr pointer (shadow, in C mmap)
    MOVQ    regFile+??(R12), AX    // read regFile from Go stack via saved R12
    ADDQ    $512, AX
    MOVQ    AX, 80(SP)

    // Publish memBase/memMask (zeroed; JIT prologue will fill from R8/R9)
    MOVQ    $0, 128(SP)
    MOVQ    $0, 136(SP)

    // ── Set up System V ABI ──
    LEAQ    0(SP), DI              // RDI = sret (on sandbox stack)
    MOVQ    regFile+??(R12), SI    // RSI = shadow register file (C mmap)
    MOVQ    memBase+32(R12), R8    // R8 = memBase (read from Go stack via R12)
    MOVQ    memMask+40(R12), R9    // R9 = memMask

    MOVQ    fn+0(R12), AX          // fn (read from Go stack via R12)
    CALL    AX

    // ── Read Result from sandbox stack ──
    MOVQ    0(SP), AX              // Result.PC
    MOVQ    8(SP), CX              // Result.IC
    MOVQ    16(SP), DX             // Result.Status
    MOVQ    24(SP), SI             // Result.FaultAddr

    // ── Restore Go RSP ──
    MOVQ    R12, SP

    // ── Write Result to Go return area ──
    MOVQ    AX, ret_PC+??(FP)
    MOVQ    CX, ret_IC+??(FP)
    MOVQ    DX, ret_Status+??(FP)
    MOVQ    SI, ret_FaultAddr+??(FP)

    // ── Copy shadow x → cpu.x ──
    MOVQ    regFile+??(FP), SI
    MOVQ    80(SP), DI             // original &cpu.x
    MOVQ    $32, CX
    REP; MOVSQ

    // ── Copy shadow f → cpu.f ──
    MOVQ    regFile+??(FP), SI
    ADDQ    $256, SI
    MOVQ    88(SP), DI             // original &cpu.f
    MOVQ    $32, CX
    REP; MOVSQ

    // ── Copy shadow fcsr → cpu.fcsr ──
    MOVQ    regFile+??(FP), AX
    MOVL    512(AX), AX
    MOVQ    96(SP), CX
    MOVL    AX, (CX)

    // ── Restore callee-saved registers ──
    MOVQ    32(SP), BX
    MOVQ    40(SP), BP
    MOVQ    48(SP), R12
    MOVQ    56(SP), R13
    MOVQ    64(SP), R14
    MOVQ    72(SP), R15
    RET
```

`CallAOT` is identical but also publishes decoder_cache params to sret[88..112] on the sandbox stack.

**Note**: The exact FP offsets (`??`) depend on the final argument layout and will be computed during implementation. R12 is used to access Go stack arguments after the RSP switch because FP-relative addressing uses RSP internally — after switching RSP to the sandbox stack, FP offsets no longer work. This needs careful attention; an alternative is to load all Go arguments into callee-saved registers before the switch.

### 4. `jit.go` — pass regFile and sandboxStack

All Call/CallAOT sites in `RunJIT` and `StepBlock`:

```go
res = jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
    cpu.mem.Base(), cpu.mem.Mask(),
    cpu.mem.RegFileBase(), cpu.mem.StackTop())
```

Same pattern for CallAOT with the additional decoder_cache args.

### 5. `ir/lower_amd64_rv8.go` — sret buffer is now on sandbox stack

The sret buffer layout is the same, but it lives on the sandbox stack instead of the Go stack. The JIT prologue and block code access sret via the stashed pointer (from RDI), so no offset changes. The key invariant — `[sret+80]` = fcsr pointer, `[sret+128]` = memBase, etc. — is preserved.

The prologue still does `RBP = RSI` where RSI now points to guest offset 0x2000 (the shadow register file in C mmap). No lowerer changes needed.

### 6. ELF loading — enforce minimum VA

ELF code must load at guest VA >= `GuestHeapOffset` (0x3000). The ELF loader should validate this:

```go
// In LoadELFBytes or equivalent:
if segVA < GuestHeapOffset {
    return 0, fmt.Errorf("ELF segment VA 0x%x below reserved area 0x%x", segVA, GuestHeapOffset)
}
```

Existing riscv-tests ELFs use VAs starting at ~0x0000. These will need relinking with a higher base address, or the `BuildELF` helper updated to default to VA >= 0x3000.

### 7. Test updates

Tests that use `codeVA = 0x1000` or similar low addresses need to be bumped to `0x3000` or higher. Affected patterns:
- `cpu_test.go:187`: `const codeVA = uint64(0x1000)` → `0x3000`
- `jit_emit_ir_test.go`: `pc := uint64(0x1000)` → `0x3000`
- `BuildELF` default VA in tests
- Riscv-tests ELFs in `riscv-elf-tests/` — may need relinking

## What does NOT change

- **IR lowerer** (`lower_amd64_rv8.go`): `RBP = RSI`, register access at `[RBP+r*8]`, spill slots at `[RSP+slot*8]` — all unchanged. The only difference is WHERE RSI/RSP point (C mmap instead of Go memory).
- **JIT compilation** (`jit_native.go`, `jit_aot.go`): No changes.
- **Block chaining**: RBP persists across chain exits, pointing to guest offset 0x2000. Chain exits dealloc/realloc frames on the sandbox stack.
- **Guest memory access**: Mask-bounded via R14/R15, unchanged.

## Performance impact

- Copy overhead: 516 bytes in + 516 bytes out per Call/CallAOT (~16 cache lines).
- RSP switch: 2 MOVQs (save Go SP, load sandbox SP).
- The register file (page 3) and sandbox stack (top of mmap) are both in the C mmap. During JIT execution, all accessed memory (registers, spills, guest memory) is in the same large allocation — good for TLB.
- Zero overhead during AOT chaining (no Go round-trip).

## Verification

1. `go test -v -run TestJIT_ADD .` — basic JIT
2. `go test -v -run TestJIT_vs_Interp_Registers .` — register writeback parity
3. `go test -v -run TestRISCVTests_UI_JIT .` — riscv-tests suite under JIT
4. `go test -v -run TestAOTInstall_RunDhrystone .` — AOT with chaining
5. `go test -v .` — full suite
6. Remote Linux box: `make test` — verify GC crash is gone
7. `go test -v -gcflags=all=-d=checkptr .` — verify no unsafe.Pointer violations remain
