package ir

import (
	"testing"
)

// ── Test helpers ──

// makeBlock builds a Block from a variadic list of IRInstr.
func makeBlock(instrs ...IRInstr) *Block {
	b := NewBlock()
	b.Instrs = instrs
	return b
}

// testPool returns a RegPool with the specified number of int/FP regs.
// Int regs numbered 100, 101, ...; FP regs numbered 200, 201, ...
func testPool(nInt, nFP int) RegPool {
	p := RegPool{}
	for i := 0; i < nInt; i++ {
		p.IntRegs = append(p.IntRegs, int16(100+i))
	}
	for i := 0; i < nFP; i++ {
		p.FPRegs = append(p.FPRegs, int16(200+i))
	}
	return p
}

func assertAllocReg(t *testing.T, alloc *Allocation, v VReg) {
	t.Helper()
	if int(v) >= len(alloc.Kind) {
		t.Fatalf("VReg %d out of range (len=%d)", v, len(alloc.Kind))
	}
	if alloc.Kind[v] != AllocReg {
		t.Errorf("VReg %d: want AllocReg, got %d", v, alloc.Kind[v])
	}
}

func assertAllocStack(t *testing.T, alloc *Allocation, v VReg) {
	t.Helper()
	if int(v) >= len(alloc.Kind) {
		t.Fatalf("VReg %d out of range (len=%d)", v, len(alloc.Kind))
	}
	if alloc.Kind[v] != AllocStack {
		t.Errorf("VReg %d: want AllocStack, got %d", v, alloc.Kind[v])
	}
}

func assertAllocUnused(t *testing.T, alloc *Allocation, v VReg) {
	t.Helper()
	if int(v) >= len(alloc.Kind) {
		t.Fatalf("VReg %d out of range (len=%d)", v, len(alloc.Kind))
	}
	if alloc.Kind[v] != AllocUnused {
		t.Errorf("VReg %d: want AllocUnused, got %d", v, alloc.Kind[v])
	}
}

// regAt returns the host register assigned to VReg v at instruction index instrIdx.
func regAt(alloc *Allocation, v VReg, instrIdx int) (int16, bool) {
	for _, ia := range alloc.IntervalMap {
		if ia.Interval.VReg == v && ia.Interval.Start <= instrIdx && instrIdx <= ia.Interval.End {
			return ia.Host, true
		}
	}
	return 0, false
}

func assertRegAt(t *testing.T, alloc *Allocation, v VReg, instrIdx int, hostReg int16) {
	t.Helper()
	got, ok := regAt(alloc, v, instrIdx)
	if !ok {
		t.Fatalf("VReg %d has no assignment at instruction %d", v, instrIdx)
	}
	if got != hostReg {
		t.Errorf("VReg %d at instr %d: want host reg %d, got %d", v, instrIdx, hostReg, got)
	}
}

