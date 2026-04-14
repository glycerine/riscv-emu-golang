# Makefile — RISC-V emulator benchmark harness
#
# Manages:
#   - libriscv source acquisition and static library build
#   - RISC-V guest ELF cross-compilation
#   - Go benchmark execution (GuestMemory vs libriscv)
#
# Quick start:
#   make bench-setup    # one-time setup
#   make bench          # run full comparison
#
# Other targets:
#   make bench-ours     # our GuestMemory benchmarks only (no libriscv needed)
#   make bench-libriscv # libriscv calibration benchmarks only
#   make bench-mem      # head-to-head memory pair benchmark only
#   make bench-smoke    # quick smoke test
#   make test           # unit tests
#   make clean          # remove vendor/ and generated ELF
#   make help           # this message
#
# Overrideable variables:
#   BENCH_COUNT   repetitions per benchmark (default 5)
#   BENCH_TIME    minimum time per benchmark (default 2s)
#   RISCV_CC      RISC-V cross compiler (auto-detected)
#   LIBRISCV_REPO libriscv git URL
#   LIBRISCV_REF  git branch/tag/commit

.PHONY: all help bench-setup bench bench-ours bench-libriscv bench-mem \
        bench-smoke bench-summary test clean check-tools \
        libriscv-clone libriscv-build guest-elf

# ── platform detection ─────────────────────────────────────────────────────

UNAME_S := $(shell uname -s)

ifeq ($(UNAME_S),Darwin)
  PLATFORM  := macos
  # Homebrew core formula — pre-built bottle, installs in seconds:
  #   brew install riscv64-elf-gcc
  # Produces: riscv64-elf-gcc (bare-metal ELF target, no Linux ABI)
  RISCV_CC      ?= riscv64-elf-gcc
  RISCV_INSTALL := brew install cmake riscv64-elf-gcc
  NPROC         := $(shell sysctl -n hw.logicalcpu)
  CPU_INFO      := $(shell sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)
  # pthreads is in libc on macOS — no -lpthread needed
  EXTRA_LDFLAGS :=
else
  PLATFORM  := linux
  RISCV_CC      ?= riscv64-linux-gnu-gcc
  RISCV_INSTALL := apt install cmake g++ gcc-riscv64-linux-gnu libc6-dev-riscv64-cross
  NPROC         := $(shell nproc)
  CPU_INFO      := $(shell grep 'model name' /proc/cpuinfo 2>/dev/null \
                       | head -1 | cut -d: -f2 | xargs || echo unknown)
  EXTRA_LDFLAGS := -lpthread
endif

# ── guest ELF compiler flags ───────────────────────────────────────────────
#
# Both builds use -nostartfiles so our _start in bench_guest.c is the sole
# entry point — no conflict with crt1.o on Linux, no assumption about
# libc on macOS bare-metal.
#
# macOS (riscv64-elf-gcc, bare-metal newlib):
#   -march=rv64imac -mabi=lp64  (no D extension in default newlib multilib)
#   -nostdlib  (no newlib, no libgcc startup)
#
# Linux (riscv64-linux-gnu-gcc, glibc cross):
#   -march=rv64imafd -mabi=lp64d  (double-float, matches libriscv pre-built ELFs)
#   -static  (self-contained binary)

ifeq ($(PLATFORM),macos)
  GUEST_MARCH  := rv64imac
  GUEST_ABI    := lp64
  GUEST_CFLAGS := -O2 -march=$(GUEST_MARCH) -mabi=$(GUEST_ABI) \
                  -nostdlib -nostartfiles -Wl,--gc-sections
else
  GUEST_MARCH  := rv64imafd
  GUEST_ABI    := lp64d
  GUEST_CFLAGS := -O2 -march=$(GUEST_MARCH) -mabi=$(GUEST_ABI) \
                  -static -nostartfiles
endif

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

# ── tools ──────────────────────────────────────────────────────────────────

GO            ?= go
CMAKE         ?= cmake
GIT           ?= git
LIBRISCV_REPO ?= https://github.com/libriscv/libriscv.git
LIBRISCV_REF  ?= master

# ── cgo environment ────────────────────────────────────────────────────────
# Go's security policy forbids ${SRCDIR}-relative paths in #cgo LDFLAGS,
# so library paths are supplied exclusively via CGO_LDFLAGS env var.

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
	@echo "    brew install cmake riscv64-elf-gcc"
	@echo "    # go from https://go.dev/dl/ if not already installed"
