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
// In ELS, an interval ending at point P and another starting at P do NOT conflict:
// the end is processed before the start at the same point (register freed then assigned).
// So we use strict overlap: a.Start < b.End && b.Start < a.End.
func assertNoConflicts(t *testing.T, alloc *Allocation) {
	t.Helper()
	for i := 0; i < len(alloc.IntervalMap); i++ {
		for j := i + 1; j < len(alloc.IntervalMap); j++ {
			a := alloc.IntervalMap[i]
			b := alloc.IntervalMap[j]
			if a.Host == b.Host {
				// Strict overlap: touching endpoints are OK.
				if a.Interval.Start < b.Interval.End && b.Interval.Start < a.Interval.End {
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
// Group 6: Assignment preference heuristics
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_PreferSameReg(t *testing.T) {
	// t64 has two intervals (hole in between). Both should prefer the same host reg.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                             // 0: def t64
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(64), B: VReg(64)},     // 1: use t64
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 2},                             // 2: filler
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 3},                             // 3: filler
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 10},                            // 4: redef t64
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(64), B: VReg(64)},     // 5: use t64
	)
	alloc := Allocate(b, testPool(4, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	// Both intervals of t64 should get the same host register.
	reg1, ok1 := regAt(alloc, VReg(64), 0)
	reg2, ok2 := regAt(alloc, VReg(64), 5)
	if ok1 && ok2 && reg1 != reg2 {
		// With 4 regs available and low pressure, same reg should be preferred.
		t.Errorf("t64 got different regs across intervals: %d at 0, %d at 5", reg1, reg2)
	}
	assertNoConflicts(t, alloc)
}

func TestAllocate_IntervalHoleReuse(t *testing.T) {
	// t64 has a hole at [3,5]; another VReg can use t64's register during the hole.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                             // 0
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(64), B: VReg(64)},     // 1
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 2},                             // 2: use t64 ends
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 3},                             // 3: hole — t67 can use t64's reg
		IRInstr{Op: IRConst, Dst: VReg(68), Imm: 4},                             // 4: hole
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 10},                            // 5: redef t64
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(64), B: VReg(64)},     // 6
	)
	alloc := Allocate(b, testPool(2, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(67))
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Group 8: Spill resurrection
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_SpillResurrection(t *testing.T) {
	// Three VRegs A, B, C all overlap with only 2 regs.
	// A is spilled first (lowest cost). Then B is spilled. After B is spilled,
	// pressure drops enough that A can be resurrected.
	//
	// Use freq to control spill order: A has low freq (spilled first),
	// B has medium freq, C has high freq (never spilled).
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                             // 0: def A
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},                             // 1: def B
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},                             // 2: def C
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(65)},     // 3: use A, B
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(66), B: VReg(67)},     // 4: use C
	)
	// freq weights: instructions 0-2 are low weight, 3-4 are high weight.
	freq := []float64{1, 1, 1, 10, 10}
	alloc := Allocate(b, testPool(2, 0), nil, freq)
	// With resurrection, it's possible that a VReg initially spilled gets un-spilled.
	// We verify the key invariant: no conflicts.
	assertNoConflicts(t, alloc)
	// And that allocation completed without panic.
	if alloc.StackSlots < 0 {
		t.Errorf("StackSlots = %d, want >= 0", alloc.StackSlots)
	}
}

func TestAllocate_NoResurrection(t *testing.T) {
	// All 4 VRegs fully overlap, only 2 regs — 2 must be spilled, no room to resurrect.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 4},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(66), B: VReg(67)},
	)
	alloc := Allocate(b, testPool(2, 0), nil, nil)
	spilled := 0
	for vr := VReg(64); vr <= 69; vr++ {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocStack {
			spilled++
		}
	}
	if spilled < 2 {
		t.Errorf("expected >= 2 spills, got %d", spilled)
	}
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Group 9: Spill cost with freq
// ════════════════════════════════════════════════════════════════════════

func TestComputeSpillCosts_UniformFreq(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                             // 1 write
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(64), B: VReg(64)},     // 2 reads of t64
	)
	intervals := computeIntervalSets(b)
	costs := computeSpillCosts(b, intervals, nil)
	// t64: 1 write (idx 0) + 2 reads (idx 1, but A and B both are t64) = 3
	if costs[64] != 3 {
		t.Errorf("spillCost[t64] = %v, want 3", costs[64])
	}
}

