package ir

import (
	"testing"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// ── Test helpers ──

// lowerBlock runs the full pipeline: allocate + lower + assemble.
func lowerBlock(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	pool := AMD64Pool(b)
	pinned := AMD64Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64: %v", err)
	}
	ctx.Append(ctx.NewRET()) // safety net RET

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return bytes, alloc
}

// a helper to fix up all the places where the tests did not create an Allocator
// to invoke Allocate() on.
func helperTestAllocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
	a := NewAllocator()
	return a.Allocate(b, pool, pinned, freq)
}

// lowerBlockWithRet runs pipeline for blocks that already contain IRRet.
func lowerBlockWithRet(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	pool := AMD64Pool(b)
	pinned := AMD64Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64: %v", err)
	}

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return bytes, alloc
}

// ── Phase A: Types, Stubs, Pool Definitions ──

func TestAMD64Pool_NoDiv(t *testing.T) {
	b := NewBlock()
	// No DIV/MUL ops → full pool.
	b.Instrs = []IRInstr{
		{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	}
	pool := AMD64Pool(b)
	if len(pool.IntRegs) != 7 {
		t.Errorf("want 7 int regs, got %d", len(pool.IntRegs))
	}
	if len(pool.FPRegs) != 16 {
		t.Errorf("want 16 FP regs, got %d", len(pool.FPRegs))
	}
	// Verify RAX and RDX are present.
	hasAX, hasDX := false, false
	for _, r := range pool.IntRegs {
		if r == goasm.REG_AMD64_AX {
			hasAX = true
		}
		if r == goasm.REG_AMD64_DX {
			hasDX = true
		}
	}
	if !hasAX || !hasDX {
		t.Errorf("no-div pool should contain RAX and RDX")
	}
}

func TestAMD64Pool_WithDiv(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRDivS, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	}
	pool := AMD64Pool(b)
	if len(pool.IntRegs) != 5 {
		t.Errorf("want 5 int regs (no RAX/RDX/RBP), got %d", len(pool.IntRegs))
	}
	// Verify RAX and RDX are absent.
	for _, r := range pool.IntRegs {
		if r == goasm.REG_AMD64_AX {
			t.Error("div pool should not contain RAX")
		}
		if r == goasm.REG_AMD64_DX {
			t.Error("div pool should not contain RDX")
		}
	}
}

func TestAMD64Pinned(t *testing.T) {
	pinned := AMD64Pinned()
	if len(pinned) != 5 {
		t.Errorf("want 5 pinned VRegs, got %d", len(pinned))
	}
	checks := map[VReg]int16{
		VRXBase:   goasm.REG_AMD64_R12,
		VRFBase:   goasm.REG_AMD64_R13,
		VRIC:      goasm.REG_AMD64_BP,
		VRMemBase: goasm.REG_AMD64_R14,
		VRMemMask: goasm.REG_AMD64_R15,
	}
	for vr, wantReg := range checks {
		got, ok := pinned[vr]
		if !ok {
			t.Errorf("pinned[%v] missing", vr)
			continue
		}
		if got != wantReg {
			t.Errorf("pinned[%v] = %d, want %d", vr, got, wantReg)
		}
	}
}

