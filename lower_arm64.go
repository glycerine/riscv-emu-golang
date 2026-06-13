//go:build arm64

package riscv

// lower_arm64.go — conservative ARM64 backend.
//
// The first ARM64 performance path honors fixed allocator choices for a small
// caller-scratch integer register pool, while FP values and unallocated temps
// still use the native stack frame. Architectural state remains in the normal
// register file / abjit.State layout at block boundaries. Unsupported IR
// returns an error so the JIT manager falls back to the interpreter for that
// block instead of miscompiling.

import (
	"encoding/binary"
	"fmt"
	"runtime"

	"github.com/glycerine/riscv-emu-golang/abjit"
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

	a64A    int16 = goasm.REG_ARM64_R6
	a64B    int16 = goasm.REG_ARM64_R7
	a64C    int16 = goasm.REG_ARM64_R8
	a64D    int16 = goasm.REG_ARM64_R9
	a64Call int16 = goasm.REG_ARM64_R16
	a64IC   int16 = goasm.REG_ARM64_R15

	a64ABJITBase int16 = goasm.REG_ARM64_R20

	a64FA int16 = goasm.REG_ARM64_F0
	a64FB int16 = goasm.REG_ARM64_F1
	a64FC int16 = goasm.REG_ARM64_F2
)

// ARM64 JIT ABI:
//
//	RV8:   R0=sret, R1=x, R2=f, R3=fcsr, R4=memBase, R5=memMask.
//	ABJIT: R20=abjit.State, whose prefix is the x/f/fcsr register file.
//	       R4/R5 are loaded from State once per block as memBase/memMask.
//	Both:  R15 is the relative instruction counter while IC tracking is on.
//
// Go reserves R28 for g on arm64; R30 is LR and must be preserved across any
// helper CALL if generated code may later return through the RV8 trampoline.
func init() {
	if runtime.GOARCH != "arm64" {
		return
	}
	PolicyRV8 = RegPolicy{
		Name:                  "rv8",
		Arch:                  goasm.ARM64,
		InstructionCounterReg: a64IC,
		Pool:                  ARM64Pool,
		Pinned:                ARM64Pinned,
		Lower:                 LowerARM64_RV8,
		PatchImm64:            patchARM64LiteralImm64,
	}
	PolicyABJIT = RegPolicy{
		Name:                  "abjit",
		Arch:                  goasm.ARM64,
		InstructionCounterReg: a64IC,
		Pool:                  ARM64Pool,
		Pinned:                ARM64Pinned,
		Lower:                 LowerARM64_ABJIT,
		PatchImm64:            patchARM64LiteralImm64,
	}
}

func ARM64Pool(_ *Block) RegPool {
	// R6-R9 are fixed staging registers in this lowerer, R16/R17 are ARM64
	// intra-procedure call temporaries, R20 carries the ABJIT State pointer,
	// R27 is Go's linker scratch register, R28 is g, R29 is FP, and R30 is LR.
	// R18 is the platform register on some targets, so keep it out of the pool
	// even though linux/arm64 does not currently use it. R15 is available only
	// when the JIT's IC register path is disabled; jitCompile removes
	// RegPolicy.InstructionCounterReg from this pool for normal IC-counted
	// blocks.
	intRegs := []int16{
		goasm.REG_ARM64_R10, goasm.REG_ARM64_R11, goasm.REG_ARM64_R12, goasm.REG_ARM64_R13,
		goasm.REG_ARM64_R14, goasm.REG_ARM64_R15,
		goasm.REG_ARM64_R19, goasm.REG_ARM64_R21, goasm.REG_ARM64_R22, goasm.REG_ARM64_R23,
		goasm.REG_ARM64_R24, goasm.REG_ARM64_R25, goasm.REG_ARM64_R26,
	}
	return RegPool{IntRegs: intRegs}
}

func ARM64Pinned() map[VReg]int16 { return nil }

func patchARM64LiteralImm64(code []byte, prog *obj.Prog, value uint64) (int, error) {
	if prog == nil {
		return 0, fmt.Errorf("nil patch prog")
	}
	patchOff := int(prog.Pc) + 8
	if patchOff < 0 || patchOff+8 > len(code) {
		return 0, fmt.Errorf("patch offset %d outside code length %d", patchOff, len(code))
	}
	binary.LittleEndian.PutUint64(code[patchOff:], value)
	return patchOff, nil
}

func LowerARM64_RV8(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	return lowerARM64(ctx, b, alloc, arm64RV8)
}

func LowerARM64_ABJIT(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	return lowerARM64(ctx, b, alloc, arm64ABJIT)
}

type lowerARM64Ctx struct {
	c              *goasm.Ctx
	blk            *Block
	alloc          *Allocation
	rIdx           regIndex
	idx            int
	abi            arm64ABI
	dirtyArch      [32]bool
	tempSlots      map[VReg]int64
	frame          arm64FrameLayout
	labelProg      map[Label]*obj.Prog
	pending        map[Label][]*obj.Prog
	chainEntryProg *obj.Prog
	chainExits     []chainExitInfo
	jalrICs        []jalrICInfo
}

type arm64FrameLayout struct {
	tempSize    int64
	hostSaveOff int64
	hostSaveLen int64
	frameSize   int64
}

var arm64HostCallSaveRegs = []int16{
	// Fixed JIT ABI registers plus LR. Allocated guest registers are not
	// preserved here: host helpers may freely clobber caller-scratch regs, so
	// syscall/generic-call continuations reload allocated guest regs from the
	// already-committed register file after the call.
	a64SRet,
	a64XBase,
	a64FBase,
	a64FCSR,
	a64MemBase,
	a64MemMask,
	a64ABJITBase,
	goasm.REG_ARM64_R30,
}

func newARM64FrameLayout(tempSlots int, hasHostCall bool) arm64FrameLayout {
	f := arm64FrameLayout{
		tempSize: int64(tempSlots) * 8,
	}
	n := f.tempSize
	if hasHostCall {
		f.hostSaveOff = n
		f.hostSaveLen = int64(len(arm64HostCallSaveRegs)) * 8
		n += f.hostSaveLen
	}
	if n > 0 {
		f.frameSize = (n + 15) &^ 15
	}
	return f
}

