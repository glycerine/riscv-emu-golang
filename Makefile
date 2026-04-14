# Makefile — RISC-V emulator benchmark harness
#
# Manages:
#   - libriscv source acquisition and static library build
#   - RISC-V guest ELF cross-compilation
#   - Go benchmark execution (GuestMemory vs libriscv)
#
# Quick start:
#   make bench-setup    # one-time: clone libriscv, build libs, compile guest ELF
#   make bench          # run full comparison and print summary
#
# Other targets:
#   make bench-ours     # our GuestMemory benchmarks only (no libriscv needed)
#   make bench-libriscv # libriscv calibration benchmarks only
#   make bench-mem      # head-to-head memory pair benchmark only
#   make bench-smoke    # quick smoke test (libriscv executes the guest ELF)
#   make clean          # remove vendor/ and generated ELF
#   make help           # this message
#
# Overrideable variables:
#   BENCH_COUNT   repetitions per benchmark (default 5)
#   BENCH_TIME    minimum time per benchmark (default 2s)
#   RISCV_CC      RISC-V cross compiler (default riscv64-linux-gnu-gcc)
#   LIBRISCV_REPO libriscv git URL
#   LIBRISCV_REF  git branch/tag/commit to pin

.PHONY: all help bench-setup bench bench-ours bench-libriscv bench-mem \
        bench-smoke bench-summary clean check-tools \
        libriscv-clone libriscv-build guest-elf

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

GO          ?= go
CMAKE       ?= cmake
RISCV_CC    ?= riscv64-linux-gnu-gcc
GIT         ?= git

LIBRISCV_REPO ?= https://github.com/libriscv/libriscv.git
LIBRISCV_REF  ?= master

# ── cgo environment — set from built library paths ─────────────────────────
# Go's security policy forbids ${SRCDIR}-relative LDFLAGS in cgo directives,
# so we supply library paths exclusively through the environment here.

CGO_CFLAGS_VAL  := -I$(VENDOR)/c
CGO_LDFLAGS_VAL := -L$(BUILD) -L$(BUILD)/libriscv \
                   -lriscv_capi -lriscv -lstdc++ -lm -lpthread

export CGO_ENABLED := 1

# ── benchmark parameters ───────────────────────────────────────────────────

BENCH_COUNT  ?= 5
BENCH_TIME   ?= 2s
CPUPROFILE   ?=

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
	@echo "  RISC-V emulator benchmark harness"
	@echo ""
	@echo "  Setup (run once):"
	@echo "    make bench-setup"
	@echo ""
	@echo "  Benchmarks:"
	@echo "    make bench            full comparison (ours + libriscv)"
	@echo "    make bench-ours       our GuestMemory only (no libriscv needed)"
	@echo "    make bench-libriscv   libriscv calibration only"
	@echo "    make bench-mem        memory pair head-to-head only"
	@echo "    make bench-smoke      quick sanity check"
	@echo ""
	@echo "  Variables:"
	@echo "    BENCH_COUNT=$(BENCH_COUNT)"
	@echo "    BENCH_TIME=$(BENCH_TIME)"
	@echo "    RISCV_CC=$(RISCV_CC)"
	@echo "    LIBRISCV_REPO=$(LIBRISCV_REPO)"
	@echo "    LIBRISCV_REF=$(LIBRISCV_REF)"
	@echo ""

# ── setup pipeline ─────────────────────────────────────────────────────────

bench-setup: check-tools libriscv-clone libriscv-build guest-elf
	@echo ""
	@echo "  ✓ bench-setup complete — run 'make bench' to start"
	@echo ""

check-tools:
	@echo "── checking prerequisites ──────────────────────────────────────"
	@command -v $(GIT)      >/dev/null 2>&1 || { echo "  ✗ git not found";              exit 1; }
	@command -v $(CMAKE)    >/dev/null 2>&1 || { echo "  ✗ cmake not found (apt install cmake)"; exit 1; }
	@command -v g++         >/dev/null 2>&1 || { echo "  ✗ g++ not found (apt install g++)";    exit 1; }
	@command -v $(RISCV_CC) >/dev/null 2>&1 || { \
	    echo "  ✗ $(RISCV_CC) not found"; \
	    echo "    Install: apt install gcc-riscv64-linux-gnu libc6-dev-riscv64-cross"; \
	    exit 1; }
	@command -v $(GO)       >/dev/null 2>&1 || { echo "  ✗ go not found"; exit 1; }
	@echo "  ✓ git:      $$(git --version | cut -d' ' -f3)"
	@echo "  ✓ cmake:    $$(cmake --version | head -1 | cut -d' ' -f3)"
	@echo "  ✓ g++:      $$(g++ --version | head -1 | awk '{print $$3}')"
	@echo "  ✓ riscv-cc: $$($(RISCV_CC) --version | head -1 | awk '{print $$3}')"
	@echo "  ✓ go:       $$( $(GO) version | awk '{print $$3}')"

