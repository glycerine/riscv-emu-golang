package riscv

import "testing"

// ── helpers ───────────────────────────────────────────────────────────────

// newCPUAt creates a CPU with PC at codeAddr and a1 pointing at dataAddr.
func newCPUAt(t *testing.T, mem *GuestMemory, codeAddr, dataAddr uint64) *CPU {
	t.Helper()
	cpu := NewCPU(mem)
	cpu.SetPC(codeAddr)
	cpu.SetReg(11, dataAddr) // a1
	return cpu
}

// storeProgram writes a slice of 32-bit instruction words starting at addr.
func storeProgram(t *testing.T, mem *GuestMemory, addr uint64, insns []uint32) {
	t.Helper()
	for i, insn := range insns {
		if f := mem.Store32(addr+uint64(i*4), insn); f != nil {
			t.Fatalf("storeProgram[%d]: %v", i, f)
		}
	}
}

// runCPU runs until ErrEbreak; any other error is fatal.
func runCPU(t *testing.T, cpu *CPU) {
	t.Helper()
	if err := cpu.Run(); err != nil && err != ErrEbreak {
		t.Fatalf("cpu.Run: %v", err)
	}
}

// ── BEQ ───────────────────────────────────────────────────────────────────

// TestCPU_BEQ_Taken verifies that BEQ jumps when rs1 == rs2.
//
// Program (a0=5, a1=5 set by harness):
//
//	BEQ  a0, a1, +8    # taken: skip the ADDI, land on EBREAK
//	ADDI a2, a2, 1     # should NOT execute
//	EBREAK
//
// BEQ B-type encoding: imm[12|10:5] rs2 rs1 000 imm[4:1|11] 1100011
//   offset = +8 (two instructions forward)
//   imm bits: imm[12]=0 imm[11]=0 imm[10:5]=000000 imm[4:1]=0100
//   insn = 0 000000 01011 01010 000 0100 0 1100011 = 0x00B50463
func TestCPU_BEQ_Taken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B50463, // BEQ a0, a1, +8
		0x00160613, // ADDI a2, a2, 1   (must be skipped)
		0x00100073, // EBREAK
	})

	cpu := NewCPU(mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 5) // a0 = 5
	cpu.SetReg(11, 5) // a1 = 5  → equal → branch taken
	runCPU(t, cpu)

	// a2 must still be 0 — ADDI was skipped
	if got := cpu.Reg(12); got != 0 {
		t.Errorf("BEQ taken: a2 = %d, want 0 (ADDI should have been skipped)", got)
	}
}

// TestCPU_BEQ_NotTaken verifies that BEQ falls through when rs1 != rs2.
//
// Program (a0=5, a1=6):
//
//	BEQ  a0, a1, +8    # not taken: fall through to ADDI
//	ADDI a2, a2, 1     # should execute → a2 = 1
//	EBREAK
func TestCPU_BEQ_NotTaken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B50463, // BEQ a0, a1, +8
		0x00160613, // ADDI a2, a2, 1
		0x00100073, // EBREAK
	})

	cpu := NewCPU(mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 5) // a0 = 5
	cpu.SetReg(11, 6) // a1 = 6  → not equal → fall through
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 1 {
		t.Errorf("BEQ not taken: a2 = %d, want 1", got)
	}
}

