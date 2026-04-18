//go:build !tcc

package riscv

import (
	"testing"
)

// ── scanUsedRegs unit tests ────────────────────────────────────────────

func TestScanUsedRegs_ADD(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	pc := uint64(0x1000)
	// ADD x5, x10, x11
	mem.Store32(pc, 0x00B505B3)
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
	mem.Store32(pc, 0x123453B7)
	// AUIPC x4, 0x1000 (only rd, no rs1/rs2)
	mem.Store32(pc+4, 0x01000217)
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

// ── Encoding helpers (not exported, used by tests) ─────────────────────

func senc(opcode, funct3, rs1, rs2 uint32, imm int64) uint32 {
	immU := uint32(imm) & 0xFFF
	hi := (immU >> 5) & 0x7F
	lo := immU & 0x1F
	return (hi << 25) | (rs2 << 20) | (rs1 << 15) | (funct3 << 12) | (lo << 7) | opcode
}
