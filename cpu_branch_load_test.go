package riscv

import "testing"

// ── helpers ───────────────────────────────────────────────────────────────

func newCPUAt(t *testing.T, mem *GuestMemory, codeAddr, dataAddr uint64) *CPU {
	t.Helper()
	cpu := NewCPU(*mem)
	cpu.SetPC(codeAddr)
	cpu.SetReg(11, dataAddr) // a1 = base address
	return cpu
}

func storeProgram(t *testing.T, mem *GuestMemory, addr uint64, insns []uint32) {
	t.Helper()
	for i, insn := range insns {
		if f := mem.Store32(addr+uint64(i*4), insn); f != nil {
			t.Fatalf("storeProgram[%d]: %v", i, f)
		}
	}
}

func runCPU(t *testing.T, cpu *CPU) {
	t.Helper()
	if err := cpu.Run(); err != nil && err != ErrEbreak {
		t.Fatalf("cpu.Run: %v", err)
	}
}

// ── BEQ ───────────────────────────────────────────────────────────────────

// TestCPU_BEQ_Taken: branch is taken when rs1 == rs2, skipping an ADDI.
//
//	BEQ  a0, a1, +8    # a0==a1 → taken, skip ADDI
//	ADDI a2, a2, 1     # must NOT execute
//	EBREAK
//
// BEQ B-type, offset=+8: 0x00B50463
func TestCPU_BEQ_Taken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B50463, // BEQ a0, a1, +8
		0x00160613, // ADDI a2, a2, 1   ← must be skipped
		0x00100073, // EBREAK
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 5) // a0 = 5
	cpu.SetReg(11, 5) // a1 = 5 → equal
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 0 {
		t.Errorf("BEQ taken: a2 = %d, want 0 (ADDI skipped)", got)
	}
}

// TestCPU_BEQ_NotTaken: branch not taken when rs1 != rs2, ADDI executes.
func TestCPU_BEQ_NotTaken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B50463, // BEQ a0, a1, +8
		0x00160613, // ADDI a2, a2, 1
		0x00100073, // EBREAK
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 5) // a0 = 5
	cpu.SetReg(11, 6) // a1 = 6 → not equal
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 1 {
		t.Errorf("BEQ not taken: a2 = %d, want 1", got)
	}
}

// TestCPU_BEQ_BackwardBranch: countdown loop using a negative-offset BEQ.
//
//	ADDI a2, zero, 3       # a2 = 3
//	loop:
//	ADDI a2, a2, -1        # a2--
//	BEQ  a2, zero, +8      # if a2==0, skip backward branch → fall to EBREAK
//	BEQ  zero, zero, -8    # always jump back to loop
//	EBREAK
//
// BEQ zero,zero,-8: offset=-8 → 0xFE000CE3
func TestCPU_BEQ_BackwardBranch(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00300613, // ADDI a2, zero, 3
		0xFFF60613, // ADDI a2, a2, -1   ← loop top
		0x00060463, // BEQ  a2, zero, +8
		0xFE000CE3, // BEQ  zero, zero, -8
		0x00100073, // EBREAK
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 0 {
		t.Errorf("BEQ backward: a2 = %d, want 0", got)
	}
}

// ── BNE ───────────────────────────────────────────────────────────────────

// TestCPU_BNE_Taken: branch taken when rs1 != rs2.
//
// BNE = BEQ with funct3=001: 0x00B51463
func TestCPU_BNE_Taken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B51463, // BNE a0, a1, +8
		0x00160613, // ADDI a2, a2, 1   ← must be skipped
		0x00100073, // EBREAK
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 3) // a0 = 3
	cpu.SetReg(11, 4) // a1 = 4 → not equal → taken
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 0 {
		t.Errorf("BNE taken: a2 = %d, want 0 (ADDI skipped)", got)
	}
}

