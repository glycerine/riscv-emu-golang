//go:build libriscv

package fuzzoracle

// oracle_test.go — unit tests comparing our CPU against libriscv for every
// implemented instruction. Each test runs the instruction in both emulators
// and asserts identical register and memory state after execution.
//
// Memory layout (128KB):
//
//	0x00000..0x0FFFF  code region — ELF PT_LOAD at oracleCodeVA
//	0x10000..0x1FFFF  data region — loads/stores target oracleDataVA

import (
	"testing"

	riscv "riscv"
)

const (
	oracleMemSize = 128 * 1024      // 128KB: smallest power-of-two that fits code at 0x10000
	oracleCodeVA  = uint64(0x10000) // code segment VA
	oracleDataVA  = uint64(0x11000) // data for loads/stores (second page)
)

// regs builds a [32]uint64 from (regnum, value) pairs.
func regs(pairs ...uint64) [32]uint64 {
	var r [32]uint64
	for i := 0; i+1 < len(pairs); i += 2 {
		r[pairs[i]] = pairs[i+1]
	}
	return r
}

// runOne executes insn in both our CPU and libriscv with the given initial
// register state and initMem written at oracleDataVA, then asserts that
// all registers, PC, and full memory are identical.
func runOne(t *testing.T, insn uint32, initRegs [32]uint64, initMem []byte) {
	t.Helper()

	elf := riscv.BuildELF(oracleCodeVA, []uint32{insn, 0x00000073}) // insn + ECALL

	// ── libriscv ─────────────────────────────────────────────────
	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed (run 'make bench-setup' first)")
	}
	defer lm.Close()

	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		if !lm.WriteGuest(oracleDataVA, padded) {
			t.Fatal("libriscv: WriteGuest failed")
		}
	}
	lm.SetRegsAndPC(initRegs, oracleCodeVA)

	// ── our CPU ───────────────────────────────────────────────────
	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	if _, err := riscv.LoadELFBytes(mem, elf); err != nil {
		t.Fatal("LoadELFBytes:", err)
	}
	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		if f := mem.WriteBytes(oracleDataVA, padded); f != nil {
			t.Fatal("WriteBytes:", f)
		}
	}

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	for r := uint8(1); r < 32; r++ {
		cpu.SetReg(r, initRegs[r])
	}

	cpu.Step()
	lm.RunToEcall()

	// Compare all registers
	lRegs := lm.SnapshotRegs()
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lRegs[r])
		}
	}
	// PC not compared: libriscv runs to ECALL halt so its PC is
	// implementation-defined after exit; our CPU stops after one Step().

	// Compare full memory
	lMem := lm.SnapshotMem(0, oracleMemSize)
	ourMem := make([]byte, oracleMemSize)
	if lMem != nil {
		if f := mem.ReadBytes(0, ourMem); f == nil {
			for i := range ourMem {
				if ourMem[i] != lMem[i] {
					t.Errorf("mem[0x%05X]: ours=0x%02X libriscv=0x%02X", i, ourMem[i], lMem[i])
					break
				}
			}
		}
	}
}

// runTwo executes two instructions sequentially in both emulators.
func runTwo(t *testing.T, insn0, insn1 uint32, initRegs [32]uint64, initMem []byte) {
	t.Helper()

	elf := riscv.BuildELF(oracleCodeVA, []uint32{insn0, insn1, 0x00000073})

	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed")
	}
	defer lm.Close()

	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		lm.WriteGuest(oracleDataVA, padded)
	}
	lm.SetRegsAndPC(initRegs, oracleCodeVA)

	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	riscv.LoadELFBytes(mem, elf)
	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		mem.WriteBytes(oracleDataVA, padded)
	}

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	for r := uint8(1); r < 32; r++ {
		cpu.SetReg(r, initRegs[r])
	}

	for range 2 {
		cpu.Step()
		lm.RunToEcall()
	}

	lRegs := lm.SnapshotRegs()
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lRegs[r])
		}
	}
	// PC not compared (see runOne comment).

	lMem := lm.SnapshotMem(0, oracleMemSize)
	ourMem := make([]byte, oracleMemSize)
	if lMem != nil {
		if f := mem.ReadBytes(0, ourMem); f == nil {
			for i := range ourMem {
				if ourMem[i] != lMem[i] {
					t.Errorf("mem[0x%05X]: ours=0x%02X libriscv=0x%02X", i, ourMem[i], lMem[i])
					break
				}
			}
		}
	}
}

