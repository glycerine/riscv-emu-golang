# Dump libriscv's TCC-compiled assembly alongside GoCPU's VizJit output

## Context

GoCPU produces VizJit dumps into `~/ris/debug_vizjit_dir/` that, per
compiled block, show guest RISC-V disassembly, internal IR, and the
emitted x86 goasm Progs. These have been essential for diagnosing
bloat (e.g. the 9-insn block at PC 0x10de ballooning to 105 IR ops
and 1079 host bytes).

We have no equivalent visibility into **libriscv**, the C++ reference
emulator used as our speed/oracle comparison. Its pipeline is
**RISC-V → hand-emitted C (one `f_<basepc>` function per block) →
TCC compiles in-memory → function pointers installed in the decoder
cache**. Today there is no dump facility — one disabled stub exists
at `xendor/libriscv/lib/libriscv/tr_translate.cpp:917-926` that was
clearly for this purpose and got commented out behind
`if constexpr (false)`.

Goal: for each block libriscv translates, write a sibling file to
`~/ris/debug_libriscv_dir/` named
`{tag}.libriscv.asm.pc_0x{basepc:08x}.asm` containing sections that
line up with GoCPU's VizJit dump, so a plain `diff` between the two
per-block files tells us where the pipelines diverge.

## Design overview

### Filename & directory

- **Dir**: `~/ris/debug_libriscv_dir/` (gitignored, parallels
  `debug_vizjit_dir/`).
- **Filename**: `{tag}.libriscv.asm.pc_0x{basepc:08x}.asm`.
  - `{tag}` mirrors GoCPU's 16-hex run tag when available. If the Go
    driver exports its tag via an env var (see "Go wiring" below),
    libriscv uses it verbatim; otherwise it falls back to the
    32-bit `translation_hash()` as 8 hex chars.
- **Index**: `{tag}.libriscv.asm.index.txt`, mirroring GoCPU's.

### File structure (mirrors GoCPU's VizJit)

```
# libriscv bintr dump
# run tag:    <tag>
# entry PC:   0x<basepc:08x>
# byte range: 0x<basepc:08x>..0x<endpc:08x> (<n> bytes)
# host code:  <fp>, <len> bytes

== Guest RISC-V ==
0x000010de  00054683  lbu x13,0(x10)
...

== libriscv bintr C ==
<extracted body of `static CALLBACK ReturnValues f_<basepc_hex>(...)`>

== Host x86-64 (from TCC) ==
<disassembly of the function's native bytes>
```

Three sections, same order as GoCPU (`Guest RISC-V` → intermediate →
`Host`). libriscv has no IR; its C source sits in the slot GoCPU's
`== IR ==` occupies. That's the unavoidable asymmetry — the rest
lines up.

### Phase split

The work falls into two phases that can land in one PR but are
usefully separable for review:

- **Phase 1** — header + Guest RISC-V + libriscv C. This is pure
  string manipulation, no TCC introspection, ~90% of the diagnostic
  value. Safe to land alone.
- **Phase 2** — Host x86-64 disassembly. Requires post-`tcc_relocate`
  symbol-pointer arithmetic plus a disassembler invocation
  (`llvm-objdump` via `popen`).

## Recommended approach

### Hook points in libriscv (C++)

Both sit in `xendor/libriscv/lib/libriscv/tr_translate.cpp`:

1. **Pre-compile hook** (Phase 1): just before
   `libtcc_compile(shared_library_code, ...)` at line **927**.
   - In scope: `shared_library_code` (full C), `output.mappings`
     (vector of `TransMapping<W>` with `.addr` and `.symbol`),
     `machine()` and `exec` (for guest disasm), `translation_hash()`.
   - Action: write one file per mapping with the header, Guest
     RISC-V, and libriscv C sections; `host code: pending` line is a
     placeholder that Phase 2 will fill in.

2. **Post-activate hook** (Phase 2): inside
   `CPU<W>::activate_dylib()`, after `dylib_lookup` has returned
   the `handlers[]` (function-pointer array) and `mappings[]`
   (addr→index pairs) — around **tr_translate.cpp:1068**, right
   after `std::copy(handlers, ...)` puts the pointers into the exec
   mapping table.
   - In scope: `handlers` (the `bintr_block_func<W>*` array),
     `unique_mappings` (count), `mappings` (the addr→index array),
     `nmappings`.
   - Action: build a sorted-by-address list of
     `(basepc, func_ptr)`; for each, `end = next_fp` (or, for the
     last, the address of a known trailing symbol like
     `mappings` or `unique_mappings`, also fetched via
     `dylib_lookup`). Disassemble the byte range. Append
     `== Host x86-64 (from TCC) ==` to the matching per-block file
     and patch the `# host code:` line in the header.

### New files

- `xendor/libriscv/lib/libriscv/tr_dump.cpp` (~250 lines, all the
  dump logic — keeps `tr_translate.cpp` clean).
