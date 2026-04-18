package ir

import "testing"

func TestPeephole_MovSelf_Deleted(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRMov, T: I64, Dst: VReg(5), A: VReg(5)})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("self-move should be deleted, got %d instrs", len(e.Block.Instrs))
	}
}

func TestPeephole_MovDifferent_Preserved(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRMov, T: I64, Dst: VReg(5), A: VReg(6)})
	if len(e.Block.Instrs) != 1 {
		t.Errorf("different-reg move should be preserved, got %d instrs", len(e.Block.Instrs))
	}
}

func TestPeephole_AddImm0_DstEqualsA_Deleted(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAddImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("AddImm +0 with dst==a should be deleted, got %d instrs", len(e.Block.Instrs))
	}
}

func TestPeephole_AddImm0_DstDiffA_BecomeMov(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAddImm, T: I64, Dst: VReg(5), A: VReg(6), Imm: 0})
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("expected 1 instr, got %d", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRMov || ins.Dst != 5 || ins.A != 6 {
		t.Errorf("expected Mov 5,6, got %+v", ins)
	}
}

func TestPeephole_AddImm0_Cascade(t *testing.T) {
	// AddImm dst=5, a=5, imm=0 -> becomes Mov 5,5 -> deleted.
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAddImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("cascaded rewrite should delete instruction, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_AddImm_NonZero_Preserved(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAddImm, T: I64, Dst: VReg(5), A: VReg(6), Imm: 7})
	if len(e.Block.Instrs) != 1 {
		t.Errorf("non-zero imm should be preserved, got %d instrs", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRAddImm {
		t.Errorf("op should still be IRAddImm, got %v", e.Block.Instrs[0].Op)
	}
}

func TestPeephole_ShlImm0_DstEqualsA(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRShlImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("ShlImm 0 with dst==a should be deleted, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_ShlImm0_DstDiffA(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRShlImm, T: I64, Dst: VReg(5), A: VReg(6), Imm: 0})
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("expected 1 instr, got %d", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRMov {
		t.Errorf("expected Mov, got %v", e.Block.Instrs[0].Op)
	}
}

func TestPeephole_ShrImm0_Deleted(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRShrImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("ShrImm 0 should be deleted, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_SarImm0_Deleted(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRSarImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("SarImm 0 should be deleted, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_AndImm_NegOne_DstEqualsA(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAndImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: -1})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("AndImm -1 with dst==a should be deleted, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_AndImm_NegOne_DstDiffA(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAndImm, T: I64, Dst: VReg(5), A: VReg(6), Imm: -1})
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("expected 1 instr, got %d", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRMov {
		t.Errorf("expected Mov, got %v", e.Block.Instrs[0].Op)
	}
}

func TestPeephole_OrImm0_Deleted(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IROrImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("OrImm 0 should be deleted, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_OrImm0_DiffDst_BecomeMov(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IROrImm, T: I64, Dst: VReg(5), A: VReg(6), Imm: 0})
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("expected 1 instr, got %d", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRMov {
		t.Errorf("expected Mov, got %v", e.Block.Instrs[0].Op)
	}
}

func TestPeephole_XorImm0_Deleted(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRXorImm, T: I64, Dst: VReg(5), A: VReg(5), Imm: 0})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("XorImm 0 should be deleted, got %d", len(e.Block.Instrs))
	}
}

func TestPeephole_XorImm0_DiffDst_BecomeMov(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRXorImm, T: I64, Dst: VReg(5), A: VReg(6), Imm: 0})
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("expected 1 instr, got %d", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRMov {
		t.Errorf("expected Mov, got %v", e.Block.Instrs[0].Op)
	}
}

func TestPeephole_ConstZero_Store_Folded(t *testing.T) {
	e := newTestEmitter()
	// IRConst temp VReg(64) = 0
	e.emit(IRInstr{Op: IRConst, T: I64, Dst: VReg(64), Imm: 0})
	// IRStore [base + 8] = VReg(64)
	e.emit(IRInstr{Op: IRStore, T: I32, A: VReg(10), B: VReg(64), Imm: 8})

	if len(e.Block.Instrs) != 1 {
		t.Fatalf("const-zero + store should fold to 1 instr, got %d", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRStore {
		t.Errorf("expected IRStore, got %v", ins.Op)
	}
	if ins.B != VRegZero {
		t.Errorf("folded store should have B=VRegZero, got %v", ins.B)
	}
	if ins.A != VReg(10) || ins.Imm != 8 || ins.T != I32 {
		t.Errorf("store fields wrong: %+v", ins)
	}
}

func TestPeephole_ConstZero_Store_NotFolded_GuestReg(t *testing.T) {
	e := newTestEmitter()
	// Use a guest reg (< VRegTempStart) — should NOT fold.
	e.emit(IRInstr{Op: IRConst, T: I64, Dst: VReg(5), Imm: 0})
	e.emit(IRInstr{Op: IRStore, T: I32, A: VReg(10), B: VReg(5), Imm: 8})

	if len(e.Block.Instrs) != 2 {
		t.Errorf("guest reg const+store should not fold, got %d instrs", len(e.Block.Instrs))
	}
}

func TestPeephole_ConstNonZero_Store_NotFolded(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRConst, T: I64, Dst: VReg(64), Imm: 42})
	e.emit(IRInstr{Op: IRStore, T: I32, A: VReg(10), B: VReg(64), Imm: 8})

	if len(e.Block.Instrs) != 2 {
		t.Errorf("non-zero const+store should not fold, got %d instrs", len(e.Block.Instrs))
	}
}

func TestPeephole_NoMatch_Preserved(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)})
	if len(e.Block.Instrs) != 1 {
		t.Errorf("non-matching instr should be preserved, got %d", len(e.Block.Instrs))
	}
}

func TestVregUsedLater_GuestReg(t *testing.T) {
	e := newTestEmitter()
	if !e.vregUsedLater(VReg(5), 0) {
		t.Error("guest reg should be considered used later")
	}
}

func TestVregUsedLater_Temp(t *testing.T) {
	e := newTestEmitter()
	if e.vregUsedLater(VReg(64), 0) {
		t.Error("temp reg should NOT be considered used later")
	}
}

func TestPeepholeSz_Value(t *testing.T) {
	if PeepholeSz != 4 {
		t.Errorf("PeepholeSz = %d, want 4", PeepholeSz)
	}
}
