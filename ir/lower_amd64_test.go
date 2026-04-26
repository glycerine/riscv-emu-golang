package ir

import (
	"syscall"
	"testing"
	"unsafe"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
	"riscv/internal/jitcall"
)

// ── Test helpers ──

// lowerBlock runs the full pipeline: allocate + lower + assemble.
func lowerBlock(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	return lowerBlockRV8(t, b)
}

// helperTestAllocate runs the fixed static register allocator on b.
func helperTestAllocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
	a := NewFixedStaticAllocator()
	return a.Allocate(b, pool, pinned, freq)
}

// lowerBlockWithRet runs pipeline for blocks that already contain IRRet.
func lowerBlockWithRet(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	return lowerBlockRV8(t, b)
}

// ── Phase A: Types, Stubs, Pool Definitions ──

func TestLowerRV8_EmptyBlock(t *testing.T) {
	b := NewBlock()
	pool := RV8Pool(b)
	alloc := helperTestAllocate(b, pool, RV8Pinned(), nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(bytes) == 0 {
		t.Error("expected non-empty output for empty block")
	}
}

func TestLowerRV8_NilAlloc(t *testing.T) {
	b := NewBlock()
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64_RV8(ctx, b, nil)
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

	lc := &lowerCtxRV8{
		lowerOps: lowerOps{
			blk:       &Block{CTab: []CSym{{Name: "test", Addr: 0x12345}}},
			alloc:     &Allocation{Kind: make([]AllocKind, 256), SpillSlot: make([]int16, 256)},
			c:         goasm.New(goasm.AMD64),
			labelProg: make(map[Label]*obj.Prog),
			pending:   make(map[Label][]*obj.Prog),
		},
	}
	lc.c.Append(lc.c.NewATEXT())

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

// ── Phase: rv8 register layout (Stage 1) ──

// ── Stage 3: rv8 prologue/epilogue ──

func lowerBlockRV8(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64_RV8(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return bytes, alloc
}

func TestRV8Prologue_SretOnStack(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRRet, T: I64, Imm: 0x1000, Imm2: 0},
	}
	b.maxVreg = MaxVReg(b)

	code, _ := lowerBlockRV8(t, b)
	if len(code) == 0 {
		t.Fatal("rv8 lowerer produced empty code")
	}
	t.Logf("rv8 trivial block: %d bytes", len(code))
}

func TestRV8Priority_Top12(t *testing.T) {
	// With 12-reg pool, the first 12 in intPriority should be
	// ra(1), sp(2), t0(5), t1(6), a0-a7(10-17) — matching rv8.
	want := map[VReg]bool{
		1: true, 2: true, 5: true, 6: true,
		10: true, 11: true, 12: true, 13: true,
		14: true, 15: true, 16: true, 17: true,
	}
	top12 := intPriority[:12]
	for _, vr := range top12 {
		if !want[vr] {
			t.Errorf("unexpected VReg %d in top 12", vr)
		}
	}
	got := make(map[VReg]bool)
	for _, vr := range top12 {
		got[vr] = true
	}
	for vr := range want {
		if !got[vr] {
			t.Errorf("missing VReg %d in top 12", vr)
		}
	}
}

func TestRV8Pool(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	}
	b.maxVreg = MaxVReg(b)
	pool := RV8Pool(b)

	if len(pool.IntRegs) != 12 {
		t.Fatalf("want 12 int regs, got %d", len(pool.IntRegs))
	}
	if len(pool.FPRegs) != 14 {
		t.Errorf("want 14 FP regs (X14/X15 reserved for FP staging), got %d", len(pool.FPRegs))
	}

	excluded := map[int16]string{
		goasm.REG_AMD64_AX: "RAX",
		goasm.REG_AMD64_CX: "RCX",
		goasm.REG_AMD64_BP: "RBP",
		goasm.REG_AMD64_SP: "RSP",
	}
	for _, r := range pool.IntRegs {
		if name, bad := excluded[r]; bad {
			t.Errorf("pool must not contain %s", name)
		}
	}

	want := map[int16]bool{
		goasm.REG_AMD64_DX:  true,
		goasm.REG_AMD64_BX:  true,
		goasm.REG_AMD64_SI:  true,
		goasm.REG_AMD64_DI:  true,
		goasm.REG_AMD64_R8:  true,
		goasm.REG_AMD64_R9:  true,
		goasm.REG_AMD64_R10: true,
		goasm.REG_AMD64_R11: true,
		goasm.REG_AMD64_R12: true,
		goasm.REG_AMD64_R13: true,
		goasm.REG_AMD64_R14: true,
		goasm.REG_AMD64_R15: true,
	}
	for _, r := range pool.IntRegs {
		if !want[r] {
			t.Errorf("unexpected register %d in pool", r)
		}
	}
}

