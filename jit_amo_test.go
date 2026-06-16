package riscv

import "testing"

const (
	opAMONative = uint32(0x2F)

	amoFunct3W = uint32(0b010)
	amoFunct3D = uint32(0b011)

	amoFunct5Add  = uint32(0b00000)
	amoFunct5Swap = uint32(0b00001)
	amoFunct5LR   = uint32(0b00010)
	amoFunct5SC   = uint32(0b00011)
	amoFunct5OR   = uint32(0b01000)
	amoFunct5Min  = uint32(0b10000)
	amoFunct5MaxU = uint32(0b11100)
)

func amoenc(funct5, funct3, rd, rs1, rs2 uint32) uint32 {
	return funct5<<27 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opAMONative
}

func runJITAMOProgram(t *testing.T, insns []uint32, setup func(*CPU, *GuestMemory)) (*CPU, *GuestMemory, *JIT, error) {
	t.Helper()
	const codeVA = uint64(0x10000)
	cpu, mem := newTestCPU(t, Size1MB, codeVA, insns)
	cpu.Notes.Push(ecallStop)
	defer cpu.Notes.Pop()
	if setup != nil {
		setup(cpu, mem)
	}
	jit := NewJIT()
	err := jit.RunJIT(cpu)
	return cpu, mem, jit, err
}

func mustStore32AMO(t *testing.T, mem *GuestMemory, addr uint64, v uint32) {
	t.Helper()
	if f := mem.Store32(addr, v); f != nil {
		t.Fatalf("Store32(0x%x): %v", addr, f)
	}
}

func mustLoad32AMO(t *testing.T, mem *GuestMemory, addr uint64) uint32 {
	t.Helper()
	v, f := mem.Load32(addr)
	if f != nil {
		t.Fatalf("Load32(0x%x): %v", addr, f)
	}
	return v
}

func mustStore64AMO(t *testing.T, mem *GuestMemory, addr uint64, v uint64) {
	t.Helper()
	if f := mem.Store64(addr, v); f != nil {
		t.Fatalf("Store64(0x%x): %v", addr, f)
	}
}

func mustLoad64AMO(t *testing.T, mem *GuestMemory, addr uint64) uint64 {
	t.Helper()
	v, f := mem.Load64(addr)
	if f != nil {
		t.Fatalf("Load64(0x%x): %v", addr, f)
	}
	return v
}

func requireNativeAMO(t *testing.T, jit *JIT) {
	t.Helper()
	if jit.DispatchInterp != 0 {
		t.Fatalf("DispatchInterp = %d, want 0 native AMO/LR/SC fallbacks", jit.DispatchInterp)
	}
}

func TestJIT_AMOOrdinaryOpsNative(t *testing.T) {
	const dataVA = uint64(0x20000)
	tests := []struct {
		name    string
		funct5  uint32
		funct3  uint32
		old     uint64
		src     uint64
		wantMem uint64
		wantRD  uint64
	}{
		{
			name:    "amoadd.w",
			funct5:  amoFunct5Add,
			funct3:  amoFunct3W,
			old:     0x7fffffff,
			src:     2,
			wantMem: 0x80000001,
			wantRD:  0x7fffffff,
		},
		{
			name:    "amoswap.d",
			funct5:  amoFunct5Swap,
			funct3:  amoFunct3D,
			old:     0x1122334455667788,
			src:     0xfeedfacecafebeef,
			wantMem: 0xfeedfacecafebeef,
			wantRD:  0x1122334455667788,
		},
		{
			name:    "amoor.w",
			funct5:  amoFunct5OR,
			funct3:  amoFunct3W,
			old:     0x1000,
			src:     0x0f,
			wantMem: 0x100f,
			wantRD:  0x1000,
		},
		{
			name:    "amomin.w-signed",
			funct5:  amoFunct5Min,
			funct3:  amoFunct3W,
			old:     0xfffffff0,
			src:     5,
			wantMem: 0xfffffff0,
			wantRD:  0xfffffffffffffff0,
		},
		{
			name:    "amomaxu.d",
			funct5:  amoFunct5MaxU,
			funct3:  amoFunct3D,
			old:     5,
			src:     9,
			wantMem: 9,
			wantRD:  5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			insns := []uint32{
				amoenc(tt.funct5, tt.funct3, 12, 10, 11),
				instrECALL,
			}
			cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
				cpu.SetReg(10, dataVA)
				cpu.SetReg(11, tt.src)
				if tt.funct3 == amoFunct3W {
					mustStore32AMO(t, mem, dataVA, uint32(tt.old))
				} else {
					mustStore64AMO(t, mem, dataVA, tt.old)
				}
				cpu.resvAddr = dataVA
				cpu.resvValid = true
			})
			defer mem.Free()
			defer jit.Close()

			if err != ErrEcall {
				t.Fatalf("RunJIT err = %v, want ErrEcall", err)
			}
			requireNativeAMO(t, jit)
			if tt.funct3 == amoFunct3W {
				if got := mustLoad32AMO(t, mem, dataVA); got != uint32(tt.wantMem) {
					t.Fatalf("mem32 = 0x%x, want 0x%x", got, uint32(tt.wantMem))
				}
			} else if got := mustLoad64AMO(t, mem, dataVA); got != tt.wantMem {
				t.Fatalf("mem64 = 0x%x, want 0x%x", got, tt.wantMem)
			}
			if got := cpu.Reg(12); got != tt.wantRD {
				t.Fatalf("rd = 0x%x, want 0x%x", got, tt.wantRD)
			}
			if cpu.resvValid {
				t.Fatalf("ordinary AMO left reservation valid")
			}
		})
	}
}