// assertNoConflicts verifies no two simultaneously-live VRegs share a host register.
func assertNoConflicts(t *testing.T, alloc *Allocation) {
	t.Helper()
	// For each pair of interval allocations, check if they overlap and share a host reg.
	for i := 0; i < len(alloc.IntervalMap); i++ {
		for j := i + 1; j < len(alloc.IntervalMap); j++ {
			a := alloc.IntervalMap[i]
			b := alloc.IntervalMap[j]
			if a.Host == b.Host {
				// Check if intervals overlap.
				if a.Interval.Start <= b.Interval.End && b.Interval.Start <= a.Interval.End {
					t.Errorf("conflict: VReg %d [%d,%d] and VReg %d [%d,%d] both use host reg %d",
						a.Interval.VReg, a.Interval.Start, a.Interval.End,
						b.Interval.VReg, b.Interval.Start, b.Interval.End,
						a.Host)
				}
			}
		}
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 1: instrDefs / instrUses
// ════════════════════════════════════════════════════════════════════════

func TestInstrDefs_ALU(t *testing.T) {
	ins := IRInstr{Op: IRAdd, Dst: VReg(5), A: VReg(1), B: VReg(2)}
	if got := instrDefs(&ins); got != VReg(5) {
		t.Errorf("instrDefs(IRAdd) = %v, want x5", got)
	}
}

func TestInstrDefs_Store(t *testing.T) {
	ins := IRInstr{Op: IRStore, A: VReg(1), B: VReg(2), Imm: 8}
	if got := instrDefs(&ins); got != VRegZero {
		t.Errorf("instrDefs(IRStore) = %v, want v0", got)
	}
}

func TestInstrDefs_StoreX(t *testing.T) {
	ins := IRInstr{Op: IRStoreX, Dst: VReg(5), A: VReg(1), B: VReg(2)}
	if got := instrDefs(&ins); got != VRegZero {
		t.Errorf("instrDefs(IRStoreX) = %v, want v0 (Dst is a use, not a def)", got)
	}
}

func TestInstrDefs_Label(t *testing.T) {
	ins := IRInstr{Op: IRLabel, Imm: 1}
	if got := instrDefs(&ins); got != VRegZero {
		t.Errorf("instrDefs(IRLabel) = %v, want v0", got)
	}
}

func TestInstrDefs_Ret(t *testing.T) {
	ins := IRInstr{Op: IRRet, A: VReg(3), Imm: 100, Imm2: 0}
	if got := instrDefs(&ins); got != VRegZero {
		t.Errorf("instrDefs(IRRet) = %v, want v0", got)
	}
}

func TestInstrUses_ALU(t *testing.T) {
	ins := IRInstr{Op: IRAdd, Dst: VReg(5), A: VReg(1), B: VReg(2)}
	uses := instrUses(&ins)
	if len(uses) != 2 || uses[0] != VReg(1) || uses[1] != VReg(2) {
		t.Errorf("instrUses(IRAdd) = %v, want [x1, x2]", uses)
	}
}

func TestInstrUses_Store(t *testing.T) {
	ins := IRInstr{Op: IRStore, T: I64, A: VReg(10), B: VReg(7), Imm: 8}
	uses := instrUses(&ins)
	if len(uses) != 2 || uses[0] != VReg(10) || uses[1] != VReg(7) {
		t.Errorf("instrUses(IRStore) = %v, want [x10, x7]", uses)
	}
}

func TestInstrUses_StoreX(t *testing.T) {
	ins := IRInstr{Op: IRStoreX, Dst: VReg(5), A: VReg(1), B: VReg(2), Scale: 8}
	uses := instrUses(&ins)
	if len(uses) != 3 {
		t.Fatalf("instrUses(IRStoreX) = %v, want 3 uses [x1, x2, x5]", uses)
	}
	// A=1 (base), B=2 (index), Dst=5 (value)
	found := map[VReg]bool{}
	for _, u := range uses {
		found[u] = true
	}
	for _, want := range []VReg{1, 2, 5} {
		if !found[want] {
			t.Errorf("instrUses(IRStoreX) missing %v, got %v", want, uses)
		}
	}
}

func TestInstrUses_Const(t *testing.T) {
	ins := IRInstr{Op: IRConst, Dst: VReg(5), Imm: 42}
	uses := instrUses(&ins)
	if len(uses) != 0 {
		t.Errorf("instrUses(IRConst) = %v, want empty", uses)
	}
}

func TestInstrUses_Ret(t *testing.T) {
	ins := IRInstr{Op: IRRet, A: VReg(3), Imm: 100}
	uses := instrUses(&ins)
	if len(uses) != 1 || uses[0] != VReg(3) {
		t.Errorf("instrUses(IRRet) = %v, want [x3]", uses)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 2: computeIntervalSets
// ════════════════════════════════════════════════════════════════════════

func TestIntervalSets_EmptyBlock(t *testing.T) {
	b := makeBlock()
	intervals := computeIntervalSets(b)
	if intervals != nil {
		t.Errorf("expected nil for empty block, got %v", intervals)
	}
}

func TestIntervalSets_SingleDef(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 42},
	)
	intervals := computeIntervalSets(b)
	is := intervals[64]
	if len(is.Intervals) != 1 {
		t.Fatalf("expected 1 interval for t64, got %d", len(is.Intervals))
	}
	iv := is.Intervals[0]
	if iv.Start != 0 || iv.End != 0 {
		t.Errorf("t64 interval = [%d,%d], want [0,0]", iv.Start, iv.End)
	}
}

func TestIntervalSets_DefAndUse(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 42},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(64), B: VReg(2)},
	)
	intervals := computeIntervalSets(b)
	is := intervals[64]
	if len(is.Intervals) != 1 {
		t.Fatalf("expected 1 interval for t64, got %d", len(is.Intervals))
	}
	iv := is.Intervals[0]
	if iv.Start != 0 || iv.End != 1 {
		t.Errorf("t64 interval = [%d,%d], want [0,1]", iv.Start, iv.End)
	}
}

