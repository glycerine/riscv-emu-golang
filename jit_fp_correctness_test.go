package riscv

import "testing"

// jit_fp_correctness_test.go — exercises the JIT's native-code FP
// path. Each test runs a tiny program through jit.StepBlock, asserts
// the JIT compiled a native block (len(lazyBlocks) > 0), and checks
// the result. Tests cover:
//   - normal inputs (non-NaN operands → non-NaN result)
//   - NaN-producing inputs (result must be canonical qNaN per §11.3)
//   - FMA single-rounding witness (§11.6)
//   - FMIN/FMAX signed-zero ordering + two-NaN canonicalization
//
// Every op is tested at both F32 and F64 precision.

// ── Helpers ────────────────────────────────────────────────────────────

// runJITFP compiles and runs `code` through the JIT (via StepBlock),
// seeds f2/f3/f4 with the provided values, and returns the resulting CPU.
// The test asserts the entry block compiled, not just the appended exit
// block, so these tests cannot pass by interpreting the instruction under
// test and then compiling the ECALL tail.
func runJITFP(t *testing.T, code []uint32, fregs map[uint8]uint64) *CPU {
	t.Helper()
	const va = uint64(0x10000)
	full := append([]uint32(nil), code...)
	// ADDI a7, x0, 93; ECALL — clean exit.
	full = append(full, 0x05D00893, 0x00000073)
	data := BuildELF(va, full)

	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mem.Free)
	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(va)
	for r, v := range fregs {
		cpu.SetFReg(r, v)
	}

	jit := NewJIT()
	t.Cleanup(jit.Close)
	for i := 0; i < 100; i++ {
		if _, err := jit.StepBlock(cpu); err != nil {
			break
		}
	}
	if jit.noJIT[va] {
		detail := "no emit result"
		if res := jit.emitBlock(&cpu.mem, va); res != nil {
			blk, err := jit.jitCompile(res, &cpu.mem)
			if err != nil {
				detail = err.Error()
			} else {
				detail = "recompile unexpectedly succeeded"
				_ = blk
			}
		}
		t.Fatalf("JIT failed to compile entry block at pc=0x%x: %s", va, detail)
	}
	if jit.lookupBlock(va) == nil {
		t.Fatalf("JIT did not compile entry block at pc=0x%x", va)
	}
	return cpu
}

func runJITFPWithSetup(t *testing.T, code []uint32, setup func(*CPU)) *CPU {
	t.Helper()
	const va = uint64(0x10000)
	full := append([]uint32(nil), code...)
	full = append(full, 0x05D00893, 0x00000073)
	data := BuildELF(va, full)

	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mem.Free)
	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(va)
	if setup != nil {
		setup(cpu)
	}

	jit := NewJIT()
	t.Cleanup(jit.Close)
	for i := 0; i < 100; i++ {
		if _, err := jit.StepBlock(cpu); err != nil {
			break
		}
	}
	if jit.noJIT[va] {
		t.Fatalf("JIT failed to compile entry block at pc=0x%x", va)
	}
	if jit.lookupBlock(va) == nil {
		t.Fatalf("JIT did not compile entry block at pc=0x%x", va)
	}
	return cpu
}

func runInterpFPWithSetup(t *testing.T, code []uint32, setup func(*CPU)) *CPU {
	t.Helper()
	const va = uint64(0x10000)
	data := BuildELF(va, code)

	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mem.Free)
	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(va)
	if setup != nil {
		setup(cpu)
	}
	for range code {
		if err := cpu.Step(); err != nil {
			t.Fatal(err)
		}
	}
	return cpu
}

// encFP encodes an RV32F FP-OP instruction (opcode 0x53).
//
//	funct5 [31:27]  fmt [26:25]  rs2 [24:20]  rs1 [19:15]  funct3 [14:12]
//	rd [11:7]  opcode=0x53 [6:0]
//
// For FSQRT, rs2 is unused (must be 0).
func encFP(funct5, fmt, rd, rs1, rs2, funct3 uint32) uint32 {
	return 0x53 |
		(rd << 7) | (funct3 << 12) | (rs1 << 15) | (rs2 << 20) |
		(fmt << 25) | (funct5 << 27)
}

