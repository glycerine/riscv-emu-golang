// test_fp_fmin_fmax.cpp
//
// RISC-V unprivileged ISA §11.6 requires FMIN/FMAX to treat
// -0.0 < +0.0 (IEEE 754 fmin/fmax leave that ordering
// implementation-defined, and host libm often returns the wrong
// sign for ±0 inputs). The fix uses bit-manipulation to pick the
// right signed zero whenever both operands compare equal to zero.
//
// This test pins the four ±0 combinations and asserts the
// spec-mandated result. Red before the fix, green after.

#include <libriscv/machine.hpp>
#include <array>
#include <cstdio>
#include <cstdlib>
using namespace riscv;

// OP-FP, funct5=0x05 (FMIN/FMAX), funct3=0 (MIN) or 1 (MAX), fmt=00 (S)/01 (D).
//   [31:27] funct5   [26:25] fmt  [24:20] rs2  [19:15] rs1  [14:12] funct3
//   [11:7]  rd       [6:0]   opc=0x53
static constexpr uint32_t enc_fmm(uint32_t funct5, uint32_t fmt,
                                  uint32_t rd, uint32_t rs1, uint32_t rs2,
                                  uint32_t funct3)
{
	return 0x53u | (rd << 7) | (funct3 << 12) | (rs1 << 15) | (rs2 << 20) |
	       (fmt << 25) | (funct5 << 27);
}

// FMIN.S f1, f2, f3  ; FMAX.S f4, f2, f3 ; FMIN.D f5, f6, f7 ; FMAX.D f8, f6, f7
static constexpr uint32_t FMIN_S_f1 = enc_fmm(0x05, 0, 1, 2, 3, 0);
static constexpr uint32_t FMAX_S_f4 = enc_fmm(0x05, 0, 4, 2, 3, 1);
static constexpr uint32_t FMIN_D_f5 = enc_fmm(0x05, 1, 5, 6, 7, 0);
static constexpr uint32_t FMAX_D_f8 = enc_fmm(0x05, 1, 8, 6, 7, 1);
static constexpr uint32_t ECALL_insn = 0x00000073u;

static const std::array<uint32_t, 5> test_insns = {
	FMIN_S_f1, FMAX_S_f4, FMIN_D_f5, FMAX_D_f8, ECALL_insn,
};

static uint64_t nb_f32(uint32_t v) { return 0xFFFFFFFF00000000ull | uint64_t(v); }

static constexpr uint32_t F32_POS_ZERO = 0x00000000u;
static constexpr uint32_t F32_NEG_ZERO = 0x80000000u;
static constexpr uint64_t F64_POS_ZERO = 0x0000000000000000ull;
static constexpr uint64_t F64_NEG_ZERO = 0x8000000000000000ull;

// Seed: f2=-0.0f, f3=+0.0f ; f6=-0.0, f7=+0.0.
static void seed_regs_neg_pos_zero(riscv::Machine<RISCV64>& m)
{
	m.cpu.registers().getfl(2).load_u64(nb_f32(F32_NEG_ZERO));
	m.cpu.registers().getfl(3).load_u64(nb_f32(F32_POS_ZERO));
	m.cpu.registers().getfl(6).load_u64(F64_NEG_ZERO);
	m.cpu.registers().getfl(7).load_u64(F64_POS_ZERO);
}

static void check_neg_pos_zero(const riscv::Machine<RISCV64>& m, const char* path)
{
	// With f2=-0.0f, f3=+0.0f: FMIN.S(-0,+0) = -0.0f (nan-boxed),
	// FMAX.S(-0,+0) = +0.0f (nan-boxed).
	const uint64_t f1 = m.cpu.registers().getfl(1).i64;
	const uint64_t f4 = m.cpu.registers().getfl(4).i64;
	const uint64_t f5 = m.cpu.registers().getfl(5).i64;
	const uint64_t f8 = m.cpu.registers().getfl(8).i64;

	if (uint32_t(f1) != F32_NEG_ZERO) {
		fprintf(stderr, "[%s] FMIN.S(-0,+0): f1 lower=0x%08x, expected 0x%08x "
				"(-0.0f per §11.6 ordering)\n",
				path, uint32_t(f1), F32_NEG_ZERO);
		std::abort();
	}
	if (uint32_t(f4) != F32_POS_ZERO) {
		fprintf(stderr, "[%s] FMAX.S(-0,+0): f4 lower=0x%08x, expected 0x%08x "
				"(+0.0f per §11.6 ordering)\n",
				path, uint32_t(f4), F32_POS_ZERO);
		std::abort();
	}
	if (f5 != F64_NEG_ZERO) {
		fprintf(stderr, "[%s] FMIN.D(-0,+0): f5=0x%016llx, expected 0x%016llx\n",
				path, (unsigned long long)f5, (unsigned long long)F64_NEG_ZERO);
		std::abort();
	}
	if (f8 != F64_POS_ZERO) {
		fprintf(stderr, "[%s] FMAX.D(-0,+0): f8=0x%016llx, expected 0x%016llx\n",
				path, (unsigned long long)f8, (unsigned long long)F64_POS_ZERO);
		std::abort();
	}
}

// Also test the symmetric case (+0, -0) — same expected result because
// -0 < +0 is a TOTAL order in the spec, not a left/right preference.
static const std::array<uint32_t, 5> test_insns_pos_neg = {
	FMIN_S_f1, FMAX_S_f4, FMIN_D_f5, FMAX_D_f8, ECALL_insn,
};

