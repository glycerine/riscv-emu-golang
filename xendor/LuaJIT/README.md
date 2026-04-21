# LJRV - LuaJIT RISC-V 64 Port

LuaJIT is a Just-In-Time (JIT) compiler for the Lua programming language,
RISC-V is a free and open ISA enabling a new era of processor innovation.

Find latest LJRV release at [plctlab/LuaJIT, branch riscv64-v2.1-branch](https://github.com/plctlab/LuaJIT/tree/riscv64-v2.1-branch) or [plctlab/LuaJIT, branch v2.1-riscv64](https://github.com/plctlab/LuaJIT/tree/v2.1-riscv64).
Development branch is avaliable at [plctlab/LuaJIT, branch riscv](https://github.com/plctlab/LuaJIT/tree/riscv).

**This is the release patch branch of LJRV, containing patchset based on LJRV dev branch commit [1bb709e4abdf ("riscv(ffi): silence warning about nsp may used uninitialized")](https://github.com/plctlab/LuaJIT/commit/1bb709e4abdf254e6ff8ef8128bef04f3bc8b6ac), based off LuaJIT v2.1 rolling [707c12bf00da ("DUALNUM: Improve/fix edge cases of unary minus.")](https://github.com/plctlab/LuaJIT/commit/707c12bf00dafdfd3899b1a6c36435dbbf6c7022)**

## Introduction

LJRV is a ongoing porting project of LuaJIT to the RISC-V 64-bit architecture by PLCT Lab, ISCAS.
The ultimate goal is to provide a RISC-V 64 LuaJIT implementation and have it upstreamed to the official LuaJIT repository.

## Building and Packaging

LJRV is built and packaged in the same way as LuaJIT, requires a recent toolchain based on GCC or Clang with RISC-V 64 support and GNU Make.
For building and installation instructions, please refer to the [LuaJIT README](https://luajit.org/install.html).

For distro maintainers and packagers, we encourage one to follow our branch and patchset, as we would keep it up-to-date with the latest LuaJIT upstream. Cherry-picking and backporting is **strongly** discouraged, ["no matter how self-standing individual changes look (because they often are not)"](https://luajit.org/download.html).

## Progress

- [x] Interpreter Runtime
- [x] JIT Compiler

LJRV is still considered of beta quality, take it with a grain of salt.

## Bug Report

Please report bugs to [Issues](https://github.com/ruyisdk/LuaJIT/issues).

## Copyright

LuaJIT is Copyright (C) 2005-2026 Mike Pall.
LuaJIT is free software, released under the MIT license.
See full Copyright Notice in the COPYRIGHT file or in luajit.h.

LJRV is Copyright (C) 2022-2026 ISRC, ISCAS. Contributed by gns.
LJRV is free software, released under the MIT license.
