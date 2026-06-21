package riscv

import "testing"

func TestHotRegionThresholdPromotesLazyRegion(t *testing.T) {
	const va = uint64(0x10000)
	data := BuildELF(va, []uint32{
		0x05D00893, // ADDI a7, x0, 93
		instrECALL,
	})

	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}

	j := NewSandboxJIT()
	defer j.Close()

	j.SetHotRegionThreshold(2)

	if j.maybeCompileHotRegion(mem, va) {
		t.Fatal("first hit compiled region before threshold")
	}
	if got := len(j.aotSegments); got != 0 {
		t.Fatalf("len(aotSegments) after first hit = %d, want 0", got)
	}
	if !j.maybeCompileHotRegion(mem, va) {
		t.Fatal("second hit did not compile region at threshold")
	}
	if got := len(j.aotSegments); got != 1 {
		t.Fatalf("len(aotSegments) after threshold = %d, want 1", got)
	}
	if j.HotRegionsCompiled != 1 {
		t.Fatalf("HotRegionsCompiled = %d, want 1", j.HotRegionsCompiled)
	}
	seg := j.aotSegments[0]
	if seg.vaddrBegin != va {
		t.Fatalf("segment begin = 0x%x, want 0x%x", seg.vaddrBegin, va)
	}
	if _, ok := seg.blocks[va]; !ok {
		t.Fatalf("segment missing entry block at 0x%x", va)
	}
}
