package ir

import (
	"testing"

	"riscv/goasm"
)

func lowerBlockABJIT(t *testing.T, b *Block) ([]byte, *Allocation) {
	t.Helper()
	b.maxVreg = MaxVReg(b)
	pool := ABJITPool(b)
	alloc := helperTestAllocate(b, pool, ABJITPinned(), nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64_ABJIT(ctx, b, alloc)
	if err != nil {
		t.Fatal(err)
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	return code, alloc
}

func TestLowerABJIT_EmptyBlock(t *testing.T) {
	b := NewBlock()
	pool := ABJITPool(b)
	alloc := helperTestAllocate(b, pool, ABJITPinned(), nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64_ABJIT(ctx, b, alloc)
	if err != nil {
		t.Fatal(err)
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) == 0 {
		t.Fatal("empty code output")
	}
	t.Logf("empty block: %d bytes", len(code))
}

func TestLowerABJIT_RetBlock(t *testing.T) {
	e := NewEmitter()
	e.Ret(0x1000, 0, VRegZero)

	code, _ := lowerBlockABJIT(t, e.Block)
	t.Logf("ret block: %d bytes", len(code))
}

func TestLowerABJIT_AddAndRet(t *testing.T) {
	e := NewEmitter()
	x1, x2 := e.XReg(1), e.XReg(2)
	tmp := e.Tmp()
	e.Add(tmp, x1, x2)
	e.WriteBackReg(VReg(0))
	e.Ret(0x1000, 0, VRegZero)

	code, _ := lowerBlockABJIT(t, e.Block)
	t.Logf("add+ret block: %d bytes", len(code))
}

func TestLowerABJIT_ChainExit(t *testing.T) {
	e := NewEmitter()
	e.ChainExit(0x2000, 0)

	code, _ := lowerBlockABJIT(t, e.Block)
	t.Logf("chain exit block: %d bytes", len(code))
}

func TestLowerABJIT_ChainEntryExists(t *testing.T) {
	b := NewBlock()
	b.maxVreg = MaxVReg(b)
	pool := ABJITPool(b)
	alloc := helperTestAllocate(b, pool, ABJITPinned(), nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	result, err := LowerAMD64_ABJIT(ctx, b, alloc)
	if err != nil {
		t.Fatal(err)
	}
	if result.ChainEntryProg == nil {
		t.Error("chain entry prog is nil")
	}
}

func TestLowerABJIT_Syscall(t *testing.T) {
	e := NewEmitter()
	e.Syscall(0x1004, 0)

	code, _ := lowerBlockABJIT(t, e.Block)
	t.Logf("syscall block: %d bytes", len(code))
}
