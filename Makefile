# Makefile — RISC-V emulator benchmark harness
#
# Guest ELF compilation uses `zig cc` as the cross-compiler on both
# macOS and Linux. Zig bundles musl libc for all targets — no separate
# toolchain, sysroot, or build step required.
#
# Prerequisites:
#   macOS:  brew install cmake zig
#   Linux:  apt install cmake  &&  snap install zig --classic --edge
#           (or download from https://ziglang.org/download/)
#
# Quick start:
#   make bench-setup    # one-time: clone+patch+build libriscv, compile guest
#   make bench          # run full comparison
#
# Other targets:
#   make bench-ours     # our GuestMemory benchmarks only (no libriscv needed)
#   make bench-libriscv # libriscv calibration benchmarks only
#   make bench-mem      # head-to-head memory pair benchmark only
#   make bench-smoke    # quick smoke test (~3s)
#   make test           # unit tests
#   make clean          # remove xendor/build_capi and generated ELF
#   make help           # this message

.PHONY: all help bench-setup softfloat test-softfloat bench bench-quick \
        bench-raw bench-ours bench-cpu lazy-bench bench-libriscv bench-mem \
        bench-smoke bench-summary bench-lots test clean check-tools \
        libriscv-build guest-elf guest-native guest-wasm \
        coremark-elf dhrystone-elf bench-coremark bench-dhrystone \
        bench-jit-coremark bench-jit-dhrystone bench-chain-ref \
        darwin-perf bench-wasm build-luajit-riscv \
        hello hello-elfs quad standard test-arm64-qemu \
        test-arm64-qemu-main test-arm64-qemu-lockstep \
        bench-arm64-qemu bench-arm64-qemu-full rstrace \
        build-slim-linux build-slim-linux-amd64

# ── platform detection ─────────────────────────────────────────────────────

UNAME_S := $(shell uname -s)

ifeq ($(UNAME_S),Darwin)
  PLATFORM      := macos
  NPROC         := $(shell sysctl -n hw.logicalcpu)
  CPU_INFO      := $(shell sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)
  # Security framework: libriscv uses SecRandomCopyBytes for getrandom syscall on macOS
  EXTRA_LDFLAGS := -framework Security
else
  PLATFORM      := linux
  NPROC         := $(shell nproc)
  CPU_INFO      := $(shell grep 'model name' /proc/cpuinfo 2>/dev/null \
                       | head -1 | cut -d: -f2 | xargs || echo unknown)
  EXTRA_LDFLAGS := -lpthread
endif

# ── guest ELF: zig cc -target riscv64-linux-musl ──────────────────────────
#
# zig cc bundles musl libc for every target. The same command works on
# macOS and Linux without any sysroot installation.
#
# Target: riscv64-linux-musl
#   - rv64gc baseline (Zig's default for riscv64, includes IMAFDC+Zicsr+Zifencei)
#   - musl libc, statically linked
#   - libriscv's Linux personality handles the musl syscall ABI identically
#     to glibc for our purposes (exit, write, brk, mmap)
#
# Override ZIG_CC if zig is not on PATH:
#   make bench-setup ZIG_CC=/path/to/zig

ZIG_CC     ?= zig
##ZIG_TARGET := riscv64-linux-musl
ZIG_TARGET := riscv64-freestanding
GUEST_CFLAGS := -O2 -target $(ZIG_TARGET) -static -mcpu=generic_rv64+m+a+f+d+c \
            -mabi=lp64d \
            -fPIC \
            -mcmodel=medany \
            -T riscv-elf-tests/env/p/link.ld \
            -I riscv-elf-tests/env/p \
            -I riscv-elf-tests/isa/macros/scalar \
            -nostdlib

GUEST_CFLAGS_WITH_STDLIB := -O2 -target $(ZIG_TARGET) -static -mcpu=generic_rv64+m+a+f+d+c \
            -mabi=lp64d \
            -fPIC \
            -mcmodel=medany \
            -T riscv-elf-tests/env/p/link.ld \
            -I riscv-elf-tests/env/p \
            -I riscv-elf-tests/isa/macros/scalar

ARM64_QEMU_BENCH ?= ^BenchmarkRVTests_UI_(Interp2|LazyJIT2|LazyJIT2_RV8|LazyJIT2_Hot|LazyJIT2_Hot_RV8|RunOnlyInterp2|RunOnlyLazyJIT2_Hot|RunOnlyLazyJIT2_Hot_RV8)$$
ARM64_QEMU_BENCHTIME ?= 3x
ARM64_QEMU_COMPARE_BENCHTIME ?= 1x

# ── paths ──────────────────────────────────────────────────────────────────

ROOT        := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
VENDOR      := $(ROOT)xendor/libriscv
BUILD       := $(VENDOR)/build_capi
LIB_CAPI    := $(BUILD)/libriscv_capi.a
LIB_CORE    := $(BUILD)/libriscv/libriscv.a
GUEST_DIR   := $(ROOT)bench/libriscv_guest
GUEST_SRC   := $(GUEST_DIR)/bench_guest.c
GUEST_ELF   := $(GUEST_DIR)/bench_guest.elf
GUEST_NATIVE := $(GUEST_DIR)/bench_guest.native
GUEST_WASM   := $(GUEST_DIR)/bench_guest.wasm
OUR_LINUX    := $(ROOT)xendor/linux-6.17-hand-built
OUR_LINUX_AMD64 := $(OUR_LINUX)/amd64
INITRAMFS_DIR := $(ROOT)xendor/alpine-minirootfs-3.24.1-riscv64
INITRAMFS_CPIO := $(ROOT)xendor/linux/initramfs.cpio.gz
RSTRACE_BIN  := $(INITRAMFS_DIR)/bin/rstrace
RSTRACE_GOCACHE ?= /tmp/riscv-emu-go-build-cache
RSTRACE_GOMODCACHE ?= /tmp/riscv-emu-go-mod-cache
UCB_BAR      := $(ROOT)xendor/ucb-bar
SOFTFLOAT_BUILD := $(UCB_BAR)/SoftFloat-3e/build/Linux-x86_64-GCC
TESTFLOAT_BUILD := $(UCB_BAR)/berkeley-testfloat-3/build/Linux-x86_64-GCC
TESTFLOAT_OPTS_NO16 := -DFLOAT64 -DEXTFLOAT80 -DFLOAT128 -DFLOAT_ROUND_ODD -DLONG_DOUBLE_IS_EXTFLOAT80

# -- profile guided optimization

PGO_FILE := $(ROOT)default.pgo
PGO_FLAG := $(if $(wildcard $(PGO_FILE)),-pgo=$(PGO_FILE),)

# ── CoreMark / Dhrystone RV64 ELFs ────────────────────────────────────────
# Sources vendored at xendor/coremark and xendor/dhrystone. Freestanding port
# layers live under bench/{coremark,dhrystone}_guest. Built ELFs are artifacts
# at bench/*.elf (gitignored).

CM_VENDOR   := $(ROOT)xendor/coremark
CM_PORT     := $(ROOT)bench/coremark_guest
CM_ELF      := $(ROOT)bench/coremark.elf
CM_SRCS     := $(CM_VENDOR)/core_main.c $(CM_VENDOR)/core_list_join.c \
               $(CM_VENDOR)/core_matrix.c $(CM_VENDOR)/core_state.c \
               $(CM_VENDOR)/core_util.c \
               $(CM_PORT)/core_portme.c $(CM_PORT)/start.c

