package riscv

import "testing"

// TestJIT_ADD verifies a single ADD instruction through the full JIT pipeline:
// emit C → compile with TCC → call via Go assembly trampoline.
func TestJIT_ADD(t *testing.T) {
	// ADD x1, x2, x3 followed by ECALL
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	// Encode: ADD x1, x2, x3 = funct7=0 rs2=3 rs1=2 funct3=0 rd=1 opcode=0x33
	add := uint32(0x003100B3) // ADD x1, x2, x3
	ecall := uint32(0x00000073)
	codeVA := uint64(0x1000)
	mem.Store32(codeVA, add)
	mem.Store32(codeVA+4, ecall)

	// Emit the block
	res := emitBlock(mem, codeVA)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	t.Logf("Generated C (%d insns):\n%s", res.numInsns, res.csrc)

	// Compile with TCC
	blk, err := tccCompile(res.csrc)
	if err != nil {
		t.Fatalf("tccCompile: %v", err)
	}
	t.Logf("Compiled block at %#x", blk.fn)

	// Use the JIT manager to run the block.
	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.SetReg(2, 100)
	cpu.SetReg(3, 42)

	jit := NewJIT()

	// Manually compile and cache the block
	jit.blocks[codeVA] = blk

	// Install a handler for the ECALL to stop execution
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseEcallU || n.Cause == CauseEcallS || n.Cause == CauseEcallM {
			return NoteFatal // stop
		}
		return NoteForward
	})

	// Run with JIT
	jit.RunJIT(cpu)

	// Check result
	got := cpu.Reg(1)
	want := uint64(142) // 100 + 42
	if got != want {
		t.Errorf("x1 = %d, want %d", got, want)
	}
	t.Logf("x1 = %d (correct: 100 + 42 = %d)", got, want)
}

// TestJIT_Fib verifies a tight loop through the JIT pipeline.
// Computes fib(20) = 6765.
func TestJIT_Fib(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	// fib(n): a=x10, b=x11, i=x12, n=x13, tmp=x5
	// 0x1000: ADD  x5, x10, x11   # tmp = a + b
	// 0x1004: ADDI x10, x11, 0    # a = b
	// 0x1008: ADDI x11, x5, 0     # b = tmp
	// 0x100c: ADDI x12, x12, 1    # i++
	// 0x1010: BLT  x12, x13, -16  # if i < n goto 0x1000
	// 0x1014: ECALL               # exit
	code := []uint32{
		0x00B50533, // ADD  x10(a0), x10(a0), x11(a1) -- wait, this is wrong
	}
	// Let me use proper encodings. Use BuildELF for simplicity.
	_ = code

	// Actually, let's build this with proper instruction encoding.
	// ADD x5, x10, x11 = funct7=0 rs2=11 rs1=10 funct3=0 rd=5 op=0x33
	add_t0_a0_a1 := renc(0x33, 0, 0x00, 5, 10, 11)
	// ADDI x10, x11, 0 = imm=0 rs1=11 funct3=0 rd=10 op=0x13
	mv_a0_a1 := uint32(0<<20 | 11<<15 | 0<<12 | 10<<7 | 0x13)
	// ADDI x11, x5, 0
	mv_a1_t0 := uint32(0<<20 | 5<<15 | 0<<12 | 11<<7 | 0x13)
	// ADDI x12, x12, 1
	addi_a2_1 := uint32(1<<20 | 12<<15 | 0<<12 | 12<<7 | 0x13)
	blt := benc(0x63, 4, 12, 13, -16) // BLT x12, x13, -16
	ecall := uint32(0x00000073)

	codeVA := uint64(0x1000)
	insns := []uint32{add_t0_a0_a1, mv_a0_a1, mv_a1_t0, addi_a2_1, blt, ecall}
	for i, insn := range insns {
		mem.Store32(codeVA+uint64(i*4), insn)
	}

	// Set up CPU
	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.SetReg(10, 0)  // a = 0
	cpu.SetReg(11, 1)  // b = 1
	cpu.SetReg(12, 0)  // i = 0
	cpu.SetReg(13, 20) // n = 20

	// Install ecall handler to stop
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleEcall(func(cpu *CPU, args SyscallArgs) NoteDisposition {
		return NoteFatal
	})
	cpu.Notes.Push(o.Handle)

	jit := NewJIT()
	jit.RunJIT(cpu)

	got := cpu.Reg(10)
	want := uint64(6765) // fib(20)
	if got != want {
		t.Errorf("fib(20) = %d, want %d", got, want)
	}
	t.Logf("fib(20) = %d, insns = %d", got, cpu.Cycle())
}

// renc encodes an R-type instruction.
func renc(opcode, funct3, funct7 uint32, rd, rs1, rs2 uint32) uint32 {
	return funct7<<25 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

// benc encodes a B-type (branch) instruction.
func benc(opcode, funct3, rs1, rs2 uint32, offset int32) uint32 {
	imm := uint32(offset)
	b12 := (imm >> 12) & 1
	b11 := (imm >> 11) & 1
	b10_5 := (imm >> 5) & 0x3F
	b4_1 := (imm >> 1) & 0xF
	return b12<<31 | b10_5<<25 | rs2<<20 | rs1<<15 | funct3<<12 | b4_1<<8 | b11<<7 | opcode
}
