package ir

import (
	"encoding/binary"
	"testing"

	"riscv/goasm"
)

// Part A — MOVABS chain exit foundation tests.
//
// These tests exercise ir.LowerAMD64's chain-exit machinery directly, without
// touching the emitChainableReturn stub in jit_emit_ir.go. The goal is to
// prove (or disprove) that:
//   - The MOVABS is encoded as 49 BA <imm64> (10 bytes).
//   - LowerResult.ChainExits is populated with the right MovProg.Pc.
//   - LowerResult.ChainEntryProg is non-nil after assembly.
//
// If all pass, the "MOVABS offset calculation" TODO in jit_emit_ir.go is
// historical and the chain-exit pipeline is ready. If any fail, the failing
// assertion pinpoints the bug to fix before wiring emitChainableReturn.

// The sentinel lowerChainExit writes into the MOVABS imm64. Must match
// ir/lower_amd64.go:378 (const sentinel).
const chainExitSentinel = int64(0x7BADC0DE7BADC0DE)

// lowerBlockWithResult runs the full pipeline (allocate + lower + assemble)
// and returns both the assembled bytes and the LowerResult. Mirrors the
// lowerBlock helper in lower_amd64_test.go but also returns the metadata
// needed for chain-exit assertions.
func lowerBlockWithResult(t *testing.T, b *Block) ([]byte, *LowerResult) {
	t.Helper()
	pool := AMD64Pool(b)
	pinned := AMD64Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	res, err := LowerAMD64(ctx, b, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64: %v", err)
	}

	bytes, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return bytes, res
}

// A1 — The MOVABS for a single chain exit is encoded as 10 bytes
// (49 BA <imm64>) with the sentinel at offset +2, followed by JMP R10
// (41 FF E2).
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
	if ce.StubProg == nil {
		t.Fatal("StubProg is nil")
	}

	pc := int(ce.MovProg.Pc)
	if pc < 0 || pc+10 > len(code) {
		t.Fatalf("MovProg.Pc = %d out of range [0, %d-10)", pc, len(code))
	}

	// Byte 0: REX.W+B for R10 = 0x49
	if code[pc] != 0x49 {
		t.Errorf("code[%d] = 0x%02x, want 0x49 (REX.W+B)", pc, code[pc])
	}
	// Byte 1: MOV r64, imm64 opcode for R10 (B8+r where r=2 under REX.B) = 0xBA
	if code[pc+1] != 0xBA {
		t.Errorf("code[%d] = 0x%02x, want 0xBA (MOV R10 opcode)", pc+1, code[pc+1])
	}
	// Bytes 2..10: sentinel little-endian.
	gotImm := int64(binary.LittleEndian.Uint64(code[pc+2 : pc+10]))
	if gotImm != chainExitSentinel {
		t.Errorf("imm64 at pc+2 = 0x%016x, want 0x%016x (sentinel)",
			uint64(gotImm), uint64(chainExitSentinel))
	}

	// Bytes 10..13: JMP R10 = 41 FF E2.
	if pc+13 > len(code) {
		t.Fatalf("not enough bytes for JMP R10 after MOVABS at pc=%d (codeLen=%d)",
			pc, len(code))
	}
	if code[pc+10] != 0x41 || code[pc+11] != 0xFF || code[pc+12] != 0xE2 {
		t.Errorf("JMP R10 bytes = %02x %02x %02x, want 41 FF E2",
			code[pc+10], code[pc+11], code[pc+12])
	}
}

// A4 — Two chain exits in one block each get their own MOVABS with
// non-overlapping patch locations; both contain the sentinel.
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
	// Each chain exit is at least 13 bytes (10 MOVABS + 3 JMP R10).
	if pc1 < pc0+13 {
		t.Errorf("second MOVABS at pc=%d overlaps first at pc=%d (need ≥ +13)",
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

// A5 — ChainEntryProg must be non-nil after lowering and its Pc must be
// strictly greater than zero after assembly (it sits past the prologue's
// arg copies and the XORQ RBP, RBP that zeros the IC).
//
// A chained inbound jump must skip IC zeroing so RBP (the instruction
// counter) accumulates across blocks. If ChainEntryProg lands at Pc=0 or
// in the middle of the prologue, IC resets per block and hot-loop MIPS
// measurements lie.
func TestLower_ChainExit_ChainEntry_NonZero_PastPrologue(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRChainExit, Imm: int64(0xDEAD0000), Imm2: 0},
	}
	b.maxVreg = MaxVReg(b)

	_, res := lowerBlockWithResult(t, b)
	if res.ChainEntryProg == nil {
		t.Fatal("ChainEntryProg is nil — chain entry Prog was never emitted " +
			"in emitPrologue. jit_native.go:90 then skips chain-exit setup " +
			"entirely. This is the bug the TODO refers to.")
	}
	if res.ChainEntryProg.Pc <= 0 {
		t.Errorf("ChainEntryProg.Pc = %d, want > 0 (past prologue)",
			res.ChainEntryProg.Pc)
	}
}