func TestRV8Pool_DivMulSameSize(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRDivS, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	}
	b.maxVreg = MaxVReg(b)
	pool := RV8Pool(b)
	if len(pool.IntRegs) != 12 {
		t.Fatalf("rv8 pool must stay 12 even with DIV/MUL (local save/restore), got %d", len(pool.IntRegs))
	}
}

func TestRV8Pinned(t *testing.T) {
	pinned := RV8Pinned()
	if len(pinned) != 1 {
		t.Fatalf("want 1 pinned VReg (VRRegFile→RBP), got %d", len(pinned))
	}
	got, ok := pinned[VRRegFile]
	if !ok {
		t.Fatal("VRRegFile not in pinned map")
	}
	if got != goasm.REG_AMD64_BP {
		t.Errorf("VRRegFile pinned to %d, want RBP (%d)", got, goasm.REG_AMD64_BP)
	}
}

func TestVRRegFile_Distinct(t *testing.T) {
	seen := map[VReg]string{
		VRXBase:   "VRXBase",
		VRFBase:   "VRFBase",
		VRIC:      "VRIC",
		VRMemBase: "VRMemBase",
		VRMemMask: "VRMemMask",
	}
	if name, dup := seen[VRRegFile]; dup {
		t.Fatalf("VRRegFile (%d) collides with %s", VRRegFile, name)
	}
}

// ── ABJIT policy tests ──

func TestABJITPool(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)},
	}
	b.maxVreg = MaxVReg(b)
	pool := ABJITPool(b)

	if len(pool.IntRegs) != 11 {
		t.Fatalf("want 11 int regs, got %d", len(pool.IntRegs))
	}
	if len(pool.FPRegs) != 14 {
		t.Errorf("want 14 FP regs, got %d", len(pool.FPRegs))
	}

	for _, r := range pool.IntRegs {
		if r == goasm.REG_AMD64_R14 {
			t.Error("pool must not contain R14 (Go goroutine pointer)")
		}
	}

	excluded := map[int16]string{
		goasm.REG_AMD64_AX: "RAX",
		goasm.REG_AMD64_CX: "RCX",
		goasm.REG_AMD64_BP: "RBP",
		goasm.REG_AMD64_SP: "RSP",
	}
	for _, r := range pool.IntRegs {
		if name, bad := excluded[r]; bad {
			t.Errorf("pool must not contain %s", name)
		}
	}

	want := map[int16]bool{
		goasm.REG_AMD64_DX:  true,
		goasm.REG_AMD64_BX:  true,
		goasm.REG_AMD64_SI:  true,
		goasm.REG_AMD64_DI:  true,
		goasm.REG_AMD64_R8:  true,
		goasm.REG_AMD64_R9:  true,
		goasm.REG_AMD64_R10: true,
		goasm.REG_AMD64_R11: true,
		goasm.REG_AMD64_R12: true,
		goasm.REG_AMD64_R13: true,
		goasm.REG_AMD64_R15: true,
	}
	for _, r := range pool.IntRegs {
		if !want[r] {
			t.Errorf("unexpected register %d in pool", r)
		}
	}
}

func TestABJITPinned(t *testing.T) {
	pinned := ABJITPinned()
	if len(pinned) != 1 {
		t.Fatalf("want 1 pinned VReg, got %d", len(pinned))
	}
	got, ok := pinned[VRRegFile]
	if !ok {
		t.Fatal("VRRegFile not in pinned map")
	}
	if got != goasm.REG_AMD64_BP {
		t.Errorf("VRRegFile pinned to %d, want RBP (%d)", got, goasm.REG_AMD64_BP)
	}
}