// ── ADDI ─────────────────────────────────────────────────────────────────

func TestADDI_Positive(t *testing.T)  { runOne(t, ienc(0x13,0,1,2,1),    regs(2,100), nil) }
func TestADDI_Negative(t *testing.T)  { runOne(t, ienc(0x13,0,1,2,-1),   regs(2,5), nil) }
func TestADDI_Zero(t *testing.T)      { runOne(t, ienc(0x13,0,1,0,42),   regs(), nil) }
func TestADDI_Overflow(t *testing.T)  { runOne(t, ienc(0x13,0,1,2,1),    regs(2,0xFFFFFFFFFFFFFFFF), nil) }
func TestADDI_MaxImm(t *testing.T)    { runOne(t, ienc(0x13,0,1,2,2047), regs(2,0), nil) }
func TestADDI_MinImm(t *testing.T)    { runOne(t, ienc(0x13,0,1,2,-2048),regs(2,0), nil) }

// ── SLTI / SLTIU ─────────────────────────────────────────────────────────

func TestSLTI_True(t *testing.T)          { runOne(t, ienc(0x13,2,1,2,10), regs(2,5), nil) }
func TestSLTI_False(t *testing.T)         { runOne(t, ienc(0x13,2,1,2,10), regs(2,20), nil) }
func TestSLTI_NegLtPos(t *testing.T)      { runOne(t, ienc(0x13,2,1,2,1),  regs(2,0xFFFFFFFFFFFFFFFF), nil) }
func TestSLTIU_True(t *testing.T)         { runOne(t, ienc(0x13,3,1,2,10), regs(2,5), nil) }
func TestSLTIU_BigUnsigned(t *testing.T)  { runOne(t, ienc(0x13,3,1,2,1),  regs(2,0xFFFFFFFFFFFFFFFF), nil) }

// ── XORI / ORI / ANDI ────────────────────────────────────────────────────

func TestXORI(t *testing.T)       { runOne(t, ienc(0x13,4,1,2,0xFF), regs(2,0xAB), nil) }
func TestXORI_Flip(t *testing.T)  { runOne(t, ienc(0x13,4,1,2,-1),   regs(2,0xDEAD), nil) }
func TestORI(t *testing.T)        { runOne(t, ienc(0x13,6,1,2,0x0F), regs(2,0xF0), nil) }
func TestANDI(t *testing.T)       { runOne(t, ienc(0x13,7,1,2,0xFF), regs(2,0xABCD), nil) }
func TestANDI_Zero(t *testing.T)  { runOne(t, ienc(0x13,7,1,2,0),    regs(2,0xFFFF), nil) }

// ── SLLI / SRLI / SRAI ───────────────────────────────────────────────────

func TestSLLI(t *testing.T)           { runOne(t, senc(0x13,1,1,2,3,false),  regs(2,1), nil) }
func TestSLLI_63(t *testing.T)        { runOne(t, senc(0x13,1,1,2,63,false), regs(2,1), nil) }
func TestSRLI(t *testing.T)           { runOne(t, senc(0x13,5,1,2,4,false),  regs(2,0xFF), nil) }
func TestSRAI_Negative(t *testing.T)  { runOne(t, senc(0x13,5,1,2,4,true),   regs(2,0xFFFFFFFFFFFFFFFF), nil) }
func TestSRAI_Positive(t *testing.T)  { runOne(t, senc(0x13,5,1,2,1,true),   regs(2,100), nil) }
func TestSRAI_Shamt35(t *testing.T)   { runOne(t, senc(0x13,5,1,2,35,true),  regs(2,0xC730303030303030), nil) }

// ── ADD / SUB / SLL / SLT / SLTU / XOR / SRL / SRA / OR / AND ───────────