// TestCPU_BEQ_BackwardBranch verifies a backward (negative offset) BEQ,
// which is the normal loop-back pattern.
//
// Program: count a2 down from 3 to 0 using a backward branch.
//
//	          ADDI a2, zero, 3   # a2 = 3
//	loop:     ADDI a2, a2,  -1  # a2--
//	          BEQ  a2, zero, +4  # if a2==0 skip next, fall to EBREAK
//	          BEQ  zero, zero, -8 # always branch back to loop
//	          EBREAK
//
// BEQ zero,zero,-8: offset=-8 → imm=1111111111111000
//   imm[12]=1 imm[11]=1 imm[10:5]=111111 imm[4:1]=1100
//   insn = 1 111111 00000 00000 000 1100 1 1100011 = 0xFE000CE3
func TestCPU_BEQ_BackwardBranch(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00300613, // ADDI a2, zero, 3      # a2 = 3
		0xFFF60613, // ADDI a2, a2,  -1      # loop: a2--
		0x00060263, // BEQ  a2, zero, +4     # if a2==0 skip backward branch
		0xFE000CE3, // BEQ  zero, zero, -8   # jump back to loop
		0x00100073, // EBREAK
	})

	cpu := NewCPU(mem)
	cpu.SetPC(code)
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 0 {
		t.Errorf("BEQ backward: a2 = %d, want 0", got)
	}
}

// ── BNE ───────────────────────────────────────────────────────────────────

// TestCPU_BNE_Taken verifies BNE jumps when rs1 != rs2.
//
// Program (a0=3, a1=4):
//
//	BNE  a0, a1, +8    # taken: values differ → skip ADDI
//	ADDI a2, a2, 1     # should NOT execute
//	EBREAK
//
// BNE = BEQ with funct3=001 instead of 000:
//   0x00B51463 (same imm/regs as BEQ test but funct3=001)
func TestCPU_BNE_Taken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B51463, // BNE a0, a1, +8
		0x00160613, // ADDI a2, a2, 1   (must be skipped)
		0x00100073, // EBREAK
	})

	cpu := NewCPU(mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 3) // a0 = 3
	cpu.SetReg(11, 4) // a1 = 4  → not equal → branch taken
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 0 {
		t.Errorf("BNE taken: a2 = %d, want 0 (ADDI should be skipped)", got)
	}
}

// TestCPU_BNE_NotTaken verifies BNE falls through when rs1 == rs2.
//
// Program (a0=7, a1=7):
//
//	BNE  a0, a1, +8    # not taken: equal → fall through
//	ADDI a2, a2, 1     # should execute → a2 = 1
//	EBREAK
func TestCPU_BNE_NotTaken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B51463, // BNE a0, a1, +8
		0x00160613, // ADDI a2, a2, 1
		0x00100073, // EBREAK
	})

	cpu := NewCPU(mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 7) // a0 = 7
	cpu.SetReg(11, 7) // a1 = 7  → equal → not taken
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 1 {
		t.Errorf("BNE not taken: a2 = %d, want 1", got)
	}
}

// ── Load widths ────────────────────────────────────────────────────────────
//
// All loads use a1 as the base address (set to dataAddr by harness).
// Result is left in a0 (x10).
//
// Memory layout at dataAddr:
//   byte 0: 0xFF
//   byte 1: 0x7F
//   byte 2: 0x00
//   byte 3: 0x80
//   bytes 4-7: 0x0000000080000000  (little-endian: 00 00 00 80 00 00 00 00)
//
// This gives us sign-extension edge cases for every width.

func setupLoadMem(t *testing.T, mem *GuestMemory, dataAddr uint64) {
	t.Helper()
	bytes := []byte{0xFF, 0x7F, 0x00, 0x80, 0x00, 0x00, 0x00, 0x80}
	if f := mem.WriteBytes(dataAddr, bytes); f != nil {
		t.Fatal(f)
	}
}

// TestCPU_LB loads a signed byte (sign-extends 8→64 bits).
//
//   LB a0, 0(a1)   # loads 0xFF → sign-extended → -1 as int64
//   EBREAK
//
// LB encoding: funct3=000, opcode=0x03
//   0x00058503
func TestCPU_LB(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x00058503, // LB a0, 0(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	got := int64(cpu.Reg(10))
	if got != -1 {
		t.Errorf("LB 0xFF: got %d (0x%016X), want -1", got, uint64(got))
	}
}