func TestPolicyRV8(t *testing.T) {
	p := PolicyRV8
	if p.Name != "rv8" {
		t.Errorf("name = %q, want %q", p.Name, "rv8")
	}
	if p.Pool == nil {
		t.Fatal("Pool is nil")
	}
	if p.Pinned == nil {
		t.Fatal("Pinned is nil")
	}
	if p.Lower == nil {
		t.Fatal("Lower is nil")
	}

	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	b.maxVreg = MaxVReg(b)
	pool := p.Pool(b)
	if len(pool.IntRegs) != 12 {
		t.Errorf("Pool().IntRegs = %d, want 12", len(pool.IntRegs))
	}

	pinned := p.Pinned()
	if len(pinned) != 1 {
		t.Errorf("Pinned() = %d entries, want 1", len(pinned))
	}
}

func TestPolicyABJIT(t *testing.T) {
	p := PolicyABJIT
	if p.Name != "abjit" {
		t.Errorf("name = %q, want %q", p.Name, "abjit")
	}
	if p.Pool == nil {
		t.Fatal("Pool is nil")
	}
	if p.Pinned == nil {
		t.Fatal("Pinned is nil")
	}
	if p.Lower == nil {
		t.Fatal("Lower is nil")
	}

	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	b.maxVreg = MaxVReg(b)
	pool := p.Pool(b)
	if len(pool.IntRegs) != 11 {
		t.Errorf("Pool().IntRegs = %d, want 11", len(pool.IntRegs))
	}
}

func TestABJITPool_R14Excluded(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	b.maxVreg = MaxVReg(b)

	rv8Pool := RV8Pool(b)
	abjitPool := ABJITPool(b)

	hasR14 := func(pool RegPool) bool {
		for _, r := range pool.IntRegs {
			if r == goasm.REG_AMD64_R14 {
				return true
			}
		}
		return false
	}
	if !hasR14(rv8Pool) {
		t.Error("RV8Pool should include R14")
	}
	if hasR14(abjitPool) {
		t.Error("ABJITPool must not include R14")
	}

	if len(abjitPool.IntRegs) != len(rv8Pool.IntRegs)-1 {
		t.Errorf("abjit=%d, rv8=%d, want abjit=rv8-1",
			len(abjitPool.IntRegs), len(rv8Pool.IntRegs))
	}
}

// ── Stage 4: rv8 ALU ops ──

