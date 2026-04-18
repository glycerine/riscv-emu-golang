package ir

import "testing"

func TestMaskedLoad_Basic(t *testing.T) {
	e := NewEmitter()
	faultLabel := e.NewLabel()
	dst := VReg(5)
	base := VReg(10)

	e.MaskedLoad(dst, base, e.MemBase(), e.MemMask(), 16, 4, true, faultLabel)

	// Expected sequence:
	//   AddImm addr, base, 16
	//   AddImm tmp1, addr, 3       (width-1)
	//   Or     tmp1, addr, tmp1
	//   Not    maskNot, memMask
	//   And    tmp1, tmp1, maskNot
	//   Branch tmp1, v0, NE, faultLabel
	//   And    masked, addr, memMask
	//   Add    host, memBase, masked
	//   Load   dst, host, 0, I32
	//   Sext   dst, dst, I32        (signed)
	n := len(e.Block.Instrs)
	if n < 9 {
		t.Fatalf("MaskedLoad produced %d instrs, want >= 9", n)
	}

	// Verify first: AddImm for addr calculation.
	if e.Block.Instrs[0].Op != IRAddImm || e.Block.Instrs[0].Imm != 16 {
		t.Errorf("instr[0] = %+v, want AddImm +16", e.Block.Instrs[0])
	}

	// Verify the Branch instruction exists with NE predicate.
	foundBranch := false
	for i, ins := range e.Block.Instrs {
		if ins.Op == IRBranch && ins.Pred == NE {
			foundBranch = true
			if ins.Imm != int64(faultLabel) {
				t.Errorf("instr[%d] branch target = %d, want %d", i, ins.Imm, faultLabel)
			}
			break
		}
	}
	if !foundBranch {
		t.Error("MaskedLoad should contain a Branch NE for OOB check")
	}

	// Verify there's a Load and Sext at the end.
	if e.Block.Instrs[n-2].Op != IRLoad || e.Block.Instrs[n-2].T != I32 {
		t.Errorf("instr[%d] = %+v, want Load I32", n-2, e.Block.Instrs[n-2])
	}
	if e.Block.Instrs[n-1].Op != IRSext || e.Block.Instrs[n-1].T != I32 {
		t.Errorf("instr[%d] = %+v, want Sext I32", n-1, e.Block.Instrs[n-1])
	}
}

func TestMaskedLoad_Unsigned(t *testing.T) {
	e := NewEmitter()
	faultLabel := e.NewLabel()
	e.MaskedLoad(VReg(5), VReg(10), e.MemBase(), e.MemMask(), 0, 2, false, faultLabel)

	n := len(e.Block.Instrs)
	// Last instr should be Zext (unsigned).
	if e.Block.Instrs[n-1].Op != IRZext || e.Block.Instrs[n-1].T != I16 {
		t.Errorf("last instr = %+v, want Zext I16", e.Block.Instrs[n-1])
	}
}

func TestMaskedLoad_I64_NoExtend(t *testing.T) {
	e := NewEmitter()
	faultLabel := e.NewLabel()
	e.MaskedLoad(VReg(5), VReg(10), e.MemBase(), e.MemMask(), 0, 8, false, faultLabel)

	n := len(e.Block.Instrs)
	// I64 load: last instr is IRLoad, no extension.
	if e.Block.Instrs[n-1].Op != IRLoad || e.Block.Instrs[n-1].T != I64 {
		t.Errorf("last instr = %+v, want Load I64", e.Block.Instrs[n-1])
	}
}

func TestGuestStore_Basic(t *testing.T) {
	e := NewEmitter()
	faultLabel := e.NewLabel()
	e.GuestStore(VReg(10), e.MemBase(), e.MemMask(), 8, VReg(5), 4, faultLabel)

	// Should contain AddImm, OOB check (Branch NE), And, Add, Store.
	foundStore := false
	foundBranch := false
	for _, ins := range e.Block.Instrs {
		if ins.Op == IRStore && ins.T == I32 {
			foundStore = true
		}
		if ins.Op == IRBranch && ins.Pred == NE {
			foundBranch = true
		}
	}
	if !foundBranch {
		t.Error("GuestStore should contain Branch NE for OOB check")
	}
	if !foundStore {
		t.Error("GuestStore should contain an IRStore")
	}
}

func TestWriteBackAll_NothingDirty(t *testing.T) {
	e := NewEmitter()
	before := len(e.Block.Instrs)
	e.WriteBackAll()
	after := len(e.Block.Instrs)
	if after != before {
		t.Errorf("WriteBackAll with no dirty regs emitted %d instrs", after-before)
	}
}

