package riscv

import (
	"encoding/binary"
	"testing"

	"riscv/goasm"
)

// Chain exit foundation tests for the rv8 lowerer.
//
// The rv8 chain exit uses MOVABS RCX, <sentinel> + JMP RCX (instead of
// the old R10-based sequence). These tests verify the encoding and
// metadata are correct.

const chainExitSentinel = int64(0x7BADC0DE7BADC0DE)

func lowerBlockWithResult(t *testing.T, b *Block) ([]byte, *LowerResult) {
	t.Helper()
	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	res, err := LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return bytes, res
}

func TestLower_ChainExit_MOVABS_EncodedAsExpected(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRChainExit, Imm: int64(0xDEAD0000), Imm2: 0},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)

	if res == nil {
		t.Fatal("LowerResult is nil")
	}
	if got := len(res.ChainExits); got != 1 {
		t.Fatalf("len(ChainExits) = %d, want 1", got)
	}
	ce := res.ChainExits[0]
	if ce.TargetPC != 0xDEAD0000 {
		t.Errorf("TargetPC = 0x%x, want 0xDEAD0000", ce.TargetPC)
	}
	if ce.MovProg == nil {
		t.Fatal("MovProg is nil")
	}

	pc := int(ce.MovProg.Pc)
	if pc < 0 || pc+10 > len(code) {
		t.Fatalf("MovProg.Pc = %d out of range [0, %d-10)", pc, len(code))
	}

	// MOVABS RCX, imm64 encoding: 48 B9 <8 bytes imm64>
	if code[pc] != 0x48 {
		t.Errorf("code[%d] = 0x%02x, want 0x48 (REX.W)", pc, code[pc])
	}
	if code[pc+1] != 0xB9 {
		t.Errorf("code[%d] = 0x%02x, want 0xB9 (MOV RCX opcode)", pc+1, code[pc+1])
	}
	gotImm := int64(binary.LittleEndian.Uint64(code[pc+2 : pc+10]))
	if gotImm != chainExitSentinel {
		t.Errorf("imm64 at pc+2 = 0x%016x, want 0x%016x (sentinel)",
			uint64(gotImm), uint64(chainExitSentinel))
	}

	// JMP RCX = FF E1 (2 bytes, no REX needed for RCX)
	if pc+12 > len(code) {
		t.Fatalf("not enough bytes for JMP RCX after MOVABS at pc=%d (codeLen=%d)",
			pc, len(code))
	}
	if code[pc+10] != 0xFF || code[pc+11] != 0xE1 {
		t.Errorf("JMP RCX bytes = %02x %02x, want FF E1",
			code[pc+10], code[pc+11])
	}
}

func TestLower_ChainExit_MultipleExitsIndependent(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRChainExit, Imm: int64(0xAAAA0000), Imm2: 0},
		{Op: IRChainExit, Imm: int64(0xBBBB0000), Imm2: 1},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)

	if got := len(res.ChainExits); got != 2 {
		t.Fatalf("len(ChainExits) = %d, want 2", got)
	}
	ce0, ce1 := res.ChainExits[0], res.ChainExits[1]
	if ce0.TargetPC != 0xAAAA0000 || ce1.TargetPC != 0xBBBB0000 {
		t.Errorf("TargetPCs: got 0x%x, 0x%x; want 0xAAAA0000, 0xBBBB0000",
			ce0.TargetPC, ce1.TargetPC)
	}
	pc0, pc1 := int(ce0.MovProg.Pc), int(ce1.MovProg.Pc)
	// Each chain exit is at least 12 bytes (10 MOVABS + 2 JMP RCX).
	if pc1 < pc0+12 {
		t.Errorf("second MOVABS at pc=%d overlaps first at pc=%d (need >= +12)",
			pc1, pc0)
	}
	for i, ce := range res.ChainExits {
		pc := int(ce.MovProg.Pc)
		if pc+10 > len(code) {
			t.Fatalf("exit %d: MovProg.Pc=%d out of range", i, pc)
		}
		imm := int64(binary.LittleEndian.Uint64(code[pc+2 : pc+10]))
		if imm != chainExitSentinel {
			t.Errorf("exit %d imm = 0x%016x, want sentinel", i, uint64(imm))
		}
	}
}

func TestLower_ChainExit_ChainEntry_NonZero_PastPrologue(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRChainExit, Imm: int64(0xDEAD0000), Imm2: 0},
	}
	b.maxVreg = MaxVReg(b)

	_, res := lowerBlockWithResult(t, b)
	if res.ChainEntryProg == nil {
		t.Fatal("ChainEntryProg is nil")
	}
	if res.ChainEntryProg.Pc <= 0 {
		t.Errorf("ChainEntryProg.Pc = %d, want > 0 (past prologue)",
			res.ChainEntryProg.Pc)
	}
}
