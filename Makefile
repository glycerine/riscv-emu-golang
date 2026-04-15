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
#   make clean          # remove vendor/ and generated ELF
#   make help           # this message

.PHONY: all help bench-setup bench bench-quick bench-ours bench-libriscv bench-mem \
        bench-smoke bench-summary test clean check-tools \
        libriscv-clone libriscv-patch libriscv-build guest-elf

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
ZIG_TARGET := riscv64-linux-musl
GUEST_CFLAGS := -O2 -target $(ZIG_TARGET) -static

# ── paths ──────────────────────────────────────────────────────────────────

ROOT        := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
VENDOR      := $(ROOT)vendor/libriscv
BUILD       := $(VENDOR)/build_capi
LIB_CAPI    := $(BUILD)/libriscv_capi.a
LIB_CORE    := $(BUILD)/libriscv/libriscv.a
GUEST_DIR   := $(ROOT)bench/libriscv_guest
GUEST_SRC   := $(GUEST_DIR)/bench_guest.c
GUEST_ELF   := $(GUEST_DIR)/bench_guest.elf
RESULTS_DIR := /tmp/riscv-bench
PATCH_STAMP := $(VENDOR)/.patched

# ── tools ──────────────────────────────────────────────────────────────────

GO            ?= go
CMAKE         ?= cmake
GIT           ?= git
LIBRISCV_REPO ?= https://github.com/libriscv/libriscv.git
LIBRISCV_REF  ?= master

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
	@echo "    make bench-quick      fast head-to-head (<1s)"
	@echo "    make bench            full comparison (ours + libriscv)"
	@echo "    make bench-ours       our GuestMemory only (no libriscv needed)"
	@echo "    make bench-libriscv   libriscv calibration only"
	@echo "    make bench-mem        memory pair head-to-head only"
	@echo "    make bench-smoke      quick sanity check (~3s)"
	@echo ""
	@echo "  Other:"
	@echo "    make test             unit tests"
	@echo "    make clean            remove vendor/ and generated ELF"
	@echo ""
	@echo "  Overrides:"
	@echo "    ZIG_CC=$(ZIG_CC)  BENCH_COUNT=$(BENCH_COUNT)  BENCH_TIME=$(BENCH_TIME)"
	@echo ""

# ── setup pipeline ─────────────────────────────────────────────────────────

bench-setup: check-tools libriscv-clone libriscv-patch libriscv-build guest-elf
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

libriscv-clone: $(VENDOR)/.git
$(VENDOR)/.git:
	@echo "── cloning libriscv ────────────────────────────────────────────"
	@mkdir -p $(dir $(VENDOR))
	$(GIT) clone --depth=1 $(LIBRISCV_REPO) $(VENDOR) 2>&1 | sed 's/^/  /'
	@echo "  ✓ cloned into $(VENDOR)"

# ── libriscv patch ─────────────────────────────────────────────────────────
# macOS SDK defines stdout as a macro (__stdoutp) in <stdio.h>.
# libriscv's RISCVOptions struct has a field named 'stdout' which clashes.
# We rename it to 'output' in both the header and implementation.

libriscv-patch: $(PATCH_STAMP)
$(PATCH_STAMP): $(VENDOR)/.git
	@echo "── patching libriscv (stdout→output field rename) ──────────────"
	sed -i.bak \
	    's/riscv_stdout_func_t stdout;/riscv_stdout_func_t output; \/\* renamed: stdout is a macro on macOS \*\//' \
	    $(VENDOR)/c/libriscv.h
	sed -i.bak \
	    's/riscv_stdout_func_t stdout = nullptr;/riscv_stdout_func_t output = nullptr;/' \
	    $(VENDOR)/c/libriscv.cpp
	sed -i.bak \
	    's/\.stdout = options->stdout,/.output = options->output,/' \
	    $(VENDOR)/c/libriscv.cpp
	sed -i.bak \
	    's/userdata\.stdout/userdata.output/g' \
	    $(VENDOR)/c/libriscv.cpp
	@grep -q '\.stdout\b' $(VENDOR)/c/libriscv.h $(VENDOR)/c/libriscv.cpp \
	    && { echo "  ✗ patch incomplete"; exit 1; } || true
	@# Fix Intel Mac: libriscv CMake assumes all macOS == ARM64 (Apple Silicon).
	@# On Intel Macs (x86_64) we must patch CMakeLists.txt to use TCC_TARGET_X86_64.
