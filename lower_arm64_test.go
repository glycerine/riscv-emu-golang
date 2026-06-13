//go:build arm64

package riscv

import (
	"testing"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

func TestLowerARM64_IRCall_Assembles(t *testing.T) {
	tests := []struct {
		name  string
		lower func(*goasm.Ctx, *Block, *Allocation) (*LowerResult, error)
	}{
		{name: "rv8", lower: LowerARM64_RV8},
		{name: "abjit", lower: LowerARM64_ABJIT},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmitter(nil)
			e.Call("test_call", 0x1000)
			e.Ret(0x1004, 0, VRegZero)

			alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
			ctx := goasm.New(goasm.ARM64)
			ctx.Append(ctx.NewATEXT())
			if _, err := tt.lower(ctx, e.Block, alloc); err != nil {
				t.Fatalf("lower: %v", err)
			}
			code, err := ctx.Assemble()
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			if len(code) == 0 {
				t.Fatal("expected non-empty code")
			}
		})
	}
}
