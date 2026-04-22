package riscv

import (
	"fmt"
	"os"
	//"path/filepath"
	"riscv/internal/jitcall"
	"riscv/ir"
	"strings"
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
		renc(0x33, 0, 0x00, 5, 10, 11), // ADD x5, x10, x11
		ienc(opOPIMM, 0, 10, 11, 0),    // MV x10, x11
		ienc(opOPIMM, 0, 11, 5, 0),     // MV x11, x5
		ienc(opOPIMM, 0, 12, 12, 1),    // ADDI x12, x12, 1
		benc(opBRANCH, 4, 12, 13, -16), // BLT x12, x13, -16
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
	mem.Store32(0x100C, ienc(opOPIMM, 0, 4, 0, 30))   // ADDI x4, x0, 30
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

	mem.Store32(0x100C, ienc(opOPIMM, 0, 4, 0, 30))   // ADDI x4, x0, 30
	mem.Store32(0x1010, renc(opOP, 0, 0x00, 5, 1, 2)) // ADD x5, x1, x2
	mem.Store32(0x1014, instrECALL)

	res := emitBlock(mem, 0x100C)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	if res.block == nil {
		t.Fatal("block is nil")
	}
	//t.Logf("block has %d IR instructions", len(res.block.Instrs))

	// Compile it
	j := NewJIT()
	compiled, err := j.jitCompile(res)
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
	_ = result
	//t.Logf("result: PC=0x%x IC=%d Status=%d", result.PC, result.IC, result.Status)

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
	//t.Logf("IR instructions: %d", len(res.block.Instrs))
	for i, ins := range res.block.Instrs {
		_, _ = i, ins
		//t.Logf("  [%d] %s", i, ins.String())
	}

	j := NewJIT()
	compiled, err := j.jitCompile(res)
	if err != nil {
		t.Fatalf("jitCompile: %v", err)
	}
	//t.Logf("compiled block fn=%x", compiled.fn)

	// Run it
	cpu := NewCPU(*mem)
	cpu.SetPC(0x1000)
	result := jitcallCall(compiled.fn, &cpu.x, &cpu.f, &cpu.fcsr,
		cpu.mem.Base(), cpu.mem.Mask())
	_ = result
	//t.Logf("result: PC=0x%x IC=%d Status=%d x1=%d x2=%d", result.PC, result.IC, result.Status, cpu.Reg(1), cpu.Reg(2))
}

// TestMixedExecution_FullSequence reproduces the full TestJIT_MixedExecution
// flow to isolate the segfault.
func TestMixedExecution_FullSequence(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00)
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 10),   // ADDI x1, x0, 10
		ienc(opOPIMM, 0, 2, 0, 20),   // ADDI x2, x0, 20
		csrrs,                        // CSRRS — terminates block
		ienc(opOPIMM, 0, 4, 0, 30),   // ADDI x4, x0, 30
		renc(opOP, 0, 0x00, 5, 1, 2), // ADD x5, x1, x2
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()

	// Step 1: compile and run block 1 (ADDI x1; ADDI x2; bail at CSR)
	//t.Log("Step 1: first block")
	ic1, err1 := jit.StepBlock(cpu)
	_, _ = ic1, err1
	//t.Logf("  after block1: pc=0x%x ic=%d err=%v x1=%d x2=%d", cpu.PC(), ic1, err1, cpu.Reg(1), cpu.Reg(2))

	// Step 2: interpreter handles CSR
	//t.Log("Step 2: interpreter step (CSR)")
	ic2, err2 := jit.StepBlock(cpu)
	_, _ = ic2, err2
	//t.Logf("  after CSR: pc=0x%x ic=%d err=%v x3=%d", cpu.PC(), ic2, err2, cpu.Reg(3))

	// Step 3: compile and run block 2 (ADDI x4; ADD x5; ECALL)
	//t.Log("Step 3: second block")
	ic3, err3 := jit.StepBlock(cpu)
	_, _ = ic3, err3
	//t.Logf("  after block2: pc=0x%x ic=%d err=%v x4=%d x5=%d", cpu.PC(), ic3, err3, cpu.Reg(4), cpu.Reg(5))
}

// TestSrc1EqDest tests SUB x1, x1, x2 (rd==rs1 aliasing).
func TestSrc1EqDest(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 13),   // ADDI x1, x0, 13
		ienc(opOPIMM, 0, 2, 0, 11),   // ADDI x2, x0, 11
		renc(opOP, 0, 0x20, 1, 1, 2), // SUB x1, x1, x2  (rd==rs1)
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 2 {
		t.Errorf("x1 = %d, want 2 (13 - 11)", cpu.Reg(1))
	}
}

// TestSubELF_Block39 runs the sub ELF to block 39 and checks what happens.
func TestSubELF_Block39(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	entry, err := LoadELF(mem, "riscv-elf-tests/rv64ui-p-sub")
	if err != nil {
		t.Fatal(err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	// Run blocks 0-38
	for block := 0; block < 39; block++ {
		pc := cpu.PC()
		ic, err := jit.StepBlock(cpu)
		_, _ = pc, ic
		if err != nil {
			//t.Logf("block %d: pc=0x%x ic=%d err=%v gp=%d", block, pc, ic, err, cpu.Reg(3))
			break
		}
	}

	// Now at block 39
	//t.Logf("block 39 starts at PC=0x%x, gp=%d", cpu.PC(), cpu.Reg(3))

	// Dump next instructions
	pc := cpu.PC()
	for i := 0; i < 20; i++ {
		half, _ := mem.Fetch16(pc)
		if half&3 != 3 {
			//t.Logf("  0x%04x: %04x (RVC)", pc, half)
			pc += 2
		} else {
			insn, _ := mem.Fetch32(pc)
			_ = insn
			//t.Logf("  0x%04x: %08x", pc, insn)
			pc += 4
		}
	}

	// Run block 39 with JIT
	pc39 := cpu.PC()
	res := emitBlock(mem, pc39)
	if res == nil {
		t.Fatal("emitBlock returned nil for block 39")
	}
	//t.Logf("block 39 IR: %d instructions", len(res.block.Instrs))
	//for i, ins := range res.block.Instrs {
	//t.Logf("  [%d] %s", i, ins.String())
	//}
}

// TestCLI_NoCorruption verifies C.LI x7, 3 doesn't corrupt other registers.
func TestCLI_NoCorruption(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	// C.LI x7, 3
	mem.Store16(0x1000, 0x438d)
	// ECALL
	mem.Store32(0x1002, instrECALL)

	cpu := NewCPU(*mem)
	cpu.SetPC(0x1000)
	// Set x12 to a known value
	cpu.SetReg(12, 999)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(7) != 3 {
		t.Errorf("x7 = %d, want 3", cpu.Reg(7))
	}
	if cpu.Reg(12) != 999 {
		t.Errorf("x12 = %d, want 999 (should be untouched)", cpu.Reg(12))
	}
	// Check all other registers are 0 (except x7)
	for i := 1; i < 32; i++ {
		if i == 7 || i == 12 {
			continue
		}
		if cpu.Reg(uint8(i)) != 0 {
			t.Errorf("x%d = %d, want 0 (should be untouched)", i, cpu.Reg(uint8(i)))
		}
	}
}

// TestSLLW_ShiftZero tests SLLW with shift amount 0 (via x12=-32, masked to 0).
func TestSLLW_ShiftZero(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		renc(0x3B, 1, 0x00, 14, 11, 12), // SLLW x14, x11, x12
		instrECALL,
	})
	defer mem.Free()
	cpu.SetReg(11, 0x21212121)
	cpu.SetReg(12, ^uint64(31)) // 0xFFFFFFFFFFFFFFE0 = -32, & 31 = 0
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	want := uint64(0x21212121)
	if cpu.Reg(14) != want {
		t.Errorf("SLLW: x14 = 0x%x, want 0x%x", cpu.Reg(14), want)
	}
}