func TestRV8Lower_Add(t *testing.T) {
	e := NewEmitter()
	e.Add(e.XReg(10), e.XReg(11), e.XReg(12))
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Sub(t *testing.T) {
	e := NewEmitter()
	e.Sub(e.XReg(10), e.XReg(11), e.XReg(12))
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Const(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 42)
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Mov(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(11), 99)
	e.Mov(e.XReg(10), e.XReg(11))
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Sext(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(11), -1)
	e.Sext(e.XReg(10), e.XReg(11), I32)
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Zext(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(11), 0xFFFF)
	e.Zext(e.XReg(10), e.XReg(11), I16)
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

// ── Stage 5: rv8 memory, shifts, div ──

func TestRV8Lower_Shl(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 1)
	e.Const(e.XReg(11), 5)
	e.Shl(e.XReg(12), e.XReg(10), e.XReg(11))
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Div(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 100)
	e.Const(e.XReg(11), 7)
	e.DivS(e.XReg(12), e.XReg(10), e.XReg(11))
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Rem(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 100)
	e.Const(e.XReg(11), 7)
	e.Rem(e.XReg(12), e.XReg(10), e.XReg(11))
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

// ── Stage 6: rv8 FP, branch, set, ret ──

func TestRV8Lower_FAdd(t *testing.T) {
	e := NewEmitter()
	fa0 := e.FRegV(10)
	fa1 := e.FRegV(11)
	fa2 := e.FRegV(12)
	e.FAdd(fa2, fa0, fa1, F64)
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Branch(t *testing.T) {
	e := NewEmitter()
	l := e.NewLabel()
	e.Const(e.XReg(10), 1)
	e.Const(e.XReg(11), 2)
	e.Branch(e.XReg(10), e.XReg(11), EQ, l)
	e.PlaceLabel(l)
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_Set(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 1)
	e.Const(e.XReg(11), 2)
	e.Set(e.XReg(12), e.XReg(10), e.XReg(11), LT)
	e.Ret(0x1000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestEmitMI_RSP_Encoding(t *testing.T) {
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	// Emit: ADDQ $1, [RSP+8]
	p := ctx.NewProg()
	p.As = x86.AADDQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = 1
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = goasm.REG_AMD64_SP
	p.To.Offset = 8
	ctx.Append(p)

	// Emit: ADDQ $1, [RBP+8] (known working, for comparison)
	p2 := ctx.NewProg()
	p2.As = x86.AADDQ
	p2.From.Type = obj.TYPE_CONST
	p2.From.Offset = 1
	p2.To.Type = obj.TYPE_MEM
	p2.To.Reg = goasm.REG_AMD64_BP
	p2.To.Offset = 8
	ctx.Append(p2)

	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// Expected: ADDQ $1, [RSP+8] = 48 83 44 24 08 01 (6 bytes)
	//           ADDQ $1, [RBP+8] = 48 83 45 08 01    (5 bytes)
	t.Logf("assembled %d bytes:", len(code))
	for i, b := range code {
		t.Logf("  [%d] %02x", i, b)
	}

	// RSP version must have SIB byte (0x24 after ModRM)
	// Find the ADDQ $1,[RSP+8] instruction
	// REX.W=48, opcode=83, ModRM=44, SIB=24, disp8=08, imm8=01
	want := []byte{0x48, 0x83, 0x44, 0x24, 0x08, 0x01}
	if len(code) < 6 {
		t.Fatalf("code too short: %d bytes", len(code))
	}
	for i, w := range want {
		if code[i] != w {
			t.Errorf("byte[%d] = %02x, want %02x", i, code[i], w)
		}
	}
}

func TestEmitMI_RSP_Functional(t *testing.T) {
	// Test: ADDQ $5, [RSP+off] actually adds 5 to the value at that location.
	// Build a block where a temp gets spilled and AddImm operates on it.
	e := NewEmitter()
	// Use 20 regs to force temps to spill.
	for i := 1; i <= 20; i++ {
		e.Const(e.XReg(uint32(i)), int64(i*100))
	}
	// x21 = x10 (1000). x21 will be spilled (reg pressure).
	e.Mov(e.XReg(21), e.XReg(10))
	// x21 = x21 + 7 → should be 1007
	e.AddImm(e.XReg(21), e.XReg(21), 7)
	// Keep x1-x20 live.
	e.Mov(e.XReg(25), e.XReg(1))
	for i := 2; i <= 20; i++ {
		e.Add(e.XReg(25), e.XReg(25), e.XReg(uint32(i)))
	}
	e.Ret(0x1000, 0, VRegZero)
	var x [32]uint64
	execBlockRV8(t, e.Block, &x)
	if x[21] != 1007 {
		t.Errorf("x21 = %d, want 1007", x[21])
	}
}

func TestEmitMI_RSP_TempVReg(t *testing.T) {
	// Same as above but using a TEMP VReg (≥70) that spills to [RSP+slot*8].
	e := NewEmitter()
	// Use 14 temps to exhaust pool (12 GPRs - parameter overhead).
	temps := make([]VReg, 14)
	for i := range temps {
		temps[i] = e.Tmp()
		e.Const(temps[i], int64((i+1)*100))
	}
	// t0 = t0 + 7: fires emitMI if t0 is spilled.
	e.AddImm(temps[0], temps[0], 7)
	// Write all temps back to RISC-V regs for checking.
	for i := 0; i < 14 && i < 31; i++ {
		e.Mov(e.XReg(uint32(i+1)), temps[i])
	}
	e.Ret(0x1000, 0, VRegZero)
	var x [32]uint64
	execBlockRV8(t, e.Block, &x)
	// t0 was 100, +7 = 107
	if x[1] != 107 {
		t.Errorf("x1 (t0+7) = %d, want 107", x[1])
	}
	// t1 was 200, unchanged
	if x[2] != 200 {
		t.Errorf("x2 (t1) = %d, want 200", x[2])
	}
}

func TestRV8Set_CmpDirection_RegReg(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 3)
	e.Const(e.XReg(11), 7)
	e.Set(e.XReg(12), e.XReg(10), e.XReg(11), LT) // 3 < 7 → 1
	e.Set(e.XReg(13), e.XReg(11), e.XReg(10), LT) // 7 < 3 → 0
	e.Ret(0x1000, 0, VRegZero)
	var x [32]uint64
	execBlockRV8(t, e.Block, &x)
	if x[12] != 1 {
		t.Errorf("Set(3 LT 7) = %d, want 1", x[12])
	}
	if x[13] != 0 {
		t.Errorf("Set(7 LT 3) = %d, want 0", x[13])
	}
}

func TestRV8Set_CmpDirection_SpilledB(t *testing.T) {
	e := NewEmitter()
	for i := 1; i <= 20; i++ {
		e.Const(e.XReg(uint32(i)), int64(i*100))
	}
	// x10 is in register, x20 is spilled → triggers emitRM CMP path.
	e.Set(e.XReg(21), e.XReg(10), e.XReg(20), LT) // 1000 < 2000 → 1
	e.Set(e.XReg(22), e.XReg(20), e.XReg(10), LT) // 2000 < 1000 → 0
	e.Mov(e.XReg(25), e.XReg(1))
	for i := 2; i <= 20; i++ {
		e.Add(e.XReg(25), e.XReg(25), e.XReg(uint32(i)))
	}
	e.Ret(0x1000, 0, VRegZero)
	var x [32]uint64
	execBlockRV8(t, e.Block, &x)
	if x[21] != 1 {
		t.Errorf("Set(x10=1000 LT x20=2000) = %d, want 1", x[21])
	}
	if x[22] != 0 {
		t.Errorf("Set(x20=2000 LT x10=1000) = %d, want 0", x[22])
	}
	var expectedSum uint64
	for i := 1; i <= 20; i++ {
		expectedSum += uint64(i * 100)
	}
	if x[25] != expectedSum {
		t.Errorf("sum = %d, want %d", x[25], expectedSum)
	}
}

func TestRV8Branch_CmpDirection_SpilledB(t *testing.T) {
	e := NewEmitter()
	for i := 1; i <= 20; i++ {
		e.Const(e.XReg(uint32(i)), int64(i*100))
	}
	trueLabel := e.NewLabel()
	endLabel := e.NewLabel()
	// x10 is in register, x20 is spilled → triggers emitRM CMP path.
	e.Branch(e.XReg(10), e.XReg(20), LT, trueLabel)
	e.Const(e.XReg(21), 0)
	e.Jump(endLabel)
	e.PlaceLabel(trueLabel)
	e.Const(e.XReg(21), 1)
	e.PlaceLabel(endLabel)
	e.Mov(e.XReg(25), e.XReg(1))
	for i := 2; i <= 20; i++ {
		e.Add(e.XReg(25), e.XReg(25), e.XReg(uint32(i)))
	}
	e.Ret(0x1000, 0, VRegZero)
	var x [32]uint64
	execBlockRV8(t, e.Block, &x)
	if x[21] != 1 {
		t.Errorf("Branch(1000 LT 2000) took false path, x21=%d want 1", x[21])
	}
}

func TestRV8Lower_Ret(t *testing.T) {
	e := NewEmitter()
	e.Ret(0x2000, 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Lower_RetDyn(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(1), 0x3000)
	e.RetDyn(e.XReg(1), 0, VRegZero)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

// ── Stage 9: rv8 trampoline round-trip ──

func execBlockRV8(t *testing.T, b *Block, x *[32]uint64) jitcall.Result {
	t.Helper()
	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	ps := syscall.Getpagesize()
	sz := ((len(code) + ps - 1) / ps) * ps
	mem, err := syscall.Mmap(-1, 0, sz,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer syscall.Munmap(mem)
	copy(mem, code)

	var f [32]uint64
	var fcsr uint32
	return jitcall.Call(uintptr(unsafe.Pointer(&mem[0])), x, &f, &fcsr, 0, 0)
}

func TestRV8Trampoline_RoundTrip(t *testing.T) {
	e := NewEmitter()
	e.Ret(0x1000, 0, VRegZero)
	var x [32]uint64
	res := execBlockRV8(t, e.Block, &x)
	if res.PC != 0x1000 {
		t.Errorf("PC = 0x%x, want 0x1000", res.PC)
	}
	if res.Status != 0 {
		t.Errorf("Status = %d, want 0", res.Status)
	}
}

func TestRV8Trampoline_ConstAndRet(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 42)
	e.Ret(0x2000, 0, VRegZero)
	var x [32]uint64
	res := execBlockRV8(t, e.Block, &x)
	if res.PC != 0x2000 {
		t.Errorf("PC = 0x%x, want 0x2000", res.PC)
	}
	if x[10] != 42 {
		t.Errorf("x[10] = %d, want 42", x[10])
	}
}

func TestRV8Trampoline_AddRegs(t *testing.T) {
	e := NewEmitter()
	e.Add(e.XReg(12), e.XReg(10), e.XReg(11))
	e.Ret(0x3000, 0, VRegZero)
	var x [32]uint64
	x[10] = 100
	x[11] = 200
	res := execBlockRV8(t, e.Block, &x)
	if res.PC != 0x3000 {
		t.Errorf("PC = 0x%x, want 0x3000", res.PC)
	}
	if x[12] != 300 {
		t.Errorf("x[12] = %d, want 300", x[12])
	}
}

func TestRV8Trampoline_ShiftRegs(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 1)
	e.Const(e.XReg(11), 10)
	e.Shl(e.XReg(12), e.XReg(10), e.XReg(11))
	e.Ret(0x4000, 0, VRegZero)
	var x [32]uint64
	res := execBlockRV8(t, e.Block, &x)
	if res.PC != 0x4000 {
		t.Errorf("PC = 0x%x, want 0x4000", res.PC)
	}
	if x[12] != 1024 {
		t.Errorf("x[12] = %d, want 1024", x[12])
	}
}

func TestRV8Trampoline_DivRegs(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 100)
	e.Const(e.XReg(11), 7)
	e.DivS(e.XReg(12), e.XReg(10), e.XReg(11))
	e.Ret(0x5000, 0, VRegZero)
	var x [32]uint64
	res := execBlockRV8(t, e.Block, &x)
	if res.PC != 0x5000 {
		t.Errorf("PC = 0x%x, want 0x5000", res.PC)
	}
	if x[12] != 14 {
		t.Errorf("x[12] = %d, want 14 (100/7)", x[12])
	}
}

// ── Stage 8: rv8 chain exit/entry ──

func TestRV8Chain_ExitAssembles(t *testing.T) {
	e := NewEmitter()
	e.Const(e.XReg(10), 42)
	e.ChainExit(0x2000, 0)
	code, _ := lowerBlockRV8(t, e.Block)
	if len(code) == 0 {
		t.Fatal("empty")
	}
}

func TestRV8Chain_HasChainEntryProg(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRRet, T: I64, Imm: 0x1000, Imm2: 0},
	}
	b.maxVreg = MaxVReg(b)
	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	result, err := LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}
	if result.ChainEntryProg == nil {
		t.Fatal("chain entry prog is nil")
	}
}

func TestRV8Chain_ExitHasDesc(t *testing.T) {
	e := NewEmitter()
	e.ChainExit(0x3000, 0)
	b := e.Block
	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	result, err := LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}
	if len(result.ChainExits) != 1 {
		t.Fatalf("want 1 chain exit, got %d", len(result.ChainExits))
	}
	if result.ChainExits[0].TargetPC != 0x3000 {
		t.Errorf("target PC = 0x%x, want 0x3000", result.ChainExits[0].TargetPC)
	}
}

