//go:build arm64

package riscv

// lower_arm64.go — conservative ARM64 backend.
//
// This is deliberately simple: it ignores the fixed allocator's host-register
// choices and stores all temporaries in a native stack frame. Architectural
// x/f registers still live in the normal register file / abjit.State layout.
// Unsupported IR returns an error so the JIT manager falls back to the
// interpreter for that block instead of miscompiling.

import (
	"fmt"
	"runtime"

	"github.com/glycerine/riscv-emu-golang/goasm"
	"github.com/glycerine/riscv-emu-golang/goasm/obj"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/arm64"
)

type arm64ABI uint8

const (
	arm64RV8 arm64ABI = iota
	arm64ABJIT
)

const (
	a64SRet    int16 = goasm.REG_ARM64_R0
	a64XBase   int16 = goasm.REG_ARM64_R1
	a64FBase   int16 = goasm.REG_ARM64_R2
	a64FCSR    int16 = goasm.REG_ARM64_R3
	a64MemBase int16 = goasm.REG_ARM64_R4
	a64MemMask int16 = goasm.REG_ARM64_R5

	a64A int16 = goasm.REG_ARM64_R6
	a64B int16 = goasm.REG_ARM64_R7
	a64C int16 = goasm.REG_ARM64_R8
	a64D int16 = goasm.REG_ARM64_R9

	a64ABJITBase int16 = goasm.REG_ARM64_R20
)

func init() {
	if runtime.GOARCH != "arm64" {
		return
	}
	PolicyRV8 = RegPolicy{
		Name:                  "rv8",
		Arch:                  goasm.ARM64,
		InstructionCounterReg: 0,
		Pool:                  ARM64Pool,
		Pinned:                ARM64Pinned,
		Lower:                 LowerARM64_RV8,
		PatchImm64:            patchARM64Unsupported,
	}
	PolicyABJIT = RegPolicy{
		Name:                  "abjit",
		Arch:                  goasm.ARM64,
		InstructionCounterReg: 0,
		Pool:                  ARM64Pool,
		Pinned:                ARM64Pinned,
		Lower:                 LowerARM64_ABJIT,
		PatchImm64:            patchARM64Unsupported,
	}
}