// TestSLL_Src1EqDest tests SLL x12, x11, x12 (rd==rs2 aliasing with shift).
func TestSLL_Src1EqDest(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		renc(opOP, 1, 0x00, 12, 11, 12), // SLL x12, x11, x12
		instrECALL,
	})
	defer mem.Free()
	cpu.SetReg(11, 2)
	cpu.SetReg(12, 13)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	want := uint64(2 << 13) // 16384
	if cpu.Reg(12) != want {
		t.Errorf("SLL: x12 = 0x%x, want 0x%x", cpu.Reg(12), want)
	}
}

// TestSRL_Src1EqDest tests SRL x1, x1, x2 (rd==rs1 aliasing with shift).
func TestSRL_Src1EqDest(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		renc(opOP, 5, 0x00, 1, 1, 2), // SRL x1, x1, x2
		instrECALL,
	})
	defer mem.Free()
	cpu.SetReg(1, 0x80000000)
	cpu.SetReg(2, 7)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	want := uint64(0x80000000 >> 7)
	if cpu.Reg(1) != want {
		t.Errorf("SRL: x1 = 0x%x, want 0x%x", cpu.Reg(1), want)
	}
}

// TestLW_ELF_Block39 traces the lw ELF test around the divergence.
func TestLW_ELF_Block39(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	entry, err := LoadELF(mem, "riscv-elf-tests/rv64ui-p-lw")
	if err != nil {
		t.Fatal(err)
	}

	// Run with JIT, tracing enabled
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	// jit.trace = true // uncomment to debug

	for block := 0; block < 50; block++ {
		pc := cpu.PC()
		ic, err := jit.StepBlock(cpu)
		_, _ = ic, pc
		if err != nil {
			//t.Logf("block %d: pc=0x%x exit err=%v gp=%d", block, pc, err, cpu.Reg(3))
			break
		}
	}
	//t.Logf("final: pc=0x%x gp=%d x10=%d", cpu.PC(), cpu.Reg(3), cpu.Reg(10))
}

// TestSRL_ZeroSrc tests SRL x2, x0, x1 (shifting zero).
func TestSRL_ZeroSrc(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		renc(opOP, 5, 0x00, 2, 0, 1), // SRL x2, x0, x1
		instrECALL,
	})
	defer mem.Free()
	cpu.SetReg(1, 31)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(2) != 0 {
		t.Errorf("SRL x2, x0, x1: x2 = 0x%x, want 0 (0 >> 31 = 0)", cpu.Reg(2))
	}
}

// Encoding helpers are in jit_test.go (senc, ienc, renc, benc).

// TestLUI_SRLI_TwoInsn verifies LUI + SRLI in a 2-instruction block.
// Regression: MarkDirty was missing, causing writeback to skip modified regs.
func TestLUI_SRLI_TwoInsn(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	pc := uint64(0x1000)
	// LUI x7, 0x80000 => x7 = 0xFFFFFFFF80000000
	mem.Store32(pc, 0x800003B7) // LUI x7, 0x80000
	// SRLI x7, x7, 7 => x7 = 0x01FFFFFFFF000000
	mem.Store32(pc+4, 0x0073D393) // SRLI x7, x7, 7
	// ECALL
	mem.Store32(pc+8, 0x00000073)

	cpu := NewCPU(*mem)
	cpu.SetPC(pc)

	jit := NewJIT()
	_, jitErr := jit.StepBlock(cpu)
	_ = jitErr

	want := uint64(0x01FFFFFFFF000000)
	got := cpu.x[7]
	//t.Logf("x7 = 0x%x (want 0x%x), jitErr=%v, pc=0x%x", got, want, jitErr, cpu.pc)
	if got != want {
		t.Fatalf("x7 = 0x%x, want 0x%x", got, want)
	}
}

// TestSRL_LargeValue_Block verifies SRL with 0xFFFFFFFF80000000 >> 0 in a block.
func TestSRL_LargeValue_Block(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	pc := uint64(0x1000)
	// C.LI x11, -1  (0x55fd = C.LI x11, -1? Let's use full instructions)
	// LUI x11, 0x80000  (x11 = 0xFFFFFFFF80000000)
	mem.Store32(pc, 0x800005B7) // LUI x11, 0x80000
	// C.LI x12, 0  (x12 = 0)
	mem.Store32(pc+4, 0x00000613) // ADDI x12, x0, 0
	// SRL x14, x11, x12
	mem.Store32(pc+8, 0x00C5D733) // SRL x14, x11, x12
	// ECALL
	mem.Store32(pc+12, 0x00000073)

	cpu := NewCPU(*mem)
	cpu.SetPC(pc)

	jit := NewJIT()
	_, _ = jit.StepBlock(cpu)

	want := uint64(0xFFFFFFFF80000000)
	got := cpu.x[14]
	//t.Logf("x[14]=0x%x (want 0x%x), x[11]=0x%x, x[12]=0x%x", got, want, cpu.x[11], cpu.x[12])
	if got != want {
		t.Fatalf("x[14]=0x%x, want 0x%x", got, want)
	}
}

// TestSRL_CrossBlock_Writeback verifies that x[11] written in block 1
// is correctly read in block 2.
func TestSRL_CrossBlock_Writeback(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	pc := uint64(0x1000)
	// Block 1: LUI x11, 0x80000 (sets x11 = 0xFFFFFFFF80000000)
	mem.Store32(pc, 0x800005B7) // LUI x11, 0x80000
	// ECALL to end block 1
	mem.Store32(pc+4, 0x00000073) // ECALL

	// Block 2 starts at pc+8:
	// ADDI x12, x0, 0 (x12 = 0)
	mem.Store32(pc+8, 0x00000613)
	// SRL x14, x11, x12 (x14 = x11 >> 0 = x11)
	mem.Store32(pc+12, 0x00C5D733)
	// ECALL
	mem.Store32(pc+16, 0x00000073)

	cpu := NewCPU(*mem)
	cpu.SetPC(pc)

	jit := NewJIT()

	// Block 1
	_, err1 := jit.StepBlock(cpu)
	_ = err1
	//t.Logf("after block 1: x[11]=0x%x, pc=0x%x, err=%v", cpu.x[11], cpu.pc, err1)
	if cpu.x[11] != 0xFFFFFFFF80000000 {
		t.Fatalf("block 1: x[11]=0x%x, want 0xFFFFFFFF80000000", cpu.x[11])
	}
	// Advance past ECALL
	cpu.SetPC(pc + 8)

	// Block 2
	_, err2 := jit.StepBlock(cpu)
	_ = err2
	//t.Logf("after block 2: x[14]=0x%x, x[11]=0x%x, x[12]=0x%x, pc=0x%x, err=%v", cpu.x[14], cpu.x[11], cpu.x[12], cpu.pc, err2)

	want := uint64(0xFFFFFFFF80000000)
	if cpu.x[14] != want {
		t.Fatalf("block 2: x[14]=0x%x, want 0x%x", cpu.x[14], want)
	}
}