// ── Stage 7: rv8 exhaustive register-pair assembly tests ──

func buildRV8SingleBlock(rd, ra, rb int, emitOp func(*Emitter, VReg, VReg, VReg)) *Block {
	e := NewEmitter()
	emitOp(e, VReg(rd), VReg(ra), VReg(rb))
	e.Ret(0x1000, 0, VRegZero)
	return e.Block
}

func runRV8ExhaustAssemble(t *testing.T, name string, emitOp func(*Emitter, VReg, VReg, VReg)) {
	const N = 12
	for rd := 1; rd <= N; rd++ {
		for ra := 1; ra <= N; ra++ {
			for rb := 1; rb <= N; rb++ {
				blk := buildRV8SingleBlock(rd, ra, rb, emitOp)
				pool := RV8Pool(blk)
				pinned := RV8Pinned()
				alloc := helperTestAllocate(blk, pool, pinned, nil)
				ctx := goasm.New(goasm.AMD64)
				ctx.Append(ctx.NewATEXT())
				if _, err := LowerAMD64_RV8(ctx, blk, alloc); err != nil {
					t.Fatalf("%s d=x%d a=x%d b=x%d: lower: %v", name, rd, ra, rb, err)
				}
				if _, err := ctx.Assemble(); err != nil {
					t.Fatalf("%s d=x%d a=x%d b=x%d: assemble: %v", name, rd, ra, rb, err)
				}
			}
		}
	}
}

