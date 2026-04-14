riscv-emulator
==============

macOS uses a different RISC-V toolchain from linux.

# Prerequisites (one-time)

brew install riscv64-elf-gcc cmake

# Extract

tar -xzf riscv-emulator.tar.gz
cd riscv

# Unit tests — works immediately

go test -v ./...

# One-time setup (clones libriscv ~400MB, builds static libs, compiles guest ELF)

make bench-setup

# Full benchmark comparison

make bench

# Quick targets (no libriscv needed)

make bench-ours       # our GuestMemory benchmarks only

make test             # unit tests

