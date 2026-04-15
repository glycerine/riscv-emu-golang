package riscv

import (
	"math"
	"riscv/internal/fenv"
	"testing"
)

// TestCPU_LoadIncrementStore encodes a minimal RV64I program and executes it:
//
//	LW   a0, 0(a1)     # load 32-bit word from address in a1 into a0
//	ADDI a0, a0, 1     # increment
//	SW   a0, 0(a1)     # store back
//	EBREAK             # halt
//
// We place the value 41 at guest address 0x1000, run the program,
// and expect to read back 42.
func TestCPU_LoadIncrementStore(t *testing.T) {
	const memSize = Size64MB
	const dataAddr = uint64(0x1000) // where our integer lives
	const codeAddr = uint64(0x2000) // where our program lives

	mem, err := NewGuestMemory(memSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	// Write initial value 41 at dataAddr
	if f := mem.Store32(dataAddr, 41); f != nil {
		t.Fatal(f)
	}

	// Encode the program at codeAddr.
	//
	// Register assignments:
	//   a1 (x11) = dataAddr  — set by the test harness via cpu.SetReg
	//   a0 (x10) = scratch
	//
	// Encodings (RV64I, all 32-bit):
	//
	//   LW a0, 0(a1)   = 0x0005a503
	//     opcode=0x03 LOAD, funct3=010 LW, rd=x10, rs1=x11, imm=0
	//
	//   ADDI a0, a0, 1 = 0x00150513
	//     opcode=0x13 OP-IMM, funct3=000 ADDI, rd=x10, rs1=x10, imm=1
	//
	//   SW a0, 0(a1)   = 0x00a5a023
	//     opcode=0x23 STORE, funct3=010 SW, rs1=x11, rs2=x10, imm=0
	//
	//   EBREAK         = 0x00100073
	//     opcode=0x73 SYSTEM, funct12=0x001

	program := []uint32{
		0x0005a503, // LW   a0, 0(a1)
		0x00150513, // ADDI a0, a0, 1
		0x00a5a023, // SW   a0, 0(a1)
		0x00100073, // EBREAK
	}
	for i, insn := range program {
		if f := mem.Store32(codeAddr+uint64(i*4), insn); f != nil {
			t.Fatalf("storing instruction %d: %v", i, f)
		}
	}

	// Create CPU, set PC and registers
	cpu := NewCPU(*mem)
	cpu.SetPC(codeAddr)
	cpu.SetReg(11, dataAddr) // a1 = address of our integer

	// Run until EBREAK or error
	if err := cpu.Run(); err != nil && err != ErrEbreak {
		t.Fatalf("cpu.Run: %v", err)
	}

	// Read back the value — expect 42
	got, f := mem.Load32(dataAddr)
	if f != nil {
		t.Fatal(f)
	}
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

// ── RV64M divide-by-zero and overflow — spec-defined corner cases ─────────
// These can't be oracle-tested via libriscv (it delivers SIGFPE instead),
// so we verify directly against the RISC-V spec values.

func TestDIV_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	// DIV x1, x2, x3 where x3=0 -> x1 = -1
	insn := uint32(0x023140B3) // DIV x0... let us encode properly
	_ = insn
	// Use the CPU directly: set up registers and step
	cpu := setupM(t, mem, 0x023140B3, 42, 0) // DIV x1,x2,x3: x2=42,x3=0
	if cpu.Reg(1) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("DIV x,0: got 0x%016X want 0xFFFFFFFFFFFFFFFF", cpu.Reg(1))
	}
}
func TestDIV_Overflow(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023140B3, 0x8000000000000000, 0xFFFFFFFFFFFFFFFF)
	if cpu.Reg(1) != 0x8000000000000000 {
		t.Errorf("DIV INT_MIN,-1: got 0x%016X want 0x8000000000000000", cpu.Reg(1))
	}
}
func TestDIVU_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023150B3, 42, 0) // DIVU x1,x2,x3
	if cpu.Reg(1) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("DIVU x,0: got 0x%016X want 0xFFFFFFFFFFFFFFFF", cpu.Reg(1))
	}
}
func TestREM_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023160B3, 42, 0) // REM x1,x2,x3
	if cpu.Reg(1) != 42 {
		t.Errorf("REM x,0: got %d want 42", cpu.Reg(1))
	}
}
func TestREM_Overflow(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023160B3, 0x8000000000000000, 0xFFFFFFFFFFFFFFFF)
	if cpu.Reg(1) != 0 {
		t.Errorf("REM INT_MIN,-1: got 0x%016X want 0", cpu.Reg(1))
	}
}
func TestREMU_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023170B3, 42, 0) // REMU x1,x2,x3
	if cpu.Reg(1) != 42 {
		t.Errorf("REMU x,0: got %d want 42", cpu.Reg(1))
	}
}
func TestDIVW_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023140BB, 42, 0) // DIVW x1,x2,x3
	if cpu.Reg(1) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("DIVW x,0: got 0x%016X want 0xFFFFFFFFFFFFFFFF", cpu.Reg(1))
	}
}
func TestDIVW_Overflow(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023140BB, 0x80000000, 0xFFFFFFFFFFFFFFFF)
	if cpu.Reg(1) != 0xFFFFFFFF80000000 {
		t.Errorf("DIVW INT32_MIN,-1: got 0x%016X want 0xFFFFFFFF80000000", cpu.Reg(1))
	}
}
func TestDIVUW_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023150BB, 42, 0) // DIVUW x1,x2,x3
	// 2^32-1 sign-extended = 0xFFFFFFFFFFFFFFFF
	if cpu.Reg(1) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("DIVUW x,0: got 0x%016X want 0xFFFFFFFFFFFFFFFF", cpu.Reg(1))
	}
}
func TestREMW_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023160BB, 42, 0) // REMW x1,x2,x3
	if cpu.Reg(1) != 42 {
		t.Errorf("REMW x,0: got %d want 42", cpu.Reg(1))
	}
}
func TestREMUW_ByZero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023170BB, 42, 0) // REMUW x1,x2,x3
	if cpu.Reg(1) != 42 {
		t.Errorf("REMUW x,0: got %d want 42", cpu.Reg(1))
	}
}

