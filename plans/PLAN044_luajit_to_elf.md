# Plan: `build-luajit-riscv` Makefile target

## Context

We want to run LuaJIT *inside* our RISC-V emulator to exercise guest-side
dynamic code generation (mmap / mprotect / trace compilation). The vendored
source at `xendor/LuaJIT` is the **plctlab LJRV fork** (`riscv64-v2.1-branch`)
which adds RISC-V support to upstream LuaJIT v2.1 — verified present:

- `src/vm_riscv64.dasc`, `src/lj_asm_riscv64.h`, `src/lj_emit_riscv.h`,
  `src/lj_target_riscv.h`, `dynasm/dasm_riscv64.lua`
- `src/lj_arch.h:34-35` defines `LUAJIT_ARCH_RISCV64`; `:70-71` auto-detects
  via `__riscv && __riscv_xlen == 64`; `:482-487` wires up `LJ_TARGET_RISCV64`
- `src/Makefile:271-272` maps `LJ_TARGET_RISCV64` → `TARGET_LJARCH=riscv64`
- `src/Makefile:490-492` passes `-D RISCV64` to DynASM

Upstream RISC-V support is in progress but not yet merged; LJRV is tagged
beta-quality by upstream (`README.md:28`). Good enough for our sandbox test.

The top-level `/Users/jaten/ris/Makefile` already uses `zig cc` for RISC-V
cross-compilation (see `coremark-elf` at line 327 and `dhrystone-elf` at
line 345). Zig 0.16.0-dev is installed. All that's missing is a target that
drives LuaJIT's own build with the right variable overrides.

Output goes to `bench/luajit.elf`, matching the existing `coremark.elf` /
`dhrystone.elf` naming convention (both live directly in `bench/`).

## Approach

Add one new target, `build-luajit-riscv`, to `/Users/jaten/ris/Makefile`.
It drives LuaJIT's own `src/Makefile` with variable overrides that redirect
the target toolchain to `zig cc -target riscv64-linux-musl` while keeping
host tools (`minilua`, `buildvm`) on the native compiler.

### Target ABI: `riscv64-linux-musl`, statically linked

LuaJIT is not freestanding: it needs `mmap`, `mprotect`, `malloc`, `dlopen`,
etc. That rules out `riscv64-freestanding` (which coremark and dhrystone use).
Choice of musl over glibc:

- zig bundles musl headers and objects for every target — zero setup
- musl static linking produces a single self-contained ELF (true "standalone")
- Existing Makefile comment at line 59-60 already documents this choice:
  *"libriscv's Linux personality handles the musl syscall ABI identically
  to glibc for our purposes"*

### LuaJIT Makefile overrides

LuaJIT's `src/Makefile` builds cross-compile-friendly via these variables
(documented at `src/Makefile:180-189`):

| Override             | Value                                               | Why                                                                |
| -------------------- | --------------------------------------------------- | ------------------------------------------------------------------ |
| `HOST_CC`            | `cc`                                                | Build `minilua` + `buildvm` for the Mac/Linux host                 |
| `CROSS`              | `""` (empty)                                        | LuaJIT prepends `$(CROSS)` to `CC`/`ar`/`strip`; we bypass via zig |
| `STATIC_CC`          | `$(ZIG_CC) cc -target riscv64-linux-musl`           | Cross-compile target `.o` files                                    |
| `DYNAMIC_CC`         | `$(ZIG_CC) cc -target riscv64-linux-musl -fPIC`     | Unused for static build; set for safety                            |
| `TARGET_LD`          | `$(ZIG_CC) cc -target riscv64-linux-musl -static`   | Final link, static musl                                            |
| `TARGET_AR`          | `$(ZIG_CC) ar rcus`                                 | zig bundles LLVM ar                                                |
| `TARGET_STRIP`       | `:`                                                 | Host `strip` can't handle RISC-V ELFs; skip                        |
| `TARGET_SYS`         | `Linux`                                             | Enables `-ldl`, Linux codepaths in Makefile                        |
| `BUILDMODE`          | `static`                                            | Produce `luajit` + `libluajit.a` only, no `.so`                    |

