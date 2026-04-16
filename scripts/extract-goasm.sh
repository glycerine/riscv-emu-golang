#!/usr/bin/env bash
# extract-goasm.sh — Copy Go's cmd/internal/obj encoders into goasm/
# and rewrite imports so they build as part of the riscv module.
#
# Run once to create goasm/; re-run after a Go upgrade, then re-apply
# every edit listed in goasm/EDITS.md.
#
# Usage: bash scripts/extract-goasm.sh

set -euo pipefail

GOROOT=${GOROOT:-$(go env GOROOT)}
DEST=goasm
SRC=$GOROOT/src/cmd/internal
ISRC=$GOROOT/src/internal   # for internal/abi and internal/goarch

echo "Extracting from GOROOT=$GOROOT"

# Remove previously extracted directories so stale files don't linger.
rm -rf "$DEST/obj" "$DEST/objabi" "$DEST/src" "$DEST/sys" "$DEST/hash" \
       "$DEST/abi" "$DEST/goarch" "$DEST/buildcfg" "$DEST/goexperiment"
# Note: obj/ subdirs (x86, arm64, arm, loong64, mips, ppc64, riscv, s390x, wasm)
# are removed as part of rm -rf "$DEST/obj" above.
mkdir -p "$DEST"

# copy_pkg <absolute-srcdir> <dest-subpath> <skip-list...>
# Copies all *.go files from absolute-srcdir to DEST/dest-subpath,
# skipping any filename listed in skip-list.
copy_pkg() {
    local srcpkg="$1"
    local destpkg="$2"
    shift 2
    local skip=("$@")
    local srcdir="$srcpkg"   # already absolute
    local destdir="$DEST/$destpkg"
    mkdir -p "$destdir"
    for f in "$srcdir"/*.go; do
        [ -f "$f" ] || continue
        local base
        base=$(basename "$f")
        local s
        local do_skip=0
        for s in "${skip[@]:-}"; do
            [ "$base" = "$s" ] && do_skip=1 && break
        done
        [ $do_skip -eq 1 ] && continue
        cp "$f" "$destdir/$base"
    done
}

# obj core (explicit file list — skip dwarf.go fips140.go mkcnames.go objfile.go pcln.go and tests)
mkdir -p "$DEST/obj"
for f in \
    abi_string.go addrtype_string.go data.go go.go inl.go ld.go \
    line.go link.go pass.go plist.go stringer.go sym.go textflag.go util.go
do
    cp "$SRC/obj/$f" "$DEST/obj/$f"
done

# obj/x86 — all .go except tests
copy_pkg "$SRC/obj/x86" "obj/x86" \
    asm_test.go obj6_test.go pcrelative_test.go

# obj/arm64 — all .go except tests.
# asm_arm64_test.go is excluded because its companion asm_arm64_test.s
# (forward-declared bodies for testvmovs/testmovk/testCombined) is not
# extracted and the test would fail to build on GOARCH=arm64.
copy_pkg "$SRC/obj/arm64" "obj/arm64" \
    asm_test.go \
    asm_arm64_test.go

# obj/arm — all .go except tests
copy_pkg "$SRC/obj/arm" "obj/arm"

# obj/loong64 — all .go except tests
copy_pkg "$SRC/obj/loong64" "obj/loong64" \
    asm_test.go

# obj/mips — all .go except tests
copy_pkg "$SRC/obj/mips" "obj/mips"

# obj/ppc64 — all .go except tests
copy_pkg "$SRC/obj/ppc64" "obj/ppc64" \
    asm_test.go

# obj/riscv — all .go except tests
copy_pkg "$SRC/obj/riscv" "obj/riscv" \
    asm_test.go obj_test.go

# obj/s390x — all .go except tests
copy_pkg "$SRC/obj/s390x" "obj/s390x" \
    rotate_test.go

# obj/wasm — all .go except tests
copy_pkg "$SRC/obj/wasm" "obj/wasm"

# cmd/internal packages — skip test files that import internal/testenv
copy_pkg "$SRC/objabi" "objabi" flag_test.go line_test.go path_test.go
copy_pkg "$SRC/src"    "src"
copy_pkg "$SRC/sys"    "sys"
copy_pkg "$SRC/hash"   "hash"

# internal/goarch — architecture constants; no external deps; copy all .go
# except the code generator (gengoarch.go uses main package)
copy_pkg "$ISRC/goarch" "goarch" gengoarch.go

# internal/abi — ABI type definitions (FuncID, FuncFlag, etc.); only
# depends on internal/goarch; copy all non-test .go.
# Skip funcpc.go: FuncPCABI0/FuncPCABIInternal are compiler intrinsics
# that only work inside the stdlib.
# funcpc_gccgo.go provides a fallback with bodies; it's kept.
copy_pkg "$ISRC/abi" "abi" \
    abi_test.go export_test.go funcpc_test.go map_test.go type_test.go \
    funcpc.go

# internal/buildcfg — GOOS/GOARCH/version constants used by encoders.
copy_pkg "$ISRC/buildcfg" "buildcfg"

# internal/goexperiment — struct of bool experiment flags; no internal deps.
# Needed by buildcfg/exp.go. Skip mkconsts.go (it's a generator, package main).
copy_pkg "$ISRC/goexperiment" "goexperiment" mkconsts.go

echo "Copied files:"
find "$DEST" -name '*.go' | wc -l

# Rewrite all cmd/internal/* imports to riscv/goasm/*.
# Order matters: rewrite longer paths first (obj/x86 before obj).
find "$DEST" -name '*.go' | while read -r f; do
    sed -i.bak -E '
        s|"cmd/internal/obj/x86"|"riscv/goasm/obj/x86"|g
        s|"cmd/internal/obj/arm64"|"riscv/goasm/obj/arm64"|g
        s|"cmd/internal/obj/arm"|"riscv/goasm/obj/arm"|g
        s|"cmd/internal/obj/loong64"|"riscv/goasm/obj/loong64"|g
        s|"cmd/internal/obj/mips"|"riscv/goasm/obj/mips"|g
        s|"cmd/internal/obj/ppc64"|"riscv/goasm/obj/ppc64"|g
        s|"cmd/internal/obj/riscv"|"riscv/goasm/obj/riscv"|g
        s|"cmd/internal/obj/s390x"|"riscv/goasm/obj/s390x"|g
        s|"cmd/internal/obj/wasm"|"riscv/goasm/obj/wasm"|g
        s|"cmd/internal/obj"|"riscv/goasm/obj"|g
        s|"cmd/internal/objabi"|"riscv/goasm/objabi"|g
        s|"cmd/internal/src"|"riscv/goasm/src"|g
        s|"cmd/internal/sys"|"riscv/goasm/sys"|g
        s|"cmd/internal/hash"|"riscv/goasm/hash"|g
        s|"internal/goarch"|"riscv/goasm/goarch"|g
        s|"internal/abi"|"riscv/goasm/abi"|g
        s|"internal/buildcfg"|"riscv/goasm/buildcfg"|g
        s|"internal/goexperiment"|"riscv/goasm/goexperiment"|g
    ' "$f"
    rm "${f}.bak"
done

# Preserve Go's BSD-3-Clause license.
cp "$GOROOT/LICENSE" "$DEST/LICENSE"
echo "Extracted goasm from Go $(go version) on $(date)" > "$DEST/EXTRACTED.txt"

echo ""
echo "Done. Next:"
echo "  1. Ensure goasm/stub/stub.go exists"
echo "  2. go build ./goasm/..."
echo "  3. Fix compilation errors iteratively"