// TestSRL_ExactIR reproduces the exact IR from the failing srl ELF block.
func TestSRL_ExactIR(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	// Manually build the exact IR block from the dump:
	e := ir.NewEmitter()
	x7 := ir.VReg(7)
	x11 := ir.VReg(11)
	x12 := ir.VReg(12)
	x14 := ir.VReg(14)

	// Prepended loads
	e.Load(x7, e.XBase(), 56, ir.I64, false)
	e.Load(x11, e.XBase(), 88, ir.I64, false)
	e.Load(x12, e.XBase(), 96, ir.I64, false)
	e.Load(x14, e.XBase(), 112, ir.I64, false)

	// SRL: shr x14 = x11, x12
	e.Shr(x14, x11, x12)

	// IC increment
	e.AddImm(e.IC(), e.IC(), 1)

	// Const x7 = -2147483648
	e.Const(x7, -2147483648)

	// IC increment
	e.AddImm(e.IC(), e.IC(), 1)

	// IC increment
	e.AddImm(e.IC(), e.IC(), 1)

	// Branch NE x14, x7 -> taken (to fail exit)
	failLabel := e.NewLabel()
	e.Branch(x14, x7, ir.NE, failLabel)

	// Fall-through: writeback + ret (pass)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	passPC := uint64(0x14c)
	e.Ret(passPC, 0, ir.VRegZero)

	// Taken: writeback + ret (fail)
	e.PlaceLabel(failLabel)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	failPC := uint64(0x592)
	e.Ret(failPC, 0, ir.VRegZero)

	// Compile and execute
	blk := e.Block
	j := NewJIT()
	compiled, cerr := j.jitCompile(&emitResult{block: blk, numInsns: 3})
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}

	var x [32]uint64
	var f [32]uint64
	var fcsr uint32
	x[11] = 0xFFFFFFFF80000000
	x[12] = 0

	res := jitcallCall(compiled.fn, &x, &f, &fcsr, mem.Base(), mem.Mask())
	//t.Logf("PC=0x%x IC=%d Status=%d x[14]=0x%x x[7]=0x%x", res.PC, res.IC, res.Status, x[14], x[7])

	if x[14] != 0xFFFFFFFF80000000 {
		t.Fatalf("x[14]=0x%x, want 0xFFFFFFFF80000000", x[14])
	}
	if res.PC != uint64(passPC) {
		t.Fatalf("PC=0x%x, want 0x%x (should have branched to pass, not fail)", res.PC, passPC)
	}
}

func TestSRL_ExactIR_V2(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	e := ir.NewEmitter()
	x7 := ir.VReg(7)
	x11 := ir.VReg(11)
	x12 := ir.VReg(12)
	x14 := ir.VReg(14)

	e.Load(x7, e.XBase(), 56, ir.I64, false)
	e.Load(x11, e.XBase(), 88, ir.I64, false)
	e.Load(x12, e.XBase(), 96, ir.I64, false)
	e.Load(x14, e.XBase(), 112, ir.I64, false)
	e.Shr(x14, x11, x12)
	e.AddImm(e.IC(), e.IC(), 1)
	e.Const(x7, -2147483648)
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(e.IC(), e.IC(), 1)
	failLabel := e.NewLabel()
	e.Branch(x14, x7, ir.NE, failLabel)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(0x14c, 0, ir.VRegZero)
	e.PlaceLabel(failLabel)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(0x592, 0, ir.VRegZero)

	blk := e.Block
	j := NewJIT()
	compiled, cerr := j.jitCompileV2(&emitResult{block: blk, numInsns: 3})
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}

	var x [32]uint64
	var f [32]uint64
	var fcsr uint32
	x[11] = 0xFFFFFFFF80000000

	res := jitcallCall(compiled.fn, &x, &f, &fcsr, mem.Base(), mem.Mask())
	_ = res
	//t.Logf("V2: PC=0x%x IC=%d x[14]=0x%x x[7]=0x%x", res.PC, res.IC, x[14], x[7])

	if x[14] != 0xFFFFFFFF80000000 {
		t.Fatalf("V2: x[14]=0x%x, want 0xFFFFFFFF80000000", x[14])
	}
}

func TestSRL_ExactIR_DumpAlloc(t *testing.T) {
	e := ir.NewEmitter()
	x7 := ir.VReg(7)
	x11 := ir.VReg(11)
	x12 := ir.VReg(12)
	x14 := ir.VReg(14)

	e.Load(x7, e.XBase(), 56, ir.I64, false)
	e.Load(x11, e.XBase(), 88, ir.I64, false)
	e.Load(x12, e.XBase(), 96, ir.I64, false)
	e.Load(x14, e.XBase(), 112, ir.I64, false)
	e.Shr(x14, x11, x12)
	e.AddImm(e.IC(), e.IC(), 1)
	e.Const(x7, -2147483648)
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(e.IC(), e.IC(), 1)
	failLabel := e.NewLabel()
	e.Branch(x14, x7, ir.NE, failLabel)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(0x14c, 0, ir.VRegZero)
	e.PlaceLabel(failLabel)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(0x592, 0, ir.VRegZero)

	blk := e.Block
	pool := ir.AMD64Pool(blk)
	pinned := ir.AMD64Pinned()
	j := NewJIT()
	alloc := j.irAlloc.Allocate(blk, pool, pinned, nil)

	//t.Logf("StackSlots=%d", alloc.StackSlots)
	for i, k := range alloc.Kind {
		_ = i
		if k != ir.AllocUnused {
			//t.Logf("VReg(%d): kind=%v spill=%d", i, k, alloc.SpillSlot[i])
		}
	}
	for _, ia := range alloc.IntervalMap {
		_ = ia
		//t.Logf("  interval: VReg(%d) [%d..%d] -> host=%d", ia.Interval.VReg, ia.Interval.Start, ia.Interval.End, ia.Host)
	}
}

// TestSRL_Block61_V1vV2 reproduces the exact IR from the failing srl ELF block 61
// and compares V1 vs V2 results.
func TestSRL_Block61_V1vV2(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	e := ir.NewEmitter()
	x1 := ir.VReg(1)
	x2 := ir.VReg(2)
	x3 := ir.VReg(3)
	x4 := ir.VReg(4)
	x5 := ir.VReg(5)
	x6 := ir.VReg(6)
	x7 := ir.VReg(7)
	x14 := ir.VReg(14)

	// Prepended loads
	e.Load(x1, e.XBase(), 8, ir.I64, false)
	e.Load(x2, e.XBase(), 16, ir.I64, false)
	e.Load(x3, e.XBase(), 24, ir.I64, false)
	e.Load(x4, e.XBase(), 32, ir.I64, false)
	e.Load(x5, e.XBase(), 40, ir.I64, false)
	e.Load(x6, e.XBase(), 48, ir.I64, false)
	e.Load(x7, e.XBase(), 56, ir.I64, false)
	e.Load(x14, e.XBase(), 112, ir.I64, false)

	// SRL x14 = x1, x2
	e.Shr(x14, x1, x2)
	e.AddImm(e.IC(), e.IC(), 1)
	// MOV x6 = x14
	e.Mov(x6, x14)
	e.AddImm(e.IC(), e.IC(), 1)
	// ADDI x4 = x4, 1
	e.AddImm(x4, x4, 1)
	e.AddImm(e.IC(), e.IC(), 1)
	// CONST x5 = 2
	e.Const(x5, 2)
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(e.IC(), e.IC(), 1)
	// BNE x4, x5 -> L7 (test count exit)
	l7 := e.NewLabel()
	e.Branch(x4, x5, ir.NE, l7)
	// CONST x7 = 16777216 (0x1000000)
	e.Const(x7, 16777216)
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(e.IC(), e.IC(), 1)
	// BNE x6, x7 -> L10 (test fail)
	l10 := e.NewLabel()
	e.Branch(x6, x7, ir.NE, l10)
	// Pass: const x3 = 26
	e.Const(x3, 26)
	e.AddImm(e.IC(), e.IC(), 1)
	e.Const(x4, 0)
	e.AddImm(e.IC(), e.IC(), 1)
	// WriteBackAll + Ret (pass → pc=886)
	e.Store(e.XBase(), 24, x3, ir.I64)
	e.Store(e.XBase(), 32, x4, ir.I64)
	e.Store(e.XBase(), 40, x5, ir.I64)
	e.Store(e.XBase(), 48, x6, ir.I64)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(886, 0, ir.VRegZero)
	// L10: fail
	e.PlaceLabel(l10)
	e.Store(e.XBase(), 24, x3, ir.I64)
	e.Store(e.XBase(), 32, x4, ir.I64)
	e.Store(e.XBase(), 40, x5, ir.I64)
	e.Store(e.XBase(), 48, x6, ir.I64)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(1426, 0, ir.VRegZero)
	// L7: count mismatch exit
	e.PlaceLabel(l7)
	e.Store(e.XBase(), 24, x3, ir.I64)
	e.Store(e.XBase(), 32, x4, ir.I64)
	e.Store(e.XBase(), 40, x5, ir.I64)
	e.Store(e.XBase(), 48, x6, ir.I64)
	e.Store(e.XBase(), 56, x7, ir.I64)
	e.Store(e.XBase(), 112, x14, ir.I64)
	e.Ret(854, 0, ir.VRegZero)

	blk := e.Block

	// Input: x[1]=0x80000000, x[2]=7, x[3]=25, x[4]=0, x[12]=0x20000
	var x1v, x2v [32]uint64
	var f1v, f2v [32]uint64
	var fcsr1, fcsr2 uint32
	x1v[1] = 0x80000000
	x1v[2] = 7
	x1v[3] = 25
	x1v[4] = 0
	x2v = x1v

	// V1
	j := NewJIT()
	c1, err := j.jitCompile(&emitResult{block: blk, numInsns: 9})
	if err != nil {
		t.Fatalf("V1 compile: %v", err)
	}
	r1 := jitcallCall(c1.fn, &x1v, &f1v, &fcsr1, mem.Base(), mem.Mask())

	// V2
	c2, err := j.jitCompileV2(&emitResult{block: blk, numInsns: 9})
	if err != nil {
		t.Fatalf("V2 compile: %v", err)
	}
	r2 := jitcallCall(c2.fn, &x2v, &f2v, &fcsr2, mem.Base(), mem.Mask())

	//t.Logf("V1: PC=0x%x IC=%d x[3]=%d x[6]=0x%x x[14]=0x%x", r1.PC, r1.IC, x1v[3], x1v[6], x1v[14])
	//t.Logf("V2: PC=0x%x IC=%d x[3]=%d x[6]=0x%x x[14]=0x%x", r2.PC, r2.IC, x2v[3], x2v[6], x2v[14])

	if x1v[6] != x2v[6] {
		t.Errorf("x[6] V1=0x%x V2=0x%x", x1v[6], x2v[6])
	}
	if x1v[14] != x2v[14] {
		t.Errorf("x[14] V1=0x%x V2=0x%x", x1v[14], x2v[14])
	}
	if r1.PC != r2.PC {
		t.Errorf("PC V1=0x%x V2=0x%x", r1.PC, r2.PC)
	}

	// Expected: SRL(0x80000000, 7) = 0x01000000
	want := uint64(0x01000000)
	if x2v[14] != want {
		t.Errorf("V2 x[14]=0x%x want 0x%x", x2v[14], want)
	}
}