func ARM64Pool(_ *Block) RegPool {
	intRegs := []int16{
		goasm.REG_ARM64_R6, goasm.REG_ARM64_R7, goasm.REG_ARM64_R8, goasm.REG_ARM64_R9,
		goasm.REG_ARM64_R10, goasm.REG_ARM64_R11, goasm.REG_ARM64_R12, goasm.REG_ARM64_R13,
		goasm.REG_ARM64_R14, goasm.REG_ARM64_R15,
	}
	fpRegs := []int16{
		goasm.REG_ARM64_F0, goasm.REG_ARM64_F1, goasm.REG_ARM64_F2, goasm.REG_ARM64_F3,
		goasm.REG_ARM64_F4, goasm.REG_ARM64_F5, goasm.REG_ARM64_F6, goasm.REG_ARM64_F7,
		goasm.REG_ARM64_F8, goasm.REG_ARM64_F9, goasm.REG_ARM64_F10, goasm.REG_ARM64_F11,
		goasm.REG_ARM64_F12, goasm.REG_ARM64_F13, goasm.REG_ARM64_F14, goasm.REG_ARM64_F15,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

func ARM64Pinned() map[VReg]int16 { return nil }

func patchARM64Unsupported(code []byte, prog *obj.Prog, value uint64) (int, error) {
	return 0, fmt.Errorf("arm64 patch sites are not implemented yet")
}

func LowerARM64_RV8(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	return lowerARM64(ctx, b, arm64RV8)
}

func LowerARM64_ABJIT(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	return lowerARM64(ctx, b, arm64ABJIT)
}

type lowerARM64Ctx struct {
	c              *goasm.Ctx
	blk            *Block
	abi            arm64ABI
	tempSlots      map[VReg]int64
	frameSize      int64
	labelProg      map[Label]*obj.Prog
	pending        map[Label][]*obj.Prog
	chainEntryProg *obj.Prog
}

func lowerARM64(ctx *goasm.Ctx, b *Block, abi arm64ABI) (*LowerResult, error) {
	lc := &lowerARM64Ctx{
		c:         ctx,
		blk:       b,
		abi:       abi,
		tempSlots: make(map[VReg]int64),
		labelProg: make(map[Label]*obj.Prog),
		pending:   make(map[Label][]*obj.Prog),
	}
	lc.collectTemps()
	if n := int64(len(lc.tempSlots) * 8); n > 0 {
		lc.frameSize = (n + 15) &^ 15
	}

	lc.chainEntryProg = lc.c.NewProg()
	lc.chainEntryProg.As = obj.ANOP
	lc.c.Append(lc.chainEntryProg)
	if lc.frameSize != 0 {
		lc.emitRRI(arm64.ASUB, lc.frameSize, goasm.REG_ARM64_RSP, goasm.REG_ARM64_RSP)
	}

	for i := range b.Instrs {
		if err := lc.lowerInstr(&b.Instrs[i]); err != nil {
			return nil, err
		}
	}
	if len(lc.pending) != 0 {
		return nil, fmt.Errorf("arm64 lower: %d unresolved labels", len(lc.pending))
	}
	return &LowerResult{ChainEntryProg: lc.chainEntryProg}, nil
}

func (lc *lowerARM64Ctx) collectTemps() {
	add := func(v VReg) {
		if v >= VRegTempStart {
			if _, ok := lc.tempSlots[v]; !ok {
				lc.tempSlots[v] = int64(len(lc.tempSlots)) * 8
			}
		}
	}
	for i := range lc.blk.Instrs {
		ins := &lc.blk.Instrs[i]
		add(ins.Dst)
		add(ins.A)
		add(ins.B)
		add(ins.C)
	}
}

func (lc *lowerARM64Ctx) emitRR(op obj.As, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitRRR(op obj.As, a, b, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	p.Reg = a
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitRI(op obj.As, imm int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitRRI(op obj.As, imm int64, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitLoad(op obj.As, base int16, off int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = off
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitStore(op obj.As, src, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) loadImm(imm int64, dst int16) {
	lc.emitRI(arm64.AMOVD, imm, dst)
}

func (lc *lowerARM64Ctx) loadV(v VReg, dst int16) error {
	switch {
	case v == VRegZero:
		lc.loadImm(0, dst)
	case v == VRXBase || v == VRRegFile:
		lc.emitRR(arm64.AMOVD, lc.xBaseReg(), dst)
	case v == VRFBase:
		lc.emitRRI(arm64.AADD, fpRegOffset, lc.fBaseReg(), dst)
	case v == VRMemBase:
		lc.loadMemBase(dst)
	case v == VRMemMask:
		lc.loadMemMask(dst)
	case v >= 1 && v < 32:
		lc.emitLoad(arm64.AMOVD, lc.xBaseReg(), int64(v)*8, dst)
	case v >= 32 && v < 64:
		return fmt.Errorf("arm64 lower: FP VReg %s is not implemented", v)
	case v >= VRegTempStart:
		off, ok := lc.tempSlots[v]
		if !ok {
			return fmt.Errorf("arm64 lower: temp %s has no slot", v)
		}
		lc.emitLoad(arm64.AMOVD, goasm.REG_ARM64_RSP, off, dst)
	default:
		lc.loadImm(0, dst)
	}
	return nil
}

func (lc *lowerARM64Ctx) storeV(v VReg, src int16) error {
	switch {
	case v == VRegZero:
		return nil
	case v >= 1 && v < 32:
		lc.emitStore(arm64.AMOVD, src, lc.xBaseReg(), int64(v)*8)
	case v >= 32 && v < 64:
		return fmt.Errorf("arm64 lower: FP VReg %s is not implemented", v)
	case v >= VRegTempStart:
		off, ok := lc.tempSlots[v]
		if !ok {
			return fmt.Errorf("arm64 lower: temp %s has no slot", v)
		}
		lc.emitStore(arm64.AMOVD, src, goasm.REG_ARM64_RSP, off)
	default:
		return fmt.Errorf("arm64 lower: cannot store %s", v)
	}
	return nil
}

func (lc *lowerARM64Ctx) xBaseReg() int16 {
	if lc.abi == arm64ABJIT {
		return a64ABJITBase
	}
	return a64XBase
}

func (lc *lowerARM64Ctx) fBaseReg() int16 {
	if lc.abi == arm64ABJIT {
		return a64ABJITBase
	}
	return a64FBase
}

func (lc *lowerARM64Ctx) loadMemBase(dst int16) {
	if lc.abi == arm64ABJIT {
		lc.emitLoad(arm64.AMOVD, a64ABJITBase, abjitMemBaseOff, dst)
		return
	}
	lc.emitRR(arm64.AMOVD, a64MemBase, dst)
}

func (lc *lowerARM64Ctx) loadMemMask(dst int16) {
	if lc.abi == arm64ABJIT {
		lc.emitLoad(arm64.AMOVD, a64ABJITBase, abjitMemMaskOff, dst)
		return
	}
	lc.emitRR(arm64.AMOVD, a64MemMask, dst)
}

func (lc *lowerARM64Ctx) sretBase() int16 {
	if lc.abi == arm64ABJIT {
		return a64ABJITBase
	}
	return a64SRet
}

func (lc *lowerARM64Ctx) resultOffsets() (pc, status, fault, ic int64) {
	if lc.abi == arm64ABJIT {
		return abjitPCOff, abjitStatusOff, abjitFaultAddrOff, abjitICOff
	}
	return 0, 8, 16, 24
}

const abjitICOff = 600

func (lc *lowerARM64Ctx) lowerInstr(ins *IRInstr) error {
	switch ins.Op {
	case IROpInvalid:
		return fmt.Errorf("arm64 lower: invalid op")
	case IRMov:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		return lc.storeV(ins.Dst, a64A)
	case IRConst:
		lc.loadImm(ins.Imm, a64A)
		return lc.storeV(ins.Dst, a64A)
	case IRSext:
		return lc.lowerSext(ins)
	case IRZext:
		return lc.lowerZext(ins)
	case IRAdd:
		return lc.lowerBinop(ins, arm64.AADD)
	case IRAddImm:
		return lc.lowerBinopImm(ins, arm64.AADD)
	case IRSub:
		return lc.lowerBinop(ins, arm64.ASUB)
	case IRSubImm:
		return lc.lowerBinopImm(ins, arm64.ASUB)
	case IRMul:
		return lc.lowerBinop(ins, arm64.AMUL)
	case IRDivS:
		return lc.lowerBinop(ins, arm64.ASDIV)
	case IRDivU:
		return lc.lowerBinop(ins, arm64.AUDIV)
	case IRRem, IRRemU:
		return lc.lowerRem(ins, ins.Op == IRRem)
	case IRNeg:
		lc.loadImm(0, a64A)
		if err := lc.loadV(ins.A, a64B); err != nil {
			return err
		}
		lc.emitRRR(arm64.ASUB, a64A, a64B, a64A)
		return lc.storeV(ins.Dst, a64A)
	case IRShl:
		return lc.lowerShift(ins, arm64.ALSL)
	case IRShlImm:
		return lc.lowerShiftImm(ins, arm64.ALSL)
	case IRShr:
		return lc.lowerShift(ins, arm64.ALSR)
	case IRShrImm:
		return lc.lowerShiftImm(ins, arm64.ALSR)
	case IRSar:
		return lc.lowerShift(ins, arm64.AASR)
	case IRSarImm:
		return lc.lowerShiftImm(ins, arm64.AASR)
	case IRAnd:
		return lc.lowerBinop(ins, arm64.AAND)
	case IRAndImm:
		return lc.lowerBinopImmViaReg(ins, arm64.AAND)
	case IROr:
		return lc.lowerBinop(ins, arm64.AORR)
	case IROrImm:
		return lc.lowerBinopImmViaReg(ins, arm64.AORR)
	case IRXor:
		return lc.lowerBinop(ins, arm64.AEOR)
	case IRXorImm:
		return lc.lowerBinopImmViaReg(ins, arm64.AEOR)
	case IRNot:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		lc.loadImm(-1, a64B)
		lc.emitRRR(arm64.AEOR, a64A, a64B, a64A)
		return lc.storeV(ins.Dst, a64A)
	case IRClz:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		op := arm64.ACLZ
		if ins.T == I32 {
			op = arm64.ACLZW
		}
		lc.emitRR(op, a64A, a64A)
		return lc.storeV(ins.Dst, a64A)
	case IRCtz:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		lc.emitRR(arm64.ARBIT, a64A, a64A)
		op := arm64.ACLZ
		if ins.T == I32 {
			op = arm64.ACLZW
		}
		lc.emitRR(op, a64A, a64A)
		return lc.storeV(ins.Dst, a64A)
	case IRBswap:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		lc.emitRR(arm64.AREV, a64A, a64A)
		return lc.storeV(ins.Dst, a64A)
	case IRSet:
		return lc.lowerSet(ins, false)
	case IRSetImm:
		return lc.lowerSet(ins, true)
	case IRLoad:
		return lc.lowerLoad(ins, false)
	case IRStore:
		return lc.lowerStore(ins, false)
	case IRLoadX:
		return lc.lowerLoad(ins, true)
	case IRStoreX:
		return lc.lowerStore(ins, true)
	case IRLabel:
		lc.placeLabel(Label(ins.Imm))
		return nil
	case IRBranch:
		return lc.lowerBranch(ins, false)
	case IRBranchImm:
		return lc.lowerBranch(ins, true)
	case IRJump:
		lc.emitBranch(arm64.AB, Label(ins.Imm))
		return nil
	case IRRet:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		lc.emitResultImm(ins.Imm, ins.Imm2, a64A)
		lc.emitReturn()
		return nil
	case IRRetDyn:
		if err := lc.loadV(ins.A, a64A); err != nil {
			return err
		}
		if err := lc.loadV(ins.B, a64B); err != nil {
			return err
		}
		lc.emitResultReg(a64A, ins.Imm, a64B)
		lc.emitReturn()
		return nil
	case IRChainExit:
		lc.loadImm(0, a64A)
		lc.emitResultImm(ins.Imm, jitOK, a64A)
		lc.emitReturn()
		return nil
	case IRSyscall:
		lc.loadImm(0, a64A)
		lc.emitResultImm(ins.Imm, jitEcall, a64A)
		lc.emitReturn()
		return nil
	case IRZeroIC:
		lc.storeICImm(0)
		return nil
	case IRIncIC:
		return lc.addIC(ins.Imm)
	case IRSpillIC, IRLoadIC, IRDecIC, IRRegBudget, IRMemBudget, IRMarkLive, IRMarkDead, IRWriteback:
		return nil
	case IRStopperLoad:
		lc.loadImm(ins.Imm, a64A)
		lc.emitLoad(arm64.AMOVD, a64A, 0, a64A)
		return nil
	case IRSetPC, IRRetBudget:
		return fmt.Errorf("arm64 lower: %s is not implemented", ins.Op)
	default:
		return fmt.Errorf("arm64 lower: %s is not implemented", ins.Op)
	}
}

func (lc *lowerARM64Ctx) lowerBinop(ins *IRInstr, op obj.As) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if err := lc.loadV(ins.B, a64B); err != nil {
		return err
	}
	lc.emitRRR(op, a64A, a64B, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerBinopImm(ins *IRInstr, op obj.As) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	lc.emitRRI(op, ins.Imm, a64A, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerBinopImmViaReg(ins *IRInstr, op obj.As) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	lc.loadImm(ins.Imm, a64B)
	lc.emitRRR(op, a64A, a64B, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerRem(ins *IRInstr, signed bool) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if err := lc.loadV(ins.B, a64B); err != nil {
		return err
	}
	divOp := arm64.AUDIV
	if signed {
		divOp = arm64.ASDIV
	}
	lc.emitRRR(divOp, a64A, a64B, a64C)              // q = a / b
	lc.emitRRRR(arm64.AMSUB, a64C, a64B, a64A, a64A) // a - q*b
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) emitRRRR(op obj.As, a, b, c, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a
	p.Reg = b
	p.AddRestSource(obj.Addr{Type: obj.TYPE_REG, Reg: c})
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) lowerShift(ins *IRInstr, op obj.As) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if err := lc.loadV(ins.B, a64B); err != nil {
		return err
	}
	lc.emitRRR(op, a64A, a64B, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerShiftImm(ins *IRInstr, op obj.As) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	lc.emitRRI(op, ins.Imm, a64A, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerSext(ins *IRInstr) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	var sh int64
	switch ins.T {
	case I8:
		sh = 56
	case I16:
		sh = 48
	case I32:
		sh = 32
	default:
		return lc.storeV(ins.Dst, a64A)
	}
	lc.emitRRI(arm64.ALSL, sh, a64A, a64A)
	lc.emitRRI(arm64.AASR, sh, a64A, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerZext(ins *IRInstr) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	var mask int64
	switch ins.T {
	case I8:
		mask = 0xff
	case I16:
		mask = 0xffff
	case I32:
		mask = 0xffffffff
	default:
		return lc.storeV(ins.Dst, a64A)
	}
	lc.loadImm(mask, a64B)
	lc.emitRRR(arm64.AAND, a64A, a64B, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerLoad(ins *IRInstr, indexed bool) error {
	if err := lc.effectiveAddr(ins, indexed, a64A); err != nil {
		return err
	}
	op := arm64.AMOVD
	switch ins.T {
	case I8:
		op = arm64.AMOVBU
	case I16:
		op = arm64.AMOVHU
	case I32:
		op = arm64.AMOVWU
	case I64:
		op = arm64.AMOVD
	default:
		return fmt.Errorf("arm64 lower: load type %s is not implemented", ins.T)
	}
	lc.emitLoad(op, a64A, 0, a64B)
	return lc.storeV(ins.Dst, a64B)
}

func (lc *lowerARM64Ctx) lowerStore(ins *IRInstr, indexed bool) error {
	if err := lc.effectiveAddr(ins, indexed, a64A); err != nil {
		return err
	}
	val := ins.B
	if indexed {
		val = ins.Dst
	}
	if err := lc.loadV(val, a64B); err != nil {
		return err
	}
	op := arm64.AMOVD
	switch ins.T {
	case I8:
		op = arm64.AMOVB
	case I16:
		op = arm64.AMOVH
	case I32:
		op = arm64.AMOVW
	case I64:
		op = arm64.AMOVD
	default:
		return fmt.Errorf("arm64 lower: store type %s is not implemented", ins.T)
	}
	lc.emitStore(op, a64B, a64A, 0)
	return nil
}

func (lc *lowerARM64Ctx) effectiveAddr(ins *IRInstr, indexed bool, dst int16) error {
	if err := lc.loadV(ins.A, dst); err != nil {
		return err
	}
	if indexed {
		if err := lc.loadV(ins.B, a64B); err != nil {
			return err
		}
		if ins.Scale != 0 {
			lc.emitRRI(arm64.ALSL, int64(ins.Scale), a64B, a64B)
		}
		lc.emitRRR(arm64.AADD, dst, a64B, dst)
	} else if ins.Imm != 0 {
		lc.loadImm(ins.Imm, a64B)
		lc.emitRRR(arm64.AADD, dst, a64B, dst)
	}
	lc.loadMemMask(a64B)
	lc.emitRRR(arm64.AAND, dst, a64B, dst)
	lc.loadMemBase(a64B)
	lc.emitRRR(arm64.AADD, dst, a64B, dst)
	return nil
}

func (lc *lowerARM64Ctx) lowerSet(ins *IRInstr, imm bool) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if imm {
		lc.cmpImm(a64A, ins.Imm)
	} else {
		if err := lc.loadV(ins.B, a64B); err != nil {
			return err
		}
		lc.cmp(a64A, a64B)
	}
	lc.loadImm(0, a64C)
	skip := lc.c.NewProg()
	skip.As = invertBranch(predBranch(ins.Pred))
	skip.To.Type = obj.TYPE_BRANCH
	lc.c.Append(skip)
	lc.loadImm(1, a64C)
	done := lc.c.NewProg()
	done.As = obj.ANOP
	skip.To.SetTarget(done)
	lc.c.Append(done)
	return lc.storeV(ins.Dst, a64C)
}

func (lc *lowerARM64Ctx) lowerBranch(ins *IRInstr, imm bool) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if imm {
		lc.cmpImm(a64A, ins.Imm2)
	} else {
		if err := lc.loadV(ins.B, a64B); err != nil {
			return err
		}
		lc.cmp(a64A, a64B)
	}
	lc.emitBranch(predBranch(ins.Pred), Label(ins.Imm))
	return nil
}

func (lc *lowerARM64Ctx) cmp(a, b int16) {
	p := lc.c.NewProg()
	p.As = arm64.ACMP
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	p.Reg = a
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) cmpImm(a int16, imm int64) {
	p := lc.c.NewProg()
	p.As = arm64.ACMP
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.Reg = a
	lc.c.Append(p)
}

func predBranch(p Pred) obj.As {
	switch p {
	case EQ:
		return arm64.ABEQ
	case NE:
		return arm64.ABNE
	case LT:
		return arm64.ABLT
	case LE:
		return arm64.ABLE
	case GT:
		return arm64.ABGT
	case GE:
		return arm64.ABGE
	case LTU:
		return arm64.ABLO
	case LEU:
		return arm64.ABLS
	case GTU:
		return arm64.ABHI
	case GEU:
		return arm64.ABHS
	default:
		return arm64.ABEQ
	}
}

func invertBranch(as obj.As) obj.As {
	switch as {
	case arm64.ABEQ:
		return arm64.ABNE
	case arm64.ABNE:
		return arm64.ABEQ
	case arm64.ABLT:
		return arm64.ABGE
	case arm64.ABLE:
		return arm64.ABGT
	case arm64.ABGT:
		return arm64.ABLE
	case arm64.ABGE:
		return arm64.ABLT
	case arm64.ABLO:
		return arm64.ABHS
	case arm64.ABLS:
		return arm64.ABHI
	case arm64.ABHI:
		return arm64.ABLS
	case arm64.ABHS:
		return arm64.ABLO
	default:
		return arm64.ABNE
	}
}

func (lc *lowerARM64Ctx) placeLabel(l Label) {
	nop := lc.c.NewProg()
	nop.As = obj.ANOP
	lc.c.Append(nop)
	lc.labelProg[l] = nop
	if pends, ok := lc.pending[l]; ok {
		for _, p := range pends {
			p.To.SetTarget(nop)
		}
		delete(lc.pending, l)
	}
}

func (lc *lowerARM64Ctx) emitBranch(as obj.As, l Label) {
	p := lc.c.NewProg()
	p.As = as
	p.To.Type = obj.TYPE_BRANCH
	if target, ok := lc.labelProg[l]; ok {
		p.To.SetTarget(target)
	} else {
		lc.pending[l] = append(lc.pending[l], p)
	}
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitResultImm(pc, status int64, faultReg int16) {
	pcOff, statusOff, faultOff, _ := lc.resultOffsets()
	base := lc.sretBase()
	lc.loadImm(pc, a64C)
	lc.emitStore(arm64.AMOVD, a64C, base, pcOff)
	lc.loadImm(status, a64C)
	lc.emitStore(arm64.AMOVD, a64C, base, statusOff)
	lc.emitStore(arm64.AMOVD, faultReg, base, faultOff)
}

func (lc *lowerARM64Ctx) emitResultReg(pcReg int16, status int64, faultReg int16) {
	pcOff, statusOff, faultOff, _ := lc.resultOffsets()
	base := lc.sretBase()
	lc.emitStore(arm64.AMOVD, pcReg, base, pcOff)
	lc.loadImm(status, a64C)
	lc.emitStore(arm64.AMOVD, a64C, base, statusOff)
	lc.emitStore(arm64.AMOVD, faultReg, base, faultOff)
}

func (lc *lowerARM64Ctx) storeICImm(v int64) {
	_, _, _, icOff := lc.resultOffsets()
	lc.loadImm(v, a64A)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), icOff)
}

func (lc *lowerARM64Ctx) addIC(v int64) error {
	if v == 0 {
		return nil
	}
	_, _, _, icOff := lc.resultOffsets()
	lc.emitLoad(arm64.AMOVD, lc.sretBase(), icOff, a64A)
	lc.emitRRI(arm64.AADD, v, a64A, a64A)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), icOff)
	return nil
}

func (lc *lowerARM64Ctx) emitReturn() {
	if lc.frameSize != 0 {
		lc.emitRRI(arm64.AADD, lc.frameSize, goasm.REG_ARM64_RSP, goasm.REG_ARM64_RSP)
	}
	lc.c.Append(lc.c.NewRET())
}
