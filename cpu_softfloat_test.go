//go:build softfloat

package riscv

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	softfloatGenRel = "xendor/ucb-bar/berkeley-testfloat-3/build/Linux-x86_64-GCC/testfloat_gen"

	softfloatDefaultCases = 8
	softfloatDefaultFMAs  = 1
)

type softfloatRM struct {
	name string
	opt  string
	rm   uint8
}

var softfloatRMs = []softfloatRM{
	{"rne", "-rnear_even", rmRNE},
	{"rtz", "-rminMag", rmRTZ},
	{"rdn", "-rmin", rmRDN},
	{"rup", "-rmax", rmRUP},
	{"rmm", "-rnear_maxMag", rmRMM},
}

func TestCPU_FPSoftFloatGeneratedArithmetic(t *testing.T) {
	limit := softfloatCaseLimit(t, "SOFTFLOAT_CASES", softfloatDefaultCases)

	for _, rm := range softfloatRMs {
		for _, tc := range []struct {
			name   string
			fn     string
			funct5 uint32
			arity  int
		}{
			{"add", "f32_add", 0x00, 2},
			{"sub", "f32_sub", 0x01, 2},
			{"mul", "f32_mul", 0x02, 2},
			{"div", "f32_div", 0x03, 2},
			{"sqrt", "f32_sqrt", 0x0B, 1},
		} {
			t.Run("f32/"+tc.name+"/"+rm.name, func(t *testing.T) {
				softfloatEach(t, tc.fn, rm.opt, false, limit, func(caseNum int, fields []string) {
					wantFields(t, tc.fn, fields, tc.arity+2)
					a := parseHex32(t, fields[0])
					var b uint32
					if tc.arity == 2 {
						b = parseHex32(t, fields[1])
					}
					want := parseHex32(t, fields[tc.arity])
					wantFlags := parseHex8(t, fields[tc.arity+1])

					insn := encFP(tc.funct5, 0, 1, 2, 3, uint32(rm.rm))
					got, gotFlags := runSoftfloatF32Insn(t, insn, rm.rm, a, b, 0)
					checkSoftfloatF32(t, tc.fn, caseNum, got, want, gotFlags, wantFlags)
				})
			})
		}

		for _, tc := range []struct {
			name   string
			fn     string
			funct5 uint32
			arity  int
		}{
			{"add", "f64_add", 0x00, 2},
			{"sub", "f64_sub", 0x01, 2},
			{"mul", "f64_mul", 0x02, 2},
			{"div", "f64_div", 0x03, 2},
			{"sqrt", "f64_sqrt", 0x0B, 1},
		} {
			t.Run("f64/"+tc.name+"/"+rm.name, func(t *testing.T) {
				softfloatEach(t, tc.fn, rm.opt, false, limit, func(caseNum int, fields []string) {
					wantFields(t, tc.fn, fields, tc.arity+2)
					a := parseHex64(t, fields[0])
					var b uint64
					if tc.arity == 2 {
						b = parseHex64(t, fields[1])
					}
					want := parseHex64(t, fields[tc.arity])
					wantFlags := parseHex8(t, fields[tc.arity+1])

					insn := encFP(tc.funct5, 1, 1, 2, 3, uint32(rm.rm))
					got, gotFlags := runSoftfloatF64Insn(t, insn, rm.rm, a, b, 0)
					checkSoftfloatF64(t, tc.fn, caseNum, got, want, gotFlags, wantFlags)
				})
			})
		}
	}
}

