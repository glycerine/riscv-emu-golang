#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."

mkdir -p testvectors/jea9linux/elf

ZIG_CC="${ZIG_CC:-zig}"
CFLAGS=(
  cc
  -target riscv64-linux-musl
  -static
  -nostdlib
  -fno-builtin
  -fno-stack-protector
  -fno-sanitize=all
  -Wl,-e,_start
)

for src in testvectors/jea9linux/src/*.c; do
  name="$(basename "$src" .c)"
  "$ZIG_CC" "${CFLAGS[@]}" "$src" -o "testvectors/jea9linux/elf/$name.elf"
done

mkdir -p testvectors/jea9linux/go/elf

for src in testvectors/jea9linux/go/src/*; do
  [[ -d "$src" ]] || continue
  name="$(basename "$src")"
  GOOS=linux GOARCH=riscv64 CGO_ENABLED=0 \
    go build -o "testvectors/jea9linux/go/elf/$name.elf" "./$src"
done