func TestADD(t *testing.T)           { runOne(t, renc(0x33,0,0x00,1,2,3), regs(2,100,3,200), nil) }
func TestADD_Overflow(t *testing.T)  { runOne(t, renc(0x33,0,0x00,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,1), nil) }
func TestSUB(t *testing.T)           { runOne(t, renc(0x33,0,0x20,1,2,3), regs(2,200,3,100), nil) }
func TestSUB_Underflow(t *testing.T) { runOne(t, renc(0x33,0,0x20,1,2,3), regs(2,0,3,1), nil) }
func TestSLL(t *testing.T)           { runOne(t, renc(0x33,1,0x00,1,2,3), regs(2,1,3,4), nil) }
func TestSLT_True(t *testing.T)      { runOne(t, renc(0x33,2,0x00,1,2,3), regs(2,1,3,2), nil) }
func TestSLT_NegLtPos(t *testing.T)  { runOne(t, renc(0x33,2,0x00,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,1), nil) }
func TestSLTU(t *testing.T)          { runOne(t, renc(0x33,3,0x00,1,2,3), regs(2,1,3,2), nil) }
func TestXOR(t *testing.T)           { runOne(t, renc(0x33,4,0x00,1,2,3), regs(2,0xAA,3,0x55), nil) }
func TestSRL(t *testing.T)           { runOne(t, renc(0x33,5,0x00,1,2,3), regs(2,0xFF,3,2), nil) }
func TestSRA_Neg(t *testing.T)       { runOne(t, renc(0x33,5,0x20,1,2,3), regs(2,0xFFFFFFFFFFFFFFFF,3,1), nil) }
func TestOR(t *testing.T)            { runOne(t, renc(0x33,6,0x00,1,2,3), regs(2,0xF0,3,0x0F), nil) }
func TestAND(t *testing.T)           { runOne(t, renc(0x33,7,0x00,1,2,3), regs(2,0xFF,3,0x0F), nil) }

// ── ADDIW / SLLIW / SRLIW / SRAIW ────────────────────────────────────────

func TestADDIW_Pos(t *testing.T)        { runOne(t, ienc(0x1B,0,1,2,1),       regs(2,100), nil) }
func TestADDIW_SignExtend(t *testing.T) { runOne(t, ienc(0x1B,0,1,2,1),       regs(2,0x7FFFFFFF), nil) }
func TestADDIW_Neg(t *testing.T)        { runOne(t, ienc(0x1B,0,1,2,-1),      regs(2,0), nil) }
func TestSLLIW(t *testing.T)            { runOne(t, senc(0x1B,1,1,2,4,false), regs(2,1), nil) }
func TestSRLIW(t *testing.T)            { runOne(t, senc(0x1B,5,1,2,4,false), regs(2,0x80000000), nil) }
func TestSRAIW_Neg(t *testing.T)        { runOne(t, senc(0x1B,5,1,2,4,true),  regs(2,0xFFFFFFFF80000000), nil) }

// ── ADDW / SUBW / SLLW / SRLW / SRAW ─────────────────────────────────────

func TestADDW(t *testing.T)            { runOne(t, renc(0x3B,0,0x00,1,2,3), regs(2,100,3,200), nil) }
func TestADDW_SignExtend(t *testing.T) { runOne(t, renc(0x3B,0,0x00,1,2,3), regs(2,0x7FFFFFFF,3,1), nil) }
func TestSUBW(t *testing.T)            { runOne(t, renc(0x3B,0,0x20,1,2,3), regs(2,200,3,100), nil) }
func TestSLLW(t *testing.T)            { runOne(t, renc(0x3B,1,0x00,1,2,3), regs(2,1,3,4), nil) }
func TestSRLW(t *testing.T)            { runOne(t, renc(0x3B,5,0x00,1,2,3), regs(2,0x80000000,3,1), nil) }
func TestSRAW_Neg(t *testing.T)        { runOne(t, renc(0x3B,5,0x20,1,2,3), regs(2,0xFFFFFFFF80000000,3,1), nil) }

// ── LUI / AUIPC ───────────────────────────────────────────────────────────

func TestLUI(t *testing.T)            { runOne(t, uenc(0x37,1,0x12345), regs(), nil) }
func TestLUI_SignExtend(t *testing.T) { runOne(t, uenc(0x37,1,0x80000), regs(), nil) }
func TestLUI_Max(t *testing.T)        { runOne(t, uenc(0x37,1,0xFFFFF), regs(), nil) }
func TestAUIPCOffset(t *testing.T)    { runOne(t, uenc(0x17,1,0x00000), regs(), nil) }
func TestAUIPCNonZero(t *testing.T)   { runOne(t, uenc(0x17,1,0x00001), regs(), nil) }

