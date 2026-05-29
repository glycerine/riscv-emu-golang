package riscv

import (
	"testing"

	"github.com/glycerine/riscv-emu-golang/goasm"
	"github.com/glycerine/riscv-emu-golang/goasm/obj"
	_ "github.com/glycerine/riscv-emu-golang/goasm/obj/x86"
)

func lowerABJITBlock(t *testing.T, b *Block) (*goasm.Ctx, *LowerResult) {
	t.Helper()
	pool := ABJITPool(b)
	pinned := ABJITPinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	res, err := LowerAMD64_ABJIT(ctx, b, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64_ABJIT: %v", err)
	}
	return ctx, res
}

// TestABJIT_NoJITtoJIT_CALL verifies that the ABJIT lowerer never
// emits x86 CALL/RET for JIT-to-JIT transitions. CALL/RET must only
// appear at Go-boundary crossings (syscall dispatch, Go callbacks,
// exit-thunk RET). A stray CALL would push a return address pointing
// into mmap'd JIT memory onto the Go stack, which panics the GC.
func TestABJIT_NoJITtoJIT_CALL(t *testing.T) {
	// Block with ALU + chain exit + JALR IC — no syscalls or Go
	// callbacks. The only permitted CALL/RET is the exit-thunk RET.
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 42, T: I64},
		{Op: IRAdd, Dst: VReg(11), A: VReg(10), B: VReg(10), T: I64},
		{Op: IRJalrIC, A: VReg(11), Imm: 0},
	}
	b.maxVreg = MaxVReg(b)

	ctx, _ := lowerABJITBlock(t, b)

	calls, rets := 0, 0
	for p := ctx.First(); p != nil; p = p.Link {
		switch p.As {
		case obj.ACALL:
			calls++
		case obj.ARET:
			rets++
		}
	}

	if calls != 0 {
		t.Errorf("found %d CALL instructions in block with no syscall/callback — expected 0", calls)
	}
	if rets != 0 {
		t.Errorf("found %d RET instructions — expected 0 (exit thunk uses JMP to retStub)", rets)
	}
}

// TestABJIT_SyscallCALL_CountsCorrect verifies that a block with one
// IRSyscall produces zero CALL/RET (syscall uses gocall JMP, exit
// thunk uses JMP to retStub).
func TestABJIT_SyscallCALL_CountsCorrect(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 0x1000, T: I64},
		{Op: IRSyscall, Imm: 0x1004},
	}
	b.CTab = []CSym{{Addr: 0}}
	b.maxVreg = MaxVReg(b)

	ctx, _ := lowerABJITBlock(t, b)

	calls, rets := 0, 0
	for p := ctx.First(); p != nil; p = p.Link {
		switch p.As {
		case obj.ACALL:
			calls++
		case obj.ARET:
			rets++
		}
	}

	if calls != 0 {
		t.Errorf("found %d CALL instructions — expected 0 (syscall uses gocall JMP)", calls)
	}
	if rets != 0 {
		t.Errorf("found %d RET instructions — expected 0 (exit thunk uses JMP to retStub)", rets)
	}
}