func TestWriteBackAll_SomeDirty(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(5))
	e.MarkDirty(VReg(10))
	before := len(e.Block.Instrs)
	e.WriteBackAll()
	after := len(e.Block.Instrs)

	stores := after - before
	if stores != 2 {
		t.Errorf("WriteBackAll with 2 dirty regs emitted %d stores, want 2", stores)
	}
	// Both should be IRStore to xBase.
	for i := before; i < after; i++ {
		ins := e.Block.Instrs[i]
		if ins.Op != IRStore || ins.A != e.xBase {
			t.Errorf("instr[%d] = %+v, want Store to xBase", i, ins)
		}
	}
}

func TestWriteBackAll_IntegerAndFP(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(5))  // x5
	e.MarkDirty(VReg(33)) // f1
	before := len(e.Block.Instrs)
	e.WriteBackAll()
	after := len(e.Block.Instrs)

	if after-before != 2 {
		t.Fatalf("expected 2 stores, got %d", after-before)
	}

	// First store: x5 -> xBase + 5*8.
	s0 := e.Block.Instrs[before]
	if s0.A != e.xBase || s0.Imm != 40 || s0.B != VReg(5) {
		t.Errorf("x5 writeback: %+v", s0)
	}
	// Second store: f1 -> fBase + 1*8.
	s1 := e.Block.Instrs[before+1]
	if s1.A != e.fBase || s1.Imm != 8 || s1.B != VReg(33) {
		t.Errorf("f1 writeback: %+v", s1)
	}
}

func TestWriteBackReg(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(5))
	before := len(e.Block.Instrs)
	e.WriteBackReg(VReg(5))
	after := len(e.Block.Instrs)

	if after-before != 1 {
		t.Fatalf("WriteBackReg emitted %d instrs, want 1", after-before)
	}
	ins := e.Block.Instrs[before]
	if ins.Op != IRStore || ins.A != e.xBase || ins.B != VReg(5) || ins.Imm != 40 {
		t.Errorf("got %+v", ins)
	}
	if e.IsDirty(VReg(5)) {
		t.Error("VReg(5) should be clean after WriteBackReg")
	}
}

func TestWriteBackReg_FP(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(32)) // f0
	before := len(e.Block.Instrs)
	e.WriteBackReg(VReg(32))
	after := len(e.Block.Instrs)

	if after-before != 1 {
		t.Fatalf("emitted %d instrs", after-before)
	}
	ins := e.Block.Instrs[before]
	if ins.A != e.fBase || ins.Imm != 0 {
		t.Errorf("f0 writeback: %+v", ins)
	}
}

func TestWriteBackReg_VRegZero(t *testing.T) {
	e := NewEmitter()
	before := len(e.Block.Instrs)
	e.WriteBackReg(VRegZero)
	if len(e.Block.Instrs) != before {
		t.Error("WriteBackReg(VRegZero) should be a no-op")
	}
}

func TestFaultExit(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(5))
	faultAddr := e.Tmp()
	before := len(e.Block.Instrs)
	e.FaultExit(0x80001000, 3, faultAddr)
	after := len(e.Block.Instrs)

	// Should have: 1 store (writeback x5) + 1 IRRet.
	if after-before < 2 {
		t.Fatalf("FaultExit emitted %d instrs, want >= 2", after-before)
	}
	last := e.Block.Instrs[after-1]
	if last.Op != IRRet || last.Imm != 0x80001000 || last.Imm2 != 3 || last.A != faultAddr {
		t.Errorf("IRRet = %+v", last)
	}
}

func TestBudgetCheck(t *testing.T) {
	e := NewEmitter()
	target := e.NewLabel()
	e.PlaceLabel(target)

	e.MarkDirty(VReg(3))
	before := len(e.Block.Instrs)
	e.BudgetCheck(target, 0x80002000)
	after := len(e.Block.Instrs)

	// Expected sequence:
	//   BranchImm ic, MaxIC, GE, tooBig
	//   Jump target
	//   Label tooBig
	//   Store (writeback x3)
	//   Ret(targetPC, 0, VRegZero)
	if after-before < 5 {
		t.Fatalf("BudgetCheck emitted %d instrs, want >= 5", after-before)
	}

	bi := e.Block.Instrs[before]
	if bi.Op != IRBranchImm || bi.Pred != GE || bi.Imm2 != int64(MaxIC) {
		t.Errorf("BranchImm = %+v", bi)
	}
	if bi.A != e.ic {
		t.Errorf("BranchImm.A = %v, want ic=%v", bi.A, e.ic)
	}

	jmp := e.Block.Instrs[before+1]
	if jmp.Op != IRJump || jmp.Imm != int64(target) {
		t.Errorf("Jump = %+v", jmp)
	}

	lbl := e.Block.Instrs[before+2]
	if lbl.Op != IRLabel {
		t.Errorf("expected Label, got %v", lbl.Op)
	}

	ret := e.Block.Instrs[after-1]
	if ret.Op != IRRet || ret.Imm != 0x80002000 || ret.Imm2 != 0 {
		t.Errorf("Ret = %+v", ret)
	}
}