func lowerARM64(ctx *goasm.Ctx, b *Block, alloc *Allocation, abi arm64ABI) (*LowerResult, error) {
	lc := &lowerARM64Ctx{
		c:         ctx,
		blk:       b,
		alloc:     alloc,
		abi:       abi,
		tempSlots: make(map[VReg]int64),
		labelProg: make(map[Label]*obj.Prog),
		pending:   make(map[Label][]*obj.Prog),
	}
	if alloc != nil {
		lc.rIdx = buildRegIndex(alloc)
	}
	lc.collectDirtyArch()
	lc.collectTemps()
	lc.frame = newARM64FrameLayout(len(lc.tempSlots), blockHasHostCall(b))

	lc.chainEntryProg = lc.c.NewProg()
	lc.chainEntryProg.As = obj.ANOP
	lc.c.Append(lc.chainEntryProg)
	if lc.frame.frameSize != 0 {
		lc.emitRRI(arm64.ASUB, lc.frame.frameSize, goasm.REG_ARM64_RSP, goasm.REG_ARM64_RSP)
	}
	lc.loadFixedABJITRegs()
	lc.loadAllocatedRegs()

	for i := range b.Instrs {
		lc.idx = i
		if err := lc.lowerInstr(&b.Instrs[i]); err != nil {
			return nil, err
		}
	}
	if len(lc.pending) != 0 {
		return nil, fmt.Errorf("arm64 lower: %d unresolved labels", len(lc.pending))
	}
	for i := range lc.chainExits {
		lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
	}
	result := &LowerResult{ChainEntryProg: lc.chainEntryProg}
	for i := range lc.chainExits {
		result.ChainExits = append(result.ChainExits, ChainExitDesc{
			TargetPC: lc.chainExits[i].targetPC,
			MovProg:  lc.chainExits[i].movProg,
			StubProg: lc.chainExits[i].stubProg,
		})
	}
	for i := range lc.jalrICs {
		result.JalrICs = append(result.JalrICs, JalrICDesc{
			SiteIdx:  lc.jalrICs[i].siteIdx,
			PcMov:    lc.jalrICs[i].pcMov,
			FnMov:    lc.jalrICs[i].fnMov,
			StubProg: lc.jalrICs[i].stubProg,
		})
	}
	return result, nil
}

