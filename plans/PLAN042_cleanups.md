# Three-Fix Cleanup Plan: Machine ergonomics, fuzz-fd oracle gap, lazy-mmap leak

## Context

Three independent housekeeping items accumulated through Phase 2a/2b/2c work.
All are small, orthogonal, and good to close before moving on to the next big
piece (os.go mmap/mprotect hooks, LuaJIT benchmark).

- **(a) Post-Clone handler re-installation ergonomics**: `Machine.Clone()`
  (Phase 2c) gives the child a fresh `NoteChain`. The caller has to remember
  to re-push OS syscall handlers on the child. A one-line convenience method
  eliminates the footgun.
- **(b) fuzz-fd NaN-payload mismatch** on seed
  `FuzzFDVsLibriscv/8771db92cbf685f9`: we are spec-compliant; libriscv is
  not. Input: FDIV.S(-0.0, -0.0) — RISC-V unprivileged ISA §11.3 mandates
  canonical qNaN `0x7FC00000`; libriscv returns `0xFFC00000`. The bug
  lives in libriscv's **binary translator** (`tr_emit.cpp:1899-1905`),
  *not* the interpreter. Previous fixes (887eaa7 FMADD, b872cff/aaa795c
  FMIN/FMAX) did not touch FDIV. The proven-red test is still red:
  running `go test -run '^FuzzFDVsLibriscv$/8771db92cbf685f9$' ./fuzzoracle/`
  fails with `ours=0x...7FC00000 libriscv=0x...FFC00000`. We'll fix
  libriscv in-place using the same callback pattern commit 887eaa7 used
  for FMA.
- **(c) Lazy-block mmap leak on JIT.Close**: every lazy-compiled block
  `allocExec`s its own mmap, but `compiledBlock` only stores a `uintptr fn`
  — the `[]byte` slice returned by `allocExec` is discarded immediately.
  `JIT.Close()` currently documents this as outside Phase 2b's scope
  (`jit.go:285`). Simple fix: retain the mmap and munmap on Close.

## Fix (a): `Machine.InstallOS(*OS)`

### What the research found
- `NoteChain` (note.go) is a simple slice-based handler stack; `Push(h)`
  appends a `NoteHandler`.
- `OS` (os.go:65-68) is functionally stateless: a `map[uint64]SyscallHandler`
  + optional fallback. Sharing one `*OS` across parent and child is safe.
- Standard installation pattern everywhere in the codebase is:
  ```go
  cpu.Notes.Push(os.Handle)
  ```
  So the helper is genuinely one line.

### Proposed change

Add to `machine.go`:

```go
// InstallOS pushes the given OS's Handle onto the Machine's CPU
// NoteChain. Commonly used after Clone() to re-enable syscall
// handling on the child (the child's NoteChain starts fresh).
//
// The same *OS can be installed on both parent and child — OS is
// stateless (its syscall table is immutable after construction and
// handlers mutate only the per-call *CPU).
func (m *Machine) InstallOS(os *OS) {
    m.CPU.Notes.Push(os.Handle)
}
```

Update the `Clone()` doc-comment to point at `InstallOS` as the
obvious follow-up.

### Test (append to machine_clone_test.go)

```go
func TestMachineClone_InstallOSOnChild(t *testing.T) {
    // Parent + child share the same OS; each ECALLs independently
    // and both reach the syscall handler.
    //
    // Assertions: both CPUs exit via ECALL → LinuxExit → ExitError.
    // A spy counter (captured by a tiny stub in the OS table)
    // bumps on each side to prove the handler ran on both.
}
```

## Fix (b): libriscv binary-translator NaN canonicalization

### What the research found
- Seed 8771db92cbf685f9 decodes to FDIV.S with inputs f2 = -0.0, f3 = -0.0.
- Our `cpu.go:694` on FDIV.S explicitly canonicalizes via `canonNaN32`
  (float.go:94) to `0x7FC00000` — spec-compliant.
- libriscv has TWO code paths for FP ops:
  - **Interpreter** (`rvf_instr.cpp:327-353`): calls `fsflags(cpu, exact,
    dst.f32[0])` which DOES canonicalize via a T& reference when
    `RISCV_FCSR` is defined (it is — see
    `xendor/libriscv/build_capi/libriscv/libriscv_settings.h`).
  - **Binary translator** (`tr_emit.cpp:1899-1905`): emits
    `set_fl(&dst, rs1.f32[0] / rs2.f32[0])`. `set_fl` is just
    `(reg)->f32[0] = (fv)` (tr_api.cpp:185) — no canonicalization.
- libriscv uses the binary translator on hot code; the interpreter path
  never runs for a tight FDIV loop. So even though the interpreter is
  correct, the oracle sees the translator's output.
