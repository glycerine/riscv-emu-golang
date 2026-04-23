package riscv

import (
	"math/rand"
	"riscv/goasm"
	"riscv/internal/jitcall"
	"riscv/ir"
	"syscall"
	"testing"
	"unsafe"
)

// compileIR compiles an IR block using the rv8 lowerer, returns executable memory.
func compileIR(t *testing.T, b *ir.Block) (uintptr, []byte) {
	t.Helper()
	pool := ir.RV8Pool(b)
	pinned := ir.RV8Pinned()
	j := NewJIT()
	alloc := j.irAlloc.Allocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := ir.LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("empty code")
	}

	execMem, err := allocExec(len(code))
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	copy(execMem, code)
	return uintptr(unsafe.Pointer(&execMem[0])), execMem
}

func freeExecMem(mem []byte) {
	if len(mem) > 0 {
		syscall.Munmap(mem)
	}
}

// TestRV8_RandomBlocks compiles random IR blocks with the rv8 lowerer
// and verifies they execute without crashing and return expected PC.
func TestRV8_RandomBlocks(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))
	const numBlocks = 2000
	const maxInsns = 25

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	j := NewJIT()
	for i := 0; i < numBlocks; i++ {
		n := rng.Intn(maxInsns) + 1
		blk := genLockstepBlock(rng, n, 6)
		if blk == nil {
			continue
		}

		pool := ir.RV8Pool(blk)
		alloc := j.irAlloc.Allocate(blk, pool, ir.RV8Pinned(), nil)
		ctx := goasm.New(goasm.AMD64)
		ctx.Append(ctx.NewATEXT())
		if _, err := ir.LowerAMD64_RV8(ctx, blk, alloc); err != nil {
			continue
		}
		code, err := ctx.Assemble()
		if err != nil || len(code) == 0 {
			continue
		}

		exec, back := mmapCode(t, code)

		var x [32]uint64
		var f [32]uint64
		var fcsr uint32
		for j := 1; j <= 6; j++ {
			x[j] = rng.Uint64()
		}

		res := jitcall.Call(exec, &x, &f, &fcsr, mem.Base(), mem.Mask())
		if res.PC != 0x1000 {
			t.Fatalf("block %d: PC=0x%x want 0x1000 (%d instrs)", i, res.PC, len(blk.Instrs))
		}

		freeExecMem(back)
	}
}

func mmapCode(t *testing.T, code []byte) (uintptr, []byte) {
	t.Helper()
	execMem, err := allocExec(len(code))
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	copy(execMem, code)
	return uintptr(unsafe.Pointer(&execMem[0])), execMem
}

// genLockstepBlock generates a valid IR block for testing.
func genLockstepBlock(rng *rand.Rand, n int, maxVR int) *ir.Block {
	e := ir.NewEmitter()
	if n < 1 {
		n = 1
	}

	numInit := maxVR
	if numInit > n {
		numInit = n
	}
	temps := make([]ir.VReg, 0, numInit+n)
	for i := 0; i < numInit && i < n-1; i++ {
		tmp := e.Tmp()
		e.Const(tmp, rng.Int63n(1000)-500)
		temps = append(temps, tmp)
	}

	remaining := n - len(temps) - 1
	for i := 0; i < remaining; i++ {
		if len(temps) < 2 {
			tmp := e.Tmp()
			e.Const(tmp, rng.Int63n(1000))
			temps = append(temps, tmp)
			continue
		}
		pickA := temps[rng.Intn(len(temps))]
		pickB := temps[rng.Intn(len(temps))]
		dst := e.Tmp()

		if rng.Intn(10) == 0 {
			pickA = ir.VRegZero
		}
		if rng.Intn(10) == 0 {
			pickB = ir.VRegZero
		}

		op := rng.Intn(22)
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
			e.Sext(dst, pickA, ir.I32)
		case 19:
			e.Zext(dst, pickA, ir.I32)
		case 20:
			e.Set(dst, pickA, pickB, ir.Pred(rng.Intn(10)))
		case 21:
			e.SetImm(dst, pickA, rng.Int63n(200)-100, ir.Pred(rng.Intn(10)))
		}
		temps = append(temps, dst)
	}

	e.Ret(0x1000, 0, ir.VRegZero)
	return e.Block
}
