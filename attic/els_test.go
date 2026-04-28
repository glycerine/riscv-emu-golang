package riscv

import "testing"

// ── Forward branch liveness tests ──────────────────────────────────────────

// TestELS_ForwardBranch_GuestRegLive tests the bug case: a forward conditional
// branch targets a label where a guest reg is used (e.g. WriteBackAll in a fault
// handler). The guest reg must be live at the branch point.
func TestELS_ForwardBranch_GuestRegLive(t *testing.T) {
	// Block:
	//  [0] const x5 = 100       ; def x5
	//  [1] const t64 = 0        ; dummy
	//  [2] add t65 = t64, t64   ; dummy
	//  [3] branch.ne t65, v0 -> L0  ; forward branch to [8]
	//  [4] add t66 = t64, t64   ; fall-through path
	//  [5] add t67 = t64, t64
	//  [6] add t68 = t64, t64
	//  [7] jump -> L1
	//  [8] L0:                  ; branch target
	//  [9] store [t64+40] = x5  ; uses x5 at the branch target
	// [10] ret
	// [11] L1:
	// [12] add x5 = x5, t64    ; uses x5 on fall-through
	// [13] store [t64+40] = x5
	// [14] ret

	b := makeBlock(
		IRInstr{Op: IRConst, Dst: 5, Imm: 100},                                         // [0]
		IRInstr{Op: IRConst, Dst: VRegTempStart, Imm: 0},                               // [1]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 1, A: VRegTempStart, B: VRegTempStart}, // [2]
		IRInstr{Op: IRBranch, A: VRegTempStart + 1, B: VRegZero, Pred: NE, Imm: 0},     // [3] -> L0
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 2, A: VRegTempStart, B: VRegTempStart}, // [4]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 3, A: VRegTempStart, B: VRegTempStart}, // [5]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 4, A: VRegTempStart, B: VRegTempStart}, // [6]
		IRInstr{Op: IRJump, Imm: 1},                                                    // [7] -> L1
		IRInstr{Op: IRLabel, Imm: 0},                                                   // [8] L0
		IRInstr{Op: IRStore, T: I64, A: VRegTempStart, B: 5, Imm: 40},                  // [9] uses x5
		IRInstr{Op: IRRet, Imm: 0x1000},                                                // [10]
		IRInstr{Op: IRLabel, Imm: 1},                                                   // [11] L1
		IRInstr{Op: IRAdd, Dst: 5, A: 5, B: VRegTempStart},                             // [12] uses x5
		IRInstr{Op: IRStore, T: I64, A: VRegTempStart, B: 5, Imm: 40},                  // [13]
		IRInstr{Op: IRRet, Imm: 0x1000},                                                // [14]
	)
	b.Labels[0] = 8  // L0 at index 8
	b.Labels[1] = 11 // L1 at index 11

	intervals := computeIntervalSets(b)

	// x5 must be live at instruction 3 (the forward branch point) because
	// the branch target at [8] leads to [9] where x5 is used.
	// Without forward branch extension, x5 has interval [0,0] (dead def)
	// with a gap until [9]. With the fix, the interval covers [0..9+].
	x5Intervals := intervals[5].Intervals
	if len(x5Intervals) == 0 {
		t.Fatal("x5 has no intervals")
	}

	// Check that x5 is live at instruction 3 (the branch point).
	liveAtBranch := false
	for _, iv := range x5Intervals {
		if iv.Start <= 3 && 3 <= iv.End {
			liveAtBranch = true
			break
		}
	}
	if !liveAtBranch {
		t.Errorf("x5 not live at instruction 3 (forward branch point); intervals: %v", x5Intervals)
	}

	// Verify allocation has no conflicts.
	pool := testPool(4, 0)
	alloc := NewAllocator().Allocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
}