// setupM builds a 1-instruction machine with x2=a, x3=b, steps it, returns CPU.
func setupM(t *testing.T, mem *GuestMemory, insn uint32, a, b uint64) *CPU {
	t.Helper()
	const codeVA = uint64(0x1000)
	mem.Store32(codeVA, insn)
	mem.Store32(codeVA+4, 0x00100073) // EBREAK
	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.SetReg(2, a)
	cpu.SetReg(3, b)
	if err := cpu.Step(); err != nil && err != ErrEbreak {
		t.Fatalf("Step: %v", err)
	}
	return cpu
}

// ── MULH/MULHSU negative-operand corner cases ─────────────────────────────
// libriscv has incorrect results for MULH/MULHSU with negative rs1, so we
// verify these directly against the RISC-V spec definition.

func TestMULH_NegPos(t *testing.T) {
	// MULH(-1, 2) = upper64(-2 as 128-bit) = 0xFFFFFFFFFFFFFFFF
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023110B3, ^uint64(0), 2) // MULH x1,x2,x3
	if cpu.Reg(1) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("MULH(-1,2): got 0x%016X want 0xFFFFFFFFFFFFFFFF", cpu.Reg(1))
	}
}
func TestMULH_NegNeg(t *testing.T) {
	// MULH(-1, -1) = upper64(1 as 128-bit) = 0
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023110B3, ^uint64(0), ^uint64(0))
	if cpu.Reg(1) != 0 {
		t.Errorf("MULH(-1,-1): got 0x%016X want 0", cpu.Reg(1))
	}
}
func TestMULHSU_NegPos(t *testing.T) {
	// MULHSU(signed(-1), unsigned(2)) = upper64(-2 as 128-bit) = 0xFFFFFFFFFFFFFFFF
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023120B3, ^uint64(0), 2) // MULHSU x1,x2,x3
	if cpu.Reg(1) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("MULHSU(-1,2): got 0x%016X want 0xFFFFFFFFFFFFFFFF", cpu.Reg(1))
	}
}
func TestMULHSU_Large(t *testing.T) {
	// MULHSU(INT_MIN, 2^64-1) = 0x8000000000000000 (see spec math)
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, 0x023120B3, 0x8000000000000000, 0xFFFFFFFFFFFFFFFF)
	if cpu.Reg(1) != 0x8000000000000000 {
		t.Errorf("MULHSU(INT_MIN,MAX): got 0x%016X want 0x8000000000000000", cpu.Reg(1))
	}
}

// ── Zbb instructions not supported by libriscv oracle ─────────────────────
// Verified directly against spec definitions.

func brtD(funct7, funct3, rd, rs1, rs2, opcode int) uint32 {
	return uint32(funct7<<25 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode)
}

