//go:build arm64

package riscv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestARM64_ZeroImmediateComparisons(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	predicates := []struct {
		pred Pred
		want func(int64) bool
	}{
		{EQ, func(v int64) bool { return 0 == v }},
		{NE, func(v int64) bool { return 0 != v }},
		{LT, func(v int64) bool { return 0 < v }},
		{LE, func(v int64) bool { return 0 <= v }},
		{GT, func(v int64) bool { return 0 > v }},
		{GE, func(v int64) bool { return 0 >= v }},
		{LTU, func(v int64) bool { return uint64(0) < uint64(v) }},
		{LEU, func(v int64) bool { return uint64(0) <= uint64(v) }},
		{GTU, func(v int64) bool { return uint64(0) > uint64(v) }},
		{GEU, func(v int64) bool { return uint64(0) >= uint64(v) }},
	}
	// Include immediate encodings (unshifted and shifted), register fallbacks,
	// and signed boundaries. Both SetImm and BranchImm must treat x0 as zero.
	for _, policy := range []RegPolicy{PolicyRV8, PolicyABJIT} {
		for _, imm := range []int64{0, 1, 69, 4095, 4096, 0xfff000, 4097, -1, -1 << 63, 1<<63 - 1} {
			for _, tc := range predicates {
				t.Run(fmt.Sprintf("%s/%s/%d", policy.Name, tc.pred, imm), func(t *testing.T) {
					j := NewSandboxJIT()
					defer j.Close()
					j.SetRegPolicy(policy)

					e := NewEmitter(nil)
					taken := e.NewLabel()
					e.SetImm(e.XReg(10), VRegZero, imm, tc.pred)
					e.BranchImm(VRegZero, imm, tc.pred, taken)
					e.Ret(0x1000, 0, VRegZero)
					e.PlaceLabel(taken)
					e.Ret(0x2000, 0, VRegZero)
					blk, err := j.jitCompile(&emitResult{block: e.Block, numInsns: 4})
					if err != nil {
						t.Fatal(err)
					}

					var x, f [32]uint64
					var fcsr uint32
					res := jitcallCall(j, blk.fn, &x, &f, &fcsr, mem.Base(), mem.Mask())
					wantPC, wantX10 := uint64(0x1000), uint64(0)
					if tc.want(imm) {
						wantPC, wantX10 = 0x2000, 1
					}
					if res.PC != wantPC || x[10] != wantX10 || x[0] != 0 || res.Status != 0 {
						t.Fatalf("PC=%#x x10=%d x0=%d status=%d; want PC=%#x x10=%d x0=0 status=0",
							res.PC, x[10], x[0], res.Status, wantPC, wantX10)
					}
				})
			}
		}
	}
}

func TestARM64_RV8IC_MatchesInterpreter(t *testing.T) {
	elfPath := filepath.Join(rvTestsDir, "rv64ui-p-add")
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skip("rv64ui-p-add not found")
	}

	interpCPU, interpMem := newARM64ICTestCPU(t, data)
	defer interpMem.Free()
	interpCode, interpErr := RunWithOS(interpCPU)
	if interpErr != nil {
		t.Fatalf("interpreter: %v", interpErr)
	}
	if interpCode != 0 {
		t.Fatalf("interpreter failed: exit %d", interpCode)
	}

	jitCPU, jitMem := newARM64ICTestCPU(t, data)
	defer jitMem.Free()
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	jitCPU.Notes.Push(o.Handle)

	jit := NewSandboxJIT()
	jit.SetRegPolicy(PolicyRV8)

	err = jit.RunJIT(jitCPU)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 0 {
			t.Fatalf("RV8 JIT failed: exit %d", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RV8 JIT: %v", err)
	}

	interpIC := interpCPU.RiscvInstrBegun()
	jitIC := jitCPU.RiscvInstrBegun()
	t.Logf("interpreter IC=%d, ARM64 RV8 JIT IC=%d", interpIC, jitIC)
	if interpIC == 0 || jitIC == 0 {
		t.Fatalf("zero IC: interpreter=%d jit=%d", interpIC, jitIC)
	}
	if interpIC != jitIC {
		t.Fatalf("IC mismatch: interpreter=%d jit=%d", interpIC, jitIC)
	}
}

func newARM64ICTestCPU(t *testing.T, elfData []byte) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)
	return cpu, mem
}
