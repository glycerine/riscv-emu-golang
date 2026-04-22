# Plan: Remove TCC Usage from the Go CPU

## Context

The Go RISC-V emulator at `/Users/jaten/ris/` has historically had two JIT backends:

1. **TCC-based**: RISC-V → C source (struct `tccEmitter`) → compiled via TinyCC's libtcc (CGO) → executed.
2. **Native amd64**: RISC-V → `ir.Block` (struct `emitter` in `jit_emit_ir.go`) → lowered via `ir.LowerAMD64{,_V2}` → register-allocated → assembled via `goasm` → executed.

The native amd64 path is now the production backend and has superseded the TCC path (recent benchmark: `BenchmarkCPU_FullExecution_JIT_Fixed` at 3273 MIPS). The two paths coexist as separate, parallel APIs — there is no runtime `UseTCC` flag; callers invoke `RunJIT/StepBlock` (native) vs `TccRunJIT/TccStepBlock` (TCC).

Goal: delete the TCC-based JIT from the Go CPU. Eliminates ~1800 lines of C-source generation, removes the CGO dependency on `libtcc`, and collapses the codebase to a single JIT backend.

**Constraint — off-limits:** `/Users/jaten/ris/xendor/` must NOT be touched. `xendor/libriscv/` (the reference emulator used for oracle fuzzing and benchmarks) continues to use TCC internally. `xendor/tcc/libtcc.a` and `xendor/tcc/libtcc.h` remain in place as historical artifacts.

## Files to Delete Entirely

| File | Contents |
|------|----------|
| `/Users/jaten/ris/jit_emit.go` | 1579 lines. `tccEmitter` struct + 39 methods (rd, rs, emit, emit32, emitOpImm, emitOp{,32}, emitLoad, emitStore, emitFPLoad, emitFPStore, emitFMA, emitFPOp{,S,D}, emitFsgnj{S,D}, emitFcmp{S,D}, emitFcvt{ToInt,FromInt}{S,D}, emitJAL, emitJALR, emitBranch, emitRVC, emitRVC_Q{0,1,2}, rsC, finalize, …), `tccEmitResult`, `tccEmitBlock`, and the C-source helpers `loadInfo`, `storeInfo`, `branchCmp` (confirmed: only used within this file). |
| `/Users/jaten/ris/jit_tcc.go` | CGO bridge to libtcc. `tccCompile()`, `(j *JIT) tccJitCompileWith()`, and the `#cgo CFLAGS: -I${SRCDIR}/xendor/tcc` / `#cgo LDFLAGS: -L${SRCDIR}/xendor/tcc -ltcc` directives. Calls `tcc_new`, `tcc_set_output_type`, `tcc_compile_string`, `tcc_add_symbol` (registers `jit_sqrtf`/`jit_sqrt`/`jit_trace`), `tcc_relocate`, `tcc_get_symbol`, `tcc_delete`. |
| `/Users/jaten/ris/jit_tcc_dispatch.go` | `(j *JIT) TccStepBlock()` and `(j *JIT) TccRunJIT()` — the TCC dispatch entry points. |

## Files to Modify

### `/Users/jaten/ris/jit.go`
- Remove `tccState unsafe.Pointer` field from the `compiledBlock` struct (line 138). The field is assigned only in the deleted `jit_tcc.go:90`; no other readers.
- Fix the comment on line 132 (`// compiledBlock holds a compiled function pointer (native IR or TCC).`) to drop the TCC mention.
- Review lines 149–150 which reference `tccState` cleanup semantics — adjust or delete.

### `/Users/jaten/ris/bench/cpu_bench_test.go`
- Delete `runTccJITBenchGuestWith()` helper (lines 83–97). Only caller of `jit.TccRunJIT`.

### `/Users/jaten/ris/bench/jit_bench_test.go`
- Delete `BenchmarkCPU_FullExecution_JIT_TCC` (around line 54-72) — the sole caller of `runTccJITBenchGuestWith`. The companion `BenchmarkCPU_FullExecution_JIT_Fixed` covers the native-backend measurement.

### `/Users/jaten/ris/jit_decode.go`
- Update header comment (lines 2–3: `// Shared decode-only functions used by both the TCC and IR JIT emission paths.`) to drop the "TCC and" fragment. Code in this file is unchanged; `scanRegion` survives (used by the native IR emitter).

