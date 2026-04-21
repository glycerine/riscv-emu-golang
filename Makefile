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

.PHONY: all help bench-setup bench bench-quick \
        bench-raw bench-ours bench-cpu bench-libriscv bench-mem \
        bench-smoke bench-summary bench-lots test clean check-tools \
        libriscv-build guest-elf guest-native guest-wasm \
        coremark-elf dhrystone-elf bench-coremark bench-dhrystone \
        darwin-perf

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

GO            ?= go
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
	@echo "    make bench-alloc      JIT allocator comparison (ELS vs Fixed vs TCC vs libriscv)"
	@echo "    make bench-quick      fast head-to-head (<1s)"
	@echo "    make bench            full comparison (ours + libriscv)"
	@echo "    make bench-ours       our GuestMemory only (no libriscv needed)"
	@echo "    make bench-libriscv   libriscv calibration only"
	@echo "    make bench-mem        memory pair head-to-head only"
	@echo "    make bench-smoke      quick sanity check (~3s)"
	@echo "    make bench-coremark   CoreMark RV64 (cached vs uncached interpreter)"
	@echo "    make bench-dhrystone  Dhrystone RV64 (cached vs uncached interpreter)"
	@echo ""
	@echo "  Guest ELFs (normally auto-built by the bench targets):"
	@echo "    make coremark-elf     build bench/coremark.elf from xendor/coremark"
	@echo "    make dhrystone-elf    build bench/dhrystone.elf from xendor/dhrystone"
	@echo ""
	@echo "  Other:"
	@echo "    make test             unit tests"
	@echo "    make clean            remove xendor/build_capi and generated ELF"
	@echo ""
	@echo "  Overrides:"
	@echo "    ZIG_CC=$(ZIG_CC)  BENCH_COUNT=$(BENCH_COUNT)  BENCH_TIME=$(BENCH_TIME)"
	@echo ""

# ── setup pipeline ─────────────────────────────────────────────────────────

bench-setup: check-tools libriscv-build guest-elf libtcc-build
	@echo "we use vendored xendor/libriscv now, and do not pull from github."
	@echo ""
	@echo "  ✓ bench-setup complete — run 'make bench' to start"
	@echo ""

check-tools:
	@echo "── checking prerequisites  [$(PLATFORM)] ───────────────────────"
	@command -v $(GIT)    >/dev/null 2>&1 \
	    || { echo "  ✗ git not found"; exit 1; }
	@command -v $(CMAKE)  >/dev/null 2>&1 \
	    || { echo "  ✗ cmake not found"; \
	         echo "    macOS: brew install cmake"; \
	         echo "    Linux: apt install cmake"; exit 1; }
	@command -v $(GO)     >/dev/null 2>&1 \
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
	@echo "  ✓ go:     $$($(GO) version | awk '{print $$3, $$4}')"

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
	cd $(ROOT) && $(GO) test $(BENCH_FLAGS) \
	    -run='^$$' -bench='^BenchmarkGuestMem' \
	    ./bench/ 2>&1 | tee $(RESULTS_DIR)/ours.txt
	@echo ""
	@echo "── [2/2] libriscv (calibration) ────────────────────────────────"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test -tags libriscv $(BENCH_FLAGS) \
	        -run='^$$' -bench='^BenchmarkLibriscv' \
	        ./bench/libriscv/ 2>&1 | tee $(RESULTS_DIR)/libriscv.txt
	@$(MAKE) --no-print-directory bench-summary


bench-quick: bench-setup
	@mkdir -p $(RESULTS_DIR)
	@echo ""
	@echo "── quick benchmark ────────────────────────────────────────────"
	@echo ""
	@printf "  %-40s " "GuestMem Store64+Load64 pair:"
	@cd $(ROOT) && $(GO) test -count=1 -benchtime=100ms -benchmem \
	    -run='^$$' -bench='^BenchmarkGuestMem_Store64Load64Pair$$' \
	    ./bench/ 2>&1 | awk '/Benchmark/{printf "%-12s %s\n", $$3, $$4}'
	@echo "  libriscv (memory pair + execution throughput):"
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test -tags libriscv -count=1 -benchtime=100ms -benchmem \
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
	    $(GO) test -count=1 -benchtime=1x -benchmem \
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
	    $(GO) test -tags libriscv -count=1 -benchtime=100ms -benchmem \
	        -run='^$$' \
	        -bench='^BenchmarkLibriscv_MemWriteRead64$$|^BenchmarkLibriscv_FullExecution_Steady$$' \
	        ./bench/libriscv/ 2>&1