func TestIntervalSets_LiveRangeHole(t *testing.T) {
	// t64 defined at 0, used at 2, dead at 3-5, redefined at 6, used at 8
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},          // 0: def t64
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},          // 1: (filler)
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)}, // 2: use t64
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 3},          // 3: (filler)
		IRInstr{Op: IRConst, Dst: VReg(68), Imm: 4},          // 4: (filler)
		IRInstr{Op: IRConst, Dst: VReg(69), Imm: 5},          // 5: (filler)
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 10},         // 6: redef t64
		IRInstr{Op: IRConst, Dst: VReg(70), Imm: 6},          // 7: (filler)
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(71), A: VReg(64), B: VReg(70)}, // 8: use t64
	)
	intervals := computeIntervalSets(b)
	is := intervals[64]
	if len(is.Intervals) != 2 {
		t.Fatalf("expected 2 intervals for t64 (hole at 3-5), got %d: %+v", len(is.Intervals), is.Intervals)
	}
	if is.Intervals[0].Start != 0 || is.Intervals[0].End != 2 {
		t.Errorf("t64 interval[0] = [%d,%d], want [0,2]", is.Intervals[0].Start, is.Intervals[0].End)
	}
	if is.Intervals[1].Start != 6 || is.Intervals[1].End != 8 {
		t.Errorf("t64 interval[1] = [%d,%d], want [6,8]", is.Intervals[1].Start, is.Intervals[1].End)
	}
}

func TestIntervalSets_GuestRegsExtendToEnd(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(5), Imm: 1},         // 0: def x5
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 2},        // 1
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(5), B: VReg(64)}, // 2: use x5
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},        // 3
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 4},        // 4
		IRInstr{Op: IRConst, Dst: VReg(68), Imm: 5},        // 5
		IRInstr{Op: IRConst, Dst: VReg(69), Imm: 6},        // 6
		IRInstr{Op: IRConst, Dst: VReg(70), Imm: 7},        // 7
		IRInstr{Op: IRConst, Dst: VReg(71), Imm: 8},        // 8
		IRInstr{Op: IRConst, Dst: VReg(72), Imm: 9},        // 9
	)
	intervals := computeIntervalSets(b)
	is := intervals[5]
	if len(is.Intervals) < 1 {
		t.Fatal("expected at least 1 interval for x5")
	}
	last := is.Intervals[len(is.Intervals)-1]
	if last.End != 9 {
		t.Errorf("x5 last interval End = %d, want 9 (extended to block end)", last.End)
	}
}

func TestIntervalSets_VRegZeroNoIntervals(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VRegZero, B: VReg(2)},
	)
	intervals := computeIntervalSets(b)
	is := intervals[0]
	if len(is.Intervals) != 0 {
		t.Errorf("VRegZero should have no intervals, got %d", len(is.Intervals))
	}
}

func TestIntervalSets_ParamVRegUsedBeforeDef(t *testing.T) {
	// t64 used at instruction 0 but never explicitly defined (it's a parameter).
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(64), B: VReg(2)},
	)
	intervals := computeIntervalSets(b)
	is := intervals[64]
	if len(is.Intervals) < 1 {
		t.Fatal("expected at least 1 interval for param t64 used before def")
	}
	if is.Intervals[0].Start != 0 {
		t.Errorf("t64 interval Start = %d, want 0", is.Intervals[0].Start)
	}
}

func TestIntervalSets_FPRegsExtendToEnd(t *testing.T) {
	// f5 = VReg(37)
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(37), Imm: 1}, // 0: def f5
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 2}, // 1
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 3}, // 2
	)
	intervals := computeIntervalSets(b)
	is := intervals[37]
	if len(is.Intervals) < 1 {
		t.Fatal("expected at least 1 interval for f5")
	}
	last := is.Intervals[len(is.Intervals)-1]
	if last.End != 2 {
		t.Errorf("f5 last interval End = %d, want 2 (extended to block end)", last.End)
	}
}