func runRV8ExhaustExec(t *testing.T, name string, emitOp func(*Emitter, VReg, VReg, VReg), ref func(uint64, uint64) uint64) {
	const N = 7
	valA := uint64(0xDEADBEEF12345678)
	valB := uint64(4)

	for rd := 1; rd <= N; rd++ {
		for ra := 1; ra <= N; ra++ {
			for rb := 1; rb <= N; rb++ {
				e := NewEmitter()
				emitOp(e, VReg(rd), VReg(ra), VReg(rb))
				e.Ret(0x1000, 0, VRegZero)
				blk := e.Block

				effA, effB := valA, valB
				if ra == rb {
					effA = valB
				}
				want := ref(effA, effB)

				var x [32]uint64
				for i := 1; i <= N; i++ {
					x[i] = uint64(i * 111)
				}
				x[ra] = valA
				x[rb] = valB

				res := execBlockRV8(t, blk, &x)
				if res.PC != 0x1000 {
					t.Fatalf("%s d=x%d a=x%d b=x%d: PC=0x%x want 0x1000", name, rd, ra, rb, res.PC)
				}
				if x[rd] != want {
					t.Fatalf("%s d=x%d a=x%d b=x%d: got 0x%x want 0x%x", name, rd, ra, rb, x[rd], want)
				}
			}
		}
	}
}