// encFMA encodes an FMA-family instruction (opcodes 0x43/47/4B/4F).
// rs3 [31:27]  fmt [26:25]  rs2 [24:20]  rs1 [19:15]  rm [14:12]
// rd [11:7]  opcode [6:0]
func encFMA(opcode, fmt, rd, rs1, rs2, rs3, rm uint32) uint32 {
	return opcode |
		(rd << 7) | (rm << 12) | (rs1 << 15) | (rs2 << 20) |
		(fmt << 25) | (rs3 << 27)
}

const (
	nanBoxUpper32 = uint64(0xFFFFFFFF00000000)

	// f32 bit patterns.
	f32_1_0     uint32 = 0x3F800000
	f32_2_0     uint32 = 0x40000000
	f32_3_0     uint32 = 0x40400000
	f32_neg1_0  uint32 = 0xBF800000
	f32_posInf  uint32 = 0x7F800000
	f32_negInf  uint32 = 0xFF800000
	f32_posZero uint32 = 0x00000000
	f32_negZero uint32 = 0x80000000
	f32_canonQ  uint32 = 0x7FC00000

	// f64 bit patterns.
	f64_1_0     uint64 = 0x3FF0000000000000
	f64_2_0     uint64 = 0x4000000000000000
	f64_3_0     uint64 = 0x4008000000000000
	f64_neg1_0  uint64 = 0xBFF0000000000000
	f64_posInf  uint64 = 0x7FF0000000000000
	f64_negInf  uint64 = 0xFFF0000000000000
	f64_posZero uint64 = 0
	f64_negZero uint64 = 0x8000000000000000
	f64_canonQ  uint64 = 0x7FF8000000000000
)

func nb32(v uint32) uint64 { return nanBoxUpper32 | uint64(v) }

// checkF32 asserts f[rd] lower 32 == want and upper 32 == 0xFFFFFFFF
// (proper NaN-box per spec §11.2).
func checkF32(t *testing.T, cpu *CPU, rd uint8, want uint32, label string) {
	t.Helper()
	got := cpu.FReg(rd)
	if got != nb32(want) {
		t.Errorf("%s: f%d = 0x%016X, want 0x%016X", label, rd, got, nb32(want))
	}
}

func checkF64(t *testing.T, cpu *CPU, rd uint8, want uint64, label string) {
	t.Helper()
	if got := cpu.FReg(rd); got != want {
		t.Errorf("%s: f%d = 0x%016X, want 0x%016X", label, rd, got, want)
	}
}

func TestJIT_FLDThenFSUBD_UsesLoadedValueInSameBlock(t *testing.T) {
	const dataVA = uint64(0x20000)
	code := []uint32{
		ienc(0x07, 3, 4, 10, 0),    // FLD f4, 0(a0)
		encFP(0x01, 1, 1, 4, 3, 0), // FSUB.D f1, f4, f3
	}
	setup := func(cpu *CPU) {
		cpu.SetReg(10, dataVA)
		cpu.SetFReg(3, f64_1_0)
		cpu.SetFReg(4, f64_posZero)
		if fault := cpu.mem.Store64(dataVA, f64_3_0); fault != nil {
			t.Fatalf("Store64(0x%x): %v", dataVA, fault)
		}
	}

	interp := runInterpFPWithSetup(t, code, setup)
	jit := runJITFPWithSetup(t, code, setup)

	if got := interp.FReg(1); got != f64_2_0 {
		t.Fatalf("interpreter sanity: f1 = 0x%016X, want 0x%016X", got, f64_2_0)
	}
	if got, want := jit.FReg(1), interp.FReg(1); got != want {
		t.Fatalf("JIT did not use same-block FLD result: f1 = 0x%016X, want 0x%016X", got, want)
	}
}

// ── FADD / FSUB / FMUL / FDIV normal inputs ───────────────────────────

func TestJIT_FADD_S_Normal(t *testing.T) {
	// FADD.S f1, f2, f3
	code := []uint32{encFP(0x00, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_1_0),
		3: nb32(f32_2_0),
	})
	checkF32(t, cpu, 1, f32_3_0, "1.0 + 2.0")
}

func TestJIT_FSUB_S_Normal(t *testing.T) {
	code := []uint32{encFP(0x01, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_3_0),
		3: nb32(f32_1_0),
	})
	checkF32(t, cpu, 1, f32_2_0, "3.0 - 1.0")
}

