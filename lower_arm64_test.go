//go:build arm64

package riscv

import (
	"strings"
	"testing"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

func TestARM64Pool(t *testing.T) {
	pool := ARM64Pool(nil)
	if len(pool.IntRegs) != 13 {
		t.Fatalf("ARM64Pool IntRegs len = %d, want 13", len(pool.IntRegs))
	}
	if len(pool.FPRegs) != 21 {
		t.Fatalf("ARM64Pool FPRegs len = %d, want 21", len(pool.FPRegs))
	}
	if pool.NoArchFP {
		t.Fatal("ARM64Pool(nil) should permit guest FP caching for F64-only blocks")
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
	reservedFP := map[int16]string{
		goasm.REG_ARM64_F0:  "F0/stage",
		goasm.REG_ARM64_F1:  "F1/stage",
		goasm.REG_ARM64_F2:  "F2/stage",
		goasm.REG_ARM64_F8:  "F8/C ABI callee-save",
		goasm.REG_ARM64_F9:  "F9/C ABI callee-save",
		goasm.REG_ARM64_F10: "F10/C ABI callee-save",
		goasm.REG_ARM64_F11: "F11/C ABI callee-save",
		goasm.REG_ARM64_F12: "F12/C ABI callee-save",
		goasm.REG_ARM64_F13: "F13/C ABI callee-save",
		goasm.REG_ARM64_F14: "F14/C ABI callee-save",
		goasm.REG_ARM64_F15: "F15/C ABI callee-save",
	}
	for _, r := range pool.FPRegs {
		if name, ok := reservedFP[r]; ok {
			t.Fatalf("ARM64Pool includes reserved %s", name)
		}
	}
}

func TestARM64Pool_HostCallDisablesFPTemps(t *testing.T) {
	e := NewEmitter(nil)
	e.Call("test_call", 0x1000)
	e.Ret(0x1004, 0, VRegZero)

	pool := ARM64Pool(e.Block)
	if len(pool.FPRegs) != 0 {
		t.Fatalf("ARM64Pool host-call FPRegs len = %d, want 0", len(pool.FPRegs))
	}
}

func TestARM64Allocator_F64GuestFPCached(t *testing.T) {
	e := NewEmitter(nil)
	a := e.Tmp()
	b := e.Tmp()
	d := e.Tmp()
	e.MovT(a, e.XReg(10), F64)
	e.MovT(b, e.XReg(11), F64)
	e.FAdd(d, a, b, F64)
	e.MovT(e.XReg(12), d, I64)
	e.FAdd(e.FRegV(10), e.FRegV(11), e.FRegV(12), F64)
	e.Ret(0x1004, 0, VRegZero)
	MaxVReg(e.Block)

	alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
	for _, vr := range []VReg{a, b, d} {
		host, ok := arm64TestAllocHost(alloc, vr)
		if !ok {
			t.Fatalf("%s was not allocated to a host register", vr)
		}
		if !arm64IsFPReg(host) {
			t.Fatalf("%s host = %d, want ARM64 FP register", vr, host)
		}
	}
	for _, vr := range []VReg{e.FRegV(10), e.FRegV(11), e.FRegV(12)} {
		host, ok := arm64TestAllocHost(alloc, vr)
		if !ok {
			t.Fatalf("guest %s was not allocated to a host FP register", vr)
		}
		if !arm64IsFPReg(host) {
			t.Fatalf("guest %s host = %d, want ARM64 FP register", vr, host)
		}
	}
}

func TestARM64Allocator_F32GuestFPMemoryBacked(t *testing.T) {
	e := NewEmitter(nil)
	e.FAdd(e.FRegV(10), e.FRegV(11), e.FRegV(12), F32)
	e.Ret(0x1004, 0, VRegZero)
	MaxVReg(e.Block)

	pool := ARM64Pool(e.Block)
	if !pool.NoArchFP {
		t.Fatal("F32 block should keep guest f0..f31 memory-backed until host-register boxing is modeled")
	}
	alloc := helperTestAllocate(e.Block, pool, ARM64Pinned(), nil)
	for _, vr := range []VReg{e.FRegV(10), e.FRegV(11), e.FRegV(12)} {
		if int(vr) < len(alloc.Kind) && alloc.Kind[vr] == AllocReg {
			t.Fatalf("guest %s should remain memory-backed for F32 blocks", vr)
		}
	}
}

func arm64TestAllocHost(alloc *Allocation, vr VReg) (int16, bool) {
	for i := range alloc.IntervalMap {
		if alloc.IntervalMap[i].Interval.VReg == vr {
			return alloc.IntervalMap[i].Host, true
		}
	}
	return 0, false
}

func TestARM64EntryLoadAnalysis_WriteBeforeRead(t *testing.T) {
	e := NewEmitter(nil)
	e.Const(e.XReg(10), 7)
	e.Add(e.XReg(11), e.XReg(10), VRegZero)
	e.Ret(0x1004, 0, VRegZero)

	loadAll, liveIn := arm64EntryLoadAnalysis(e.Block)
	if loadAll {
		t.Fatal("straight-line write-before-read block should not require conservative entry loads")
	}
	for _, vr := range []VReg{e.XReg(10), e.XReg(11)} {
		if liveIn[vr] {
			t.Fatalf("%s marked live-in after write-before-read", vr)
		}
	}
}

func TestARM64EntryLoadAnalysis_ReadBeforeWrite(t *testing.T) {
	e := NewEmitter(nil)
	e.Add(e.XReg(10), e.XReg(10), e.XReg(11))
	e.Ret(0x1004, 0, VRegZero)

	loadAll, liveIn := arm64EntryLoadAnalysis(e.Block)
	if loadAll {
		t.Fatal("straight-line read-before-write block should not require conservative entry loads")
	}
	for _, vr := range []VReg{e.XReg(10), e.XReg(11)} {
		if !liveIn[vr] {
			t.Fatalf("%s not marked live-in before first write", vr)
		}
	}
}

func TestARM64EntryLoadAnalysis_StoreXReadsDst(t *testing.T) {
	e := NewEmitter(nil)
	e.StoreX(e.XReg(1), e.XReg(2), 8, e.XReg(3), I64)
	e.Ret(0x1004, 0, VRegZero)

	loadAll, liveIn := arm64EntryLoadAnalysis(e.Block)
	if loadAll {
		t.Fatal("straight-line StoreX block should not require conservative entry loads")
	}
	for _, vr := range []VReg{e.XReg(1), e.XReg(2), e.XReg(3)} {
		if !liveIn[vr] {
			t.Fatalf("%s not marked live-in for StoreX", vr)
		}
	}
}

func TestARM64EntryLoadAnalysis_ControlFlowConservative(t *testing.T) {
	e := NewEmitter(nil)
	done := e.NewLabel()
	e.Branch(e.XReg(10), VRegZero, EQ, done)
	e.Const(e.XReg(10), 7)
	e.PlaceLabel(done)
	e.Ret(0x1004, 0, VRegZero)

	loadAll, _ := arm64EntryLoadAnalysis(e.Block)
	if !loadAll {
		t.Fatal("block with internal control flow should keep conservative entry loads")
	}
}

func TestARM64EntryLoadAnalysis_HostCallConservative(t *testing.T) {
	e := NewEmitter(nil)
	e.Call("test_call", 0x1000)
	e.Const(e.XReg(10), 7)
	e.Ret(0x1004, 0, VRegZero)

	loadAll, _ := arm64EntryLoadAnalysis(e.Block)
	if !loadAll {
		t.Fatal("block with host call should keep conservative entry loads")
	}
}

func TestARM64RegfileWritebackStoreClassifier(t *testing.T) {
	tests := []struct {
		name    string
		ins     IRInstr
		indexed bool
		want    bool
	}{
		{
			name: "x writeback",
			ins:  IRInstr{Op: IRStore, T: I64, A: VRXBase, B: VReg(10), Imm: 80},
			want: true,
		},
		{
			name: "f writeback",
			ins:  IRInstr{Op: IRStore, T: I64, A: VRFBase, B: VReg(32 + 10), Imm: 80},
			want: true,
		},
		{
			name: "wrong offset",
			ins:  IRInstr{Op: IRStore, T: I64, A: VRXBase, B: VReg(10), Imm: 88},
		},
		{
			name: "wrong base",
			ins:  IRInstr{Op: IRStore, T: I64, A: VRMemBase, B: VReg(10), Imm: 80},
		},
		{
			name: "wrong type",
			ins:  IRInstr{Op: IRStore, T: I32, A: VRXBase, B: VReg(10), Imm: 80},
		},
		{
			name:    "indexed store",
			ins:     IRInstr{Op: IRStoreX, T: I64, A: VRXBase, B: VReg(10), Dst: VReg(10), Imm: 80},
			indexed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := arm64IsRegfileWritebackStore(&tt.ins, tt.indexed); got != tt.want {
				t.Fatalf("arm64IsRegfileWritebackStore = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestARM64RegfileLoadClassifier(t *testing.T) {
	tests := []struct {
		name    string
		ins     IRInstr
		indexed bool
		want    bool
	}{
		{
			name: "x load",
			ins:  IRInstr{Op: IRLoad, T: I64, Dst: VReg(10), A: VRXBase, Imm: 80},
			want: true,
		},
		{
			name: "f load",
			ins:  IRInstr{Op: IRLoad, T: I64, Dst: VReg(32 + 10), A: VRFBase, Imm: 80},
			want: true,
		},
		{
			name: "wrong offset",
			ins:  IRInstr{Op: IRLoad, T: I64, Dst: VReg(10), A: VRXBase, Imm: 88},
		},
		{
			name: "wrong base",
			ins:  IRInstr{Op: IRLoad, T: I64, Dst: VReg(10), A: VRMemBase, Imm: 80},
		},
		{
			name: "wrong type",
			ins:  IRInstr{Op: IRLoad, T: I32, Dst: VReg(10), A: VRXBase, Imm: 80},
		},
		{
			name:    "indexed load",
			ins:     IRInstr{Op: IRLoadX, T: I64, Dst: VReg(10), A: VRXBase, B: VReg(10), Imm: 80},
			indexed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := arm64IsRegfileLoad(&tt.ins, tt.indexed); got != tt.want {
				t.Fatalf("arm64IsRegfileLoad = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestARM64DirtyTimelineIsExitSpecific(t *testing.T) {
	b := NewBlock()
	b.Instrs = []IRInstr{
		{Op: IRLoad, T: I64, Dst: VReg(10), A: VRXBase, Imm: 80},
		{Op: IRChainExit, Imm: 0x2000},
		{Op: IRConst, T: I64, Dst: VReg(11), Imm: 7},
		{Op: IRChainExit, Imm: 0x3000},
	}
	MaxVReg(b)

	lc := &lowerARM64Ctx{blk: b}
	lc.collectDirtyArch()

	if lc.dirtyTimeline[1][10] {
		t.Fatal("entry regfile load marked x10 dirty at first exit")
	}
	if lc.dirtyTimeline[1][11] {
		t.Fatal("write after first exit polluted first exit dirty set")
	}
	if !lc.dirtyTimeline[3][11] {
		t.Fatal("x11 write missing from second exit dirty set")
	}
	if lc.dirtyArch[10] {
		t.Fatal("entry regfile load marked x10 dirty in block union")
	}
	if !lc.dirtyArch[11] {
		t.Fatal("x11 write missing from block dirty union")
	}
}

func TestLowerARM64_PairLoadStoreHelpers_Assemble(t *testing.T) {
	ctx := goasm.New(goasm.ARM64)
	ctx.Append(ctx.NewATEXT())
	lc := &lowerARM64Ctx{c: ctx}
	lc.emitLoadPair(a64ABJITBase, 8, goasm.REG_ARM64_R10, goasm.REG_ARM64_R11)
	lc.emitStorePair(goasm.REG_ARM64_R10, goasm.REG_ARM64_R11, a64ABJITBase, 8)
	lc.emitFLoadPair(a64ABJITBase, int64(fpRegOffset)+8, goasm.REG_ARM64_F3, goasm.REG_ARM64_F4)
	lc.emitFStorePair(goasm.REG_ARM64_F3, goasm.REG_ARM64_F4, a64ABJITBase, int64(fpRegOffset)+8)

	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("expected non-empty code")
	}
	dump := ctx.DumpProgs()
	for _, want := range []string{"LDP", "STP", "FLDPD", "FSTPD"} {
		if !strings.Contains(dump, want) {
			t.Fatalf("DumpProgs missing %s:\n%s", want, dump)
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

func TestLowerARM64_CompareNegativeImmediate_Assemble(t *testing.T) {
	e := NewEmitter(nil)
	done := e.NewLabel()
	e.SetImm(e.XReg(10), VRegZero, -1, LT)
	e.BranchImm(VRegZero, -1, LT, done)
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

func TestLowerARM64_FPTempRegs_Assemble(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
	}{
		{name: "f32", typ: F32},
		{name: "f64", typ: F64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmitter(nil)
			a := e.Tmp()
			b := e.Tmp()
			d := e.Tmp()
			e.MovT(a, e.XReg(10), tt.typ)
			e.MovT(b, e.XReg(11), tt.typ)
			e.FAdd(d, a, b, tt.typ)
			e.MovT(e.XReg(12), d, I64)
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

func TestLowerARM64_LiveChainSlot_Assemble(t *testing.T) {
	e := NewEmitter(nil)
	e.ChainExit(0x2000, 0)
	MaxVReg(e.Block)

	alloc := helperTestAllocate(e.Block, ARM64Pool(e.Block), ARM64Pinned(), nil)
	ctx := goasm.New(goasm.ARM64)
	ctx.Append(ctx.NewATEXT())
	res, err := LowerARM64_ABJIT(ctx, e.Block, alloc)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if res.LiveChainEntryProg == nil {
		t.Fatal("expected ARM64 live-chain entry marker")
	}
	if len(res.ChainExits) != 1 {
		t.Fatalf("ChainExits len = %d, want 1", len(res.ChainExits))
	}
	if res.ChainExits[0].LiveMovProg == nil {
		t.Fatal("expected ARM64 live-chain patch slot")
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("expected non-empty code")
	}
}

func TestLowerARM64_LiveChainEntry_DisabledForFrame(t *testing.T) {
	e := NewEmitter(nil)
	tmp := e.Tmp()
	e.Const(tmp, 1)
	e.ChainExit(0x2000, 0)
	MaxVReg(e.Block)

	ctx := goasm.New(goasm.ARM64)
	ctx.Append(ctx.NewATEXT())
	res, err := LowerARM64_ABJIT(ctx, e.Block, nil)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if res.LiveChainEntryProg != nil {
		t.Fatal("frame-backed ARM64 block must not expose a live-chain entry")
	}
	if res.LiveChain.Enabled {
		t.Fatal("frame-backed ARM64 block must not expose live-chain metadata")
	}
	if len(res.ChainExits) != 1 {
		t.Fatalf("ChainExits len = %d, want 1", len(res.ChainExits))
	}
	if res.ChainExits[0].LiveMovProg == nil {
		t.Fatal("source live-chain patch slot should still be emitted")
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("expected non-empty code")
	}
}

func TestLowerARM64_ALUAddressImm_Assemble(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Emitter)
	}{
		{name: "neg", emit: func(e *Emitter) { e.Neg(e.XReg(10), e.XReg(11)) }},
		{name: "not", emit: func(e *Emitter) { e.Not(e.XReg(10), e.XReg(11)) }},
		{name: "add-from-zero-immediate", emit: func(e *Emitter) { e.AddImm(e.XReg(10), VRegZero, 32) }},
		{name: "sub-from-zero-immediate", emit: func(e *Emitter) { e.SubImm(e.XReg(10), VRegZero, 32) }},
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