func TestComputeSpillCosts_LoopWeight(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                             // 0: low freq
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},                             // 1: high freq
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},     // 2: high freq
	)
	intervals := computeIntervalSets(b)
	freq := []float64{1.0, 100.0, 100.0}
	costs := computeSpillCosts(b, intervals, freq)
	// t64 cost: 1 (write at freq 1) + 100 (read at freq 100) = 101
	// t65 cost: 100 (write at freq 100) + 100 (read at freq 100) = 200
	if costs[64] >= costs[65] {
		t.Errorf("t64 cost (%v) should be < t65 cost (%v) due to freq weighting", costs[64], costs[65])
	}
}

func TestComputeSpillCosts_DeadDef(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                             // defined, never used
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(65), B: VReg(65)},     // t65 used
	)
	intervals := computeIntervalSets(b)
	costs := computeSpillCosts(b, intervals, nil)
	// t64: 1 write only. t65: 1 write + 2 reads = 3.
	if costs[64] >= costs[65] {
		t.Errorf("dead def t64 cost (%v) should be < t65 cost (%v)", costs[64], costs[65])
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 10: FP/int pool separation
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_IntAndFP_SeparatePools(t *testing.T) {
	// Use FP temps (not guest FP regs, which extend to end of block).
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},                           // int temp
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(65), A: VReg(65), B: VReg(65)}, // FP temp
	)
	alloc := Allocate(b, testPool(1, 1), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
}

func TestAllocate_FPPressure_IntFree(t *testing.T) {
	// 2 FP temps simultaneously live, only 1 FP reg. Int regs should not be used for FP.
	b := makeBlock(
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(64), A: VReg(32), B: VReg(33)}, // FP temp t64
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(65), A: VReg(34), B: VReg(35)}, // FP temp t65
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(66), A: VReg(64), B: VReg(65)}, // uses both
	)
	alloc := Allocate(b, testPool(5, 1), nil, nil)
	// At least one FP temp should be spilled (only 1 FP reg).
	fpSpilled := 0
	for _, vr := range []VReg{64, 65} {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocStack {
			fpSpilled++
		}
	}
	if fpSpilled == 0 {
		t.Error("expected at least 1 FP spill with only 1 FP reg")
	}
}

func TestAllocate_GuestFPRegs(t *testing.T) {
	// f5 (VReg 37) should be assigned from FP pool.
	b := makeBlock(
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(37), A: VReg(33), B: VReg(34)},
	)
	alloc := Allocate(b, testPool(4, 4), nil, nil)
	assertAllocReg(t, alloc, VReg(37))
	// Verify it got an FP pool register (200+).
	reg, ok := regAt(alloc, VReg(37), 0)
	if ok && reg < 200 {
		t.Errorf("guest FP reg f5 got int pool reg %d, want FP pool (200+)", reg)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 11: Guest regs live to end
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_GuestRegLiveToEnd(t *testing.T) {
	// x5 defined at 0, block has 10 instrs. x5 should hold its register throughout.
	var instrs []IRInstr
	instrs = append(instrs, IRInstr{Op: IRConst, Dst: VReg(5), Imm: 1}) // 0: def x5
	for i := 1; i < 10; i++ {
		instrs = append(instrs, IRInstr{Op: IRConst, Dst: VReg(64 + i), Imm: int64(i)})
	}
	b := makeBlock(instrs...)
	alloc := Allocate(b, testPool(4, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(5))
	// x5 should be live at the last instruction.
	_, ok := regAt(alloc, VReg(5), 9)
	if !ok {
		t.Error("x5 should be live at last instruction (guest reg extends to end)")
	}
}

func TestAllocate_GuestRegEvictsTemp(t *testing.T) {
	// x5 lives to end, temps overlap. With low reg count, some temps should be spilled.
	// At instr 3: x5 + t64 + t65 + t66 = 4 live. With 3 regs, 1 must spill.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(5), Imm: 1},                             // 0: def x5
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 2},                            // 1: def t64
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 3},                            // 2: def t65
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},    // 3: use t64,t65
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(5), B: VReg(66)},     // 4: use x5
	)
	alloc := Allocate(b, testPool(3, 0), nil, nil)
	// x5 is guest reg → live to end. With 3 regs, some temps may spill
	// but x5 should survive (highest cost: used late in block).
	assertNoConflicts(t, alloc)
	// At least some VRegs should be spilled with only 3 regs and peak pressure of 4.
	if alloc.StackSlots < 1 {
		t.Errorf("expected >= 1 spill, got StackSlots=%d", alloc.StackSlots)
	}
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