static void seed_regs_pos_neg_zero(riscv::Machine<RISCV64>& m)
{
	m.cpu.registers().getfl(2).load_u64(nb_f32(F32_POS_ZERO));
	m.cpu.registers().getfl(3).load_u64(nb_f32(F32_NEG_ZERO));
	m.cpu.registers().getfl(6).load_u64(F64_POS_ZERO);
	m.cpu.registers().getfl(7).load_u64(F64_NEG_ZERO);
}

static void test_fmm_interpreter_neg_pos()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns.data(), 0x1000, 4 * test_insns.size());
	m.cpu.jump(0x1000);
	seed_regs_neg_pos_zero(m);
	m.cpu.step_one(); m.cpu.step_one(); m.cpu.step_one(); m.cpu.step_one();
	check_neg_pos_zero(m, "fmm-interp(-0,+0)");
}

static void test_fmm_interpreter_pos_neg()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns_pos_neg.data(), 0x1000, 4 * test_insns_pos_neg.size());
	m.cpu.jump(0x1000);
	seed_regs_pos_neg_zero(m);
	m.cpu.step_one(); m.cpu.step_one(); m.cpu.step_one(); m.cpu.step_one();
	check_neg_pos_zero(m, "fmm-interp(+0,-0)");
}

static void test_fmm_translator_neg_pos()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns.data(), 0x1000, 4 * test_insns.size());
	m.cpu.jump(0x1000);
	seed_regs_neg_pos_zero(m);
	try { m.simulate(1000); } catch (...) {}
	check_neg_pos_zero(m, "fmm-trans(-0,+0)");
}

static void test_fmm_translator_pos_neg()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns_pos_neg.data(), 0x1000, 4 * test_insns_pos_neg.size());
	m.cpu.jump(0x1000);
	seed_regs_pos_neg_zero(m);
	try { m.simulate(1000); } catch (...) {}
	check_neg_pos_zero(m, "fmm-trans(+0,-0)");
}

// ── Two-NaN canon (§11.3) ───────────────────────────────────────────────
//
// When both operands of FMIN/FMAX are NaN, the result must be the
// canonical qNaN, not one of the input NaN payloads.

static constexpr uint32_t F32_CANON_QNAN = 0x7FC00000u;
static constexpr uint64_t F64_CANON_QNAN = 0x7FF8000000000000ull;

static void seed_regs_both_nan(riscv::Machine<RISCV64>& m)
{
	// Use non-canonical NaN payloads so we can detect whether the emulator
	// silently passes them through (spec violation) or canonicalizes.
	m.cpu.registers().getfl(2).load_u64(nb_f32(0x7FC00001));
	m.cpu.registers().getfl(3).load_u64(nb_f32(0x7FC00002));
	m.cpu.registers().getfl(6).load_u64(0x7FF8000000000001ull);
	m.cpu.registers().getfl(7).load_u64(0x7FF8000000000002ull);
}

static void check_both_nan_canonical(const riscv::Machine<RISCV64>& m, const char* path)
{
	const uint64_t f1 = m.cpu.registers().getfl(1).i64;
	const uint64_t f4 = m.cpu.registers().getfl(4).i64;
	const uint64_t f5 = m.cpu.registers().getfl(5).i64;
	const uint64_t f8 = m.cpu.registers().getfl(8).i64;

	const uint64_t want_f32 = nb_f32(F32_CANON_QNAN);
	if (f1 != want_f32) {
		fprintf(stderr, "[%s] FMIN.S(NaN,NaN): f1=0x%016llx, expected 0x%016llx "
				"(canonical qNaN per §11.3)\n",
				path, (unsigned long long)f1, (unsigned long long)want_f32);
		std::abort();
	}
	if (f4 != want_f32) {
		fprintf(stderr, "[%s] FMAX.S(NaN,NaN): f4=0x%016llx, expected 0x%016llx\n",
				path, (unsigned long long)f4, (unsigned long long)want_f32);
		std::abort();
	}
	if (f5 != F64_CANON_QNAN) {
		fprintf(stderr, "[%s] FMIN.D(NaN,NaN): f5=0x%016llx, expected 0x%016llx\n",
				path, (unsigned long long)f5, (unsigned long long)F64_CANON_QNAN);
		std::abort();
	}
	if (f8 != F64_CANON_QNAN) {
		fprintf(stderr, "[%s] FMAX.D(NaN,NaN): f8=0x%016llx, expected 0x%016llx\n",
				path, (unsigned long long)f8, (unsigned long long)F64_CANON_QNAN);
		std::abort();
	}
}

static void test_fmm_interpreter_both_nan()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns.data(), 0x1000, 4 * test_insns.size());
	m.cpu.jump(0x1000);
	seed_regs_both_nan(m);
	m.cpu.step_one(); m.cpu.step_one(); m.cpu.step_one(); m.cpu.step_one();
	check_both_nan_canonical(m, "fmm-interp(NaN,NaN)");
}

static void test_fmm_translator_both_nan()
{
	riscv::Machine<RISCV64> m { std::string_view{}, { .memory_max = 65536 } };
	m.cpu.init_execute_area(test_insns.data(), 0x1000, 4 * test_insns.size());
	m.cpu.jump(0x1000);
	seed_regs_both_nan(m);
	try { m.simulate(1000); } catch (...) {}
	check_both_nan_canonical(m, "fmm-trans(NaN,NaN)");
}

void test_fp_fmin_fmax()
{
	test_fmm_interpreter_neg_pos();
	test_fmm_interpreter_pos_neg();
	test_fmm_translator_neg_pos();
	test_fmm_translator_pos_neg();
	test_fmm_interpreter_both_nan();
	test_fmm_translator_both_nan();
}