DHRY_VENDOR := $(ROOT)xendor/dhrystone
DHRY_PORT   := $(ROOT)bench/dhrystone_guest
DHRY_ELF    := $(ROOT)bench/dhrystone.elf
DHRY_SRCS   := $(DHRY_VENDOR)/dhrystone.c $(DHRY_VENDOR)/dhrystone_main.c \
               $(DHRY_PORT)/port.c
RESULTS_DIR := /tmp/riscv-bench
#PATCH_STAMP := $(VENDOR)/.patched

# ── tools ──────────────────────────────────────────────────────────────────

CMAKE         ?= cmake
GIT           ?= git
#LIBRISCV_REPO ?= https://github.com/libriscv/libriscv.git
#LIBRISCV_REF  ?= master

# ── cgo environment ────────────────────────────────────────────────────────

CGO_CFLAGS_VAL  := -I$(VENDOR)/c
CGO_LDFLAGS_VAL := -L$(BUILD) -L$(BUILD)/libriscv \
                   -lriscv_capi -lriscv -lstdc++ -lm $(EXTRA_LDFLAGS)

export CGO_ENABLED := 1

# ── benchmark parameters ───────────────────────────────────────────────────

BENCH_COUNT ?= 5
BENCH_TIME  ?= 2s
CPUPROFILE  ?=

BENCH_FLAGS := \
    -bench=. \
    -benchmem \
    -count=$(BENCH_COUNT) \
    -benchtime=$(BENCH_TIME) \
    $(if $(CPUPROFILE),-cpuprofile=$(CPUPROFILE),)

# ── top-level ──────────────────────────────────────────────────────────────

all: help
	#GOEXPERIMENT=nojsonv2 go install ./cmd/emu
	#GOEXPERIMENT=nojsonv2 go install ./cmd/emul
	#GOEXPERIMENT=nojsonv2 go install ./cmd/rekey
	go install ./cmd/emu
	go install ./cmd/emul
	go install ./cmd/rekey

help:
	@echo ""
	@echo "  RISC-V emulator benchmark harness  [platform: $(PLATFORM)]"
	@echo ""
	@echo "  Prerequisites:"
ifeq ($(PLATFORM),macos)
	@echo "    brew install cmake zig"
else
	@echo "    apt install cmake"
	@echo "    snap install zig --classic --edge"
	@echo "    # or: https://ziglang.org/download/"
endif
	@echo ""
	@echo "  Setup (run once):"
	@echo "    make bench-setup"
	@echo ""
	@echo "  Benchmarks:"
	@echo "    make bench-alloc      JIT allocator comparison (rv8 vs abjit vs libriscv)"
	@echo "    make bench-quick      fast head-to-head (<1s)"
	@echo "    make bench            full comparison (ours + libriscv)"
	@echo "    make bench-ours       our GuestMemory only (no libriscv needed)"
	@echo "    make bench-libriscv   libriscv calibration only"
	@echo "    make bench-mem        memory pair head-to-head only"
	@echo "    make bench-smoke      quick sanity check (~3s)"
	@echo "    make bench-coremark   CoreMark RV64 (cached vs uncached interpreter)"
	@echo "    make bench-dhrystone  Dhrystone RV64 (cached vs uncached interpreter)"
	@echo "    make lazy-bench       zygo fib(10) under Jea9Linux lazy JIT"
	@echo "    make bench-jit-coremark   CoreMark under JIT (rv8 vs abjit)"
	@echo "    make bench-jit-dhrystone  Dhrystone under JIT (rv8 vs abjit)"
	@echo "    make bench-chain-ref      chain-counter reference, abjit (all 3 workloads)"
	@echo ""
	@echo "  Guest ELFs (normally auto-built by the bench targets):"
	@echo "    make coremark-elf        build bench/coremark.elf from xendor/coremark"
	@echo "    make dhrystone-elf       build bench/dhrystone.elf from xendor/dhrystone"
	@echo "    make build-luajit-riscv  cross-compile LuaJIT → bench/luajit.elf"
	@echo "    make hello-elfs          build bench/hello_guest/hello_{libriscv,gocpu}.elf"
	@echo ""
	@echo "  ECALL benchmark:"
	@echo "    make hello               per-ECALL timing: libriscv vs GoCPU"
	@echo ""
	@echo "  Other:"
	@echo "    make test             unit tests"
	@echo "    make rstrace          build guest linux/riscv64 syscall tracer"
	@echo "    make build-slim-linux        build slim riscv64 Linux kernel"
	@echo "    make build-slim-linux-amd64  build slim amd64/x86_64 Linux kernel"
	@echo "    make softfloat        build vendored Berkeley SoftFloat/TestFloat"
	@echo "    make test-softfloat   stream TestFloat gold cases into cpu.go FP tests"
	@echo "    make test-arm64-qemu           cross-build root/riscv-tests/lockstep under qemu-system-aarch64"
	@echo "    make test-arm64-qemu-main      same, but skip sharded lockstep"
	@echo "    make test-arm64-qemu-lockstep  sharded non-FP lockstep only"
	@echo "    make bench-arm64-qemu          qemu-system arm64 JIT vs interpreter comparison"
	@echo "    make bench-arm64-qemu-full     qemu-system arm64 benchmark smoke lane"
	@echo "      ARM64_QEMU_CPU=cortex-a72 by default; override if your QEMU needs another model"
	@echo "    make clean            remove xendor/build_capi and generated ELF"
	@echo ""
	@echo "  Overrides:"
	@echo "    ZIG_CC=$(ZIG_CC)  BENCH_COUNT=$(BENCH_COUNT)  BENCH_TIME=$(BENCH_TIME)"
	@echo ""

# ── setup pipeline ─────────────────────────────────────────────────────────

bench-setup: check-tools libriscv-build guest-elf
	go install github.com/tetratelabs/wazero/cmd/wazero@latest
	@echo "we use vendored xendor/libriscv now, and do not pull from github."
	@echo ""
	@echo "  ✓ bench-setup complete — run 'make bench' to start"
	@echo ""

softfloat:
	@echo "── building Berkeley SoftFloat/TestFloat ───────────────────────"
	$(MAKE) -C $(SOFTFLOAT_BUILD)
	$(MAKE) -C $(TESTFLOAT_BUILD) clean
	$(MAKE) -C $(TESTFLOAT_BUILD) TESTFLOAT_OPTS='$(TESTFLOAT_OPTS_NO16)'
	@echo ""
	@echo "  ✓ softfloat complete — TestFloat built without FLOAT16/BFLOAT16"
	@echo ""

test-softfloat: softfloat
	GOCPU_VIZJIT_OFF=1 go test -tags softfloat -run 'TestCPU_(FPSoftFloat|FPRISCVSpec)' .

