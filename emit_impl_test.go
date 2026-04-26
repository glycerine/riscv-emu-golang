package riscv

import "testing"

func newTestEmitter() *Emitter {
	return &Emitter{
		Block:   NewBlock(),
		dirty:   make([]bool, initialDirtySize),
		nextTmp: VRegTempStart,
	}
}

func TestOp3_Basic(t *testing.T) {
	e := newTestEmitter()
	e.op3(IRAdd, I64, VReg(1), VReg(2), VReg(3))
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRAdd || ins.T != I64 || ins.Dst != 1 || ins.A != 2 || ins.B != 3 {
		t.Errorf("got %+v", ins)
	}
}

func TestOp3_VRegZero(t *testing.T) {
	e := newTestEmitter()
	e.op3(IRAdd, I64, VRegZero, VReg(1), VReg(2))
	if len(e.Block.Instrs) != 0 {
		t.Errorf("VRegZero dst should produce 0 instrs, got %d", len(e.Block.Instrs))
	}
}

func TestOp3_MarksDirty(t *testing.T) {
	e := newTestEmitter()
	e.op3(IRAdd, I64, VReg(5), VReg(1), VReg(2))
	if !e.dirty[5] {
		t.Error("expected dirty[5] = true after op3")
	}
	if e.dirty[1] || e.dirty[2] {
		t.Error("source regs should not be marked dirty")
	}
}

func TestOp2i_Basic(t *testing.T) {
	e := newTestEmitter()
	e.op2i(IRAddImm, I64, VReg(1), VReg(2), 42)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRAddImm || ins.Dst != 1 || ins.A != 2 || ins.Imm != 42 {
		t.Errorf("got %+v", ins)
	}
}

func TestOp2i_VRegZero(t *testing.T) {
	e := newTestEmitter()
	e.op2i(IRAddImm, I64, VRegZero, VReg(1), 42)
	if len(e.Block.Instrs) != 0 {
		t.Errorf("VRegZero dst should produce 0 instrs, got %d", len(e.Block.Instrs))
	}
}

func TestOp2_Basic(t *testing.T) {
	e := newTestEmitter()
	e.op2(IRMov, I64, VReg(1), VReg(2))
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRMov || ins.Dst != 1 || ins.A != 2 {
		t.Errorf("got %+v", ins)
	}
}

func TestOp2_VRegZero(t *testing.T) {
	e := newTestEmitter()
	e.op2(IRNeg, I64, VRegZero, VReg(1))
	if len(e.Block.Instrs) != 0 {
		t.Errorf("VRegZero dst should produce 0 instrs, got %d", len(e.Block.Instrs))
	}
}

func TestOpConst_Basic(t *testing.T) {
	e := newTestEmitter()
	e.opConst(VReg(1), 0xFF)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRConst || ins.Dst != 1 || ins.Imm != 0xFF {
		t.Errorf("got %+v", ins)
	}
}

func TestOpConst_VRegZero(t *testing.T) {
	e := newTestEmitter()
	e.opConst(VRegZero, 42)
	if len(e.Block.Instrs) != 0 {
		t.Errorf("VRegZero dst should produce 0 instrs, got %d", len(e.Block.Instrs))
	}
}

func TestOpSet_Basic(t *testing.T) {
	e := newTestEmitter()
	e.opSet(IRSet, VReg(1), VReg(2), VReg(3), LT)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRSet || ins.Pred != LT || ins.Dst != 1 || ins.A != 2 || ins.B != 3 {
		t.Errorf("got %+v", ins)
	}
}

func TestOpSetImm_Basic(t *testing.T) {
	e := newTestEmitter()
	e.opSetImm(IRSetImm, VReg(1), VReg(2), 100, GE)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRSetImm || ins.Pred != GE || ins.Imm != 100 {
		t.Errorf("got %+v", ins)
	}
}

func TestOpExt_Basic(t *testing.T) {
	e := newTestEmitter()
	e.opExt(IRSext, VReg(1), VReg(2), I32)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs, want 1", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRSext || ins.T != I32 || ins.Dst != 1 || ins.A != 2 {
		t.Errorf("got %+v", ins)
	}
}

func TestOpExt_VRegZero(t *testing.T) {
	e := newTestEmitter()
	e.opExt(IRSext, VRegZero, VReg(1), I32)
	if len(e.Block.Instrs) != 0 {
		t.Errorf("VRegZero dst should produce 0 instrs, got %d", len(e.Block.Instrs))
	}
}

func TestEmit_RecordsLabelIndex(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRLabel, Imm: 5})
	idx, ok := e.Block.Labels[Label(5)]
	if !ok {
		t.Fatal("label 5 not recorded in Block.Labels")
	}
	if idx != 0 {
		t.Errorf("label 5 index = %d, want 0", idx)
	}
}

func TestOp2i_AddImm0_SameDst_NoDirty(t *testing.T) {
	e := newTestEmitter()
	e.op2i(IRAddImm, I64, VReg(5), VReg(5), 0)
	if e.dirty[5] {
		t.Error("AddImm(x5, x5, 0) should not mark x5 dirty (peephole deletes identity)")
	}
}

func TestOp2i_AddImm0_DiffDst_Dirty(t *testing.T) {
	e := newTestEmitter()
	e.op2i(IRAddImm, I64, VReg(5), VReg(6), 0)
	if !e.dirty[5] {
		t.Error("AddImm(x5, x6, 0) should mark x5 dirty (rewritten to Mov)")
	}
}

func TestOp2_MovSelf_NoDirty(t *testing.T) {
	e := newTestEmitter()
	e.op2(IRMov, I64, VReg(5), VReg(5))
	if e.dirty[5] {
		t.Error("Mov(x5, x5) should not mark x5 dirty (peephole deletes self-move)")
	}
}

func TestEmit_TriggersPeephole(t *testing.T) {
	e := newTestEmitter()
	// Emit a self-move which peephole should delete.
	e.emit(IRInstr{Op: IRMov, T: I64, Dst: VReg(5), A: VReg(5)})
	if len(e.Block.Instrs) != 0 {
		t.Errorf("self-move should be deleted by peephole, got %d instrs", len(e.Block.Instrs))
	}
}

func TestEmit_PreservesNonPeepholeTarget(t *testing.T) {
	e := newTestEmitter()
	e.emit(IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)})
	if len(e.Block.Instrs) != 1 {
		t.Errorf("non-peephole target should be preserved, got %d instrs", len(e.Block.Instrs))
	}
}