// TestELS_ForwardBranch_TempNotExtended verifies temps NOT live at a branch
// target are not extended by the forward branch extension.
func TestELS_ForwardBranch_TempNotExtended(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VRegTempStart, Imm: 0},                                   // [0] def t64
		IRInstr{Op: IRConst, Dst: VRegTempStart + 1, Imm: 1},                               // [1] def t65
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 2, A: VRegTempStart, B: VRegTempStart + 1}, // [2] use t64,t65
		IRInstr{Op: IRBranch, A: VRegTempStart + 2, B: VRegZero, Pred: NE, Imm: 0},         // [3] -> L0
		IRInstr{Op: IRConst, Dst: VRegTempStart + 3, Imm: 99},                              // [4]
		IRInstr{Op: IRRet, Imm: 0x1000},                                                    // [5]
		IRInstr{Op: IRLabel, Imm: 0},                                                       // [6] L0
		IRInstr{Op: IRRet, Imm: 0x2000},                                                    // [7]
	)
	b.Labels[0] = 6

	intervals := computeIntervalSets(b)

	// t65 is NOT live at the branch target (instruction 6).
	// Its interval should be [1, 2] only.
	t65Intervals := intervals[VRegTempStart+1].Intervals
	for _, iv := range t65Intervals {
		if iv.Start <= 6 && 6 <= iv.End {
			t.Errorf("t65 should not be live at branch target; intervals: %v", t65Intervals)
		}
	}
}

// TestELS_ForwardBranch_MultipleTargets tests multiple forward branches
// targeting different labels, all with a guest reg live at the targets.
func TestELS_ForwardBranch_MultipleTargets(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: 5, Imm: 42},                                      // [0] def x5
		IRInstr{Op: IRConst, Dst: VRegTempStart, Imm: 0},                           // [1]
		IRInstr{Op: IRBranch, A: VRegTempStart, B: VRegZero, Pred: NE, Imm: 0},     // [2] -> L0
		IRInstr{Op: IRConst, Dst: VRegTempStart + 1, Imm: 0},                       // [3]
		IRInstr{Op: IRBranch, A: VRegTempStart + 1, B: VRegZero, Pred: NE, Imm: 1}, // [4] -> L1
		IRInstr{Op: IRAdd, Dst: 5, A: 5, B: VRegTempStart},                         // [5] use x5
		IRInstr{Op: IRRet, Imm: 0x1000},                                            // [6]
		IRInstr{Op: IRLabel, Imm: 0},                                               // [7] L0
		IRInstr{Op: IRStore, T: I64, A: VRegTempStart, B: 5, Imm: 40},              // [8] use x5
		IRInstr{Op: IRRet, Imm: 0x1000},                                            // [9]
		IRInstr{Op: IRLabel, Imm: 1},                                               // [10] L1
		IRInstr{Op: IRStore, T: I64, A: VRegTempStart, B: 5, Imm: 40},              // [11] use x5
		IRInstr{Op: IRRet, Imm: 0x1000},                                            // [12]
	)
	b.Labels[0] = 7
	b.Labels[1] = 10

	intervals := computeIntervalSets(b)

	// x5 must be live at both branch points (instructions 2 and 4).
	for _, branchIdx := range []int{2, 4} {
		live := false
		for _, iv := range intervals[5].Intervals {
			if iv.Start <= branchIdx && branchIdx <= iv.End {
				live = true
				break
			}
		}
		if !live {
			t.Errorf("x5 not live at branch point %d; intervals: %v", branchIdx, intervals[5].Intervals)
		}
	}

	pool := testPool(3, 0)
	alloc := NewAllocator().Allocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
}

