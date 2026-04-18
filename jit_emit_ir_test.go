//go:build !tcc

package riscv

import (
	"riscv/internal/jitcall"
	"testing"
)

func jitcallCall(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64) jitcall.Result {
	return jitcall.Call(fn, x, f, fcsr, memBase, memMask)
}

// ── scanUsedRegs unit tests ────────────────────────────────────────────

func TestScanUsedRegs_ADD(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// ADD x5, x10, x11
	mem.Store32(pc, renc(opOP, 0, 0x00, 5, 10, 11))
	mem.Store32(pc+4, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+8, &used)

	if !used[5] {
		t.Error("x5 (rd) should be used")
	}
	if !used[10] {
		t.Error("x10 (rs1) should be used")
	}
	if !used[11] {
		t.Error("x11 (rs2) should be used")
	}
	if used[0] {
		t.Error("x0 should never be marked used")
	}
	if used[1] {
		t.Error("x1 should not be used")
	}
}

func TestScanUsedRegs_ADDI(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// ADDI x1, x0, 42
	mem.Store32(pc, ienc(opOPIMM, 0, 1, 0, 42))
	mem.Store32(pc+4, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+8, &used)

	if !used[1] {
		t.Error("x1 (rd) should be used")
	}
	// rs1=x0, should not mark x0
	if used[0] {
		t.Error("x0 should not be marked")
	}
}

func TestScanUsedRegs_Branch(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// BEQ x3, x4, +8
	mem.Store32(pc, benc(opBRANCH, 0, 3, 4, 8))
	mem.Store32(pc+4, instrECALL)
	mem.Store32(pc+8, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+12, &used)

	if !used[3] {
		t.Error("x3 (rs1) should be used in branch")
	}
	if !used[4] {
		t.Error("x4 (rs2) should be used in branch")
	}
}

func TestScanUsedRegs_Store(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// SD x7, 0(x8)
	mem.Store32(pc, senc(opSTORE, 3, 8, 7, 0))
	mem.Store32(pc+4, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+8, &used)

	if !used[8] {
		t.Error("x8 (rs1/base) should be used")
	}
	if !used[7] {
		t.Error("x7 (rs2/src) should be used")
	}
}

func TestScanUsedRegs_FibLoop(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	insns := []uint32{
		renc(0x33, 0, 0x00, 5, 10, 11),  // ADD x5, x10, x11
		ienc(opOPIMM, 0, 10, 11, 0),     // MV x10, x11
		ienc(opOPIMM, 0, 11, 5, 0),      // MV x11, x5
		ienc(opOPIMM, 0, 12, 12, 1),     // ADDI x12, x12, 1
		benc(opBRANCH, 4, 12, 13, -16),  // BLT x12, x13, -16
		instrECALL,
	}
	for i, insn := range insns {
		mem.Store32(pc+uint64(i)*4, insn)
	}

	var used [32]bool
	scanUsedRegs(mem, pc, pc+uint64(len(insns))*4, &used)

	for _, r := range []uint32{5, 10, 11, 12, 13} {
		if !used[r] {
			t.Errorf("x%d should be used in fib loop", r)
		}
	}
	// Verify no spurious registers
	for i := uint32(1); i < 32; i++ {
		switch i {
		case 5, 10, 11, 12, 13:
			continue
		default:
			if used[i] {
				t.Errorf("x%d should NOT be used in fib loop", i)
			}
		}
	}
}

func TestScanUsedRegs_NoX0(t *testing.T) {
	// x0 should never be in the used set regardless of encoding.
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// ADD x0, x1, x2 (rd=0 means discard, but rs1/rs2 are used)
	mem.Store32(pc, renc(0x33, 0, 0x00, 0, 1, 2))
	mem.Store32(pc+4, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+8, &used)

	if used[0] {
		t.Error("x0 should never be marked used")
	}
	if !used[1] {
		t.Error("x1 should be used")
	}
	if !used[2] {
		t.Error("x2 should be used")
	}
}

func TestScanUsedRegs_LUI_AUIPC(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// LUI x3, 0x12345  (only rd, no rs1/rs2)
	mem.Store32(pc, 0x12345000|3<<7|0x37) // LUI rd=3, imm=0x12345
	// AUIPC x4, 0x1000 (only rd, no rs1/rs2)
	mem.Store32(pc+4, 0x01000000|4<<7|0x17) // AUIPC rd=4, imm=0x1000
	mem.Store32(pc+8, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+12, &used)

	if !used[3] {
		t.Error("x3 (rd of LUI) should be used")
	}
	if !used[4] {
		t.Error("x4 (rd of AUIPC) should be used")
	}
}