func TestCPU_FPSoftFloatGeneratedFMA(t *testing.T) {
	limit := softfloatCaseLimit(t, "SOFTFLOAT_FMA_CASES", softfloatDefaultFMAs)
	for _, rm := range softfloatFmaRMs() {
		t.Run("f32/"+rm.name, func(t *testing.T) {
			softfloatEach(t, "f32_mulAdd", rm.opt, false, limit, func(caseNum int, fields []string) {
				wantFields(t, "f32_mulAdd", fields, 5)
				a := parseHex32(t, fields[0])
				b := parseHex32(t, fields[1])
				c := parseHex32(t, fields[2])
				want := parseHex32(t, fields[3])
				wantFlags := parseHex8(t, fields[4])

				for _, op := range softfloatFMAOps32(a, b, c) {
					insn := encFMA(op.opcode, 0, 1, 2, 3, 4, uint32(rm.rm))
					got, gotFlags := runSoftfloatF32Insn(t, insn, rm.rm, op.a, op.b, op.c)
					checkSoftfloatF32(t, op.name, caseNum, got, want, gotFlags, wantFlags)
				}
			})
		})

		t.Run("f64/"+rm.name, func(t *testing.T) {
			softfloatEach(t, "f64_mulAdd", rm.opt, false, limit, func(caseNum int, fields []string) {
				wantFields(t, "f64_mulAdd", fields, 5)
				a := parseHex64(t, fields[0])
				b := parseHex64(t, fields[1])
				c := parseHex64(t, fields[2])
				want := parseHex64(t, fields[3])
				wantFlags := parseHex8(t, fields[4])

				for _, op := range softfloatFMAOps64(a, b, c) {
					insn := encFMA(op.opcode, 1, 1, 2, 3, 4, uint32(rm.rm))
					got, gotFlags := runSoftfloatF64Insn(t, insn, rm.rm, op.a, op.b, op.c)
					checkSoftfloatF64(t, op.name, caseNum, got, want, gotFlags, wantFlags)
				}
			})
		})
	}
}

func TestCPU_FPSoftFloatGeneratedConversions(t *testing.T) {
	limit := softfloatCaseLimit(t, "SOFTFLOAT_CASES", softfloatDefaultCases)

	for _, rm := range softfloatRMs {
		t.Run("f64_to_f32/"+rm.name, func(t *testing.T) {
			softfloatEach(t, "f64_to_f32", rm.opt, false, limit, func(caseNum int, fields []string) {
				wantFields(t, "f64_to_f32", fields, 3)
				a := parseHex64(t, fields[0])
				want := parseHex32(t, fields[1])
				wantFlags := parseHex8(t, fields[2])

				insn := encFP(0x08, 0, 1, 2, 1, uint32(rm.rm)) // FCVT.S.D
				got, gotFlags := runSoftfloatF32FromF64Insn(t, insn, rm.rm, a)
				checkSoftfloatF32(t, "FCVT.S.D", caseNum, got, want, gotFlags, wantFlags)
			})
		})

		t.Run("f32_to_f64/"+rm.name, func(t *testing.T) {
			softfloatEach(t, "f32_to_f64", rm.opt, false, limit, func(caseNum int, fields []string) {
				wantFields(t, "f32_to_f64", fields, 3)
				a := parseHex32(t, fields[0])
				want := parseHex64(t, fields[1])
				wantFlags := parseHex8(t, fields[2])

				insn := encFP(0x08, 1, 1, 2, 0, uint32(rm.rm)) // FCVT.D.S
				got, gotFlags := runSoftfloatF64FromF32Insn(t, insn, rm.rm, a)
				checkSoftfloatF64(t, "FCVT.D.S", caseNum, got, want, gotFlags, wantFlags)
			})
		})

		for _, tc := range []struct {
			name string
			fn   string
			rs2  uint32
			xreg func(*testing.T, string) uint64
		}{
			{"i32_to_f32", "i32_to_f32", 0, parseSoftfloatI32XReg},
			{"ui32_to_f32", "ui32_to_f32", 1, parseSoftfloatUI32XReg},
			{"i64_to_f32", "i64_to_f32", 2, parseSoftfloatI64XReg},
			{"ui64_to_f32", "ui64_to_f32", 3, parseSoftfloatUI64XReg},
		} {
			t.Run(tc.name+"/"+rm.name, func(t *testing.T) {
				softfloatEach(t, tc.fn, rm.opt, false, limit, func(caseNum int, fields []string) {
					wantFields(t, tc.fn, fields, 3)
					x := tc.xreg(t, fields[0])
					want := parseHex32(t, fields[1])
					wantFlags := parseHex8(t, fields[2])

					insn := encFP(0x1A, 0, 1, 2, tc.rs2, uint32(rm.rm)) // FCVT.S.{W,WU,L,LU}
					got, gotFlags := runSoftfloatIntToF32Insn(t, insn, rm.rm, x)
					checkSoftfloatF32(t, tc.fn, caseNum, got, want, gotFlags, wantFlags)
				})
			})
		}

		for _, tc := range []struct {
			name string
			fn   string
			rs2  uint32
			xreg func(*testing.T, string) uint64
		}{
			{"i32_to_f64", "i32_to_f64", 0, parseSoftfloatI32XReg},
			{"ui32_to_f64", "ui32_to_f64", 1, parseSoftfloatUI32XReg},
			{"i64_to_f64", "i64_to_f64", 2, parseSoftfloatI64XReg},
			{"ui64_to_f64", "ui64_to_f64", 3, parseSoftfloatUI64XReg},
		} {
			t.Run(tc.name+"/"+rm.name, func(t *testing.T) {
				softfloatEach(t, tc.fn, rm.opt, false, limit, func(caseNum int, fields []string) {
					wantFields(t, tc.fn, fields, 3)
					x := tc.xreg(t, fields[0])
					want := parseHex64(t, fields[1])
					wantFlags := parseHex8(t, fields[2])

					insn := encFP(0x1A, 1, 1, 2, tc.rs2, uint32(rm.rm)) // FCVT.D.{W,WU,L,LU}
					got, gotFlags := runSoftfloatIntToF64Insn(t, insn, rm.rm, x)
					checkSoftfloatF64(t, tc.fn, caseNum, got, want, gotFlags, wantFlags)
				})
			})
		}
	}
}