func TestJIT_FMUL_S_Normal(t *testing.T) {
	code := []uint32{encFP(0x02, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_2_0),
		3: nb32(f32_3_0),
	})
	const f32_6_0 uint32 = 0x40C00000
	checkF32(t, cpu, 1, f32_6_0, "2.0 * 3.0")
}

func TestJIT_FDIV_S_Normal(t *testing.T) {
	code := []uint32{encFP(0x03, 0, 1, 2, 3, 0)}
	const f32_6_0 uint32 = 0x40C00000
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_6_0),
		3: nb32(f32_2_0),
	})
	checkF32(t, cpu, 1, f32_3_0, "6.0 / 2.0")
}

func TestJIT_FSQRT_S_Normal(t *testing.T) {
	code := []uint32{encFP(0x0B, 0, 1, 2, 0, 0)}
	const f32_4_0 uint32 = 0x40800000
	const f32_2_0_ uint32 = 0x40000000
	cpu := runJITFP(t, code, map[uint8]uint64{2: nb32(f32_4_0)})
	checkF32(t, cpu, 1, f32_2_0_, "sqrt(4.0)")
}

// ── FADD / FSUB / FMUL / FDIV / FSQRT NaN-producing inputs ────────────

func TestJIT_FADD_S_InfMinusInf_Canonical(t *testing.T) {
	code := []uint32{encFP(0x00, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_posInf),
		3: nb32(f32_negInf),
	})
	checkF32(t, cpu, 1, f32_canonQ, "+inf + -inf")
}

func TestJIT_FSUB_S_InfMinusInf_Canonical(t *testing.T) {
	code := []uint32{encFP(0x01, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_posInf),
		3: nb32(f32_posInf),
	})
	checkF32(t, cpu, 1, f32_canonQ, "+inf - +inf")
}

func TestJIT_FMUL_S_ZeroTimesInf_Canonical(t *testing.T) {
	code := []uint32{encFP(0x02, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_posZero),
		3: nb32(f32_posInf),
	})
	checkF32(t, cpu, 1, f32_canonQ, "0 * +inf")
}

func TestJIT_FDIV_S_NegZeroByNegZero_Canonical(t *testing.T) {
	code := []uint32{encFP(0x03, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_negZero),
		3: nb32(f32_negZero),
	})
	checkF32(t, cpu, 1, f32_canonQ, "-0 / -0")
}

func TestJIT_FSQRT_S_Negative_Canonical(t *testing.T) {
	code := []uint32{encFP(0x0B, 0, 1, 2, 0, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: nb32(f32_neg1_0)})
	checkF32(t, cpu, 1, f32_canonQ, "sqrt(-1)")
}

// ── FMIN / FMAX signed-zero + NaN ──────────────────────────────────────

func TestJIT_FMIN_S_NegPosZero(t *testing.T) {
	code := []uint32{encFP(0x05, 0, 1, 2, 3, 0)} // funct3=0 FMIN
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_negZero),
		3: nb32(f32_posZero),
	})
	checkF32(t, cpu, 1, f32_negZero, "min(-0, +0)")
}

func TestJIT_FMAX_S_NegPosZero(t *testing.T) {
	code := []uint32{encFP(0x05, 0, 1, 2, 3, 1)} // funct3=1 FMAX
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(f32_negZero),
		3: nb32(f32_posZero),
	})
	checkF32(t, cpu, 1, f32_posZero, "max(-0, +0)")
}

func TestJIT_FMIN_S_BothNaN_Canonical(t *testing.T) {
	code := []uint32{encFP(0x05, 0, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(0x7FC00001), // non-canonical NaN
		3: nb32(0x7FC00002), // non-canonical NaN
	})
	checkF32(t, cpu, 1, f32_canonQ, "min(NaN, NaN)")
}

func TestJIT_FMAX_S_BothNaN_Canonical(t *testing.T) {
	code := []uint32{encFP(0x05, 0, 1, 2, 3, 1)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: nb32(0x7FC00003),
		3: nb32(0x7FC00004),
	})
	checkF32(t, cpu, 1, f32_canonQ, "max(NaN, NaN)")
}

// ── FMA single-rounding ────────────────────────────────────────────────