func TestRORW_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	// RORW x1, x2, x3: x2=0x10, x3=4 → rorw(16,4) = (16>>4)|(16<<28)&0xFFFFFFFF = 1
	cpu := setupM(t, mem, brtD(0x30, 5, 1, 2, 3, 0x3B), 0x10, 4)
	if cpu.Reg(1) != 1 {
		t.Errorf("RORW: got 0x%X want 1", cpu.Reg(1))
	}
}
func TestCLZ_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 0, 0x33), 0x0001000000000000, 0)
	if cpu.Reg(1) != 15 {
		t.Errorf("CLZ: got %d want 15", cpu.Reg(1))
	}
}
func TestCLZ_Zero_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 0, 0x33), 0, 0)
	if cpu.Reg(1) != 64 {
		t.Errorf("CLZ(0): got %d want 64", cpu.Reg(1))
	}
}
func TestCTZ_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 1, 0x33), 0x100, 0)
	if cpu.Reg(1) != 8 {
		t.Errorf("CTZ: got %d want 8", cpu.Reg(1))
	}
}
func TestCTZ_Zero_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 1, 0x33), 0, 0)
	if cpu.Reg(1) != 64 {
		t.Errorf("CTZ(0): got %d want 64", cpu.Reg(1))
	}
}
func TestCPOP_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 2, 0x33), 0xFF00FF00FF00FF00, 0)
	if cpu.Reg(1) != 32 {
		t.Errorf("CPOP: got %d want 32", cpu.Reg(1))
	}
}
func TestCLZW_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 0, 0x3B), 0x00010000, 0)
	if cpu.Reg(1) != 15 {
		t.Errorf("CLZW: got %d want 15", cpu.Reg(1))
	}
}
func TestCTZW_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 1, 0x3B), 0x100, 0)
	if cpu.Reg(1) != 8 {
		t.Errorf("CTZW: got %d want 8", cpu.Reg(1))
	}
}
func TestCPOPW_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x60, 1, 1, 2, 2, 0x3B), 0xFFFF0000, 0)
	if cpu.Reg(1) != 16 {
		t.Errorf("CPOPW: got %d want 16", cpu.Reg(1))
	}
}
func TestORC_B_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x14, 5, 1, 2, 7, 0x33), 0x00FF0100000000AB, 0)
	want := uint64(0x00FFFF00000000FF)
	if cpu.Reg(1) != want {
		t.Errorf("ORC.B: got 0x%016X want 0x%016X", cpu.Reg(1), want)
	}
}
func TestREV8_Spec(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := setupM(t, mem, brtD(0x35, 5, 1, 2, 24, 0x33), 0x0102030405060708, 0)
	want := uint64(0x0807060504030201)
	if cpu.Reg(1) != want {
		t.Errorf("REV8: got 0x%016X want 0x%016X", cpu.Reg(1), want)
	}
}

// ── RORIW (Zbb) — not supported by libriscv oracle ────────────────────────

func roriw_enc(rd, rs1, shamt int) uint32 {
	return uint32(0x60<<25 | shamt<<20 | rs1<<15 | 5<<12 | rd<<7 | 0x1B)
}

func TestRORIW_Basic(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	// ror32(1, 4) = 0x10000000; sign-extend (bit31=0) = 0x10000000
	cpu := setupM(t, mem, roriw_enc(1, 2, 4), 1, 0)
	if cpu.Reg(1) != 0x10000000 {
		t.Errorf("RORIW(1,4): got 0x%X want 0x10000000", cpu.Reg(1))
	}
}

func TestRORIW_SignExtend(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	// ror32(1, 1) = 0x80000000; sign-extend = 0xFFFFFFFF80000000
	cpu := setupM(t, mem, roriw_enc(1, 2, 1), 1, 0)
	if cpu.Reg(1) != 0xFFFFFFFF80000000 {
		t.Errorf("RORIW(1,1): got 0x%X", cpu.Reg(1))
	}
}

func TestRORIW_Zero(t *testing.T) {
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	// ror32(0xFF, 0) = 0xFF; sign-extend = 0xFF
	cpu := setupM(t, mem, roriw_enc(1, 2, 0), 0xFF, 0)
	if cpu.Reg(1) != 0xFF {
		t.Errorf("RORIW(0xFF,0): got 0x%X want 0xFF", cpu.Reg(1))
	}
}

func TestFflags_NV(t *testing.T) {
	inf := math.Float32frombits(0x7F800000)
	r, fl := fenv.SubF32(inf, inf) // Inf - Inf = NaN, should set NV
	t.Logf("SubF32(Inf,Inf) = 0x%08X flags=0x%02X", math.Float32bits(r), fl)
	if fl&0x10 == 0 {
		t.Errorf("NV not set: flags=0x%02X", fl)
	}
}