check-tools:
	@echo "── checking prerequisites  [$(PLATFORM)] ───────────────────────"
	@command -v $(GIT)    >/dev/null 2>&1 \
	    || { echo "  ✗ git not found"; exit 1; }
	@command -v $(CMAKE)  >/dev/null 2>&1 \
	    || { echo "  ✗ cmake not found"; \
	         echo "    macOS: brew install cmake"; \
	         echo "    Linux: apt install cmake"; exit 1; }
	@command -v go     >/dev/null 2>&1 \
	    || { echo "  ✗ go not found  →  https://go.dev/dl/"; exit 1; }
	@# C++ compiler for libriscv build (clang++ on macOS, g++ on Linux)
	@command -v clang++ >/dev/null 2>&1 \
	  || command -v g++  >/dev/null 2>&1 \
	  || { echo "  ✗ no C++ compiler"; \
	       echo "    macOS: xcode-select --install"; \
	       echo "    Linux: apt install g++"; exit 1; }
	@command -v $(ZIG_CC) >/dev/null 2>&1 \
	    || { echo "  ✗ zig not found"; \
	         echo "    macOS: brew install zig"; \
	         echo "    Linux: snap install zig --classic --edge"; \
	         echo "           or https://ziglang.org/download/"; exit 1; }
	@echo "  ✓ git:    $$(git --version | cut -d' ' -f3)"
	@echo "  ✓ cmake:  $$(cmake --version | head -1 | cut -d' ' -f3)"
	@echo "  ✓ c++:    $$(clang++ --version 2>/dev/null | head -1 \
	                 || g++ --version | head -1)"
	@echo "  ✓ zig:    $$($(ZIG_CC) version)"
	@echo "  ✓ go:     $$(go version | awk '{print $$3, $$4}')"

# ── libriscv clone ─────────────────────────────────────────────────────────

# libriscv-clone: $(VENDOR)/.git
# $(VENDOR)/.git:
# 	@echo "── cloning libriscv ────────────────────────────────────────────"
# 	@mkdir -p $(dir $(VENDOR))
# 	$(GIT) clone --depth=1 $(LIBRISCV_REPO) $(VENDOR) 2>&1 | sed 's/^/  /'
# 	@echo "  ✓ cloned into $(VENDOR)"

# ── libriscv patch ─────────────────────────────────────────────────────────
# macOS SDK defines stdout as a macro (__stdoutp) in <stdio.h>.
# libriscv's RISCVOptions struct has a field named 'stdout' which clashes.
# We rename it to 'output' in both the header and implementation.

# libriscv-patch: $(PATCH_STAMP)
# $(PATCH_STAMP): $(VENDOR)/CMakeLists.txt
# 	@echo "── patching libriscv (stdout→output field rename) ──────────────"
# 	sed -i.bak \
# 	    's/riscv_stdout_func_t stdout;/riscv_stdout_func_t output; \/\* renamed: stdout is a macro on macOS \*\//' \
# 	    $(VENDOR)/c/libriscv.h
# 	sed -i.bak \
# 	    's/riscv_stdout_func_t stdout = nullptr;/riscv_stdout_func_t output = nullptr;/' \
# 	    $(VENDOR)/c/libriscv.cpp
# 	sed -i.bak \
# 	    's/\.stdout = options->stdout,/.output = options->output,/' \
# 	    $(VENDOR)/c/libriscv.cpp
# 	sed -i.bak \
# 	    's/userdata\.stdout/userdata.output/g' \
# 	    $(VENDOR)/c/libriscv.cpp
# 	@grep -q '\.stdout\b' $(VENDOR)/c/libriscv.h $(VENDOR)/c/libriscv.cpp \
# 	    && { echo "  ✗ patch incomplete"; exit 1; } || true
# 	@# no_translate field (for JIT-disabled benchmarks) should already be
# 	@# present in the committed vendor files. Warn if missing.
# 	@grep -q 'no_translate' $(VENDOR)/c/libriscv.h \
# 	    || echo "  ⚠ no_translate field missing from libriscv.h — no-JIT benchmark will not work"
# 	@# Fix Intel Mac: libriscv CMake assumes all macOS == ARM64 (Apple Silicon).
# 	@# On Intel Macs (x86_64) we must patch CMakeLists.txt to use TCC_TARGET_X86_64.
# ifeq ($(PLATFORM),macos)
# 	@if [ "$$(uname -m)" = "x86_64" ]; then \
# 	    echo "  ✓ Intel Mac: patching libtcc target to x86_64"; \
# 	    awk '/CMAKE_HOST_APPLE OR APPLE/{in_apple=1} in_apple && /TCC_TARGET_ARM64=1/{sub(/TCC_TARGET_ARM64=1/,"TCC_TARGET_X86_64=1");in_apple=0} {print}' \
# 	        $(VENDOR)/lib/CMakeLists.txt > $(VENDOR)/lib/CMakeLists.txt.tmp \
# 	    && mv $(VENDOR)/lib/CMakeLists.txt.tmp $(VENDOR)/lib/CMakeLists.txt; \
# 	else \
# 	    echo "  ✓ Apple Silicon: TCC_TARGET_ARM64 correct"; \
# 	fi
# endif
# 	@touch $(PATCH_STAMP)
# 	@echo "  ✓ patch applied"

# ── libriscv build ─────────────────────────────────────────────────────────

libriscv-build: $(LIB_CAPI)
$(LIB_CAPI): # $(PATCH_STAMP)
	@echo "── building libriscv C API ─────────────────────────────────────"
	@mkdir -p $(BUILD)
	cd $(BUILD) && $(CMAKE) $(VENDOR)/c \
	    -DCMAKE_BUILD_TYPE=Release \
	    -DCMAKE_CXX_FLAGS="-O2 -DNDEBUG" \
	    -DCMAKE_C_FLAGS="-O2 -DNDEBUG" \
	    -DRISCV_FCSR=ON \
	    -Wno-dev \
	    2>&1 | grep -E "^(--|Configuring|Generating|Build)" | sed 's/^/  /'
	cd $(BUILD) && $(MAKE) -j$(NPROC) 2>&1 \
	    | grep -v "^make\[" | grep -v "^--" | sed 's/^/  /'
	@test -f $(LIB_CAPI) || { echo "  ✗ libriscv_capi.a not built"; exit 1; }
	@test -f $(LIB_CORE) || { echo "  ✗ libriscv.a not built"; exit 1; }
	@echo "  ✓ libriscv_capi.a: $$(du -h $(LIB_CAPI) | cut -f1)"
	@echo "  ✓ libriscv.a:      $$(du -h $(LIB_CORE) | cut -f1)"

# ── guest ELF ──────────────────────────────────────────────────────────────

