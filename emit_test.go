package riscv

import "testing"

func TestEmitter_NewEmitter(t *testing.T) {
	e := NewEmitter(nil)
	if e.Block == nil {
		t.Fatal("Block is nil")
	}
	if len(e.Block.Instrs) != 0 {
		t.Errorf("Block.Instrs has %d entries, want 0", len(e.Block.Instrs))
	}
	// 5 param VRegs should have been allocated (4 params + VRRegFile).
	if e.nextTmp != VRegTempStart+5 {
		t.Errorf("nextTmp = %d, want %d", e.nextTmp, VRegTempStart+5)
	}
	if e.xBase != VReg(64) || e.fBase != VReg(65) ||
		e.memBase != VReg(66) || e.memMask != VReg(67) {
		t.Errorf("param VRegs: xBase=%d fBase=%d memBase=%d memMask=%d",
			e.xBase, e.fBase, e.memBase, e.memMask)
	}
}

func TestEmitter_Tmp(t *testing.T) {
	e := NewEmitter(nil)
	first := e.Tmp()
	second := e.Tmp()
	if first >= second {
		t.Errorf("Tmp not monotonic: first=%d second=%d", first, second)
	}
	// 5 param VRegs should have been allocated (4 params + VRRegFile).
	if first != VRegTempStart+5 {
		t.Errorf("first Tmp = %d, want %d", first, VRegTempStart+6)
	}
}

func TestEmitter_XReg(t *testing.T) {
	e := NewEmitter(nil)
	if e.XReg(0) != VRegZero {
		t.Errorf("XReg(0) = %d, want %d", e.XReg(0), VRegZero)
	}
	if e.XReg(31) != VReg(31) {
		t.Errorf("XReg(31) = %d, want 31", e.XReg(31))
	}
}

func TestEmitter_XReg_Panics(t *testing.T) {
	e := NewEmitter(nil)
	defer func() {
		if r := recover(); r == nil {
			t.Error("XReg(32) should panic")
		}
	}()
	e.XReg(32)
}

func TestEmitter_FRegV(t *testing.T) {
	e := NewEmitter(nil)
	if e.FRegV(0) != VReg(32) {
		t.Errorf("FRegV(0) = %d, want 32", e.FRegV(0))
	}
	if e.FRegV(31) != VReg(63) {
		t.Errorf("FRegV(31) = %d, want 63", e.FRegV(31))
	}
}

func TestEmitter_FRegV_Panics(t *testing.T) {
	e := NewEmitter(nil)
	defer func() {
		if r := recover(); r == nil {
			t.Error("FRegV(32) should panic")
		}
	}()
	e.FRegV(32)
}

func TestEmitter_ParamAccessors(t *testing.T) {
	e := NewEmitter(nil)
	if e.XBase() != VReg(64) {
		t.Errorf("XBase = %d", e.XBase())
	}
	if e.FBase() != VReg(65) {
		t.Errorf("FBase = %d", e.FBase())
	}
	if e.MemBase() != VReg(66) {
		t.Errorf("MemBase = %d", e.MemBase())
	}
	if e.MemMask() != VReg(67) {
		t.Errorf("MemMask = %d", e.MemMask())
	}
}

// ── Integer ALU ──