- Commit 887eaa7 fixed FMADD the same way by adding an `fmaf32`/`fmaf64`
  callback in `tr_translate.cpp:1440-1458` that canonicalizes NaN
  results. The pattern is established; FDIV/FADD/FSUB/FMUL/FSQRT just
  need the same treatment.
- Commits b872cff and aaa795c only touched FMIN/FMAX. FDIV is untouched.
- **Red test, still red** — verified by running:
  ```
  go test -run '^FuzzFDVsLibriscv$/8771db92cbf685f9$' -v ./fuzzoracle/
  → fuzz_float_test.go:219: f1: ours=0xFFFFFFFF7FC00000 libriscv=0xFFFFFFFFFFC00000
  ```

### Proposed change

Apply the FMA-style callback pattern to every binary-translator FP op
that can produce a NaN from non-NaN inputs. Minimum set:

- FADD.S / FADD.D
- FSUB.S / FSUB.D
- FMUL.S / FMUL.D
- FDIV.S / FDIV.D
- FSQRT.S / FSQRT.D (already has callbacks `sqrtf32`/`sqrtf64` in
  `tr_translate.cpp:1432-1437`, but without NaN canonicalization —
  extend these)

For each, add or extend a callback in the `CallbackTable` populated in
`tr_translate.cpp` around line 1440, following the FMA template:

```cpp
.divf32 = [] (float a, float b) -> float {
    float r = a / b;
    if (std::isnan(r)) {
        uint32_t canon = 0x7FC00000u;
        float out;
        __builtin_memcpy(&out, &canon, 4);
        return out;
    }
    return r;
},
.divf64 = [] (double a, double b) -> double { /* parallel, canon = 0x7FF8000000000000 */ },
// ...addf32/addf64, subf32/subf64, mulf32/mulf64
```

Extend the existing sqrt callbacks to canonicalize:

```cpp
.sqrtf32 = [] (float f) -> float {
    float r = std::sqrt(f);
    if (std::isnan(r)) {
        uint32_t canon = 0x7FC00000u;
        float out; __builtin_memcpy(&out, &canon, 4); return out;
    }
    return r;
},
```

Declare the new callbacks in `tr_api.hpp` (next to the existing
`sqrtf32/sqrtf64/fmaf32/fmaf64` fields — see commit 887eaa7's additions
to `tr_api.hpp` for the signature pattern).

Update `tr_emit.cpp:1899-1905` (FDIV case) and the analogous cases
for FADD/FSUB/FMUL/FSQRT to emit a call through the new callback
instead of the raw host operator, following the same emit pattern
commit 887eaa7 established for FMA.

### After the libriscv fix, rebuild + re-run

- `make bench-setup` (re-builds libriscv with the patched translator).
- The failing seed should now pass with no oracle changes — both sides
  produce `0x7FC00000`.
- **No changes to fuzzoracle test code.** We keep the oracle strict;
  if other NaN-canonicalization gaps exist in libriscv for ops we
  haven't patched yet, they'll surface as future failing seeds —
  exactly what we want.

### Verification

New targeted test `TestFDivNegZeroCanonicalNaN_Libriscv` in
`fuzzoracle/float_oracle_test.go` (red before the libriscv patch, green
after) — direct assertion, independent of the fuzz corpus:

```go
// Build: FDIV.S f1, f2, f3 ; ECALL — run libriscv alone, read f1.
// Expect 0x7FC00000 in lower 32 bits (NaN-boxed as 0xFFFFFFFF7FC00000).
```

This lets us unambiguously prove the libriscv fix without relying on
the fuzz seed filename.

## Fix (c): Lazy-block mmap leak

### What the research found
- `jit_native.go:226-239` `allocExec` returns `[]byte` per call.
- `jitCompileWith` (jit_native.go line ~78-87) captures `codeBase :=
  uintptr(unsafe.Pointer(&execMem[0]))` and stores ONLY `fn: codeBase` in
  the new `compiledBlock`. The `execMem []byte` goes out of scope.
- Nothing else holds the slice; the underlying mmap lives forever
  (beyond JIT.Close).
- Each lazy block is its own mmap (page-sized minimum, ~4KB).
- Dispatch counters from bench-chain-ref: coremark compiles 6 lazy blocks,
  dhrystone 95, bench_guest similar. Bounded per workload.
- Chain-exit patches across lazy blocks pin their target blocks' code
  addresses — evicting a block from the direct-mapped cache does not make
  its mmap safe to free (another block may still jump to it).

### Proposed change

Pick **Option B** from research (separate registry, munmap only on Close).
Preserves chain-exit validity for the JIT's entire lifetime; Option A
(munmap on cache eviction) would dangle chain patches and segfault.

Changes:

