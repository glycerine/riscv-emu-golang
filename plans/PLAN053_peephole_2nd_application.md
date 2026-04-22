# Step 1: Lock in a reproducible bloat-measurement test

## Context

The VizJit dump at
`~/ris/debug_vizjit_dir/f26ebdbc5762567c.gocpu.asm.pc_0x000010de.asm`
shows a 9-insn RISC-V block ballooning into 100 IR ops and 1222 host
bytes. Before attempting any optimization, we need a **deterministic
test** that produces these same measurements every run so that
subsequent peephole/allocator work can be verified with clean
before/after numbers.

The binary has now been identified:
- **ELF**: `bench/libriscv_guest/bench_guest.elf`
- **Entry PC in the block**: `0x000010de`
- **Disassembly confirmed** via `tools/disasm_riscv.py`:
  ```
  0x010de: 00054683  lbu x13,0(x10)
  0x010e2: 0505      c.addi x10,1
  0x010e4: 95b6      c.add x11,x13
  0x010e6: fec51ce3  bne x10,x12,0x10de
  0x010ea: fcb42e23  sw x11,-36(x8)
  0x010ee: fdc42003  lw x0,-36(x8)
  0x010f2: 05d00893  addi x17,x0,93
  0x010f6: 4501      c.li x10,0
  0x010f8: 00000073  ecall
  0x010fc: 1141      c.addi x2,-16
  ```

This ELF is already a first-class bench input — see
`bench/cpu_bench_test.go:17-27`, which loads it via `BENCH_ELF` env
var or the fixed path. The test will reuse that convention so it
runs without extra setup.

## Goal

A single Go test file, checked in, that when run:

1. Loads `bench/libriscv_guest/bench_guest.elf`.
2. Compiles the block entered at PC `0x10de` through the full
   root-package AOT path (emit + lower + assemble) so the host-byte
   count it reports matches what VizJit would produce.
3. Logs three numbers for that block: IR op count, emitted host-code
   byte count, and number of chain exits.
4. Asserts each number is **within a budget** (high-water mark) so
   optimization work lowers the budgets over time, and accidental
   regressions fail loudly.
5. Optionally dumps a VizJit-style file for the block to `t.TempDir()`
   so developers can eyeball diffs between runs.

No actual optimization work is done in this step. The test is the
scaffolding.

## Recommended approach

### File location and naming

New file: `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_bloat_test.go`

Package: `riscv` (root). Needs access to internal-to-root helpers
(`NewGuestMemory`, `LoadELFBytes`, `emitBlock`, and an AOT-lower path).
The existing `jit_bench_export.go:EmitBlockForBench` already exposes
`emitBlock` results, but it doesn't run the lowerer. The test will
call the lowerer directly in the same style as `jit_aot.go` does.

### Test skeleton

One test function, `TestBloat_BenchGuest_0x10de`:

```go
func TestBloat_BenchGuest_0x10de(t *testing.T) {
    const (
        elfPath        = "bench/libriscv_guest/bench_guest.elf"
        blockEntryPC   = uint64(0x000010de)

        // Current high-water marks (2026-04-22). Lower these as
        // optimizations land. If any measurement exceeds these,
        // the test fails — catches bloat regressions.
        maxIRInstrs    = 110
        maxHostBytes   = 1300
        maxChainExits  = 5
    )

    data, err := os.ReadFile(elfPath)
    if err != nil {
        t.Fatalf("ReadFile %q: %v (override with BENCH_ELF env if relocated)", elfPath, err)
    }

    mem, err := NewGuestMemory(Size64MB)
    if err != nil { t.Fatal(err) }
    defer mem.Free()

    if _, err := LoadELFBytes(mem, data); err != nil {
        t.Fatalf("LoadELFBytes: %v", err)
    }

    // Emit IR, then lower + assemble to measure host byte count.
    // Mirrors the Pass 1 flow in jit_aot.go:jitCompileAOTSegment.
    emitRes := emitBlock(mem, blockEntryPC)
    if emitRes == nil || emitRes.block == nil {
        t.Fatalf("emitBlock(0x%x) returned nil", blockEntryPC)
    }
    irOps := len(emitRes.block.Instrs)

    alloc, err := ir.AllocateFixedStatic(emitRes.block)
    if err != nil { t.Fatalf("regalloc: %v", err) }

    ctx := goasm.NewCtx()
    // ctx setup — match jit_aot.go's pattern (ATEXT etc.).
    // ... see "Wiring" below ...

    lowerRes, err := ir.LowerAMD64AOT(ctx, emitRes.block, alloc)
    if err != nil { t.Fatalf("LowerAMD64AOT: %v", err) }

    code, err := ctx.Assemble()
    if err != nil { t.Fatalf("Assemble: %v", err) }

    hostBytes := len(code)
    chainExits := len(lowerRes.ChainExits)

    t.Logf("bench_guest 0x%x: ir=%d host=%d chain_exits=%d",
        blockEntryPC, irOps, hostBytes, chainExits)

    if irOps > maxIRInstrs {
        t.Errorf("IR bloat regression: got %d ops, budget %d", irOps, maxIRInstrs)
    }
    if hostBytes > maxHostBytes {
        t.Errorf("host-code bloat regression: got %d bytes, budget %d", hostBytes, maxHostBytes)
    }
    if chainExits > maxChainExits {
        t.Errorf("chain-exit count regression: got %d, budget %d", chainExits, maxChainExits)
    }
}
```

