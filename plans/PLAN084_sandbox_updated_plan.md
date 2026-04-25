# Plan: Fully sandbox JIT via CGO trampoline

## Context

The JIT had two sandbox violations:
1. Passed Go heap pointers (`&cpu.x`, `&cpu.f`, `&cpu.fcsr`) to native code
2. JIT native code ran on the Go goroutine stack (spill slots, sret, callee saves)

Both are eliminated. Switching RSP away from the goroutine stack in Go assembly is unsafe — the GC's `gentraceback` panics if RSP falls outside `g.stack` bounds. The correct boundary is **CGO**: when Go calls into C, the runtime marks the goroutine as GC-quiescent and stops scanning its stack.

The old Go assembly trampoline (`internal/jitcall/call_amd64.s`) is replaced by a C function + assembly helper called via CGO. The JIT code runs entirely in C mmap memory (guest memory allocation).

## Guest memory layout

For a guest mmap `[base, base+guestSize)`, where `b = base + guestSize`:

```
[base, ...)                      ELF code + data (loaded at VA from ELF headers)
  ...
[b-3*4096, b-2*4096)            sandbox stack, grows DOWN from b-2*4096
[b-2*4096, b-4096)              guard page (catches stack underflow)
[b-4096,   b)                   shadow register file (520 bytes used)
```

- `RegFileBase() = b - 4096`
- `StackTop() = b - 8192`
- Minimum guest size: **64 MB** (`Size64MB`). Smaller sizes (`Size16KB`, `Size32KB`, `Size64KB`) are removed — they put the reserved area too close to guest code/data.

### Shadow register file layout (at `b - 4096`)

```
+0     x[0..31]    256 bytes  (32 × uint64)
+256   f[0..31]    256 bytes  (32 × uint64)
+512   fcsr        8 bytes    (uint32, 8-aligned)
```

Total: 520 bytes. Rest of the page is padding/containment.

### Why end-of-mmap works

Guest loads/stores use mask-bounded access: `host = base + (addr & mask)`. The shadow register file is at the last page of the mmap. A guest store can only reach it if `addr & mask` falls in the last page — for a 64 MB guest, that's VA 0x3FFF000+, far from any real ELF segment. An earlier design placed the register file at fixed low page 3 (VA 0x2000), but real ELF PT_LOAD segments (e.g., dhrystone rodata) overlap there.

### System RSP recovery

All callee-saved registers (RBX, RBP, R12-R15) are in the JIT's allocatable pool and will be clobbered during execution. The system RSP is stashed at `sret[32]` (an unused slot in the sret buffer, inside C mmap). After the JIT's RET, RSP equals the sret address, so `[RSP+32]` recovers the system RSP.

## Architecture

```
Go code                    CGO boundary              C mmap (sandbox)
────────                   ────────────              ──────────────────
jit.RunJIT()          ──>  C.jit_sandbox_call()  ──> jit_trampoline_asm()
  passes &cpu.x/f/fcsr      copies regs to shadow     saves callee-saved on sys stack
  passes regFile, stackTop   sets up sret in mmap      saves sys RSP to sret[32]
                             calls asm helper           switches RSP to sandbox stack
                             copies shadow back         calls JIT fn (System V ABI)
                             returns JitResult          JIT runs: [RBP+r*8] = shadow
                                                        spills on sandbox stack
                                                        chains via JMP (stays in mmap)
                                                        returns via RET
                                                        recovers sys RSP from sret[32]
```

## Files modified / created

### 1. `guestmem.go` — layout constants and accessors

- Removed `Size16KB`, `Size32KB`, `Size64KB` — minimum is `Size64MB`.
- `RegFileBase() = base + size - 4096`
- `StackTop() = base + size - 8192`

### 2. `jit_sandbox_cgo.go` (new) — Go CGO wrapper

Calls `C.jit_sandbox_call(...)` passing `&cpu.x`, `&cpu.f`, `&cpu.fcsr` (for copy-in/copy-out by the C side), plus `regFile` and `stackTop` (end-of-mmap addresses).

### 3. `jit_sandbox.h` (new) — C header

Declares `JitResult` struct and `jit_sandbox_call` function.

### 4. `jit_sandbox.c` (new) — C wrapper

