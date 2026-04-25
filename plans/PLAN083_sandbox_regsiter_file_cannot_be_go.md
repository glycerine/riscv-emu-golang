# Plan: Isolate JIT native code from Go heap via shadow register file

## Context

The JIT trampoline (`internal/jitcall/call_amd64.s`) currently passes Go heap pointers directly to JIT-compiled native code:

- `RSI = &cpu.x` — pointer into heap-allocated CPU struct
- `RDX = &cpu.f` — same struct, offset 256
- `RCX = &cpu.fcsr` — same struct, offset 512
- `sret[80] = &cpu.fcsr` — stashed for chained blocks

This breaks the sandbox invariant: JIT guest code must never hold a pointer to Go heap memory. A buggy or malicious JIT block could corrupt the CPU struct or adjacent heap objects.

The fix: the trampoline copies cpu.x/f/fcsr into a **shadow register file** on its own 65536-byte stack frame, passes shadow pointers to native code, and copies back after the block returns. No changes to the IR lowerer — JIT code accesses registers as `[RBP+r*8]` via an opaque base pointer set from RSI.

## Files to modify

| File | Change |
|------|--------|
| `internal/jitcall/call_amd64.s` | Add copy-in/copy-out to both `Call` and `CallAOT` |
| `internal/jitcall/call.go` | Update doc comments to describe shadow copy semantics |

No changes to: `ir/lower_amd64_rv8.go`, `jit.go`, `jit_native.go`, or any other Go code. The JIT lowerer accesses registers as `[RBP+offset]` via an opaque base — it doesn't know or care whether RBP points to the CPU struct or a shadow.

## New stack frame layout

The trampoline's existing 65536-byte local frame has ~144 bytes used. The shadow fits easily.

```
[SP+0,   SP+32)    Result struct (PC, IC, Status, FaultAddr)         — unchanged
[SP+32,  SP+80)    Callee-saved regs (BX, BP, R12-R15)              — unchanged
[SP+80,  SP+88)    shadow fcsr ptr (→ SP+768)                       — CHANGED target
[SP+88,  SP+120)   decoder_cache params (AOT only)                  — unchanged
[SP+120, SP+128)   (unused)
[SP+128, SP+144)   memBase, memMask (published by JIT prologue)     — unchanged
[SP+144, SP+152)   saved original x pointer (for copy-back)         — NEW
[SP+152, SP+160)   saved original f pointer (for copy-back)         — NEW
[SP+160, SP+168)   saved original fcsr pointer (for copy-back)      — NEW
  ...
[SP+256, SP+512)   shadow x[0..31]  (256 bytes = 32 × uint64)      — NEW
[SP+512, SP+768)   shadow f[0..31]  (256 bytes = 32 × uint64)      — NEW
[SP+768, SP+776)   shadow fcsr      (4 bytes, 8-byte aligned)      — NEW
```

Total new usage: ~776 bytes. Well within the 65536-byte frame.

## Trampoline changes (both `Call` and `CallAOT`)

### Pre-call: copy IN

After saving callee-saved registers and zeroing the metadata region (existing code), add:

```asm
// ── Save original Go heap pointers for post-call copy-back ──
MOVQ    x+8(FP), AX
MOVQ    AX, 144(SP)
MOVQ    f+16(FP), AX
MOVQ    AX, 152(SP)
MOVQ    fcsr+24(FP), AX
MOVQ    AX, 160(SP)

// ── Copy x[32] to shadow (256 bytes = 32 qwords) ──
MOVQ    x+8(FP), SI          // source: Go heap x array
LEAQ    256(SP), DI           // dest: shadow x
MOVQ    $32, CX
REP; MOVSQ

// ── Copy f[32] to shadow (256 bytes = 32 qwords) ──
MOVQ    f+16(FP), SI          // source: Go heap f array
LEAQ    512(SP), DI           // dest: shadow f
MOVQ    $32, CX
REP; MOVSQ

// ── Copy fcsr to shadow (4 bytes) ──
MOVQ    fcsr+24(FP), AX
MOVL    (AX), AX
MOVL    AX, 768(SP)
```

Then set up System V ABI registers using **shadow** pointers instead of originals:

```asm
LEAQ    0(SP), DI             // RDI = sret pointer (unchanged)
LEAQ    256(SP), SI           // RSI = shadow x  (NOT &cpu.x)
LEAQ    512(SP), DX           // RDX = shadow f  (NOT &cpu.f)
LEAQ    768(SP), CX           // RCX = shadow fcsr (NOT &cpu.fcsr)
MOVQ    memBase+32(FP), R8    // R8 = memBase (C mmap, already safe)
MOVQ    memMask+40(FP), R9    // R9 = memMask

// Publish shadow fcsr pointer into sret[80]
LEAQ    768(SP), AX
MOVQ    AX, 80(SP)
```

### Post-call: copy OUT

After the native code returns (before copying Result to Go return area):

```asm
// ── Copy shadow x back to Go heap ──
LEAQ    256(SP), SI           // source: shadow x
MOVQ    144(SP), DI           // dest: original x (saved earlier)
MOVQ    $32, CX
REP; MOVSQ

// ── Copy shadow f back to Go heap ──
LEAQ    512(SP), SI           // source: shadow f
MOVQ    152(SP), DI           // dest: original f (saved earlier)
MOVQ    $32, CX
REP; MOVSQ

// ── Copy shadow fcsr back to Go heap ──
MOVL    768(SP), AX
MOVQ    160(SP), CX
MOVL    AX, (CX)
```

Then proceed with existing Result copy and callee-saved register restore.

## Why no lowerer changes are needed

1. **Register file access**: JIT prologue does `RBP = RSI` (line 152 of `lower_amd64_rv8.go`). All register reads/writes use `[RBP+r*8]`. RBP is an opaque base pointer — shadow at SP+256 has the exact same layout as cpu.x/cpu.f.

2. **Chain entry**: Chained blocks inherit RBP from the previous block. Since RBP was set from the shadow RSI, all chained blocks use the shadow. No RSI re-setup occurs at chain entry.

3. **FCSR access**: JIT code reads sret[80] to get the fcsr pointer. We publish the shadow fcsr address there. JIT code reads/writes through it unchanged.

4. **memBase/memMask**: These are `uintptr` values pointing to C mmap memory (not Go heap). They're passed via R8/R9 and published to sret[128..136]. No change needed.

5. **decoder_cache**: Published by CallAOT into sret[88..112]. Points to mmap memory. No change needed.

## Performance impact

Copy overhead per Call/CallAOT invocation:
- Copy-in: 256 + 256 + 4 = 516 bytes (~8 cache lines)
- Copy-out: same
- Total: ~1032 bytes, both sequential memcpy via REP MOVSQ

For a JIT block executing hundreds+ RISC-V instructions, this is negligible. For AOT chained execution (no round-trip to Go), there is zero overhead — the shadow is used continuously across chains; copying only happens at the Go↔JIT boundary.

## Verification

1. `go test -v -run TestJIT_ADD .` — basic JIT correctness
2. `go test -v -run TestJIT_vs_Interp_Registers .` — register writeback parity
3. `go test -v -run TestRISCVTests_UI_JIT .` — full riscv-tests suite under JIT
4. `go test -v -run TestAOTInstall_RunDhrystone .` — AOT with chaining
5. `go test -v .` — full unit test suite
6. On remote Linux box: `make test` — verify GC crash is resolved
