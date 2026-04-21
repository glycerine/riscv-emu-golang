// test_fp_nan_canon.cpp
//
// RISC-V ISA §11.3 mandates that the result of any FP operation whose
// result is NaN be the canonical qNaN:
//   - f32:  0x7FC00000
//   - f64:  0x7FF8000000000000
//
// Host FP hardware typically produces the NEGATIVE canonical payload
// (e.g. -0.0 / -0.0  →  0xFFC00000) and the naive `set_fl` / `set_dbl`
// macros in tr_api.cpp propagate that verbatim. This file exercises
// FDIV.S, FDIV.D, and FSQRT.S on inputs that force host FP to return
// a negative NaN, and asserts that libriscv canonicalizes to the spec
// quiet-NaN on BOTH the interpreter path (rvf_instr.cpp) AND the
// binary-translator path (tr_emit.cpp / tr_api.cpp set_fl_canon).
//
// Before the canonicalization fix (adding `set_fl_canon` /
// `set_dbl_canon` and routing FADD/FSUB/FMUL/FDIV/FSQRT through
// them), `test_fdiv_canon_translator` below would FAIL with
// `f1=0xFFFFFFFFFFC00000`. After the fix it passes (f1=0x...7FC00000).

#include <libriscv/machine.hpp>
#include <array>
#include <cassert>
#include <cstdio>
using namespace riscv;

static constexpr uint32_t FDIV_S_f1_f2_f3_rne = 0x183100D3u; // FDIV.S  f1, f2, f3, rm=RNE
static constexpr uint32_t FDIV_D_f4_f5_f6_rne = 0x1A628253u; // FDIV.D  f4, f5, f6, rm=RNE
static constexpr uint32_t FSQRT_S_f7_f8_rne   = 0x580403D3u; // FSQRT.S f7, f8, rm=RNE
// FADD.S f9, f10, f11 ; rm=RNE  — tests +inf + -inf → canonical qNaN.
//   funct5=0x00, fmt=S(00), rs2=11, rs1=10, rm=0, rd=9, opcode=0x53
static constexpr uint32_t FADD_S_f9_f10_f11_rne =
	0x53u | (9u<<7) | (0u<<12) | (10u<<15) | (11u<<20) | (0u<<25) | (0x00u<<27);
// FSUB.S f12, f13, f14 — tests +inf - +inf → canonical qNaN.  funct5=0x01
static constexpr uint32_t FSUB_S_f12_f13_f14_rne =
	0x53u | (12u<<7) | (0u<<12) | (13u<<15) | (14u<<20) | (0u<<25) | (0x01u<<27);
// FMUL.S f15, f16, f17 — tests 0 * inf → canonical qNaN.  funct5=0x02
static constexpr uint32_t FMUL_S_f15_f16_f17_rne =
	0x53u | (15u<<7) | (0u<<12) | (16u<<15) | (17u<<20) | (0u<<25) | (0x02u<<27);
static constexpr uint32_t ECALL_insn          = 0x00000073u;

// Build a small code segment that executes each NaN-producing op once
// then ECALLs. All ops must canonicalize their NaN result (§11.3).
static const std::array<uint32_t, 7> test_insns = {
	FDIV_S_f1_f2_f3_rne,
	FDIV_D_f4_f5_f6_rne,
	FSQRT_S_f7_f8_rne,
	FADD_S_f9_f10_f11_rne,
	FSUB_S_f12_f13_f14_rne,
	FMUL_S_f15_f16_f17_rne,
	ECALL_insn,
};

static constexpr uint64_t NAN_BOX_NEG_ZERO_F32 = 0xFFFFFFFF'80000000ull;
static constexpr uint64_t NAN_BOX_POS_ZERO_F32 = 0xFFFFFFFF'00000000ull;
static constexpr uint64_t NAN_BOX_POS_INF_F32  = 0xFFFFFFFF'7F800000ull;
static constexpr uint64_t NAN_BOX_NEG_INF_F32  = 0xFFFFFFFF'FF800000ull;
static constexpr uint64_t CANONICAL_QNAN_F32_NB = 0xFFFFFFFF'7FC00000ull;
static constexpr uint64_t NEG_ZERO_F64          = 0x8000000000000000ull;
static constexpr uint64_t CANONICAL_QNAN_F64    = 0x7FF8000000000000ull;
static constexpr uint64_t NEG_ONE_F32_NB        = 0xFFFFFFFF'BF800000ull; // -1.0f NaN-boxed