func TestIntervalSets_MultipleVRegs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
	)
	intervals := computeIntervalSets(b)
	// t64: [0,2], t65: [1,2], t66: [2,2]
	if len(intervals[64].Intervals) != 1 || intervals[64].Intervals[0].Start != 0 {
		t.Errorf("t64 intervals = %+v", intervals[64].Intervals)
	}
	if len(intervals[65].Intervals) != 1 || intervals[65].Intervals[0].Start != 1 {
		t.Errorf("t65 intervals = %+v", intervals[65].Intervals)
	}
	if len(intervals[66].Intervals) != 1 || intervals[66].Intervals[0].Start != 2 {
		t.Errorf("t66 intervals = %+v", intervals[66].Intervals)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 3: buildIEP / computeCount
// ════════════════════════════════════════════════════════════════════════

func TestBuildIEP_Order(t *testing.T) {
	intervals := []intervalSet{
		{VReg: VReg(64), Intervals: []Interval{{VReg: VReg(64), Start: 5, End: 10}}},
		{VReg: VReg(65), Intervals: []Interval{{VReg: VReg(65), Start: 0, End: 3}}},
	}
	eps := buildIEP(intervals)
	for i := 1; i < len(eps); i++ {
		if eps[i].Point < eps[i-1].Point {
			t.Errorf("endpoints not sorted: [%d].Point=%d < [%d].Point=%d",
				i, eps[i].Point, i-1, eps[i-1].Point)
		}
	}
}

func TestBuildIEP_EndsBeforeStarts(t *testing.T) {
	// Two intervals: [0,3] and [3,5]. At point 3, end should come before start.
	intervals := []intervalSet{
		{VReg: VReg(64), Intervals: []Interval{{VReg: VReg(64), Start: 0, End: 3}}},
		{VReg: VReg(65), Intervals: []Interval{{VReg: VReg(65), Start: 3, End: 5}}},
	}
	eps := buildIEP(intervals)
	// Find the two endpoints at point 3.
	var atThree []iep
	for _, ep := range eps {
		if ep.Point == 3 {
			atThree = append(atThree, ep)
		}
	}
	if len(atThree) < 2 {
		t.Fatalf("expected 2 endpoints at point 3, got %d", len(atThree))
	}
	// First should be the end (IsStart=false), second the start (IsStart=true).
	if atThree[0].IsStart {
		t.Error("at point 3: expected end before start")
	}
}

func TestComputeCount_NoPressure(t *testing.T) {
	intervals := []intervalSet{
		{VReg: VReg(64), Intervals: []Interval{{VReg: VReg(64), Start: 0, End: 2}}},
		{VReg: VReg(65), Intervals: []Interval{{VReg: VReg(65), Start: 4, End: 6}}},
		{VReg: VReg(66), Intervals: []Interval{{VReg: VReg(66), Start: 8, End: 9}}},
	}
	count := computeCount(intervals, 10)
	for _, c := range count {
		if c > 1 {
			t.Errorf("max count should be 1 (no overlap), got count=%v", count)
			break
		}
	}
}

func TestComputeCount_FullOverlap(t *testing.T) {
	intervals := []intervalSet{
		{VReg: VReg(64), Intervals: []Interval{{VReg: VReg(64), Start: 0, End: 5}}},
		{VReg: VReg(65), Intervals: []Interval{{VReg: VReg(65), Start: 0, End: 5}}},
		{VReg: VReg(66), Intervals: []Interval{{VReg: VReg(66), Start: 0, End: 5}}},
	}
	count := computeCount(intervals, 6)
	for p := 0; p <= 5; p++ {
		if count[p] != 3 {
			t.Errorf("count[%d] = %d, want 3", p, count[p])
		}
	}
}

func TestComputeCount_Gradient(t *testing.T) {
	intervals := []intervalSet{
		{VReg: VReg(64), Intervals: []Interval{{VReg: VReg(64), Start: 0, End: 5}}},
		{VReg: VReg(65), Intervals: []Interval{{VReg: VReg(65), Start: 1, End: 5}}},
		{VReg: VReg(66), Intervals: []Interval{{VReg: VReg(66), Start: 2, End: 5}}},
	}
	count := computeCount(intervals, 6)
	if count[0] != 1 {
		t.Errorf("count[0] = %d, want 1", count[0])
	}
	if count[1] != 2 {
		t.Errorf("count[1] = %d, want 2", count[1])
	}
	if count[2] != 3 {
		t.Errorf("count[2] = %d, want 3", count[2])
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 4: classifyVRegs / BlockHasDivMul / maxVReg
// ════════════════════════════════════════════════════════════════════════

func TestClassifyVRegs_GuestInt(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(5), Imm: 1},
	)
	intervals := computeIntervalSets(b)
	isFP := classifyVRegs(b, intervals)
	for vr := 1; vr <= 31; vr++ {
		if vr < len(isFP) && isFP[vr] {
			t.Errorf("VReg %d (guest int) classified as FP", vr)
		}
	}
}

func TestClassifyVRegs_GuestFP(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(37), Imm: 1}, // f5
	)
	intervals := computeIntervalSets(b)
	isFP := classifyVRegs(b, intervals)
	if !isFP[37] {
		t.Error("VReg 37 (f5) should be classified as FP")
	}
}