func TestJIT_LRSCGeneralSuccessNative(t *testing.T) {
	const dataVA = uint64(0x20000)
	tests := []struct {
		name      string
		funct3    uint32
		old       uint64
		src       uint64
		wantLR    uint64
		wantMem64 uint64
		wantMem32 uint32
	}{
		{
			name:      "lrsc.w",
			funct3:    amoFunct3W,
			old:       0xfffffff0,
			src:       0x12345678,
			wantLR:    0xfffffffffffffff0,
			wantMem32: 0x12345678,
		},
		{
			name:      "lrsc.d",
			funct3:    amoFunct3D,
			old:       0x1122334455667788,
			src:       0xaabbccddeeff0011,
			wantLR:    0x1122334455667788,
			wantMem64: 0xaabbccddeeff0011,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			insns := []uint32{
				amoenc(amoFunct5LR, tt.funct3, 12, 10, 0),
				ienc(opOPIMM, 0, 0, 0, 0), // NOP: keep this on the general, unfused path.
				amoenc(amoFunct5SC, tt.funct3, 13, 10, 11),
				instrECALL,
			}
			cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
				cpu.SetReg(10, dataVA)
				cpu.SetReg(11, tt.src)
				if tt.funct3 == amoFunct3W {
					mustStore32AMO(t, mem, dataVA, uint32(tt.old))
				} else {
					mustStore64AMO(t, mem, dataVA, tt.old)
				}
			})
			defer mem.Free()
			defer jit.Close()

			if err != ErrEcall {
				t.Fatalf("RunJIT err = %v, want ErrEcall", err)
			}
			requireNativeAMO(t, jit)
			if got := cpu.Reg(12); got != tt.wantLR {
				t.Fatalf("LR rd = 0x%x, want 0x%x", got, tt.wantLR)
			}
			if got := cpu.Reg(13); got != 0 {
				t.Fatalf("SC rd = %d, want success 0", got)
			}
			if tt.funct3 == amoFunct3W {
				if got := mustLoad32AMO(t, mem, dataVA); got != tt.wantMem32 {
					t.Fatalf("mem32 = 0x%x, want 0x%x", got, tt.wantMem32)
				}
			} else if got := mustLoad64AMO(t, mem, dataVA); got != tt.wantMem64 {
				t.Fatalf("mem64 = 0x%x, want 0x%x", got, tt.wantMem64)
			}
			if cpu.resvValid {
				t.Fatalf("successful SC left reservation valid")
			}
		})
	}
}

func TestJIT_SCFailsWithoutReservationNative(t *testing.T) {
	const dataVA = uint64(0x20000)
	insns := []uint32{
		amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
		instrECALL,
	}
	cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
		cpu.SetReg(10, dataVA)
		cpu.SetReg(11, 0xaaaaaaaaaaaaaaaa)
		mustStore64AMO(t, mem, dataVA, 0x1111222233334444)
	})
	defer mem.Free()
	defer jit.Close()

	if err != ErrEcall {
		t.Fatalf("RunJIT err = %v, want ErrEcall", err)
	}
	requireNativeAMO(t, jit)
	if got := cpu.Reg(13); got != 1 {
		t.Fatalf("SC rd = %d, want failure 1", got)
	}
	if got := mustLoad64AMO(t, mem, dataVA); got != 0x1111222233334444 {
		t.Fatalf("mem64 = 0x%x, want unchanged", got)
	}
	if cpu.resvValid {
		t.Fatalf("failed SC left reservation valid")
	}
}