bench-cpu: guest-elf
	@echo "── Go CPU execution benchmark ─────────────────────────────────"
	cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU' \
	        ./bench/ 2>&1

bench-coremark: coremark-elf
	@echo "── CoreMark (cached vs uncached) ───────────────────────────────"
	cd $(ROOT) && CM_ELF=$(CM_ELF) \
	    $(GO) test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_CoreMark' \
	        ./bench/ 2>&1

bench-dhrystone: dhrystone-elf
	@echo "── Dhrystone (cached vs uncached) ──────────────────────────────"
	cd $(ROOT) && DHRY_ELF=$(DHRY_ELF) \
	    $(GO) test -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_Dhrystone' \
	        ./bench/ 2>&1

bench-ours:
	@echo "── our GuestMemory benchmarks ──────────────────────────────────"
	cd $(ROOT) && $(GO) test $(BENCH_FLAGS) \
	    -run='^$$' -bench='^BenchmarkGuestMem' \
	    ./bench/ 2>&1

bench-libriscv: bench-setup
	@echo "── libriscv calibration benchmarks ─────────────────────────────"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test -tags libriscv $(BENCH_FLAGS) \
	        -run='^$$' -bench='^BenchmarkLibriscv' \
	        ./bench/libriscv/ 2>&1

bench-mem: bench-setup
	@echo ""
	@echo "── memory pair head-to-head ────────────────────────────────────"
	@echo ""
	@echo "  [ours] GuestMemory Store64+Load64 pair"
	cd $(ROOT) && $(GO) test $(BENCH_FLAGS) \
	    -run='^$$' -bench='^BenchmarkGuestMem_Store64Load64Pair$$' \
	    ./bench/ 2>&1
	@echo ""
	@echo "  [libriscv] copy_to_guest+copy_from_guest pair"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test -tags libriscv $(BENCH_FLAGS) \
	        -run='^$$' -bench='^BenchmarkLibriscv_MemWriteRead64$$' \
	        ./bench/libriscv/ 2>&1

bench-smoke: bench-setup
	@echo "── smoke test ──────────────────────────────────────────────────"
	cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test -tags libriscv -v \
	        -run='^TestLibriscvSmokeTest$$' \
	        ./bench/libriscv/ 2>&1

# The number of riscv instructions retired during a
# run on compiled bench/libriscv_guest/bench_guest.c ;
# we use this number for the numerator in native 
# benchmarking for an apples-to-apples comparison.
NATIVE_RETIRED := 2524935201

bench:
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  JIT ALLOCATOR COMPARISON — $$(date '+%Y-%m-%d %H:%M')  [$(PLATFORM)]"
	@echo "  cpu: $(CPU_INFO)"
	@echo "══════════════════════════════════════════════════════════════════"
	@echo ""
	@printf "  %-44s %s\n" "Strategy" "MIPS"
	@printf "  %-44s %s\n" "────────────────────────────────────────────" "──────────"
	@printf "  %-44s " "Go interpreter (no JIT):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "Go JIT — ELS allocator (native):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution_JIT$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "Go JIT — Fixed Static Mapping (native):"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution_JIT_Fixed$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "Go JIT — TCC backend:"
	@cd $(ROOT) && BENCH_ELF=$(GUEST_ELF) CGO_ENABLED=1 \
	    $(GO) test $(PGO_FLAG) -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkCPU_FullExecution_JIT_TCC$$' \
	        ./bench/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "libriscv — JIT (TCC):"
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test $(PGO_FLAG) -tags libriscv -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkLibriscv_FullExecution_Steady$$' \
	        ./bench/libriscv/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "libriscv — interpreter (no JIT):"
	@cd $(ROOT) && \
	    CGO_CFLAGS="$(CGO_CFLAGS_VAL)" \
	    CGO_LDFLAGS="$(CGO_LDFLAGS_VAL)" \
	    BENCH_ELF=$(GUEST_ELF) \
	    $(GO) test $(PGO_FLAG) -tags libriscv -count=1 -benchtime=1x -benchmem \
	        -run='^$$' -bench='^BenchmarkLibriscv_FullExecution_NoJIT$$' \
	        ./bench/libriscv/ 2>&1 \
	    | awk '/MIPS/{for(i=1;i<=NF;i++){if($$i=="MIPS"){print p" MIPS";next}; p=$$i}}' \
	    || echo "(failed)"
	@printf "  %-44s " "native x86-64 (-O3 -march=native):"
	@elapsed=$$({ /usr/bin/time -p $(GUEST_NATIVE) >/dev/null; } 2>&1 | awk '/^real/{print $$NF}'); \
         awk "BEGIN{printf \"%.0f MIPS  (%.1f ms)\n\", $(NATIVE_RETIRED)/$$elapsed/1000000, $$elapsed*1000}"
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
	cd $(ROOT) && $(GO) test -count=1 -v . ./fuzzoracle 2>&1