// TestSRL_Block61_V1vV2b reproduces the exact IR from the failing srl block 61
// with writeback helper.
func TestSRL_Block61_V1vV2b(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	e := ir.NewEmitter()
	x1, x2, x3, x4 := ir.VReg(1), ir.VReg(2), ir.VReg(3), ir.VReg(4)
	x5, x6, x7, x14 := ir.VReg(5), ir.VReg(6), ir.VReg(7), ir.VReg(14)

	// Prepended loads (8 guest regs)
	for _, vr := range []ir.VReg{x1, x2, x3, x4, x5, x6, x7, x14} {
		e.Load(vr, e.XBase(), int64(vr)*8, ir.I64, false)
	}

	e.Shr(x14, x1, x2) // SRL x14 = x1, x2
	e.AddImm(e.IC(), e.IC(), 1)
	e.Mov(x6, x14) // MOV x6 = x14
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(x4, x4, 1) // ADDI x4 = x4 + 1
	e.AddImm(e.IC(), e.IC(), 1)
	e.Const(x5, 2) // CONST x5 = 2
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(e.IC(), e.IC(), 1)

	l7 := e.NewLabel()
	e.Branch(x4, x5, ir.NE, l7) // BNE x4, x5 → L7

	e.Const(x7, 0x1000000) // CONST x7 = 16777216
	e.AddImm(e.IC(), e.IC(), 1)
	e.AddImm(e.IC(), e.IC(), 1)

	l10 := e.NewLabel()
	e.Branch(x6, x7, ir.NE, l10) // BNE x6, x7 → L10

	// Pass exit
	e.Const(x3, 26)
	e.AddImm(e.IC(), e.IC(), 1)
	e.Const(x4, 0)
	e.AddImm(e.IC(), e.IC(), 1)
	wb := func() {
		for _, vr := range []ir.VReg{x3, x4, x5, x6, x7, x14} {
			e.Store(e.XBase(), int64(vr)*8, vr, ir.I64)
		}
	}
	wb()
	e.Ret(886, 0, ir.VRegZero)

	// L10: fail
	e.PlaceLabel(l10)
	wb()
	e.Ret(1426, 0, ir.VRegZero)

	// L7: count exit
	e.PlaceLabel(l7)
	wb()
	e.Ret(854, 0, ir.VRegZero)

	blk := e.Block

	// Input: x[1]=0x80000000, x[2]=7, x[3]=25, x[4]=0
	setup := func(x *[32]uint64) {
		x[1] = 0x80000000
		x[2] = 7
		x[3] = 25
		x[4] = 0
	}

	// Expected: SRL(0x80000000, 7) = 0x01000000, test passes → PC=886
	var xv1, xv2 [32]uint64
	var fv1, fv2 [32]uint64
	var fc1, fc2 uint32
	setup(&xv1)
	setup(&xv2)

	j := NewJIT()
	c1, err := j.jitCompile(&emitResult{block: blk, numInsns: 9})
	if err != nil {
		t.Fatalf("V1 compile: %v", err)
	}
	r1 := jitcallCall(c1.fn, &xv1, &fv1, &fc1, mem.Base(), mem.Mask())

	c2, err := j.jitCompileV2(&emitResult{block: blk, numInsns: 9})
	if err != nil {
		t.Fatalf("V2 compile: %v", err)
	}
	r2 := jitcallCall(c2.fn, &xv2, &fv2, &fc2, mem.Base(), mem.Mask())

	//t.Logf("V1: PC=0x%x IC=%d x[3]=%d x[6]=0x%x x[14]=0x%x", r1.PC, r1.IC, xv1[3], xv1[6], xv1[14])
	//t.Logf("V2: PC=0x%x IC=%d x[3]=%d x[6]=0x%x x[14]=0x%x", r2.PC, r2.IC, xv2[3], xv2[6], xv2[14])

	if r1.PC != r2.PC {
		t.Errorf("PC mismatch: V1=0x%x V2=0x%x", r1.PC, r2.PC)
	}
	if xv1[14] != xv2[14] {
		t.Errorf("x[14] mismatch: V1=0x%x V2=0x%x", xv1[14], xv2[14])
	}
	if xv1[6] != xv2[6] {
		t.Errorf("x[6] mismatch: V1=0x%x V2=0x%x", xv1[6], xv2[6])
	}
	if xv1[3] != xv2[3] {
		t.Errorf("x[3] mismatch: V1=%d V2=%d", xv1[3], xv2[3])
	}

	want14 := uint64(0x01000000)
	if xv2[14] != want14 {
		t.Errorf("V2 x[14]=0x%x, want 0x%x", xv2[14], want14)
	}
}

// TestSRL_RealBlock_V1vV2 uses the real emitBlock on the srl ELF binary,
// then compiles the resulting IR block with both V1 and V2 to find divergences.
func TestSRL_RealBlock_V1vV2(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	// Block 61 starts at pc=0x35c with maxBlockInsns=9 (from earlier tracing).
	// But with maxBlockInsns=2048, the failing block is block 39 starting
	// much earlier. Let's find the first failing block by running lockstep.
	cpu1 := NewCPU(*mem)
	cpu1.SetPC(0)
	cpu1.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })

	cpu2 := NewCPU(*mem)
	cpu2.SetPC(0)
	cpu2.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })

	jitV1 := NewJIT()
	jitV2 := NewJIT()
	jitV2.UseV2 = true

	for block := 0; block < 200; block++ {
		if cpu1.pc != cpu2.pc {
			t.Fatalf("block %d: PC desync before dispatch: V1=0x%x V2=0x%x", block, cpu1.pc, cpu2.pc)
		}

		ic1, err1 := jitV1.StepBlock(cpu1)
		ic2, err2 := jitV2.StepBlock(cpu2)
		_, _ = ic2, err2

		// Run interpreter for V2 CPU the same number of V1 steps
		// Actually both should run independently via their own JIT.
		_ = ic1
		_ = err1

		// Compare registers
		for r := 0; r < 32; r++ {
			if cpu1.x[r] != cpu2.x[r] {
				t.Fatalf("block %d (pc=0x%x, ic1=%d): x[%d] V1=0x%x V2=0x%x",
					block, cpu1.pc, ic1, r, cpu1.x[r], cpu2.x[r])
			}
		}
		if cpu1.pc != cpu2.pc {
			t.Fatalf("block %d: PC after: V1=0x%x V2=0x%x", block, cpu1.pc, cpu2.pc)
		}

		// Check for exit
		if err1 != nil || err2 != nil {
			break
		}
	}
	//t.Logf("V1 vs V2 lockstep passed, pc=0x%x cycles=%d", cpu1.pc, cpu1.Cycle())
}