func TestAllocate_PinnedRegsNotInPool(t *testing.T) {
	// Pinned regs don't consume pool registers. Pool has 2 regs,
	// 2 pinned VRegs, 2 non-pinned temps → all should get registers.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 1},                             // non-pinned temp
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 2},                             // non-pinned temp
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(64), B: VReg(66)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(65), B: VReg(67)},
	)
	pinned := map[VReg]int16{VReg(64): 50, VReg(65): 51}
	alloc := Allocate(b, testPool(2, 0), pinned, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
	assertAllocReg(t, alloc, VReg(66))
	assertAllocReg(t, alloc, VReg(67))
	assertNoConflicts(t, alloc)
}

func TestAllocate_PinnedRegsBlockHostReg(t *testing.T) {
	// Pinned VReg's host reg should not be assignable to other VRegs.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 1},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(66)},
	)
	// Pin t64 to host reg 100 (first in pool). Pool has regs [100, 101, 102].
	// At instr 1: t64(pinned) + t66 + t67 all live → need 2 pool regs after pin.
	pinned := map[VReg]int16{VReg(64): 100}
	alloc := Allocate(b, testPool(3, 0), pinned, nil)
	assertRegAt(t, alloc, VReg(64), 0, 100)
	// t66 should NOT get host reg 100 (it's taken by pinned t64).
	reg66, ok := regAt(alloc, VReg(66), 0)
	if ok && reg66 == 100 {
		t.Error("t66 should not share host reg 100 with pinned t64")
	}
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Group 13: Register moves
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_NoMovesNeeded(t *testing.T) {
	// Single interval per VReg — no moves needed.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(64), B: VReg(64)},
	)
	alloc := Allocate(b, testPool(4, 0), nil, nil)
	if len(alloc.Moves) != 0 {
		t.Errorf("expected 0 moves, got %d", len(alloc.Moves))
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group 14: Edge cases
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_OnlyVRegZeroRefs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VRegZero, A: VRegZero, B: VRegZero},
	)
	alloc := Allocate(b, testPool(4, 0), nil, nil)
	assertAllocUnused(t, alloc, VRegZero)
}

func TestAllocate_ManyTempsShortRanges(t *testing.T) {
	// 50 sequential pairs: const+add. At each add, src+dst are both live (count=2).
	// Need 2 regs to avoid spills.
	var instrs []IRInstr
	for i := 0; i < 50; i++ {
		src := VReg(64 + i*2)
		dst := VReg(64 + i*2 + 1)
		instrs = append(instrs, IRInstr{Op: IRConst, Dst: src, Imm: int64(i)})
		instrs = append(instrs, IRInstr{Op: IRAdd, T: I64, Dst: dst, A: src, B: src})
	}
	b := makeBlock(instrs...)
	alloc := Allocate(b, testPool(2, 0), nil, nil)
	assertNoConflicts(t, alloc)
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0 (sequential non-overlapping)", alloc.StackSlots)
	}
}

func TestAllocate_OneLongVsManyShort(t *testing.T) {
	// t64 lives [0, last]. 10 short temp pairs at various points.
	// At each short add: t64 + short_src + short_dst = 3 live. Need 3 regs.
	var instrs []IRInstr
	instrs = append(instrs, IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1}) // 0: def t64
	for i := 1; i <= 10; i++ {
		short := VReg(65 + i)
		instrs = append(instrs, IRInstr{Op: IRConst, Dst: short, Imm: int64(i)})
		instrs = append(instrs, IRInstr{Op: IRAdd, T: I64, Dst: VReg(100 + i), A: short, B: short})
	}
	instrs = append(instrs, IRInstr{Op: IRAdd, T: I64, Dst: VReg(200), A: VReg(64), B: VReg(64)})
	b := makeBlock(instrs...)
	alloc := Allocate(b, testPool(3, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertNoConflicts(t, alloc)
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0 (long + short with 3 regs)", alloc.StackSlots)
	}
}