static void seed_registers(riscv::Machine<RISCV64>& m)
{
	// f2 = f3 = -0.0f (NaN-boxed)
	m.cpu.registers().getfl(2).load_u64(NAN_BOX_NEG_ZERO_F32);
	m.cpu.registers().getfl(3).load_u64(NAN_BOX_NEG_ZERO_F32);
	// f5 = f6 = -0.0 (raw f64)
	m.cpu.registers().getfl(5).load_u64(NEG_ZERO_F64);
	m.cpu.registers().getfl(6).load_u64(NEG_ZERO_F64);
	// f8 = -1.0f (NaN-boxed) — FSQRT.S of negative → NaN
	m.cpu.registers().getfl(8).load_u64(NEG_ONE_F32_NB);
	// FADD.S f9 = f10 + f11 with +inf + -inf → NaN
	m.cpu.registers().getfl(10).load_u64(NAN_BOX_POS_INF_F32);
	m.cpu.registers().getfl(11).load_u64(NAN_BOX_NEG_INF_F32);
	// FSUB.S f12 = f13 - f14 with +inf - +inf → NaN
	m.cpu.registers().getfl(13).load_u64(NAN_BOX_POS_INF_F32);
	m.cpu.registers().getfl(14).load_u64(NAN_BOX_POS_INF_F32);
	// FMUL.S f15 = f16 * f17 with 0 * +inf → NaN
	m.cpu.registers().getfl(16).load_u64(NAN_BOX_POS_ZERO_F32);
	m.cpu.registers().getfl(17).load_u64(NAN_BOX_POS_INF_F32);
}

static void check_canonical(const riscv::Machine<RISCV64>& m, const char* path)
{
	const uint64_t f1 = m.cpu.registers().getfl(1).i64;
	const uint64_t f4 = m.cpu.registers().getfl(4).i64;
	const uint64_t f7 = m.cpu.registers().getfl(7).i64;
	if (f1 != CANONICAL_QNAN_F32_NB) {
		fprintf(stderr, "[%s] FDIV.S(-0,-0) f1=0x%016lx, expected 0x%016lx (canonical qNaN f32, NaN-boxed)\n",
			path, (unsigned long)f1, (unsigned long)CANONICAL_QNAN_F32_NB);
		std::abort();
	}
	if (f4 != CANONICAL_QNAN_F64) {
		fprintf(stderr, "[%s] FDIV.D(-0,-0) f4=0x%016lx, expected 0x%016lx (canonical qNaN f64)\n",
			path, (unsigned long)f4, (unsigned long)CANONICAL_QNAN_F64);
		std::abort();
	}
	if (f7 != CANONICAL_QNAN_F32_NB) {
		fprintf(stderr, "[%s] FSQRT.S(-1) f7=0x%016lx, expected 0x%016lx (canonical qNaN f32, NaN-boxed)\n",
			path, (unsigned long)f7, (unsigned long)CANONICAL_QNAN_F32_NB);
		std::abort();
	}
	// FADD.S(+inf, -inf) → canonical qNaN.
	const uint64_t f9 = m.cpu.registers().getfl(9).i64;
	if (f9 != CANONICAL_QNAN_F32_NB) {
		fprintf(stderr, "[%s] FADD.S(+inf,-inf) f9=0x%016lx, expected 0x%016lx\n",
			path, (unsigned long)f9, (unsigned long)CANONICAL_QNAN_F32_NB);
		std::abort();
	}
	// FSUB.S(+inf, +inf) → canonical qNaN.
	const uint64_t f12 = m.cpu.registers().getfl(12).i64;
	if (f12 != CANONICAL_QNAN_F32_NB) {
		fprintf(stderr, "[%s] FSUB.S(+inf,+inf) f12=0x%016lx, expected 0x%016lx\n",
			path, (unsigned long)f12, (unsigned long)CANONICAL_QNAN_F32_NB);
		std::abort();
	}
	// FMUL.S(0, +inf) → canonical qNaN.
	const uint64_t f15 = m.cpu.registers().getfl(15).i64;
	if (f15 != CANONICAL_QNAN_F32_NB) {
		fprintf(stderr, "[%s] FMUL.S(0,+inf) f15=0x%016lx, expected 0x%016lx\n",
			path, (unsigned long)f15, (unsigned long)CANONICAL_QNAN_F32_NB);
		std::abort();
	}
}

// Interpreter path: step_one walks rvf_instr.cpp's lambda for each
// instruction, exercising fsflags() canonicalization (requires
// RISCV_FCSR=ON at compile time).
static void test_fdiv_canon_interpreter()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns.data(), 0x1000, 4 * test_insns.size());
	m.cpu.jump(0x1000);
	seed_registers(m);

	// Step each arithmetic instruction; stop before ECALL.
	m.cpu.step_one(); // FDIV.S
	m.cpu.step_one(); // FDIV.D
	m.cpu.step_one(); // FSQRT.S
	m.cpu.step_one(); // FADD.S
	m.cpu.step_one(); // FSUB.S
	m.cpu.step_one(); // FMUL.S

	check_canonical(m, "interpreter");
}

// Binary-translator path: simulate() walks the segment and (with
// RISCV_BINARY_TRANSLATION=ON, the default) JITs it through
// tr_emit.cpp → TCC-compiled native code. Exercises set_fl_canon /
// set_dbl_canon inlined into the translated C.
static void test_fdiv_canon_translator()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns.data(), 0x1000, 4 * test_insns.size());
	m.cpu.jump(0x1000);
	seed_registers(m);

	// simulate() runs until ECALL (or error / instruction limit).
	try {
		m.simulate(1000);
	} catch (...) {
		// ECALL throws out of simulate on default unhandled-syscall
		// behavior; that's fine, we only care about register state.
	}

	check_canonical(m, "translator");
}

void test_fp_nan_canon()
{
	test_fdiv_canon_interpreter();
	test_fdiv_canon_translator();
}