// TestSRL_Block39_Alloc dumps the register allocation for the real block 39.
func TestSRL_Block39_Alloc(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	// Block 39 starts near the beginning of the test code.
	// Find it by running StepBlock until we get a large block.
	cpu := NewCPU(*mem)
	cpu.SetPC(0)
	cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })
	jit := NewJIT()
	for i := 0; i < 39; i++ {
		jit.StepBlock(cpu)
	}
	// Now cpu.pc is at the start of block 39.
	pc := cpu.pc
	//t.Logf("block 39 starts at pc=0x%x", pc)

	res := emitBlock(&cpu.mem, pc)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	//t.Logf("block: numInsns=%d, %d IR instrs", res.numInsns, len(res.block.Instrs))

	pool := ir.AMD64Pool(res.block)
	j := NewJIT()
	alloc := j.irAlloc.Allocate(res.block, pool, ir.AMD64Pinned(), nil)

	// Find all intervals for x1.
	for _, ia := range alloc.IntervalMap {
		if ia.Interval.VReg == ir.VReg(1) {
			//t.Logf("x1 interval: [%d..%d] host=%d", ia.Interval.Start, ia.Interval.End, ia.Host)
		}
	}
	// Print first 30 IR instructions with their vreg uses/defs.
	for i := 0; i < 30 && i < len(res.block.Instrs); i++ {
		ins := &res.block.Instrs[i]
		_ = ins
		//t.Logf("[%d] %v", i, ins)
	}
}

// TestDebugV1V2_SRL runs the srl ELF test with the V1-vs-V2 debug machine
// to find the exact block and registers where V1 diverges from V2.
func TestDebugV1V2_SRL(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(0)
	cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })

	jit := NewJIT()
	jit.DebugV1V2 = true // compile every block with V1+V2, compare results

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("V1/V2 mismatch: %v", r)
		}
	}()

	for i := 0; i < 500; i++ {
		_, err := jit.StepBlock(cpu)
		if err != nil {
			//t.Logf("exit at block %d pc=0x%x: %v", i, cpu.PC(), err)
			return
		}
	}
	//t.Logf("passed %d blocks, pc=0x%x", 500, cpu.PC())
}

func TestDebugV1V2_SRL_DumpAlloc(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	j := NewJIT()

	// The failing block is at pc=0x322. But emitBlock starts at a block
	// boundary, not necessarily 0x322. Let me find it by running to that PC.
	cpu := NewCPU(*mem)
	cpu.SetPC(0)
	cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })
	jit := NewJIT()
	for i := 0; i < 500; i++ {
		if cpu.pc == 0x322 || (cpu.pc < 0x322 && cpu.pc+0x400 > 0x322) {
			res := emitBlock(&cpu.mem, cpu.pc)
			if res != nil && res.startPC <= 0x322 && res.endPC > 0x322 {
				//t.Logf("found block: startPC=0x%x endPC=0x%x numInsns=%d irLen=%d", res.startPC, res.endPC, res.numInsns, len(res.block.Instrs))
				pool := ir.AMD64Pool(res.block)
				alloc := j.irAlloc.Allocate(res.block, pool, ir.AMD64Pinned(), nil)
				for _, ia := range alloc.IntervalMap {
					vr := ia.Interval.VReg
					if vr == ir.VReg(11) || vr == ir.VReg(12) {
						//t.Logf("  VReg(%d) [%d..%d] host=%d", vr, ia.Interval.Start, ia.Interval.End, ia.Host)
					}
				}
				return
			}
		}
		jit.StepBlock(cpu)
	}
	t.Fatal("did not find block covering 0x322")
}

/*
// TestMetaIterOrder runs the V1-vs-V2 comparison across 100 different
// gotoTargets iteration start offsets. This flushes out any remaining
// order-dependent lowerer bugs that a single sorted order might hide.
func TestMetaIterOrder_SRL(t *testing.T) {
	t.Skip("too slow for normal test runs.")
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	for offset := 0; offset < 100; offset++ {
		testIterStart = offset
		t.Run(fmt.Sprintf("offset=%d", offset), func(t *testing.T) {
			cpu := NewCPU(*mem)
			cpu.SetPC(0)
			cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })
			jit := NewJIT()
			jit.DebugV1V2 = true
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("V1/V2 mismatch at iterStart=%d: %v", offset, r)
				}
			}()
			for i := 0; i < 500; i++ {
				_, err := jit.StepBlock(cpu)
				if err != nil {
					return
				}
			}
		})
	}
	testIterStart = 0 // reset
}

// TestMetaIterOrder_AllUI runs ALL rv64ui ELF tests across multiple
// iteration orderings.
func TestMetaIterOrder_AllUI(t *testing.T) {
	t.Skip("too slow for normal test runs.")
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found")
	}

	for offset := 0; offset < 20; offset++ {
		testIterStart = offset
		t.Run(fmt.Sprintf("offset=%d", offset), func(t *testing.T) {
			for _, path := range entries {
				name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
				t.Run(name, func(t *testing.T) {
					mem, err := NewGuestMemory(Size64MB)
					if err != nil {
						t.Fatal(err)
					}
					defer mem.Free()
					_, err = LoadELF(mem, path)
					if err != nil {
						t.Fatal(err)
					}

					cpu := NewCPU(*mem)
					cpu.SetPC(0)
					cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition { return NoteHandled })
					jit := NewJIT()
					jit.DebugV1V2 = true
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("V1/V2 mismatch at iterStart=%d: %v", offset, r)
						}
					}()
					for i := 0; i < 1000; i++ {
						_, err := jit.StepBlock(cpu)
						if err != nil {
							return
						}
					}
				})
			}
		})
	}
	testIterStart = 0
}
*/

// TestBisectBlockSize finds the smallest maxBlockInsns where V1 diverges from V2.
func TestBisectBlockSize(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	trySize := func(n int) bool {
		maxBlockInsns = n
		defer func() { maxBlockInsns = 2048 }()

		cpu := NewCPU(*mem)
		cpu.SetPC(0)
		cpu.Notes.Push(func(c *CPU, note Note) NoteDisposition { return NoteHandled })
		jit := NewJIT()
		jit.DebugV1V2 = true

		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			for i := 0; i < 500; i++ {
				_, err := jit.StepBlock(cpu)
				if err != nil {
					return
				}
			}
		}()
		return !panicked // true = PASS
	}

	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 15, 20, 30, 50, 100, 200, 500, 2048} {
		pass := trySize(n)
		_ = pass
		//t.Logf("maxBlockInsns=%d: %v", n, map[bool]string{true: "PASS", false: "FAIL"}[pass])
	}
}

