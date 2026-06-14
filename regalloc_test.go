package riscv

import (
	"testing"
)

// ── Test helpers ──

// makeBlock builds a Block from a variadic list of IRInstr.
func makeBlock(instrs ...IRInstr) *Block {
	b := NewBlock()
	b.Instrs = instrs
	MaxVReg(b)
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
// Uses strict overlap (a.Start < b.End && b.Start < a.End); touching endpoints are OK.
func assertNoConflicts(t *testing.T, alloc *Allocation) {
	t.Helper()
	for i := 0; i < len(alloc.IntervalMap); i++ {
		for j := i + 1; j < len(alloc.IntervalMap); j++ {
			a := alloc.IntervalMap[i]
			b := alloc.IntervalMap[j]
			if a.Host == b.Host {
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
// BlockHasDivMul
// ════════════════════════════════════════════════════════════════════════

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

// ════════════════════════════════════════════════════════════════════════
// MaxVReg
// ════════════════════════════════════════════════════════════════════════

func TestMaxVReg_EmptyBlock(t *testing.T) {
	b := makeBlock()
	if got := b.maxVreg; got != 0 {
		t.Errorf("maxVReg(empty) = %d, want 0", got)
	}
}

func TestMaxVReg_HighTemp(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(200), Imm: 1},
	)
	if got := b.maxVreg; got != 200 {
		t.Errorf("maxVReg = %d, want 200", got)
	}
}

func TestMaxVReg_IgnoresBudgetLabels(t *testing.T) {
	const hugeLabel = 500000

	incremental := NewBlock()
	incremental.appendIns(IRInstr{Op: IRConst, T: I64, Dst: VReg(64), Imm: 1})
	incremental.appendIns(IRInstr{Op: IRBudgetReserve, Imm: 1, Dst: VReg(hugeLabel)})
	if got := incremental.maxVreg; got != 64 {
		t.Fatalf("incremental maxVReg with budget label = %d, want 64", got)
	}

	b := makeBlock(
		IRInstr{Op: IRConst, T: I64, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRBudgetReserve, Imm: 1, Dst: VReg(hugeLabel)},
		IRInstr{Op: IRBudgetZero, Dst: VReg(hugeLabel + 1)},
		IRInstr{Op: IRRegBudget, Imm2: 8, Dst: VReg(hugeLabel + 2)},
	)
	if got := b.maxVreg; got != 64 {
		t.Fatalf("maxVReg with budget labels = %d, want 64", got)
	}

	alloc := helperTestAllocate(b, testPool(4, 0), nil, nil)
	if len(alloc.Kind) != 65 {
		t.Fatalf("allocation table len = %d, want 65", len(alloc.Kind))
	}
}

// ════════════════════════════════════════════════════════════════════════
// Basic allocation (fixed static mapping)
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_EmptyBlock(t *testing.T) {
	b := makeBlock()
	alloc := helperTestAllocate(b, testPool(8, 4), nil, nil)
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
	alloc := helperTestAllocate(b, testPool(8, 4), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0", alloc.StackSlots)
	}
}

func TestAllocate_AllFitNoOverlap(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
	)
	alloc := helperTestAllocate(b, testPool(3, 0), nil, nil)
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
	alloc := helperTestAllocate(b, testPool(4, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
	assertAllocReg(t, alloc, VReg(66))
	assertNoConflicts(t, alloc)
}

func TestAllocate_VRegZeroNeverAllocated(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VRegZero, B: VReg(2)},
	)
	alloc := helperTestAllocate(b, testPool(8, 0), nil, nil)
	assertAllocUnused(t, alloc, VRegZero)
}

// ════════════════════════════════════════════════════════════════════════
// Spill behavior
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_OneSpill(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(66), B: VReg(67)},
	)
	alloc := helperTestAllocate(b, testPool(2, 0), nil, nil)
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
	alloc := helperTestAllocate(b, testPool(2, 0), nil, nil)
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
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 4},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(66), B: VReg(67)},
	)
	alloc := helperTestAllocate(b, testPool(1, 0), nil, nil)
	if alloc.StackSlots < 1 {
		t.Errorf("StackSlots = %d, want >= 1", alloc.StackSlots)
	}
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
// FP/int pool separation
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_IntAndFP_SeparatePools(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(65), A: VReg(65), B: VReg(65)},
	)
	alloc := helperTestAllocate(b, testPool(1, 1), nil, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
}