// Classic fused-vs-non-fused witness:
//
//	a=0.1f, b=10.0f, c=-1.0f
//	Two-rounded: (0.1f * 10.0f = 1.0f exactly) + (-1.0f) = 0.0
//	Single-rounded fma: infinite-precision (0.1f * 10.0f = 1.0000000149...)
//	  + (-1.0) = 1.49e-8, rounded to 0x32800000.
func TestJIT_FMADD_S_Fused(t *testing.T) {
	// FMADD.S f1, f2, f3, f4 rm=RNE
	insn := encFMA(0x43, 0, 1, 2, 3, 4, 0)
	cpu := runJITFP(t, []uint32{insn}, map[uint8]uint64{
		2: nb32(0x3DCCCCCD), // 0.1f
		3: nb32(0x41200000), // 10.0f
		4: nb32(f32_neg1_0),
	})
	const fusedResidual uint32 = 0x32800000
	checkF32(t, cpu, 1, fusedResidual, "fma(0.1, 10, -1)")
}

func TestJIT_FMSUB_S_Fused(t *testing.T) {
	// FMSUB.S f1, f2, f3, f4:  a*b - c
	// With a=0.1, b=10, c=+1.0: fused = 1.49e-8 (0x32800000), non-fused = 0.
	insn := encFMA(0x47, 0, 1, 2, 3, 4, 0)
	cpu := runJITFP(t, []uint32{insn}, map[uint8]uint64{
		2: nb32(0x3DCCCCCD),
		3: nb32(0x41200000),
		4: nb32(f32_1_0),
	})
	checkF32(t, cpu, 1, 0x32800000, "fmsub(0.1, 10, 1)")
}

func TestJIT_FNMADD_S_Fused(t *testing.T) {
	// FNMADD.S: -(a*b + c). With a=0.1, b=10, c=-1: -(fused residual) = -1.49e-8.
	insn := encFMA(0x4F, 0, 1, 2, 3, 4, 0)
	cpu := runJITFP(t, []uint32{insn}, map[uint8]uint64{
		2: nb32(0x3DCCCCCD),
		3: nb32(0x41200000),
		4: nb32(f32_neg1_0),
	})
	const negResidual uint32 = 0xB2800000 // -1.49e-8
	checkF32(t, cpu, 1, negResidual, "-fma(0.1, 10, -1)")
}

func TestJIT_FNMSUB_S_Fused(t *testing.T) {
	// FNMSUB.S: -(a*b - c) = -a*b + c. With a=0.1, b=10, c=1.0:
	//   fma(-a, b, c) = -1.0000000149... + 1 = -1.49e-8.
	insn := encFMA(0x4B, 0, 1, 2, 3, 4, 0)
	cpu := runJITFP(t, []uint32{insn}, map[uint8]uint64{
		2: nb32(0x3DCCCCCD),
		3: nb32(0x41200000),
		4: nb32(f32_1_0),
	})
	const negResidual uint32 = 0xB2800000
	checkF32(t, cpu, 1, negResidual, "fnmsub(0.1, 10, 1)")
}

// ── F64 counterparts ───────────────────────────────────────────────────

func TestJIT_FADD_D_Normal(t *testing.T) {
	code := []uint32{encFP(0x00, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_1_0, 3: f64_2_0})
	checkF64(t, cpu, 1, f64_3_0, "1.0 + 2.0 (D)")
}

func TestJIT_FSUB_D_Normal(t *testing.T) {
	code := []uint32{encFP(0x01, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_3_0, 3: f64_1_0})
	checkF64(t, cpu, 1, f64_2_0, "3.0 - 1.0 (D)")
}

func TestJIT_FMUL_D_Normal(t *testing.T) {
	code := []uint32{encFP(0x02, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_2_0, 3: f64_3_0})
	const f64_6_0 uint64 = 0x4018000000000000
	checkF64(t, cpu, 1, f64_6_0, "2.0 * 3.0 (D)")
}

func TestJIT_FDIV_D_Normal(t *testing.T) {
	code := []uint32{encFP(0x03, 1, 1, 2, 3, 0)}
	const f64_6_0 uint64 = 0x4018000000000000
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_6_0, 3: f64_2_0})
	checkF64(t, cpu, 1, f64_3_0, "6.0 / 2.0 (D)")
}