func TestEmitter_Add(t *testing.T) {
	e := NewEmitter(nil)
	e.Add(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRAdd, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_AddT(t *testing.T) {
	e := NewEmitter(nil)
	e.AddT(VReg(1), VReg(2), VReg(3), I32)
	if e.Block.Instrs[0].T != I32 {
		t.Errorf("T = %v, want I32", e.Block.Instrs[0].T)
	}
}

func TestEmitter_AddImm(t *testing.T) {
	e := NewEmitter(nil)
	e.AddImm(VReg(1), VReg(2), 42)
	assertInstrImm(t, e, 0, IRAddImm, VReg(1), VReg(2), 42)
}

func TestEmitter_Sub(t *testing.T) {
	e := NewEmitter(nil)
	e.Sub(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRSub, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_SubImm(t *testing.T) {
	e := NewEmitter(nil)
	e.SubImm(VReg(1), VReg(2), 10)
	assertInstrImm(t, e, 0, IRSubImm, VReg(1), VReg(2), 10)
}

func TestEmitter_Mul(t *testing.T) {
	e := NewEmitter(nil)
	e.Mul(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRMul, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_DivS(t *testing.T) {
	e := NewEmitter(nil)
	e.DivS(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRDivS, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_DivU(t *testing.T) {
	e := NewEmitter(nil)
	e.DivU(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRDivU, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_Rem(t *testing.T) {
	e := NewEmitter(nil)
	e.Rem(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRRem, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_MulHS(t *testing.T) {
	e := NewEmitter(nil)
	e.MulHS(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRMulHS, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_MulHU(t *testing.T) {
	e := NewEmitter(nil)
	e.MulHU(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRMulHU, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_MulHSU(t *testing.T) {
	e := NewEmitter(nil)
	e.MulHSU(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRMulHSU, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_Neg(t *testing.T) {
	e := NewEmitter(nil)
	e.Neg(VReg(1), VReg(2))
	assertInstr2(t, e, 0, IRNeg, VReg(1), VReg(2))
}

// ── Shifts ──

func TestEmitter_Shl(t *testing.T) {
	e := NewEmitter(nil)
	e.Shl(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRShl, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_ShlImm(t *testing.T) {
	e := NewEmitter(nil)
	e.ShlImm(VReg(1), VReg(2), 5)
	assertInstrImm(t, e, 0, IRShlImm, VReg(1), VReg(2), 5)
}

func TestEmitter_Shr(t *testing.T) {
	e := NewEmitter(nil)
	e.Shr(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRShr, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_ShrImm(t *testing.T) {
	e := NewEmitter(nil)
	e.ShrImm(VReg(1), VReg(2), 3)
	assertInstrImm(t, e, 0, IRShrImm, VReg(1), VReg(2), 3)
}

func TestEmitter_Sar(t *testing.T) {
	e := NewEmitter(nil)
	e.Sar(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRSar, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_SarImm(t *testing.T) {
	e := NewEmitter(nil)
	e.SarImm(VReg(1), VReg(2), 7)
	assertInstrImm(t, e, 0, IRSarImm, VReg(1), VReg(2), 7)
}

// ── Bitwise ──

func TestEmitter_And(t *testing.T) {
	e := NewEmitter(nil)
	e.And(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRAnd, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_AndImm(t *testing.T) {
	e := NewEmitter(nil)
	e.AndImm(VReg(1), VReg(2), 0xFF)
	assertInstrImm(t, e, 0, IRAndImm, VReg(1), VReg(2), 0xFF)
}

func TestEmitter_Or(t *testing.T) {
	e := NewEmitter(nil)
	e.Or(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IROr, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_OrImm(t *testing.T) {
	e := NewEmitter(nil)
	e.OrImm(VReg(1), VReg(2), 0x80)
	assertInstrImm(t, e, 0, IROrImm, VReg(1), VReg(2), 0x80)
}

func TestEmitter_Xor(t *testing.T) {
	e := NewEmitter(nil)
	e.Xor(VReg(1), VReg(2), VReg(3))
	assertInstr(t, e, 0, IRXor, I64, VReg(1), VReg(2), VReg(3))
}

func TestEmitter_XorImm(t *testing.T) {
	e := NewEmitter(nil)
	e.XorImm(VReg(1), VReg(2), 0x55)
	assertInstrImm(t, e, 0, IRXorImm, VReg(1), VReg(2), 0x55)
}

func TestEmitter_Not(t *testing.T) {
	e := NewEmitter(nil)
	e.Not(VReg(1), VReg(2))
	assertInstr2(t, e, 0, IRNot, VReg(1), VReg(2))
}

// ── Comparison ──

func TestEmitter_Set(t *testing.T) {
	e := NewEmitter(nil)
	e.Set(VReg(1), VReg(2), VReg(3), LT)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRSet || ins.Pred != LT || ins.Dst != 1 || ins.A != 2 || ins.B != 3 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_SetImm(t *testing.T) {
	e := NewEmitter(nil)
	e.SetImm(VReg(1), VReg(2), 100, GEU)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRSetImm || ins.Pred != GEU || ins.Imm != 100 {
		t.Errorf("got %+v", ins)
	}
}

// ── Data movement ──

func TestEmitter_Mov(t *testing.T) {
	e := NewEmitter(nil)
	e.Mov(VReg(1), VReg(2))
	assertInstr2(t, e, 0, IRMov, VReg(1), VReg(2))
}

func TestEmitter_Const(t *testing.T) {
	e := NewEmitter(nil)
	e.Const(VReg(1), 0xDEADBEEF)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRConst || ins.Dst != 1 || ins.Imm != 0xDEADBEEF {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_Sext(t *testing.T) {
	e := NewEmitter(nil)
	e.Sext(VReg(1), VReg(2), I32)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRSext || ins.T != I32 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_Zext(t *testing.T) {
	e := NewEmitter(nil)
	e.Zext(VReg(1), VReg(2), I16)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRZext || ins.T != I16 {
		t.Errorf("got %+v", ins)
	}
}

// ── Memory ──

func TestEmitter_Load_I64(t *testing.T) {
	e := NewEmitter(nil)
	e.Load(VReg(1), VReg(2), 8, I64, false)
	// I64 load: just one IRLoad, no extension.
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("I64 load should produce 1 instr, got %d", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRLoad || ins.T != I64 || ins.Dst != 1 || ins.A != 2 || ins.Imm != 8 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_Load_Signed_I32(t *testing.T) {
	e := NewEmitter(nil)
	e.Load(VReg(1), VReg(2), 0, I32, true)
	// Signed I32: IRLoad + IRSext.
	if len(e.Block.Instrs) != 2 {
		t.Fatalf("signed I32 load should produce 2 instrs, got %d", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRLoad {
		t.Errorf("first instr should be IRLoad, got %v", e.Block.Instrs[0].Op)
	}
	if e.Block.Instrs[1].Op != IRSext || e.Block.Instrs[1].T != I32 {
		t.Errorf("second instr should be IRSext I32, got %+v", e.Block.Instrs[1])
	}
}

func TestEmitter_Load_Unsigned_I32(t *testing.T) {
	e := NewEmitter(nil)
	e.Load(VReg(1), VReg(2), 0, I32, false)
	if len(e.Block.Instrs) != 2 {
		t.Fatalf("unsigned I32 load should produce 2 instrs, got %d", len(e.Block.Instrs))
	}
	if e.Block.Instrs[1].Op != IRZext {
		t.Errorf("second instr should be IRZext, got %v", e.Block.Instrs[1].Op)
	}
}

func TestEmitter_Load_Signed_I8(t *testing.T) {
	e := NewEmitter(nil)
	e.Load(VReg(1), VReg(2), 0, I8, true)
	if len(e.Block.Instrs) != 2 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	if e.Block.Instrs[1].Op != IRSext || e.Block.Instrs[1].T != I8 {
		t.Errorf("got %+v", e.Block.Instrs[1])
	}
}

func TestEmitter_Load_F64(t *testing.T) {
	e := NewEmitter(nil)
	e.Load(VReg(32), VReg(2), 0, F64, false)
	// F64: just one IRLoad, no extension.
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("F64 load should produce 1 instr, got %d", len(e.Block.Instrs))
	}
}

func TestEmitter_Load_VRegZero(t *testing.T) {
	e := NewEmitter(nil)
	e.Load(VRegZero, VReg(2), 0, I64, false)
	if len(e.Block.Instrs) != 0 {
		t.Errorf("load to VRegZero should produce 0 instrs, got %d", len(e.Block.Instrs))
	}
}

func TestEmitter_Store(t *testing.T) {
	e := NewEmitter(nil)
	e.Store(VReg(1), 16, VReg(2), I32)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRStore || ins.T != I32 || ins.A != 1 || ins.B != 2 || ins.Imm != 16 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_LoadX(t *testing.T) {
	e := NewEmitter(nil)
	e.LoadX(VReg(1), VReg(2), VReg(3), 4, I32, false)
	// Unsigned I32: IRLoadX + IRZext.
	if len(e.Block.Instrs) != 2 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRLoadX || ins.Scale != 4 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_StoreX(t *testing.T) {
	e := NewEmitter(nil)
	e.StoreX(VReg(1), VReg(2), 8, VReg(3), I64)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRStoreX || ins.Scale != 8 {
		t.Errorf("got %+v", ins)
	}
}

// ── Control flow ──

func TestEmitter_NewLabel_PlaceLabel(t *testing.T) {
	e := NewEmitter(nil)
	l := e.NewLabel()
	if l != 1 {
		t.Errorf("first label = %d, want 0", l)
	}
	e.PlaceLabel(l)
	idx, ok := e.Block.Labels[l]
	if !ok {
		t.Fatal("label not in Block.Labels after PlaceLabel")
	}
	if idx != 0 {
		t.Errorf("label index = %d, want 0", idx)
	}
	if *e.lastLabelSerial != 1 {
		t.Errorf("*lastLabelSerial = %d, want 1", *e.lastLabelSerial)
	}
}

func TestEmitter_Branch(t *testing.T) {
	e := NewEmitter(nil)
	l := e.NewLabel()
	e.Branch(VReg(1), VReg(2), NE, l)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRBranch || ins.Pred != NE || ins.A != 1 || ins.B != 2 || ins.Imm != int64(l) {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_BranchImm(t *testing.T) {
	e := NewEmitter(nil)
	l := e.NewLabel()
	e.BranchImm(VReg(1), 100, GE, l)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRBranchImm || ins.Pred != GE || ins.A != 1 || ins.Imm != int64(l) || ins.Imm2 != 100 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_Jump(t *testing.T) {
	e := NewEmitter(nil)
	l := e.NewLabel()
	e.Jump(l)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	if e.Block.Instrs[0].Op != IRJump || e.Block.Instrs[0].Imm != int64(l) {
		t.Errorf("got %+v", e.Block.Instrs[0])
	}
}

func TestEmitter_Call(t *testing.T) {
	e := NewEmitter(nil)
	idx := e.Call("jit_sqrt", 0x12345678)
	if idx != 0 {
		t.Errorf("first call index = %d, want 0", idx)
	}
	if len(e.Block.CTab) != 1 {
		t.Fatalf("CTab has %d entries", len(e.Block.CTab))
	}
	if e.Block.CTab[0].Name != "jit_sqrt" {
		t.Errorf("CTab[0].Name = %q", e.Block.CTab[0].Name)
	}
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRCall || ins.Imm != 0 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_Call_Dedup(t *testing.T) {
	e := NewEmitter(nil)
	idx1 := e.Call("jit_sqrt", 0x1)
	idx2 := e.Call("jit_sqrt", 0x1)
	if idx1 != idx2 {
		t.Errorf("duplicate call should return same index: %d vs %d", idx1, idx2)
	}
	if len(e.Block.CTab) != 1 {
		t.Errorf("CTab should have 1 entry after dedup, got %d", len(e.Block.CTab))
	}
}

func TestEmitter_Call_InconsistentAddr_Panics(t *testing.T) {
	e := NewEmitter(nil)
	e.Call("jit_sqrt", 0x1)
	defer func() {
		if r := recover(); r == nil {
			t.Error("Call with different addr should panic")
		}
	}()
	e.Call("jit_sqrt", 0x2)
}

func TestEmitter_Ret(t *testing.T) {
	e := NewEmitter(nil)
	e.Ret(0x80001000, 3, VReg(64))
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRRet || ins.Imm != 0x80001000 || ins.Imm2 != 3 || ins.A != VReg(64) {
		t.Errorf("got %+v", ins)
	}
}

// ── FP ──

func TestEmitter_FAdd_F32(t *testing.T) {
	e := NewEmitter(nil)
	e.FAdd(VReg(32), VReg(33), VReg(34), F32)
	assertInstr(t, e, 0, IRFAdd, F32, VReg(32), VReg(33), VReg(34))
}

func TestEmitter_FAdd_F64(t *testing.T) {
	e := NewEmitter(nil)
	e.FAdd(VReg(32), VReg(33), VReg(34), F64)
	assertInstr(t, e, 0, IRFAdd, F64, VReg(32), VReg(33), VReg(34))
}

func TestEmitter_FSub(t *testing.T) {
	e := NewEmitter(nil)
	e.FSub(VReg(32), VReg(33), VReg(34), F64)
	assertInstr(t, e, 0, IRFSub, F64, VReg(32), VReg(33), VReg(34))
}

func TestEmitter_FMul(t *testing.T) {
	e := NewEmitter(nil)
	e.FMul(VReg(32), VReg(33), VReg(34), F64)
	assertInstr(t, e, 0, IRFMul, F64, VReg(32), VReg(33), VReg(34))
}

func TestEmitter_FDiv(t *testing.T) {
	e := NewEmitter(nil)
	e.FDiv(VReg(32), VReg(33), VReg(34), F64)
	assertInstr(t, e, 0, IRFDiv, F64, VReg(32), VReg(33), VReg(34))
}

func TestEmitter_FSqrt(t *testing.T) {
	e := NewEmitter(nil)
	e.FSqrt(VReg(32), VReg(33), F64)
	assertInstr2(t, e, 0, IRFSqrt, VReg(32), VReg(33))
}

func TestEmitter_FNeg(t *testing.T) {
	e := NewEmitter(nil)
	e.FNeg(VReg(32), VReg(33), F32)
	assertInstr2(t, e, 0, IRFNeg, VReg(32), VReg(33))
}

func TestEmitter_FAbs(t *testing.T) {
	e := NewEmitter(nil)
	e.FAbs(VReg(32), VReg(33), F64)
	assertInstr2(t, e, 0, IRFAbs, VReg(32), VReg(33))
}

func TestEmitter_FCmp(t *testing.T) {
	e := NewEmitter(nil)
	e.FCmp(VReg(1), VReg(32), VReg(33), EQ, F64)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRFCmp || ins.T != F64 || ins.Pred != EQ {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_FCvtToI(t *testing.T) {
	e := NewEmitter(nil)
	e.FCvtToI(VReg(1), VReg(32), F64, I32)
	if len(e.Block.Instrs) != 1 {
		t.Fatalf("got %d instrs", len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[0]
	if ins.Op != IRFCvtToI || ins.T != I32 || ins.U != F64 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_FCvtToU(t *testing.T) {
	e := NewEmitter(nil)
	e.FCvtToU(VReg(1), VReg(32), F32, I64)
	ins := e.Block.Instrs[0]
	if ins.Op != IRFCvtToU || ins.T != I64 || ins.U != F32 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_FCvtFromI(t *testing.T) {
	e := NewEmitter(nil)
	e.FCvtFromI(VReg(32), VReg(1), I32, F64)
	ins := e.Block.Instrs[0]
	if ins.Op != IRFCvtFromI || ins.T != F64 || ins.U != I32 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_FCvtFromU(t *testing.T) {
	e := NewEmitter(nil)
	e.FCvtFromU(VReg(32), VReg(1), I64, F32)
	ins := e.Block.Instrs[0]
	if ins.Op != IRFCvtFromU || ins.T != F32 || ins.U != I64 {
		t.Errorf("got %+v", ins)
	}
}

func TestEmitter_FCvtFF(t *testing.T) {
	e := NewEmitter(nil)
	e.FCvtFF(VReg(32), VReg(33), F32, F64)
	ins := e.Block.Instrs[0]
	if ins.Op != IRFCvtFF || ins.T != F64 || ins.U != F32 {
		t.Errorf("got %+v", ins)
	}
}

// ── VRegZero discard ──

func TestEmitter_VRegZero_Discard_AllOps(t *testing.T) {
	ops := []struct {
		name string
		fn   func(e *Emitter)
	}{
		{"Add", func(e *Emitter) { e.Add(VRegZero, VReg(1), VReg(2)) }},
		{"AddImm", func(e *Emitter) { e.AddImm(VRegZero, VReg(1), 42) }},
		{"Sub", func(e *Emitter) { e.Sub(VRegZero, VReg(1), VReg(2)) }},
		{"Mul", func(e *Emitter) { e.Mul(VRegZero, VReg(1), VReg(2)) }},
		{"Neg", func(e *Emitter) { e.Neg(VRegZero, VReg(1)) }},
		{"Shl", func(e *Emitter) { e.Shl(VRegZero, VReg(1), VReg(2)) }},
		{"And", func(e *Emitter) { e.And(VRegZero, VReg(1), VReg(2)) }},
		{"Or", func(e *Emitter) { e.Or(VRegZero, VReg(1), VReg(2)) }},
		{"Xor", func(e *Emitter) { e.Xor(VRegZero, VReg(1), VReg(2)) }},
		{"Not", func(e *Emitter) { e.Not(VRegZero, VReg(1)) }},
		{"Set", func(e *Emitter) { e.Set(VRegZero, VReg(1), VReg(2), EQ) }},
		{"Mov", func(e *Emitter) { e.Mov(VRegZero, VReg(1)) }},
		{"Const", func(e *Emitter) { e.Const(VRegZero, 42) }},
		{"Sext", func(e *Emitter) { e.Sext(VRegZero, VReg(1), I32) }},
		{"Zext", func(e *Emitter) { e.Zext(VRegZero, VReg(1), I32) }},
		{"Load", func(e *Emitter) { e.Load(VRegZero, VReg(1), 0, I64, false) }},
		{"FAdd", func(e *Emitter) { e.FAdd(VRegZero, VReg(32), VReg(33), F64) }},
		{"FSqrt", func(e *Emitter) { e.FSqrt(VRegZero, VReg(32), F64) }},
		{"FCmp", func(e *Emitter) { e.FCmp(VRegZero, VReg(32), VReg(33), EQ, F64) }},
		{"FCvtToI", func(e *Emitter) { e.FCvtToI(VRegZero, VReg(32), F64, I32) }},
	}
	for _, tt := range ops {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmitter(nil)
			tt.fn(e)
			if len(e.Block.Instrs) != 0 {
				t.Errorf("%s to VRegZero produced %d instrs, want 0", tt.name, len(e.Block.Instrs))
			}
		})
	}
}

// ── Dirty tracking ──

func TestEmitter_DirtyTracking(t *testing.T) {
	e := NewEmitter(nil)
	e.Add(VReg(5), VReg(1), VReg(2))
	if !e.IsDirty(VReg(5)) {
		t.Error("VReg(5) should be dirty after Add")
	}
	if e.IsDirty(VReg(1)) || e.IsDirty(VReg(2)) {
		t.Error("source regs should not be dirty")
	}
}

func TestEmitter_DirtyTracking_VRegZero(t *testing.T) {
	e := NewEmitter(nil)
	e.Add(VRegZero, VReg(1), VReg(2))
	if e.IsDirty(VRegZero) {
		t.Error("VRegZero should never be dirty")
	}
}

// ── helpers ──

func assertInstr(t *testing.T, e *Emitter, idx int, op IROp, ty Type, dst, a, b VReg) {
	t.Helper()
	if idx >= len(e.Block.Instrs) {
		t.Fatalf("no instr at index %d (have %d)", idx, len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[idx]
	if ins.Op != op {
		t.Errorf("[%d] Op = %v, want %v", idx, ins.Op, op)
	}
	if ins.T != ty {
		t.Errorf("[%d] T = %v, want %v", idx, ins.T, ty)
	}
	if ins.Dst != dst {
		t.Errorf("[%d] Dst = %v, want %v", idx, ins.Dst, dst)
	}
	if ins.A != a {
		t.Errorf("[%d] A = %v, want %v", idx, ins.A, a)
	}
	if ins.B != b {
		t.Errorf("[%d] B = %v, want %v", idx, ins.B, b)
	}
}

func assertInstr2(t *testing.T, e *Emitter, idx int, op IROp, dst, a VReg) {
	t.Helper()
	if idx >= len(e.Block.Instrs) {
		t.Fatalf("no instr at index %d (have %d)", idx, len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[idx]
	if ins.Op != op {
		t.Errorf("[%d] Op = %v, want %v", idx, ins.Op, op)
	}
	if ins.Dst != dst {
		t.Errorf("[%d] Dst = %v, want %v", idx, ins.Dst, dst)
	}
	if ins.A != a {
		t.Errorf("[%d] A = %v, want %v", idx, ins.A, a)
	}
}

func assertInstrImm(t *testing.T, e *Emitter, idx int, op IROp, dst, a VReg, imm int64) {
	t.Helper()
	if idx >= len(e.Block.Instrs) {
		t.Fatalf("no instr at index %d (have %d)", idx, len(e.Block.Instrs))
	}
	ins := e.Block.Instrs[idx]
	if ins.Op != op {
		t.Errorf("[%d] Op = %v, want %v", idx, ins.Op, op)
	}
	if ins.Dst != dst {
		t.Errorf("[%d] Dst = %v, want %v", idx, ins.Dst, dst)
	}
	if ins.A != a {
		t.Errorf("[%d] A = %v, want %v", idx, ins.A, a)
	}
	if ins.Imm != imm {
		t.Errorf("[%d] Imm = %d, want %d", idx, ins.Imm, imm)
	}
}