func TestJIT_AMOClearsReservationBeforeSC(t *testing.T) {
	const dataVA = uint64(0x20000)
	insns := []uint32{
		amoenc(amoFunct5LR, amoFunct3D, 12, 10, 0),
		amoenc(amoFunct5Add, amoFunct3D, 13, 10, 11),
		amoenc(amoFunct5SC, amoFunct3D, 14, 10, 11),
		instrECALL,
	}
	cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
		cpu.SetReg(10, dataVA)
		cpu.SetReg(11, 3)
		mustStore64AMO(t, mem, dataVA, 10)
	})
	defer mem.Free()
	defer jit.Close()

	if err != ErrEcall {
		t.Fatalf("RunJIT err = %v, want ErrEcall", err)
	}
	requireNativeAMO(t, jit)
	if got := cpu.Reg(12); got != 10 {
		t.Fatalf("LR rd = %d, want 10", got)
	}
	if got := cpu.Reg(13); got != 10 {
		t.Fatalf("AMO rd = %d, want old value 10", got)
	}
	if got := cpu.Reg(14); got != 1 {
		t.Fatalf("SC rd = %d, want failure 1 after AMO clears reservation", got)
	}
	if got := mustLoad64AMO(t, mem, dataVA); got != 13 {
		t.Fatalf("mem64 = %d, want AMO result 13", got)
	}
	if cpu.resvValid {
		t.Fatalf("failed SC left reservation valid")
	}
}

func TestJIT_LRSCFusionSuccessAndAlias(t *testing.T) {
	const dataVA = uint64(0x20000)
	insns := []uint32{
		amoenc(amoFunct5LR, amoFunct3D, 12, 10, 0),
		amoenc(amoFunct5SC, amoFunct3D, 11, 10, 11), // SC.rd == SC.rs2: store original x11, then write success.
		instrECALL,
	}
	cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
		cpu.SetReg(10, dataVA)
		cpu.SetReg(11, 0xaabbccddeeff0011)
		mustStore64AMO(t, mem, dataVA, 0x1122334455667788)
	})
	defer mem.Free()
	defer jit.Close()

	if err != ErrEcall {
		t.Fatalf("RunJIT err = %v, want ErrEcall", err)
	}
	requireNativeAMO(t, jit)
	if got := cpu.Reg(12); got != 0x1122334455667788 {
		t.Fatalf("LR rd = 0x%x, want old memory value", got)
	}
	if got := cpu.Reg(11); got != 0 {
		t.Fatalf("aliased SC rd = 0x%x, want success 0", got)
	}
	if got := mustLoad64AMO(t, mem, dataVA); got != 0xaabbccddeeff0011 {
		t.Fatalf("mem64 = 0x%x, want original SC source value", got)
	}
}

func TestJIT_LRSCNoFusionWhenLRClobbersBase(t *testing.T) {
	const (
		dataVA  = uint64(0x20000)
		otherVA = uint64(0x21000)
	)
	insns := []uint32{
		amoenc(amoFunct5LR, amoFunct3D, 10, 10, 0), // LR.rd == rs1 must not fuse.
		amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
		instrECALL,
	}
	cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
		cpu.SetReg(10, dataVA)
		cpu.SetReg(11, 0xbbbb)
		mustStore64AMO(t, mem, dataVA, otherVA)
		mustStore64AMO(t, mem, otherVA, 0xaaaa)
	})
	defer mem.Free()
	defer jit.Close()

	if err != ErrEcall {
		t.Fatalf("RunJIT err = %v, want ErrEcall", err)
	}
	requireNativeAMO(t, jit)
	if got := cpu.Reg(10); got != otherVA {
		t.Fatalf("LR clobbered base to 0x%x, want 0x%x", got, otherVA)
	}
	if got := cpu.Reg(13); got != 1 {
		t.Fatalf("SC rd = %d, want failure 1 after address changed", got)
	}
	if got := mustLoad64AMO(t, mem, dataVA); got != otherVA {
		t.Fatalf("mem[dataVA] = 0x%x, want unchanged 0x%x", got, otherVA)
	}
	if got := mustLoad64AMO(t, mem, otherVA); got != 0xaaaa {
		t.Fatalf("mem[otherVA] = 0x%x, want unchanged 0xaaaa", got)
	}
}

func countReservationOffsetOps(res *emitResult, op IROp, offset int64) int {
	if res == nil || res.block == nil {
		return 0
	}
	count := 0
	for _, ins := range res.block.Instrs {
		if ins.Op == op && ins.A == VRXBase && ins.Imm == offset {
			count++
		}
	}
	return count
}