### `/Users/jaten/ris/jit_native.go`
- Update header comment (lines 3–4: `// Replaces TCC: emitBlock produces ir.Block, which is lowered and assembled`) to remove the "Replaces TCC:" framing.

### `/Users/jaten/ris/riscv_test.go`
- Line 329: comment mentioning `tcc_add_symbol` in a test skip reason — review and clean up if stale.

### `/Users/jaten/ris/Makefile`
- **Line 167**: `make bench-alloc` help text — drop "TCC" (currently "Fixed vs TCC vs libriscv" → "Fixed vs libriscv").
- **Line 199**: `bench-setup` target — remove the `libtcc-build` prerequisite so first-time setup no longer tries to build libtcc for our Go code.
- **Lines 617–623** (inside the `bench-alloc` recipe): delete the `"Go JIT — TCC backend:"` printf stanza and its benchmark invocation of `BenchmarkCPU_FullExecution_JIT_TCC`. Keep the separate `"libriscv — JIT (TCC):"` stanza beneath it — that measures libriscv, not our code.
- **Lines 267–278**: commented-out block about patching `TCC_TARGET_ARM64`/`TCC_TARGET_X86_64` — delete if TCC-specific.
- **Lines 877–915**: delete the `libtcc-build` target, the `$(TCC_ARCHIVE)` recipe, and the variables `TCC_SRC_DIR`, `TCC_ARCHIVE`, `TCC_HDR_DST`, `TCC_TARGET_DEFS`, `TCC_CFLAGS`.
- Keep: nothing in the Makefile should still reference libtcc/libtcc.a after this. `xendor/tcc/libtcc.a` stays on disk untouched but becomes an orphaned artifact.

## Not Touched (per constraint)

- `/Users/jaten/ris/xendor/tcc/` — `libtcc.a`, `libtcc.h` remain in place.
- `/Users/jaten/ris/xendor/libriscv/` — entire tree, including any TCC usage internal to libriscv's build.

## Verification

Run from `/Users/jaten/ris`:

1. **Compile cleanly:** `go build ./ ./ir/ ./internal/... ./bench/` — must succeed without CGO errors about missing libtcc (the cgo LDFLAGS for libtcc are only in the now-deleted `jit_tcc.go`).
2. **Non-bench tests pass:** `go test ./ ./ir/ ./internal/...`
3. **Bench package vets and compiles:** `go vet ./bench/`
4. **Native JIT benchmark still runs:**
   `go test -run='^$' -bench='^BenchmarkCPU_FullExecution_JIT_Fixed$' -benchtime=1x -count=1 ./bench/` — expect ~3000+ MIPS.
5. **ELF test suite passes:** `go test -run='^TestRiscvTests' .` (if relevant).
6. **Residual-reference sweep (zero .go hits outside `xendor/`):**
   - `grep -rn "tccEmitter\|tccEmitBlock\|tccEmitResult\|tccCompile\|tccJitCompileWith\|TccRunJIT\|TccStepBlock\|tccState" .` (exclude `xendor/`)
   - `grep -rn "libtcc\|-ltcc" .` (exclude `xendor/`)
7. **Makefile sanity:**
   - `make -n bench-setup` — must not reference `libtcc-build`.
   - `make -n bench-alloc` — must not mention `BenchmarkCPU_FullExecution_JIT_TCC` or the "Go JIT — TCC backend" label.
8. **Optional smoke:** run a JIT-driven workload (e.g. one of the existing `TestJIT_*` or `TestRunJIT_*` tests) to confirm the native path still executes guest code end-to-end.

## Notes

- **CGO still needed** after this change: `/Users/jaten/ris/guestmem.go` and other files use CGO for guest memory allocation and mmap. Only the libtcc-specific cgo directives go away.
- **`scanRegion`** (in `jit_decode.go`) is shared by both emitters; it stays. Likewise the TCC-registered helpers `jit_sqrtf`/`jit_sqrt`/`jit_trace` disappear from the runtime when `jit_tcc.go` goes, but the native path doesn't need them (amd64 SSE `sqrtss`/`sqrtsd` are used directly in lowering).
- **Dead disk files:** `xendor/tcc/libtcc.a` becomes an orphan (no Go code links against it after this change). Leaving it in place respects the "nothing under xendor" constraint; it can be cleaned up later if the user decides the xendor/tcc/ directory itself should be removed.