func (lc *lowerARM64Ctx) collectTemps() {
	add := func(v VReg) {
		if v >= VRegTempStart && !lc.tempHasHostReg(v) {
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

func (lc *lowerARM64Ctx) collectDirtyArch() {
	for i := range lc.blk.Instrs {
		ins := &lc.blk.Instrs[i]
		if !arm64OpWritesDst(ins.Op) {
			continue
		}
		if ins.Dst > VRegZero && ins.Dst < 32 {
			lc.dirtyArch[ins.Dst] = true
		}
	}
}

func arm64OpWritesDst(op IROp) bool {
	switch op {
	case IRMov, IRConst, IRSext, IRZext,
		IRAdd, IRAddImm, IRSub, IRSubImm,
		IRMul, IRMulHS, IRMulHU, IRMulHSU, IRDivS, IRDivU, IRRem, IRRemU,
		IRNeg,
		IRShl, IRShlImm, IRShr, IRShrImm, IRSar, IRSarImm,
		IRAnd, IRAndImm, IROr, IROrImm, IRXor, IRXorImm, IRNot,
		IRClz, IRCtz, IRPopcount, IRBswap,
		IRSet, IRSetImm,
		IRFCmp, IRFCvtToI, IRFCvtToU,
		IRLoad, IRLoadX, IRMisalignLoad:
		return true
	default:
		return false
	}
}

func (lc *lowerARM64Ctx) canUseHostReg(v VReg) bool {
	if v == VRegZero {
		return false
	}
	if v >= 32 && v < 64 {
		return false
	}
	if v >= VRegTempStart && v <= VRRegFile {
		return false
	}
	return v < 32 || v >= VRegTempStart
}

func (lc *lowerARM64Ctx) hostReg(v VReg) int16 {
	if lc.alloc == nil || !lc.canUseHostReg(v) {
		return -1
	}
	if int(v) >= len(lc.alloc.Kind) || lc.alloc.Kind[v] != AllocReg {
		return -1
	}
	if len(lc.rIdx) == 0 {
		return -1
	}
	return lc.rIdx.lookup(v, lc.idx)
}

func (lc *lowerARM64Ctx) tempHasHostReg(v VReg) bool {
	if v < VRegTempStart || v <= VRRegFile {
		return false
	}
	if lc.alloc == nil || int(v) >= len(lc.alloc.Kind) {
		return false
	}
	return lc.alloc.Kind[v] == AllocReg && len(lc.rIdx) != 0
}

func (lc *lowerARM64Ctx) loadAllocatedRegs() {
	for vr := VReg(1); vr < 32; vr++ {
		host := lc.hostReg(vr)
		if host >= 0 {
			lc.emitLoad(arm64.AMOVD, lc.xBaseReg(), int64(vr)*8, host)
		}
	}
}

func (lc *lowerARM64Ctx) storeRegsBack() {
	for vr := VReg(1); vr < 32; vr++ {
		if !lc.dirtyArch[vr] {
			continue
		}
		host := lc.hostReg(vr)
		if host >= 0 {
			lc.emitStore(arm64.AMOVD, host, lc.xBaseReg(), int64(vr)*8)
		}
	}
}

func blockHasHostCall(b *Block) bool {
	for i := range b.Instrs {
		switch b.Instrs[i].Op {
		case IRCall, IRSyscall:
			return true
		}
	}
	return false
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

func (lc *lowerARM64Ctx) emitFLoad(op obj.As, base int16, off int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = off
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitFStore(op obj.As, src, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitWord(word uint32) *obj.Prog {
	p := lc.c.NewProg()
	p.As = arm64.AWORD
	p.To.Type = obj.TYPE_CONST
	p.To.Offset = int64(word)
	lc.c.Append(p)
	return p
}

func (lc *lowerARM64Ctx) loadImm(imm int64, dst int16) {
	lc.emitRI(arm64.AMOVD, imm, dst)
}

func (lc *lowerARM64Ctx) loadFP(v VReg, dst int16, t Type) error {
	op := arm64.AFMOVD
	if t == F32 {
		op = arm64.AFMOVS
	}
	switch {
	case v >= 32 && v < 64:
		base, off := lc.fpMem(v)
		lc.emitFLoad(op, base, off, dst)
	case v >= VRegTempStart:
		off, ok := lc.tempSlots[v]
		if !ok {
			return fmt.Errorf("arm64 lower: temp %s has no slot", v)
		}
		lc.emitFLoad(op, goasm.REG_ARM64_RSP, off, dst)
	default:
		return fmt.Errorf("arm64 lower: cannot load FP %s", v)
	}
	return nil
}

func (lc *lowerARM64Ctx) storeFP(v VReg, src int16, t Type) error {
	op := arm64.AFMOVD
	if t == F32 {
		op = arm64.AFMOVS
	}
	switch {
	case v >= 32 && v < 64:
		base, off := lc.fpMem(v)
		if t == F32 {
			lc.loadImm(0, a64A)
			lc.emitStore(arm64.AMOVD, a64A, base, off)
		}
		lc.emitFStore(op, src, base, off)
	case v >= VRegTempStart:
		off, ok := lc.tempSlots[v]
		if !ok {
			return fmt.Errorf("arm64 lower: temp %s has no slot", v)
		}
		if t == F32 {
			lc.loadImm(0, a64A)
			lc.emitStore(arm64.AMOVD, a64A, goasm.REG_ARM64_RSP, off)
		}
		lc.emitFStore(op, src, goasm.REG_ARM64_RSP, off)
	default:
		return fmt.Errorf("arm64 lower: cannot store FP %s", v)
	}
	return nil
}

func (lc *lowerARM64Ctx) loadV(v VReg, dst int16) error {
	if host := lc.hostReg(v); host >= 0 {
		lc.emitRR(arm64.AMOVD, host, dst)
		return nil
	}
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
		base, off := lc.fpMem(v)
		lc.emitLoad(arm64.AMOVD, base, off, dst)
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
	if host := lc.hostReg(v); host >= 0 {
		if src != host {
			lc.emitRR(arm64.AMOVD, src, host)
		}
		return nil
	}
	switch {
	case v == VRegZero:
		return nil
	case v >= 1 && v < 32:
		lc.emitStore(arm64.AMOVD, src, lc.xBaseReg(), int64(v)*8)
	case v >= 32 && v < 64:
		base, off := lc.fpMem(v)
		lc.emitStore(arm64.AMOVD, src, base, off)
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

func (lc *lowerARM64Ctx) valueReg(v VReg, scratch int16) (int16, error) {
	if v == VRegZero {
		return goasm.REG_ARM64_ZR, nil
	}
	if host := lc.hostReg(v); host >= 0 {
		return host, nil
	}
	if err := lc.loadV(v, scratch); err != nil {
		return 0, err
	}
	return scratch, nil
}

func (lc *lowerARM64Ctx) resultReg(v VReg, scratch int16) (reg int16, direct bool) {
	if host := lc.hostReg(v); host >= 0 {
		return host, true
	}
	return scratch, false
}

func (lc *lowerARM64Ctx) finishResult(v VReg, reg int16, direct bool) error {
	if direct || v == VRegZero {
		return nil
	}
	return lc.storeV(v, reg)
}

func (lc *lowerARM64Ctx) moveV(dst, src VReg) error {
	if dst == VRegZero {
		return nil
	}
	if dstReg, direct := lc.resultReg(dst, a64A); direct {
		srcReg, err := lc.valueReg(src, dstReg)
		if err != nil {
			return err
		}
		if srcReg != dstReg {
			lc.emitRR(arm64.AMOVD, srcReg, dstReg)
		}
		return nil
	}
	if err := lc.loadV(src, a64A); err != nil {
		return err
	}
	return lc.storeV(dst, a64A)
}

func (lc *lowerARM64Ctx) fpMem(v VReg) (base int16, off int64) {
	if lc.abi == arm64ABJIT {
		return a64ABJITBase, int64(fpRegOffset) + int64(v-32)*8
	}
	return a64FBase, int64(v-32) * 8
}

func (lc *lowerARM64Ctx) loadFixedABJITRegs() {
	if lc.abi != arm64ABJIT {
		return
	}
	lc.emitLoad(arm64.AMOVD, a64ABJITBase, abjitMemBaseOff, a64MemBase)
	lc.emitLoad(arm64.AMOVD, a64ABJITBase, abjitMemMaskOff, a64MemMask)
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
	lc.emitRR(arm64.AMOVD, a64MemBase, dst)
}

func (lc *lowerARM64Ctx) loadMemMask(dst int16) {
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
		return lc.moveV(ins.Dst, ins.A)
	case IRConst:
		if ins.Dst == VRegZero {
			return nil
		}
		dst, direct := lc.resultReg(ins.Dst, a64A)
		lc.loadImm(ins.Imm, dst)
		return lc.finishResult(ins.Dst, dst, direct)
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
	case IRMulHS:
		return lc.lowerMulHigh(ins, arm64.ASMULH)
	case IRMulHU:
		return lc.lowerMulHigh(ins, arm64.AUMULH)
	case IRMulHSU:
		return lc.lowerMulHSU(ins)
	case IRDivS:
		return lc.lowerBinop(ins, arm64.ASDIV)
	case IRDivU:
		return lc.lowerBinop(ins, arm64.AUDIV)
	case IRRem, IRRemU:
		return lc.lowerRem(ins, ins.Op == IRRem)
	case IRNeg:
		src, err := lc.valueReg(ins.A, a64B)
		if err != nil {
			return err
		}
		dst, direct := lc.resultReg(ins.Dst, a64A)
		lc.emitRR(arm64.ANEG, src, dst)
		return lc.finishResult(ins.Dst, dst, direct)
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
		return lc.lowerLogicalImm(ins, arm64.AAND)
	case IROr:
		return lc.lowerBinop(ins, arm64.AORR)
	case IROrImm:
		return lc.lowerLogicalImm(ins, arm64.AORR)
	case IRXor:
		return lc.lowerBinop(ins, arm64.AEOR)
	case IRXorImm:
		return lc.lowerLogicalImm(ins, arm64.AEOR)
	case IRNot:
		src, err := lc.valueReg(ins.A, a64A)
		if err != nil {
			return err
		}
		dst, direct := lc.resultReg(ins.Dst, a64A)
		lc.emitRR(arm64.AMVN, src, dst)
		return lc.finishResult(ins.Dst, dst, direct)
	case IRClz:
		src, err := lc.valueReg(ins.A, a64A)
		if err != nil {
			return err
		}
		op := arm64.ACLZ
		if ins.T == I32 {
			op = arm64.ACLZW
		}
		dst, direct := lc.resultReg(ins.Dst, a64A)
		lc.emitRR(op, src, dst)
		return lc.finishResult(ins.Dst, dst, direct)
	case IRCtz:
		src, err := lc.valueReg(ins.A, a64A)
		if err != nil {
			return err
		}
		dst, direct := lc.resultReg(ins.Dst, a64A)
		lc.emitRR(arm64.ARBIT, src, dst)
		op := arm64.ACLZ
		if ins.T == I32 {
			op = arm64.ACLZW
		}
		lc.emitRR(op, dst, dst)
		return lc.finishResult(ins.Dst, dst, direct)
	case IRPopcount:
		return lc.lowerPopcount(ins)
	case IRBswap:
		src, err := lc.valueReg(ins.A, a64A)
		if err != nil {
			return err
		}
		dst, direct := lc.resultReg(ins.Dst, a64A)
		lc.emitRR(arm64.AREV, src, dst)
		return lc.finishResult(ins.Dst, dst, direct)
	case IRSet:
		return lc.lowerSet(ins, false)
	case IRSetImm:
		return lc.lowerSet(ins, true)
	case IRFAdd:
		return lc.lowerFPBinop(ins, arm64.AFADDD, arm64.AFADDS)
	case IRFSub:
		return lc.lowerFPBinop(ins, arm64.AFSUBD, arm64.AFSUBS)
	case IRFMul:
		return lc.lowerFPBinop(ins, arm64.AFMULD, arm64.AFMULS)
	case IRFDiv:
		return lc.lowerFPBinop(ins, arm64.AFDIVD, arm64.AFDIVS)
	case IRFSqrt:
		return lc.lowerFPUnary(ins, arm64.AFSQRTD, arm64.AFSQRTS)
	case IRFma:
		return lc.lowerFMA(ins, arm64.AFMADDD, arm64.AFMADDS)
	case IRFmsub:
		return lc.lowerFMA(ins, arm64.AFNMSUBD, arm64.AFNMSUBS)
	case IRFnmadd:
		return lc.lowerFMA(ins, arm64.AFNMADDD, arm64.AFNMADDS)
	case IRFnmsub:
		return lc.lowerFMA(ins, arm64.AFMSUBD, arm64.AFMSUBS)
	case IRFCmp:
		return lc.lowerFCmp(ins)
	case IRFNeg:
		return lc.lowerFPUnary(ins, arm64.AFNEGD, arm64.AFNEGS)
	case IRFAbs:
		return lc.lowerFPUnary(ins, arm64.AFABSD, arm64.AFABSS)
	case IRFCvtToI:
		return lc.lowerFCvtToInt(ins, false)
	case IRFCvtToU:
		return lc.lowerFCvtToInt(ins, true)
	case IRFCvtFromI:
		return lc.lowerFCvtFromInt(ins, false)
	case IRFCvtFromU:
		return lc.lowerFCvtFromInt(ins, true)
	case IRFCvtFF:
		return lc.lowerFCvtFF(ins)
	case IRLoad:
		return lc.lowerLoad(ins, false)
	case IRStore:
		return lc.lowerStore(ins, false)
	case IRLoadX:
		return lc.lowerLoad(ins, true)
	case IRStoreX:
		return lc.lowerStore(ins, true)
	case IRMisalignLoad:
		return lc.lowerMisalignLoad(ins)
	case IRMisalignStore:
		return lc.lowerMisalignStore(ins)
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
		lc.emitChainExit(uint64(ins.Imm))
		return nil
	case IRJalrIC:
		return lc.jalrIC(ins)
	case IRCall:
		return lc.call(ins)
	case IRSyscall:
		return lc.syscall(ins)
	case IRZeroIC:
		lc.zeroICReg()
		return nil
	case IRIncIC:
		return lc.addICReg(1)
	case IRLoadIC:
		lc.loadICReg()
		return nil
	case IRSpillIC:
		lc.spillICReg()
		return nil
	case IRMarkLive, IRMarkDead, IRWriteback:
		return nil
	case IRDecIC:
		return lc.addICReg(-1)
	case IRRegBudget:
		lc.cmpImm(a64IC, ins.Imm2)
		lc.emitBranch(arm64.ABGE, Label(ins.Dst))
		return nil
	case IRMemAdd:
		return lc.memAdd(ins.Imm, ins.Imm2)
	case IRMemBudget:
		return lc.memBudget(ins)
	case IRStopperLoad:
		lc.loadImm(ins.Imm, a64A)
		lc.emitLoad(arm64.AMOVD, a64A, 0, a64A)
		return nil
	case IRSetPC:
		lc.setPC(ins.Imm)
		return nil
	case IRRetBudget:
		lc.retBudget()
		return nil
	default:
		return fmt.Errorf("arm64 lower: %s is not implemented", ins.Op)
	}
}

func (lc *lowerARM64Ctx) lowerBinop(ins *IRInstr, op obj.As) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	b, err := lc.valueReg(ins.B, a64B)
	if err != nil {
		return err
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	lc.emitRRR(op, a, b, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerBinopImm(ins *IRInstr, op obj.As) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	switch {
	case op == arm64.AADD:
		lc.emitAddImm(a, ins.Imm, dst)
	case op == arm64.ASUB && ins.Imm < 0:
		lc.emitAddImm(a, -ins.Imm, dst)
	default:
		lc.emitRRI(op, ins.Imm, a, dst)
	}
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerLogicalImm(ins *IRInstr, op obj.As) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	switch op {
	case arm64.AAND:
		switch ins.Imm {
		case 0:
			lc.loadImm(0, dst)
			return lc.finishResult(ins.Dst, dst, direct)
		case -1:
			if a != dst {
				lc.emitRR(arm64.AMOVD, a, dst)
			}
			return lc.finishResult(ins.Dst, dst, direct)
		}
	case arm64.AORR:
		switch ins.Imm {
		case 0:
			if a != dst {
				lc.emitRR(arm64.AMOVD, a, dst)
			}
			return lc.finishResult(ins.Dst, dst, direct)
		case -1:
			lc.loadImm(-1, dst)
			return lc.finishResult(ins.Dst, dst, direct)
		}
	case arm64.AEOR:
		switch ins.Imm {
		case 0:
			if a != dst {
				lc.emitRR(arm64.AMOVD, a, dst)
			}
			return lc.finishResult(ins.Dst, dst, direct)
		case -1:
			lc.emitRR(arm64.AMVN, a, dst)
			return lc.finishResult(ins.Dst, dst, direct)
		}
	}
	if arm64LogicalImmEncodable(uint64(ins.Imm)) {
		lc.emitRRI(op, ins.Imm, a, dst)
	} else {
		lc.loadImm(ins.Imm, a64B)
		lc.emitRRR(op, a, a64B, dst)
	}
	return lc.finishResult(ins.Dst, dst, direct)
}

func arm64LogicalImmEncodable(x uint64) bool {
	if x == 0 || x == ^uint64(0) {
		return false
	}
	switch {
	case x != x>>32|x<<32:
	case x != x>>16|x<<48:
		x = uint64(int64(int32(x)))
	case x != x>>8|x<<56:
		x = uint64(int64(int16(x)))
	case x != x>>4|x<<60:
		x = uint64(int64(int8(x)))
	default:
		return true
	}
	return arm64SequenceOfOnes(x) || arm64SequenceOfOnes(^x)
}

func arm64SequenceOfOnes(x uint64) bool {
	y := x & -x
	y += x
	return (y-1)&y == 0
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
	lc.emitRRRR(arm64.AMSUB, a64B, a64A, a64C, a64A) // a - q*b
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerMulHigh(ins *IRInstr, op obj.As) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	b, err := lc.valueReg(ins.B, a64B)
	if err != nil {
		return err
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	lc.emitRRR(op, a, b, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerMulHSU(ins *IRInstr) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if err := lc.loadV(ins.B, a64B); err != nil {
		return err
	}
	lc.emitRRR(arm64.AUMULH, a64A, a64B, a64C)
	lc.emitRRI(arm64.AASR, 63, a64A, a64D)
	lc.emitRRR(arm64.AAND, a64D, a64B, a64D)
	lc.emitRRR(arm64.ASUB, a64C, a64D, a64A)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerPopcount(ins *IRInstr) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	if ins.T == I32 {
		lc.loadImm(0xffffffff, a64B)
		lc.emitRRR(arm64.AAND, a64A, a64B, a64A)
	}

	lc.emitRRI(arm64.ALSR, 1, a64A, a64B)
	lc.loadImm(0x5555555555555555, a64C)
	lc.emitRRR(arm64.AAND, a64B, a64C, a64B)
	lc.emitRRR(arm64.ASUB, a64A, a64B, a64A)

	lc.emitRR(arm64.AMOVD, a64A, a64B)
	lc.emitRRI(arm64.ALSR, 2, a64B, a64B)
	lc.loadImm(0x3333333333333333, a64C)
	lc.emitRRR(arm64.AAND, a64A, a64C, a64A)
	lc.emitRRR(arm64.AAND, a64B, a64C, a64B)
	lc.emitRRR(arm64.AADD, a64A, a64B, a64A)

	lc.emitRR(arm64.AMOVD, a64A, a64B)
	lc.emitRRI(arm64.ALSR, 4, a64B, a64B)
	lc.emitRRR(arm64.AADD, a64A, a64B, a64A)
	lc.loadImm(0x0f0f0f0f0f0f0f0f, a64C)
	lc.emitRRR(arm64.AAND, a64A, a64C, a64A)

	lc.loadImm(0x0101010101010101, a64C)
	lc.emitRRR(arm64.AMUL, a64A, a64C, a64A)
	lc.emitRRI(arm64.ALSR, 56, a64A, a64A)
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
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	b, err := lc.valueReg(ins.B, a64B)
	if err != nil {
		return err
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	lc.emitRRR(op, a, b, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerShiftImm(ins *IRInstr, op obj.As) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	lc.emitRRI(op, ins.Imm, a, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerSext(ins *IRInstr) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	var op obj.As
	switch ins.T {
	case I8:
		op = arm64.ASXTB
	case I16:
		op = arm64.ASXTH
	case I32:
		op = arm64.ASXTW
	default:
		return lc.moveV(ins.Dst, ins.A)
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	lc.emitRR(op, a, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerZext(ins *IRInstr) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	var op obj.As
	switch ins.T {
	case I8:
		op = arm64.AUXTB
	case I16:
		op = arm64.AUXTH
	case I32:
		op = arm64.AUXTW
	default:
		return lc.moveV(ins.Dst, ins.A)
	}
	dst, direct := lc.resultReg(ins.Dst, a64A)
	lc.emitRR(op, a, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerLoad(ins *IRInstr, indexed bool) error {
	if err := lc.hostAddr(ins, indexed, a64A); err != nil {
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
	dst, direct := lc.resultReg(ins.Dst, a64B)
	lc.emitLoad(op, a64A, 0, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) lowerStore(ins *IRInstr, indexed bool) error {
	if err := lc.hostAddr(ins, indexed, a64A); err != nil {
		return err
	}
	val := ins.B
	if indexed {
		val = ins.Dst
	}
	valReg, err := lc.valueReg(val, a64B)
	if err != nil {
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
	lc.emitStore(op, valReg, a64A, 0)
	return nil
}

func (lc *lowerARM64Ctx) hostAddr(ins *IRInstr, indexed bool, dst int16) error {
	if err := lc.loadV(ins.A, dst); err != nil {
		return err
	}
	if indexed {
		if err := lc.loadV(ins.B, a64B); err != nil {
			return err
		}
		if ins.Scale != 0 {
			lc.emitRRI(arm64.ALSL, int64(scaleShift(ins.Scale)), a64B, a64B)
		}
		lc.emitRRR(arm64.AADD, dst, a64B, dst)
	} else if ins.Imm != 0 {
		lc.emitAddImm(dst, ins.Imm, dst)
	}
	return nil
}

func scaleShift(scale uint8) uint8 {
	switch scale {
	case 1:
		return 0
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	default:
		return scale
	}
}

func (lc *lowerARM64Ctx) emitAddImm(src int16, imm int64, dst int16) {
	if imm >= 0 {
		lc.emitRRI(arm64.AADD, imm, src, dst)
	} else {
		lc.emitRRI(arm64.ASUB, -imm, src, dst)
	}
}

func (lc *lowerARM64Ctx) guestByteAddr(addr VReg, add int, dst int16) error {
	if err := lc.loadV(addr, dst); err != nil {
		return err
	}
	if add != 0 {
		lc.emitAddImm(dst, int64(add), dst)
	}
	lc.emitRRR(arm64.AAND, dst, a64MemMask, dst)
	lc.emitRRR(arm64.AADD, dst, a64MemBase, dst)
	return nil
}

func (lc *lowerARM64Ctx) lowerMisalignLoad(ins *IRInstr) error {
	width := typeWidth(ins.T)
	if width <= 0 {
		return fmt.Errorf("arm64 lower: misaligned load type %s is not implemented", ins.T)
	}
	lc.loadImm(0, a64C)
	for i := 0; i < width; i++ {
		if err := lc.guestByteAddr(ins.A, i, a64A); err != nil {
			return err
		}
		lc.emitLoad(arm64.AMOVBU, a64A, 0, a64B)
		if i != 0 {
			lc.emitRRI(arm64.ALSL, int64(i*8), a64B, a64B)
		}
		lc.emitRRR(arm64.AORR, a64C, a64B, a64C)
	}
	return lc.storeV(ins.Dst, a64C)
}

func (lc *lowerARM64Ctx) lowerMisalignStore(ins *IRInstr) error {
	width := typeWidth(ins.T)
	if width <= 0 {
		return fmt.Errorf("arm64 lower: misaligned store type %s is not implemented", ins.T)
	}
	if err := lc.loadV(ins.B, a64C); err != nil {
		return err
	}
	for i := 0; i < width; i++ {
		if err := lc.guestByteAddr(ins.A, i, a64A); err != nil {
			return err
		}
		lc.emitRR(arm64.AMOVD, a64C, a64B)
		if i != 0 {
			lc.emitRRI(arm64.ALSR, int64(i*8), a64B, a64B)
		}
		lc.emitStore(arm64.AMOVB, a64B, a64A, 0)
	}
	return nil
}

func (lc *lowerARM64Ctx) lowerSet(ins *IRInstr, imm bool) error {
	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	if imm {
		lc.cmpImm(a, ins.Imm)
	} else {
		b, err := lc.valueReg(ins.B, a64B)
		if err != nil {
			return err
		}
		lc.cmp(a, b)
	}
	dst, direct := lc.resultReg(ins.Dst, a64C)
	lc.emitCSet(ins.Pred, dst)
	return lc.finishResult(ins.Dst, dst, direct)
}

func (lc *lowerARM64Ctx) emitCSet(pred Pred, dst int16) {
	p := lc.c.NewProg()
	p.As = arm64.ACSET
	p.From.Type = obj.TYPE_SPECIAL
	p.From.Offset = int64(condFromPred(pred))
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) lowerFPBinop(ins *IRInstr, f64op, f32op obj.As) error {
	if err := lc.loadFP(ins.A, a64FA, ins.T); err != nil {
		return err
	}
	if err := lc.loadFP(ins.B, a64FB, ins.T); err != nil {
		return err
	}
	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64FB
	p.Reg = a64FA
	p.To.Type = obj.TYPE_REG
	p.To.Reg = a64FA
	lc.c.Append(p)
	return lc.storeFP(ins.Dst, a64FA, ins.T)
}

func (lc *lowerARM64Ctx) lowerFPUnary(ins *IRInstr, f64op, f32op obj.As) error {
	if err := lc.loadFP(ins.A, a64FA, ins.T); err != nil {
		return err
	}
	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64FA
	p.To.Type = obj.TYPE_REG
	p.To.Reg = a64FA
	lc.c.Append(p)
	return lc.storeFP(ins.Dst, a64FA, ins.T)
}

func (lc *lowerARM64Ctx) lowerFMA(ins *IRInstr, f64op, f32op obj.As) error {
	if err := lc.loadFP(ins.A, a64FA, ins.T); err != nil {
		return err
	}
	if err := lc.loadFP(ins.B, a64FB, ins.T); err != nil {
		return err
	}
	if err := lc.loadFP(ins.C, a64FC, ins.T); err != nil {
		return err
	}
	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64FB // Fm
	p.Reg = a64FC      // Fa
	p.AddRestSource(obj.Addr{Type: obj.TYPE_REG, Reg: a64FA})
	p.To.Type = obj.TYPE_REG
	p.To.Reg = a64FA
	lc.c.Append(p)
	return lc.storeFP(ins.Dst, a64FA, ins.T)
}

func (lc *lowerARM64Ctx) lowerFCmp(ins *IRInstr) error {
	if err := lc.loadFP(ins.A, a64FA, ins.T); err != nil {
		return err
	}
	if err := lc.loadFP(ins.B, a64FB, ins.T); err != nil {
		return err
	}
	op := arm64.AFCMPD
	if ins.T == F32 {
		op = arm64.AFCMPS
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64FB
	p.Reg = a64FA
	lc.c.Append(p)

	lc.loadImm(0, a64A)
	var setBranches []*obj.Prog
	var doneBranches []*obj.Prog
	switch ins.Pred {
	case EQ:
		doneBranches = append(doneBranches, lc.branchTo(arm64.ABVS), lc.branchTo(arm64.ABNE))
		setBranches = append(setBranches, lc.branchTo(arm64.AB))
	case NE:
		setBranches = append(setBranches, lc.branchTo(arm64.ABVS), lc.branchTo(arm64.ABNE))
	case LT:
		setBranches = append(setBranches, lc.branchTo(arm64.ABMI))
	case LE:
		setBranches = append(setBranches, lc.branchTo(arm64.ABLS))
	case GT:
		setBranches = append(setBranches, lc.branchTo(arm64.ABGT))
	case GE:
		setBranches = append(setBranches, lc.branchTo(arm64.ABGE))
	default:
		return fmt.Errorf("arm64 lower: FP predicate %s is not implemented", ins.Pred)
	}

	done := lc.c.NewProg()
	done.As = obj.ANOP
	if len(setBranches) > 0 {
		skipSet := lc.branchTo(arm64.AB)
		setProg := lc.c.NewProg()
		setProg.As = obj.ANOP
		lc.c.Append(setProg)
		for _, br := range setBranches {
			br.To.SetTarget(setProg)
		}
		lc.loadImm(1, a64A)
		lc.c.Append(done)
		skipSet.To.SetTarget(done)
	} else {
		lc.c.Append(done)
	}
	for _, br := range doneBranches {
		br.To.SetTarget(done)
	}
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerFCvtToInt(ins *IRInstr, unsigned bool) error {
	if err := lc.loadFP(ins.A, a64FA, ins.U); err != nil {
		return err
	}
	var op obj.As
	switch {
	case !unsigned && ins.U == F64 && ins.T == I64:
		op = arm64.AFCVTZSD
	case !unsigned && ins.U == F64:
		op = arm64.AFCVTZSDW
	case !unsigned && ins.U == F32 && ins.T == I64:
		op = arm64.AFCVTZSS
	case !unsigned && ins.U == F32:
		op = arm64.AFCVTZSSW
	case unsigned && ins.U == F64 && ins.T == I64:
		op = arm64.AFCVTZUD
	case unsigned && ins.U == F64:
		op = arm64.AFCVTZUDW
	case unsigned && ins.U == F32 && ins.T == I64:
		op = arm64.AFCVTZUS
	case unsigned && ins.U == F32:
		op = arm64.AFCVTZUSW
	default:
		return fmt.Errorf("arm64 lower: FP-to-int conversion %s to %s is not implemented", ins.U, ins.T)
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64FA
	p.To.Type = obj.TYPE_REG
	p.To.Reg = a64A
	lc.c.Append(p)
	return lc.storeV(ins.Dst, a64A)
}

func (lc *lowerARM64Ctx) lowerFCvtFromInt(ins *IRInstr, unsigned bool) error {
	if err := lc.loadV(ins.A, a64A); err != nil {
		return err
	}
	var op obj.As
	switch {
	case !unsigned && ins.U == I64 && ins.T == F64:
		op = arm64.ASCVTFD
	case !unsigned && ins.T == F64:
		op = arm64.ASCVTFWD
	case !unsigned && ins.U == I64 && ins.T == F32:
		op = arm64.ASCVTFS
	case !unsigned && ins.T == F32:
		op = arm64.ASCVTFWS
	case unsigned && ins.U == I64 && ins.T == F64:
		op = arm64.AUCVTFD
	case unsigned && ins.T == F64:
		op = arm64.AUCVTFWD
	case unsigned && ins.U == I64 && ins.T == F32:
		op = arm64.AUCVTFS
	case unsigned && ins.T == F32:
		op = arm64.AUCVTFWS
	default:
		return fmt.Errorf("arm64 lower: int-to-FP conversion %s to %s is not implemented", ins.U, ins.T)
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64A
	p.To.Type = obj.TYPE_REG
	p.To.Reg = a64FA
	lc.c.Append(p)
	return lc.storeFP(ins.Dst, a64FA, ins.T)
}

func (lc *lowerARM64Ctx) lowerFCvtFF(ins *IRInstr) error {
	if err := lc.loadFP(ins.A, a64FA, ins.U); err != nil {
		return err
	}
	var op obj.As
	switch {
	case ins.U == F32 && ins.T == F64:
		op = arm64.AFCVTSD
	case ins.U == F64 && ins.T == F32:
		op = arm64.AFCVTDS
	default:
		return fmt.Errorf("arm64 lower: FP conversion %s to %s is not implemented", ins.U, ins.T)
	}
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = a64FA
	p.To.Type = obj.TYPE_REG
	p.To.Reg = a64FA
	lc.c.Append(p)
	return lc.storeFP(ins.Dst, a64FA, ins.T)
}

func (lc *lowerARM64Ctx) lowerBranch(ins *IRInstr, imm bool) error {
	if reg, as, ok, err := lc.zeroBranch(ins, imm); ok || err != nil {
		if err != nil {
			return err
		}
		lc.emitRegBranch(as, reg, Label(ins.Imm))
		return nil
	}

	a, err := lc.valueReg(ins.A, a64A)
	if err != nil {
		return err
	}
	if imm {
		lc.cmpImm(a, ins.Imm2)
	} else {
		b, err := lc.valueReg(ins.B, a64B)
		if err != nil {
			return err
		}
		lc.cmp(a, b)
	}
	lc.emitBranch(predBranch(ins.Pred), Label(ins.Imm))
	return nil
}

func (lc *lowerARM64Ctx) zeroBranch(ins *IRInstr, imm bool) (int16, obj.As, bool, error) {
	var as obj.As
	switch ins.Pred {
	case EQ:
		as = arm64.ACBZ
	case NE:
		as = arm64.ACBNZ
	default:
		return 0, 0, false, nil
	}
	if imm {
		if ins.Imm2 != 0 {
			return 0, 0, false, nil
		}
		reg, err := lc.valueReg(ins.A, a64A)
		return reg, as, true, err
	}
	if ins.B == VRegZero {
		reg, err := lc.valueReg(ins.A, a64A)
		return reg, as, true, err
	}
	if ins.A == VRegZero {
		reg, err := lc.valueReg(ins.B, a64A)
		return reg, as, true, err
	}
	return 0, 0, false, nil
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

func condFromPred(p Pred) arm64.SpecialOperand {
	switch p {
	case EQ:
		return arm64.SPOP_EQ
	case NE:
		return arm64.SPOP_NE
	case LT:
		return arm64.SPOP_LT
	case LE:
		return arm64.SPOP_LE
	case GT:
		return arm64.SPOP_GT
	case GE:
		return arm64.SPOP_GE
	case LTU:
		return arm64.SPOP_LO
	case LEU:
		return arm64.SPOP_LS
	case GTU:
		return arm64.SPOP_HI
	case GEU:
		return arm64.SPOP_HS
	default:
		return arm64.SPOP_EQ
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

func (lc *lowerARM64Ctx) emitRegBranch(as obj.As, reg int16, l Label) {
	p := lc.c.NewProg()
	p.As = as
	p.From.Type = obj.TYPE_REG
	p.From.Reg = reg
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

func (lc *lowerARM64Ctx) loadIC(dst int16) {
	_, _, _, icOff := lc.resultOffsets()
	lc.emitLoad(arm64.AMOVD, lc.sretBase(), icOff, dst)
}

func (lc *lowerARM64Ctx) zeroICReg() {
	lc.loadImm(0, a64IC)
}

func (lc *lowerARM64Ctx) loadICReg() {
	lc.loadIC(a64IC)
}

func (lc *lowerARM64Ctx) spillICReg() {
	_, _, _, icOff := lc.resultOffsets()
	lc.emitStore(arm64.AMOVD, a64IC, lc.sretBase(), icOff)
}

func (lc *lowerARM64Ctx) addICReg(v int64) error {
	if v == 0 {
		return nil
	}
	if v > 0 {
		lc.emitRRI(arm64.AADD, v, a64IC, a64IC)
	} else {
		lc.emitRRI(arm64.ASUB, -v, a64IC, a64IC)
	}
	return nil
}

func (lc *lowerARM64Ctx) addIC(v int64) error {
	if v == 0 {
		return nil
	}
	_, _, _, icOff := lc.resultOffsets()
	lc.emitLoad(arm64.AMOVD, lc.sretBase(), icOff, a64A)
	if v > 0 {
		lc.emitRRI(arm64.AADD, v, a64A, a64A)
	} else {
		lc.emitRRI(arm64.ASUB, -v, a64A, a64A)
	}
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), icOff)
	return nil
}

func (lc *lowerARM64Ctx) memAdd(off, delta int64) error {
	lc.emitLoad(arm64.AMOVD, lc.sretBase(), off, a64A)
	if delta > 0 {
		lc.emitRRI(arm64.AADD, delta, a64A, a64A)
	} else if delta < 0 {
		lc.emitRRI(arm64.ASUB, -delta, a64A, a64A)
	}
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), off)
	return nil
}

func (lc *lowerARM64Ctx) memBudget(ins *IRInstr) error {
	_, _, _, icOff := lc.resultOffsets()
	if err := lc.memAdd(icOff, ins.Imm); err != nil {
		return err
	}
	lc.loadIC(a64A)
	lc.cmpImm(a64A, ins.Imm2)
	lc.emitBranch(arm64.ABGE, Label(ins.Dst))
	return nil
}

func (lc *lowerARM64Ctx) setPC(pc int64) {
	pcOff, _, _, _ := lc.resultOffsets()
	lc.loadImm(pc, a64A)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), pcOff)
}

func (lc *lowerARM64Ctx) retBudget() {
	_, statusOff, faultOff, _ := lc.resultOffsets()
	lc.loadImm(0, a64A)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), statusOff)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), faultOff)
	lc.emitReturn()
}

func (lc *lowerARM64Ctx) dcOffsets() (dcBase, dcMask, vaddrBegin, segSize int64) {
	if lc.abi == arm64ABJIT {
		return abjitDCBaseOff, abjitDCMaskOff, abjitVAddrBeginOff, abjitSegSizeOff
	}
	return 32, 40, 48, 56
}

func (lc *lowerARM64Ctx) branchTo(as obj.As) *obj.Prog {
	p := lc.c.NewProg()
	p.As = as
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	return p
}

func (lc *lowerARM64Ctx) jalrIC(ins *IRInstr) error {
	if err := lc.loadV(ins.A, a64B); err != nil {
		return err
	}

	// Result.PC/State.PC is the dynamic target on every miss path.
	pcOff, _, _, _ := lc.resultOffsets()
	lc.emitStore(arm64.AMOVD, a64B, lc.sretBase(), pcOff)

	dcBaseOff, dcMaskOff, vaddrBeginOff, segSizeOff := lc.dcOffsets()
	base := lc.sretBase()

	// Decoder-cache fast path for AOT blocks.
	lc.emitLoad(arm64.AMOVD, base, dcBaseOff, a64C)
	lc.cmpImm(a64C, 0)
	miss1 := lc.branchTo(arm64.ABEQ)

	lc.emitRR(arm64.AMOVD, a64B, a64A)                  // offset = target
	lc.emitLoad(arm64.AMOVD, base, vaddrBeginOff, a64D) // vaddrBegin
	lc.emitRRR(arm64.ASUB, a64A, a64D, a64A)            // offset = target - vaddrBegin
	lc.emitLoad(arm64.AMOVD, base, segSizeOff, a64D)
	lc.cmp(a64A, a64D)
	miss2 := lc.branchTo(arm64.ABHS) // unsigned offset >= segSize

	lc.emitRRI(arm64.ALSL, 2, a64A, a64A)
	lc.emitLoad(arm64.AMOVD, base, dcMaskOff, a64D)
	lc.emitRRR(arm64.AAND, a64A, a64D, a64A)
	lc.emitRRR(arm64.AADD, a64C, a64A, a64D)
	lc.emitLoad(arm64.AMOVD, a64D, 0, a64D)
	lc.cmpImm(a64D, 0)
	miss3 := lc.branchTo(arm64.ABEQ)

	lc.storeRegsBack()
	lc.emitDeallocFrame()
	lc.emitIndirectJump(a64D)

	trySlots := lc.c.NewProg()
	trySlots.As = obj.ANOP
	lc.c.Append(trySlots)
	miss1.To.SetTarget(trySlots)
	miss2.To.SetTarget(trySlots)
	miss3.To.SetTarget(trySlots)

	lc.twoSlotJalrIC(int(ins.Imm), a64B)
	return nil
}

func (lc *lowerARM64Ctx) twoSlotJalrIC(siteIdx int, targetReg int16) {
	pcMov0 := lc.emitPatchableLiteralLoad(a64A, nativePatchSentinel)
	lc.cmp(targetReg, a64A)
	hit0 := lc.branchTo(arm64.ABEQ)

	pcMov1 := lc.emitPatchableLiteralLoad(a64A, nativePatchSentinel)
	lc.cmp(targetReg, a64A)
	miss := lc.branchTo(arm64.ABNE)

	// Slot 1 hit.
	lc.storeRegsBack()
	lc.emitDeallocFrame()
	fnMov1 := lc.emitPatchableLiteralLoad(a64A, nativePatchSentinel)
	lc.emitIndirectJump(a64A)

	// Slot 0 hit.
	hit0Prog := lc.c.NewProg()
	hit0Prog.As = obj.ANOP
	lc.c.Append(hit0Prog)
	hit0.To.SetTarget(hit0Prog)
	lc.storeRegsBack()
	lc.emitDeallocFrame()
	fnMov0 := lc.emitPatchableLiteralLoad(a64A, nativePatchSentinel)
	lc.emitIndirectJump(a64A)

	missProg := lc.c.NewProg()
	missProg.As = obj.ANOP
	lc.c.Append(missProg)
	miss.To.SetTarget(missProg)

	lc.jalrICs = append(lc.jalrICs, jalrICInfo{
		siteIdx:  siteIdx,
		pcMov:    [2]*obj.Prog{pcMov0, pcMov1},
		fnMov:    [2]*obj.Prog{fnMov0, fnMov1},
		stubProg: missProg,
	})

	lc.emitJalrMiss(siteIdx)
}

func (lc *lowerARM64Ctx) emitJalrMiss(siteIdx int) {
	_, statusOff, faultOff, _ := lc.resultOffsets()
	lc.spillICReg()
	lc.loadImm(int64(JitOKJalrMiss), a64A)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), statusOff)
	lc.loadImm(int64(siteIdx), a64A)
	lc.emitStore(arm64.AMOVD, a64A, lc.sretBase(), faultOff)
	lc.emitReturn()
}

func (lc *lowerARM64Ctx) saveHostCallState() error {
	if lc.frame.hostSaveLen == 0 {
		return fmt.Errorf("arm64 lower: block has host call but no save area")
	}
	for i, reg := range arm64HostCallSaveRegs {
		lc.emitStore(arm64.AMOVD, reg, goasm.REG_ARM64_RSP, lc.frame.hostSaveOff+int64(i)*8)
	}
	return nil
}

func (lc *lowerARM64Ctx) restoreHostCallState() {
	for i, reg := range arm64HostCallSaveRegs {
		lc.emitLoad(arm64.AMOVD, goasm.REG_ARM64_RSP, lc.frame.hostSaveOff+int64(i)*8, reg)
	}
}

func (lc *lowerARM64Ctx) call(ins *IRInstr) error {
	if int(ins.Imm) < 0 || int(ins.Imm) >= len(lc.blk.CTab) {
		return fmt.Errorf("arm64 lower: CTab index %d out of range (len=%d)", ins.Imm, len(lc.blk.CTab))
	}
	sym := lc.blk.CTab[ins.Imm]

	lc.storeRegsBack()
	lc.spillICReg()
	if err := lc.saveHostCallState(); err != nil {
		return err
	}
	lc.loadImm(int64(sym.Addr), a64Call)
	lc.emitIndirectCall(a64Call)
	lc.restoreHostCallState()
	lc.loadICReg()
	lc.loadAllocatedRegs()
	return nil
}

func (lc *lowerARM64Ctx) syscall(ins *IRInstr) error {
	if int(ins.Imm2) >= len(lc.blk.CTab) {
		lc.loadImm(0, a64A)
		lc.emitResultImm(ins.Imm, jitEcall, a64A)
		lc.emitReturn()
		return nil
	}
	sym := lc.blk.CTab[ins.Imm2]

	lc.storeRegsBack()
	lc.spillICReg()
	if err := lc.saveHostCallState(); err != nil {
		return err
	}
	lc.emitRR(arm64.AMOVD, lc.xBaseReg(), goasm.REG_ARM64_R0)
	lc.loadMemBase(goasm.REG_ARM64_R1)
	lc.loadMemMask(goasm.REG_ARM64_R2)
	lc.loadImm(int64(sym.Addr), a64Call)
	lc.emitIndirectCall(a64Call)
	lc.emitRR(arm64.AMOVD, goasm.REG_ARM64_R0, a64A)
	lc.restoreHostCallState()

	lc.cmpImm(a64A, 0)
	slow := lc.branchTo(arm64.ABNE)

	lc.loadICReg()
	lc.loadAllocatedRegs()
	lc.emitChainExit(uint64(ins.Imm))

	slowProg := lc.c.NewProg()
	slowProg.As = obj.ANOP
	lc.c.Append(slowProg)
	slow.To.SetTarget(slowProg)

	lc.loadAllocatedRegs()
	lc.loadImm(0, a64A)
	lc.emitResultImm(ins.Imm, jitEcall, a64A)
	lc.emitReturn()
	return nil
}

func arm64GPRegNum(reg int16) uint32 {
	return uint32(reg - goasm.REG_ARM64_R0)
}

func arm64LDRLiteral64(rt int16, imm19 int32) uint32 {
	return 0x58000000 | (uint32(imm19)&0x7ffff)<<5 | arm64GPRegNum(rt)
}

func (lc *lowerARM64Ctx) emitPatchableAddrLoad(dst int16, value uint64) *obj.Prog {
	// LDR literal loads the 8-byte slot after the following JMP:
	//
	//   LDR dst, [PC+8]
	//   JMP (dst)
	//   WORD low32
	//   WORD high32
	//
	// PatchImm64 returns the data-slot offset so the shared runtime patcher
	// can keep writing a plain little-endian uint64.
	load := lc.emitWord(arm64LDRLiteral64(dst, 2))
	lc.emitIndirectJump(dst)
	lc.emitWord(uint32(value))
	lc.emitWord(uint32(value >> 32))
	return load
}

func (lc *lowerARM64Ctx) emitPatchableLiteralLoad(dst int16, value uint64) *obj.Prog {
	// LDR literal loads the 8-byte slot after the following branch:
	//
	//   LDR dst, [PC+8]
	//   B afterData
	//   WORD low32
	//   WORD high32
	// afterData:
	//
	// This keeps the same patch offset convention as emitPatchableAddrLoad:
	// PatchImm64 writes the little-endian data slot at load.Pc+8.
	load := lc.emitWord(arm64LDRLiteral64(dst, 2))
	skip := lc.c.NewProg()
	skip.As = arm64.AB
	skip.To.Type = obj.TYPE_BRANCH
	lc.c.Append(skip)
	lc.emitWord(uint32(value))
	lc.emitWord(uint32(value >> 32))
	after := lc.c.NewProg()
	after.As = obj.ANOP
	lc.c.Append(after)
	skip.To.SetTarget(after)
	return load
}

func (lc *lowerARM64Ctx) emitIndirectJump(reg int16) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = reg
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitIndirectCall(reg int16) {
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = reg
	lc.c.Append(p)
}

func (lc *lowerARM64Ctx) emitDeallocFrame() {
	if lc.frame.frameSize != 0 {
		lc.emitRRI(arm64.AADD, lc.frame.frameSize, goasm.REG_ARM64_RSP, goasm.REG_ARM64_RSP)
	}
}

func (lc *lowerARM64Ctx) emitChainExit(targetPC uint64) {
	lc.storeRegsBack()
	lc.emitDeallocFrame()
	load := lc.emitPatchableAddrLoad(a64D, nativePatchSentinel)
	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: targetPC,
		movProg:  load,
	})
}

func (lc *lowerARM64Ctx) emitSlowExitStub(targetPC uint64) *obj.Prog {
	first := lc.c.NewProg()
	first.As = obj.ANOP
	lc.c.Append(first)

	lc.loadImm(0, a64A)
	lc.emitResultImm(int64(targetPC), jitOK, a64A)
	lc.emitReturnFrameFreed()
	return first
}

func (lc *lowerARM64Ctx) emitReturn() {
	lc.storeRegsBack()
	lc.emitDeallocFrame()
	lc.emitReturnFrameFreed()
}

func (lc *lowerARM64Ctx) emitReturnFrameFreed() {
	if lc.abi == arm64ABJIT {
		lc.loadImm(int64(abjit.RetStubAddr()), a64A)
		lc.emitIndirectJump(a64A)
		return
	}
	lc.c.Append(lc.c.NewRET())
}