func TestAllocate_FPPressure_IntFree(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(64), A: VReg(32), B: VReg(33)},
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(65), A: VReg(34), B: VReg(35)},
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
	)
	alloc := helperTestAllocate(b, testPool(5, 1), nil, nil)
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
	b := makeBlock(
		IRInstr{Op: IRFAdd, T: F64, Dst: VReg(37), A: VReg(33), B: VReg(34)},
	)
	alloc := helperTestAllocate(b, testPool(4, 4), nil, nil)
	assertAllocReg(t, alloc, VReg(37))
	reg, ok := regAt(alloc, VReg(37), 0)
	if ok && reg < 200 {
		t.Errorf("guest FP reg f5 got int pool reg %d, want FP pool (200+)", reg)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Guest regs live to end
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_GuestRegLiveToEnd(t *testing.T) {
	var instrs []IRInstr
	instrs = append(instrs, IRInstr{Op: IRConst, Dst: VReg(5), Imm: 1})
	for i := 1; i < 10; i++ {
		instrs = append(instrs, IRInstr{Op: IRConst, Dst: VReg(64 + i), Imm: int64(i)})
	}
	b := makeBlock(instrs...)
	alloc := helperTestAllocate(b, testPool(4, 0), nil, nil)
	assertAllocReg(t, alloc, VReg(5))
	_, ok := regAt(alloc, VReg(5), 9)
	if !ok {
		t.Error("x5 should be live at last instruction (guest reg extends to end)")
	}
}

func TestAllocate_GuestRegEvictsTemp(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(5), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 3},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(5), B: VReg(66)},
	)
	alloc := helperTestAllocate(b, testPool(3, 0), nil, nil)
	assertNoConflicts(t, alloc)
	if alloc.StackSlots < 1 {
		t.Errorf("expected >= 1 spill, got StackSlots=%d", alloc.StackSlots)
	}
}

func TestAllocate_TempIntervalsReuseHostReg(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 9},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 10},
	)
	pool := testPool(1, 0)
	pool.TempIntervals = true
	alloc := helperTestAllocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
	if alloc.StackSlots != 0 {
		t.Errorf("StackSlots = %d, want 0 because the temp lifetimes do not overlap", alloc.StackSlots)
	}
	r64, ok := regAt(alloc, VReg(64), 0)
	if !ok {
		t.Fatal("x64 missing host register")
	}
	r66, ok := regAt(alloc, VReg(66), 2)
	if !ok {
		t.Fatal("x66 missing host register")
	}
	if r64 != r66 {
		t.Fatalf("non-overlapping temps used host regs %d and %d, want reuse", r64, r66)
	}
}

// ════════════════════════════════════════════════════════════════════════
// Pinned parameter VRegs
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_PinnedRegs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(64), B: VReg(65)},
	)
	pinned := map[VReg]int16{
		VReg(64): 50,
		VReg(65): 51,
	}
	alloc := helperTestAllocate(b, testPool(4, 0), pinned, nil)
	assertRegAt(t, alloc, VReg(64), 0, 50)
	assertRegAt(t, alloc, VReg(65), 0, 51)
}

func TestAllocate_PinnedRegsNotInPool(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(67), Imm: 2},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(64), B: VReg(66)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(69), A: VReg(65), B: VReg(67)},
	)
	pinned := map[VReg]int16{VReg(64): 50, VReg(65): 51}
	alloc := helperTestAllocate(b, testPool(2, 0), pinned, nil)
	assertAllocReg(t, alloc, VReg(64))
	assertAllocReg(t, alloc, VReg(65))
	assertAllocReg(t, alloc, VReg(66))
	assertAllocReg(t, alloc, VReg(67))
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Register moves
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_NoMovesNeeded(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(65), A: VReg(64), B: VReg(64)},
	)
	alloc := helperTestAllocate(b, testPool(4, 0), nil, nil)
	if len(alloc.Moves) != 0 {
		t.Errorf("expected 0 moves, got %d", len(alloc.Moves))
	}
}

// ════════════════════════════════════════════════════════════════════════
// Edge cases
// ════════════════════════════════════════════════════════════════════════

func TestAllocate_OnlyVRegZeroRefs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRAdd, T: I64, Dst: VRegZero, A: VRegZero, B: VRegZero},
	)
	alloc := helperTestAllocate(b, testPool(4, 0), nil, nil)
	assertAllocUnused(t, alloc, VRegZero)
}

func TestAllocate_PoolWithoutDivMulRegs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 10},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 3},
		IRInstr{Op: IRDivS, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
	)
	if !BlockHasDivMul(b) {
		t.Fatal("expected BlockHasDivMul = true")
	}
	pool := testPool(3, 0)
	alloc := helperTestAllocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
}

// ════════════════════════════════════════════════════════════════════════
// Invariant verification
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
	alloc := helperTestAllocate(b, testPool(3, 0), nil, nil)
	assertNoConflicts(t, alloc)
}

func TestAllocate_AllReferencedVRegsAllocated(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(66), A: VReg(64), B: VReg(65)},
	)
	alloc := helperTestAllocate(b, testPool(4, 0), nil, nil)
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
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VReg(64), Imm: 1},
		IRInstr{Op: IRConst, Dst: VReg(65), Imm: 2},
		IRInstr{Op: IRConst, Dst: VReg(66), Imm: 3},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(67), A: VReg(64), B: VReg(65)},
		IRInstr{Op: IRAdd, T: I64, Dst: VReg(68), A: VReg(66), B: VReg(67)},
	)
	alloc := helperTestAllocate(b, testPool(1, 0), nil, nil)
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