func TestBisectBlockSize2(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	trySize := func(n int) (pass bool, detail string) {
		maxBlockInsns = n
		defer func() { maxBlockInsns = 2048 }()

		cpu := NewCPU(*mem)
		cpu.SetPC(0)
		cpu.Notes.Push(func(c *CPU, note Note) NoteDisposition { return NoteHandled })
		jit := NewJIT()
		jit.DebugV1V2 = true

		func() {
			defer func() {
				if r := recover(); r != nil {
					pass = false
					detail = fmt.Sprintf("%v", r)
				}
			}()
			pass = true
			for i := 0; i < 500; i++ {
				_, err := jit.StepBlock(cpu)
				if err != nil {
					return
				}
			}
		}()
		return
	}

	for n := 11; n <= 14; n++ {
		pass, detail := trySize(n)
		_ = detail
		if pass {
			//t.Logf("n=%d: PASS", n)
		} else {
			//t.Logf("n=%d: FAIL %s", n, detail)
		}
	}
}

func TestBisectBlockSize3(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	trySize := func(n int) (pass bool, pc uint64, detail string) {
		maxBlockInsns = n
		defer func() { maxBlockInsns = 2048 }()
		cpu := NewCPU(*mem)
		cpu.SetPC(0)
		cpu.Notes.Push(func(c *CPU, note Note) NoteDisposition { return NoteHandled })
		jit := NewJIT()
		jit.DebugV1V2 = true
		func() {
			defer func() {
				if r := recover(); r != nil {
					pass = false
					pc = cpu.pc
					detail = fmt.Sprintf("%v", r)
				}
			}()
			pass = true
			for i := 0; i < 500; i++ {
				_, err := jit.StepBlock(cpu)
				if err != nil {
					return
				}
			}
		}()
		return
	}

	for n := 11; n <= 16; n++ {
		pass, pc, detail := trySize(n)
		_, _ = pc, detail
		if pass {
			//t.Logf("n=%d: PASS", n)
		} else {
			//t.Logf("n=%d: FAIL at pc=0x%x: %s", n, pc, detail)
		}
	}
}

func TestDumpBlock_0x34e(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	maxBlockInsns = 15
	defer func() { maxBlockInsns = 2048 }()

	// Run until we're about to compile the block at 0x34e
	cpu := NewCPU(*mem)
	cpu.SetPC(0)
	cpu.Notes.Push(func(c *CPU, note Note) NoteDisposition { return NoteHandled })
	jit := NewJIT()
	for i := 0; i < 500; i++ {
		if cpu.pc == 0x34e {
			break
		}
		jit.StepBlock(cpu)
	}
	//t.Logf("stopped at pc=0x%x", cpu.pc)
	if cpu.pc != 0x34e {
		// Search nearby
		for pc := uint64(0x340); pc <= 0x360; pc += 2 {
			res := emitBlock(&cpu.mem, pc)
			if res != nil && res.startPC <= 0x34e && res.endPC > 0x34e {
				//t.Logf("block at 0x%x covers 0x34e: numInsns=%d irLen=%d", res.startPC, res.numInsns, len(res.block.Instrs))
				pool := ir.AMD64Pool(res.block)
				alloc := jit.irAlloc.Allocate(res.block, pool, ir.AMD64Pinned(), nil)
				// Print IR and allocation for shift instructions
				for i, ins := range res.block.Instrs {
					if ins.Op == ir.IRShr || ins.Op == ir.IRShl || ins.Op == ir.IRSar {
						aHost := findHost(alloc, ins.A, i)
						bHost := findHost(alloc, ins.B, i)
						dHost := findHost(alloc, ins.Dst, i)
						_, _, _ = aHost, bHost, dHost
						//t.Logf("  [%d] %v  a=VR%d→%s b=VR%d→%s dst=VR%d→%s", i, ins, ins.A, regName(aHost), ins.B, regName(bHost), ins.Dst, regName(dHost))
					}
				}
				break
			}
		}
	}
}

func findHost(alloc *ir.Allocation, vr ir.VReg, idx int) int16 {
	for _, ia := range alloc.IntervalMap {
		if ia.Interval.VReg == vr && ia.Interval.Start <= idx && idx <= ia.Interval.End {
			return ia.Host
		}
	}
	return -1
}

func regName(r int16) string {
	names := map[int16]string{
		2064: "RAX", 2065: "RCX", 2066: "RDX", 2067: "RBX",
		2068: "RSP", 2069: "RBP", 2070: "RSI", 2071: "RDI",
		2072: "R8", 2073: "R9", 2074: "R10", 2075: "R11",
		2076: "R12", 2077: "R13", 2078: "R14", 2079: "R15",
	}
	if n, ok := names[r]; ok {
		return n
	}
	return fmt.Sprintf("?%d", r)
}

func TestDumpBlock_0x34e_v2(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	maxBlockInsns = 15
	defer func() { maxBlockInsns = 2048 }()

	res := emitBlock(mem, 0x34e)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	//t.Logf("block: start=0x%x end=0x%x insns=%d irLen=%d", res.startPC, res.endPC, res.numInsns, len(res.block.Instrs))

	j := NewJIT()
	pool := ir.AMD64Pool(res.block)
	alloc := j.irAlloc.Allocate(res.block, pool, ir.AMD64Pinned(), nil)
	for i, ins := range res.block.Instrs {
		if ins.Op == ir.IRShr || ins.Op == ir.IRShl || ins.Op == ir.IRSar {
			aHost := findHost(alloc, ins.A, i)
			bHost := findHost(alloc, ins.B, i)
			dHost := findHost(alloc, ins.Dst, i)
			_, _, _ = aHost, bHost, dHost
			//t.Logf("  [%d] %v  a=VR%d→%s b=VR%d→%s dst=VR%d→%s", i, ins, ins.A, regName(aHost), ins.B, regName(bHost), ins.Dst, regName(dHost))
		}
	}
}

func TestDumpBlock_0x34e_v3(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	maxBlockInsns = 15
	defer func() { maxBlockInsns = 2048 }()

	res := emitBlock(mem, 0x34e)
	if res == nil {
		t.Fatal("nil")
	}

	pool := ir.AMD64Pool(res.block)
	j := NewJIT()
	alloc := j.irAlloc.Allocate(res.block, pool, ir.AMD64Pinned(), nil)

	//t.Logf("StackSlots=%d", alloc.StackSlots)
	for i := 0; i < len(alloc.Kind); i++ {
		if alloc.Kind[i] != ir.AllocUnused {
			//t.Logf("  VReg(%d): kind=%d spill=%d", i, alloc.Kind[i], alloc.SpillSlot[i])
		}
	}
	// Print ALL intervals for VReg 1 and 2
	for _, ia := range alloc.IntervalMap {
		vr := ia.Interval.VReg
		if vr == ir.VReg(1) || vr == ir.VReg(2) || vr == ir.VReg(14) {
			//t.Logf("  interval VR%d [%d..%d] host=%s", vr, ia.Interval.Start, ia.Interval.End, regName(ia.Host))
		}
	}

	// Print the IR around instruction 28
	for i := 25; i <= 32 && i < len(res.block.Instrs); i++ {
		//t.Logf("  [%d] %v", i, res.block.Instrs[i])
	}
}