Runs on the system stack (g0) after CGO transition:
1. `memcpy` cpu.x/f/fcsr → shadow register file (C mmap)
2. Build 144-byte sret buffer at `sandbox_stack_top - 144` (C mmap)
3. Call `jit_trampoline_asm` (switches RSP, calls JIT, switches back)
4. Read Result from sret
5. `memcpy` shadow → cpu.x/f/fcsr

### 5. `jit_sandbox_amd64.S` (new) — assembly trampoline

- Saves callee-saved regs on system stack
- Saves system RSP to temp, switches RSP to sandbox stack
- Stashes system RSP at `sret[32]` (JIT code never touches `[32..79]`)
- Shuffles registers for JIT calling convention: RDI=sret, RSI=regfile, R8=memBase, R9=memMask
- `CALL *%rax` (JIT function)
- After RET: `RSP = sret`, recovers system RSP from `[RSP+32]`
- Restores callee-saved regs, returns to C

### 6. `jit.go` — wired to `sandboxCall`

All `jitcall.Call` / `jitcall.CallAOT` sites in `RunJIT` and `StepBlock` replaced with:
```go
res = sandboxCall(blk.fn, cpu,
    cpu.mem.RegFileBase(), cpu.mem.StackTop(),
    seg.decoderCacheBase, seg.decoderCacheMask,
    seg.vaddrBegin, seg.vaddrSize)
```

### 7. `ir/lower_amd64_rv8.go` — no changes

- `RBP = RSI` in prologue: RSI now points to shadow in C mmap. Unchanged.
- `[RBP+r*8]` for x, `[RBP+256+r*8]` for f. Unchanged.
- `[RSP+slot*8]` for spill slots: RSP on sandbox stack. Unchanged.
- Sret buffer via stashed RDI pointer: on sandbox stack. Unchanged.
- Chain exits: RBP persists, sret persists, both in C mmap. Unchanged.

### 8. `internal/jitcall/` — retained for `ir/` and `bench/` tests

The Go assembly trampoline (`Call`/`CallAOT`) is no longer used by production code. It is retained because `ir/lower_amd64_test.go`, `ir/lower_exhaust_test.go`, `ir/lower_amd64_dcache_test.go`, and `bench/addimm_rsp_bench_test.go` call it directly to test the IR lowerer in isolation (different Go package, can't use `sandboxCall`).

### 9. Test updates (done)

- `riscv_test.go`: JIT and lockstep tests bumped from `Size32KB` to `Size64MB`.
- `jit_test.go`: Data addresses changed from `0x2000` to `0x4000` (avoids old conflict; harmless now).
- `jit_emit_ir_test.go`: `jitcallCall` helper rewired to use `sandboxCall` internally.
- `lockstep_v1v2_test.go`: Switched from `jitcall.Call` to `jitcallCall` (uses sandbox).
- `jit_decode_test.go`, `guestmem_exec_test.go`: Bumped to `Size64MB`.

## Why CGO is safe here

When Go calls into C via CGO, the runtime:
1. Saves goroutine state and switches to g0 (system stack)
2. Marks the goroutine as `_Gsyscall` / in-CGO-call
3. GC scanning of this goroutine is suspended
4. The C function runs on the system stack — free to switch RSP

On return, the runtime restores everything. The GC never sees the sandbox stack or the shadow register file. Go heap pointers (`&cpu.x`, etc.) are passed as C arguments, used for memcpy, then forgotten — no retention beyond the call, satisfying CGO pointer rules.

## Performance

- **CGO overhead**: ~100-200ns per dispatch cycle.
- **Copy overhead**: 520 bytes in + 520 bytes out (~16 cache lines).
- **Zero overhead during chaining**: chained blocks jump directly to each other in C mmap, never crossing the CGO boundary.
- **Net impact**: CGO overhead only at Go↔JIT transitions. AOT-chained workloads (dhrystone, coremark) stay in-sandbox.

## Verification

1. `go test -v -run TestJIT_ADD .` — basic JIT ✅
2. `go test -v -run 'TestJIT_' .` — all JIT tests ✅
3. `go test -v -run TestRISCVTests_UI_JIT .` — riscv-tests under JIT (in progress)
4. `go test -v -run TestAOTInstall_RunDhrystone .` — AOT with chaining (in progress)
5. `go test -v .` — full root package suite (in progress)
6. Remote Linux box: `make test` — verify GC crash gone
7. `go test -gcflags=all=-d=checkptr .` — no unsafe.Pointer violations
