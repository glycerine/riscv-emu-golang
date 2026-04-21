package ir

import (
	"encoding/binary"
	"testing"
)

// Byte-level tests for lowerJalrIC / emitJalrMissStub (Phase 1, Step 2).
//
// The JALR inline-cache sequence at each site is:
//
//	MOVQ   tgt, 0(RBX)
//	MOVABS R10, <cache_pc_sentinel>  ; 10 bytes; imm64 at +2 = patch slot 1
//	CMPQ   tgt, R10
//	JNE    .miss
//	ADDQ   $frameSize, RSP           ; (omitted when frameSize == 0)
//	MOVABS R10, <cache_fn_sentinel>  ; 10 bytes; imm64 at +2 = patch slot 2
//	JMP    R10                        ; 3 bytes (41 FF E2)
//
// The sentinel is 0x7BADC0DE7BADC0DE (reused from chain-exit — see
// chainExitSentinel in lower_amd64_chain_test.go).

// JI1 — Single JALR IC site encodes two MOVABS R10 with the sentinel in
// each imm64, followed eventually by JMP R10.
func TestLower_JalrIC_MOVABS_EncodedAsExpected(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: 10, Imm: 0x1000, T: I64}, // populate vreg x10
		{Op: IRJalrIC, A: 10, Imm: 7},                // siteIdx = 7
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)

	if res == nil {
		t.Fatal("LowerResult is nil")
	}
	if got := len(res.JalrICs); got != 1 {
		t.Fatalf("len(JalrICs) = %d, want 1", got)
	}
	ic := res.JalrICs[0]
	if ic.SiteIdx != 7 {
		t.Errorf("SiteIdx = %d, want 7", ic.SiteIdx)
	}
	if ic.PcMov == nil || ic.FnMov == nil {
		t.Fatalf("PcMov=%v, FnMov=%v, want both non-nil", ic.PcMov, ic.FnMov)
	}
	if ic.StubProg == nil {
		t.Fatal("StubProg is nil")
	}

	// Check cache_pc MOVABS: 49 BA <sentinel>
	pcOff := int(ic.PcMov.Pc)
	if pcOff < 0 || pcOff+10 > len(code) {
		t.Fatalf("PcMov.Pc = %d out of range [0, %d-10)", pcOff, len(code))
	}
	if code[pcOff] != 0x49 || code[pcOff+1] != 0xBA {
		t.Errorf("cache_pc MOVABS first 2 bytes = %02x %02x, want 49 BA",
			code[pcOff], code[pcOff+1])
	}
	pcImm := int64(binary.LittleEndian.Uint64(code[pcOff+2 : pcOff+10]))
	if pcImm != chainExitSentinel {
		t.Errorf("cache_pc imm64 = 0x%016x, want sentinel 0x%016x",
			uint64(pcImm), uint64(chainExitSentinel))
	}

	// Check cache_fn MOVABS: 49 BA <sentinel>
	fnOff := int(ic.FnMov.Pc)
	if fnOff <= pcOff+10 {
		t.Errorf("FnMov.Pc = %d should be > PcMov.Pc+10 = %d", fnOff, pcOff+10)
	}
	if fnOff+10 > len(code) {
		t.Fatalf("FnMov.Pc = %d out of range", fnOff)
	}
	if code[fnOff] != 0x49 || code[fnOff+1] != 0xBA {
		t.Errorf("cache_fn MOVABS first 2 bytes = %02x %02x, want 49 BA",
			code[fnOff], code[fnOff+1])
	}
	fnImm := int64(binary.LittleEndian.Uint64(code[fnOff+2 : fnOff+10]))
	if fnImm != chainExitSentinel {
		t.Errorf("cache_fn imm64 = 0x%016x, want sentinel 0x%016x",
			uint64(fnImm), uint64(chainExitSentinel))
	}

	// Check JMP R10 (41 FF E2) immediately after cache_fn MOVABS.
	if fnOff+13 > len(code) {
		t.Fatalf("not enough bytes for JMP R10 after cache_fn MOVABS "+
			"(fnOff=%d, codeLen=%d)", fnOff, len(code))
	}
	if code[fnOff+10] != 0x41 || code[fnOff+11] != 0xFF ||
		code[fnOff+12] != 0xE2 {
		t.Errorf("JMP R10 bytes after cache_fn = %02x %02x %02x, want 41 FF E2",
			code[fnOff+10], code[fnOff+11], code[fnOff+12])
	}
}

