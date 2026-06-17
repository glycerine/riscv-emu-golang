package riscv

import "testing"

func TestCPU_RuntimeAtomicXchg8Sequence(t *testing.T) {
	const (
		codeVA = uint64(0x10000)
		dataVA = uint64(0x20000)
	)
	insns := []uint32{
		ienc(opOPIMM, 7, 12, 10, 3),                 // ANDI x12, x10, 3
		ienc(opOPIMM, 1, 12, 12, 3),                 // SLLI x12, x12, 3
		ienc(opOPIMM, 0, 14, 0, 255),                // ADDI x14, x0, 255
		renc(opOP, 1, 0, 14, 14, 12),                // SLL x14, x14, x12
		ienc(opOPIMM, 4, 14, 14, -1),                // XORI x14, x14, -1
		ienc(opOPIMM, 7, 10, 10, -4),                // ANDI x10, x10, -4
		renc(opOP, 1, 0, 11, 11, 12),                // SLL x11, x11, x12
		amoenc(amoFunct5LR, amoFunct3W, 15, 10, 0),  // LR.W x15, (x10)
		renc(opOP, 7, 0, 13, 15, 14),                // AND x13, x15, x14
		renc(opOP, 6, 0, 13, 13, 11),                // OR x13, x13, x11
		amoenc(amoFunct5SC, amoFunct3W, 16, 10, 13), // SC.W x16, x13, (x10)
		benc(opBRANCH, 1, 16, 0, -16),               // BNE x16, x0, LR.W
		renc(opOP, 5, 0, 15, 15, 12),                // SRL x15, x15, x12
		ienc(opOPIMM, 7, 15, 15, 255),               // ANDI x15, x15, 255
	}

	tests := []struct {
		initial uint32
		offset  uint64
		oldByte uint64
		wantMem uint32
	}{
		{initial: 0x11223344, offset: 0, oldByte: 0x44, wantMem: 0x112233aa},
		{initial: 0x11223344, offset: 1, oldByte: 0x33, wantMem: 0x1122aa44},
		{initial: 0x11223344, offset: 2, oldByte: 0x22, wantMem: 0x11aa3344},
		{initial: 0x11223344, offset: 3, oldByte: 0x11, wantMem: 0xaa223344},
		{initial: 0x81223344, offset: 0, oldByte: 0x44, wantMem: 0x812233aa},
		{initial: 0x81223344, offset: 3, oldByte: 0x81, wantMem: 0xaa223344},
	}
	for _, mode := range []string{"step", "cached"} {
		for _, tt := range tests {
			t.Run(mode, func(t *testing.T) {
				cpu, mem := newTestCPU(t, Size1MB, codeVA, insns)
				defer mem.Free()
				mustStore32AMO(t, mem, dataVA, tt.initial)
				cpu.SetReg(10, dataVA+tt.offset)
				cpu.SetReg(11, 0xaa)

				endPC := codeVA + uint64(len(insns))*4
				switch mode {
				case "step":
					const maxSteps = 64
					for step := 0; step < maxSteps && cpu.PC() != endPC; step++ {
						if err := cpu.Step(); err != nil {
							t.Fatalf("step %d pc=0x%x: %v", step, cpu.PC(), err)
						}
					}
				case "cached":
					cache := NewDecoderCache(codeVA, 4096)
					res, err := runCachedBudget(cpu, cache, &cpu.Notes, uint64(len(insns)))
					if err != nil {
						t.Fatalf("runCachedBudget: %v", err)
					}
					if res != RunBudgetExpired {
						t.Fatalf("runCachedBudget result = %v, want %v", res, RunBudgetExpired)
					}
				}
				if cpu.PC() != endPC {
					t.Fatalf("program did not finish, pc=0x%x", cpu.PC())
				}
				if got := cpu.Reg(15); got != tt.oldByte {
					t.Fatalf("old byte = 0x%x, want 0x%x", got, tt.oldByte)
				}
				if got := mustLoad32AMO(t, mem, dataVA); got != tt.wantMem {
					t.Fatalf("word after xchg8 = 0x%x, want 0x%x", got, tt.wantMem)
				}
				if cpu.resvValid {
					t.Fatalf("xchg8 sequence left reservation valid")
				}
			})
		}
	}
}