// TestCPU_BNE_NotTaken: branch not taken when rs1 == rs2.
func TestCPU_BNE_NotTaken(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	const code = uint64(0x2000)
	storeProgram(t, mem, code, []uint32{
		0x00B51463, // BNE a0, a1, +8
		0x00160613, // ADDI a2, a2, 1
		0x00100073, // EBREAK
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	cpu.SetReg(10, 7) // a0 = 7
	cpu.SetReg(11, 7) // a1 = 7 → equal → not taken
	runCPU(t, cpu)

	if got := cpu.Reg(12); got != 1 {
		t.Errorf("BNE not taken: a2 = %d, want 1", got)
	}
}

// ── Load widths ────────────────────────────────────────────────────────────
//
// Memory layout at dataAddr (8 bytes, little-endian):
//   offset 0: 0xFF  offset 1: 0x7F  offset 2: 0x00  offset 3: 0x80
//   offset 4: 0x00  offset 5: 0x00  offset 6: 0x00  offset 7: 0x80
//
// Sign-extended bit patterns (all as uint64 two's complement):
//   LB  [0]  0xFF → 0xFFFFFFFFFFFFFFFF  (-1)
//   LB  [1]  0x7F → 0x000000000000007F  (+127)
//   LBU [0]  0xFF → 0x00000000000000FF  (255)
//   LH  [2]  0x8000 → 0xFFFFFFFFFFFF8000  (-32768)
//   LHU [2]  0x8000 → 0x0000000000008000  (32768)
//   LW  [0]  0x80007FFF → 0xFFFFFFFF80007FFF  (sign-extended)
//   LWU [0]  0x80007FFF → 0x0000000080007FFF  (zero-extended)
//   LD  [0]  0x8000000080007FFF

func setupLoadMem(t *testing.T, mem *GuestMemory, dataAddr uint64) {
	t.Helper()
	if f := mem.WriteBytes(dataAddr, []byte{
		0xFF, 0x7F, 0x00, 0x80, 0x00, 0x00, 0x00, 0x80,
	}); f != nil {
		t.Fatal(f)
	}
}

// LB a0, 0(a1): funct3=000, imm=0 → 0x00058503
func TestCPU_LB(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x00058503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	const want = uint64(0xFFFFFFFFFFFFFFFF)
	if got := cpu.Reg(10); got != want {
		t.Errorf("LB 0xFF: got 0x%016X, want 0x%016X", got, want)
	}
}

// LB a0, 1(a1): imm=1 → 0x00158503
func TestCPU_LB_Positive(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x00158503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	if got := cpu.Reg(10); got != 0x7F {
		t.Errorf("LB 0x7F: got 0x%016X, want 0x7F", got)
	}
}

// LBU a0, 0(a1): funct3=100 → 0x0005C503
func TestCPU_LBU(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x0005C503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	if got := cpu.Reg(10); got != 0xFF {
		t.Errorf("LBU 0xFF: got 0x%016X, want 0xFF", got)
	}
}

// LH a0, 2(a1): funct3=001, imm=2 → 0x00259503
func TestCPU_LH(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x00259503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	const want = uint64(0xFFFFFFFFFFFF8000)
	if got := cpu.Reg(10); got != want {
		t.Errorf("LH 0x8000: got 0x%016X, want 0x%016X", got, want)
	}
}

// LHU a0, 2(a1): funct3=101, imm=2 → 0x0025D503
func TestCPU_LHU(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x0025D503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	if got := cpu.Reg(10); got != 0x8000 {
		t.Errorf("LHU 0x8000: got 0x%016X, want 0x8000", got)
	}
}

// LW a0, 0(a1): funct3=010, imm=0 → 0x0005A503  (already tested in cpu_test.go)
// Here we specifically check the sign-extension of a negative word.
func TestCPU_LW_SignExtend(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x0005A503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	// bytes [0xFF,0x7F,0x00,0x80] LE → uint32 0x80007FFF → sign-extended to 64-bit
	const want = uint64(0xFFFFFFFF80007FFF)
	if got := cpu.Reg(10); got != want {
		t.Errorf("LW sign-extend: got 0x%016X, want 0x%016X", got, want)
	}
}

// LWU a0, 0(a1): funct3=110, imm=0 → 0x0005E503
func TestCPU_LWU(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x0005E503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	if got := cpu.Reg(10); got != 0x80007FFF {
		t.Errorf("LWU: got 0x%016X, want 0x80007FFF", got)
	}
}

// LD a0, 0(a1): funct3=011, imm=0 → 0x0005B503
func TestCPU_LD(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	const data, code = uint64(0x1000), uint64(0x2000)
	setupLoadMem(t, mem, data)
	storeProgram(t, mem, code, []uint32{0x0005B503, 0x00100073})
	cpu := newCPUAt(t, mem, code, data)
	runCPU(t, cpu)
	if got := cpu.Reg(10); got != 0x8000000080007FFF {
		t.Errorf("LD: got 0x%016X, want 0x8000000080007FFF", got)
	}
}
