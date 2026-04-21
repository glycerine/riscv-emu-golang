package ir

import (
	"encoding/binary"
	"testing"
)

// Byte-level tests for the 2-way lowerJalrIC / emitJalrMissStub
// (Phase 1.5). The inline-cache sequence at each site is:
//
//	MOVQ   tgt, 0(RBX)
//	MOVABS R10, <pc0_sentinel>    ; 10 bytes; imm64 at +2 = slot pc[0]
//	CMPQ   tgt, R10
//	JEQ    .hit0                   ; short (2B) when .hit0 is within 127
//	MOVABS R10, <pc1_sentinel>    ; 10 bytes; imm64 at +2 = slot pc[1]
//	CMPQ   tgt, R10
//	JNE    .miss                   ; short or near (2B/6B)
//	ADDQ   $frameSize, RSP         ; omitted if frameSize == 0
//	MOVABS R10, <fn1_sentinel>    ; 10 bytes; imm64 at +2 = slot fn[1]
//	JMP    R10                     ; 3 bytes (41 FF E2)
//	.hit0:
//	ADDQ   $frameSize, RSP
//	MOVABS R10, <fn0_sentinel>    ; 10 bytes; imm64 at +2 = slot fn[0]
//	JMP    R10                     ; 3 bytes
//
// The shared miss stub is appended after all sites.

// JI1 — Single 2-way JALR IC site encodes four MOVABS R10 (pc[0],
// pc[1], fn[1], fn[0]) each with the sentinel in imm64. The last
// two are followed by JMP R10.
func TestLower_JalrIC_MOVABS_EncodedAsExpected(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: 10, Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: 10, Imm: 7},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)

	if got := len(res.JalrICs); got != 1 {
		t.Fatalf("len(JalrICs) = %d, want 1", got)
	}
	ic := res.JalrICs[0]
	if ic.SiteIdx != 7 {
		t.Errorf("SiteIdx = %d, want 7", ic.SiteIdx)
	}
	for k := 0; k < 2; k++ {
		if ic.PcMov[k] == nil {
			t.Fatalf("PcMov[%d] is nil", k)
		}
		if ic.FnMov[k] == nil {
			t.Fatalf("FnMov[%d] is nil", k)
		}
	}
	if ic.StubProg == nil {
		t.Fatal("StubProg is nil")
	}

	// All four imm64 slots should hold the sentinel before backpatch.
	slots := [4]int{
		int(ic.PcMov[0].Pc),
		int(ic.PcMov[1].Pc),
		int(ic.FnMov[0].Pc),
		int(ic.FnMov[1].Pc),
	}
	for i, off := range slots {
		if off+10 > len(code) {
			t.Fatalf("slot %d: offset %d out of range (codeLen=%d)",
				i, off, len(code))
		}
		if code[off] != 0x49 || code[off+1] != 0xBA {
			t.Errorf("slot %d prefix = %02x %02x, want 49 BA (MOVABS R10)",
				i, code[off], code[off+1])
		}
		imm := int64(binary.LittleEndian.Uint64(code[off+2 : off+10]))
		if imm != chainExitSentinel {
			t.Errorf("slot %d imm64 = 0x%016x, want sentinel 0x%016x",
				i, uint64(imm), uint64(chainExitSentinel))
		}
	}

	// JMP R10 (41 FF E2) must appear immediately after fnMov[1] and
	// immediately after fnMov[0] (both hit-path terminators).
	for _, off := range []int{int(ic.FnMov[1].Pc), int(ic.FnMov[0].Pc)} {
		if off+13 > len(code) {
			t.Fatalf("not enough bytes after MOVABS at %d", off)
		}
		if code[off+10] != 0x41 || code[off+11] != 0xFF || code[off+12] != 0xE2 {
			t.Errorf("JMP R10 after MOVABS at %d = %02x %02x %02x, want 41 FF E2",
				off, code[off+10], code[off+11], code[off+12])
		}
	}
}