func TestJIT_LRSCFusionIRShape(t *testing.T) {
	const codeVA = uint64(0x10000)
	insns := []uint32{
		amoenc(amoFunct5LR, amoFunct3D, 12, 10, 0),
		amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
		instrECALL,
	}
	_, mem := newTestCPU(t, Size1MB, codeVA, insns)
	defer mem.Free()

	jit := NewJIT()
	defer jit.Close()
	res := jit.emitBlock(mem, codeVA)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	if stores := countReservationOffsetOps(res, IRStore, abjitStateResvAddrOffset); stores != 0 {
		t.Fatalf("fused LR/SC emitted %d ResvAddr stores, want 0", stores)
	}
	if loads := countReservationOffsetOps(res, IRLoad, abjitStateResvAddrOffset); loads != 0 {
		t.Fatalf("fused LR/SC emitted %d ResvAddr loads, want 0", loads)
	}
}

func TestJIT_LRSCFusionGuardsIRShape(t *testing.T) {
	const codeVA = uint64(0x10000)
	tests := []struct {
		name  string
		insns []uint32
	}{
		{
			name: "lr-rd-clobbers-base",
			insns: []uint32{
				amoenc(amoFunct5LR, amoFunct3D, 10, 10, 0),
				amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
				instrECALL,
			},
		},
		{
			name: "mixed-width",
			insns: []uint32{
				amoenc(amoFunct5LR, amoFunct3W, 12, 10, 0),
				amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
				instrECALL,
			},
		},
		{
			name: "different-base",
			insns: []uint32{
				amoenc(amoFunct5LR, amoFunct3D, 12, 10, 0),
				amoenc(amoFunct5SC, amoFunct3D, 13, 11, 12),
				instrECALL,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, mem := newTestCPU(t, Size1MB, codeVA, tt.insns)
			defer mem.Free()
			jit := NewJIT()
			defer jit.Close()
			res := jit.emitBlock(mem, codeVA)
			if res == nil {
				t.Fatal("emitBlock returned nil")
			}
			if stores := countReservationOffsetOps(res, IRStore, abjitStateResvAddrOffset); stores == 0 {
				t.Fatalf("unfused LR/SC emitted no ResvAddr stores")
			}
			if loads := countReservationOffsetOps(res, IRLoad, abjitStateResvAddrOffset); loads == 0 {
				t.Fatalf("unfused LR/SC emitted no ResvAddr loads")
			}
		})
	}
}

func TestJIT_AMOMisalignedFaultNative(t *testing.T) {
	const dataVA = uint64(0x20002)
	insns := []uint32{
		amoenc(amoFunct5Add, amoFunct3W, 12, 10, 11),
		instrECALL,
	}
	cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
		cpu.SetReg(10, dataVA)
		cpu.SetReg(11, 3)
	})
	defer mem.Free()
	defer jit.Close()

	if _, ok := err.(*MemFault); !ok {
		t.Fatalf("RunJIT err = %T %v, want *MemFault", err, err)
	}
	requireNativeAMO(t, jit)
	if got := cpu.PC(); got != 0x10000 {
		t.Fatalf("fault PC = 0x%x, want AMO PC 0x10000", got)
	}
}

func TestJIT_LRSCFaultPCs(t *testing.T) {
	t.Run("lr-load-fault-reports-lr-pc", func(t *testing.T) {
		const dataVA = uint64(Size1MB)
		insns := []uint32{
			amoenc(amoFunct5LR, amoFunct3D, 12, 10, 0),
			amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
			instrECALL,
		}
		cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
			cpu.SetReg(10, dataVA)
			cpu.SetReg(11, 3)
		})
		defer mem.Free()
		defer jit.Close()

		if _, ok := err.(*MemFault); !ok {
			t.Fatalf("RunJIT err = %T %v, want *MemFault", err, err)
		}
		requireNativeAMO(t, jit)
		if got := cpu.PC(); got != 0x10000 {
			t.Fatalf("fault PC = 0x%x, want LR PC 0x10000", got)
		}
	})

	t.Run("sc-store-fault-reports-sc-pc", func(t *testing.T) {
		const dataVA = uint64(Size1MB)
		insns := []uint32{
			ienc(opOPIMM, 0, 0, 0, 0),
			amoenc(amoFunct5SC, amoFunct3D, 13, 10, 11),
			instrECALL,
		}
		cpu, mem, jit, err := runJITAMOProgram(t, insns, func(cpu *CPU, mem *GuestMemory) {
			cpu.SetReg(10, dataVA)
			cpu.SetReg(11, 3)
			cpu.resvAddr = dataVA
			cpu.resvValid = true
		})
		defer mem.Free()
		defer jit.Close()

		if _, ok := err.(*MemFault); !ok {
			t.Fatalf("RunJIT err = %T %v, want *MemFault", err, err)
		}
		requireNativeAMO(t, jit)
		if got := cpu.PC(); got != 0x10004 {
			t.Fatalf("fault PC = 0x%x, want SC PC 0x10004", got)
		}
	})
}