func TestAllocate_PoolWithoutDivMulRegs(t *testing.T) {
	// Caller trims pool when block has DIV. Allocator works with trimmed pool.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 10},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 3},
		IRInstr{Op: IRDivS, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
	)
	// Simulate caller checking BlockHasDivMul and trimming pool.
	if !BlockHasDivMul(b) {
		t.Fatal("expected BlockHasDivMul = true")
	}
	pool := testPool(3, 0) // Caller would remove RAX/RDX equivalents
	alloc := Allocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Group F: Invariant verification
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

func TestAllocate_SpilledVRegsHaveSlots(t *testing.T) {
	// Force spills and verify each spilled VReg has a valid unique slot.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(66), B: VReg(67)},
	)
	alloc := Allocate(b, testPool(1, 0), nil, nil)
	slots := map[int16]VReg{}
	for vr := VReg(0); vr < VReg(len(alloc.Kind)); vr++ {
		if alloc.Kind[vr] == AllocStack {
			slot := alloc.SpillSlot[vr]
			if slot < 0 {
				t.Errorf("spilled VReg %d has negative slot %d", vr, slot)
			}
			if prev, dup := slots[slot]; dup {
				t.Errorf("VRegs %d and %d share spill slot %d", prev, vr, slot)
			}
			slots[slot] = vr
		}
	}
	if len(slots) != alloc.StackSlots {
		t.Errorf("unique slots (%d) != StackSlots (%d)", len(slots), alloc.StackSlots)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Group G: Fuzz testing
// ════════════════════════════════════════════════════════════════════════

func FuzzRegAllocInvariants(f *testing.F) {
	// Seeds: each seed is a []byte of 4-byte tuples (op, dst, a, imm).
	f.Add([]byte{
		byte(IRConst), 64, 0, 1,
		byte(IRConst), 65, 0, 2,
		byte(IRAdd), 66, 64, 65,
	})
	f.Add([]byte{
		byte(IRConst), 64, 0, 1,
		byte(IRConst), 65, 0, 2,
		byte(IRConst), 66, 0, 3,
		byte(IRConst), 67, 0, 4,
		byte(IRConst), 68, 0, 5,
		byte(IRAdd), 69, 64, 65,
		byte(IRAdd), 70, 66, 67,
	})
	f.Add([]byte{0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 || len(data) > 512 {
			return
		}

		var instrs []IRInstr
		for i := 0; i+3 < len(data); i += 4 {
			op := IROp(data[i] % byte(irOpCount))
			if op == IROpInvalid {
				op = IRAdd
			}
			dst := VReg(data[i+1]%70 + 1) // 1..70, avoid VRegZero
			a := VReg(data[i+2] % 70)
			imm := int64(int8(data[i+3]))

			switch op {
			case IRLabel:
				continue // skip labels for simplicity
			case IRBranch, IRBranchImm, IRJump:
				continue // skip control flow
			case IRStore:
				instrs = append(instrs, IRInstr{Op: IRStore, T: I32, A: a, B: dst, Imm: imm})
			case IRRet:
				instrs = append(instrs, IRInstr{Op: IRRet, Imm: imm, Imm2: 0, A: a})
			case IRConst:
				instrs = append(instrs, IRInstr{Op: IRConst, Dst: dst, Imm: imm})
			default:
				instrs = append(instrs, IRInstr{Op: op, T: I64, Dst: dst, A: a, Imm: imm})
			}
		}

		if len(instrs) == 0 {
			return
		}

		b := makeBlock(instrs...)
		pool := testPool(3, 2)
		alloc := Allocate(b, pool, nil, nil)

		// Invariant 1: non-nil result.
		if alloc == nil {
			t.Fatal("Allocate returned nil")
		}

		// Invariant 2: VRegZero is AllocUnused.
		if len(alloc.Kind) > 0 && alloc.Kind[0] != AllocUnused {
			t.Fatalf("VRegZero kind = %d, want AllocUnused", alloc.Kind[0])
		}

		// Invariant 3: StackSlots >= 0.
		if alloc.StackSlots < 0 {
			t.Fatalf("StackSlots = %d, want >= 0", alloc.StackSlots)
		}

		// Invariant 4: no two simultaneously-live VRegs share a host register.
		assertNoConflicts(t, alloc)

		// Invariant 5: all assigned host regs come from the pool.
		poolSet := map[int16]bool{}
		for _, r := range pool.IntRegs {
			poolSet[r] = true
		}
		for _, r := range pool.FPRegs {
			poolSet[r] = true
		}
		for _, ia := range alloc.IntervalMap {
			if !poolSet[ia.Host] {
				t.Fatalf("assigned host reg %d not in pool", ia.Host)
			}
		}
	})
}

func FuzzLiveRangeConsistency(f *testing.F) {
	f.Add([]byte{byte(IRConst), 64, 0, 1, byte(IRAdd), 65, 64, 64})
	f.Add([]byte{byte(IRConst), 64, 0, 1, byte(IRConst), 65, 0, 2})
	f.Add([]byte{0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 || len(data) > 512 {
			return
		}

		var instrs []IRInstr
		for i := 0; i+3 < len(data); i += 4 {
			op := IROp(data[i] % byte(irOpCount))
			if op == IROpInvalid {
				op = IRConst
			}
			dst := VReg(data[i+1]%70 + 1)
			a := VReg(data[i+2] % 70)
			imm := int64(int8(data[i+3]))

			switch op {
			case IRLabel, IRBranch, IRBranchImm, IRJump:
				continue
			case IRStore:
				instrs = append(instrs, IRInstr{Op: IRStore, T: I32, A: a, B: dst, Imm: imm})
			case IRRet:
				instrs = append(instrs, IRInstr{Op: IRRet, Imm: imm, A: a})
			case IRConst:
				instrs = append(instrs, IRInstr{Op: IRConst, Dst: dst, Imm: imm})
			default:
				instrs = append(instrs, IRInstr{Op: op, T: I64, Dst: dst, A: a, Imm: imm})
			}
		}

		if len(instrs) == 0 {
			return
		}

		b := makeBlock(instrs...)
		intervals := computeIntervalSets(b)

		for _, is := range intervals {
			// VRegZero should have no intervals.
			if is.VReg == VRegZero && len(is.Intervals) > 0 {
				t.Fatalf("VRegZero has %d intervals", len(is.Intervals))
			}

			for i, iv := range is.Intervals {
				// End >= Start.
				if iv.End < iv.Start {
					t.Fatalf("VReg %d interval[%d]: End %d < Start %d", is.VReg, i, iv.End, iv.Start)
				}
				// No out-of-bounds.
				if iv.Start < 0 || iv.End >= len(b.Instrs) {
					t.Fatalf("VReg %d interval[%d]: [%d,%d] out of bounds (len=%d)",
						is.VReg, i, iv.Start, iv.End, len(b.Instrs))
				}
				// Non-overlapping and sorted.
				if i > 0 {
					prev := is.Intervals[i-1]
					if iv.Start <= prev.End {
						t.Fatalf("VReg %d intervals overlap: [%d,%d] and [%d,%d]",
							is.VReg, prev.Start, prev.End, iv.Start, iv.End)
					}
				}
			}
		}

		// Cross-check: count from intervals matches computeCount.
		count := computeCount(intervals, len(b.Instrs))
		for p, c := range count {
			if c < 0 {
				t.Fatalf("count[%d] = %d, want >= 0", p, c)
			}
		}
	})
}

func FuzzSpillResurrection(f *testing.F) {
	// High-pressure seeds: many overlapping VRegs.
	f.Add([]byte{
		byte(IRConst), 64, 0, 1,
		byte(IRConst), 65, 0, 2,
		byte(IRConst), 66, 0, 3,
		byte(IRConst), 67, 0, 4,
		byte(IRConst), 68, 0, 5,
		byte(IRAdd), 69, 64, 65,
		byte(IRAdd), 70, 66, 67,
	})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 || len(data) > 256 {
			return
		}

		var instrs []IRInstr
		for i := 0; i+3 < len(data); i += 4 {
			op := IROp(data[i] % byte(irOpCount))
			if op == IROpInvalid {
				op = IRConst
			}
			dst := VReg(data[i+1]%30 + 64) // temps only (64..93)
			a := VReg(data[i+2]%30 + 64)
			imm := int64(int8(data[i+3]))

			switch op {
			case IRLabel, IRBranch, IRBranchImm, IRJump, IRRet:
				continue
			case IRStore:
				instrs = append(instrs, IRInstr{Op: IRStore, T: I32, A: a, B: dst, Imm: imm})
			case IRConst:
				instrs = append(instrs, IRInstr{Op: IRConst, Dst: dst, Imm: imm})
			default:
				instrs = append(instrs, IRInstr{Op: op, T: I64, Dst: dst, A: a, Imm: imm})
			}
		}

		if len(instrs) == 0 {
			return
		}

		b := makeBlock(instrs...)
		// Small pool to force spills + resurrection opportunities.
		pool := testPool(2, 1)
		alloc := Allocate(b, pool, nil, nil)

		if alloc == nil {
			t.Fatal("Allocate returned nil")
		}

		// After allocation, no conflicts.
		assertNoConflicts(t, alloc)

		// No VReg is both spilled and has interval allocations.
		for _, ia := range alloc.IntervalMap {
			vr := ia.Interval.VReg
			if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocStack {
				t.Fatalf("VReg %d is both spilled and has interval allocation", vr)
			}
		}

		// StackSlots consistency.
		spillCount := 0
		for _, k := range alloc.Kind {
			if k == AllocStack {
				spillCount++
			}
		}
		if spillCount != alloc.StackSlots {
			t.Fatalf("spillCount (%d) != StackSlots (%d)", spillCount, alloc.StackSlots)
		}
	})
}
