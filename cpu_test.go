package riscv

import (
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