func TestCPU_FPSoftFloatGeneratedComparisons(t *testing.T) {
	limit := softfloatCaseLimit(t, "SOFTFLOAT_CASES", softfloatDefaultCases)
	for _, tc := range []struct {
		name   string
		fn32   string
		fn64   string
		funct3 uint32
	}{
		{"fle", "f32_le", "f64_le", 0},
		{"flt", "f32_lt", "f64_lt", 1},
		{"feq", "f32_eq", "f64_eq", 2},
	} {
		t.Run("f32/"+tc.name, func(t *testing.T) {
			softfloatEach(t, tc.fn32, "", false, limit, func(caseNum int, fields []string) {
				wantFields(t, tc.fn32, fields, 4)
				a := parseHex32(t, fields[0])
				b := parseHex32(t, fields[1])
				want := parseBool01(t, fields[2])
				wantFlags := parseHex8(t, fields[3])

				insn := encFP(0x14, 0, 1, 2, 3, tc.funct3)
				got, gotFlags := runSoftfloatCmpF32Insn(t, insn, a, b)
				checkSoftfloatInt(t, tc.fn32, caseNum, got, want, gotFlags, wantFlags)
			})
		})

		t.Run("f64/"+tc.name, func(t *testing.T) {
			softfloatEach(t, tc.fn64, "", false, limit, func(caseNum int, fields []string) {
				wantFields(t, tc.fn64, fields, 4)
				a := parseHex64(t, fields[0])
				b := parseHex64(t, fields[1])
				want := parseBool01(t, fields[2])
				wantFlags := parseHex8(t, fields[3])

				insn := encFP(0x14, 1, 1, 2, 3, tc.funct3)
				got, gotFlags := runSoftfloatCmpF64Insn(t, insn, a, b)
				checkSoftfloatInt(t, tc.fn64, caseNum, got, want, gotFlags, wantFlags)
			})
		})
	}
}

