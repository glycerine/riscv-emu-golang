package riscv

import (
	"errors"
	"testing"
)

// TestJIT_Close_FreesLazyMmaps verifies the Phase 2c lazy-mmap
// cleanup on JIT.Close. Running a tiny guest through the lazy JIT
// path compiles one or more blocks whose mmaps are tracked in
// j.lazyBlocks; Close munmaps them and zeros the fn/nativeMmap
// fields. A second Close is a no-op.
func TestJIT_Close_FreesLazyMmaps(t *testing.T) {
	const va = uint64(0x10000)
	// ADDI a7, x0, 93 ; ECALL — minimal "exit" stub.
	code := []uint32{
		0x05D00893,
		0x00000073,
	}
	data := BuildELF(va, code)

	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(va)
	j := NewJIT()
	// No InstallAOT → pure lazy path.

	// Step the block once; this lazy-compiles the entry block and
	// registers it on j.lazyBlocks.
	if _, err := j.StepBlock(cpu); err != nil && !errors.Is(err, ErrEcall) {
		t.Fatalf("StepBlock: %v", err)
	}

	if len(j.lazyBlocks) == 0 {
		t.Fatalf("lazyBlocks empty after StepBlock; expected ≥1 lazy compile")
	}

	// Snapshot the block pointers + their fn values pre-Close.
	snap := make([]*compiledBlock, len(j.lazyBlocks))
	copy(snap, j.lazyBlocks)
	for i, blk := range snap {
		if blk.fn == 0 {
			t.Fatalf("lazyBlocks[%d].fn == 0 pre-Close", i)
		}
		if blk.nativeMmap == nil {
			t.Fatalf("lazyBlocks[%d].nativeMmap nil pre-Close", i)
		}
	}

	j.Close()

	if j.lazyBlocks != nil {
		t.Errorf("j.lazyBlocks not nil after Close: %v", j.lazyBlocks)
	}
	for i, blk := range snap {
		if blk.nativeMmap != nil {
			t.Errorf("block[%d].nativeMmap not nil after Close", i)
		}
		if blk.fn != 0 {
			t.Errorf("block[%d].fn=0x%x after Close, want 0", i, blk.fn)
		}
	}

	// Idempotent: second Close is a no-op.
	j.Close()
}