- `xendor/libriscv/lib/libriscv/tr_dump.hpp` — two template
  function declarations:

  ```cpp
  template <int W>
  void dump_bintr_c_source(
      const std::string& shared_library_code,
      const std::vector<TransMapping<W>>& mappings,
      Memory<W>& mem,
      uint32_t translation_hash);

  template <int W>
  void dump_bintr_host_asm(
      const std::vector<TransMapping<W>>& mappings,
      const bintr_block_func<W>* handlers,
      unsigned unique_count,
      uint32_t translation_hash);
  ```

Templates match tr_translate.cpp's template-per-width idiom (the
existing file instantiates for `W=4`, `W=8`, `W=16`).

### Extraction of the per-block C body

Walk `shared_library_code` once, finding block-function definitions.
The emitter prints each as `static ... f_<basepc_hex>(...)` followed
by a balanced `{ ... }` body (see `tr_emit.cpp:107` and the
`FUNCLABEL` macro at :84). The dumper:

1. Scans for the literal `f_<hex>` tokens matching the mapping
   addresses.
2. From each hit, walks left to the preceding `static` /
   `CALLBACK` keyword to get the full signature.
3. Walks right tracking `{` / `}` depth until the matching closing
   brace, that is the function body.

One-shot parse (build a map `addr → (start, end)` in the C buffer),
then slice.

### Disassembling the x86-64 bytes

libriscv does not link Capstone (verified: zero Capstone refs in
`xendor/libriscv/`). TCC's public `libtcc.h` has no "get text" API
(verified — only `tcc_get_symbol` is public). So Phase 2 uses:

1. `tcc_get_symbol(state, "f_<hex>")` for each mapping to get entry
   pointers. (Already done inside `activate_dylib`; reuse those.)
2. Sort function pointers; each function's length is
   `next_fp - this_fp`. For the last function, look up a known
   trailing symbol (e.g. `mappings` or `unique_mappings`) as a
   conservative upper bound.
3. Write the raw bytes to a temp file and invoke
   `popen("llvm-objdump -b binary -m i386 -M x86-64,intel -D <file>",
   "r")` to get assembly text. `llvm-objdump` is standard on macOS
   (shipped with the toolchain) and widely available on Linux. If
   `popen` fails or the tool isn't present, fall back to a 16-bytes-
   per-line hex dump so the file is still useful.
4. Clean up the temp file afterwards.

Risks of symbol-pointer arithmetic:
- TCC can in principle scatter functions, interleave constants, or
  reorder them. Empirically, TCC-compiled C emitted by libriscv
  lays out functions sequentially in the same order they appear in
  source. We'll validate against the 0x10de block first; if the
  byte ranges look wrong we'll detect it by seeing non-RET endings
  or mid-function gibberish.
- Fallback if it breaks: upstream a 10-line patch to TCC exposing
  `tcc_get_text_section(state, &ptr, &size)`. That source sits in
  `xendor/libriscv/build_capi/_deps/tinycc-src/`. Out of scope for
  this plan.

### Guest RISC-V section

libriscv already decodes RVI/RVC/RVF/etc. in `rvi_instr.cpp`,
`rvc_instr.cpp`, etc. but there is no public "format-one-line"
helper. Two options:

- **Preferred**: use `Machine<W>::cpu.to_string(...)` if it exists
  (libriscv has a disasm path used by `VERBOSE` mode — needs a
  5-minute spot-check during implementation to find the exact
  entry point).
- **Fallback**: ship a minimal RVI+RVC decoder inside `tr_dump.cpp`,
  just enough for the common instruction subset. Good enough to
  eyeball blocks and compare with GoCPU's output.

If libriscv's disasm output differs textually from GoCPU's, the
`Guest RISC-V` sections of the two dumps won't `diff`-match
byte-for-byte but will be semantically equivalent. Acceptable.

### CMake integration

Add `tr_dump.cpp` to `xendor/libriscv/lib/CMakeLists.txt`
(confirmed by Glob that `lib/libriscv/CMakeLists.txt` does NOT
exist — the list lives one level up). Grep for `tr_emit.cpp` there;
append `tr_dump.cpp` alongside.

### Env-var control

libriscv's dumper reads:

- `LIBRISCV_DUMP_DIR` — output directory. If unset/empty, the
  dumper is an early `return;` no-op. Default off.
- `LIBRISCV_DUMP_TAG` — optional 16-hex tag to group files from
  the same run. Falls back to `translation_hash` as 8 hex.

No build-time flag: the dumper compiles in unconditionally but
costs zero when unset.

### Go-side wiring

In `bench/hellobench/main.go`, extend the `-only=<mode>` paths that
run libriscv:

1. If `GOCPU_VIZJIT` is set (i.e. user wants dumps), also
   `os.Setenv("LIBRISCV_DUMP_DIR", parentDir + "/debug_libriscv_dir")`
   — so a single env var flip enables both sides.
2. If GoCPU has a tag, export `LIBRISCV_DUMP_TAG=<same tag>` so the
   two sets of files live under matching keys.

Look for the existing `getVizJitTag()` / `ir.VIZJIT_DIR` pattern in
`jit_vizjit.go` to source these values from Go. The tag lives in a
package-local once-initialised variable (see
`jit_vizjit.go:30`-ish from the earlier exploration).

## Files modified