func TestCPU_FPRISCVSpecFloatToInt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fmt       uint32
		rs2       uint32
		rm        uint8
		f32       uint32
		f64       uint64
		want      uint64
		wantFlags uint32
	}{
		{"FCVT.W.S qNaN", 0, 0, rmRNE, f32CanonNaN, 0, 0x000000007fffffff, fflagNV},
		{"FCVT.W.S -inf", 0, 0, rmRNE, f32NegInf, 0, 0xffffffff80000000, fflagNV},
		{"FCVT.W.S 1.5 rne", 0, 0, rmRNE, 0x3fc00000, 0, 0x0000000000000002, fflagNX},
		{"FCVT.W.S 1.5 rtz", 0, 0, rmRTZ, 0x3fc00000, 0, 0x0000000000000001, fflagNX},
		{"FCVT.W.S -1.5 rdn", 0, 0, rmRDN, 0xbfc00000, 0, 0xfffffffffffffffe, fflagNX},
		{"FCVT.W.S -1.5 rup", 0, 0, rmRUP, 0xbfc00000, 0, 0xffffffffffffffff, fflagNX},
		{"FCVT.W.S 2.5 rmm", 0, 0, rmRMM, 0x40200000, 0, 0x0000000000000003, fflagNX},
		{"FCVT.WU.S -1", 0, 1, rmRNE, 0xbf800000, 0, 0, fflagNV},
		{"FCVT.WU.S qNaN", 0, 1, rmRNE, f32CanonNaN, 0, 0xffffffffffffffff, fflagNV},
		{"FCVT.L.S +inf", 0, 2, rmRNE, f32PosInf, 0, 0x7fffffffffffffff, fflagNV},
		{"FCVT.L.S -inf", 0, 2, rmRNE, f32NegInf, 0, 0x8000000000000000, fflagNV},
		{"FCVT.LU.S -1", 0, 3, rmRNE, 0xbf800000, 0, 0, fflagNV},
		{"FCVT.LU.S qNaN", 0, 3, rmRNE, f32CanonNaN, 0, 0xffffffffffffffff, fflagNV},

		{"FCVT.W.D qNaN", 1, 0, rmRNE, 0, f64CanonNaN, 0x000000007fffffff, fflagNV},
		{"FCVT.WU.D -1", 1, 1, rmRNE, 0, 0xbff0000000000000, 0, fflagNV},
		{"FCVT.L.D +inf", 1, 2, rmRNE, 0, f64PosInf, 0x7fffffffffffffff, fflagNV},
		{"FCVT.L.D 1.5 rne", 1, 2, rmRNE, 0, 0x3ff8000000000000, 0x0000000000000002, fflagNX},
		{"FCVT.LU.D -1", 1, 3, rmRNE, 0, 0xbff0000000000000, 0, fflagNV},
		{"FCVT.LU.D qNaN", 1, 3, rmRNE, 0, f64CanonNaN, 0xffffffffffffffff, fflagNV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			insn := encFP(0x18, tc.fmt, 1, 2, tc.rs2, uint32(tc.rm))
			got, gotFlags := runSoftfloatFloatToIntInsn(t, insn, tc.rm, tc.fmt, tc.f32, tc.f64)
			checkSoftfloatInt(t, tc.name, 1, got, tc.want, gotFlags, tc.wantFlags)
		})
	}
}

func softfloatEach(t *testing.T, function, roundingOpt string, exact bool, limit int, handle func(caseNum int, fields []string)) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skipf("vendored TestFloat binary is built by the Linux-x86_64-GCC tree, not GOARCH=%s", runtime.GOARCH)
	}
	gen := filepath.Join(mustGetwd(t), softfloatGenRel)
	if _, err := os.Stat(gen); err != nil {
		t.Skipf("%s missing; run `make softfloat` first", gen)
	}

	args := []string{"-level", "1", "-seed", "1"}
	if exact {
		args = append(args, "-exact")
	}
	if roundingOpt != "" {
		args = append(args, roundingOpt)
	}
	args = append(args, function)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, gen, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe %s: %v", function, err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", function, err)
	}

	scanner := bufio.NewScanner(stdout)
	caseNum := 0
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		caseNum++
		handle(caseNum, fields)
		if caseNum >= limit {
			cancel()
			break
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("scan %s: %v", function, err)
	}
	waitErr := cmd.Wait()
	if caseNum == 0 {
		t.Fatalf("%s produced no cases; stderr=%s wait=%v", function, strings.TrimSpace(stderr.String()), waitErr)
	}
	if caseNum < limit && ctx.Err() == nil {
		t.Fatalf("%s produced %d cases, want %d; stderr=%s wait=%v", function, caseNum, limit, strings.TrimSpace(stderr.String()), waitErr)
	}
	if waitErr != nil && ctx.Err() == nil {
		t.Fatalf("%s failed: %v; stderr=%s", function, waitErr, strings.TrimSpace(stderr.String()))
	}
}