### Wiring to the AOT lower path

`jit_aot.go:jitCompileAOTSegment` is the canonical caller of
`ir.LowerAMD64AOT`. Before calling the lowerer it:
1. Creates a fresh `goasm.Ctx`.
2. Runs `ir.AllocateFixedStatic(block)`.
3. Calls `ir.LowerAMD64AOT(ctx, block, alloc)`.
4. Calls `ctx.Assemble()` → byte slice.

The test mirrors this. The `NewCtx` + `ATEXT` setup in `jit_aot.go`
is a small fixed preamble — lift it into a tiny test helper
`newAOTCtxForBench(fnName string) *goasm.Ctx` (private to the root
package) to keep the test readable. If the same setup already lives
in a test-only helper elsewhere (check
`jit_chaining_test.go` / `jit_chain_foundation_test.go`), reuse
that instead of duplicating.

### Baseline vs. budget

On first run this test should log something like:

```
bench_guest 0x10de: ir=100 host=1222 chain_exits=3
```

The `max*` constants start a touch above the measured values so the
test is green on commit. Each successful optimization PR lowers the
relevant `max*` in the same commit — this couples "the code size
drops" to "the test gets tighter" and prevents silent regressions
when unrelated refactors reshuffle IR.

### Path portability

The test will fail fast if `bench/libriscv_guest/bench_guest.elf` is
absent. Rather than skip (which hides regressions when
`make bench-setup` has been skipped), print a clear "run
`make bench-setup` or set BENCH_ELF" message and `t.Fatal`.
Matching `bench/cpu_bench_test.go:19-21` already uses `BENCH_ELF`
env var as override — we'll mirror that.

### Optional VizJit-style dump side-car

If `testing.Verbose()` is true (i.e. `-v` was passed), also write
a VizJit-format file into `t.TempDir()` so `-v` runs produce a
comparable artifact. Reuse `vizJitDumpAOT` from `jit_vizjit.go` by
temporarily pointing `ir.VIZJIT_DIR` at `t.TempDir()` for the
duration of the test (save/restore). This way running
`go test -v -run TestBloat_BenchGuest_0x10de .` produces a dump
alongside the logged numbers — ideal for diff-inspecting before/
after.

## Files modified

- **New**: `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/jit_bloat_test.go`
- **Possibly touched**: a minimal private helper (`newAOTCtxForBench`)
  added to an existing test-only file or directly inline in
  `jit_bloat_test.go`. No production-code changes.

## Existing functions to reuse

- `NewGuestMemory`, `LoadELFBytes` — from root package.
- `emitBlock` (or `EmitBlockForBench` wrapper at `jit_bench_export.go:26`).
- `ir.AllocateFixedStatic` — `ir/regalloc_fixed.go`.
- `ir.LowerAMD64AOT` — `ir/lower_amd64.go:300`.
- `goasm.(*Ctx).Assemble` — produces native bytes.
- `vizJitDumpAOT` (optional) — `jit_vizjit.go`.
- Env-var convention `BENCH_ELF` — mirrors `bench/cpu_bench_test.go:19`.

## Verification

1. `go test -run TestBloat_BenchGuest_0x10de -v .` — must pass, and
   logged IR/host/chain_exits values must match the VizJit dump's
   header (`# host code: ..., 1222 bytes` → test logs `host=1222`).

2. Temporarily raise the `maxHostBytes` budget by 1, re-run → pass.
   Lower it by 1 → fail with clear message. Confirms the assertion
   arms correctly.

3. `GOCPU_VIZJIT_OFF=1 go test -timeout 120s . ./ir/` — full suite
   green, to confirm the new test doesn't trip any other behaviour.

4. With `-v`, confirm a `.asm` file lands in the test's tempdir and
   its IR/host sections match what the user saw in
   `~/ris/debug_vizjit_dir/`.

## Out of scope for this step

- The two optimizations (post-lowering spill peephole, IR-level 1-byte
  bounds-check cleanup). Those come next, gated by this test.
- Adding more blocks to the bloat test. Once the framework is proven
  on 0x10de we can parameterize it with a list of
  `(elf, pc, budget)` tuples — but not yet.

## Rollback

Delete the one new test file.