ifeq ($(PLATFORM),macos)
	@if [ "$$(uname -m)" = "x86_64" ]; then \
	    echo "  ✓ Intel Mac: patching libtcc target to x86_64"; \
	    awk '/CMAKE_HOST_APPLE OR APPLE/{in_apple=1} in_apple && /TCC_TARGET_ARM64=1/{sub(/TCC_TARGET_ARM64=1/,"TCC_TARGET_X86_64=1");in_apple=0} {print}' \
	        $(VENDOR)/lib/CMakeLists.txt > $(VENDOR)/lib/CMakeLists.txt.tmp \
	    && mv $(VENDOR)/lib/CMakeLists.txt.tmp $(VENDOR)/lib/CMakeLists.txt; \
	else \
	    echo "  ✓ Apple Silicon: TCC_TARGET_ARM64 correct"; \
	fi
endif
	@touch $(PATCH_STAMP)
	@echo "  ✓ patch applied"

# ── libriscv build ─────────────────────────────────────────────────────────

libriscv-build: $(LIB_CAPI)
$(LIB_CAPI): $(PATCH_STAMP)
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
	$(ZIG_CC) cc $(GUEST_CFLAGS) -o $(GUEST_ELF) $(GUEST_SRC)
	@test -f $(GUEST_ELF) || { echo "  ✗ guest ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(GUEST_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(GUEST_ELF) | cut -f1)"

# ── benchmark targets ──────────────────────────────────────────────────────

bench: bench-setup
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
	@echo "── quick benchmark  [<1s total] ────────────────────────────────"
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
	        -bench='^BenchmarkLibriscv_MemWriteRead64$$|^BenchmarkLibriscv_FullExecution_Steady$$' \
	        ./bench/libriscv/ 2>&1 \
	    | awk ' \
	        /MemWriteRead64/ { \
	            for(i=1;i<=NF;i++) if($(i)=="ns/pair") printf "  %-40s %s ns/pair\n","libriscv copy_to+from_guest:",$(i-1) \
	        } \
	        /FullExecution_Steady/ { \
	            for(i=1;i<=NF;i++) if($(i)=="MIPS") printf "  %-40s %s MIPS\n","libriscv full execution:",$(i-1) \
	        } \
	    '
	@echo ""

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
	cd $(ROOT) && $(GO) test -count=1 . ./bench 2>&1

# ── clean ──────────────────────────────────────────────────────────────────

clean:
	@echo "── cleaning ────────────────────────────────────────────────────"
	rm -rf $(VENDOR) $(GUEST_ELF) $(RESULTS_DIR)
	@echo "  ✓ done"

# ── fuzz oracle targets (fuzzoracle package, CGO always on) ───────────────
# Requires: make bench-setup

FUZZ_ORACLE_CGO_LDFLAGS := -L$(BUILD) -L$(BUILD)/libriscv \
                            -lriscv_capi -lriscv -lstdc++ -lm $(EXTRA_LDFLAGS)

.PHONY: fuzz-oracle fuzz-stores
fuzz-oracle: bench-setup
	@echo "── fuzz ALU vs libriscv oracle ($(FUZZ_TIME)) ──────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    CGO_LDFLAGS="$(FUZZ_ORACLE_CGO_LDFLAGS)" \
	    $(GO) test \
	        -run FuzzALUVsLibriscv -fuzz=FuzzALUVsLibriscv \
	        -fuzztime=$(FUZZ_TIME) \
	        ./fuzzoracle/ 2>&1

fuzz-stores: bench-setup
	@echo "── fuzz loads/stores vs libriscv oracle ($(FUZZ_TIME)) ─────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 \
	    CGO_LDFLAGS="$(FUZZ_ORACLE_CGO_LDFLAGS)" \
	    $(GO) test \
	        -run FuzzStoresVsLibriscv -fuzz=FuzzStoresVsLibriscv \
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

.PHONY: fuzz
fuzz:
	@echo "── fuzzing CPU ($(FUZZ_TIME)) ──────────────────────────────────"
	cd $(ROOT) && FUZZ_TIMEOUT=1 $(GO) test \
	    -run FuzzCPU -fuzz=FuzzCPU \
	    -fuzztime=$(FUZZ_TIME) \
	    . 2>&1