func softfloatCaseLimit(t *testing.T, env string, def int) int {
	t.Helper()
	if v := os.Getenv(env); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q, want positive integer", env, v)
		}
		return n
	}
	return def
}

func softfloatFmaRMs() []softfloatRM {
	if os.Getenv("SOFTFLOAT_FMA_ALL_RM") != "" {
		return softfloatRMs
	}
	return softfloatRMs[:1]
}

func runSoftfloatF32Insn(t *testing.T, insn uint32, rm uint8, a, b, c uint32) (uint32, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	cpu.SetFReg(2, boxF32(a))
	cpu.SetFReg(3, boxF32(b))
	cpu.SetFReg(4, boxF32(c))
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return uint32(cpu.FReg(1)), cpu.FCSR() & 0x1f
}

func runSoftfloatF64Insn(t *testing.T, insn uint32, rm uint8, a, b, c uint64) (uint64, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	cpu.SetFReg(2, boxF64(a))
	cpu.SetFReg(3, boxF64(b))
	cpu.SetFReg(4, boxF64(c))
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return cpu.FReg(1), cpu.FCSR() & 0x1f
}

func runSoftfloatF32FromF64Insn(t *testing.T, insn uint32, rm uint8, a uint64) (uint32, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	cpu.SetFReg(2, boxF64(a))
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return uint32(cpu.FReg(1)), cpu.FCSR() & 0x1f
}

func runSoftfloatF64FromF32Insn(t *testing.T, insn uint32, rm uint8, a uint32) (uint64, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	cpu.SetFReg(2, boxF32(a))
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return cpu.FReg(1), cpu.FCSR() & 0x1f
}

func runSoftfloatIntToF32Insn(t *testing.T, insn uint32, rm uint8, x uint64) (uint32, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	cpu.SetReg(2, x)
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return uint32(cpu.FReg(1)), cpu.FCSR() & 0x1f
}

func runSoftfloatIntToF64Insn(t *testing.T, insn uint32, rm uint8, x uint64) (uint64, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	cpu.SetReg(2, x)
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return cpu.FReg(1), cpu.FCSR() & 0x1f
}

func runSoftfloatCmpF32Insn(t *testing.T, insn uint32, a, b uint32) (uint64, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFReg(2, boxF32(a))
	cpu.SetFReg(3, boxF32(b))
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return cpu.Reg(1), cpu.FCSR() & 0x1f
}

func runSoftfloatCmpF64Insn(t *testing.T, insn uint32, a, b uint64) (uint64, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFReg(2, boxF64(a))
	cpu.SetFReg(3, boxF64(b))
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return cpu.Reg(1), cpu.FCSR() & 0x1f
}

func runSoftfloatFloatToIntInsn(t *testing.T, insn uint32, rm uint8, fmt uint32, f32 uint32, f64 uint64) (uint64, uint32) {
	t.Helper()
	cpu := NewCPU(GuestMemory{})
	cpu.SetFCSR(uint32(rm) << 5)
	if fmt == 0 {
		cpu.SetFReg(2, boxF32(f32))
	} else {
		cpu.SetFReg(2, boxF64(f64))
	}
	if err := cpu.stepFromInsn(insn); err != nil {
		t.Fatalf("step 0x%08x: %v", insn, err)
	}
	return cpu.Reg(1), cpu.FCSR() & 0x1f
}

