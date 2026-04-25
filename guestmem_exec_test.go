package riscv

import "testing"

// exec returns a fresh GuestMemory for exec-region tests (size is
// irrelevant for the table; we use the smallest power-of-two we have).
func execMem(t *testing.T) *GuestMemory {
	t.Helper()
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	t.Cleanup(mem.Free)
	return mem
}

func TestExecRegion_AddFind_Basic(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)

	if got := mem.FindExecRegion(0x1000); got == nil {
		t.Fatal("FindExecRegion(begin): got nil, want region")
	}
	if got := mem.FindExecRegion(0x1FFF); got == nil {
		t.Fatal("FindExecRegion(end-1): got nil, want region")
	}
	if got := mem.FindExecRegion(0x2000); got != nil {
		t.Fatalf("FindExecRegion(end): got %+v, want nil", got)
	}
	if got := mem.FindExecRegion(0x0FFF); got != nil {
		t.Fatalf("FindExecRegion(begin-1): got %+v, want nil", got)
	}
}

func TestExecRegion_Empty(t *testing.T) {
	mem := execMem(t)
	if got := mem.FindExecRegion(0x1000); got != nil {
		t.Fatalf("FindExecRegion on empty: got %+v, want nil", got)
	}
}

func TestExecRegion_ZeroRange(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x2000, 0x2000, false) // begin == end → no-op
	mem.AddExecRegion(0x3000, 0x2000, false) // begin > end  → no-op
	if got := mem.ExecRegions(); len(got) != 0 {
		t.Fatalf("got %d regions, want 0", len(got))
	}
}

func TestExecRegion_Coalesce_Overlap(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.AddExecRegion(0x1800, 0x2800, true)

	regs := mem.ExecRegions()
	if len(regs) != 1 {
		t.Fatalf("got %d regions, want 1 (coalesced); regs=%+v", len(regs), regs)
	}
	want := ExecRegion{VAddrBegin: 0x1000, VAddrEnd: 0x2800, IsLikelyJIT: true}
	if regs[0] != want {
		t.Fatalf("coalesced region: got %+v, want %+v", regs[0], want)
	}
}

func TestExecRegion_Coalesce_Adjacent_Disjoint(t *testing.T) {
	// Abutting but non-overlapping ranges are kept as two entries.
	// (Our coalesce is overlap-based, not adjacency-based. Either
	// behavior is fine; this test locks in the current choice.)
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.AddExecRegion(0x2000, 0x3000, false)

	regs := mem.ExecRegions()
	if len(regs) != 2 {
		t.Fatalf("got %d regions, want 2 (disjoint); regs=%+v", len(regs), regs)
	}
}

func TestExecRegion_Coalesce_Contained(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x3000, false)
	mem.AddExecRegion(0x1800, 0x2000, true) // fully inside prior

	regs := mem.ExecRegions()
	if len(regs) != 1 {
		t.Fatalf("got %d regions, want 1; regs=%+v", len(regs), regs)
	}
	// Span unchanged; IsLikelyJIT now true (last-writer-wins).
	want := ExecRegion{VAddrBegin: 0x1000, VAddrEnd: 0x3000, IsLikelyJIT: true}
	if regs[0] != want {
		t.Fatalf("got %+v, want %+v", regs[0], want)
	}
}

func TestExecRegion_Remove_Exact(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.RemoveExecRegion(0x1000, 0x2000)
	if got := mem.ExecRegions(); len(got) != 0 {
		t.Fatalf("after remove: got %d regions, want 0", len(got))
	}
}

func TestExecRegion_Remove_Inside_SplitsInTwo(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x3000, true)
	mem.RemoveExecRegion(0x1800, 0x2000) // punches a hole

	regs := mem.ExecRegions()
	if len(regs) != 2 {
		t.Fatalf("got %d regions, want 2 after hole-punch; regs=%+v", len(regs), regs)
	}
	if regs[0] != (ExecRegion{0x1000, 0x1800, true}) {
		t.Fatalf("regs[0] = %+v, want {0x1000,0x1800,true}", regs[0])
	}
	if regs[1] != (ExecRegion{0x2000, 0x3000, true}) {
		t.Fatalf("regs[1] = %+v, want {0x2000,0x3000,true}", regs[1])
	}
}

func TestExecRegion_Remove_PartialOverlap_Left(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.RemoveExecRegion(0x0800, 0x1400)

	regs := mem.ExecRegions()
	if len(regs) != 1 {
		t.Fatalf("got %d regions, want 1; regs=%+v", len(regs), regs)
	}
	if regs[0] != (ExecRegion{0x1400, 0x2000, false}) {
		t.Fatalf("got %+v, want {0x1400,0x2000,false}", regs[0])
	}
}

func TestExecRegion_Remove_PartialOverlap_Right(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.RemoveExecRegion(0x1C00, 0x2800)

	regs := mem.ExecRegions()
	if len(regs) != 1 {
		t.Fatalf("got %d regions, want 1; regs=%+v", len(regs), regs)
	}
	if regs[0] != (ExecRegion{0x1000, 0x1C00, false}) {
		t.Fatalf("got %+v, want {0x1000,0x1C00,false}", regs[0])
	}
}

func TestExecRegion_Remove_Disjoint_Noop(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.RemoveExecRegion(0x3000, 0x4000)

	regs := mem.ExecRegions()
	if len(regs) != 1 {
		t.Fatalf("got %d regions, want 1; regs=%+v", len(regs), regs)
	}
}

func TestExecRegion_Multiple_Disjoint(t *testing.T) {
	mem := execMem(t)
	mem.AddExecRegion(0x1000, 0x2000, false)
	mem.AddExecRegion(0x4000, 0x5000, true)
	mem.AddExecRegion(0x7000, 0x8000, false)

	if got := mem.FindExecRegion(0x1800); got == nil || !got.Contains(0x1800) {
		t.Fatalf("find 0x1800: got %+v", got)
	}
	if got := mem.FindExecRegion(0x4800); got == nil || !got.IsLikelyJIT {
		t.Fatalf("find 0x4800 (should be JIT): got %+v", got)
	}
	if got := mem.FindExecRegion(0x7800); got == nil || got.IsLikelyJIT {
		t.Fatalf("find 0x7800 (should not be JIT): got %+v", got)
	}
	if got := mem.FindExecRegion(0x3000); got != nil {
		t.Fatalf("find 0x3000 (gap): got %+v, want nil", got)
	}
}