// TestScanUsedRegs_MixedBlock2 reproduces the second block from TestJIT_MixedExecution.
func TestScanUsedRegs_MixedBlock2(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x100C)
	mem.Store32(0x100C, ienc(opOPIMM, 0, 4, 0, 30)) // ADDI x4, x0, 30
	mem.Store32(0x1010, renc(opOP, 0, 0x00, 5, 1, 2)) // ADD x5, x1, x2
	mem.Store32(0x1014, instrECALL)

	var used [32]bool
	scanUsedRegs(mem, pc, pc+12, &used)

	// Must load x1 and x2 (read by ADD), x4 and x5 (written)
	for _, r := range []uint32{1, 2, 4, 5} {
		if !used[r] {
			t.Errorf("x%d should be used", r)
		}
	}
}

// TestMixedExecution_Block2_Compile tests that the second block from
// TestJIT_MixedExecution compiles and runs without crashing.
func TestMixedExecution_Block2_Compile(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	mem.Store32(0x100C, ienc(opOPIMM, 0, 4, 0, 30)) // ADDI x4, x0, 30
	mem.Store32(0x1010, renc(opOP, 0, 0x00, 5, 1, 2)) // ADD x5, x1, x2
	mem.Store32(0x1014, instrECALL)

	res := emitBlock(mem, 0x100C)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	if res.block == nil {
		t.Fatal("block is nil")
	}
	t.Logf("block has %d IR instructions", len(res.block.Instrs))

	// Compile it
	compiled, err := jitCompile(res)
	if err != nil {
		t.Fatalf("jitCompile: %v", err)
	}
	if compiled == nil {
		t.Fatal("compiled block is nil")
	}

	// Execute it
	cpu := NewCPU(*mem)
	cpu.SetPC(0x100C)
	cpu.SetReg(1, 10)
	cpu.SetReg(2, 20)

	result := jitcallCall(compiled.fn, &cpu.x, &cpu.f, &cpu.fcsr,
		cpu.mem.Base(), cpu.mem.Mask())
	t.Logf("result: PC=0x%x IC=%d Status=%d", result.PC, result.IC, result.Status)

	if cpu.Reg(4) != 30 {
		t.Errorf("x4 = %d, want 30", cpu.Reg(4))
	}
	if cpu.Reg(5) != 30 {
		t.Errorf("x5 = %d, want 30", cpu.Reg(5))
	}
}

// TestMixedExecution_Block1_Dump compiles block 1 and dumps the native bytes.
func TestMixedExecution_Block1_Dump(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00)
	mem.Store32(0x1000, ienc(opOPIMM, 0, 1, 0, 10))
	mem.Store32(0x1004, ienc(opOPIMM, 0, 2, 0, 20))
	mem.Store32(0x1008, csrrs)

	res := emitBlock(mem, 0x1000)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	t.Logf("IR instructions: %d", len(res.block.Instrs))
	for i, ins := range res.block.Instrs {
		t.Logf("  [%d] %s", i, ins.String())
	}

	compiled, err := jitCompile(res)
	if err != nil {
		t.Fatalf("jitCompile: %v", err)
	}
	t.Logf("compiled block fn=%x, backing=%d bytes", compiled.fn, len(compiled.backing))

	// Run it
	cpu := NewCPU(*mem)
	cpu.SetPC(0x1000)
	result := jitcallCall(compiled.fn, &cpu.x, &cpu.f, &cpu.fcsr,
		cpu.mem.Base(), cpu.mem.Mask())
	t.Logf("result: PC=0x%x IC=%d Status=%d x1=%d x2=%d",
		result.PC, result.IC, result.Status, cpu.Reg(1), cpu.Reg(2))
}

// TestMixedExecution_FullSequence reproduces the full TestJIT_MixedExecution
// flow to isolate the segfault.
func TestMixedExecution_FullSequence(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00)
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 10),       // ADDI x1, x0, 10
		ienc(opOPIMM, 0, 2, 0, 20),       // ADDI x2, x0, 20
		csrrs,                              // CSRRS — terminates block
		ienc(opOPIMM, 0, 4, 0, 30),       // ADDI x4, x0, 30
		renc(opOP, 0, 0x00, 5, 1, 2),     // ADD x5, x1, x2
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()

	// Step 1: compile and run block 1 (ADDI x1; ADDI x2; bail at CSR)
	t.Log("Step 1: first block")
	ic1, err1 := jit.StepBlock(cpu)
	t.Logf("  after block1: pc=0x%x ic=%d err=%v x1=%d x2=%d",
		cpu.PC(), ic1, err1, cpu.Reg(1), cpu.Reg(2))

	// Step 2: interpreter handles CSR
	t.Log("Step 2: interpreter step (CSR)")
	ic2, err2 := jit.StepBlock(cpu)
	t.Logf("  after CSR: pc=0x%x ic=%d err=%v x3=%d",
		cpu.PC(), ic2, err2, cpu.Reg(3))

	// Step 3: compile and run block 2 (ADDI x4; ADD x5; ECALL)
	t.Log("Step 3: second block")
	ic3, err3 := jit.StepBlock(cpu)
	t.Logf("  after block2: pc=0x%x ic=%d err=%v x4=%d x5=%d",
		cpu.PC(), ic3, err3, cpu.Reg(4), cpu.Reg(5))
}

// Encoding helpers are in jit_test.go (senc, ienc, renc, benc).