func TestRV8ExhaustExec_ADD(t *testing.T) {
	runRV8ExhaustExec(t, "ADD", (*Emitter).Add, func(a, b uint64) uint64 { return a + b })
}

func TestRV8ExhaustExec_SUB(t *testing.T) {
	runRV8ExhaustExec(t, "SUB", (*Emitter).Sub, func(a, b uint64) uint64 { return a - b })
}

func TestRV8ExhaustExec_SHL(t *testing.T) {
	runRV8ExhaustExec(t, "SHL", (*Emitter).Shl, func(a, b uint64) uint64 { return a << (b & 63) })
}

func TestRV8ExhaustExec_SHR(t *testing.T) {
	runRV8ExhaustExec(t, "SHR", (*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) })
}

func TestRV8ExhaustExec_XOR(t *testing.T) {
	runRV8ExhaustExec(t, "XOR", (*Emitter).Xor, func(a, b uint64) uint64 { return a ^ b })
}

func TestRV8Exhaust_ADD(t *testing.T) {
	runRV8ExhaustAssemble(t, "ADD", (*Emitter).Add)
}

func TestRV8Exhaust_SUB(t *testing.T) {
	runRV8ExhaustAssemble(t, "SUB", (*Emitter).Sub)
}

func TestRV8Exhaust_SHL(t *testing.T) {
	runRV8ExhaustAssemble(t, "SHL", (*Emitter).Shl)
}

func TestRV8Exhaust_SHR(t *testing.T) {
	runRV8ExhaustAssemble(t, "SHR", (*Emitter).Shr)
}

func TestRV8Exhaust_XOR(t *testing.T) {
	runRV8ExhaustAssemble(t, "XOR", (*Emitter).Xor)
}

// suppress unused import
var _ = x86.AMOVQ