// ── BEQ / BNE / BLT / BGE / BLTU / BGEU ─────────────────────────────────

func TestBEQ_Taken(t *testing.T)    { runOne(t, benc(0x63,0,2,3,8), regs(2,42,3,42), nil) }
func TestBEQ_NotTaken(t *testing.T) { runOne(t, benc(0x63,0,2,3,8), regs(2,1,3,2), nil) }
func TestBNE_Taken(t *testing.T)    { runOne(t, benc(0x63,1,2,3,8), regs(2,1,3,2), nil) }
func TestBNE_NotTaken(t *testing.T) { runOne(t, benc(0x63,1,2,3,8), regs(2,7,3,7), nil) }
func TestBLT_Taken(t *testing.T)    { runOne(t, benc(0x63,4,2,3,8), regs(2,0xFFFFFFFFFFFFFFFF,3,1), nil) }
func TestBLT_NotTaken(t *testing.T) { runOne(t, benc(0x63,4,2,3,8), regs(2,2,3,1), nil) }
func TestBGE_Taken(t *testing.T)    { runOne(t, benc(0x63,5,2,3,8), regs(2,5,3,5), nil) }
func TestBGE_NotTaken(t *testing.T) { runOne(t, benc(0x63,5,2,3,8), regs(2,0,3,1), nil) }
func TestBLTU_Taken(t *testing.T)   { runOne(t, benc(0x63,6,2,3,8), regs(2,1,3,2), nil) }
func TestBGEU_Taken(t *testing.T)   { runOne(t, benc(0x63,7,2,3,8), regs(2,5,3,5), nil) }

// ── Loads (x2 = oracleDataVA, result in x1) ───────────────────────────────

func TestLB_Neg(t *testing.T) { runOne(t, lenc(0x03,0,1,2,0), regs(2,oracleDataVA), []byte{0xFF}) }
func TestLB_Pos(t *testing.T) { runOne(t, lenc(0x03,0,1,2,0), regs(2,oracleDataVA), []byte{0x7F}) }
func TestLBU(t *testing.T)    { runOne(t, lenc(0x03,4,1,2,0), regs(2,oracleDataVA), []byte{0xFF}) }
func TestLH_Neg(t *testing.T) { runOne(t, lenc(0x03,1,1,2,0), regs(2,oracleDataVA), []byte{0x00,0x80}) }
func TestLHU(t *testing.T)    { runOne(t, lenc(0x03,5,1,2,0), regs(2,oracleDataVA), []byte{0x00,0x80}) }
func TestLW_Neg(t *testing.T) { runOne(t, lenc(0x03,2,1,2,0), regs(2,oracleDataVA), []byte{0xFF,0xFF,0xFF,0x80}) }
func TestLWU(t *testing.T)    { runOne(t, lenc(0x03,6,1,2,0), regs(2,oracleDataVA), []byte{0xFF,0xFF,0xFF,0x80}) }
func TestLD(t *testing.T)     { runOne(t, lenc(0x03,3,1,2,0), regs(2,oracleDataVA), []byte{0xDE,0xAD,0xBE,0xEF,0xCA,0xFE,0xBA,0xBE}) }

// ── Stores (x2 = oracleDataVA, x3 = value) ────────────────────────────────

func TestSB(t *testing.T)         { runOne(t, lenc(0x23,0,2,3,0), regs(2,oracleDataVA,3,0xAB), make([]byte,8)) }
func TestSH(t *testing.T)         { runOne(t, lenc(0x23,1,2,3,0), regs(2,oracleDataVA,3,0xABCD), make([]byte,8)) }
func TestSW(t *testing.T)         { runOne(t, lenc(0x23,2,2,3,0), regs(2,oracleDataVA,3,0xDEADBEEF), make([]byte,8)) }
func TestSD(t *testing.T)         { runOne(t, lenc(0x23,3,2,3,0), regs(2,oracleDataVA,3,0xDEADBEEFCAFEBABE), make([]byte,8)) }
func TestSB_Masking(t *testing.T) { runOne(t, lenc(0x23,0,2,3,0), regs(2,oracleDataVA,3,0xDEADBEEFCAFEBAFF), make([]byte,8)) }
func TestSW_SignBit(t *testing.T) { runOne(t, lenc(0x23,2,2,3,0), regs(2,oracleDataVA,3,0x80000000), make([]byte,8)) }