// JI2 — Two JALR IC sites in one block each get their own MOVABS pair
// and their own miss stub. Patch offsets don't overlap.
func TestLower_JalrIC_MultipleSitesIndependent(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: 10, Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: 10, Imm: 0},
		{Op: IRConst, Dst: 11, Imm: 0x2000, T: I64},
		{Op: IRJalrIC, A: 11, Imm: 1},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)

	if got := len(res.JalrICs); got != 2 {
		t.Fatalf("len(JalrICs) = %d, want 2", got)
	}
	ic0, ic1 := res.JalrICs[0], res.JalrICs[1]
	if ic0.SiteIdx != 0 || ic1.SiteIdx != 1 {
		t.Errorf("SiteIdxs: got %d, %d; want 0, 1", ic0.SiteIdx, ic1.SiteIdx)
	}

	// All four MOVABS imm64 slots must be non-overlapping and contain sentinel.
	offs := []int{
		int(ic0.PcMov.Pc), int(ic0.FnMov.Pc),
		int(ic1.PcMov.Pc), int(ic1.FnMov.Pc),
	}
	for i := 0; i < len(offs); i++ {
		for j := i + 1; j < len(offs); j++ {
			diff := offs[j] - offs[i]
			if diff < 0 {
				diff = -diff
			}
			if diff < 10 {
				t.Errorf("MOVABS at offs[%d]=%d overlaps offs[%d]=%d (need ≥10 bytes apart)",
					i, offs[i], j, offs[j])
			}
		}
	}

	// Each imm64 slot should contain the sentinel before backpatch.
	for i, off := range offs {
		if off+10 > len(code) {
			t.Fatalf("offs[%d]=%d out of range (len=%d)", i, off, len(code))
		}
		imm := int64(binary.LittleEndian.Uint64(code[off+2 : off+10]))
		if imm != chainExitSentinel {
			t.Errorf("offs[%d] imm64 = 0x%016x, want sentinel", i, uint64(imm))
		}
	}

	// Stubs should also be distinct.
	if ic0.StubProg == nil || ic1.StubProg == nil {
		t.Fatalf("stub progs nil: %v, %v", ic0.StubProg, ic1.StubProg)
	}
	if ic0.StubProg.Pc == ic1.StubProg.Pc {
		t.Errorf("two sites share a stub at Pc=%d", ic0.StubProg.Pc)
	}
}

// JI3 — The miss stub writes JitOKJalrMiss to sret.Status and siteIdx
// to sret.FaultAddr. Specifically, the stub ends with RET (0xC3). The
// site index appears somewhere in the stub bytes as a literal.
func TestLower_JalrIC_MissStubWritesSiteIdx(t *testing.T) {
	const wantSiteIdx = int64(42)
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: 10, Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: 10, Imm: wantSiteIdx},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)
	ic := res.JalrICs[0]
	if ic.StubProg == nil {
		t.Fatal("StubProg is nil")
	}

	stubStart := int(ic.StubProg.Pc)
	if stubStart < 0 || stubStart >= len(code) {
		t.Fatalf("StubProg.Pc = %d out of range [0, %d)", stubStart, len(code))
	}

	// Scan from stubStart to end for the siteIdx and JitOKJalrMiss immediates.
	// Both are written as MOVQ $imm, mem(RBX) which encodes as:
	//   48 C7 43 <disp8> <imm32>  (for imm that fits in int32)
	// We don't match the exact byte sequence, just the immediates' presence.
	tail := code[stubStart:]
	foundSiteIdx := false
	foundMissStatus := false
	for i := 0; i+4 <= len(tail); i++ {
		imm32 := int64(int32(binary.LittleEndian.Uint32(tail[i : i+4])))
		if imm32 == wantSiteIdx {
			foundSiteIdx = true
		}
		if imm32 == int64(JitOKJalrMiss) {
			foundMissStatus = true
		}
	}
	if !foundSiteIdx {
		t.Errorf("siteIdx %d not found as imm32 in miss stub bytes", wantSiteIdx)
	}
	if !foundMissStatus {
		t.Errorf("JitOKJalrMiss (=%d) not found as imm32 in miss stub bytes",
			JitOKJalrMiss)
	}

	// RET must be the last byte.
	if code[len(code)-1] != 0xC3 {
		t.Errorf("last byte = 0x%02x, want 0xC3 (RET)", code[len(code)-1])
	}
}

// JI4 — The JNE after cache_pc MOVABS targets the miss stub.
// Verified by emitted-branch byte patterns: after the JNE, we should
// either have a short-form (75 disp8) or near-form (0F 85 disp32)
// branch whose displacement lands inside the miss stub.
func TestLower_JalrIC_JNETargetsMissStub(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: 10, Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: 10, Imm: 0},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)
	ic := res.JalrICs[0]

	// JNE must be AFTER the cache_pc MOVABS (+10 bytes) and AFTER the
	// CMPQ (3 bytes) that follows. The assembler may short-form or
	// near-form the JNE; both end up pointing into the miss stub.
	stubStart := int(ic.StubProg.Pc)
	pcAfterCMP := int(ic.PcMov.Pc) + 10 + 3 // MOVABS(10) + CMPQ(3)
	if pcAfterCMP >= len(code) {
		t.Fatalf("pcAfterCMP=%d beyond code len %d", pcAfterCMP, len(code))
	}

	// Decode a short-form JNE (75 disp8) or near-form (0F 85 disp32).
	var jneEnd, jneTarget int
	switch code[pcAfterCMP] {
	case 0x75: // short JNE
		disp := int8(code[pcAfterCMP+1])
		jneEnd = pcAfterCMP + 2
		jneTarget = jneEnd + int(disp)
	case 0x0F:
		if code[pcAfterCMP+1] != 0x85 {
			t.Fatalf("byte after 0F is 0x%02x, want 0x85 (near JNE)",
				code[pcAfterCMP+1])
		}
		disp := int32(binary.LittleEndian.Uint32(code[pcAfterCMP+2 : pcAfterCMP+6]))
		jneEnd = pcAfterCMP + 6
		jneTarget = jneEnd + int(disp)
	default:
		t.Fatalf("byte at pcAfterCMP=%d is 0x%02x, want 0x75 (short JNE) or 0x0F (near JNE prefix)",
			pcAfterCMP, code[pcAfterCMP])
	}

	if jneTarget != stubStart {
		t.Errorf("JNE target = %d, want stubStart = %d", jneTarget, stubStart)
	}
}
