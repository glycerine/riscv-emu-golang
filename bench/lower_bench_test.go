// bench/lower_bench_test.go — Benchmarks comparing V1 vs V2 lowering and execution.
//
// Uses a real RISC-V ELF (Go compiler cross-compiled to riscv64, ~30MB)
// as a representative workload with diverse basic blocks.
//
// Usage:
//   go test -run='^$' -bench='BenchmarkLower' -benchtime=3s ./bench/
//   go test -run='TestLower_CodeSize' -v ./bench/
//
// To regenerate the ELF:
//   GOOS=linux GOARCH=riscv64 CGO_ENABLED=0 go build -o ~/ris/test_data/gc_riscv64 cmd/compile

package bench

import (
	"os"
	"testing"

	"riscv"
	"riscv/goasm"
	"riscv/ir"
)

const defaultGcELF = "/Users/jaten/ris/test_data/gc_riscv64"

// collectIRBlocks loads a RISC-V ELF and collects IR blocks by scanning PCs.
func collectIRBlocks(tb testing.TB, elfData []byte, maxBlocks int) ([]*riscv.EmitBlockResult, *riscv.GuestMemory) {
	tb.Helper()
	mem, err := riscv.NewGuestMemory(riscv.Size64MB)
	if err != nil {
		tb.Fatal(err)
	}

	entry, err := riscv.LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		tb.Fatal(err)
	}

	var results []*riscv.EmitBlockResult
	pc := entry
	seen := make(map[uint64]bool)

	for len(results) < maxBlocks && pc < entry+0x1000000 {
		if seen[pc] {
			pc += 4
			continue
		}
		seen[pc] = true

		res := riscv.EmitBlockForBench(mem, pc)
		if res != nil {
			results = append(results, res)
			// Advance past this block.
			advance := uint64(res.NumInsns) * 4
			if advance < 4 {
				advance = 4
			}
			pc += advance
		} else {
			pc += 4
		}
	}
	return results, mem
}

func loadGcELF(tb testing.TB) []byte {
	tb.Helper()
	path := os.Getenv("BENCH_ELF")
	if path == "" {
		path = defaultGcELF
	}
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("ELF not found at %q (set BENCH_ELF or run: GOOS=linux GOARCH=riscv64 CGO_ENABLED=0 go build -o %s cmd/compile)", path, defaultGcELF)
	}
	return data
}

// ── Lowering benchmarks ──

func BenchmarkLower_V1(b *testing.B) {
	elfData := loadGcELF(b)
	results, mem := collectIRBlocks(b, elfData, 2000)
	defer mem.Free()
	if len(results) == 0 {
		b.Skip("no IR blocks collected")
	}

	// Pre-allocate with V1 pool.
	type prepped struct {
		blk   *ir.Block
		alloc *ir.Allocation
	}
	items := make([]prepped, 0, len(results))
	for _, r := range results {
		pool := ir.AMD64Pool(r.Block)
		alloc := ir.Allocate(r.Block, pool, ir.AMD64Pinned(), nil)
		items = append(items, prepped{r.Block, alloc})
	}
	b.Logf("collected %d IR blocks", len(items))

	b.ResetTimer()
	b.ReportAllocs()

	totalInstrs := 0
	for i := 0; i < b.N; i++ {
		for _, it := range items {
			ctx := goasm.New(goasm.AMD64)
			ctx.Append(ctx.NewATEXT())
			if err := ir.LowerAMD64(ctx, it.blk, it.alloc); err != nil {
				continue
			}
			_, _ = ctx.Assemble()
			totalInstrs += len(it.blk.Instrs)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(totalInstrs)/float64(b.N), "ir-instrs/op")
	b.ReportMetric(float64(len(items)), "blocks")
}

func BenchmarkLower_V2(b *testing.B) {
	elfData := loadGcELF(b)
	results, mem := collectIRBlocks(b, elfData, 2000)
	defer mem.Free()
	if len(results) == 0 {
		b.Skip("no IR blocks collected")
	}

	type prepped struct {
		blk   *ir.Block
		alloc *ir.Allocation
	}
	items := make([]prepped, 0, len(results))
	for _, r := range results {
		pool := ir.AMD64Pool_V2(r.Block)
		alloc := ir.Allocate(r.Block, pool, ir.AMD64Pinned(), nil)
		items = append(items, prepped{r.Block, alloc})
	}
	b.Logf("collected %d IR blocks", len(items))

	b.ResetTimer()
	b.ReportAllocs()

	totalInstrs := 0
	for i := 0; i < b.N; i++ {
		for _, it := range items {
			ctx := goasm.New(goasm.AMD64)
			ctx.Append(ctx.NewATEXT())
			if err := ir.LowerAMD64_V2(ctx, it.blk, it.alloc); err != nil {
				continue
			}
			_, _ = ctx.Assemble()
			totalInstrs += len(it.blk.Instrs)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(totalInstrs)/float64(b.N), "ir-instrs/op")
	b.ReportMetric(float64(len(items)), "blocks")
}

// ── Execution throughput: V1 vs V2 on bench_guest.elf ──

func BenchmarkExec_V1(b *testing.B) {
	elfData := loadCPUELF(b)
	b.ReportAllocs()
	b.ResetTimer()
	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		// V1 is the default
		_, insns := runJITBenchGuestWith(cpu, jit)
		totalInsns += insns
		mem.Free()
	}
	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
	}
}

