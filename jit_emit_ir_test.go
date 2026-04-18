//go:build !tcc

package riscv

import (
	"riscv/internal/jitcall"
	"riscv/ir"
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

// TestSrc1EqDest tests SUB x1, x1, x2 (rd==rs1 aliasing).
func TestSrc1EqDest(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 13),       // ADDI x1, x0, 13
		ienc(opOPIMM, 0, 2, 0, 11),       // ADDI x2, x0, 11
		renc(opOP, 0, 0x20, 1, 1, 2),     // SUB x1, x1, x2  (rd==rs1)
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
		if err != nil {
			t.Logf("block %d: pc=0x%x ic=%d err=%v gp=%d", block, pc, ic, err, cpu.Reg(3))
			break
		}
	}

	// Now at block 39
	t.Logf("block 39 starts at PC=0x%x, gp=%d", cpu.PC(), cpu.Reg(3))

	// Dump next instructions
	pc := cpu.PC()
	for i := 0; i < 20; i++ {
		half, _ := mem.Fetch16(pc)
		if half&3 != 3 {
			t.Logf("  0x%04x: %04x (RVC)", pc, half)
			pc += 2
		} else {
			insn, _ := mem.Fetch32(pc)
			t.Logf("  0x%04x: %08x", pc, insn)
			pc += 4
		}
	}

	// Run block 39 with JIT
	pc39 := cpu.PC()
	res := emitBlock(mem, pc39)
	if res == nil {
		t.Fatal("emitBlock returned nil for block 39")
	}
	t.Logf("block 39 IR: %d instructions", len(res.block.Instrs))
	for i, ins := range res.block.Instrs {
		t.Logf("  [%d] %s", i, ins.String())
	}
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
	jit.trace = true

	for block := 0; block < 50; block++ {
		pc := cpu.PC()
		ic, err := jit.StepBlock(cpu)
		_ = ic
		if err != nil {
			t.Logf("block %d: pc=0x%x exit err=%v gp=%d", block, pc, err, cpu.Reg(3))
			break
		}
	}
	t.Logf("final: pc=0x%x gp=%d x10=%d", cpu.PC(), cpu.Reg(3), cpu.Reg(10))
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

	want := uint64(0x01FFFFFFFF000000)
	got := cpu.x[7]
	t.Logf("x7 = 0x%x (want 0x%x), jitErr=%v, pc=0x%x", got, want, jitErr, cpu.pc)
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
	mem.Store32(pc, 0x800005B7)   // LUI x11, 0x80000
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
	t.Logf("x[14]=0x%x (want 0x%x), x[11]=0x%x, x[12]=0x%x", got, want, cpu.x[11], cpu.x[12])
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
	mem.Store32(pc, 0x800005B7)   // LUI x11, 0x80000
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
	t.Logf("after block 1: x[11]=0x%x, pc=0x%x, err=%v", cpu.x[11], cpu.pc, err1)
	if cpu.x[11] != 0xFFFFFFFF80000000 {
		t.Fatalf("block 1: x[11]=0x%x, want 0xFFFFFFFF80000000", cpu.x[11])
	}
	// Advance past ECALL
	cpu.SetPC(pc + 8)

	// Block 2
	_, err2 := jit.StepBlock(cpu)
	t.Logf("after block 2: x[14]=0x%x, x[11]=0x%x, x[12]=0x%x, pc=0x%x, err=%v",
		cpu.x[14], cpu.x[11], cpu.x[12], cpu.pc, err2)

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
	compiled, cerr := jitCompile(&emitResult{block: blk, numInsns: 3})
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}

	var x [32]uint64
	var f [32]uint64
	var fcsr uint32
	x[11] = 0xFFFFFFFF80000000
	x[12] = 0

	res := jitcallCall(compiled.fn, &x, &f, &fcsr, mem.Base(), mem.Mask())
	t.Logf("PC=0x%x IC=%d Status=%d x[14]=0x%x x[7]=0x%x", res.PC, res.IC, res.Status, x[14], x[7])

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
	compiled, cerr := jitCompileV2(&emitResult{block: blk, numInsns: 3})
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}

	var x [32]uint64
	var f [32]uint64
	var fcsr uint32
	x[11] = 0xFFFFFFFF80000000

	res := jitcallCall(compiled.fn, &x, &f, &fcsr, mem.Base(), mem.Mask())
	t.Logf("V2: PC=0x%x IC=%d x[14]=0x%x x[7]=0x%x", res.PC, res.IC, x[14], x[7])

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
	alloc := ir.Allocate(blk, pool, pinned, nil)

	t.Logf("StackSlots=%d", alloc.StackSlots)
	for i, k := range alloc.Kind {
		if k != ir.AllocUnused {
			t.Logf("VReg(%d): kind=%v spill=%d", i, k, alloc.SpillSlot[i])
		}
	}
	for _, ia := range alloc.IntervalMap {
		t.Logf("  interval: VReg(%d) [%d..%d] -> host=%d",
			ia.Interval.VReg, ia.Interval.Start, ia.Interval.End, ia.Host)
	}
}