// TestNativeTrace_0x34e dumps V1 vs V2 Prog listings and disassembled
// native code for the block at pc=0x34e that triggers the R11 clobber bug.
func TestNativeTrace_0x34e(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-srl")
	if err != nil {
		t.Fatal(err)
	}

	maxBlockInsns = 15
	defer func() { maxBlockInsns = 2048 }()

	// Find the block covering 0x34e.
	var res *emitResult
	for pc := uint64(0x340); pc <= 0x360; pc += 2 {
		r := emitBlock(mem, pc)
		if r != nil && r.startPC <= 0x34e && r.endPC > 0x34e {
			res = r
			break
		}
	}
	if res == nil {
		t.Fatal("could not find block covering 0x34e")
	}
	//t.Logf("block: start=0x%x end=0x%x insns=%d irLen=%d", res.startPC, res.endPC, res.numInsns, len(res.block.Instrs))

	// Print IR for the block.
	//t.Logf("=== IR ===")
	//for i, ins := range res.block.Instrs {
	//t.Logf("  [%2d] %v", i, ins)
	//}

	// Compile with V1.
	j := NewJIT()
	_, v1dbg, err := j.jitCompileDebug(res, false)
	if err != nil {
		t.Fatalf("V1 compile: %v", err)
	}

	// Compile with V2.
	_, v2dbg, err := j.jitCompileDebug(res, true)
	if err != nil {
		t.Fatalf("V2 compile: %v", err)
	}

	// Dump Prog listings.
	//t.Logf("=== V1 Progs ===")
	for _, line := range strings.Split(v1dbg.progs, "\n") {
		if line != "" {
			//t.Logf("  %s", line)
		}
	}
	//t.Logf("=== V2 Progs ===")
	for _, line := range strings.Split(v2dbg.progs, "\n") {
		if line != "" {
			//t.Logf("  %s", line)
		}
	}

	// Dump hex of assembled bytes.
	//t.Logf("=== V1 code (%d bytes) ===", len(v1dbg.code))
	//t.Logf("  % x", v1dbg.code)
	//t.Logf("=== V2 code (%d bytes) ===", len(v2dbg.code))
	//t.Logf("  % x", v2dbg.code)

	// Now actually execute both and compare results.
	cpu := NewCPU(*mem)
	cpu.SetPC(0)
	cpu.Notes.Push(func(c *CPU, note Note) NoteDisposition { return NoteHandled })
	jit := NewJIT()

	// Run until we reach 0x34e.
	for i := 0; i < 500; i++ {
		if cpu.pc == 0x34e {
			break
		}
		jit.StepBlock(cpu)
	}
	if cpu.pc != 0x34e {
		t.Fatalf("did not reach 0x34e, stopped at 0x%x", cpu.pc)
	}

	// Snapshot register state.
	var xSnap [32]uint64
	copy(xSnap[:], cpu.x[:])

	// Execute with V1.
	blkV1, _, err := jit.jitCompileDebug(res, false)
	if err != nil {
		t.Fatal(err)
	}
	r1 := jitcallCall(blkV1.fn, &cpu.x, &cpu.f, &cpu.fcsr, cpu.mem.Base(), cpu.mem.Mask())
	_ = r1
	var x1 [32]uint64
	copy(x1[:], cpu.x[:])

	// Restore and execute with V2.
	copy(cpu.x[:], xSnap[:])
	blkV2, _, err := jit.jitCompileDebug(res, true)
	if err != nil {
		t.Fatal(err)
	}
	r2 := jitcallCall(blkV2.fn, &cpu.x, &cpu.f, &cpu.fcsr, cpu.mem.Base(), cpu.mem.Mask())
	_ = r2
	// Compare.
	//t.Logf("V1: pc=0x%x ic=%d status=%d", r1.PC, r1.IC, r1.Status)
	//t.Logf("V2: pc=0x%x ic=%d status=%d", r2.PC, r2.IC, r2.Status)
	mismatch := false
	for i := 0; i < 32; i++ {
		if x1[i] != cpu.x[i] {
			//t.Logf("  x[%d] V1=0x%x V2=0x%x", i, x1[i], cpu.x[i])
			mismatch = true
		}
	}
	if mismatch {
		t.Error("V1/V2 register mismatch!")
	} else {
		//t.Log("V1/V2 registers match.")
	}
}

// TestDumpBlock_ld_st_0x1a0 investigates the ld_st hang by dumping the IR
// for the block at pc=0x1a0 in rv64ui-p-ld_st. We look for backward branches
// that lack BudgetCheck, which would cause an infinite loop in native code.
func TestDumpBlock_ld_st_0x1a0(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, "riscv-elf-tests/rv64ui-p-ld_st")
	if err != nil {
		t.Fatal(err)
	}

	// Try to find a block covering 0x1a0.
	var res *emitResult
	for pc := uint64(0x180); pc <= 0x1b0; pc += 2 {
		r := emitBlock(mem, pc)
		if r != nil && r.startPC <= 0x1a0 && r.endPC > 0x1a0 {
			res = r
			break
		}
	}
	if res == nil {
		// Try emitting directly from 0x1a0.
		res = emitBlock(mem, 0x1a0)
	}
	if res == nil {
		t.Fatal("could not emit block covering 0x1a0")
	}
	//t.Logf("block: start=0x%x end=0x%x insns=%d irLen=%d", res.startPC, res.endPC, res.numInsns, len(res.block.Instrs))

	// Dump full IR.
	//t.Logf("=== IR (%d instructions) ===", len(res.block.Instrs))
	//for i, ins := range res.block.Instrs {
	//t.Logf("  [%3d] %v", i, ins)
	//}

	// Count backward jumps and budget checks.
	budgetChecks := 0
	jumps := 0
	branches := 0
	for _, ins := range res.block.Instrs {
		switch ins.Op {
		case ir.IRJump:
			jumps++
		case ir.IRBranch, ir.IRBranchImm:
			branches++
		}
	}
	// BudgetCheck emits: BranchImm + Jump + PlaceLabel + WriteBackAll-seq + Ret
	// Count BranchImm with Imm2=4096 (MaxIC) as budget checks.
	for _, ins := range res.block.Instrs {
		if ins.Op == ir.IRBranchImm && ins.Imm2 == int64(ir.MaxIC) {
			budgetChecks++
		}
	}
	//t.Logf("jumps=%d branches=%d budgetChecks=%d", jumps, branches, budgetChecks)

	// Also dump the Prog listing.
	j := NewJIT()
	_, dbg, cerr := j.jitCompileDebug(res, false)
	if cerr != nil {
		t.Fatalf("V1 compile: %v", cerr)
	}
	progLines := strings.Split(dbg.progs, "\n")
	//t.Logf("=== V1 Progs (%d lines, %d bytes) ===", len(progLines), len(dbg.code))
	for _, line := range progLines {
		if line != "" {
			//t.Logf("  %s", line)
		}
	}
}

// TestNativeTrace_sraw investigates the sraw lockstep failure by comparing
// V1 vs V2 on the failing block, and also running the interpreter to see
// where the divergence occurs.
func TestNativeTrace_sraw(t *testing.T) {
	testNativeTraceW(t, "riscv-elf-tests/rv64ui-p-sraw", 39)
}

// TestDispatchTrace_sraw traces the RunJIT dispatch loop to diagnose
// why sraw hangs — logs first 100 dispatch cycles with PC/IC/status.
func TestDispatchTrace_sraw(t *testing.T) {
	data, err := os.ReadFile("riscv-elf-tests/rv64ui-p-sraw")
	if err != nil {
		t.Skip("ELF not found")
	}
	mem, merr := NewGuestMemory(Size32KB)
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()
	entry, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	if addr, ok := FindSymbolAddr(data, "tohost"); ok {
		cpu.SetWatchAddr(addr)
		//t.Logf("tohost=0x%x", addr)
	}
	cpu.Notes.Push(func(c *CPU, n Note) NoteDisposition {
		if IsEcall(n) {
			//t.Logf("ECALL at pc=0x%x a7=%d a0=%d", n.PC, c.Reg(17), c.Reg(10))
			return NoteFatal
		}
		return NoteForward
	})
	jit := NewJIT()
	maxCycles := 200
	for i := 0; i < maxCycles; i++ {
		pc := cpu.pc
		ic, serr := jit.StepBlock(cpu)
		_, _, _ = pc, ic, serr
		//t.Logf("cycle %d: pc=0x%x -> pc=0x%x ic=%d err=%v", i, pc, cpu.pc, ic, serr)
		if serr != nil {
			//t.Logf("  stopped: %v", serr)
			break
		}
		// Check tohost
		if cpu.WatchAddr() != 0 {
			if v, _ := (&cpu.mem).Load64(cpu.WatchAddr()); v != 0 {
				//t.Logf("  tohost=0x%x at cycle %d", v, i)
				break
			}
		}
	}
}

func TestNativeTrace_srlw(t *testing.T) {
	testNativeTraceW(t, "riscv-elf-tests/rv64ui-p-srlw", 39)
}

func TestNativeTrace_sllw(t *testing.T) {
	testNativeTraceW(t, "riscv-elf-tests/rv64ui-p-sllw", 39)
}