// JI2 — Two JALR IC sites in one block each get their own 4-slot
// MOVABS set. All 8 imm64 offsets are distinct and non-overlapping.
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

	offs := []int{
		int(ic0.PcMov[0].Pc), int(ic0.PcMov[1].Pc),
		int(ic0.FnMov[0].Pc), int(ic0.FnMov[1].Pc),
		int(ic1.PcMov[0].Pc), int(ic1.PcMov[1].Pc),
		int(ic1.FnMov[0].Pc), int(ic1.FnMov[1].Pc),
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

	for i, off := range offs {
		if off+10 > len(code) {
			t.Fatalf("offs[%d]=%d out of range (len=%d)", i, off, len(code))
		}
		imm := int64(binary.LittleEndian.Uint64(code[off+2 : off+10]))
		if imm != chainExitSentinel {
			t.Errorf("offs[%d] imm64 = 0x%016x, want sentinel", i, uint64(imm))
		}
	}

	if ic0.StubProg == nil || ic1.StubProg == nil {
		t.Fatalf("stub progs nil: %v, %v", ic0.StubProg, ic1.StubProg)
	}
	if ic0.StubProg.Pc == ic1.StubProg.Pc {
		t.Errorf("two sites share a stub at Pc=%d", ic0.StubProg.Pc)
	}
}

// JI3 — The miss stub writes JitOKJalrMiss to sret.Status and siteIdx
// to sret.FaultAddr. Both appear as imm32 literals in the stub bytes.
// The stub terminates with RET (0xC3).
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

	if code[len(code)-1] != 0xC3 {
		t.Errorf("last byte = 0x%02x, want 0xC3 (RET)", code[len(code)-1])
	}
}

// JI4 — The JEQ after pc[0] CMPQ targets the .hit0 NOP; the JNE after
// pc[1] CMPQ targets the miss stub. Decode each to confirm destination.
func TestLower_JalrIC_BranchTargets(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: 10, Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: 10, Imm: 0},
	}
	b.maxVreg = MaxVReg(b)

	code, res := lowerBlockWithResult(t, b)
	ic := res.JalrICs[0]
	stubStart := int(ic.StubProg.Pc)

	// Decode JEQ after pcMov[0] CMPQ.
	pcAfterCmp0 := int(ic.PcMov[0].Pc) + 10 + 3 // MOVABS + CMPQ
	jeq0Target, jeq0End := decodeConditionalBranch(t, code, pcAfterCmp0, 0x74, 0x84)
	if jeq0Target == 0 {
		t.Fatalf("could not decode JEQ at pc=%d", pcAfterCmp0)
	}

	// Decode JNE after pcMov[1] CMPQ. The JNE is at pcMov[1].Pc + 10 + 3.
	pcAfterCmp1 := int(ic.PcMov[1].Pc) + 10 + 3
	jne1Target, _ := decodeConditionalBranch(t, code, pcAfterCmp1, 0x75, 0x85)
	if jne1Target == 0 {
		t.Fatalf("could not decode JNE at pc=%d", pcAfterCmp1)
	}

	// JEQ should land in the range [pcMov[1].Pc+.., FnMov[0].Pc-ish] — i.e.
	// somewhere between the hit1 JMP R10 and the hit0 MOVABS. The exact
	// position of the .hit0 NOP is within that range. Verify target is > jeq0End
	// (forward) and < stubStart (above the stub).
	if jeq0Target <= jeq0End {
		t.Errorf("JEQ target = %d, want forward (> end=%d)", jeq0Target, jeq0End)
	}
	if jeq0Target >= stubStart {
		t.Errorf("JEQ target = %d, should land before stubStart=%d",
			jeq0Target, stubStart)
	}

	// JNE target should equal stubStart.
	if jne1Target != stubStart {
		t.Errorf("JNE target = %d, want stubStart = %d", jne1Target, stubStart)
	}
}

// decodeConditionalBranch decodes a short (shortOpc) or near (0x0F + nearOpc)
// conditional branch starting at `off`. Returns (absTarget, endOfBranchInsn)
// or (0, 0) if not one of those forms.
func decodeConditionalBranch(t *testing.T, code []byte, off int, shortOpc, nearOpc byte) (int, int) {
	t.Helper()
	if off >= len(code) {
		return 0, 0
	}
	switch code[off] {
	case shortOpc:
		if off+2 > len(code) {
			return 0, 0
		}
		disp := int8(code[off+1])
		end := off + 2
		return end + int(disp), end
	case 0x0F:
		if off+6 > len(code) || code[off+1] != nearOpc {
			return 0, 0
		}
		disp := int32(binary.LittleEndian.Uint32(code[off+2 : off+6]))
		end := off + 6
		return end + int(disp), end
	}
	return 0, 0
}