1. `jit.go` — extend `compiledBlock`:

   ```go
   type compiledBlock struct {
       fn         uintptr
       ...existing fields...
       segment    *DecodedExecuteSegment
       
       // nativeMmap is the per-block code slab for lazy-compiled blocks.
       // nil for AOT blocks (their code lives in segment.nativeCodeMmap,
       // reclaimed by segment.Release). Held here so JIT.Close can munmap.
       nativeMmap []byte
   }
   ```

2. `jit.go` — add a per-JIT registry:

   ```go
   type JIT struct {
       ...existing fields...
       
       // lazyBlocks holds every lazy-compiled block, keeping its
       // nativeMmap alive for JIT.Close to munmap. Grows unbounded
       // per JIT lifetime; bounded by the number of distinct PCs
       // ever lazily compiled.
       lazyBlocks []*compiledBlock
   }
   ```

3. `jit_native.go:~78-87` in `jitCompileWith` — retain the slice and
   register the block:

   ```go
   execMem, err := allocExec(len(code))
   if err != nil { return nil, err }
   copy(execMem, code)
   codeBase := uintptr(unsafe.Pointer(&execMem[0]))
   blk := &compiledBlock{
       fn:         codeBase,
       nativeMmap: execMem,
   }
   // ...rest of block setup...
   j.lazyBlocks = append(j.lazyBlocks, blk)
   ```

   (Same retention at every `allocExec` call site that produces a lazy
   block — e.g., `jitCompileV2`, TCC path if applicable. Research cited
   `jit_native.go` only; verify during implementation.)

4. `jit.go` — extend `Close()`:

   ```go
   func (j *JIT) Close() {
       for _, s := range j.aotSegments {
           s.Release()
       }
       j.aotSegments = nil
       j.hotSegment = nil
       j.soleSegment = nil
       
       // Phase 2c: free per-block lazy mmaps.
       for _, blk := range j.lazyBlocks {
           if len(blk.nativeMmap) > 0 {
               _ = syscall.Munmap(blk.nativeMmap)
               blk.nativeMmap = nil
               blk.fn = 0
           }
       }
       j.lazyBlocks = nil
   }
   ```

5. `jit.go` — `CloneShared` must NOT copy `lazyBlocks` (child compiles its
   own lazy blocks; parent's registry keeps parent's mmaps alive). Since
   `CloneShared` constructs a `&JIT{}` literal and does not touch
   `lazyBlocks`, this is already correct — just add a comment.

6. `jit.go:285` — remove the "those leak today" comment; replace with one
   sentence noting Close now frees lazy mmaps.

### Tests (new file `jit_lazy_close_test.go`)

```go
// TestJIT_Close_FreesLazyMmaps: run a small ELF through lazy JIT
// compilation so several lazyBlocks exist, confirm len(j.lazyBlocks)>0,
// call Close, confirm each block's nativeMmap is nil and fn is 0, and
// that Close is idempotent (second call is a no-op).
```

Adjacent: re-running the bench-chain-ref suite after the change, verify
`DispatchCompile` counter unchanged (same number of lazy compiles) and
MIPS within ±2% of Phase 2c baseline.

## Files to create / modify