func TestLowerAMD64_EmptyBlock(t *testing.T) {
	b := NewBlock()
	pool := AMD64Pool(b)
	alloc := helperTestAllocate(b, pool, AMD64Pinned(), nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64(ctx, b, alloc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx.Append(ctx.NewRET())
	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(bytes) == 0 {
		t.Error("expected non-empty output for empty block")
	}
}

func TestLowerAMD64_NilAlloc(t *testing.T) {
	b := NewBlock()
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64(ctx, b, nil)
	if err == nil {
		t.Error("expected error for nil allocation")
	}
}

// ── Phase B: Data Movement ──

func TestLower_IRConst(t *testing.T) {
	e := NewEmitter()
	tmp := e.Tmp() // t69
	e.Const(tmp, 42)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRMov(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 99)
	e.Mov(t2, t1)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRSext_I32(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, -1)
	e.Sext(t2, t1, I32)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRZext_I32(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 0xDEADBEEF)
	e.Zext(t2, t1, I32)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase C: Integer ALU ──

func TestLower_IRAdd(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 10)
	e.Const(t2, 20)
	e.Add(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRAddImm(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 10)
	e.AddImm(t2, t1, 5)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRSub(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 30)
	e.Const(t2, 10)
	e.Sub(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRMul(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 6)
	e.Const(t2, 7)
	e.Mul(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRNeg(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 42)
	e.Neg(t2, t1)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase C: Bitwise ──

func TestLower_IRAnd(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 0xFF)
	e.Const(t2, 0x0F)
	e.And(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IROr(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 0xF0)
	e.Const(t2, 0x0F)
	e.Or(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRXor(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 0xFF)
	e.Const(t2, 0x0F)
	e.Xor(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRNot(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 0xFF)
	e.Not(t2, t1)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase D: Shifts ──

func TestLower_IRShlImm(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 1)
	e.ShlImm(t2, t1, 4)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRShrImm(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 256)
	e.ShrImm(t2, t1, 4)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase E: Comparison ──

func TestLower_IRSet_EQ(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 42)
	e.Const(t2, 42)
	e.Set(t3, t1, t2, EQ)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase E: Memory ──

func TestLower_IRLoad_I64(t *testing.T) {
	e := NewEmitter()
	dst := e.Tmp()
	e.Load(dst, e.MemBase(), 0, I64, false)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRStore_I64(t *testing.T) {
	e := NewEmitter()
	src := e.Tmp()
	e.Const(src, 99)
	e.Store(e.MemBase(), 0, src, I64)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase F: Control Flow ──

func TestLower_IRBranch_Forward(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	e.Const(t1, 42)
	target := e.NewLabel()
	e.Branch(t1, VRegZero, NE, target)
	e.Ret(0x2000, 1, VRegZero) // taken if t1 == 0
	e.PlaceLabel(target)
	e.Ret(0x1000, 0, VRegZero) // taken if t1 != 0

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRJump(t *testing.T) {
	e := NewEmitter()
	target := e.NewLabel()
	e.Jump(target)
	e.Ret(0x2000, 1, VRegZero) // unreachable
	e.PlaceLabel(target)
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestLower_IRRet(t *testing.T) {
	e := NewEmitter()
	e.Ret(0x1234, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Phase I: Pseudo-ops ──

func TestLower_IRMarkLive_NoOp(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	e.Const(t1, 1)
	e.Block.Instrs = append(e.Block.Instrs, IRInstr{Op: IRMarkLive, A: t1})
	e.Ret(0x1000, 0, VRegZero)

	bytes, _ := lowerBlockWithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Verify all ops have a handler ──

func TestLower_AllOpsHandled(t *testing.T) {
	// Build a block with one of each non-pseudo op and verify LowerAMD64
	// doesn't return "unhandled op".
	allOps := []IROp{
		IRLoad, IRStore, IRLoadX, IRStoreX,
		IRAdd, IRAddImm, IRSub, IRSubImm, IRMul,
		IRDivS, IRDivU, IRRem, IRMulHS, IRMulHU, IRMulHSU,
		IRNeg,
		IRShl, IRShlImm, IRShr, IRShrImm, IRSar, IRSarImm,
		IRAnd, IRAndImm, IROr, IROrImm, IRXor, IRXorImm, IRNot,
		IRSet, IRSetImm,
		IRMov, IRConst, IRSext, IRZext,
		IRLabel, IRBranch, IRBranchImm, IRJump, IRCall, IRRet,
		IRFAdd, IRFSub, IRFMul, IRFDiv, IRFSqrt, IRFCmp,
		IRFNeg, IRFAbs,
		IRFCvtToI, IRFCvtToU, IRFCvtFromI, IRFCvtFromU, IRFCvtFF,
		IRMarkLive, IRMarkDead, IRWriteback,
	}

	lc := &lowerCtx{
		blk:       &Block{CTab: []CSym{{Name: "test", Addr: 0x12345}}},
		alloc:     &Allocation{Kind: make([]AllocKind, 256), SpillSlot: make([]int16, 256)},
		c:         goasm.New(goasm.AMD64),
		labelProg: make(map[Label]*obj.Prog),
		pending:   make(map[Label][]*obj.Prog),
	}
	lc.c.Append(lc.c.NewATEXT())

	// Place a label so branch targets resolve.
	lc.placeLabel(Label(0))

	for _, op := range allOps {
		ins := IRInstr{
			Op:    op,
			T:     I64,
			U:     F64,
			Dst:   VRegZero,
			A:     VRegZero,
			B:     VRegZero,
			Imm:   0,
			Imm2:  0,
			Pred:  EQ,
			Scale: 1,
		}
		if err := lc.lowerInstr(&ins); err != nil {
			t.Errorf("op %v: %v", op, err)
		}
	}
}

// ── Verify parameter VReg constants match Emitter ──

func TestParamVRegs_MatchEmitter(t *testing.T) {
	e := NewEmitter()
	if e.XBase() != VRXBase {
		t.Errorf("XBase = %v, want %v", e.XBase(), VRXBase)
	}
	if e.FBase() != VRFBase {
		t.Errorf("FBase = %v, want %v", e.FBase(), VRFBase)
	}
	if e.IC() != VRIC {
		t.Errorf("IC = %v, want %v", e.IC(), VRIC)
	}
	if e.MemBase() != VRMemBase {
		t.Errorf("MemBase = %v, want %v", e.MemBase(), VRMemBase)
	}
	if e.MemMask() != VRMemMask {
		t.Errorf("MemMask = %v, want %v", e.MemMask(), VRMemMask)
	}
}

// suppress unused import
var _ = x86.AMOVQ