type softfloatFMAOp32 struct {
	name   string
	opcode uint32
	a, b   uint32
	c      uint32
}

func softfloatFMAOps32(a, b, c uint32) []softfloatFMAOp32 {
	return []softfloatFMAOp32{
		{"FMADD.S", 0x43, a, b, c},
		{"FMSUB.S", 0x47, a, b, c ^ f32SignBit},
		{"FNMSUB.S", 0x4B, a ^ f32SignBit, b, c},
		{"FNMADD.S", 0x4F, a ^ f32SignBit, b, c ^ f32SignBit},
	}
}

type softfloatFMAOp64 struct {
	name   string
	opcode uint32
	a, b   uint64
	c      uint64
}

func softfloatFMAOps64(a, b, c uint64) []softfloatFMAOp64 {
	return []softfloatFMAOp64{
		{"FMADD.D", 0x43, a, b, c},
		{"FMSUB.D", 0x47, a, b, c ^ f64SignBit},
		{"FNMSUB.D", 0x4B, a ^ f64SignBit, b, c},
		{"FNMADD.D", 0x4F, a ^ f64SignBit, b, c ^ f64SignBit},
	}
}

func checkSoftfloatF32(t *testing.T, fn string, caseNum int, got, want uint32, gotFlags, wantFlags uint32) {
	t.Helper()
	want = canonNaN32(want)
	if got != want || gotFlags != wantFlags {
		t.Fatalf("%s case %d: got result=0x%08x flags=0x%02x, want result=0x%08x flags=0x%02x",
			fn, caseNum, got, gotFlags, want, wantFlags)
	}
}

func checkSoftfloatF64(t *testing.T, fn string, caseNum int, got, want uint64, gotFlags, wantFlags uint32) {
	t.Helper()
	want = canonNaN64(want)
	if got != want || gotFlags != wantFlags {
		t.Fatalf("%s case %d: got result=0x%016x flags=0x%02x, want result=0x%016x flags=0x%02x",
			fn, caseNum, got, gotFlags, want, wantFlags)
	}
}

func checkSoftfloatInt(t *testing.T, fn string, caseNum int, got, want uint64, gotFlags, wantFlags uint32) {
	t.Helper()
	if got != want || gotFlags != wantFlags {
		t.Fatalf("%s case %d: got result=0x%x flags=0x%02x, want result=0x%x flags=0x%02x",
			fn, caseNum, got, gotFlags, want, wantFlags)
	}
}

func wantFields(t *testing.T, fn string, fields []string, want int) {
	t.Helper()
	if len(fields) != want {
		t.Fatalf("%s output field count = %d (%q), want %d", fn, len(fields), strings.Join(fields, " "), want)
	}
}

func parseHex8(t *testing.T, s string) uint32 {
	t.Helper()
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		t.Fatalf("parse hex8 %q: %v", s, err)
	}
	return uint32(v)
}

func parseHex32(t *testing.T, s string) uint32 {
	t.Helper()
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		t.Fatalf("parse hex32 %q: %v", s, err)
	}
	return uint32(v)
}

func parseHex64(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		t.Fatalf("parse hex64 %q: %v", s, err)
	}
	return v
}

func parseBool01(t *testing.T, s string) uint64 {
	t.Helper()
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	default:
		t.Fatalf("parse bool %q: want 0 or 1", s)
		return 0
	}
}

func parseSoftfloatI32XReg(t *testing.T, s string) uint64 {
	t.Helper()
	return uint64(int64(int32(parseHex32(t, s))))
}

func parseSoftfloatUI32XReg(t *testing.T, s string) uint64 {
	t.Helper()
	return uint64(parseHex32(t, s))
}

func parseSoftfloatI64XReg(t *testing.T, s string) uint64 {
	t.Helper()
	return parseHex64(t, s)
}

func parseSoftfloatUI64XReg(t *testing.T, s string) uint64 {
	t.Helper()
	return parseHex64(t, s)
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