// ── Store then load ───────────────────────────────────────────────────────

func TestSD_then_LD(t *testing.T) {
	runTwo(t,
		lenc(0x23, 3, 2, 3, 0), // SD x3, 0(x2)
		lenc(0x03, 3, 4, 2, 0), // LD x4, 0(x2)
		regs(2, oracleDataVA, 3, 0xDEADBEEFCAFEBABE),
		make([]byte, 8),
	)
}
func TestSW_then_LW(t *testing.T) {
	runTwo(t,
		lenc(0x23, 2, 2, 3, 0), // SW x3, 0(x2)
		lenc(0x03, 2, 4, 2, 0), // LW x4, 0(x2)  — sign-extends
		regs(2, oracleDataVA, 3, 0xDEADBEEF),
		make([]byte, 8),
	)
}
func TestSB_then_LBU(t *testing.T) {
	runTwo(t,
		lenc(0x23, 0, 2, 3, 0), // SB x3, 0(x2)
		lenc(0x03, 4, 4, 2, 0), // LBU x4, 0(x2)
		regs(2, oracleDataVA, 3, 0xFF),
		make([]byte, 8),
	)
}

// ── JAL ──────────────────────────────────────────────────────────────────
//
// Code layout for JAL tests (at oracleCodeVA = 0x10000):
//   0x10000: JAL x1, +8      -- rd=x1 gets 0x10004, PC -> 0x10008
//   0x10004: ECALL            -- not executed (jumped over)
//   0x10008: ECALL            -- halts libriscv; our CPU stops after JAL
//
// After execution: x1 == oracleCodeVA+4, all other regs unchanged.

func TestJAL_Forward(t *testing.T) {
	// JAL x1, +8: link=PC+4=0x10004, jump to 0x10008
	runJAL(t, jalenc(1, 8), regs(), oracleCodeVA+4, oracleCodeVA+8)
}

func TestJAL_StoreLink(t *testing.T) {
	// JAL x5, +8: link stored in x5
	runJAL(t, jalenc(5, 8), regs(), oracleCodeVA+4, oracleCodeVA+8)
}

func TestJAL_x0_NoLink(t *testing.T) {
	// JAL x0, +8: rd=x0 means link discarded, x0 stays 0
	runJAL(t, jalenc(0, 8), regs(), 0, oracleCodeVA+8)
}

func TestJAL_LargeOffset(t *testing.T) {
	// JAL x1, +0x1000: jump into data region
	// libriscv will fault there; we only care that x1=PC+4 before jump
	// So we skip the memory/PC oracle and just check link register.
	runJALLinkOnly(t, jalenc(1, 0x1000), regs(), oracleCodeVA+4)
}

// runJAL builds a 3-instruction ELF:
//   [0] JAL insn
//   [1] ECALL (skipped if taken)
//   [2] ECALL (halt for libriscv)
// Asserts wantLink in the rd register and wantPC as where libriscv ends up.
// Our CPU stops after the JAL so we check x1 only.
func runJAL(t *testing.T, insn uint32, initRegs [32]uint64, wantLink, wantPC uint64) {
	t.Helper()

	rd := uint8((insn >> 7) & 0x1F)

	elf := riscv.BuildELF(oracleCodeVA, []uint32{
		insn,
		0x00000073, // ECALL — skipped by forward jump
		0x00000073, // ECALL — halt for libriscv at oracleCodeVA+8
	})

	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed")
	}
	defer lm.Close()
	lm.SetRegsAndPC(initRegs, oracleCodeVA)

	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	riscv.LoadELFBytes(mem, elf)
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	for r := uint8(1); r < 32; r++ {
		cpu.SetReg(r, initRegs[r])
	}

	cpu.Step()        // execute JAL only
	lm.RunToEcall()  // runs to halt

	lRegs := lm.SnapshotRegs()

	// Link register must match
	if wantLink != 0 {
		if cpu.Reg(rd) != wantLink {
			t.Errorf("x%d (link): ours=0x%016X want=0x%016X", rd, cpu.Reg(rd), wantLink)
		}
		if lRegs[rd] != wantLink {
			t.Errorf("x%d (link) libriscv=0x%016X want=0x%016X", rd, lRegs[rd], wantLink)
		}
	}
	// All registers must agree between our CPU and libriscv
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lRegs[r])
		}
	}
}