func TestClassifyVRegs_TempFromFAdd(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(64), A: VReg(32), B: VReg(33)},
	)
	intervals := computeIntervalSets(b)
	isFP := classifyVRegs(b, intervals)
	if !isFP[64] {
		t.Error("temp defined by IRFAdd(F64) should be FP")
	}
}

func TestClassifyVRegs_TempFromFCvtToI(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRFCvtToI, T: I64, U: F64, Dst: VReg(64), A: VReg(32)},
	)
	intervals := computeIntervalSets(b)
	isFP := classifyVRegs(b, intervals)
	if isFP[64] {
		t.Error("temp defined by IRFCvtToI(I64) should be integer, not FP")
	}
}

func TestBlockHasDivMul_NoDivMul(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	)
	if BlockHasDivMul(b) {
		t.Error("expected false for block with only IRAdd")
	}
}

func TestBlockHasDivMul_HasDivS(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRDivS, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	)
	if !BlockHasDivMul(b) {
		t.Error("expected true for block with IRDivS")
	}
}

func TestBlockHasDivMul_HasRem(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRRem, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	)
	if !BlockHasDivMul(b) {
		t.Error("expected true for block with IRRem")
	}
}

func TestMaxVReg_EmptyBlock(t *testing.T) {
	b := makeBlock()
	if got := maxVReg(b); got != 0 {
		t.Errorf("maxVReg(empty) = %d, want 0", got)
	}
}

func TestMaxVReg_HighTemp(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(200), Imm: 1},
	)
	if got := maxVReg(b); got != 200 {
		t.Errorf("maxVReg = %d, want 200", got)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 5: ELS_0 basic assignment
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_EmptyBlock(t *testing.T) {
	b := makeBlock()
	alloc := Allocate(b, testPool(8, 4), nil, nil)
	if alloc == nil {
		t.Fatal("Allocate returned nil for empty block")
	}
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0", alloc.StackSlots)
	}
}

func TestAllocate_SingleInstrNoPressure(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 42},
	)
	alloc := Allocate(b, testPool(8, 4), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0", alloc.StackSlots)
	}
}

func TestAllocate_AllFitNoOverlap(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1}, // 0
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2}, // 1
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3}, // 2
	)
	alloc := Allocate(b, testPool(3, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
	assertAllocReg(t, alloc, VReg(66))
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0", alloc.StackSlots)
	}
}

func TestAllocate_AllFitOverlapping(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(66), B: VReg(67)},
	)
	alloc := Allocate(b, testPool(4, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
	assertAllocReg(t, alloc, VReg(66))
	assertNoConflicts(t, alloc)
}

func TestAllocate_VRegZeroNeverAllocated(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VRegZero, B: VReg(2)},
	)
	alloc := Allocate(b, testPool(8, 0), nil, nil)
	assertAllocUnused(t, alloc, VRegZero)
}

func TestAllocate_ReuseAfterDeath(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},          // 0: def t64
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(64), B: VReg(64)}, // 1: use t64 (last use)
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 2},          // 2: (gap)
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 3},          // 3: (gap)
		IRInstr{Op: IRConst, Dst: VReg(68), Imm: 4},          // 4: def t68
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(68), B: VReg(68)}, // 5: use t68
	)
	alloc := Allocate(b, testPool(2, 0), nil, nil)
	// t64 and t68 don't overlap — both should get registers with 2 regs.
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(68))
	assertNoConflicts(t, alloc)
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0 (ranges don't overlap)", alloc.StackSlots)
	}
}

