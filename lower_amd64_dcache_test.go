package riscv

import (
	"encoding/binary"
	//"encoding/hex"
	"syscall"
	"testing"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/internal/jitcall"
)

// ── Lowerer-level tests for the decoder_cache JALR IC (rv8JalrIC) ──
//
// rv8JalrIC reads four fields from the sret buffer published by
// CallAOT:
//   [sret+88]  = dcBase       (host pointer to decoder_cache mmap)
//   [sret+96]  = dcMask       (power-of-two size − 1, in bytes)
//   [sret+104] = vaddrBegin   (segment's guest-VA start)
//   [sret+112] = segSize      (segment's guest-VA size)
//
// When dcBase == 0 (no AOT segment / plain Call trampoline), or the
// target PC is out of bounds, or the cache slot is zero, the miss
// path fires: Result.Status = JitOKJalrMiss, Result.FaultAddr = siteIdx.
//
// When the cache slot is non-zero, the hit path deallocates the frame
// and jumps to the cached address (a chainEntry).

// DC1 — Encoding sanity: IRJalrIC produces non-trivial code that
// contains both JitOKJalrMiss (status on miss) and the siteIdx.
func TestLower_RV8_JalrIC_Encoding(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: VReg(10), Imm: 0},
	}
	b.maxVreg = MaxVReg(b)

	code, _ := lowerBlockWithResult(t, b)
	//t.Logf("rv8 JalrIC len=%d bytes", len(code))
	//t.Logf("bytes:\n%s", hex.Dump(code))

	if len(code) < 50 {
		t.Errorf("code too short (%d bytes) — expected decoder_cache sequence", len(code))
	}
}

// DC2 — The miss stub contains JitOKJalrMiss as an imm32 and the
// siteIdx as an imm32, verifiable by scanning the assembled bytes.
func TestLower_RV8_JalrIC_MissStubContents(t *testing.T) {
	const wantSiteIdx = int64(42)
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: VReg(10), Imm: wantSiteIdx},
	}
	b.maxVreg = MaxVReg(b)

	code, _ := lowerBlockWithResult(t, b)

	foundSiteIdx := false
	foundMissStatus := false
	for i := 0; i+4 <= len(code); i++ {
		imm32 := int64(int32(binary.LittleEndian.Uint32(code[i : i+4])))
		if imm32 == wantSiteIdx {
			foundSiteIdx = true
		}
		if imm32 == int64(JitOKJalrMiss) {
			foundMissStatus = true
		}
	}
	if !foundSiteIdx {
		t.Errorf("siteIdx %d not found as imm32 in assembled bytes", wantSiteIdx)
	}
	if !foundMissStatus {
		t.Errorf("JitOKJalrMiss (%d) not found as imm32 in assembled bytes", JitOKJalrMiss)
	}
}

// DC3 — Two IRJalrIC sites in one block each produce independent
// miss stubs. Both siteIdx values appear in the assembled bytes.
func TestLower_RV8_JalrIC_MultipleSitesIndependent(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRConst, Dst: VReg(10), Imm: 0x1000, T: I64},
		{Op: IRJalrIC, A: VReg(10), Imm: 7},
		{Op: IRConst, Dst: VReg(11), Imm: 0x2000, T: I64},
		{Op: IRJalrIC, A: VReg(11), Imm: 13},
	}
	b.maxVreg = MaxVReg(b)

	code, _ := lowerBlockWithResult(t, b)

	found7 := false
	found13 := false
	for i := 0; i+4 <= len(code); i++ {
		imm32 := int64(int32(binary.LittleEndian.Uint32(code[i : i+4])))
		if imm32 == 7 {
			found7 = true
		}
		if imm32 == 13 {
			found13 = true
		}
	}
	if !found7 {
		t.Error("siteIdx 7 not found in assembled bytes")
	}
	if !found13 {
		t.Error("siteIdx 13 not found in assembled bytes")
	}
}

// execJalrIC compiles and executes an IRJalrIC block via CallAOT,
// returning the Result. The caller provides the target PC (loaded
// into x10) and the decoder_cache parameters. Execution uses the host's
// RV8 backend; the encoding-only tests above still inspect AMD64 code.
func execJalrIC(t *testing.T, targetPC uint64, siteIdx int,
	dcBase uintptr, dcMask, vaddrBegin, segSize uint64) jitcall.Result {
	t.Helper()

	e := NewEmitter(nil)
	e.Const(e.XReg(10), int64(targetPC))
	e.WriteBackAll()
	e.JalrIC(e.XReg(10), siteIdx)

	fn, mem := compileIR(t, e.Block)
	defer freeExecMem(mem)

	var x [32]uint64
	var f [32]uint64
	var fcsr uint32
	return jitcall.CallAOT(fn, &x, &f, &fcsr, 0, 0,
		dcBase, dcMask, vaddrBegin, segSize, jitMaxBudget)
}

