#include <cstdio>

extern void test_custom_machine();
extern void test_crashes();
extern void test_rv32i();
extern void test_rv32c();
extern void test_fp_nan_canon();
extern void test_fp_fma();
extern void test_fp_fmin_fmax();

int main()
{
	test_custom_machine();

	printf("* Test crashes\n");
	test_crashes();
	printf("* Test RV32I\n");
	test_rv32i();
	printf("* Test RV32C\n");
	test_rv32c();
	printf("* Test FP NaN canonicalization (FADD/FSUB/FMUL/FDIV/FSQRT)\n");
	test_fp_nan_canon();
	printf("* Test FP FMA single-rounding (§11.6)\n");
	test_fp_fma();
	printf("* Test FP FMIN/FMAX signed-zero ordering (§11.6)\n");
	test_fp_fmin_fmax();
	printf("Tests passed!\n");
	return 0;
}
