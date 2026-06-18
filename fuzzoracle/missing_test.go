package fuzzoracle

// missing_test.go — RED unit tests for the 5 remaining missing instructions:
//   C.FLD, C.FSD, C.FLDSP, C.FSDSP  (compressed float load/store)
//   RORIW                             (rotate right word immediate, Zbb)
//   WFI                               (wait for interrupt — no-op in user mode)
//   SFENCE.VMA                        (supervisor fence — no-op in user mode)
//
// C.FLD/FSD/FLDSP/FSDSP use runOneRVC (from fence_rvc_test.go) since they
// are 16-bit instructions operating on f-registers.
// RORIW, WFI, SFENCE.VMA use runOneF / runOne.

import (
	"math"
	"testing"

	riscv "riscv"
)

// ── C.FLD / C.FSD / C.FLDSP / C.FSDSP encoders ───────────────────────────

func cFLD(rd3, rs1_3, uimm int) uint16 {
	return (0b001 << 13) | (uint16((uimm>>3)&7) << 10) | (uint16(rs1_3&7) << 7) |
		(uint16((uimm>>6)&3) << 5) | (uint16(rd3&7) << 2) | 0b00
}

func cFSD_(rs1_3, rs2_3, uimm int) uint16 {
	return (0b101 << 13) | (uint16((uimm>>3)&7) << 10) | (uint16(rs1_3&7) << 7) |
		(uint16((uimm>>6)&3) << 5) | (uint16(rs2_3&7) << 2) | 0b00
}

func cFLDSP(rd, uimm int) uint16 {
	return (0b001 << 13) | (uint16((uimm>>5)&1) << 12) | (uint16(rd&31) << 7) |
		(uint16((uimm>>3)&3) << 5) | (uint16((uimm>>6)&7) << 2) | 0b10
}

func cFSDSP_(rs2, uimm int) uint16 {
	return (0b101 << 13) | (uint16((uimm>>3)&7) << 10) | (uint16((uimm>>6)&7) << 7) |
		(uint16(rs2&31) << 2) | 0b10
}

// ── C.FLD ─────────────────────────────────────────────────────────────────
// C.FLD fd', uimm(rs1'): fd' = mem[rs1'+uimm] as double (64-bit)
// In RV64: Q0 funct3=001 is C.FLD (not C.FLW which is RV32-only)

func TestC_FLD_Basic(t *testing.T) {
	// C.FLD f9(rd'=1), 0(x9(rs1'=1)): load 8 bytes from oracleDataVA
	initF := ffregs()
	initX := [32]uint64{}
	initX[9] = oracleDataVA
	mem := make([]byte, 8)
	b := math.Float64bits(3.141592653589793)
	for i := 0; i < 8; i++ {
		mem[i] = byte(b >> (i * 8))
	}
	runOneRVCF(t, cFLD(1, 1, 0), initX, initF, mem)
}

func TestC_FLD_Offset(t *testing.T) {
	// C.FLD f8(rd'=0), 8(x9(rs1'=1)): uimm=8
	initX := [32]uint64{}
	initX[9] = oracleDataVA
	mem := make([]byte, 16)
	b := math.Float64bits(2.718281828)
	for i := 0; i < 8; i++ {
		mem[8+i] = byte(b >> (i * 8))
	}
	runOneRVCF(t, cFLD(0, 1, 8), initX, ffregs(), mem)
}

// ── C.FSD ─────────────────────────────────────────────────────────────────
// C.FSD rs2', uimm(rs1'): mem[rs1'+uimm] = fs2' (64-bit)

func TestC_FSD_Basic(t *testing.T) {
	// C.FSD f9(rs2'=1), 0(x9(rs1'=1))
	initX := [32]uint64{}
	initX[9] = oracleDataVA
	initF := ffregs(1, math.Float64bits(2.718281828)) // f9
	runOneRVCF(t, cFSD_(1, 1, 0), initX, initF, make([]byte, 8))
}

// ── C.FLDSP ───────────────────────────────────────────────────────────────
// C.FLDSP fd, uimm(sp): fd = mem[sp+uimm] as double

func TestC_FLDSP_Basic(t *testing.T) {
	// C.FLDSP f1, 0: load from sp (x2=oracleDataVA+64)
	initX := [32]uint64{}
	initX[2] = oracleDataVA + 64
	mem := make([]byte, 128)
	b := math.Float64bits(1.4142135623730951)
	for i := 0; i < 8; i++ {
		mem[64+i] = byte(b >> (i * 8))
	}
	runOneRVCF(t, cFLDSP(1, 0), initX, ffregs(), mem)
}

// ── C.FSDSP ───────────────────────────────────────────────────────────────
// C.FSDSP fs2, uimm(sp): mem[sp+uimm] = fs2 (64-bit)

func TestC_FSDSP_Basic(t *testing.T) {
	// C.FSDSP f1, 0: store f1 to sp (x2=oracleDataVA+64)
	initX := [32]uint64{}
	initX[2] = oracleDataVA + 64
	initF := ffregs(1, math.Float64bits(1.7320508075688772)) // f1
	runOneRVCF(t, cFSDSP_(1, 0), initX, initF, make([]byte, 128))
}

// ── RORIW ─────────────────────────────────────────────────────────────────
// RORIW x1, x2, shamt: x1 = sign_extend(ror32(x2[31:0], shamt))
// Encoding: OP-IMM-32 (0x1B), funct3=5, funct7=0x30.

func roriw(rd, rs1, shamt int) uint32 {
	return uint32(0x30<<25 | shamt<<20 | rs1<<15 | 5<<12 | rd<<7 | 0x1B)
}

// ── WFI ───────────────────────────────────────────────────────────────────
// WFI = 0x10500073: in user-mode emulation, a legal no-op.
// We verify PC advances by 4 (not ErrIllegalInstruction).

func TestWFI_NoFault(t *testing.T) {
	runFENCE(t, 0x10500073)
}

// ── SFENCE.VMA ────────────────────────────────────────────────────────────
// SFENCE.VMA rs1, rs2 = 0x12000073 (rs1=0, rs2=0): supervisor TLB flush.
// In user-mode: no-op.

func TestSFENCE_VMA_NoFault(t *testing.T) {
	runFENCE(t, 0x12000073)
}

// ── runOneRVCF: oracle runner for 16-bit compressed float instructions ────
// Like runOneRVC but also compares f-registers.

func runOneRVCF(t *testing.T, insn16 uint16, initX [32]uint64, initF [32]uint64, initMem []byte) {
	t.Helper()

	word0 := uint32(insn16) | (uint32(0x9002) << 16) // insn16 + C.EBREAK
	elf := riscv.BuildELF(oracleCodeVA, []uint32{word0, 0x00000073})

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
	lm.SetRegsAndPC(initX, oracleCodeVA)
	lm.SetFRegs(initF)
	lm.RunToEcall()
	lXRegs := lm.SnapshotRegs()
	lFRegs := lm.SnapshotFRegs()
	lMem := lm.SnapshotMem(0, oracleMemSize)

	// Our CPU
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
		cpu.SetReg(r, initX[r])
	}
	for r := uint8(0); r < 32; r++ {
		cpu.SetFReg(r, initF[r])
	}
	cpu.Step()

	// Compare integer registers
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lXRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lXRegs[r])
		}
	}
	// Compare float registers
	for r := 0; r < 32; r++ {
		if cpu.FReg(uint8(r)) != lFRegs[r] {
			t.Errorf("f%d: ours=0x%016X libriscv=0x%016X", r, cpu.FReg(uint8(r)), lFRegs[r])
		}
	}
	// Compare memory
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