// DC4 — When dcBase == 0 (no AOT segment, plain Call), the JALR IC
// falls through to the miss path immediately. Result.Status must be
// JitOKJalrMiss and Result.FaultAddr must carry the siteIdx.
func TestLower_RV8_JalrIC_NoDcBase_ReturnsMiss(t *testing.T) {
	res := execJalrIC(t, 0x1000, 5, 0, 0, 0, 0)

	if res.PC != 0x1000 {
		t.Errorf("Result.PC = 0x%x, want 0x1000", res.PC)
	}
	if res.Status != uint64(JitOKJalrMiss) {
		t.Errorf("Result.Status = %d, want %d (JitOKJalrMiss)", res.Status, JitOKJalrMiss)
	}
	if res.FaultAddr != 5 {
		t.Errorf("Result.FaultAddr = %d, want 5 (siteIdx)", res.FaultAddr)
	}
}

// DC5 — When dcBase is set but the target PC is outside the segment
// (target - vaddrBegin >= segSize), the bounds check fails and the
// miss path fires.
func TestLower_RV8_JalrIC_OutOfBounds_ReturnsMiss(t *testing.T) {
	// Allocate a real page for dcBase so the pointer is valid (even
	// though we never actually read from it in this path).
	ps := syscall.Getpagesize()
	dcMem, err := syscall.Mmap(-1, 0, ps,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap dcMem: %v", err)
	}
	defer syscall.Munmap(dcMem)
	dcBase := uintptr(unsafe.Pointer(&dcMem[0]))

	// Segment covers [0x2000, 0x3000). Target 0x1000 is outside.
	res := execJalrIC(t, 0x1000, 3, dcBase, uint64(ps-1), 0x2000, 0x1000)

	if res.Status != uint64(JitOKJalrMiss) {
		t.Errorf("Result.Status = %d, want %d (JitOKJalrMiss)", res.Status, JitOKJalrMiss)
	}
	if res.FaultAddr != 3 {
		t.Errorf("Result.FaultAddr = %d, want 3 (siteIdx)", res.FaultAddr)
	}
}

// DC6 — When dcBase is set, target is in bounds, but the cache slot
// is zero (no block compiled at that address), the miss path fires.
func TestLower_RV8_JalrIC_EmptySlot_ReturnsMiss(t *testing.T) {
	ps := syscall.Getpagesize()
	dcMem, err := syscall.Mmap(-1, 0, ps,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap dcMem: %v", err)
	}
	defer syscall.Munmap(dcMem)
	dcBase := uintptr(unsafe.Pointer(&dcMem[0]))

	// Segment covers [0x1000, 0x2000). Target 0x1000 is in bounds.
	// dcMem is zeroed (mmap guarantee), so the cache slot is zero.
	res := execJalrIC(t, 0x1000, 9, dcBase, uint64(ps-1), 0x1000, 0x1000)

	if res.Status != uint64(JitOKJalrMiss) {
		t.Errorf("Result.Status = %d, want %d (JitOKJalrMiss)", res.Status, JitOKJalrMiss)
	}
	if res.FaultAddr != 9 {
		t.Errorf("Result.FaultAddr = %d, want 9 (siteIdx)", res.FaultAddr)
	}
}

// DC7 — Cache hit: when dcBase is set, target is in bounds, and the
// cache slot holds a valid chainEntry address, the JALR IC jumps
// directly to that address (no miss). Use a real native chainEntry so this
// exercises both AMD64 and ARM64 calling conventions.
func TestLower_RV8_JalrIC_CacheHit_JumpsToEntry(t *testing.T) {
	ps := syscall.Getpagesize()

	// Allocate a decoder_cache page (zeroed).
	dcMem, err := syscall.Mmap(-1, 0, ps,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap dcMem: %v", err)
	}
	defer syscall.Munmap(dcMem)
	dcBase := uintptr(unsafe.Pointer(&dcMem[0]))

	// Compile a target that writes a distinctive Result.PC and returns.
	j := NewSandboxJIT()
	defer j.Close()
	j.SetRegPolicy(PolicyRV8)
	e := NewEmitter(nil)
	e.Ret(0xBEEF, 0, VRegZero)
	target, err := j.jitCompile(&emitResult{block: e.Block, numInsns: 1})
	if err != nil {
		t.Fatalf("compile cache target: %v", err)
	}
	if target.chainEntry == 0 {
		t.Fatal("cache target has no chain entry")
	}

	// Segment covers [0x1000, 0x2000). Target = 0x1000.
	// Cache index for target 0x1000 with vaddrBegin=0x1000:
	//   offset = target - vaddrBegin = 0
	//   byteOff = offset * 4 = 0
	//   masked = 0 & dcMask = 0
	// So slot 0 in dcMem holds the chainEntry pointer.
	binary.LittleEndian.PutUint64(dcMem[0:], uint64(target.chainEntry))

	res := execJalrIC(t, 0x1000, 0, dcBase, uint64(ps-1), 0x1000, 0x1000)

	if res.PC != 0xBEEF {
		t.Errorf("Result.PC = 0x%x, want 0xBEEF (hit stub should have set it)", res.PC)
	}
	if res.Status != 0 {
		t.Errorf("Result.Status = %d, want 0 (hit path)", res.Status)
	}
}