func TestAllocate_ManyShortRanges(t *testing.T) {
	var instrs []IRInstr
	// 10 temps, each defined and immediately used (2 instrs each, sequential).
	// Use separate temp destinations (not guest regs, which extend to end of block).
	for i := 0; i < 10; i++ {
		src := VReg(64 + i*2)
		dst := VReg(64 + i*2 + 1)
		instrs = append(instrs, IRInstr{Op: IRConst, Dst: src, Imm: int64(i)})
		instrs = append(instrs, IRInstr{Op: IRAdd, T: I64, Dst: dst, A: src, B: src})
	}
	b := makeBlock(instrs...)
	alloc := Allocate(b, testPool(1, 0), nil, nil)
	for i := 0; i < 10; i++ {
		assertAllocReg(t, alloc, VReg(64+i*2))
	}
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Group 7: Spill identification
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_OneSpill(t *testing.T) {
	// 3 temps all live simultaneously, but only 2 regs.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(66), B: VReg(67)},
	)
	alloc := Allocate(b, testPool(2, 0), nil, nil)
	// At least one should be spilled.
	spilled := 0
	for vr := VReg(64); vr <= 68; vr++ {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocStack {
			spilled++
		}
	}
	if spilled == 0 {
		t.Error("expected at least 1 spill with 2 regs and 3+ simultaneous live")
	}
	assertNoConflicts(t, alloc)
}

func TestAllocate_MultipleSpills(t *testing.T) {
	// 5 temps all live simultaneously, but only 2 regs.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 4},
		IRInstr{Op: IRConst, Dst: VReg(68), Imm: 5},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(70), A: VReg(66), B: VReg(67)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(71), A: VReg(68), B: VReg(69)},
	)
	alloc := Allocate(b, testPool(2, 0), nil, nil)
	spilled := 0
	for vr := VReg(64); vr <= 71; vr++ {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocStack {
			spilled++
		}
	}
	if spilled < 3 {
		t.Errorf("expected at least 3 spills, got %d", spilled)
	}
	assertNoConflicts(t, alloc)
}

func TestAllocate_StackSlotCounting(t *testing.T) {
	// Force exactly 3 spills.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 4},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(66), B: VReg(67)},
	)
	alloc := Allocate(b, testPool(1, 0), nil, nil)
	if alloc.StackSlots < 1 {
		t.Errorf("StackSlots = %d, want >= 1", alloc.StackSlots)
	}
	// Verify unique slot indices for spilled VRegs.
	slots := map[int16]bool{}
	for vr := VReg(64); vr <= 69; vr++ {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocStack {
			slot := alloc.SpillSlot[vr]
			if slots[slot] {
				t.Errorf("duplicate spill slot %d", slot)
			}
			slots[slot] = true
		}
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 10: FP/int pool separation
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_IntAndFP_SeparatePools(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                         // int temp
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(32), A: VReg(33), B: VReg(34)}, // FP (guest f0)
	)
	alloc := Allocate(b, testPool(1, 1), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(32))
}

// ════════════════════════════════════════════════════════════════════════
// Group 12: Pinned parameter VRegs
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_PinnedRegs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(64), B: VReg(65)},
	)
	pinned := map[VReg]int16{
		VReg(64): 50,
		VReg(65): 51,
	}
	alloc := Allocate(b, testPool(4, 0), pinned, nil)
	assertRegAt(t, alloc, VReg(64), 0, 50)
	assertRegAt(t, alloc, VReg(65), 0, 51)
}

// ════════════════════════════════════════════════════════════════════════
// Group 13: Invariant verification
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_NoConflicts_SmallBlock(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 4},
		IRInstr{Op: IRConst, Dst: VReg(68), Imm: 5},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(70), A: VReg(66), B: VReg(67)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(71), A: VReg(68), B: VReg(69)},
	)
	alloc := Allocate(b, testPool(3, 0), nil, nil)
	assertNoConflicts(t, alloc)
}

func TestAllocate_AllReferencedVRegsAllocated(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
	)
	alloc := Allocate(b, testPool(4, 0), nil, nil)
	// Every referenced VReg (except VRegZero) should have Kind != AllocUnused.
	referenced := map[VReg]bool{}
	for _, ins := range b.Instrs {
		for _, vr := range []VReg{ins.Dst, ins.A, ins.B} {
			if vr != VRegZero {
				referenced[vr] = true
			}
		}
	}
	for vr := range referenced {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocUnused {
			t.Errorf("VReg %d is referenced but AllocUnused", vr)
		}
	}
}