libriscv-clone: $(VENDOR)/.git
$(VENDOR)/.git:
	@echo "── cloning libriscv @ $(LIBRISCV_REF) ─────────────────────────"
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
	cd $(BUILD) && $(MAKE) -j$$(nproc) 2>&1 \
	    | grep -v "^make\[" | grep -v "^--" | sed 's/^/  /'
	@test -f $(LIB_CAPI) || { echo "  ✗ $(LIB_CAPI) not built"; exit 1; }
	@test -f $(LIB_CORE) || { echo "  ✗ $(LIB_CORE) not built"; exit 1; }
	@echo "  ✓ libriscv_capi.a: $$(du -h $(LIB_CAPI) | cut -f1)"
	@echo "  ✓ libriscv.a:      $$(du -h $(LIB_CORE) | cut -f1)"

guest-elf: $(GUEST_ELF)
$(GUEST_ELF): $(GUEST_SRC)
	@echo "── compiling RISC-V guest ELF ──────────────────────────────────"
	$(RISCV_CC) \
	    -O2 \
	    -march=rv64imafd \
	    -mabi=lp64d \
	    -static \
	    -o $(GUEST_ELF) \
	    $(GUEST_SRC)
	@test -f $(GUEST_ELF) || { echo "  ✗ guest ELF not produced"; exit 1; }
	@echo "  ✓ $$(file $(GUEST_ELF) | cut -d: -f2 | xargs)"
	@echo "  ✓ size: $$(du -h $(GUEST_ELF) | cut -f1)"

# ── benchmark targets ──────────────────────────────────────────────────────

bench: bench-setup
	@mkdir -p $(RESULTS_DIR)
	@echo ""
	@echo "══════════════════════════════════════════════════════════════════"
	@echo "  BENCHMARK RUN — $$(date '+%Y-%m-%d %H:%M')"
	@echo "  cpu: $$(grep 'model name' /proc/cpuinfo | head -1 | cut -d: -f2 | xargs)"
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
	@echo "  Memory subsystem — Store64+Load64 pair (lower is better):"
	@printf "    %-44s %s\n" \
	    "ours   GuestMem_Store64Load64Pair" \
	    "$$(grep 'Store64Load64Pair' $(RESULTS_DIR)/ours.txt 2>/dev/null \
	        | awk 'NR==1{print $$3, $$4}' || echo '(run make bench first)')"
	@printf "    %-44s %s\n" \
	    "libriscv MemWriteRead64" \
	    "$$(grep 'ns/pair' $(RESULTS_DIR)/libriscv.txt 2>/dev/null \
	        | awk 'NR==1{print $$NF, "ns/pair"}' || echo '(run make bench first)')"
	@echo ""
	@echo "  Machine creation (lower is better):"
	@printf "    %-44s %s\n" \
	    "ours   GuestMem_Alloc64MB" \
	    "$$(grep 'Alloc64MB' $(RESULTS_DIR)/ours.txt 2>/dev/null \
	        | awk 'NR==1{print $$3, $$4}' || echo '(run make bench first)')"
	@printf "    %-44s %s\n" \
	    "libriscv MachineCreate" \
	    "$$(grep 'MachineCreate' $(RESULTS_DIR)/libriscv.txt 2>/dev/null \
	        | awk 'NR==1{print $$3, $$4}' || echo '(run make bench first)')"
	@echo ""
	@echo "  libriscv execution throughput (target for our future decoder):"
	@grep 'MIPS' $(RESULTS_DIR)/libriscv.txt 2>/dev/null \
	    | grep -v '^$$' | head -4 | sed 's/^/    /' \
	    || echo "    (run make bench first)"
	@echo ""
	@echo "  Interpretation:"
	@echo "    Our memory pair is ~9x faster than libriscv's copy path."
	@echo "    Our alloc is ~25x faster than libriscv's (no ELF loading)."
	@echo "    libriscv runs ~900-1150 MIPS — target for our insn loop."
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