// TestELS_GuestRegHole_BetweenRedefs verifies that guest registers CAN have
// holes between redefinitions — this is valid and preserves ELS efficiency.
func TestELS_GuestRegHole_BetweenRedefs(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: 5, Imm: 10},                              // [0] def x5
		IRInstr{Op: IRConst, Dst: VRegTempStart, Imm: 0},                   // [1]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 1, A: 5, B: VRegTempStart}, // [2] use x5
		IRInstr{Op: IRConst, Dst: VRegTempStart + 2, Imm: 1},               // [3]
		IRInstr{Op: IRConst, Dst: VRegTempStart + 3, Imm: 2},               // [4]
		IRInstr{Op: IRConst, Dst: 5, Imm: 20},                              // [5] redef x5 (new value)
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 4, A: 5, B: VRegTempStart}, // [6] use x5
		IRInstr{Op: IRStore, T: I64, A: VRegTempStart, B: 5, Imm: 40},      // [7] use x5
		IRInstr{Op: IRRet, Imm: 0x1000},                                    // [8]
	)

	intervals := computeIntervalSets(b)

	// x5 should have TWO intervals: [0, 2] and [5, n-1].
	// The hole at [3, 4] is valid — x5 holds a dead value there.
	x5Intervals := intervals[5].Intervals
	if len(x5Intervals) != 2 {
		t.Errorf("x5 should have 2 intervals (hole between redefs), got %d: %v",
			len(x5Intervals), x5Intervals)
	}

	pool := testPool(3, 0)
	alloc := NewAllocator().Allocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
}

// TestELS_BackwardBranch_LoopExtension tests that VRegs in a loop body have
// intervals spanning the full loop range.
func TestELS_BackwardBranch_LoopExtension(t *testing.T) {
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: 1, Imm: 0},                         // [0] x1 = 0
		IRInstr{Op: IRConst, Dst: 2, Imm: 10},                        // [1] x2 = 10
		IRInstr{Op: IRLabel, Imm: 0},                                 // [2] L0 (loop top)
		IRInstr{Op: IRAdd, Dst: 1, A: 1, B: 2},                       // [3] x1 += x2
		IRInstr{Op: IRBranch, A: 1, B: VRegZero, Pred: NE, Imm: 0},   // [4] -> L0
		IRInstr{Op: IRStore, T: I64, A: VRegTempStart, B: 1, Imm: 8}, // [5]
		IRInstr{Op: IRRet, Imm: 0x1000},                              // [6]
	)
	b.Labels[0] = 2

	intervals := computeIntervalSets(b)

	// x2 must be live across the loop body [2, 4].
	x2Live := false
	for _, iv := range intervals[2].Intervals {
		if iv.Start <= 2 && 4 <= iv.End {
			x2Live = true
			break
		}
	}
	if !x2Live {
		t.Errorf("x2 not live across loop body [2,4]; intervals: %v", intervals[2].Intervals)
	}

	pool := testPool(3, 0)
	alloc := NewAllocator().Allocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)
}

// TestELS_MaskedLoadPattern tests the real-world MaskedLoad + fault handler
// pattern via the Emitter, verifying no allocation conflicts.
func TestELS_MaskedLoadPattern(t *testing.T) {
	e := NewEmitter()

	base := e.XReg(10)
	e.Const(base, 0x2000)
	e.MarkDirty(base)

	faultLabel := e.NewLabel()
	dst := e.XReg(11)
	e.MaskedLoad(dst, base, e.MemBase(), e.MemMask(), 0, 8, false, faultLabel)
	e.MarkDirty(dst)

	// Normal exit.
	e.WriteBackAll()
	e.Ret(0x1000, 0, VRegZero)

	// Fault handler.
	e.PlaceLabel(faultLabel)
	e.WriteBackAll()
	e.Ret(0x1000, 3, VRegZero)

	pool := AMD64Pool(e.Block)
	pinned := AMD64Pinned()
	alloc := NewAllocator().Allocate(e.Block, pool, pinned, nil)
	assertNoConflicts(t, alloc)

	// Verify x10 and x11 are allocated (not unused).
	assertAllocReg(t, alloc, 10)
	assertAllocReg(t, alloc, 11)
}

