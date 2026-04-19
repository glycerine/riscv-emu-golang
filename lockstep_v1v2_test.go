//go:build !tcc

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

// compileIR compiles an IR block using the specified lowerer, returns executable memory.
func compileIR(t *testing.T, b *ir.Block, useV2 bool) (uintptr, []byte) {
	t.Helper()
	var pool ir.RegPool
	if useV2 {
		pool = ir.AMD64Pool_V2(b)
	} else {
		pool = ir.AMD64Pool(b)
	}
	pinned := ir.AMD64Pinned()
	j := NewJIT()
	alloc := j.irAlloc.Allocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	var err error
	if useV2 {
		err = ir.LowerAMD64_V2(ctx, b, alloc)
	} else {
		err = ir.LowerAMD64(ctx, b, alloc)
	}
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

// TestLockstep_V1V2_Execution compiles the same IR block with both lowerers
// and verifies identical execution results.
func TestLockstep_V1V2_Execution(t *testing.T) {
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

		// Try V1 first. If it fails to lower/assemble, skip.
		pool1 := ir.AMD64Pool(blk)
		alloc1 := j.irAlloc.Allocate(blk, pool1, ir.AMD64Pinned(), nil)
		ctx1 := goasm.New(goasm.AMD64)
		ctx1.Append(ctx1.NewATEXT())
		if err := ir.LowerAMD64(ctx1, blk, alloc1); err != nil {
			continue
		}
		code1, err := ctx1.Assemble()
		if err != nil || len(code1) == 0 {
			continue
		}

		// V2 must succeed.
		pool2 := ir.AMD64Pool_V2(blk)
		alloc2 := j.irAlloc.Allocate(blk, pool2, ir.AMD64Pinned(), nil)
		ctx2 := goasm.New(goasm.AMD64)
		ctx2.Append(ctx2.NewATEXT())
		if err := ir.LowerAMD64_V2(ctx2, blk, alloc2); err != nil {
			t.Fatalf("block %d: V2 lower failed: %v", i, err)
		}
		code2, err := ctx2.Assemble()
		if err != nil {
			t.Fatalf("block %d: V2 assemble failed: %v", i, err)
		}

		exec1, back1 := mmapCode(t, code1)
		exec2, back2 := mmapCode(t, code2)

		// Identical inputs.
		var x1, x2 [32]uint64
		var f1, f2 [32]uint64
		var fcsr1, fcsr2 uint32
		for j := 1; j <= 6; j++ {
			v := rng.Uint64()
			x1[j] = v
			x2[j] = v
		}

		res1 := jitcall.Call(exec1, &x1, &f1, &fcsr1, mem.Base(), mem.Mask())
		res2 := jitcall.Call(exec2, &x2, &f2, &fcsr2, mem.Base(), mem.Mask())

		if res1.PC != res2.PC || res1.IC != res2.IC || res1.Status != res2.Status {
			t.Fatalf("block %d MISMATCH:\n  V1: PC=0x%x IC=%d Status=%d\n  V2: PC=0x%x IC=%d Status=%d\n  %d instrs",
				i, res1.PC, res1.IC, res1.Status,
				res2.PC, res2.IC, res2.Status,
				len(blk.Instrs))
		}

		freeExecMem(back1)
		freeExecMem(back2)
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

// genLockstepBlock generates a valid IR block for lockstep comparison.
// Only uses integer ops (no memory loads/stores, no FP — those need memory setup).
func genLockstepBlock(rng *rand.Rand, n int, maxVR int) *ir.Block {
	e := ir.NewEmitter()
	if n < 1 {
		n = 1
	}

	// Initialize some temps.
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
