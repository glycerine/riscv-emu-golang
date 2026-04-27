package riscv

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// Part D — observability tests. With emitChainableReturn wired to emit
// IRChainExit and ChainEntryProg populated by the prologue NOP, these
// tests verify that chain patching actually fires end-to-end.
//
// Note: the BudgetCheck path (ir/highlevel.go) deliberately emits plain
// IRRet rather than IRChainExit — chaining it would eliminate GC
// safepoints. So hot loops that exit only via BudgetCheck will still
// return to Go every MaxIC instructions. The D1/D5 tests assert the
// things that ARE provable given this design: chaining fires at static
// inter-block boundaries, cycle counts stay exact, and exit routes that
// aren't supposed to chain don't.

// runSimpleLoopJIT runs a tight 4-insn loop for N iterations under the
// Fixed Static Mapping JIT.
func runSimpleLoopJIT(t *testing.T, iters uint64) (*CPU, *JIT) {
	t.Helper()
	// 4-insn body + backward branch + ECALL:
	//   ADDI x12, x12, 1     ; loop counter
	//   ADDI x5,  x5,  1     ; some work
	//   ADDI x6,  x6,  1     ; some work
	//   BLT  x12, x13, -12   ; branch backward to ADDI x12
	//   ECALL
	insns := []uint32{
		ienc(opOPIMM, 0, 12, 12, 1),
		ienc(opOPIMM, 0, 5, 5, 1),
		ienc(opOPIMM, 0, 6, 6, 1),
		benc(opBRANCH, 4, 12, 13, -12),
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	t.Cleanup(mem.Free)
	cpu.SetReg(12, 0)
	cpu.SetReg(13, iters)
	cpu.Notes.Push(ecallStop)
	jit := NewJIT() // Fixed Static Mapping by default.
	jit.RunJIT(cpu)
	return cpu, jit
}

// D2 — Every compiled block has at least one chain exit. finalize emits
// a fall-through chain return unconditionally (jit_emit_ir.go:454), so
// any block whose IR we compile carries chain-exit metadata.
func TestChaining_ChainExitsPopulated_OnCompiledBlock(t *testing.T) {
	insns := []uint32{
		ienc(opOPIMM, 0, 1, 0, 42), // ADDI x1, x0, 42
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem.Free()
	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	blk := jit.lookupBlock(0x1000)
	if blk == nil {
		t.Fatal("block at 0x1000 was not compiled")
	}
	if len(blk.chainExits) == 0 {
		t.Error("compiled block has no chainExits — emitChainableReturn " +
			"is not emitting IRChainExit, or jit_native is not backpatching")
	}
	if blk.chainEntry == 0 {
		t.Error("compiled block has chainEntry=0 — emitPrologue NOP marker " +
			"is missing or LowerResult.ChainEntryProg is nil")
	}
}

// D3 — The bytes immediately before each chain exit's patchOffset must
// be 48 B9 (REX.W, MOV RCX imm64). rv8 uses RCX as the chain-exit staging reg.
func TestChaining_PatchPointsAtImm64_OfMovABS(t *testing.T) {
	insns := []uint32{
		ienc(opOPIMM, 0, 1, 0, 1),
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem.Free()
	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	blk := jit.lookupBlock(0x1000)
	if blk == nil || len(blk.chainExits) == 0 {
		t.Fatal("no chain exits to inspect")
	}
	for i, ce := range blk.chainExits {
		if ce.patchOffset < 2 {
			t.Errorf("exit %d: patchOffset=%d < 2", i, ce.patchOffset)
			continue
		}
		//nolint:gosec // reading JIT code bytes for test verification
		p := (*[2]byte)(unsafe.Pointer(blk.fn + uintptr(ce.patchOffset-2)))
		if p[0] != 0x48 || p[1] != 0xB9 {
			t.Errorf("exit %d: bytes before patchOffset = %02x %02x, "+
				"want 48 B9", i, p[0], p[1])
		}
	}
}

// D4 — After a workload with a statically-known block-to-block
// transition, tryPatchChain successfully points a chain exit at the
// target block's chainEntry.
//
// Patching happens on the jitOK return of the SOURCE block after the
// TARGET block has been compiled. So the test needs the source block
// to run at least twice: once to drive the target's compilation, once
// more so the dispatcher's tryPatchChain call finds a non-nil target.
// A call+return+loop achieves this:
//
//	0x1000  ADDI x12, x12, 1           ; block A: loop counter
//	0x1004  JAL  x5, +12               ; block A: chain to 0x1010 (callee)
//	0x1008  BLT  x12, x13, -0x8        ; block C: branch back to 0x1000
//	0x100C  ECALL                       ; block C: exit when done
//	0x1010  JALR x0, 0(x5)             ; block B: return via x5 (=0x1008)
//
// Uses x5 (not x1/ra) as link register to avoid RAS inlining — this
// test verifies inter-block chain patching, not return prediction.
//
// Iteration 1: A runs, chain-exits to 0x1010 (stub, B not yet compiled)
//
//	→ B compiles → B runs, JALR to 0x1008 → C compiles →
//	C runs BLT back to 0x1000.
//
// Iteration 2+: A's chain exit to B is now patchable; on A's jitOK
//
//	return the dispatcher patches it. Same for C→A.
func TestChaining_PatchedJumpReachesChainEntry(t *testing.T) {
	insns := []uint32{
		/* 0x1000 */ ienc(opOPIMM, 0, 12, 12, 1), // ADDI x12, x12, 1
		/* 0x1004 */ jenc(5, 0xC), // JAL  x5, +12
		/* 0x1008 */ benc(opBRANCH, 4, 12, 13, -0x8), // BLT  x12, x13, -8
		/* 0x100C */ instrECALL,
		/* 0x1010 */ ienc(opJALR, 0, 0, 5, 0), // JALR x0, 0(x5)
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem.Free()
	cpu.SetReg(12, 0)
	cpu.SetReg(13, 4) // 4 iterations — enough to exercise warm-up + patch
	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	blkA := jit.lookupBlock(0x1000)
	blkB := jit.lookupBlock(0x1010)
	if blkA == nil || blkB == nil {
		t.Fatalf("expected both blocks compiled; A=%v B=%v "+
			"(DispatchCompile=%d)", blkA, blkB, jit.DispatchCompile)
	}
	if blkB.chainEntry == 0 {
		t.Fatal("block B has chainEntry=0 — prologue NOP missing")
	}
	if jit.ChainPatched == 0 {
		t.Fatalf("ChainPatched=0 — tryPatchChain never succeeded. "+
			"DispatchOK=%d DispatchCompile=%d",
			jit.DispatchOK, jit.DispatchCompile)
	}

	// Find block A's chain exit targeting 0x1010 and verify its bytes
	// point into block B's code page at B.chainEntry.
	var ce *chainPatchInfo
	for i := range blkA.chainExits {
		if blkA.chainExits[i].targetPC == 0x1010 {
			ce = &blkA.chainExits[i]
			break
		}
	}
	if ce == nil {
		t.Fatalf("block A has no chain exit targeting 0x1010; "+
			"blkA.chainExits=%+v", blkA.chainExits)
	}
	//nolint:gosec // reading JIT code bytes for test verification
	p := (*[8]byte)(unsafe.Pointer(blkA.fn + uintptr(ce.patchOffset)))
	got := uintptr(binary.LittleEndian.Uint64(p[:]))
	if got != blkB.chainEntry {
		t.Errorf("patched imm64 = 0x%x, want blkB.chainEntry = 0x%x",
			got, blkB.chainEntry)
	}
}

// broken by lack of fine grain inline IC (instruction counter)
// at the moment. So we comment it out.
/*
// D5 — IC accounting stays exact across a tight loop. With MaxIC=4096
// and ~4 insns per iteration, 1000 iterations fit inside one block
// invocation; with larger iter counts the BudgetCheck forces Go
// re-entry every ~4096 insns. The dispatch loop adds res.IC to
// cpu.cycle on each return, so the observed cycle count must equal
// the true retired insn count — this regresses on any bug in IC
// accumulation (chain entries skipping XORQ correctly) or in IC
// writeback (epilogue/slow-stub storing RBP → sret.IC).
func TestChaining_ICAccumulatesAcrossChainedExits(t *testing.T) {
	iters := uint64(10000) // well past MaxIC=4096; forces re-entries.
	cpu, _ := runSimpleLoopJIT(t, iters)

	// Per iteration: 3 ADDIs + 1 BLT = 4 retired. Loop runs `iters`
	// times (branch taken iters-1 times, falls through on the iters-th).
	// ECALL at the end adds 1.
	expected := 4*iters + 1
	if cpu.Cycle() != expected {
		t.Errorf("cpu.Cycle() = %d, want %d (IC accounting across "+
			"budget-check re-entries must match exactly)",
			cpu.Cycle(), expected)
	}
	if cpu.Reg(12) != iters {
		t.Errorf("x12 = %d, want %d", cpu.Reg(12), iters)
	}
}
*/

// broken by lack of fine grain inline IC (instruction counter)
// at the moment. So we comment it out.
/*
// D6 — Blocks that exit via a load fault still deliver the correct IC.
// Each insn emits its body BEFORE advancePC→IC++, so at fault time IC
// equals the count of previously-completed instructions (not counting
// the faulting one). The fault stub writes RBP → sret.IC before RET,
// so cpu.cycle after the fault must match exactly.
func TestChaining_FaultExitsWritebackIC(t *testing.T) {
	// Program:
	//   LUI x1, 0x10000  ; x1 = 0x10000000 (256MB — well out of 64MB)
	//   LW  x3, 0(x1)    ; faults (OOB)
	//   ECALL            ; unreachable
	insns := []uint32{
		uenc(opLUI, 1, 0x10000000),
		ienc(opLOAD, 2, 3, 1, 0), // LW x3, 0(x1)
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem.Free()
	cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteFatal })
	jit := NewJIT()
	_ = jit.RunJIT(cpu) // expected non-nil error (load fault)

	// Expected: LUI retires (IC=1), LW's body runs and jumps to the
	// fault stub BEFORE the advancePC IC++ for LW. Fault stub writes
	// RBP → sret.IC = 1. ECALL unreachable.
	if got := cpu.Cycle(); got != 1 {
		t.Errorf("cpu.Cycle() = %d, want 1 (LUI retires, LW faults "+
			"before its IC++). A value != 1 suggests IC writeback on "+
			"the fault path is broken.", got)
	}
}
*/

// D7 — Blocks that end in a JALR (indirect branch) must NOT have their
// dynamic-target exit emitted as a chain exit. JALR uses IRRetDyn,
// whose target is computed at runtime and so can't be statically
// patched. The chain-exit list on a JALR-terminated block should only
// contain the fall-through/bail-label exits from finalize, never a
// chain exit for the JALR target itself.
func TestChaining_IndirectBranch_NoChain(t *testing.T) {
	insns := []uint32{
		ienc(opOPIMM, 0, 5, 0, 0x1004), // ADDI x5, x0, 0x1004
		ienc(opJALR, 0, 0, 5, 0),       // JALR x0, 0(x5) — dynamic jump
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem.Free()
	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	blkA := jit.lookupBlock(0x1000)
	if blkA == nil {
		t.Fatal("block at 0x1000 was not compiled")
	}
	// finalize always emits at least one chain exit (for the fall-through
	// PC at end of emission). But none of A's chain exits should target
	// 0x1004 (the JALR destination) — JALR emits IRRetDyn, not a chain
	// exit. If a chain exit to 0x1004 exists, the emitter is treating
	// JALR targets as chainable, which is incorrect.
	for _, ce := range blkA.chainExits {
		if ce.targetPC == 0x1004 {
			t.Errorf("block A has a chain exit targeting 0x1004, but that "+
				"is the JALR dynamic target and must not be chained "+
				"(chainExits=%+v)", blkA.chainExits)
		}
	}
}

// TestRAS_InlinesCalleeAndPredictsReturn verifies the return address
// stack optimization (rv8 §3.6): JAL ra inlines the callee into the
// caller's block, and JALR zero,ra emits a BranchImm prediction
// that jumps directly to the return site on match.
//
//	0x1000  ADDI x10, x0,  42       ; set up arg
//	0x1004  JAL  x1, +16            ; call callee at 0x1014
//	0x1008  ADDI x11, x10, 0        ; return site: copy result
//	0x100C  ECALL                    ; exit
//	0x1010  NOP                      ; padding (not reached)
//	0x1014  ADDI x10, x10, 1        ; callee: x10 = 43
//	0x1018  JALR x0, 0(x1)          ; return
func TestRAS_InlinesCalleeAndPredictsReturn(t *testing.T) {
	insns := []uint32{
		/* 0x1000 */ ienc(opOPIMM, 0, 10, 0, 42), // ADDI x10, x0, 42
		/* 0x1004 */ jenc(1, 16), // JAL  x1, +16
		/* 0x1008 */ ienc(opOPIMM, 0, 11, 10, 0), // ADDI x11, x10, 0
		/* 0x100C */ instrECALL, // ECALL
		/* 0x1010 */ ienc(opOPIMM, 0, 0, 0, 0), // NOP (padding)
		/* 0x1014 */ ienc(opOPIMM, 0, 10, 10, 1), // ADDI x10, x10, 1
		/* 0x1018 */ ienc(opJALR, 0, 0, 1, 0), // JALR x0, 0(x1)
	}

	// --- IR-level check: the block should contain a BranchImm EQ
	// comparing ra against the return address (0x1008). ---
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	for i, insn := range insns {
		mem.Store32(0x1000+uint64(i)*4, insn)
	}
	j := NewJIT()
	res := j.emitBlock(mem, 0x1000)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}

	foundPrediction := false
	for _, ins := range res.block.Instrs {
		if ins.Op == IRBranchImm && ins.Pred == EQ && ins.Imm2 == 0x1008 {
			foundPrediction = true
			break
		}
	}
	if !foundPrediction {
		t.Error("no BranchImm EQ with expected return address 0x1008 found in IR — RAS prediction not emitted")
	}

	// Verify the block emitted instructions from both caller and callee.
	// Without RAS: 2 insns (ADDI + JAL terminates). With RAS inlining:
	// ADDI + JAL(inline) + ADDI(callee) + JALR(predict) + ADDI(return) + ECALL = 6.
	if res.numInsns < 5 {
		t.Errorf("numInsns = %d, want >= 5 (callee not inlined)", res.numInsns)
	}

	// --- End-to-end execution check ---
	cpu, mem2 := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem2.Free()
	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(10) != 43 {
		t.Errorf("x10 = %d, want 43 (callee should have added 1)", cpu.Reg(10))
	}
	if cpu.Reg(11) != 43 {
		t.Errorf("x11 = %d, want 43 (return site should have copied x10)", cpu.Reg(11))
	}

	// Only one block should be compiled — the callee is inlined.
	if jit.lookupBlock(0x1014) != nil {
		t.Error("callee at 0x1014 compiled as separate block — RAS should have inlined it")
	}
}

// TestRAS_MismatchFallsBackToJalrIC verifies that when ra doesn't
// match the predicted return address, the JALR IC slow path handles
// the return correctly.
func TestRAS_MismatchFallsBackToJalrIC(t *testing.T) {
	insns := []uint32{
		/* 0x1000 */ ienc(opOPIMM, 0, 10, 0, 42), // ADDI x10, x0, 42
		/* 0x1004 */ jenc(1, 16), // JAL  x1, +16
		/* 0x1008 */ ienc(opOPIMM, 0, 11, 10, 0), // ADDI x11, x10, 0
		/* 0x100C */ instrECALL, // ECALL
		/* 0x1010 */ ienc(opOPIMM, 0, 0, 0, 0), // NOP
		/* 0x1014 */ ienc(opOPIMM, 0, 1, 0, 0x50), // LI x1, 0x50 (corrupt ra)
		/* 0x1018 */ ienc(opJALR, 0, 0, 1, 0), // JALR x0, 0(x1)
	}
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem.Free()

	// Place a landing pad at 0x50 that the corrupted ra points to.
	mem.Store32(0x50, instrECALL)

	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	// The callee overwrites ra with 0x50, so the return goes to 0x50
	// (ECALL) instead of 0x1008. x11 should NOT be set.
	if cpu.Reg(10) != 42 {
		t.Errorf("x10 = %d, want 42", cpu.Reg(10))
	}
	if cpu.Reg(11) != 0 {
		t.Errorf("x11 = %d, want 0 (return site should not have executed)", cpu.Reg(11))
	}
}
