package fuzzoracle

// float_oracle_test.go — runOneF helper for F+D instruction oracle tests.
// Separated from oracle_test.go so float imports are isolated.

import (
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

// runOneF executes one floating-point instruction in both our CPU and
// libriscv, then compares all integer registers, all float registers,
// and full memory.
//
// initX  — initial x0..x31 values (x2 should point at oracleDataVA for loads/stores)
// initF  — initial f0..f31 values as raw uint64 (NaN-boxed float32 or raw float64)
// initMem — bytes written at oracleDataVA before execution
func runOneF(t *testing.T, insn uint32, initX [32]uint64, initF [32]uint64, initMem []byte) {
	t.Helper()

	elf := riscv.BuildELF(oracleCodeVA, []uint32{insn, 0x00000073})

	// ── libriscv ─────────────────────────────────────────────────
	lm := NewMachine(elf)
	if lm == nil {
		t.Fatal("libriscv: NewMachine failed (run 'make bench-setup' first)")
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
	lFRegs := lm.SnapshotFRegs() // [32]uint64 — raw bits per register
	lMem   := lm.SnapshotMem(0, oracleMemSize)

	// ── our CPU ───────────────────────────────────────────────────
	mem, err := riscv.NewGuestMemory(oracleMemSize)
	if err != nil { t.Fatal(err) }
	defer mem.Free()

	riscv.LoadELFBytes(mem, elf)
	if len(initMem) > 0 {
		padded := make([]byte, 128)
		copy(padded, initMem)
		mem.WriteBytes(oracleDataVA, padded)
	}

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(oracleCodeVA)
	for r := uint8(1); r < 32; r++ { cpu.SetReg(r, initX[r]) }
	for r := uint8(0); r < 32; r++ { cpu.SetFReg(r, initF[r]) }

	cpu.Step()

	// Compare integer registers
	for r := 0; r < 32; r++ {
		if cpu.Reg(uint8(r)) != lXRegs[r] {
			t.Errorf("x%d: ours=0x%016X libriscv=0x%016X", r, cpu.Reg(uint8(r)), lXRegs[r])
		}
	}

	// Compare float registers (full 64-bit raw value)
	for r := 0; r < 32; r++ {
		ours := cpu.FReg(uint8(r))
		lib  := lFRegs[r]
		if ours != lib {
			t.Errorf("f%d: ours=0x%016X libriscv=0x%016X", r, ours, lib)
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