// TestELS_TwoMaskedLoads tests two MaskedLoad+GuestStore in the same block.
func TestELS_TwoMaskedLoads(t *testing.T) {
	e := NewEmitter()

	base := e.XReg(10)
	e.Const(base, 0x2000)
	e.MarkDirty(base)

	src := e.XReg(11)
	e.Const(src, 42)
	e.MarkDirty(src)

	storeFault := e.NewLabel()
	e.GuestStore(base, e.MemBase(), e.MemMask(), 0, src, 4, storeFault)

	loadFault := e.NewLabel()
	dst := e.XReg(12)
	e.MaskedLoad(dst, base, e.MemBase(), e.MemMask(), 0, 4, true, loadFault)
	e.MarkDirty(dst)

	e.WriteBackAll()
	e.Ret(0x1000, 0, VRegZero)

	e.PlaceLabel(loadFault)
	e.WriteBackAll()
	e.Ret(0x1000, 3, VRegZero)

	e.PlaceLabel(storeFault)
	e.WriteBackAll()
	e.Ret(0x1000, 4, VRegZero)

	pool := AMD64Pool(e.Block)
	pinned := AMD64Pinned()
	alloc := NewAllocator().Allocate(e.Block, pool, pinned, nil)
	assertNoConflicts(t, alloc)
}

// TestELS_SpillWithHoles tests spilling with interval holes.
func TestELS_SpillWithHoles(t *testing.T) {
	// 6 VRegs, only 2 pool regs → heavy spilling.
	b := makeBlock(
		IRInstr{Op: IRConst, Dst: VRegTempStart, Imm: 1},                                       // [0]
		IRInstr{Op: IRConst, Dst: VRegTempStart + 1, Imm: 2},                                   // [1]
		IRInstr{Op: IRConst, Dst: VRegTempStart + 2, Imm: 3},                                   // [2]
		IRInstr{Op: IRConst, Dst: VRegTempStart + 3, Imm: 4},                                   // [3]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 4, A: VRegTempStart, B: VRegTempStart + 1},     // [4]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 5, A: VRegTempStart + 2, B: VRegTempStart + 3}, // [5]
		IRInstr{Op: IRAdd, Dst: VRegTempStart + 6, A: VRegTempStart + 4, B: VRegTempStart + 5}, // [6]
		IRInstr{Op: IRRet, A: VRegTempStart + 6, Imm: 0x1000},                                  // [7]
	)

	pool := testPool(2, 0)
	alloc := NewAllocator().Allocate(b, pool, nil, nil)
	assertNoConflicts(t, alloc)

	// Verify spill slots are unique.
	slots := make(map[int16]VReg)
	for vr := VReg(0); vr < VReg(len(alloc.Kind)); vr++ {
		if alloc.Kind[vr] == AllocStack {
			slot := alloc.SpillSlot[vr]
			if prev, ok := slots[slot]; ok {
				t.Errorf("VReg %d and %d share spill slot %d", prev, vr, slot)
			}
			slots[slot] = vr
		}
	}
}

// TestELS_PinnedRegsExcluded verifies pinned VRegs don't consume pool regs.
func TestELS_PinnedRegsExcluded(t *testing.T) {
	e := NewEmitter()
	x1 := e.XReg(1)
	e.Const(x1, 42)
	e.MarkDirty(x1)
	e.WriteBackAll()
	e.Ret(0x1000, 0, VRegZero)

	pool := AMD64Pool(e.Block)
	pinned := AMD64Pinned()
	alloc := NewAllocator().Allocate(e.Block, pool, pinned, nil)

	// Pinned VRegs should not get pool registers.
	for vr, hostReg := range pinned {
		for _, ia := range alloc.IntervalMap {
			if ia.Interval.VReg != vr && ia.Host == hostReg {
				t.Errorf("non-pinned VReg %d assigned to pinned host reg %d", ia.Interval.VReg, hostReg)
			}
		}
	}
}
