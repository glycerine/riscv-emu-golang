package ir

import (
	"encoding/hex"
	"testing"

	"riscv/goasm"
)

func TestLower_RV8_JalrIC_Encoding(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: VReg(10), Imm: 0},
	}
	b.maxVreg = MaxVReg(b)

	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64_RV8(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Logf("rv8 JalrIC len=%d bytes", len(code))
	t.Logf("bytes:\n%s", hex.Dump(code))
}