// TestCPU_LB_Positive loads a positive signed byte.
//
//   LB a0, 1(a1)   # loads 0x7F → +127
//
// imm=1: 0x00158503
func TestCPU_LB_Positive(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x00158503, // LB a0, 1(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	if got := cpu.Reg(10); got != 127 {
		t.Errorf("LB 0x7F: got %d, want 127", got)
	}
}

// TestCPU_LBU loads an unsigned byte (zero-extends 8→64 bits).
//
//   LBU a0, 0(a1)  # loads 0xFF → 255 (not -1)
//
// LBU: funct3=100, opcode=0x03
//   0x00058503 with funct3=100 → 0x00059503... wait:
//   funct3=100 → bits[14:12]=100
//   insn = imm[11:0]=0 rs1=01011 funct3=100 rd=01010 opcode=0000011
//        = 000000000000 01011 100 01010 0000011 = 0x0005C503
func TestCPU_LBU(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x0005C503, // LBU a0, 0(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	if got := cpu.Reg(10); got != 255 {
		t.Errorf("LBU 0xFF: got %d, want 255", got)
	}
}

// TestCPU_LH loads a signed halfword (sign-extends 16→64 bits).
//
//   LH a0, 2(a1)   # loads bytes [0x00, 0x80] LE → 0x8000 → -32768
//
// LH: funct3=001, imm=2
//   000000000010 01011 001 01010 0000011 = 0x00259503
func TestCPU_LH(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x00259503, // LH a0, 2(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	got := int64(cpu.Reg(10))
	if got != -32768 {
		t.Errorf("LH 0x8000: got %d, want -32768", got)
	}
}

// TestCPU_LHU loads an unsigned halfword (zero-extends 16→64 bits).
//
//   LHU a0, 2(a1)  # loads 0x8000 → 32768 (not -32768)
//
// LHU: funct3=101, imm=2
//   000000000010 01011 101 01010 0000011 = 0x0025D503
func TestCPU_LHU(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x0025D503, // LHU a0, 2(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	if got := cpu.Reg(10); got != 32768 {
		t.Errorf("LHU 0x8000: got %d, want 32768", got)
	}
}

// TestCPU_LW_SignExtend verifies LW sign-extends 32→64 bits.
//
//   LW a0, 0(a1)   # loads bytes [0xFF,0x7F,0x00,0x80] LE → 0x80007FFF → negative
//
// 0x80007FFF as int32 = -2147450881; sign-extended to int64 = same value.
//
// LW: funct3=010, imm=0 → 0x0005A503
func TestCPU_LW_SignExtend(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x0005A503, // LW a0, 0(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	got := int64(cpu.Reg(10))
	v32 := uint32(0x80007FFF)
	want := int64(int32(v32))
	if got != want {
		t.Errorf("LW sign-extend: got %d, want %d", got, want)
	}
}

// TestCPU_LWU loads an unsigned 32-bit word (zero-extends 32→64 bits).
//
//   LWU a0, 0(a1)  # loads 0x80007FFF → 2147516415 (positive)
//
// LWU: funct3=110, imm=0
//   000000000000 01011 110 01010 0000011 = 0x0005E503
func TestCPU_LWU(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x0005E503, // LWU a0, 0(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	if got := cpu.Reg(10); got != 0x80007FFF {
		t.Errorf("LWU: got 0x%X, want 0x80007FFF", got)
	}
}

// TestCPU_LD loads a full 64-bit doubleword.
//
//   LD a0, 0(a1)   # loads all 8 bytes → 0x8000000080007FFF
//
// LD: funct3=011, imm=0
//   000000000000 01011 011 01010 0000011 = 0x0005B503
func TestCPU_LD(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{
		0x0005B503, // LD a0, 0(a1)
		0x00100073, // EBREAK
	})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)

	if got := cpu.Reg(10); got != 0x8000000080007FFF {
		t.Errorf("LD: got 0x%016X, want 0x8000000080007FFF", got)
	}
}