`lj_arch.h` preprocessing (`src/Makefile:237`) works because
`zig cc -target riscv64-linux-musl` predefines `__riscv=1` and
`__riscv_xlen=64`, which is all `lj_arch.h:70-71` needs to trigger
`LJ_TARGET_RISCV64`.

Version-string generation at `src/Makefile:499-505` works even without a
`.git` directory (it's been renamed `dot.git` in the vendor): the fallback
reads `../.relver`, which **exists** (verified).

### New Makefile lines (sketched)

Add near the CoreMark/Dhrystone section (around line 358 in the current
Makefile), using the same `──` banner style:

```make
# ── LuaJIT riscv64 ELF ────────────────────────────────────────────────────
# Cross-compile the LJRV fork (plctlab LuaJIT RISC-V port) into a standalone
# riscv64-linux-musl binary. Host tools (minilua, buildvm) build with cc;
# all target objects and the final link go through `zig cc -target
# riscv64-linux-musl -static`. Output: bench/luajit.elf.

LUAJIT_SRC := $(ROOT)xendor/LuaJIT
LUAJIT_ELF := $(ROOT)bench/luajit.elf

build-luajit-riscv: $(LUAJIT_ELF)
$(LUAJIT_ELF):
	@echo "── cross-compiling LuaJIT → riscv64-linux-musl ─────────────────"
	$(MAKE) -C $(LUAJIT_SRC)/src clean
	$(MAKE) -C $(LUAJIT_SRC)/src \
	    HOST_CC="cc" \
	    CROSS="" \
	    STATIC_CC="$(ZIG_CC) cc -target riscv64-linux-musl" \
	    DYNAMIC_CC="$(ZIG_CC) cc -target riscv64-linux-musl -fPIC" \
	    TARGET_LD="$(ZIG_CC) cc -target riscv64-linux-musl -static" \
	    TARGET_AR="$(ZIG_CC) ar rcus" \
	    TARGET_STRIP=":" \
	    TARGET_SYS=Linux \
	    BUILDMODE=static
	cp $(LUAJIT_SRC)/src/luajit $(LUAJIT_ELF)
	@test -f $(LUAJIT_ELF) || { echo "  ✗ luajit ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(LUAJIT_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(LUAJIT_ELF) | cut -f1)"
```

Also:

1. Add `build-luajit-riscv` to the `.PHONY` list at `Makefile:25-31`.
2. Add a help line to the `help:` target (around `Makefile:181`):
   `@echo "    make build-luajit-riscv  cross-compile LuaJIT → bench/luajit.elf"`
3. Extend `clean:` at `Makefile:625-628` to also `rm -f $(LUAJIT_ELF)` and
   optionally `$(MAKE) -C $(LUAJIT_SRC)/src clean` (gated on the directory
   existing).

## Files to modify

- `/Users/jaten/ris/Makefile` — only file touched.

No changes needed to the vendored LuaJIT — its `src/Makefile` already has
the full RISC-V path.

## Verification

```bash
# 1. Build
make build-luajit-riscv

# 2. Confirm ELF metadata
file bench/luajit.elf
# Expected: "ELF 64-bit LSB executable, UCB RISC-V, RVC, double-float ABI,
#            version 1 (SYSV), statically linked, ..."

du -h bench/luajit.elf
# Expected: ~600 KB – 1.2 MB

# 3. Quick header sanity check
zig objdump -f bench/luajit.elf | head
# Expected: architecture: riscv:rv64

# 4. Run under the project's RISC-V runner (once OS personality supports it):
#    Exact invocation depends on which cmd-line entry point the user wires
#    up; could look something like:
#       go run ./cmd/... bench/luajit.elf -e 'print("hi from RISC-V")'
#    Fallback for now: confirm the binary loads via the existing ELF loader
#    in a smoke test.
```

## Open items (to confirm via plan approval)

- **Filename**: proposed `bench/luajit.elf`; alternatives would be
  `bench/luajit` (bare, matches upstream), `bench/luajit-riscv.elf`, or a
  `bench/luajit_guest/` subdirectory mirroring `bench/libriscv_guest/`.
- **`amalg` vs default target**: plan uses the default (per-file) for
  simplicity; switching to `amalg` would slightly shrink the binary and
  speed up the build but is not required.
