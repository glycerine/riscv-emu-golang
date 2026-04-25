package ir

import (
	"testing"
	"unsafe"
)

func TestVRegConstants(t *testing.T) {
	if VRegZero != 0 {
		t.Errorf("VRegZero = %d, want 0", VRegZero)
	}
	if VRegTempStart != 64 {
		t.Errorf("VRegTempStart = %d, want 64", VRegTempStart)
	}
}

func TestVRegString(t *testing.T) {
	tests := []struct {
		v    VReg
		want string
	}{
		{VRegZero, "v0"},
		{VReg(1), "x1"},
		{VReg(31), "x31"},
		{VReg(32), "f0"},
		{VReg(63), "f31"},
		{VReg(64), "t64"},
		{VReg(100), "t100"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("VReg(%d).String() = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestTypeConstants(t *testing.T) {
	// All types are distinct.
	types := []Type{I8, I16, I32, I64, F32, F64}
	seen := make(map[Type]bool)
	for _, ty := range types {
		if seen[ty] {
			t.Errorf("duplicate Type value %d", ty)
		}
		seen[ty] = true
	}
}

func TestTypeSize(t *testing.T) {
	tests := []struct {
		ty   Type
		want int
	}{
		{I8, 1}, {I16, 2}, {I32, 4}, {I64, 8}, {F32, 4}, {F64, 8},
	}
	for _, tt := range tests {
		if got := tt.ty.Size(); got != tt.want {
			t.Errorf("%s.Size() = %d, want %d", tt.ty, got, tt.want)
		}
	}
}

func TestTypeString(t *testing.T) {
	tests := []struct {
		ty   Type
		want string
	}{
		{I8, "i8"}, {I16, "i16"}, {I32, "i32"}, {I64, "i64"},
		{F32, "f32"}, {F64, "f64"},
	}
	for _, tt := range tests {
		if got := tt.ty.String(); got != tt.want {
			t.Errorf("Type(%d).String() = %q, want %q", tt.ty, got, tt.want)
		}
	}
}

func TestPredConstants(t *testing.T) {
	preds := []Pred{EQ, NE, LT, LE, GT, GE, LTU, LEU, GTU, GEU}
	seen := make(map[Pred]bool)
	for _, p := range preds {
		if seen[p] {
			t.Errorf("duplicate Pred value %d", p)
		}
		seen[p] = true
	}
	if len(preds) != 10 {
		t.Errorf("expected 10 predicates, got %d", len(preds))
	}
}

func TestPredString(t *testing.T) {
	tests := []struct {
		p    Pred
		want string
	}{
		{EQ, "eq"}, {NE, "ne"}, {LT, "lt"}, {LE, "le"},
		{GT, "gt"}, {GE, "ge"}, {LTU, "ltu"}, {LEU, "leu"},
		{GTU, "gtu"}, {GEU, "geu"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("Pred(%d).String() = %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestIROpConstants(t *testing.T) {
	if IROpInvalid != 0 {
		t.Errorf("IROpInvalid = %d, want 0", IROpInvalid)
	}
	// All ops should be distinct.
	seen := make(map[IROp]string)
	ops := []struct {
		op   IROp
		name string
	}{
		{IROpInvalid, "invalid"}, {IRLoad, "load"}, {IRStore, "store"},
		{IRLoadX, "loadx"}, {IRStoreX, "storex"},
		{IRAdd, "add"}, {IRAddImm, "add_imm"}, {IRSub, "sub"}, {IRSubImm, "sub_imm"},
		{IRMul, "mul"}, {IRDivS, "divs"}, {IRDivU, "divu"}, {IRRem, "rem"},
		{IRMulHS, "mulhs"}, {IRMulHU, "mulhu"}, {IRMulHSU, "mulhsu"}, {IRNeg, "neg"},
		{IRShl, "shl"}, {IRShlImm, "shl_imm"}, {IRShr, "shr"}, {IRShrImm, "shr_imm"},
		{IRSar, "sar"}, {IRSarImm, "sar_imm"},
		{IRAnd, "and"}, {IRAndImm, "and_imm"}, {IROr, "or"}, {IROrImm, "or_imm"},
		{IRXor, "xor"}, {IRXorImm, "xor_imm"}, {IRNot, "not"},
		{IRSet, "set"}, {IRSetImm, "set_imm"},
		{IRMov, "mov"}, {IRConst, "const"}, {IRSext, "sext"}, {IRZext, "zext"},
		{IRLabel, "label"}, {IRBranch, "branch"}, {IRBranchImm, "branch_imm"},
		{IRJump, "jump"}, {IRCall, "call"}, {IRRet, "ret"},
		{IRFAdd, "fadd"}, {IRFSub, "fsub"}, {IRFMul, "fmul"}, {IRFDiv, "fdiv"},
		{IRFSqrt, "fsqrt"}, {IRFCmp, "fcmp"}, {IRFNeg, "fneg"}, {IRFAbs, "fabs"},
		{IRFCvtToI, "fcvt_to_i"}, {IRFCvtToU, "fcvt_to_u"},
		{IRFCvtFromI, "fcvt_from_i"}, {IRFCvtFromU, "fcvt_from_u"},
		{IRFCvtFF, "fcvt_ff"},
		{IRMarkLive, "mark_live"}, {IRMarkDead, "mark_dead"}, {IRWriteback, "writeback"},
	}
	for _, tt := range ops {
		if prev, ok := seen[tt.op]; ok {
			t.Errorf("IROp %d has duplicate names: %q and %q", tt.op, prev, tt.name)
		}
		seen[tt.op] = tt.name
	}
}

func TestIROpString(t *testing.T) {
	if got := IRAdd.String(); got != "add" {
		t.Errorf("IRAdd.String() = %q, want %q", got, "add")
	}
	if got := IRFCvtFF.String(); got != "fcvt_ff" {
		t.Errorf("IRFCvtFF.String() = %q, want %q", got, "fcvt_ff")
	}
	if got := IROpInvalid.String(); got != "invalid" {
		t.Errorf("IROpInvalid.String() = %q, want %q", got, "invalid")
	}
}

func TestIRInstrZeroValue(t *testing.T) {
	var ins IRInstr
	if ins.Op != IROpInvalid {
		t.Errorf("zero IRInstr.Op = %v, want IROpInvalid", ins.Op)
	}
}

func TestIRInstrNoPointers(t *testing.T) {
	// IRInstr should be a fixed-size value type. Verify its size is reasonable
	// (no hidden slices/maps which would add 24+ bytes each).
	sz := unsafe.Sizeof(IRInstr{})
	// Expected: Op(1) + T(1) + U(1) + Pred(1) + Scale(1) + pad + Dst(2) + A(2) + B(2) + Imm(8) + Imm2(8)
	// Total should be <= 40 bytes.
	if sz > 48 {
		t.Errorf("IRInstr size = %d, expected <= 48 (no slices/maps inside)", sz)
	}
}

func TestIRInstrString(t *testing.T) {
	ins := IRInstr{Op: IRAdd, T: I64, Dst: VReg(1), A: VReg(2), B: VReg(3)}
	got := ins.String()
	if got == "" {
		t.Error("IRInstr.String() returned empty")
	}
	// Verify it contains the op name and register names.
	if got != "add.i64 x1 = x2, x3" {
		t.Errorf("IRInstr.String() = %q, want %q", got, "add.i64 x1 = x2, x3")
	}
}

func TestIRInstrString_Const(t *testing.T) {
	ins := IRInstr{Op: IRConst, T: I64, Dst: VReg(5), Imm: 42}
	want := "const x5 = 42"
	if got := ins.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIRInstrString_Label(t *testing.T) {
	ins := IRInstr{Op: IRLabel, Imm: 3}
	want := "L3:"
	if got := ins.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIRInstrString_Branch(t *testing.T) {
	ins := IRInstr{Op: IRBranch, Pred: NE, A: VReg(1), B: VReg(2), Imm: 5}
	want := "branch.ne x1, x2 -> L5"
	if got := ins.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIRInstrString_Ret(t *testing.T) {
	ins := IRInstr{Op: IRRet, Imm: 0x80001000, Imm2: 3, A: VReg(64)}
	want := "ret pc=2147487744 status=3 fault=t64"
	if got := ins.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNewBlock(t *testing.T) {
	b := NewBlock()
	if b == nil {
		t.Fatal("NewBlock returned nil")
	}
	if b.Labels == nil {
		t.Error("NewBlock().Labels is nil, should be initialized map")
	}
	if len(b.Instrs) != 0 {
		t.Errorf("NewBlock().Instrs has %d entries, want 0", len(b.Instrs))
	}
	if b.NextLabel != 0 {
		t.Errorf("NewBlock().NextLabel = %d, want 0", b.NextLabel)
	}
}

func TestWidthToType(t *testing.T) {
	tests := []struct {
		width int
		want  Type
	}{
		{1, I8}, {2, I16}, {4, I32}, {8, I64},
	}
	for _, tt := range tests {
		if got := WidthToType(tt.width); got != tt.want {
			t.Errorf("WidthToType(%d) = %v, want %v", tt.width, got, tt.want)
		}
	}
}

func TestWidthToType_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("WidthToType(3) should panic")
		}
	}()
	WidthToType(3)
}

func TestCSym(t *testing.T) {
	cs := CSym{Name: "jit_sqrt", Addr: 0x12345678}
	if cs.Name != "jit_sqrt" {
		t.Errorf("CSym.Name = %q", cs.Name)
	}
	if cs.Addr != 0x12345678 {
		t.Errorf("CSym.Addr = %#x", cs.Addr)
	}
}

func TestVRegLiveness(t *testing.T) {
	vl := VRegLiveness{Start: 3, End: 10}
	if vl.Start != 3 || vl.End != 10 {
		t.Errorf("VRegLiveness = %+v", vl)
	}
}