# ── clean ──────────────────────────────────────────────────────────────────

clean:
	@echo "── cleaning ────────────────────────────────────────────────────"
	rm -rf $(BUILD) $(GUEST_ELF) $(RESULTS_DIR)
	@echo "  ✓ done"

# ── fuzz oracle targets (fuzzoracle package, CGO always on) ───────────────
# Requires: make bench-setup

FUZZ_ORACLE_CGO_LDFLAGS := -L$(BUILD) -L$(BUILD)/libriscv \
                            -lriscv_capi -lriscv -lstdc++ -lm $(EXTRA_LDFLAGS)

.PHONY: fuzz-oracle fuzz-stores fuzz-rvc fuzz-amo fuzz-fd fuzz-bitmanip fuzz-cfloat
fuzz-oracle: bench-setup
	@echo "── fuzz ALU vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
	        -run FuzzALUVsLibriscv -fuzz=FuzzALUVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

fuzz-stores: bench-setup
	@echo "── fuzz loads/stores vs libriscv oracle ($(FUZZ_TIME)) ─────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
	        -run FuzzStoresVsLibriscv -fuzz=FuzzStoresVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1


.PHONY: fuzz-rvc
fuzz-rvc: bench-setup
	@echo "── fuzz RVC vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
	        -run FuzzRVCVsLibriscv -fuzz=FuzzRVCVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-amo
fuzz-amo: bench-setup
	@echo "── fuzz AMO vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
	        -run FuzzAMOVsLibriscv -fuzz=FuzzAMOVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-fd
fuzz-fd: bench-setup
	@echo "── fuzz F+D vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
	        -run FuzzFDVsLibriscv -fuzz=FuzzFDVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-bitmanip
fuzz-bitmanip: bench-setup
	@echo "── fuzz Zicsr/Zba/Zbb/Zbs vs libriscv oracle ($(FUZZ_TIME)) ─"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
	        -run FuzzBitmanipVsLibriscv -fuzz=FuzzBitmanipVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

.PHONY: fuzz-cfloat
fuzz-cfloat: bench-setup
	@echo "── fuzz C.FLD/FSD/FLDSP/FSDSP vs libriscv oracle ($(FUZZ_TIME)) "
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    $(GO) test \
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
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzCPU -fuzz=FuzzCPU \
	    -fuzztime=$(FUZZ_TIME) \
	    . 2>&1

# fuzz-all: run every fuzz target sequentially, 4h each by default.
# Usage:
#   make fuzz-all                    # 4h per target (19 targets ≈ 76h)
#   make fuzz-all FUZZ_LONG_TIME=1h  # 1h per target
#   nohup make fuzz-all &            # background overnight
.PHONY: fuzz-all
fuzz-all:
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  FUZZ ALL — $(FUZZ_LONG_TIME) per target — $$(date)"
	@echo "══════════════════════════════════════════════════════════════════"
	@echo ""
	@echo "── [1/19] FuzzCPU ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzCPU -fuzz=FuzzCPU \
	    -fuzztime=$(FUZZ_LONG_TIME) . 2>&1 || true
	@echo ""
	@echo "── [2/19] FuzzPeepholeTermination ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzPeepholeTermination -fuzz=FuzzPeepholeTermination \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [3/19] FuzzEmitterSequences ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzEmitterSequences -fuzz=FuzzEmitterSequences \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [4/19] FuzzBlockStructure ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzBlockStructure -fuzz=FuzzBlockStructure \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [5/19] FuzzHighLevelHelpers ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzHighLevelHelpers -fuzz=FuzzHighLevelHelpers \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [6/19] FuzzRegAllocInvariants ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzRegAllocInvariants -fuzz=FuzzRegAllocInvariants \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [7/19] FuzzLiveRangeConsistency ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzLiveRangeConsistency -fuzz=FuzzLiveRangeConsistency \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [8/19] FuzzSpillResurrection ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzSpillResurrection -fuzz=FuzzSpillResurrection \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [9/19] FuzzELS_NoConflicts ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzELS_NoConflicts -fuzz=FuzzELS_NoConflicts \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [10/19] FuzzELS_ForwardBranchLiveness ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzELS_ForwardBranchLiveness -fuzz=FuzzELS_ForwardBranchLiveness \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [11/19] FuzzELS_GuestRegExtension ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzELS_GuestRegExtension -fuzz=FuzzELS_GuestRegExtension \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [12/19] FuzzELS_SpillSlotUniqueness ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && $(GO) test \
	    -run FuzzELS_SpillSlotUniqueness -fuzz=FuzzELS_SpillSlotUniqueness \
	    -fuzztime=$(FUZZ_LONG_TIME) ./ir/ 2>&1 || true
	@echo ""
	@echo "── [13/19] FuzzALUVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzALUVsLibriscv -fuzz=FuzzALUVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [14/19] FuzzStoresVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzStoresVsLibriscv -fuzz=FuzzStoresVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [15/19] FuzzRVCVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzRVCVsLibriscv -fuzz=FuzzRVCVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [16/19] FuzzAMOVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzAMOVsLibriscv -fuzz=FuzzAMOVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [17/19] FuzzFDVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzFDVsLibriscv -fuzz=FuzzFDVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [18/19] FuzzCFloatVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzCFloatVsLibriscv -fuzz=FuzzCFloatVsLibriscv \
	    -fuzztime=$(FUZZ_LONG_TIME) ./fuzzoracle/ 2>&1 || true
	@echo ""
	@echo "── [19/19] FuzzBitmanipVsLibriscv ($(FUZZ_LONG_TIME)) ──"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
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


