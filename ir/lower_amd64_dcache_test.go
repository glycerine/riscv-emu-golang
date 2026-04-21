package ir

import (
	"encoding/hex"
	"testing"

	"riscv/goasm"
)

// TestLower_DecoderCacheAttempt_Encoding dumps the assembled bytes
// for a block whose sole op is IRJalrIC, both with and without the
// AOT decoder_cache attempt, so we can visually confirm the
// sequence is what we intended.
func TestLower_DecoderCacheAttempt_Encoding(t *testing.T) {
	// Minimal IR: a const into x10 + JalrIC targeting x10.
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: VReg(10), Imm: 0},
	}
	b.maxVreg = MaxVReg(b)

	// Lower WITHOUT the AOT attempt (baseline).
	pool := AMD64Pool(b)
	pinned := AMD64Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64: %v", err)
	}
	bytesNoAot, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// Lower WITH the AOT attempt.
	ctx2 := goasm.New(goasm.AMD64)
	ctx2.Append(ctx2.NewATEXT())
	if _, err := LowerAMD64AOT(ctx2, b, alloc); err != nil {
		t.Fatalf("LowerAMD64AOT: %v", err)
	}
	bytesAot, err := ctx2.Assemble()
	if err != nil {
		t.Fatalf("Assemble(AOT): %v", err)
	}

	t.Logf("baseline  len=%d bytes", len(bytesNoAot))
	t.Logf("AOT       len=%d bytes", len(bytesAot))
	t.Logf("AOT-extra len=%d bytes", len(bytesAot)-len(bytesNoAot))

	// Print the AOT bytes in two halves so we can diff them.
	t.Logf("AOT bytes:\n%s", hex.Dump(bytesAot))
}