func runJALLinkOnly(t *testing.T, insn uint32, initRegs [32]uint64, wantLink uint64) {
	t.Helper()
	rd := uint8((insn >> 7) & 0x1F)

	// Only check our CPU's link register — don't run libriscv since
	// the jump target may be outside the code region.
	elf := riscv.BuildELF(oracleCodeVA, []uint32{insn, 0x00000073})
	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	riscv.LoadELFBytes(mem, elf)
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	for r := uint8(1); r < 32; r++ {
		cpu.SetReg(r, initRegs[r])
	}
	cpu.Step()
	if cpu.Reg(rd) != wantLink {
		t.Errorf("x%d (link): ours=0x%016X want=0x%016X", rd, cpu.Reg(rd), wantLink)
	}
}

// ── JALR ─────────────────────────────────────────────────────────────────
//
// Code layout for JALR tests (at oracleCodeVA = 0x10000):
//   0x10000: JALR x1, x2, imm   -- rd=x1 gets 0x10004, PC -> (x2+imm)&~1
//   0x10004: ECALL               -- skipped if JALR jumps forward past it
//   0x10008: ECALL               -- halt for libriscv
//
// x2 is pre-set to oracleCodeVA so JALR x1, x2, 8 -> PC = 0x10008.

func TestJALR_Forward(t *testing.T) {
	// JALR x1, x2, 8: target = (oracleCodeVA+8)&~1 = 0x10008
	runOne(t, jalrenc(1, 2, 8), regs(2, oracleCodeVA), nil)
}

func TestJALR_LinkValue(t *testing.T) {
	// x1 must receive PC+4 = 0x10004
	elf := riscv.BuildELF(oracleCodeVA, []uint32{
		jalrenc(1, 2, 8), // JALR x1, x2, 8
		0x00000073,       // ECALL (skipped)
		0x00000073,       // ECALL (halt)
	})
	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed")
	}
	defer lm.Close()
	initRegs := regs(2, oracleCodeVA)
	lm.SetRegsAndPC(initRegs, oracleCodeVA)

	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	riscv.LoadELFBytes(mem, elf)
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	cpu.SetReg(2, oracleCodeVA)

	cpu.Step()
	lm.RunToEcall()
	lRegs := lm.SnapshotRegs()

	if cpu.Reg(1) != oracleCodeVA+4 {
		t.Errorf("x1 (link) ours=0x%016X want=0x%016X", cpu.Reg(1), oracleCodeVA+4)
	}
	if lRegs[1] != oracleCodeVA+4 {
		t.Errorf("x1 (link) libriscv=0x%016X want=0x%016X", lRegs[1], oracleCodeVA+4)
	}
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lRegs[r])
		}
	}
}

func TestJALR_AlignMask(t *testing.T) {
	// JALR clears bit 0 of the target address (spec requirement).
	// x2 = oracleCodeVA+1 (odd), imm=7 -> target = (oracleCodeVA+8)&~1 = 0x10008
	elf := riscv.BuildELF(oracleCodeVA, []uint32{
		jalrenc(1, 2, 7),
		0x00000073,
		0x00000073,
	})
	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed")
	}
	defer lm.Close()
	initRegs := regs(2, oracleCodeVA+1)
	lm.SetRegsAndPC(initRegs, oracleCodeVA)

	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	riscv.LoadELFBytes(mem, elf)
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	cpu.SetReg(2, oracleCodeVA+1)

	cpu.Step()
	lm.RunToEcall()
	lRegs := lm.SnapshotRegs()

	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lRegs[r])
		}
	}
}

func TestJALR_NegativeImm(t *testing.T) {
	// JALR x1, x2, -4: target = (oracleCodeVA+8 - 4)&~1 = 0x10004 (ECALL)
	// libriscv halts at 0x10004; our CPU stops after JALR.
	// Both should agree on register state (x1 = oracleCodeVA+4).
	runOne(t, jalrenc(1, 2, -4), regs(2, oracleCodeVA+8), nil)
}

func TestJALR_x0_NoLink(t *testing.T) {
	// JALR x0, x2, 8: rd=x0, link discarded, x0 stays 0
	runOne(t, jalrenc(0, 2, 8), regs(2, oracleCodeVA), nil)
}