# ── libtcc.a build ─────────────────────────────────────────────────────────
# Reuses the TCC source already fetched by libriscv's CMake FetchContent at
# $(BUILD)/_deps/tinycc-src. No additional clone needed.

TCC_SRC_DIR := $(BUILD)/_deps/tinycc-src
TCC_ARCHIVE := $(ROOT)xendor/tcc/libtcc.a
TCC_HDR_DST := $(ROOT)xendor/tcc/libtcc.h

# Target macros: keep in sync with xendor/libriscv/lib/CMakeLists.txt:255-288
ifeq ($(PLATFORM),macos)
  TCC_TARGET_DEFS := -DTARGETOS_Darwin=1 -DTCC_TARGET_MACHO=1
  ifeq ($(shell uname -m),arm64)
    TCC_TARGET_DEFS += -DTCC_TARGET_ARM64=1
  else
    TCC_TARGET_DEFS += -DTCC_TARGET_X86_64=1
  endif
else
  TCC_TARGET_DEFS := -DTARGETOS_Linux=1
  ifeq ($(shell uname -m),aarch64)
    TCC_TARGET_DEFS += -DTCC_TARGET_ARM64=1
  else
    TCC_TARGET_DEFS += -DTCC_TARGET_X86_64=1
  endif
endif

TCC_CFLAGS := -O2 -fPIC \
              -DCONFIG_TCC_PREDEFS=1 -DTCC_VERSION=\"0.9.27\" \
              -DTCC_IS_NATIVE=1 -DCONFIG_TCC_BACKTRACE=0 -DCONFIG_TCC_BCHECK=0 \
              $(TCC_TARGET_DEFS)

libtcc-build: $(TCC_ARCHIVE)
$(TCC_ARCHIVE): $(LIB_CAPI)
	@echo "── building xendor/tcc/libtcc.a  [$(PLATFORM)/$(shell uname -m)] ──"
	@test -f $(TCC_SRC_DIR)/libtcc.c || { echo "  ✗ $(TCC_SRC_DIR)/libtcc.c not found — run 'make libriscv-build' first"; exit 1; }
	cc $(TCC_CFLAGS) -I$(TCC_SRC_DIR) -c $(TCC_SRC_DIR)/libtcc.c -o $(TCC_SRC_DIR)/libtcc.o
	ar rcs $(TCC_ARCHIVE) $(TCC_SRC_DIR)/libtcc.o
	cp $(TCC_SRC_DIR)/libtcc.h $(TCC_HDR_DST)
	@echo "  ✓ $(TCC_ARCHIVE): $$(du -h $(TCC_ARCHIVE) | cut -f1)"
	@echo "  ✓ $$(file $(TCC_ARCHIVE) | cut -d: -f2- | xargs)"

darwin-perf: kpc_perf/kpc_perf.c
	cd kpc_perf && clang -O2 -o ../bench/darwin-perf kpc_perf.c -framework CoreFoundation && cd ..
	cd bench && go test -c
	cd bench && sudo ./darwin-perf ./bench.test -test.v -test.count=1 -test.benchtime=1x -test.benchmem  -test.run=xxx -test.bench='^BenchmarkCPU_FullExecution$$' 

linux-perf:
	cd bench && go test -c && sudo ./perf_l1.sh ./bench.test -test.v -test.count=1 -test.benchtime=1x -test.benchmem -test.run=xxx -test.bench='^BenchmarkCPU_FullExecution$$'