func TestJIT_FSQRT_D_Normal(t *testing.T) {
	code := []uint32{encFP(0x0B, 1, 1, 2, 0, 0)}
	const f64_4_0 uint64 = 0x4010000000000000
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_4_0})
	checkF64(t, cpu, 1, f64_2_0, "sqrt(4.0) (D)")
}

func TestJIT_FADD_D_InfMinusInf_Canonical(t *testing.T) {
	code := []uint32{encFP(0x00, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_posInf, 3: f64_negInf})
	checkF64(t, cpu, 1, f64_canonQ, "+inf - +inf (D)")
}

func TestJIT_FMUL_D_ZeroTimesInf_Canonical(t *testing.T) {
	code := []uint32{encFP(0x02, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_posZero, 3: f64_posInf})
	checkF64(t, cpu, 1, f64_canonQ, "0 * +inf (D)")
}

func TestJIT_FDIV_D_NegZeroByNegZero_Canonical(t *testing.T) {
	code := []uint32{encFP(0x03, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_negZero, 3: f64_negZero})
	checkF64(t, cpu, 1, f64_canonQ, "-0 / -0 (D)")
}

func TestJIT_FSQRT_D_Negative_Canonical(t *testing.T) {
	code := []uint32{encFP(0x0B, 1, 1, 2, 0, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_neg1_0})
	checkF64(t, cpu, 1, f64_canonQ, "sqrt(-1) (D)")
}

func TestJIT_FMIN_D_NegPosZero(t *testing.T) {
	code := []uint32{encFP(0x05, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_negZero, 3: f64_posZero})
	checkF64(t, cpu, 1, f64_negZero, "min(-0, +0) (D)")
}

func TestJIT_FMAX_D_NegPosZero(t *testing.T) {
	code := []uint32{encFP(0x05, 1, 1, 2, 3, 1)}
	cpu := runJITFP(t, code, map[uint8]uint64{2: f64_negZero, 3: f64_posZero})
	checkF64(t, cpu, 1, f64_posZero, "max(-0, +0) (D)")
}

func TestJIT_FMIN_D_BothNaN_Canonical(t *testing.T) {
	code := []uint32{encFP(0x05, 1, 1, 2, 3, 0)}
	cpu := runJITFP(t, code, map[uint8]uint64{
		2: 0x7FF8000000000001,
		3: 0x7FF8000000000002,
	})
	checkF64(t, cpu, 1, f64_canonQ, "min(NaN, NaN) (D)")
}

func TestJIT_FMADD_D_Fused(t *testing.T) {
	// FMADD.D f1, f2, f3, f4: fused a*b+c with a=1.0+ulp, b=1.0+ulp, c=-(1.0+2*ulp)
	// exact a*b = 1 + 2*2^-52 + 2^-104; fused + c = 2^-104 (non-zero);
	// non-fused = 0.
	insn := encFMA(0x43, 1, 1, 2, 3, 4, 0)
	const f64_1_plus_ulp uint64 = 0x3FF0000000000001
	const f64_neg_1_plus_2ulp uint64 = 0xBFF0000000000002
	cpu := runJITFP(t, []uint32{insn}, map[uint8]uint64{
		2: f64_1_plus_ulp,
		3: f64_1_plus_ulp,
		4: f64_neg_1_plus_2ulp,
	})
	got := cpu.FReg(1)
	if got == 0 {
		t.Errorf("FMADD.D fused witness: f1 = 0 (non-fused) — expected non-zero fused residual")
	}
}

func TestJIT_FMSUB_D_Fused(t *testing.T) {
	// FMSUB.D: a*b - c. Using same a,b and c = +(1+2*ulp):
	// fma(a, b, -(1+2*ulp)) = 2^-104 (exact).
	insn := encFMA(0x47, 1, 1, 2, 3, 4, 0)
	const f64_1_plus_ulp uint64 = 0x3FF0000000000001
	const f64_1_plus_2ulp uint64 = 0x3FF0000000000002
	cpu := runJITFP(t, []uint32{insn}, map[uint8]uint64{
		2: f64_1_plus_ulp,
		3: f64_1_plus_ulp,
		4: f64_1_plus_2ulp,
	})
	got := cpu.FReg(1)
	if got == 0 {
		t.Errorf("FMSUB.D fused witness: f1 = 0 (non-fused)")
	}
}
