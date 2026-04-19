package riscv

import "riscv/ir"

// EmitBlockResult holds IR block + metadata for benchmarking.
type EmitBlockResult struct {
	Block    *ir.Block
	NumInsns int
	StartPC  uint64
	EndPC    uint64
}

// ScanRegionResult holds scanRegion output for debugging.
type ScanRegionResult struct {
	EndPC   uint64
	PCCount int
}

// ScanRegionForBench exposes scanRegion for debugging.
func ScanRegionForBench(mem *GuestMemory, pc uint64) ScanRegionResult {
	r := scanRegion(mem, pc)
	return ScanRegionResult{r.endPC, r.pcCount}
}

// EmitBlockForBench exposes emitBlock for benchmarking.
func EmitBlockForBench(mem *GuestMemory, pc uint64) *EmitBlockResult {
	res := emitBlock(mem, pc)
	if res == nil || res.block == nil || res.numInsns == 0 {
		return nil
	}
	return &EmitBlockResult{
		Block:    res.block,
		NumInsns: res.numInsns,
		StartPC:  res.startPC,
		EndPC:    res.endPC,
	}
}