- **New**: `xendor/libriscv/lib/libriscv/tr_dump.cpp`
- **New**: `xendor/libriscv/lib/libriscv/tr_dump.hpp`
- **Edit**: `xendor/libriscv/lib/libriscv/tr_translate.cpp` (two
  hook calls, ~10 lines each)
- **Edit**: `xendor/libriscv/lib/CMakeLists.txt` (add source file)
- **Edit**: `bench/hellobench/main.go` (propagate env vars in
  libriscv modes; ~15 lines)
- **Edit**: `.gitignore` (add `debug_libriscv_dir/` alongside the
  existing `debug_vizjit_dir/` at line 17)

## Existing pieces to reuse

- **Hook site**: `tr_translate.cpp:910-927` — disabled `if
  constexpr (false)` stub shows previous author's intent; we
  replace with a live getenv-gated call.
- **Activation site**: `tr_translate.cpp:1006-1068` — already has
  all the symbol data we need for Phase 2.
- **Function label macro**: `tr_emit.cpp:77-84` (`funclabel<W>` +
  `FUNCLABEL`) — block entries are `f_<basepc_hex>` where hex is
  lowercase without leading zeros.
- **Emitter state**: `tr_emit.cpp:107` — each block's function
  name set from its base PC at emission.
- **Mapping struct**: `tr_translate.cpp:696-701` — what `mappings[]`
  in the generated C holds, and what `dlmappings` (a
  `std::vector<TransMapping<W>>`) exposes to us.
- **Translation hash**: `exec.translation_hash()` gives a 32-bit
  CRC32C per translation — natural tag fallback.
- **GoCPU tag source**: `jit_vizjit.go:30` (`getVizJitTag()`).
- **GoCPU dump dir source**: `ir.VIZJIT_DIR` — exported, readable
  from `bench/hellobench/main.go`.

## Verification

1. **Build libriscv after edits**: there's a CMake build under
   `xendor/libriscv/build_capi/`. `make bench-setup` may or may
   not re-invoke CMake; if not, rebuild manually:
   `cd xendor/libriscv/build_capi && cmake --build .`. Confirm
   `libriscv_capi.a` / `libriscv.a` link the new `tr_dump.o`.

2. **No-op when env is unset**:
   `go test -run TestJIT_BenchGuest_Smoke ./bench/` should pass with
   no new files under `~/ris/debug_libriscv_dir/` (dir empty or
   non-existent).

3. **Phase 1 check**:
   ```
   GOCPU_VIZJIT=~/ris/debug_vizjit_dir \
   LIBRISCV_DUMP_DIR=~/ris/debug_libriscv_dir \
     go run ./bench/hellobench -only=libriscv
   ```
   Then `ls ~/ris/debug_libriscv_dir/*.libriscv.asm.pc_0x000010de.asm`
   should exist. Its first two sections (`Guest RISC-V`, `libriscv
   bintr C`) should populate; the `Host x86-64` section should be
   either populated (if Phase 2 is in) or marked `pending`.

4. **Phase 2 check**: the `== Host x86-64 ==` section must end on a
   `ret` instruction (or `ret`-equivalent) and the byte count must
   be plausible (hundreds of bytes, not 10 and not 10000 for a
   10-insn guest block). Spot-check one function: does `llvm-objdump
   -d` on the whole binary produce the same bytes for a symbol of
   the same name?

5. **Diffability**:
   `diff -u ~/ris/debug_vizjit_dir/{tag}.gocpu.asm.pc_0x000010de.asm
   ~/ris/debug_libriscv_dir/{tag}.libriscv.asm.pc_0x000010de.asm`
   should produce a clean-looking diff — header lines aligned,
   `== Guest RISC-V ==` roughly matching, then the intermediate and
   host sections differing as expected.

## Risks and open questions

- **Is `xendor/libriscv/` tracked by git?** Confirmed yes —
  `git ls-files xendor/libriscv/lib/libriscv/tr_translate.cpp`
  returns the path and `.gitmodules` does not exist. Our edits
  will live in this repo's history like normal code.
- **Does `make bench-setup` re-clone or re-build?** Not yet
  confirmed — if it re-clones, we'd need to make it either a
  no-op-on-exists or re-run CMake without wiping sources. Worth
  a 2-minute read of the Makefile before implementation.
- **TCC symbol-pointer layout**: described above in "Disassembling
  the x86-64 bytes". Phase 2 depends on this; if it turns out
  unreliable, fall back to either (a) patching TCC's header to
  expose text-section base+size, or (b) skip Phase 2 and
  ship Phase 1 only.
- **Darwin `llvm-objdump`**: present by default on modern macOS dev
  installs but not universal. Hex-dump fallback covers everyone.

## Out of scope

- Diff/compare tooling (we'll use `diff -u` / `diffoscope` by hand).
- Live mid-run re-dumps (translations happen once, at install).
- A dedicated comparison test case in Go (useful follow-up once
  the files exist, but not required for this plan).
- Upstreaming a TCC patch for text-section extraction (only needed
  if our symbol-math approach proves unreliable).

## Rollback

Revert the five modified files and delete the two new ones; no
runtime effect when env vars are unset so rollback is low-risk.