else
	@echo "    apt install cmake g++ gcc-riscv64-linux-gnu libc6-dev-riscv64-cross"
endif
	@echo ""
	@echo "  Setup (run once after installing prerequisites):"
	@echo "    make bench-setup"
	@echo ""
	@echo "  Benchmarks:"
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
	@echo "    BENCH_COUNT=$(BENCH_COUNT)  BENCH_TIME=$(BENCH_TIME)  RISCV_CC=$(RISCV_CC)"
	@echo ""

# ── setup pipeline ─────────────────────────────────────────────────────────

bench-setup: check-tools libriscv-clone libriscv-build guest-elf
	@echo ""
	@echo "  ✓ bench-setup complete — run 'make bench' to start"
	@echo ""

check-tools:
	@echo "── checking prerequisites  [$(PLATFORM)] ───────────────────────"
	@command -v $(GIT)       >/dev/null 2>&1 \
	    || { echo "  ✗ git not found"; exit 1; }
	@command -v $(CMAKE)     >/dev/null 2>&1 \
	    || { echo "  ✗ cmake not found  →  $(RISCV_INSTALL)"; exit 1; }
	@command -v $(GO)        >/dev/null 2>&1 \
	    || { echo "  ✗ go not found  →  https://go.dev/dl/"; exit 1; }
	@# C++ compiler: clang++ ships with Xcode CLT on macOS; g++ on Linux
	@command -v clang++ >/dev/null 2>&1 \
	  || command -v g++  >/dev/null 2>&1 \
	  || { echo "  ✗ no C++ compiler"; \
	       echo "    macOS: xcode-select --install"; \
	       echo "    Linux: apt install g++"; exit 1; }
	@command -v $(RISCV_CC)  >/dev/null 2>&1 \
	    || { echo "  ✗ $(RISCV_CC) not found"; \
	         echo "    Install: $(RISCV_INSTALL)"; exit 1; }
	@echo "  ✓ git:      $$(git --version | cut -d' ' -f3)"
	@echo "  ✓ cmake:    $$(cmake --version | head -1 | cut -d' ' -f3)"
	@echo "  ✓ c++:      $$(clang++ --version 2>/dev/null | head -1 \
	                   || g++ --version | head -1)"
	@echo "  ✓ riscv-cc: $$($(RISCV_CC) --version | head -1)"
	@echo "  ✓ go:       $$($(GO) version | awk '{print $$3, $$4}')"

# ── libriscv ───────────────────────────────────────────────────────────────

libriscv-clone: $(VENDOR)/.git
$(VENDOR)/.git:
	@echo "── cloning libriscv ────────────────────────────────────────────"
	@mkdir -p $(dir $(VENDOR))
	$(GIT) clone --depth=1 $(LIBRISCV_REPO) $(VENDOR) 2>&1 | sed 's/^/  /'
	@echo "  ✓ cloned into $(VENDOR)"

libriscv-build: $(LIB_CAPI)
$(LIB_CAPI): $(VENDOR)/.git
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
	@test -f $(LIB_CAPI) \
	    || { echo "  ✗ libriscv_capi.a not built"; exit 1; }
	@test -f $(LIB_CORE) \
	    || { echo "  ✗ libriscv.a not built"; exit 1; }
	@echo "  ✓ libriscv_capi.a: $$(du -h $(LIB_CAPI) | cut -f1)"
	@echo "  ✓ libriscv.a:      $$(du -h $(LIB_CORE) | cut -f1)"

# ── guest ELF ──────────────────────────────────────────────────────────────

guest-elf: $(GUEST_ELF)
$(GUEST_ELF): $(GUEST_SRC)
	@echo "── compiling RISC-V guest ELF ──────────────────────────────────"
	@echo "  arch=$(GUEST_MARCH)  abi=$(GUEST_ABI)"
	$(RISCV_CC) $(GUEST_CFLAGS) -o $(GUEST_ELF) $(GUEST_SRC)
	@test -f $(GUEST_ELF) \
	    || { echo "  ✗ guest ELF not produced"; exit 1; }
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
	cd $(ROOT) && $(GO) test -count=1 ./... 2>&1

# ── clean ──────────────────────────────────────────────────────────────────

clean:
	@echo "── cleaning ────────────────────────────────────────────────────"
	rm -rf $(VENDOR) $(GUEST_ELF) $(RESULTS_DIR)
	@echo "  ✓ done"
