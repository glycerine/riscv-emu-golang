//go:build amd64

package riscv

import (
	"testing"

	"github.com/glycerine/riscv-emu-golang/abjit"
)

func TestABJITDispatchCopiesFPStateForNonFPEntryBlock(t *testing.T) {
	cb, err := abjit.NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	const (
		x5Off = 5 * 8
		f1Off = 256 + 1*8
		inF1  = uint64(0x3eb0000000000000)
		outF1 = uint64(0x4123a917015a22ab)
	)

	cb.LoadFromRBP(abjit.RAX, f1Off)
	cb.StoreToRBP(abjit.RAX, x5Off)
	cb.Movabs(abjit.RAX, outF1)
	cb.StoreToRBP(abjit.RAX, f1Off)
	cb.Exit()

	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	cpu := NewCPU(*mem)
	cpu.f[1] = inF1
	j := NewSandboxJIT()
	defer j.Close()

	blk := &compiledBlock{fn: cb.Addr(), hasFP: false}
	_ = abjitDispatch(blk, cpu, j, 0, 0, 0, 0, 1)

	if cpu.x[5] != inF1 {
		t.Fatalf("native code saw f1=0x%x, want 0x%x", cpu.x[5], inF1)
	}
	if cpu.f[1] != outF1 {
		t.Fatalf("cpu.f[1] after dispatch = 0x%x, want 0x%x", cpu.f[1], outF1)
	}
}