| file | change |
|------|--------|
| `machine.go` | add `InstallOS` method + doc |
| `machine_clone_test.go` | add `TestMachineClone_InstallOSOnChild` |
| `xendor/libriscv/lib/libriscv/tr_api.hpp` | declare new callbacks: `addf32/64, subf32/64, mulf32/64, divf32/64`; extend `sqrtf32/64` signature if needed |
| `xendor/libriscv/lib/libriscv/tr_translate.cpp` | populate new callbacks (around line 1440) with NaN canonicalization; canonicalize in existing `sqrtf32/64` |
| `xendor/libriscv/lib/libriscv/tr_emit.cpp` | rewrite FADD/FSUB/FMUL/FDIV/FSQRT cases to emit callback invocations instead of raw host operators |
| `fuzzoracle/float_oracle_test.go` | add `TestFDivNegZeroCanonicalNaN_Libriscv` as a direct red-then-green assertion for the patch |
| `jit.go` | add `nativeMmap []byte` to `compiledBlock`, `lazyBlocks []*compiledBlock` to `JIT`; update `Close` to munmap lazy blocks; remove the "those leak today" comment; add comment to `CloneShared` noting it does not share `lazyBlocks` |
| `jit_native.go` | retain `execMem` on block, append to `j.lazyBlocks` — at every lazy-compile call site (verify there's only one: `jitCompileWith`; check `jitCompileV2` too) |
| `jit_lazy_close_test.go` (new) | `TestJIT_Close_FreesLazyMmaps` |

Unchanged: AOT path (segment-owned mmap), interpreter, CPU, GuestMemory,
JIT hot path (dispatch), ir/ package, internal/jitcall.

## Execution order

1. **Fix (c) first** (lazy-block leak). Largest surface; benefits from
   confirming no regression before the other two changes land. Run
   `go test . ./bench/`, `make bench-chain-ref`, verify MIPS unchanged.
2. **Fix (a)** (`InstallOS`). Trivial; add + test.
3. **Fix (b)** (libriscv binary-translator NaN canon). Patch
   `tr_api.hpp`, `tr_translate.cpp`, `tr_emit.cpp`. Add
   `TestFDivNegZeroCanonicalNaN_Libriscv` (red). Rebuild libriscv
   (`make bench-setup`). Re-run the direct test → green. Re-run
   `make fuzz-fd` → fuzz corpus seed 8771db92cbf685f9 now passes
   with no oracle changes.
4. **Regression sweep**:
   ```bash
   go test . ./ir/ ./bench/
   make fuzz-oracle    # 60s
   make fuzz-fd        # 60s  ← should now pass
   make fuzz-rvc       # 60s
   make fuzz-amo       # 60s
   make fuzz-bitmanip  # 60s
   make bench-chain-ref
   ```

## Verification

### Correctness
- `go test . ./ir/ ./bench/` green.
- `make fuzz-fd` now passes (previously failing seed is exempted with
  log message).
- `TestJIT_Close_FreesLazyMmaps` green.
- `TestMachineClone_InstallOSOnChild` green.
- Phase 2b + 2c existing tests stay green (refcount balance, multi-
  segment dispatch, Clone memory isolation).

### Performance
- bench-chain-ref MIPS within ±2% of Phase 2c baseline on all three
  workloads (coremark / dhrystone / bench_guest). Fix (c) only runs at
  Close; fixes (a) and (b) don't touch the hot path.

### Behavioral
- Running a Machine with InstallOS(linuxOS) behaves identically to the
  current `cpu.Notes.Push(linuxOS.Handle)` + RunJIT pattern — same exits,
  same syscalls intercepted.

## Non-goals

- **No cache-eviction-time munmap** for lazy blocks. Chain-exit patches
  pin block addresses across blocks; freeing on eviction would dangle
  them. Accept unbounded per-JIT lazy memory growth (bounded in practice
  by distinct PCs compiled).
- **No compacting GC for lazy blocks.** Separate concern; Phase 2c scope
  is plugging the Close leak only.
- **No broad FP-spec overhaul in libriscv.** We canonicalize NaN outputs
  on the five ops that can produce NaN from non-NaN inputs (FADD, FSUB,
  FMUL, FDIV, FSQRT). Sign-preservation ops (FSGNJ, FMV) and selection
  ops (FMIN, FMAX — already fixed by commit b872cff) are unchanged.
  Conversion ops (FCVT.W.S, FCVT.S.W, etc.) are not in this pass — if
  a future fuzz seed surfaces a mismatch there, fix it then.
- **No oracle loosening.** We keep the fuzz oracle strict so any other
  libriscv non-conformance surfaces as a new failing seed.
- **No OS-personality-specific install helpers** (`InstallLinux`, etc.).
  `InstallOS(*OS)` is the minimum; richer helpers can come with a
  concrete Linux-personality pass if one is needed.

## Risks / edge cases

- **Chain exits to evicted-from-cache lazy blocks**: existing pre-Phase-2c
  behavior. Fix (c) doesn't change this — evicted blocks remain in
  `lazyBlocks` and their code stays mapped. This is a *fix*, not a
  regression.
- **CloneShared sharing lazy blocks accidentally**: by construction, the
  child's `JIT` literal in `CloneShared` does not touch `lazyBlocks`, so
  it starts at `nil`. Double-check after implementation that the slice
  isn't accidentally initialized by a copy of j.
- **Close called twice**: the second iteration sees each `blk.nativeMmap`
  already nil from the first call, so Munmap is skipped. Explicitly
  setting `j.lazyBlocks = nil` makes the second call a no-op loop.
- **Concurrent Close vs RunJIT**: existing pattern requires no
  concurrency; CloneShared and Close are single-goroutine per JIT.
- **fuzz-fd double-precision counterparts**: if a future seed surfaces
  for FDIV.D with -0.0/-0.0, the same libriscv bug may fire with
  `0xFFC00000_0000_0000` (D-precision canonical neg NaN) vs our
  `0x7FF8_0000_0000_0000`. Extend the helper when/if that happens; not
  speculatively added.
- **InstallOS double-install**: calling InstallOS twice pushes the same
  handler twice, which is harmless (second invocation is never reached
  because the first one handles or forwards). Documented.

## What to expect

Each fix is small and self-contained. The entire change set should land
in < 200 lines of code + tests. No hot-path code touched; benchmarks
should be identical to Phase 2c medians. fuzz-fd goes from red to green.
