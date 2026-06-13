package riscv

// helperTestAllocate runs the fixed static register allocator on b.
func helperTestAllocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
	a := NewFixedStaticAllocator()
	return a.Allocate(b, pool, pinned, freq)
}
