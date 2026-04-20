package ir

import (
	"math/rand"
	"testing"

	"riscv/goasm"
)

// ── V2 test helpers ──

func lowerBlockV2(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	pool := AMD64Pool_V2(b)
	pinned := AMD64Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64_V2(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64_V2: %v", err)
	}
	ctx.Append(ctx.NewRET())

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble V2: %v", err)
	}
	return bytes, alloc
}

func lowerBlockV2WithRet(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	pool := AMD64Pool_V2(b)
	pinned := AMD64Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64_V2(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64_V2: %v", err)
	}

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble V2: %v", err)
	}
	return bytes, alloc
}

// ── Pool tests ──

func TestAMD64Pool_V2_NoDiv(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	MaxVReg(b)
	pool := AMD64Pool_V2(b)
	if len(pool.IntRegs) != 7 {
		t.Errorf("want 7 int regs, got %d", len(pool.IntRegs))
	}
	if len(pool.FPRegs) != 14 {
		t.Errorf("want 14 FP regs (XMM14/15 reserved), got %d", len(pool.FPRegs))
	}
}

func TestAMD64Pool_V2_WithDiv(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{{Op: IRDivS, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}}
	MaxVReg(b)
	pool := AMD64Pool_V2(b)
	if len(pool.IntRegs) != 5 {
		t.Errorf("want 5 int regs, got %d", len(pool.IntRegs))
	}
}

// ── Data movement ──

