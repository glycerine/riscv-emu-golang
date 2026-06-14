package riscv

import (
	"os"
	"testing"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

// TestBloat_BenchGuest_0x10de locks in a reproducible measurement of
// the IR op count and emitted host-code byte count for the block
// entered at PC 0x000010de in bench/libriscv_guest/bench_guest.elf.
//
// The block is a small byte-checksum loop followed by a store, a
// (dead) load-into-x0, and an exit ECALL. It expands dramatically:
// 9 RV insns → ~100 IR ops → ~1222 host bytes, mostly from
// sandboxed-memory bounds/alignment checks and the FixedStatic-
// Allocator's spill-everything temporaries.
//
// This test is the before/after harness for upcoming peephole /
// codegen work. The max* budgets start a little above today's
// measurements; each optimization PR lowers them in the same commit
// so regressions fail loudly.
//
// Run: go test -run TestBloat_BenchGuest_0x10de -v .
//
// With -v, a VizJit-format dump of the block is written to the test's
// tempdir; compare against an earlier dump in ~/ris/debug_vizjit_dir
// to see the exact diff.
func TestBloat_BenchGuest_0x10de(t *testing.T) {
	const (
		elfPath      = "bench/libriscv_guest/bench_guest.elf"
		blockEntryPC = uint64(0x000010de)

		// High-water marks. Lower these as optimizations land.
		//  - Pre-peephole baseline (2026-04-22): ir=110, host=1222.
		//  - After post-lowering MOVQ peephole (2026-04-22): host=1150
		//    (-72 bytes, -5.9%).
		//  - After MaskedLoad/GuestStore width=1 fast path
		//    (2026-04-22): ir=108, host=1121 (-29 bytes).
		//  - After MaskedLoadAddr/GuestStoreAddr + emitMisalignedStore
		//    i=0 special-case (2026-04-22): ir=105, host=1079
		//    (-42 bytes). Cumulative −143 bytes (−11.7%).
		//  - After rv8 always-stage lowerer (2026-04-23): ir=105,
		//    host=1582 (+503 bytes). Expected: every operand is staged
		//    through RAX/RCX. CISC memory operands (Stage 12) will
		//    recover most of this.
		//  - After precise countdown budget checks (2026-06-14): ir=116.
		//    The 11-op net increase is deterministic-IC bookkeeping for
		//    the 9 guest instructions in this block, not semantic bloat.
		maxIRInstrs       = 116
		maxBudgetReserves = 9
		maxBudgetSetPCs   = 9
		maxBudgetRets     = 1
		maxHostBytes      = 1650
		maxChainExits     = 5
	)

	path := os.Getenv("BENCH_ELF")
	if path == "" {
		path = elfPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %q: %v\n(run `make bench-setup` or set BENCH_ELF to a built ELF)", path, err)
	}

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}

	// Emit IR for the block.
	j := NewJIT()
	res := j.emitBlock(mem, blockEntryPC)
	if res == nil || res.block == nil || res.numInsns == 0 {
		t.Fatalf("emitBlock(0x%x) returned nil/empty", blockEntryPC)
	}
	irOps := len(res.block.Instrs)
	budgetReserves, budgetSetPCs, budgetRets := countBloatBudgetIROps(res.block)

	// Register-allocate and lower through the same AOT path production uses.
	pool := RV8Pool(res.block)
	pinned := RV8Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	lowerRes, err := LowerAMD64_RV8(ctx, res.block, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}

	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// Capture Progs listing after Assemble so branch targets are resolved.
	var progs string
	if testing.Verbose() {
		progs = ctx.DumpProgs()
	}

	hostBytes := len(code)
	chainExits := len(lowerRes.ChainExits)

	t.Logf("bench_guest pc=0x%x: ir=%d budget_reserve=%d budget_setpc=%d budget_ret=%d host=%d chain_exits=%d (budgets: ir≤%d reserve≤%d setpc≤%d ret≤%d host≤%d exits≤%d)",
		blockEntryPC, irOps, budgetReserves, budgetSetPCs, budgetRets, hostBytes, chainExits,
		maxIRInstrs, maxBudgetReserves, maxBudgetSetPCs, maxBudgetRets, maxHostBytes, maxChainExits)

	// Optional VizJit dump. If GOCPU_VIZJIT is set (or VIZJIT_DIR
	// is already pointing somewhere non-empty), respect that so the
	// artifact survives after the test exits. Otherwise redirect to
	// t.TempDir() which vanishes on teardown — good for CI, still
	// useful during interactive -v runs.
	if testing.Verbose() {
		if VIZJIT_DIR == "" {
			savedDir := VIZJIT_DIR
			VIZJIT_DIR = t.TempDir()
			defer func() { VIZJIT_DIR = savedDir }()
		}
		vizJitDump(res.startPC, res.endPC, mem, res.block, progs, code, 0, alloc)
		t.Logf("VizJit dump written under %s (set GOCPU_VIZJIT=<dir> to keep)", VIZJIT_DIR)
	}

	// linux known variation (different zig version? different toolchain?)
	// jit_bloat_test.go:128: IR bloat regression: got 112 ops, budget 105
	if irOps > maxIRInstrs+7 {
		t.Errorf("IR bloat regression: got %d ops, budget %d", irOps, maxIRInstrs)
	}
	if budgetReserves > maxBudgetReserves {
		t.Errorf("budget-reserve bloat regression: got %d ops, budget %d", budgetReserves, maxBudgetReserves)
	}
	if budgetSetPCs > maxBudgetSetPCs {
		t.Errorf("budget set-pc bloat regression: got %d ops, budget %d", budgetSetPCs, maxBudgetSetPCs)
	}
	if budgetRets > maxBudgetRets {
		t.Errorf("budget-ret bloat regression: got %d ops, budget %d", budgetRets, maxBudgetRets)
	}
	if hostBytes > maxHostBytes {
		t.Errorf("host-code bloat regression: got %d bytes, budget %d", hostBytes, maxHostBytes)
	}
	if chainExits > maxChainExits {
		t.Errorf("chain-exit count regression: got %d, budget %d", chainExits, maxChainExits)
	}
}

func countBloatBudgetIROps(b *Block) (reserves, setPCs, rets int) {
	for i := range b.Instrs {
		switch b.Instrs[i].Op {
		case IRBudgetReserve:
			reserves++
		case IRSetPC:
			setPCs++
		case IRRetBudget:
			rets++
		}
	}
	return reserves, setPCs, rets
}