guest-elf: $(GUEST_ELF)
$(GUEST_ELF): $(GUEST_SRC)
	@echo "── compiling RISC-V guest ELF ──────────────────────────────────"
	@echo "  target=$(ZIG_TARGET)"
	$(ZIG_CC) cc $(subst -T riscv-elf-tests/env/p/link.ld,-T $(GUEST_DIR)/link.ld,$(GUEST_CFLAGS)) -o $(GUEST_ELF) $(GUEST_SRC)
	@test -f $(GUEST_ELF) || { echo "  ✗ guest ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(GUEST_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(GUEST_ELF) | cut -f1)"

timing.elf:
	$(ZIG_CC) cc $(subst -T riscv-elf-tests/env/p/link.ld,-T $(GUEST_DIR)/link.ld,$(GUEST_CFLAGS_WITH_STDLIB)) -o bench/timing.elf bench/timing_guest/timer.c
	@test -f bench/timing.elf || { echo "  ✗ guest ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(GUEST_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(GUEST_ELF) | cut -f1)"

guest-wasm: $(GUEST_SRC)
	$(WASI_SDK)/bin/clang -O3 -D__linux__ \
      --target=wasm32-wasi \
      --sysroot=$(WASI_SDK)/share/wasi-sysroot \
      -o $(GUEST_WASM) $(GUEST_SRC)

guest-native: $(GUEST_NATIVE)
$(GUEST_NATIVE): $(GUEST_SRC)
	@echo "── compiling native guest  ──────────────────────────────────"
	$(ZIG_CC) cc -O3 -march=native -o $(GUEST_NATIVE) $(GUEST_SRC)
	@test -f $(GUEST_NATIVE) || { echo "  ✗ guest native not produced"; exit 1; }
	@echo "  ✓ $$(file $(GUEST_NATIVE) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(GUEST_NATIVE) | cut -f1)"

# ── hello ECALL benchmark ─────────────────────────────────────────────────
# Builds two near-identical RV64 ELFs that differ only in the message
# string they print, then runs a Go driver that times per-ECALL cost
# across libriscv and our emulator. See bench/hello_guest/hello.c and
# bench/hellobench/main.go.

HELLO_DIR := $(ROOT)bench/hello_guest
HELLO_SRC := $(HELLO_DIR)/hello.c
HELLO_LD  := $(HELLO_DIR)/link.ld
HELLO_LR  := $(HELLO_DIR)/hello_libriscv.elf
HELLO_GC  := $(HELLO_DIR)/hello_gocpu.elf

HELLO_CFLAGS := -O2 -target riscv64-freestanding \
    -mcpu=generic_rv64+m+a+f+d+c -mabi=lp64d \
    -fPIC -mcmodel=medany \
    -nostdlib -ffreestanding \
    -T $(HELLO_LD)

hello-elfs: $(HELLO_LR) $(HELLO_GC)

$(HELLO_LR): $(HELLO_SRC) $(HELLO_LD)
	@echo "── compiling hello_libriscv.elf ────────────────────────────────"
	$(ZIG_CC) cc $(HELLO_CFLAGS) -DMSG='"Hello, libriscv!\n"' -o $@ $<
	@echo "  ✓ $$(file $@ | cut -d: -f2 | xargs)"

$(HELLO_GC): $(HELLO_SRC) $(HELLO_LD)
	@echo "── compiling hello_gocpu.elf ───────────────────────────────────"
	$(ZIG_CC) cc $(HELLO_CFLAGS) -DMSG='"Hello, Go CPU!\n"' -o $@ $<
	@echo "  ✓ $$(file $@ | cut -d: -f2 | xargs)"

hello: hello-elfs
	@echo "── per-ECALL timing (libriscv vs GoCPU) ────────────────────────"
	GOCPU_VIZJIT_OFF=1 go run -tags libriscv ./bench/hellobench/

bench-wasm:
	go run ./bench/wazero_bench $(ROOT)/bench/libriscv_guest/bench_guest.wasm

# ── CoreMark ELF ──────────────────────────────────────────────────────────
coremark-elf: $(CM_ELF)
$(CM_ELF): $(CM_SRCS) $(CM_PORT)/core_portme.h $(CM_PORT)/link.ld
	@echo "── compiling CoreMark RV64 ELF ─────────────────────────────────"
	$(ZIG_CC) cc -O2 -target riscv64-freestanding \
	    -mcpu=generic_rv64+m+a+f+d+c -mabi=lp64d -mcmodel=medany \
	    -nostdlib -static \
	    -T $(CM_PORT)/link.ld \
	    -I $(CM_VENDOR) -I $(CM_PORT) \
	    -DITERATIONS=1000 -DPERFORMANCE_RUN=1 -DFLAGS_STR=\"-O2\" \
	    -o $(CM_ELF) $(CM_SRCS)
	@test -f $(CM_ELF) || { echo "  ✗ coremark ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(CM_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(CM_ELF) | cut -f1)"

# ── Dhrystone ELF ─────────────────────────────────────────────────────────
# The vendored dhry header pulls in <stdio.h>/<string.h>, so we inject our
# own self-contained header via -include before the vendored one can be
# (accidentally) pulled in via the file-local include search path.
dhrystone-elf: $(DHRY_ELF)
$(DHRY_ELF): $(DHRY_SRCS) $(DHRY_PORT)/dhrystone.h $(DHRY_PORT)/util.h $(DHRY_PORT)/alloca.h $(DHRY_PORT)/link.ld
	@echo "── compiling Dhrystone RV64 ELF ────────────────────────────────"
	$(ZIG_CC) cc -O2 -std=gnu89 -target riscv64-freestanding \
	    -mcpu=generic_rv64+m+a+f+d+c -mabi=lp64d -mcmodel=medany \
	    -nostdlib -static \
	    -T $(DHRY_PORT)/link.ld \
	    -include $(DHRY_PORT)/dhrystone.h \
	    -I $(DHRY_PORT) \
	    -DTIME=1 -DNUMBER_OF_RUNS=500000 -Wno-everything \
	    -o $(DHRY_ELF) $(DHRY_SRCS)
	@test -f $(DHRY_ELF) || { echo "  ✗ dhrystone ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(DHRY_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(DHRY_ELF) | cut -f1)"

# ── LuaJIT riscv64 ELF ────────────────────────────────────────────────────
# Cross-compile the LJRV fork (plctlab LuaJIT RISC-V port, vendored at
# xendor/LuaJIT) into a standalone riscv64-linux-musl binary. Host tools
# (minilua, buildvm) build with the native cc; all target objects and the
# final link go through `zig cc -target riscv64-linux-musl -static`.
# Output: bench/luajit.elf.

LUAJIT_SRC := $(ROOT)xendor/LuaJIT
LUAJIT_ELF := $(ROOT)bench/luajit.elf

build-luajit-riscv: $(LUAJIT_ELF)
$(LUAJIT_ELF):
	@echo "── cross-compiling LuaJIT → riscv64-linux-musl ─────────────────"
	$(MAKE) -C $(LUAJIT_SRC)/src clean TARGET_SYS=Linux
	$(MAKE) -C $(LUAJIT_SRC)/src \
	    HOST_CC="cc" \
	    CROSS="" \
	    STATIC_CC="$(ZIG_CC) cc -target riscv64-linux-musl" \
	    DYNAMIC_CC="$(ZIG_CC) cc -target riscv64-linux-musl -fPIC" \
	    TARGET_LD="$(ZIG_CC) cc -target riscv64-linux-musl -static -s" \
	    TARGET_AR="$(ZIG_CC) ar rcus" \
	    TARGET_STRIP=":" \
	    TARGET_SYS=Linux \
	    TARGET_CFLAGS="-DLUAJIT_NO_UNWIND -fno-asynchronous-unwind-tables -fno-unwind-tables" \
	    BUILDMODE=static
	cp $(LUAJIT_SRC)/src/luajit $(LUAJIT_ELF)
	@test -f $(LUAJIT_ELF) || { echo "  ✗ luajit ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(LUAJIT_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(LUAJIT_ELF) | cut -f1)"


# ── benchmark targets ──────────────────────────────────────────────────────

bench-lots: bench-setup
	@mkdir -p $(RESULTS_DIR)
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  BENCHMARK — $$(date '+%Y-%m-%d %H:%M')  [$(PLATFORM)]"
	@echo "  cpu: $(CPU_INFO)"
	@echo "  count=$(BENCH_COUNT)  benchtime=$(BENCH_TIME)"
	@echo "══════════════════════════════════════════════════════════════════"
	@echo ""
	@echo "── [1/2] our GuestMemory ───────────────────────────────────────"
	cd $(ROOT) && go test $(BENCH_FLAGS) \
	    -run='^$$' -bench='^BenchmarkGuestMem' \
	    ./bench/ 2>&1 | tee $(RESULTS_DIR)/ours.txt
	@echo ""
	@echo "── [2/2] libriscv (calibration) ────────────────────────────────"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test -tags libriscv $(BENCH_FLAGS) \
	        -run='^$$' -bench='^BenchmarkLibriscv' \
	        ./bench/libriscv/ 2>&1 | tee $(RESULTS_DIR)/libriscv.txt
	@$(MAKE) --no-print-directory bench-summary


bench-quick: bench-setup
	@mkdir -p $(RESULTS_DIR)
	@echo ""
	@echo "── quick benchmark ────────────────────────────────────────────"
	@echo ""
	@printf "  %-40s " "GuestMem Store64+Load64 pair:"
	@cd $(ROOT) && go test -count=1 -benchtime=100ms -benchmem \
	    -run='^$$' -bench='^BenchmarkGuestMem_Store64Load64Pair$$' \
	    ./bench/ 2>&1 | awk '/Benchmark/{printf "%-12s %s\n", $$3, $$4}'
	@echo "  libriscv (memory pair + execution throughput):"
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test -tags libriscv -count=1 -benchtime=100ms -benchmem \
	        -run='^$$' \
	        -bench='^BenchmarkLibriscv_MemWriteRead64$$|^BenchmarkLibriscv_FullExecution_Steady$$|^BenchmarkLibriscv_FullExecution_NoJIT$$' \
	        ./bench/libriscv/ 2>&1 \
	    | awk ' \
	        /MemWriteRead64/      { p=""; for(i=1;i<=NF;i++){ if($$i=="ns/pair")   printf "  %-40s %s ns/pair\n","libriscv copy_to+from_guest:",p; p=$$i } } \
	        /FullExecution_Steady/ { p=""; for(i=1;i<=NF;i++){ if($$i=="MIPS")       printf "  %-40s %s MIPS\n","libriscv full execution:",p;    p=$$i } } \
	        /FullExecution_NoJIT/  { p=""; for(i=1;i<=NF;i++){ if($$i=="MIPS")       printf "  %-40s %s MIPS\n","libriscv interp (no JIT):",p;   p=$$i } } \
	    '
	@echo "  Go CPU (full execution throughput):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution$$' \
	        ./bench/ 2>&1 \
	    | awk ' \
	        /FullExecution/ { p=""; for(i=1;i<=NF;i++){ if($$i=="MIPS") printf "  %-40s %s MIPS\n","Go CPU full execution:",p; p=$$i } } \
	    '
	@echo ""

bench-raw: bench-setup
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test -tags libriscv -count=1 -benchtime=100ms -benchmem \
	        -run='^$$' \
	        -bench='^BenchmarkLibriscv_MemWriteRead64$$|^BenchmarkLibriscv_FullExecution_Steady$$' \
	        ./bench/libriscv/ 2>&1


bench-cpu: guest-elf
	@echo "── Go CPU execution benchmark ─────────────────────────────────"
	cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU' \
	        ./bench/ 2>&1

lazy-bench:
	@echo "── zygo fib(10), Jea9Linux lazy JIT vs Interp vs Libriscv ─────"
	if command -v zygo >/dev/null 2>&1; then time zygo -c '(defn fib [x] (cond (== x 0) 0 (== x 1) 1 (+ (fib (- x 1)) (fib (- x 2))))) (println (fib 10))'; else echo "native zygo command not available, skipping."; fi
	cd $(ROOT) && go test -count=1 -benchtime=1x -benchmem \
	         -run=xxx -bench=BenchmarkCPU_ZygoFib10_Interpreter \
	         ./bench/ 2>&1
	cd $(ROOT) && ZYGO_ELF=$(ROOT)bench/zygo.elf \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run=xxx -bench=BenchmarkCPU_ZygoFib10_LazyJIT \
	        ./bench/ 2>&1
	cd $(ROOT) && go test -tags libriscv -count=1 -benchtime=1x -benchmem \
	         -run=xxx -bench=BenchmarkCPU_ZygoFib10_Libriscv \
	         ./bench/ 2>&1

bench-coremark: coremark-elf
	@echo "── CoreMark (cached vs uncached) ───────────────────────────────"
	cd $(ROOT) && CM_ELF=$(CM_ELF) \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_CoreMark' \
	        ./bench/ 2>&1

bench-dhrystone: dhrystone-elf
	@echo "── Dhrystone (cached vs uncached) ──────────────────────────────"
	cd $(ROOT) && DHRY_ELF=$(DHRY_ELF) \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_Dhrystone' \
	        ./bench/ 2>&1

bench-jit-coremark: coremark-elf
	@echo "── CoreMark (JIT, rv8 vs abjit) ───────────────────────────────"
	cd $(ROOT) && CM_ELF=$(CM_ELF) \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkJIT_CoreMark' \
	        ./bench/ 2>&1

bench-jit-dhrystone: dhrystone-elf
	@echo "── Dhrystone (JIT, rv8 vs abjit) ──────────────────────────────"
	cd $(ROOT) && DHRY_ELF=$(DHRY_ELF) \
	    go test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkJIT_Dhrystone' \
	        ./bench/ 2>&1

bench-chain-ref: coremark-elf dhrystone-elf
	@echo "── Chain-counter reference, abjit (bench_guest + CoreMark + Dhrystone) ─"
	cd $(ROOT) && CM_ELF=$(CM_ELF) DHRY_ELF=$(DHRY_ELF) \
	    go test -count=1 -v \
	        -run='^TestJIT_(ChainReference|CoreMark_ChainReference|Dhrystone_ChainReference)$$' \
	        ./bench/ 2>&1

bench-ours:
	@echo "── our GuestMemory benchmarks ──────────────────────────────────"
	cd $(ROOT) && go test $(BENCH_FLAGS) \
	    -run='^$$' -bench='^BenchmarkGuestMem' \
	    ./bench/ 2>&1

bench-libriscv: bench-setup
	@echo "── libriscv calibration benchmarks ─────────────────────────────"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test -tags libriscv $(BENCH_FLAGS) \
	        -run='^$$' -bench='^BenchmarkLibriscv' \
	        ./bench/libriscv/ 2>&1

bench-mem: bench-setup
	@echo ""
	@echo "── memory pair head-to-head ────────────────────────────────────"
	@echo ""
	@echo "  [ours] GuestMemory Store64+Load64 pair"
	cd $(ROOT) && go test $(BENCH_FLAGS) \
	    -run='^$$' -bench='^BenchmarkGuestMem_Store64Load64Pair$$' \
	    ./bench/ 2>&1
	@echo ""
	@echo "  [libriscv] copy_to_guest+copy_from_guest pair"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test -tags libriscv $(BENCH_FLAGS) \
	        -run='^$$' -bench='^BenchmarkLibriscv_MemWriteRead64$$' \
	        ./bench/libriscv/ 2>&1

bench-smoke: bench-setup
	@echo "── smoke test ──────────────────────────────────────────────────"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test -tags libriscv -v \
	        -run='^TestLibriscvSmokeTest$$' \
	        ./bench/libriscv/ 2>&1

# The number of riscv instruction attempts during a
# run on compiled bench/libriscv_guest/bench_guest.c ;
# we use this number for the numerator in native 
# benchmarking for an apples-to-apples comparison.
NATIVE_INS_ATTEMPTS := 2524935201

bench:
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  JIT COMPARISON (rv8 vs abjit) — $$(date '+%Y-%m-%d %H:%M')  [$(PLATFORM)]"
	@echo "  cpu: $(CPU_INFO)"
	@echo "══════════════════════════════════════════════════════════════════"
	@echo ""
	@printf "  %-44s %s\n" "Strategy" "MIPS"
	@printf "  %-44s %s\n" "────────────────────────────────────────────" "──────────"
	@printf "  %-44s " "Go JIT — rv8 Fixed Static Mapping (native):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    go test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution_JIT_Rv8$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "Go JIT — abjit (native):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    go test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution_JIT_ABJIT$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "Go interpreter (no JIT):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    go test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "libriscv — JIT (TCC):"
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test $(PGO_FLAG) -tags libriscv -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkLibriscv_FullExecution_Steady$$' \
	        ./bench/libriscv/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "libriscv — interpreter (no JIT):"
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    go test $(PGO_FLAG) -tags libriscv -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkLibriscv_FullExecution_NoJIT$$' \
	        ./bench/libriscv/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "native x86-64 (-O3 -march=native):"
	@elapsed=$$({ /usr/bin/time -p $(GUEST_NATIVE) >/dev/null; } 2>&1 | awk '/^real/{print $$NF}'); \
         awk "BEGIN{printf \"%.0f MIPS  (%.1f ms)\n\", $(NATIVE_INS_ATTEMPTS)/$$elapsed/1000000, $$elapsed*1000}"
	@/bin/echo -n "  " ; make bench-wasm | grep MIPS
	@echo ""
	#make bench-coremark
	#make bench-dhrystone

bench-summary:
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  SUMMARY"
	@echo "══════════════════════════════════════════════════════════════════"
	@echo ""
	@echo "  Memory — Store64+Load64 pair (lower ns/op is better):"
	@printf "    %-44s %s\n" "ours   GuestMem_Store64Load64Pair" \
	    "$$(grep 'Store64Load64Pair' $(RESULTS_DIR)/ours.txt 2>/dev/null \
	        | awk 'NR==1{print $$3, $$4}' || echo '(run make bench first)')"
	@printf "    %-44s %s\n" "libriscv MemWriteRead64" \
	    "$$(grep 'ns/pair' $(RESULTS_DIR)/libriscv.txt 2>/dev/null \
	        | awk 'NR==1{print $$NF, "ns/pair"}' || echo '(run make bench first)')"
	@echo ""
	@echo "  Creation (lower ns/op is better):"
	@printf "    %-44s %s\n" "ours   GuestMem_Alloc64MB" \
	    "$$(grep 'Alloc64MB' $(RESULTS_DIR)/ours.txt 2>/dev/null \
	        | awk 'NR==1{print $$3, $$4}' || echo '(run make bench first)')"
	@printf "    %-44s %s\n" "libriscv MachineCreate (incl. ELF load)" \
	    "$$(grep 'MachineCreate' $(RESULTS_DIR)/libriscv.txt 2>/dev/null \
	        | awk 'NR==1{print $$3, $$4}' || echo '(run make bench first)')"
	@echo ""
	@echo "  libriscv execution throughput (target for our future decoder):"
	@grep 'MIPS' $(RESULTS_DIR)/libriscv.txt 2>/dev/null \
	    | grep -v '^$$' | head -4 | sed 's/^/    /' \
	    || echo "    (run make bench first)"
	@echo ""


# ── unit tests ─────────────────────────────────────────────────────────────

test:
	@echo "── unit tests ──────────────────────────────────────────────────"
	GOCPU_VIZJIT_OFF=1 cd $(ROOT) && go test -count=1 -v ./bench/ 2>&1
	GOCPU_VIZJIT_OFF=1 cd $(ROOT) && time go test -timeout=0 -count=1 -v 2>&1

test-arm64-qemu:
	@echo "── linux/arm64 qemu-system test lane ───────────────────────────"
	GO=go $(ROOT)scripts/test-arm64-qemu.sh

test-arm64-qemu-main: # about 2 minutes on linux
	@echo "── linux/arm64 qemu-system main lane ───────────────────────────"
	ARM64_QEMU_LOCKSTEP=0 GO=go $(ROOT)scripts/test-arm64-qemu.sh

test-arm64-qemu-lockstep:
	@echo "── linux/arm64 qemu-system lockstep lane ───────────────────────"
	ARM64_QEMU_MAIN=0 ARM64_QEMU_LOCKSTEP=1 GO=go $(ROOT)scripts/test-arm64-qemu.sh

bench-arm64-qemu: guest-elf
	@echo "── linux/arm64 qemu-system JIT comparison ──────────────────────"
	GO=go ARM64_QEMU_COMPARE_BENCHTIME=$(ARM64_QEMU_COMPARE_BENCHTIME) \
	    $(ROOT)scripts/bench-arm64-qemu.sh

bench-arm64-qemu-full:
	@echo "── linux/arm64 qemu-system benchmark smoke lane ────────────────"
	ARM64_QEMU_PACKAGE=./bench \
	ARM64_QEMU_REQUIRE_RISCV_TESTS=0 \
	ARM64_QEMU_MAIN=0 \
	ARM64_QEMU_LOCKSTEP=0 \
	ARM64_QEMU_STAGE_BENCH_ELFS=1 \
	GO=go $(ROOT)scripts/test-arm64-qemu.sh \
	    -test.run '^$$' \
	    -test.bench '$(ARM64_QEMU_BENCH)' \
	    -test.benchtime=$(ARM64_QEMU_BENCHTIME) \
	    -test.benchmem \
	    -test.timeout 20m

# ── clean ──────────────────────────────────────────────────────────────────

clean:
	@echo "── cleaning ────────────────────────────────────────────────────"
	rm -rf $(BUILD) $(GUEST_ELF) $(LUAJIT_ELF) $(RESULTS_DIR)
	@echo "  ✓ done"

# ── fuzz oracle targets (fuzzoracle package, CGO always on) ───────────────
# Requires: make bench-setup

FUZZ_ORACLE_CGO_LDFLAGS := -L$(BUILD) -L$(BUILD)/libriscv \
                            -lriscv_capi -lriscv -lstdc++ -lm $(EXTRA_LDFLAGS)

.PHONY: fuzz-oracle fuzz-stores fuzz-rvc fuzz-amo fuzz-fd fuzz-bitmanip fuzz-cfloat
fuzz-oracle: bench-setup
	@echo "── fuzz ALU vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzALUVsLibriscv -fuzz=FuzzALUVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

fuzz-stores: bench-setup
	@echo "── fuzz loads/stores vs libriscv oracle ($(FUZZ_TIME)) ─────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzStoresVsLibriscv -fuzz=FuzzStoresVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1


.PHONY: fuzz-rvc
fuzz-rvc: bench-setup
	@echo "── fuzz RVC vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzRVCVsLibriscv -fuzz=FuzzRVCVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-amo
fuzz-amo: bench-setup
	@echo "── fuzz AMO vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzAMOVsLibriscv -fuzz=FuzzAMOVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-fd
fuzz-fd: bench-setup
	@echo "── fuzz F+D vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzFDVsLibriscv -fuzz=FuzzFDVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-bitmanip
fuzz-bitmanip: bench-setup
	@echo "── fuzz Zicsr/Zba/Zbb/Zbs vs libriscv oracle ($(FUZZ_TIME)) ─"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzBitmanipVsLibriscv -fuzz=FuzzBitmanipVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-cfloat
fuzz-cfloat: bench-setup
	@echo "── fuzz C.FLD/FSD/FLDSP/FSDSP vs libriscv oracle ($(FUZZ_TIME)) "
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    go test \
	        -run FuzzCFloatVsLibriscv -fuzz=FuzzCFloatVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

# ── rebuild libriscv (after flag changes) ──────────────────────────────────
# Use when cmake flags have changed (e.g. after updating the Makefile):
#   make rebuild-libriscv

.PHONY: rebuild-libriscv
rebuild-libriscv:
	@echo "── rebuilding libriscv from scratch ────────────────────────────"
	rm -rf $(BUILD) $(PATCH_STAMP)
	$(MAKE) libriscv-patch libriscv-build
	@echo "  ✓ rebuild complete"

# ── fuzz ───────────────────────────────────────────────────────────────────
# Fuzz the CPU instruction decoder against a pure-Go reference oracle.
# Uses FUZZ_TIMEOUT env var to extend the TestMain timeout beyond 3s.
#
# Usage:
#   make fuzz           # 60s run
#   make fuzz FUZZ_TIME=5m

FUZZ_TIME ?= 60s
FUZZ_LONG_TIME ?= 4h

.PHONY: fuzz
fuzz:
	@echo "── fuzzing CPU ($(FUZZ_TIME)) ──────────────────────────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzCPU -fuzz=FuzzCPU \
	    -fuzztime=$(FUZZ_TIME) \
	    . 2>&1

# fuzz-all: run every fuzz target sequentially, 4h each by default.
# Usage:
#   make fuzz-all                    # 4h per target (12 targets ≈ 48h)
#   make fuzz-all FUZZ_LONG_TIME=1h  # 1h per target
#   nohup make fuzz-all &            # background overnight
.PHONY: fuzz-all
fuzz-all:
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  FUZZ ALL — $(FUZZ_LONG_TIME) per target — $$(date)"
	@echo "══════════════════════════════════════════════════════════════════"
	@echo ""
	@echo "── [1/12] FuzzCPU ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzCPU -fuzz=FuzzCPU \
	    -fuzztime=$(FUZZ_LONG_TIME) . 2>&1 || true
	@echo ""
	@echo "── [2/12] FuzzPeepholeTermination ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && go test \
	    -run FuzzPeepholeTermination -fuzz=FuzzPeepholeTermination \
	    -fuzztime=$(FUZZ_LONG_TIME)  2>&1 || true
	@echo ""
	@echo "── [3/12] FuzzEmitterSequences ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && go test \
	    -run FuzzEmitterSequences -fuzz=FuzzEmitterSequences \
	    -fuzztime=$(FUZZ_LONG_TIME)  2>&1 || true
	@echo ""
	@echo "── [4/12] FuzzBlockStructure ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && go test \
	    -run FuzzBlockStructure -fuzz=FuzzBlockStructure \
	    -fuzztime=$(FUZZ_LONG_TIME)  2>&1 || true
	@echo ""
	@echo "── [5/12] FuzzHighLevelHelpers ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && go test \
	    -run FuzzHighLevelHelpers -fuzz=FuzzHighLevelHelpers \
	    -fuzztime=$(FUZZ_LONG_TIME)  2>&1 || true
	@echo ""
	@echo "── [6/12] FuzzALUVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzALUVsLibriscv -fuzz=FuzzALUVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [7/12] FuzzStoresVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzStoresVsLibriscv -fuzz=FuzzStoresVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [8/12] FuzzRVCVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzRVCVsLibriscv -fuzz=FuzzRVCVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [9/12] FuzzAMOVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzAMOVsLibriscv -fuzz=FuzzAMOVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [10/12] FuzzFDVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzFDVsLibriscv -fuzz=FuzzFDVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [11/12] FuzzCFloatVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzCFloatVsLibriscv -fuzz=FuzzCFloatVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [12/12] FuzzBitmanipVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 go test \
	    -run FuzzBitmanipVsLibriscv -fuzz=FuzzBitmanipVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  FUZZ ALL COMPLETE — $$(date)"
	@echo "══════════════════════════════════════════════════════════════════"

prof:
	go test -run=xxx -bench=BenchmarkCPU_FullExecution -benchtime=1x -cpuprofile cpu.prof ./bench/
	go tool pprof -http=:8080 cpu.prof

mem:
	go test -run=xxx -bench=BenchmarkCPU_FullExecution -benchtime=1x -memprofile mem.prof ./bench/
	go tool pprof -http=:8080 mem.prof


darwin-perf: kpc_perf/kpc_perf.c
	cd kpc_perf && clang -O2 -o ../bench/darwin-perf kpc_perf.c -framework CoreFoundation && cd ..
	cd bench && go test -c
	cd bench && sudo ./darwin-perf ./bench.test -test.v -test.count=1 -test.benchtime=1x -test.benchmem  -test.run=xxx -test.bench='^BenchmarkCPU_FullExecution$$' 

linux-perf:
	cd bench && go test -c && sudo ./perf_l1.sh ./bench.test -test.v -test.count=1 -test.benchtime=1x -test.benchmem -test.run=xxx -test.bench='^BenchmarkCPU_FullExecution$$'

hello-lib:
	# hellobench auto-sets LIBRISCV_DUMP_DIR to a sibling path 
	# and propagates GoCPU's 16-hex run tag, so diff works:
	# diff ~/ris/debug_vizjit_dir/<tag>.gocpu.asm.pc_<X>.asm ~/ris/debug_libriscv_dir/<tag>.libriscv.asm.pc_<X>.asm
	GOCPU_VIZJIT=~/ris/debug_vizjit_dir \
        go run -tags libriscv ./bench/hellobench/ # -only=libriscv


# ── quad: AotJIT vs LazyJIT on four workloads (wall time only) ────────────
# MIPS metric is intentionally omitted — IC counting is disabled for perf.
quad:
	@echo "── quad: AotJIT vs LazyJIT ─────────────────────────────────────"
	@echo ""
	@echo "── 1/4: BenchGuest (fib/sieve) ──"
	cd $(ROOT) && go test -count=1 -benchtime=1x -benchmem \
	    -run='^$$' -bench='^Benchmark(AotJIT|LazyJIT)_BenchGuest$$' \
	    ./bench/ 2>&1
	@echo ""
	@echo "── 2/4: CoreMark ──"
	cd $(ROOT) && go test -count=1 -benchtime=1x -benchmem \
	    -run='^$$' -bench='^Benchmark(AotJIT|LazyJIT)_CoreMark$$' \
	    ./bench/ 2>&1
	@echo ""
	@echo "── 3/4: Dhrystone ──"
	cd $(ROOT) && go test -count=1 -benchtime=1x -benchmem \
	    -run='^$$' -bench='^Benchmark(AotJIT|LazyJIT)_Dhrystone$$' \
	    ./bench/ 2>&1
	@echo ""
	make standard

standard:
	@echo "── 4/4: RISC-V test ELFs (all rv64ui) ──"
	cd $(ROOT) && go test -count=1 -benchtime=1x -benchmem -timeout=120s \
	    -run='^$$' -bench='^BenchmarkRVTests_UI_(AotJIT|LazyJIT)$$' \
	    ./bench/ 2>&1

# this allows idle emu to consume only 1% of cpu when
# real network (non-deterministic) mode is active: we 
# aggressively yield time to the host when we don't need it.
EMU_IDLE ?= -idle 1s

# Go 1.27rc1 defaults github.com/go-json-experiment/json onto the
# stdlib jsonv2 alias path. That compiles with the bumped dependency, but
# tsnet stalls before reaching AuthLoop/IP-ready in -net-direct mode. Keep
# emu on the module's self-contained JSON path until that upstream path
# is healthy.
EMU_GOEXPERIMENT ?= nojsonv2

linux:
	GOEXPERIMENT=nojsonv2 go install ./cmd/emu
	@# Older reference Ubuntu kernel, kept for comparison:
	@# emu -mem 256MB -bios xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf -kernel xendor/linux/boot/vmlinuz-6.17.0-35-generic -initrd $(INITRAMFS_CPIO) -append "console=ttyS0,115200 earlycon=uart8250,mmio,0x10000000 rdinit=/init panic=1 reboot=t init_on_alloc=0 init_on_free=0 audit=0 lsm=capability cma=0 numa=off slub_debug=- lpj=XXXXX"
		@# Slim in-tree Image with built-in hostfs plus virtio-net MMIO.
		emu $(EMU_IDLE) -hostio -net -net-direct -mem 1GB -bios xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf -kernel xendor/linux-6.17-hand-built/Image -initrd $(INITRAMFS_CPIO) -append "console=ttyS0,115200 earlycon=uart8250,mmio,0x10000000 rdinit=/init panic=1 reboot=t init_on_alloc=0 init_on_free=0 audit=0 lsm=capability cma=0 numa=off slub_debug=- lpj=XXXXX"

rstrace:
	GOOS=linux GOARCH=riscv64 CGO_ENABLED=0 \
	GOCACHE=$(RSTRACE_GOCACHE) \
	GOMODCACHE=$(RSTRACE_GOMODCACHE) \
	go build -o $(RSTRACE_BIN) ./cmd/rstrace
	@ls -lh $(RSTRACE_BIN)
	@file $(RSTRACE_BIN) 2>/dev/null || true

save-linux-config:
	cd ~/linux && PATH=/private/tmp/linux-host-tools:/usr/local/opt/llvm/bin:/usr/local/bin:$PATH \
	gmake ARCH=riscv LLVM=1 \
	HOSTCFLAGS=\
	'-I/private/tmp/linux-host-elf-include -include /private/tmp/linux-host-elf-include/darwin_compat.h'\
	savedefconfig
	cp defconfig ~/ris/xendor/linux-6.17-hand-built/
	cp .config ~/ris/xendor/linux-6.17-hand-built/dot.config

# to setup the same kernel build again;
# for a new Linux tree, use
#
# either a)
#   cp ~/ris/xendor/linux-6.17-hand-built/dot.config ~/linux/.config
#   gmake ARCH=riscv olddefconfig
#
# or b)
#   if using savedefconfig:
#   cp ~/ris/xendor/linux-6.17-hand-built/defconfig arch/riscv/configs/ris_fastboot_defconfig
#   gmake ARCH=riscv ris_fastboot_defconfig

build-slim-linux:
	@# note that on darwin we needed to use clang to cross compile to riscv64. 
	@# Neither zig nor riscv64-elf-gcc were up to the job.
	@# The output kernel Image file is a slim 5.8MB uncompressed--nice.
	@# leaves out alot of hardware drivers we do not need. 
	@# Boots in < 8 seconds on the intrepreter--very nice.
	cd ~/linux && PATH='$(OUR_LINUX)/linux-host-tools:/usr/local/opt/llvm/bin:/usr/local/bin:$(PATH)' \
	gmake -j6 ARCH=riscv LLVM=1 \
	HOSTCFLAGS=\
	'-I$(OUR_LINUX)/linux-host-elf-include -include $(OUR_LINUX)/linux-host-elf-include/darwin_compat.h' \
	clean olddefconfig Image savedefconfig && \
	cp -p ~/linux/arch/riscv/boot/Image ~/ris/xendor/linux-6.17-hand-built/ && \
	cp -p ~/linux/.config ~/ris/xendor/linux-6.17-hand-built/dot.config && \
	cp -p ~/linux/defconfig ~/ris/xendor/linux-6.17-hand-built/defconfig

build-slim-linux-amd64:
	@# Build the same ~/linux tree for 64-bit amd64/x86_64.
	@# LLVM=1 gives the target build clang/ld.lld; HOSTLD stays Darwin for host tools.
	cd ~/linux && PATH='$(OUR_LINUX)/linux-host-tools:/usr/local/opt/coreutils/libexec/gnubin:/opt/local/libexec/gnubin:/usr/local/opt/llvm/bin:/usr/local/bin:$(PATH)' \
	PKG_CONFIG_PATH='/opt/local/lib/pkgconfig:$(PKG_CONFIG_PATH)' \
	gmake -j6 ARCH=x86_64 LLVM=1 \
	HOSTLD=/usr/bin/ld \
	HOSTCFLAGS=\
	'-Wno-macro-redefined -I$(OUR_LINUX)/linux-host-elf-include -I$(HOME)/linux/tools/arch/x86/include/uapi -I$(HOME)/linux/arch/x86/include/generated/uapi -idirafter $(HOME)/linux/include/uapi -include $(OUR_LINUX)/linux-host-elf-include/darwin_compat.h' \
	clean olddefconfig bzImage savedefconfig && \
	mkdir -p $(OUR_LINUX_AMD64) && \
	cp -p ~/linux/arch/x86/boot/bzImage $(OUR_LINUX_AMD64)/bzImage && \
	cp -p ~/linux/.config $(OUR_LINUX_AMD64)/dot.config && \
	cp -p ~/linux/defconfig $(OUR_LINUX_AMD64)/defconfig

repack:
	cd $(INITRAMFS_DIR) && find . -print0 | \
	cpio --null --create --format=newc --owner=root | \
	gzip -9 > $(INITRAMFS_CPIO)