func BenchmarkExec_V2(b *testing.B) {
	elfData := loadCPUELF(b)
	b.ReportAllocs()
	b.ResetTimer()
	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		cpu, mem := newBenchCPU(b, elfData)
		jit := riscv.NewJIT()
		jit.UseV2 = true
		_, insns := runJITBenchGuestWith(cpu, jit)
		totalInsns += insns
		mem.Free()
	}
	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
	}
}

func runJITBenchGuestWith(cpu *riscv.CPU, jit *riscv.JIT) (exitCode int, insns uint64) {
	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*riscv.ExitError); ok {
				exitCode = ex.Code
				insns = cpu.Cycle()
				return
			}
			panic(r)
		}
	}()
	_ = jit.RunJIT(cpu)
	insns = cpu.Cycle()
	return
}

// ── Code size comparison ──

func TestLower_CodeSize_V1_vs_V2(t *testing.T) {
	elfData := loadGcELF(t)
	results, mem := collectIRBlocks(t, elfData, 2000)
	defer mem.Free()
	if len(results) == 0 {
		t.Skip("no IR blocks collected")
	}

	var v1Total, v2Total, v1Count, v2Count int
	var totalIRInstrs int
	for _, r := range results {
		blk := r.Block
		totalIRInstrs += len(blk.Instrs)

		// V1
		pool1 := ir.AMD64Pool(blk)
		alloc1 := ir.Allocate(blk, pool1, ir.AMD64Pinned(), nil)
		ctx1 := goasm.New(goasm.AMD64)
		ctx1.Append(ctx1.NewATEXT())
		if err := ir.LowerAMD64(ctx1, blk, alloc1); err == nil {
			if code, err := ctx1.Assemble(); err == nil {
				v1Total += len(code)
				v1Count++
			}
		}

		// V2
		pool2 := ir.AMD64Pool_V2(blk)
		alloc2 := ir.Allocate(blk, pool2, ir.AMD64Pinned(), nil)
		ctx2 := goasm.New(goasm.AMD64)
		ctx2.Append(ctx2.NewATEXT())
		if err := ir.LowerAMD64_V2(ctx2, blk, alloc2); err == nil {
			if code, err := ctx2.Assemble(); err == nil {
				v2Total += len(code)
				v2Count++
			}
		}
	}

	t.Logf("Blocks: %d total, V1 compiled %d, V2 compiled %d", len(results), v1Count, v2Count)
	t.Logf("IR instructions: %d total", totalIRInstrs)
	t.Logf("V1: %d bytes total (avg %.1f bytes/block)", v1Total, float64(v1Total)/float64(v1Count))
	t.Logf("V2: %d bytes total (avg %.1f bytes/block)", v2Total, float64(v2Total)/float64(v2Count))
	if v1Total > 0 {
		ratio := float64(v2Total) / float64(v1Total)
		t.Logf("V2/V1 code size ratio: %.3f (%+.1f%%)", ratio, (ratio-1)*100)
	}
}