func TestMarkDirty_Basic(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(5))
	if !e.IsDirty(VReg(5)) {
		t.Error("VReg(5) should be dirty")
	}
}

func TestMarkDirty_VRegZero(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VRegZero)
	if e.IsDirty(VRegZero) {
		t.Error("VRegZero should never be dirty")
	}
}

func TestIsDirty_OutOfRange(t *testing.T) {
	e := NewEmitter()
	// Query a VReg way beyond dirty slice.
	if e.IsDirty(VReg(9999)) {
		t.Error("out-of-range VReg should not be dirty")
	}
}

func TestMarkDirty_GrowsSlice(t *testing.T) {
	e := NewEmitter()
	// Allocate many temps to push past initial dirty size.
	var last VReg
	for i := 0; i < 200; i++ {
		last = e.Tmp()
	}
	// Should not panic.
	e.MarkDirty(last)
	if !e.IsDirty(last) {
		t.Errorf("VReg(%d) should be dirty after MarkDirty", last)
	}
}

func TestWriteBackAll_PreservesDirty(t *testing.T) {
	e := NewEmitter()
	e.MarkDirty(VReg(5))
	e.MarkDirty(VReg(33))
	e.WriteBackAll()
	// Dirty flags are preserved — multiple exit points each need writeback.
	if !e.IsDirty(VReg(5)) {
		t.Error("VReg(5) should remain dirty after WriteBackAll")
	}
	if !e.IsDirty(VReg(33)) {
		t.Error("VReg(33) should remain dirty after WriteBackAll")
	}
	// Second call emits the same stores again (each exit needs writeback).
	before := len(e.Block.Instrs)
	e.WriteBackAll()
	if len(e.Block.Instrs) == before {
		t.Error("second WriteBackAll should also emit stores")
	}
}

func TestMaxIC(t *testing.T) {
	if MaxIC != 4096 {
		t.Errorf("MaxIC = %d, want 4096", MaxIC)
	}
}

// End-to-end: simulate ADDI x1, x0, 42 ; SW x1, 0(x2) ; ECALL
func TestEndToEnd_SimpleBlock(t *testing.T) {
	e := NewEmitter()

	// ADDI x1, x0, 42  — x0 is always zero, so this is Const(x1, 42).
	e.Const(e.XReg(1), 42)

	// SW x1, 0(x2) — guest store with bounds check.
	faultLabel := e.NewLabel()
	e.GuestStore(e.XReg(2), e.MemBase(), e.MemMask(), 0, e.XReg(1), 4, faultLabel)

	// ECALL — writeback + return status=1 (ecall).
	e.FaultExit(0x80001008, 1, VRegZero)

	// Place the fault label body.
	e.PlaceLabel(faultLabel)
	e.FaultExit(0x80001004, 4, e.Tmp()) // store fault

	n := len(e.Block.Instrs)
	if n < 5 {
		t.Errorf("end-to-end block has %d instrs, expected more", n)
	}

	// Verify the block has at least one IRConst, one IRStore, and two IRRet.
	counts := make(map[IROp]int)
	for _, ins := range e.Block.Instrs {
		counts[ins.Op]++
	}
	if counts[IRConst] < 1 {
		t.Error("expected at least 1 IRConst")
	}
	if counts[IRStore] < 1 {
		t.Error("expected at least 1 IRStore")
	}
	if counts[IRRet] < 2 {
		t.Errorf("expected at least 2 IRRet, got %d", counts[IRRet])
	}
}

// End-to-end: loop with backward branch budget check.
func TestEndToEnd_LoopWithBudget(t *testing.T) {
	e := NewEmitter()

	// Label L (loop top).
	loopTop := e.NewLabel()
	e.PlaceLabel(loopTop)

	// ADDI x1, x1, 1
	e.AddImm(e.XReg(1), e.XReg(1), 1)

	// BNE x1, x2, L — backward branch with budget check.
	e.BudgetCheck(loopTop, 0x80001000)

	n := len(e.Block.Instrs)
	if n < 4 {
		t.Errorf("loop block has %d instrs, expected >= 4", n)
	}

	// Should have a BranchImm with GE predicate (budget check).
	found := false
	for _, ins := range e.Block.Instrs {
		if ins.Op == IRBranchImm && ins.Pred == GE {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected BranchImm GE for budget check")
	}
}
