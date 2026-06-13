//go:build arm64

package riscv

import (
	"testing"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

func TestARM64Pool(t *testing.T) {
	pool := ARM64Pool(nil)
	if len(pool.IntRegs) != 13 {
		t.Fatalf("ARM64Pool IntRegs len = %d, want 13", len(pool.IntRegs))
	}
	reserved := map[int16]string{
		goasm.REG_ARM64_R16: "R16/IP0",
		goasm.REG_ARM64_R17: "R17/IP1",
		goasm.REG_ARM64_R18: "R18/platform",
		goasm.REG_ARM64_R20: "R20/ABJIT state",
		goasm.REG_ARM64_R27: "R27/REGTMP",
		goasm.REG_ARM64_R28: "R28/g",
		goasm.REG_ARM64_R29: "R29/FP",
		goasm.REG_ARM64_R30: "R30/LR",
	}
	for _, r := range pool.IntRegs {
		if name, ok := reserved[r]; ok {
			t.Fatalf("ARM64Pool includes reserved %s", name)
		}
	}
}

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

func TestLowerARM64_Extensions_Assemble(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Emitter)
	}{
		{name: "sext8", emit: func(e *Emitter) { e.Sext(e.XReg(10), e.XReg(11), I8) }},
		{name: "sext16", emit: func(e *Emitter) { e.Sext(e.XReg(10), e.XReg(11), I16) }},
		{name: "sext32", emit: func(e *Emitter) { e.Sext(e.XReg(10), e.XReg(11), I32) }},
		{name: "zext8", emit: func(e *Emitter) { e.Zext(e.XReg(10), e.XReg(11), I8) }},
		{name: "zext16", emit: func(e *Emitter) { e.Zext(e.XReg(10), e.XReg(11), I16) }},
		{name: "zext32", emit: func(e *Emitter) { e.Zext(e.XReg(10), e.XReg(11), I32) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmitter(nil)
			tt.emit(e)
			e.Ret(0x1004, 0, VRegZero)
			MaxVReg(e.Block)

			alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
			ctx := goasm.New(goasm.ARM64)
			ctx.Append(ctx.NewATEXT())
			if _, err := LowerARM64_ABJIT(ctx, e.Block, alloc); err != nil {
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

func TestLowerARM64_Set_Assemble(t *testing.T) {
	preds := []Pred{EQ, NE, LT, LE, GT, GE, LTU, LEU, GTU, GEU}

	for _, pred := range preds {
		t.Run(pred.String(), func(t *testing.T) {
			e := NewEmitter(nil)
			e.Set(e.XReg(10), e.XReg(11), e.XReg(12), pred)
			e.SetImm(e.XReg(13), e.XReg(14), 7, pred)
			e.Ret(0x1004, 0, VRegZero)
			MaxVReg(e.Block)

			alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
			ctx := goasm.New(goasm.ARM64)
			ctx.Append(ctx.NewATEXT())
			if _, err := LowerARM64_ABJIT(ctx, e.Block, alloc); err != nil {
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

func TestLowerARM64_ALUAddressImm_Assemble(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Emitter)
	}{
		{name: "neg", emit: func(e *Emitter) { e.Neg(e.XReg(10), e.XReg(11)) }},
		{name: "not", emit: func(e *Emitter) { e.Not(e.XReg(10), e.XReg(11)) }},
		{name: "add-negative", emit: func(e *Emitter) { e.AddImm(e.XReg(10), e.XReg(11), -16) }},
		{name: "sub-negative", emit: func(e *Emitter) { e.SubImm(e.XReg(10), e.XReg(11), -16) }},
		{name: "load-negative-offset", emit: func(e *Emitter) { e.Load(e.XReg(10), e.XReg(11), -8, I64, false) }},
		{name: "store-negative-offset", emit: func(e *Emitter) { e.Store(e.XReg(11), -8, e.XReg(10), I64) }},
		{name: "misaligned-load", emit: func(e *Emitter) { e.MisalignedLoad(e.XReg(10), e.XReg(11), I64) }},
		{name: "misaligned-store", emit: func(e *Emitter) { e.MisalignedStore(e.XReg(11), e.XReg(10), I64) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmitter(nil)
			tt.emit(e)
			e.Ret(0x1004, 0, VRegZero)
			MaxVReg(e.Block)

			alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
			ctx := goasm.New(goasm.ARM64)
			ctx.Append(ctx.NewATEXT())
			if _, err := LowerARM64_ABJIT(ctx, e.Block, alloc); err != nil {
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

func TestARM64LogicalImmEncodable(t *testing.T) {
	tests := []struct {
		imm  uint64
		want bool
	}{
		{imm: 0, want: false},
		{imm: ^uint64(0), want: false},
		{imm: 0xff, want: true},
		{imm: 0x00ff00ff00ff00ff, want: true},
		{imm: 0x8000000000000001, want: true},
		{imm: 0x123456789abcdef0, want: false},
	}
	for _, tt := range tests {
		if got := arm64LogicalImmEncodable(tt.imm); got != tt.want {
			t.Fatalf("arm64LogicalImmEncodable(%#x) = %v, want %v", tt.imm, got, tt.want)
		}
	}
}

func TestLowerARM64_LogicalImm_Assemble(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Emitter)
	}{
		{name: "and-zero", emit: func(e *Emitter) { e.AndImm(e.XReg(10), e.XReg(11), 0) }},
		{name: "and-all-ones", emit: func(e *Emitter) { e.AndImm(e.XReg(10), e.XReg(11), -1) }},
		{name: "and-bitmask", emit: func(e *Emitter) { e.AndImm(e.XReg(10), e.XReg(11), 0x00ff00ff00ff00ff) }},
		{name: "or-zero", emit: func(e *Emitter) { e.OrImm(e.XReg(10), e.XReg(11), 0) }},
		{name: "or-all-ones", emit: func(e *Emitter) { e.OrImm(e.XReg(10), e.XReg(11), -1) }},
		{name: "or-bitmask", emit: func(e *Emitter) { e.OrImm(e.XReg(10), e.XReg(11), 0x00ff00ff00ff00ff) }},
		{name: "xor-zero", emit: func(e *Emitter) { e.XorImm(e.XReg(10), e.XReg(11), 0) }},
		{name: "xor-all-ones", emit: func(e *Emitter) { e.XorImm(e.XReg(10), e.XReg(11), -1) }},
		{name: "xor-bitmask", emit: func(e *Emitter) { e.XorImm(e.XReg(10), e.XReg(11), 0x00ff00ff00ff00ff) }},
		{name: "xor-fallback", emit: func(e *Emitter) { e.XorImm(e.XReg(10), e.XReg(11), 0x123456789abcdef0) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmitter(nil)
			tt.emit(e)
			e.Ret(0x1004, 0, VRegZero)
			MaxVReg(e.Block)

			alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
			ctx := goasm.New(goasm.ARM64)
			ctx.Append(ctx.NewATEXT())
			if _, err := LowerARM64_ABJIT(ctx, e.Block, alloc); err != nil {
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

func TestLowerARM64_ZeroOperand_Assemble(t *testing.T) {
	e := NewEmitter(nil)
	done := e.NewLabel()
	e.Add(e.XReg(10), e.XReg(11), VRegZero)
	e.Sub(e.XReg(12), VRegZero, e.XReg(13))
	e.And(e.XReg(14), e.XReg(15), VRegZero)
	e.Or(e.XReg(16), VRegZero, e.XReg(17))
	e.Xor(e.XReg(18), e.XReg(19), VRegZero)
	e.Set(e.XReg(20), e.XReg(21), VRegZero, NE)
	e.Store(e.XReg(22), 0, VRegZero, I64)
	e.Branch(e.XReg(23), VRegZero, EQ, done)
	e.Branch(VRegZero, e.XReg(24), NE, done)
	e.BranchImm(e.XReg(25), 0, EQ, done)
	e.BranchImm(e.XReg(26), 0, NE, done)
	e.PlaceLabel(done)
	e.Ret(0x1004, 0, VRegZero)
	MaxVReg(e.Block)

	alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
	ctx := goasm.New(goasm.ARM64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerARM64_ABJIT(ctx, e.Block, alloc); err != nil {
		t.Fatalf("lower: %v", err)
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("expected non-empty code")
	}
}