func testNativeTraceW(t *testing.T, elfPath string, targetBlock int) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	_, err = LoadELF(mem, elfPath)
	if err != nil {
		t.Fatal(err)
	}

	// Run JIT until we reach the target block.
	cpu := NewCPU(*mem)
	cpu.SetPC(0)
	cpu.Notes.Push(func(c *CPU, note Note) NoteDisposition { return NoteHandled })
	jit := NewJIT()
	var lastPC uint64
	_ = lastPC
	for i := 0; i < targetBlock; i++ {
		lastPC = cpu.pc
		ic, jerr := jit.StepBlock(cpu)
		_ = ic
		if jerr != nil {
			// Check for ECALL exit
			if _, ok := jerr.(*MemFault); ok {
				continue
			}
		}
	}
	//t.Logf("at block %d, pc=0x%x (prev=0x%x)", targetBlock, cpu.pc, lastPC)

	// Snapshot state.
	var xSnap [32]uint64
	copy(xSnap[:], cpu.x[:])
	pcSnap := cpu.pc

	// Emit the block at current PC.
	res := emitBlock(&cpu.mem, cpu.pc)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	//t.Logf("block: start=0x%x end=0x%x insns=%d irLen=%d", res.startPC, res.endPC, res.numInsns, len(res.block.Instrs))

	// Compile with V1 and V2.
	blkV1, v1dbg, err := jit.jitCompileDebug(res, false)
	if err != nil {
		t.Fatalf("V1 compile: %v", err)
	}
	blkV2, v2dbg, err := jit.jitCompileDebug(res, true)
	if err != nil {
		t.Fatalf("V2 compile: %v", err)
	}

	// Execute V1.
	copy(cpu.x[:], xSnap[:])
	cpu.pc = pcSnap
	r1 := jitcallCall(blkV1.fn, &cpu.x, &cpu.f, &cpu.fcsr, cpu.mem.Base(), cpu.mem.Mask())
	var xV1 [32]uint64
	copy(xV1[:], cpu.x[:])

	// Execute V2.
	copy(cpu.x[:], xSnap[:])
	cpu.pc = pcSnap
	r2 := jitcallCall(blkV2.fn, &cpu.x, &cpu.f, &cpu.fcsr, cpu.mem.Base(), cpu.mem.Mask())
	_ = r2
	var xV2 [32]uint64
	copy(xV2[:], cpu.x[:])

	//t.Logf("V1: pc=0x%x ic=%d status=%d", r1.PC, r1.IC, r1.Status)
	//t.Logf("V2: pc=0x%x ic=%d status=%d", r2.PC, r2.IC, r2.Status)

	// Compare V1 vs V2.
	v1v2Match := true
	for i := 0; i < 32; i++ {
		if xV1[i] != xV2[i] {
			//t.Logf("  V1!=V2 x[%d]: V1=0x%x V2=0x%x", i, xV1[i], xV2[i])
			v1v2Match = false
		}
	}
	if v1v2Match {
		//t.Log("V1==V2 (both lowerers agree)")
	}

	// Run interpreter for the same IC steps.
	copy(cpu.x[:], xSnap[:])
	cpu.pc = pcSnap
	interpIC := r1.IC
	var interpErr error
	for i := uint64(0); i < interpIC; i++ {
		interpErr = cpu.step()
		cpu.cycle++
		if interpErr != nil {
			//t.Logf("interpreter error at step %d: %v (pc=0x%x)", i, interpErr, cpu.pc)
			break
		}
	}
	//t.Logf("interp: pc=0x%x after %d steps", cpu.pc, interpIC)

	// Compare V1 vs interpreter.
	v1InterpMatch := true
	for i := 0; i < 32; i++ {
		if xV1[i] != cpu.x[i] {
			//t.Logf("  V1!=interp x[%d]: V1=0x%x interp=0x%x", i, xV1[i], cpu.x[i])
			v1InterpMatch = false
		}
	}
	if v1InterpMatch {
		//t.Log("V1==interp (JIT and interpreter agree)")
	} else {
		//t.Log("V1!=interp — DIVERGENCE")
		// Dump first few progs for debugging.
		lines := strings.Split(v1dbg.progs, "\n")
		limit := 30
		if len(lines) < limit {
			limit = len(lines)
		}
		//t.Logf("=== V1 Progs (first %d) ===", limit)
		for _, line := range lines[:limit] {
			if line != "" {
				//t.Logf("  %s", line)
			}
		}
	}
	_ = v2dbg
}

// TestEmitEcallNonTerminal verifies that the emitter continues past
// ECALL when a dispatcher is available. The emitted IR should contain
// IRSyscall followed by reload instructions and then the post-ECALL
// guest instructions.
func TestEmitEcallNonTerminal(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	pc := uint64(0x1000)
	// Guest program:
	//   0x1000: LI a7, 64        (ADDI x17, x0, 64) — set syscall number
	//   0x1004: LI a0, 1         (ADDI x10, x0, 1)  — fd = stdout
	//   0x1008: ECALL
	//   0x100c: ADDI x5, x0, 99  — post-ECALL instruction
	//   0x1010: ECALL            — final ECALL (termination)
	mem.Store32(pc+0, ienc(opOPIMM, 0, 17, 0, 64))  // LI a7, 64
	mem.Store32(pc+4, ienc(opOPIMM, 0, 10, 0, 1))   // LI a0, 1
	mem.Store32(pc+8, instrECALL)                     // ECALL
	mem.Store32(pc+12, ienc(opOPIMM, 0, 5, 0, 99))  // ADDI x5, x0, 99
	mem.Store32(pc+16, instrECALL)                    // ECALL

	res := emitBlockRange(mem, pc, pc+20)
	if res == nil {
		t.Fatal("emitBlockRange returned nil")
	}

	// The emitted range should cover past the first ECALL.
	// With non-terminal ECALL, endPC should be > pc+12 (past first ECALL).
	if res.endPC <= pc+12 {
		t.Errorf("endPC = 0x%x, want > 0x%x (emission should continue past first ECALL)",
			res.endPC, pc+12)
	}

	// Count IRSyscall instructions — should be at least 1 (possibly 2).
	syscallCount := 0
	for _, ins := range res.block.Instrs {
		if ins.Op == ir.IRSyscall {
			syscallCount++
		}
	}
	if syscallCount == 0 {
		t.Error("no IRSyscall found — ECALL should emit IRSyscall")
	}

	// There should be IR instructions after the first IRSyscall
	// (the reload of a0/a1 and the ADDI x5, x0, 99).
	firstSyscall := -1
	for i, ins := range res.block.Instrs {
		if ins.Op == ir.IRSyscall {
			firstSyscall = i
			break
		}
	}
	if firstSyscall >= 0 {
		remaining := len(res.block.Instrs) - firstSyscall - 1
		if remaining < 3 {
			t.Errorf("only %d IR ops after first IRSyscall, want >= 3 "+
				"(reload a0, reload a1, ADDI x5)", remaining)
		}
	}

	t.Logf("emitBlockRange: %d guest insns, endPC=0x%x, %d IR ops, %d syscalls",
		res.numInsns, res.endPC, len(res.block.Instrs), syscallCount)
}

// TestLastIRWasTerminator_SyscallNotTerminal verifies that IRSyscall
// is no longer treated as a terminator by the emitter.
func TestLastIRWasTerminator_SyscallNotTerminal(t *testing.T) {
	e := &emitter{irEm: ir.NewEmitter()}

	// Emit an IRSyscall as the last instruction.
	e.irEm.Syscall(0x1022, 0xDEAD)

	if e.lastIRWasTerminator() {
		t.Error("IRSyscall should NOT be a terminator — function-level compilation continues past ECALL")
	}

	// Verify that actual terminators still report true.
	e.irEm.Ret(0x1030, 0, ir.VRegZero)
	if !e.lastIRWasTerminator() {
		t.Error("IRRet should be a terminator")
	}
}
