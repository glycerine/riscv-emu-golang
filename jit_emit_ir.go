//go:build !tcc

package riscv

// jit_emit_ir.go — Translates RISC-V basic blocks to IR (ir.Block).
// This replaces jit_emit.go's C source generation with IR emission.

// emitResult holds the generated IR block and metadata about the block.
type emitResult struct {
	startPC  uint64   // first instruction PC
	endPC    uint64   // PC past the last instruction
	numInsns int      // number of RISC-V instructions translated
	regsUsed [32]bool // which registers are read or written
}

// emitBlock translates a basic block starting at pc into an IR block.
// Uses a two-phase approach: scan the region, then emit all instructions.
func emitBlock(mem *GuestMemory, pc uint64) *emitResult {
	// Phase 1: pre-scan the region to determine block extent.
	region := scanRegion(mem, pc)
	if region.pcCount == 0 {
		return nil
	}

	// TODO: Phase 5 Steps 1-7 — create IR emitter, walk PCs, emit IR
	// For now, return nil to fall back to interpreter.
	_ = region
	return nil
}