func TestV2_Const(t *testing.T) {
	e := NewEmitter()
	tmp := e.Tmp()
	e.Const(tmp, 42)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Mov(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 99)
	e.Mov(t2, t1)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Sext_I32(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, -1)
	e.Sext(t2, t1, I32)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Zext_I32(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 0xDEADBEEF)
	e.Zext(t2, t1, I32)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Integer ALU ──

func TestV2_Add(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 10)
	e.Const(t2, 20)
	e.Add(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Sub(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 30)
	e.Const(t2, 10)
	e.Sub(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Mul(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 6)
	e.Const(t2, 7)
	e.Mul(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_AddImm(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 10)
	e.AddImm(t2, t1, 5)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Neg(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 42)
	e.Neg(t2, t1)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Shifts ──

func TestV2_Shl(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 1)
	e.Const(t2, 4)
	e.Shl(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Shr(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 256)
	e.Const(t2, 4)
	e.Shr(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Sar(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, -16)
	e.Const(t2, 2)
	e.Sar(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_ShlImm(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 1)
	e.ShlImm(t2, t1, 8)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Bitwise ──

func TestV2_And(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 0xFF)
	e.Const(t2, 0x0F)
	e.And(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Or(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 0xF0)
	e.Const(t2, 0x0F)
	e.Or(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Xor(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 0xFF)
	e.Const(t2, 0xF0)
	e.Xor(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Not(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 0xFF)
	e.Not(t2, t1)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Division ──

func TestV2_DivS(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 42)
	e.Const(t2, 7)
	e.DivS(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_Rem(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 17)
	e.Const(t2, 5)
	e.Rem(t3, t1, t2)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Comparison ──

func TestV2_Set(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	t3 := e.Tmp()
	e.Const(t1, 10)
	e.Const(t2, 20)
	e.Set(t3, t1, t2, LT)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestV2_SetImm(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 10)
	e.SetImm(t2, t1, 10, EQ)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Control flow ──

func TestV2_BranchJump(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	t2 := e.Tmp()
	e.Const(t1, 1)
	e.Const(t2, 1)
	lbl := e.NewLabel()
	e.Branch(t1, t2, EQ, lbl)
	e.Ret(0x2000, 0, VRegZero)
	e.PlaceLabel(lbl)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── VRegZero handling ──

func TestV2_VRegZero_Source(t *testing.T) {
	e := NewEmitter()
	t1 := e.Tmp()
	e.Add(t1, VRegZero, VRegZero)
	e.Ret(0x1000, 0, VRegZero)
	bytes, _ := lowerBlockV2WithRet(t, e.Block)
	if len(bytes) == 0 {
		t.Fatal("expected non-empty output")
	}
}

// ── Assembly-level lockstep: both lowerers produce assembleable code ──

func TestLockstep_V1V2_Assembly(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const numBlocks = 500
	const maxInsns = 20

	for i := 0; i < numBlocks; i++ {
		blk := genRandomBlock(rng, rng.Intn(maxInsns)+1, 6)
		if blk == nil {
			continue
		}

		// V1
		pool1 := AMD64Pool(blk)
		alloc1 := helperTestAllocate(blk, pool1, AMD64Pinned(), nil)
		ctx1 := goasm.New(goasm.AMD64)
		ctx1.Append(ctx1.NewATEXT())
		_, err1 := LowerAMD64(ctx1, blk, alloc1)
		if err1 != nil {
			continue // V1 can't handle → skip
		}
		_, asmErr1 := ctx1.Assemble()

		// V2
		pool2 := AMD64Pool_V2(blk)
		alloc2 := helperTestAllocate(blk, pool2, AMD64Pinned(), nil)
		ctx2 := goasm.New(goasm.AMD64)
		ctx2.Append(ctx2.NewATEXT())
		_, err2 := LowerAMD64_V2(ctx2, blk, alloc2)
		if err2 != nil {
			t.Fatalf("block %d: V1 succeeded but V2 failed: %v", i, err2)
		}
		_, asmErr2 := ctx2.Assemble()

		// Both should assemble successfully (or both fail).
		if asmErr1 == nil && asmErr2 != nil {
			t.Fatalf("block %d: V1 assembled but V2 failed: %v", i, asmErr2)
		}
	}
}

// ── Random IR block generator ──

// genRandomBlock generates a valid IR block with n instructions ending with Ret.
func genRandomBlock(rng *rand.Rand, n int, maxVR int) *Block {
	e := NewEmitter()
	if n < 1 {
		n = 1
	}

	// Pre-define some temps with constants so we have values to operate on.
	numInit := maxVR
	if numInit > n {
		numInit = n
	}
	temps := make([]VReg, 0, numInit+n)
	for i := 0; i < numInit && i < n-1; i++ {
		tmp := e.Tmp()
		e.Const(tmp, rng.Int63n(1000)-500)
		temps = append(temps, tmp)
	}

	// Generate random ops.
	remaining := n - len(temps) - 1 // save 1 for Ret
	for i := 0; i < remaining; i++ {
		if len(temps) < 2 {
			// Need more temps.
			tmp := e.Tmp()
			e.Const(tmp, rng.Int63n(1000))
			temps = append(temps, tmp)
			continue
		}
		pickA := temps[rng.Intn(len(temps))]
		pickB := temps[rng.Intn(len(temps))]
		dst := e.Tmp()

		// 10% chance: use VRegZero as a source
		if rng.Intn(10) == 0 {
			pickA = VRegZero
		}
		if rng.Intn(10) == 0 {
			pickB = VRegZero
		}

		op := rng.Intn(20)
		switch op {
		case 0:
			e.Add(dst, pickA, pickB)
		case 1:
			e.Sub(dst, pickA, pickB)
		case 2:
			e.And(dst, pickA, pickB)
		case 3:
			e.Or(dst, pickA, pickB)
		case 4:
			e.Xor(dst, pickA, pickB)
		case 5:
			e.Mul(dst, pickA, pickB)
		case 6:
			e.AddImm(dst, pickA, rng.Int63n(100))
		case 7:
			e.SubImm(dst, pickA, rng.Int63n(100))
		case 8:
			e.Shl(dst, pickA, pickB)
		case 9:
			e.Shr(dst, pickA, pickB)
		case 10:
			e.Sar(dst, pickA, pickB)
		case 11:
			e.ShlImm(dst, pickA, rng.Int63n(63))
		case 12:
			e.ShrImm(dst, pickA, rng.Int63n(63))
		case 13:
			e.SarImm(dst, pickA, rng.Int63n(63))
		case 14:
			e.Neg(dst, pickA)
		case 15:
			e.Not(dst, pickA)
		case 16:
			e.Mov(dst, pickA)
		case 17:
			e.Const(dst, rng.Int63())
		case 18:
			e.Sext(dst, pickA, I32)
		case 19:
			e.Zext(dst, pickA, I32)
		}
		temps = append(temps, dst)
	}

	e.Ret(0x1000, 0, VRegZero)
	return e.Block
}
