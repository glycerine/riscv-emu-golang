package ir

// lower_amd64_v2.go — Clean-room "always-stage" AMD64 lowerer.
//
// Every instruction stages ALL source operands into fixed staging registers
// (R10, R11 for int; XMM15, XMM14 for FP) before touching any destination.
// These staging registers are never in the allocation pool, so aliasing
// between sources, destinations, and implicit x86 registers is impossible
// by construction.

import (
	"fmt"
	"sort"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// Staging register constants (same physical regs as V1 scratch, different role).
const (
	v2StgA  int16 = goasm.REG_AMD64_R10 // integer staging slot A
	v2StgB  int16 = goasm.REG_AMD64_R11 // integer staging slot B
	v2StgFA int16 = goasm.REG_AMD64_X15 // FP staging slot A
	v2StgFB int16 = goasm.REG_AMD64_X14 // FP staging slot B
)

// AMD64Pool_V2 returns the register pool for the V2 lowerer.
// Same integer pool as V1; FP pool excludes XMM14/XMM15 (staging).
func AMD64Pool_V2(b *Block) RegPool {
	intRegs := []int16{
		goasm.REG_AMD64_AX, goasm.REG_AMD64_CX, goasm.REG_AMD64_DX,
		goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8, goasm.REG_AMD64_R9,
	}
	if BlockHasDivMul(b) {
		intRegs = []int16{
			goasm.REG_AMD64_CX, goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
			goasm.REG_AMD64_R8, goasm.REG_AMD64_R9,
		}
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13,
		// XMM14, XMM15 reserved for FP staging
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

// ── V2 lowering context ──

type lowerCtxV2 struct {
	blk   *Block
	alloc *Allocation
	c     *goasm.Ctx
	idx   int

	// Fast per-VReg host register lookup (shared types from lower_amd64.go).
	rIdx   regIndex
	fpSet  map[VReg]bool
	cxLive []regEntry

	labelProg map[Label]*obj.Prog
	pending   map[Label][]*obj.Prog

	stackSlots int
	frameSize  int64
}

// LowerAMD64_V2 converts a register-allocated IR Block into x86-64 machine code
// using the "always-stage" approach.
func LowerAMD64_V2(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	if alloc == nil {
		return nil, fmt.Errorf("ir.LowerAMD64_V2: nil allocation")
	}
	// Build fast lookup indices (shared helpers from lower_amd64.go).
	rIdx := buildRegIndex(alloc)
	fpSet := make(map[VReg]bool)
	var cxLive []regEntry
	for i := range alloc.IntervalMap {
		ia := &alloc.IntervalMap[i]
		if isXMMReg(ia.Host) {
			fpSet[ia.Interval.VReg] = true
		}
		if ia.Host == goasm.REG_AMD64_CX {
			cxLive = append(cxLive, regEntry{
				start: ia.Interval.Start,
				end:   ia.Interval.End,
				host:  ia.Host,
			})
		}
	}
	for vr := VReg(32); vr < 64; vr++ {
		fpSet[vr] = true
	}
	sort.Sort(regEntriesByStart(cxLive))

	lc := &lowerCtxV2{
		blk:        b,
		alloc:      alloc,
		c:          ctx,
		rIdx:       rIdx,
		fpSet:      fpSet,
		cxLive:     cxLive,
		labelProg:  make(map[Label]*obj.Prog),
		pending:    make(map[Label][]*obj.Prog),
		stackSlots: alloc.StackSlots,
	}
	lc.frameSize = int64(lc.stackSlots)*8 + 8
	if lc.stackSlots == 0 {
		lc.frameSize = 0
	}
	lc.emitPrologue()
	for idx := range b.Instrs {
		lc.idx = idx
		if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
			return nil, err
		}
	}
	if len(lc.pending) > 0 {
		return nil, fmt.Errorf("ir.LowerAMD64_V2: %d unresolved forward labels", len(lc.pending))
	}
	return &LowerResult{}, nil
}

// ── Prologue / Epilogue (identical to V1) ──

func (lc *lowerCtxV2) emitPrologue() {
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_SI, amd64RegXBase)
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DX, amd64RegFBase)
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_R8, amd64RegMemBase)
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_R9, amd64RegMemMask)
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DI, amd64RegSret)
	if lc.frameSize > 0 {
		lc.emitRI2(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)
		lc.emitMR2(x86.AMOVQ, goasm.REG_AMD64_CX, goasm.REG_AMD64_SP, int64(lc.stackSlots)*8)
	}
	lc.emit2(x86.AXORQ, amd64RegIC, amd64RegIC)
}

func (lc *lowerCtxV2) emitEpilogue() {
	if lc.frameSize > 0 {
		lc.emitRI2(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)
}

// ── Staging helpers ──

// stageInt loads VReg v into R10 (idx=0) or R11 (idx=1). Always returns the staging reg.
func (lc *lowerCtxV2) stageInt(v VReg, idx int) int16 {
	stg := v2StgA
	if idx != 0 {
		stg = v2StgB
	}
	if v == VRegZero {
		lc.emit2(x86.AXORQ, stg, stg)
		return stg
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		if hr != stg {
			lc.emit2(x86.AMOVQ, hr, stg)
		}
		return stg
	}
	// Stack-allocated.
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[v], stg)
		return stg
	}
	// Fallback: zero.
	lc.emit2(x86.AXORQ, stg, stg)
	return stg
}

// stageFP loads FP VReg v into XMM15 (idx=0) or XMM14 (idx=1).
func (lc *lowerCtxV2) stageFP(v VReg, idx int) int16 {
	stg := v2StgFA
	if idx != 0 {
		stg = v2StgFB
	}
	if v == VRegZero {
		lc.emit2(x86.APXOR, stg, stg)
		return stg
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		lc.emit2(x86.AMOVSD, hr, stg)
		return stg
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		lc.loadFPSpill(lc.alloc.SpillSlot[v], stg)
		return stg
	}
	lc.emit2(x86.APXOR, stg, stg)
	return stg
}

// writeDst returns the host register for an integer destination.
// If Dst is in a register, returns it. If spilled, returns R10 (safe — sources consumed).
func (lc *lowerCtxV2) writeDst(v VReg) int16 {
	if v == VRegZero {
		return v2StgA // writes discarded
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		return hr
	}
	return v2StgA
}

// writeDstFP returns the host register for an FP destination.
func (lc *lowerCtxV2) writeDstFP(v VReg) int16 {
	if v == VRegZero {
		return v2StgFA
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		return hr
	}
	return v2StgFA
}

// commitDst spills back to stack if needed.
func (lc *lowerCtxV2) commitDst(v VReg, hostReg int16) {
	if v == VRegZero {
		return
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if isXMMReg(hostReg) {
			lc.storeFPSpill(hostReg, lc.alloc.SpillSlot[v])
		} else {
			lc.storeSpill(hostReg, lc.alloc.SpillSlot[v])
		}
	}
}

// ── Register resolution ──

func (lc *lowerCtxV2) hostReg(v VReg) int16 {
	if v == VRegZero || int(v) >= len(lc.alloc.Kind) {
		return -1
	}
	if lc.alloc.Kind[v] != AllocReg {
		return -1
	}
	return lc.rIdx.lookup(v, lc.idx)
}

func (lc *lowerCtxV2) isCXLive() bool {
	idx := lc.idx
	lo, hi := 0, len(lc.cxLive)
	for lo < hi {
		mid := (lo + hi) / 2
		if lc.cxLive[mid].end < idx {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo < len(lc.cxLive) && lc.cxLive[lo].start <= idx && idx <= lc.cxLive[lo].end
}

func (lc *lowerCtxV2) isRegLive(hostReg int16) bool {
	// Check all VRegs for any interval assigned to hostReg containing lc.idx.
	for vr := 0; vr < len(lc.rIdx); vr++ {
		for _, e := range lc.rIdx[VReg(vr)] {
			if e.host == hostReg && e.start <= lc.idx && lc.idx <= e.end {
				return true
			}
		}
	}
	return false
}

// ── Emission helpers ──

func (lc *lowerCtxV2) emit2(op obj.As, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxV2) emitRI2(op obj.As, imm int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxV2) emitRM2(op obj.As, base int16, off int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = off
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxV2) emitMR2(op obj.As, src int16, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerCtxV2) emitMI2(op obj.As, imm int64, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerCtxV2) emitUnary2(op obj.As, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxV2) emitCmpRI2(reg int16, imm int64) {
	p := lc.c.NewProg()
	p.As = x86.ACMPQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = reg
	p.To.Type = obj.TYPE_CONST
	p.To.Offset = imm
	lc.c.Append(p)
}

func (lc *lowerCtxV2) loadImm(imm int64, dst int16) {
	if imm == 0 {
		lc.emit2(x86.AXORQ, dst, dst)
		return
	}
	lc.emitRI2(x86.AMOVQ, imm, dst)
}

func (lc *lowerCtxV2) loadSpill(slot int16, dst int16) {
	lc.emitRM2(x86.AMOVQ, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

func (lc *lowerCtxV2) storeSpill(src int16, slot int16) {
	lc.emitMR2(x86.AMOVQ, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

func (lc *lowerCtxV2) loadFPSpill(slot int16, dst int16) {
	lc.emitRM2(x86.AMOVSD, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

func (lc *lowerCtxV2) storeFPSpill(src int16, slot int16) {
	lc.emitMR2(x86.AMOVSD, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// ── Label resolution ──

func (lc *lowerCtxV2) placeLabel2(l Label) {
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

func (lc *lowerCtxV2) bindLabel2(l Label, branch *obj.Prog) {
	if target, ok := lc.labelProg[l]; ok {
		branch.To.SetTarget(target)
	} else {
		lc.pending[l] = append(lc.pending[l], branch)
	}
}

// ── Instruction dispatch ──

func (lc *lowerCtxV2) lowerInstr(ins *IRInstr) error {
	switch ins.Op {
	case IROpInvalid:
		return fmt.Errorf("ir.LowerAMD64_V2: invalid op at index %d", lc.idx)

	// Data movement
	case IRMov:
		lc.v2Mov(ins)
	case IRConst:
		lc.v2Const(ins)
	case IRSext:
		lc.v2Sext(ins)
	case IRZext:
		lc.v2Zext(ins)

	// Integer ALU
	case IRAdd:
		lc.v2Binop(ins, x86.AADDQ)
	case IRAddImm:
		lc.v2BinopImm(ins, x86.AADDQ)
	case IRSub:
		lc.v2Binop(ins, x86.ASUBQ)
	case IRSubImm:
		lc.v2BinopImm(ins, x86.ASUBQ)
	case IRMul:
		lc.v2Binop(ins, x86.AIMULQ)
	case IRNeg:
		lc.v2Unary(ins, x86.ANEGQ)

	// DIV/MUL high
	case IRDivS:
		lc.v2Div(ins, true, false)
	case IRDivU:
		lc.v2Div(ins, false, false)
	case IRRem:
		lc.v2Div(ins, true, true)
	case IRRemU:
		lc.v2Div(ins, false, true)
	case IRMulHS:
		lc.v2MulHigh(ins, true)
	case IRMulHU:
		lc.v2MulHigh(ins, false)
	case IRMulHSU:
		lc.v2MulHSU(ins)

	// Shifts
	case IRShl:
		lc.v2Shift(ins, x86.ASHLQ)
	case IRShlImm:
		lc.v2ShiftImm(ins, x86.ASHLQ)
	case IRShr:
		lc.v2Shift(ins, x86.ASHRQ)
	case IRShrImm:
		lc.v2ShiftImm(ins, x86.ASHRQ)
	case IRSar:
		lc.v2Shift(ins, x86.ASARQ)
	case IRSarImm:
		lc.v2ShiftImm(ins, x86.ASARQ)

	// Bitwise
	case IRAnd:
		lc.v2Binop(ins, x86.AANDQ)
	case IRAndImm:
		lc.v2BinopImm(ins, x86.AANDQ)
	case IROr:
		lc.v2Binop(ins, x86.AORQ)
	case IROrImm:
		lc.v2BinopImm(ins, x86.AORQ)
	case IRXor:
		lc.v2Binop(ins, x86.AXORQ)
	case IRXorImm:
		lc.v2BinopImm(ins, x86.AXORQ)
	case IRNot:
		lc.v2Unary(ins, x86.ANOTQ)

	// Bit manipulation
	case IRClz:
		if ins.T == I32 {
			lc.v2Unary(ins, x86.ALZCNTL)
		} else {
			lc.v2Unary(ins, x86.ALZCNTQ)
		}
	case IRCtz:
		if ins.T == I32 {
			lc.v2Unary(ins, x86.ATZCNTL)
		} else {
			lc.v2Unary(ins, x86.ATZCNTQ)
		}
	case IRPopcount:
		if ins.T == I32 {
			lc.v2Unary(ins, x86.APOPCNTL)
		} else {
			lc.v2Unary(ins, x86.APOPCNTQ)
		}
	case IRBswap:
		lc.v2Unary(ins, x86.ABSWAPQ)

	// Comparison
	case IRSet:
		lc.v2Set(ins)
	case IRSetImm:
		lc.v2SetImm(ins)

	// Memory
	case IRLoad:
		lc.v2Load(ins)
	case IRStore:
		lc.v2Store(ins)
	case IRLoadX:
		lc.v2LoadX(ins)
	case IRStoreX:
		lc.v2StoreX(ins)

	// Control flow
	case IRLabel:
		lc.placeLabel2(Label(ins.Imm))
	case IRBranch:
		lc.v2Branch(ins)
	case IRBranchImm:
		lc.v2BranchImm(ins)
	case IRJump:
		lc.v2Jump(ins)
	case IRRet:
		lc.v2Ret(ins)
	case IRRetDyn:
		lc.v2RetDyn(ins)
	case IRChainExit:
		// V2 doesn't support chaining — lower as a regular ret.
		lc.v2Ret(&IRInstr{Op: IRRet, Imm: ins.Imm, Imm2: 0, A: VRegZero})
	case IRCall:
		lc.v2Call(ins)

	// FP arithmetic
	case IRFAdd:
		lc.v2FPBinop(ins, x86.AADDSD, x86.AADDSS)
	case IRFSub:
		lc.v2FPBinop(ins, x86.ASUBSD, x86.ASUBSS)
	case IRFMul:
		lc.v2FPBinop(ins, x86.AMULSD, x86.AMULSS)
	case IRFDiv:
		lc.v2FPBinop(ins, x86.ADIVSD, x86.ADIVSS)
	case IRFSqrt:
		lc.v2FPUnary(ins, x86.ASQRTSD, x86.ASQRTSS)
	case IRFNeg:
		lc.v2FNeg(ins)
	case IRFAbs:
		lc.v2FAbs(ins)
	case IRFCmp:
		lc.v2FCmp(ins)

	// FP conversions
	case IRFCvtToI:
		lc.v2FCvtToI(ins)
	case IRFCvtToU:
		lc.v2FCvtToI(ins) // signed path, works for values < 2^63
	case IRFCvtFromI:
		lc.v2FCvtFromI(ins)
	case IRFCvtFromU:
		lc.v2FCvtFromI(ins) // signed path, works for values < 2^63
	case IRFCvtFF:
		lc.v2FCvtFF(ins)

	// Pseudo-ops
	case IRMarkLive, IRMarkDead, IRWriteback:
		// no-op

	default:
		return fmt.Errorf("ir.LowerAMD64_V2: unhandled op %v at index %d", ins.Op, lc.idx)
	}
	return nil
}

// ── Data movement ──

func (lc *lowerCtxV2) v2Mov(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2Const(ins *IRInstr) {
	dst := lc.writeDst(ins.Dst)
	lc.loadImm(ins.Imm, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2Sext(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0)
	dst := lc.writeDst(ins.Dst)
	var op obj.As
	switch ins.T {
	case I8:
		op = x86.AMOVBQSX
	case I16:
		op = x86.AMOVWQSX
	case I32:
		op = x86.AMOVLQSX
	default:
		op = x86.AMOVQ
	}
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2Zext(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0)
	dst := lc.writeDst(ins.Dst)
	var op obj.As
	switch ins.T {
	case I8:
		op = x86.AMOVBQZX
	case I16:
		op = x86.AMOVWQZX
	case I32:
		op = x86.AMOVL // auto-zeros upper 32
	default:
		op = x86.AMOVQ
	}
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

// ── Integer ALU ──

func (lc *lowerCtxV2) v2Binop(ins *IRInstr, op obj.As) {
	a := lc.stageInt(ins.A, 0) // R10
	b := lc.stageInt(ins.B, 1) // R11
	lc.emit2(op, b, a)         // R10 = R10 OP R11
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2BinopImm(ins *IRInstr, op obj.As) {
	a := lc.stageInt(ins.A, 0) // R10
	imm := ins.Imm
	if imm >= -(1<<31) && imm < (1<<31) {
		lc.emitRI2(op, imm, a)
	} else {
		lc.loadImm(imm, v2StgB) // R11 = imm
		lc.emit2(op, v2StgB, a) // R10 = R10 OP R11
	}
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2Unary(ins *IRInstr, op obj.As) {
	a := lc.stageInt(ins.A, 0) // R10
	lc.emitUnary2(op, a)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── Shifts ──

func (lc *lowerCtxV2) v2Shift(ins *IRInstr, op obj.As) {
	a := lc.stageInt(ins.A, 0) // R10 = value
	b := lc.stageInt(ins.B, 1) // R11 = count
	dst := lc.writeDst(ins.Dst)

	// Move count to CX. If CX holds a live VReg (not our dst), save it.
	needCXSave := dst != goasm.REG_AMD64_CX && lc.isCXLive()
	if needCXSave {
		// XCHG atomically swaps: CX gets count, R11 gets old CX.
		lc.emit2(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
	} else {
		lc.emit2(x86.AMOVQ, b, goasm.REG_AMD64_CX)
	}

	// Shift: R10 by CL.
	lc.emit2(op, goasm.REG_AMD64_CX, a) // SHL/SHR/SAR CL, R10

	// Write result.
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}

	// Restore CX if we saved it.
	if needCXSave {
		lc.emit2(x86.AMOVQ, v2StgB, goasm.REG_AMD64_CX) // R11 held old CX
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2ShiftImm(ins *IRInstr, op obj.As) {
	a := lc.stageInt(ins.A, 0) // R10
	lc.emitRI2(op, ins.Imm, a)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── Division ──

func (lc *lowerCtxV2) v2Div(ins *IRInstr, signed, wantRem bool) {
	a := lc.stageInt(ins.A, 0) // R10 = dividend
	b := lc.stageInt(ins.B, 1) // R11 = divisor

	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_AX)

	if signed {
		p := lc.c.NewProg()
		p.As = x86.ACQO
		lc.c.Append(p)
		p = lc.c.NewProg()
		p.As = x86.AIDIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = b // R11 — not clobbered by CQO
		lc.c.Append(p)
	} else {
		lc.emit2(x86.AXORQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
		p := lc.c.NewProg()
		p.As = x86.ADIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = b // R11 — not clobbered by XOR RDX
		lc.c.Append(p)
	}

	var result int16 = goasm.REG_AMD64_AX
	if wantRem {
		result = goasm.REG_AMD64_DX
	}
	dst := lc.writeDst(ins.Dst)
	if dst != result {
		lc.emit2(x86.AMOVQ, result, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── MulHigh ──

func (lc *lowerCtxV2) v2MulHigh(ins *IRInstr, signed bool) {
	a := lc.stageInt(ins.A, 0) // R10
	b := lc.stageInt(ins.B, 1) // R11

	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_AX)

	p := lc.c.NewProg()
	if signed {
		p.As = x86.AIMULQ
	} else {
		p.As = x86.AMULQ
	}
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b // R11
	lc.c.Append(p)

	dst := lc.writeDst(ins.Dst)
	if dst != goasm.REG_AMD64_DX {
		lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2MulHSU(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // R10 = signed
	b := lc.stageInt(ins.B, 1) // R11 = unsigned

	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_AX) // RAX = a

	// Sign correction mask: R10 = (a < 0) ? b : 0
	lc.emitRI2(x86.ASARQ, 63, a) // R10 = sign bits of a
	lc.emit2(x86.AANDQ, b, a)    // R10 = (a_neg) ? b : 0

	// Unsigned multiply: RDX:RAX = RAX * R11
	p := lc.c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b // R11
	lc.c.Append(p)

	// Correct: RDX -= correction
	lc.emit2(x86.ASUBQ, a, goasm.REG_AMD64_DX)

	dst := lc.writeDst(ins.Dst)
	if dst != goasm.REG_AMD64_DX {
		lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── Comparison ──

func (lc *lowerCtxV2) v2Set(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // R10
	b := lc.stageInt(ins.B, 1) // R11
	lc.emit2(x86.ACMPQ, a, b)  // flags = a - b
	dst := lc.writeDst(ins.Dst)
	bReg := byteReg(dst)
	setOp := predToSETcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = setOp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = bReg
	lc.c.Append(p)
	lc.emit2(x86.AMOVBQZX, bReg, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2SetImm(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // R10
	if ins.Imm >= -(1<<31) && ins.Imm < (1<<31) {
		lc.emitCmpRI2(a, ins.Imm)
	} else {
		lc.loadImm(ins.Imm, v2StgB) // R11 = imm
		lc.emit2(x86.ACMPQ, a, v2StgB)
	}
	dst := lc.writeDst(ins.Dst)
	bReg := byteReg(dst)
	setOp := predToSETcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = setOp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = bReg
	lc.c.Append(p)
	lc.emit2(x86.AMOVBQZX, bReg, dst)
	lc.commitDst(ins.Dst, dst)
}

// ── Memory ──

func (lc *lowerCtxV2) v2Load(ins *IRInstr) {
	base := lc.stageInt(ins.A, 0) // R10
	dst := lc.writeDst(ins.Dst)
	if lc.isVRegFP2(ins.Dst) {
		dst = lc.writeDstFP(ins.Dst)
	}
	lc.emitRM2(loadOp(ins.T), base, ins.Imm, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2Store(ins *IRInstr) {
	base := lc.stageInt(ins.A, 0) // R10
	if lc.isVRegFP2(ins.B) {
		src := lc.stageFP(ins.B, 1)
		lc.emitMR2(storeOp(ins.T), src, base, ins.Imm)
	} else {
		src := lc.stageInt(ins.B, 1) // R11
		lc.emitMR2(storeOp(ins.T), src, base, ins.Imm)
	}
}

func (lc *lowerCtxV2) v2LoadX(ins *IRInstr) {
	base := lc.stageInt(ins.A, 0) // R10
	idx := lc.stageInt(ins.B, 1)  // R11
	dst := lc.writeDst(ins.Dst)
	if lc.isVRegFP2(ins.Dst) {
		dst = lc.writeDstFP(ins.Dst)
	}
	p := lc.c.NewProg()
	p.As = loadOp(ins.T)
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Index = idx
	p.From.Scale = int16(ins.Scale)
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2StoreX(ins *IRInstr) {
	base := lc.stageInt(ins.A, 0) // R10
	idx := lc.stageInt(ins.B, 1)  // R11

	// Collapse base+idx*scale into R11 via LEA, freeing R10 for the value.
	p := lc.c.NewProg()
	p.As = x86.ALEAQ
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Index = idx
	p.From.Scale = int16(ins.Scale)
	p.To.Type = obj.TYPE_REG
	p.To.Reg = v2StgB // R11 = effective address
	lc.c.Append(p)

	// Now load the value into R10.
	src := ins.Dst // VReg (repurposed as value)
	if lc.isVRegFP2(src) {
		srcReg := lc.stageFP(src, 0)
		lc.emitMR2(storeOp(ins.T), srcReg, v2StgB, 0)
	} else {
		srcReg := lc.stageInt(src, 0) // R10 = value
		lc.emitMR2(storeOp(ins.T), srcReg, v2StgB, 0)
	}
}

func (lc *lowerCtxV2) isVRegFP2(v VReg) bool {
	if v == VRegZero {
		return false
	}
	return lc.fpSet[v]
}

// ── Control flow ──

func (lc *lowerCtxV2) v2Branch(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // R10
	b := lc.stageInt(ins.B, 1) // R11
	lc.emit2(x86.ACMPQ, a, b)
	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel2(Label(ins.Imm), p)
}

func (lc *lowerCtxV2) v2BranchImm(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // R10
	if ins.Imm2 >= -(1<<31) && ins.Imm2 < (1<<31) {
		lc.emitCmpRI2(a, ins.Imm2)
	} else {
		lc.loadImm(ins.Imm2, v2StgB)
		lc.emit2(x86.ACMPQ, a, v2StgB)
	}
	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel2(Label(ins.Imm), p)
}

func (lc *lowerCtxV2) v2Jump(ins *IRInstr) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel2(Label(ins.Imm), p)
}

func (lc *lowerCtxV2) v2Ret(ins *IRInstr) {
	// pc
	lc.loadImm(ins.Imm, v2StgA)
	lc.emitMR2(x86.AMOVQ, v2StgA, amd64RegSret, 0)
	// ic
	lc.emitMR2(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)
	// status
	lc.emitMI2(x86.AMOVQ, ins.Imm2, amd64RegSret, 16)
	// faultAddr
	if ins.A != VRegZero {
		fa := lc.stageInt(ins.A, 0)
		lc.emitMR2(x86.AMOVQ, fa, amd64RegSret, 24)
	} else {
		lc.emitMI2(x86.AMOVQ, 0, amd64RegSret, 24)
	}
	lc.emitEpilogue()
}

func (lc *lowerCtxV2) v2RetDyn(ins *IRInstr) {
	// pc (from VReg A)
	if ins.A != VRegZero {
		pcReg := lc.stageInt(ins.A, 0)
		lc.emitMR2(x86.AMOVQ, pcReg, amd64RegSret, 0)
	} else {
		lc.emitMI2(x86.AMOVQ, 0, amd64RegSret, 0)
	}
	// ic
	lc.emitMR2(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)
	// status
	lc.emitMI2(x86.AMOVQ, ins.Imm, amd64RegSret, 16)
	// faultAddr (from VReg B)
	if ins.B != VRegZero {
		fa := lc.stageInt(ins.B, 1)
		lc.emitMR2(x86.AMOVQ, fa, amd64RegSret, 24)
	} else {
		lc.emitMI2(x86.AMOVQ, 0, amd64RegSret, 24)
	}
	lc.emitEpilogue()
}

func (lc *lowerCtxV2) v2Call(ins *IRInstr) {
	if int(ins.Imm) >= len(lc.blk.CTab) {
		return
	}
	sym := lc.blk.CTab[ins.Imm]

	// Save live caller-saved registers.
	callerSavedInt := []int16{
		goasm.REG_AMD64_AX, goasm.REG_AMD64_CX, goasm.REG_AMD64_DX,
		goasm.REG_AMD64_BP, goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8, goasm.REG_AMD64_R9,
	}
	var liveInt, liveFP []int16
	for _, r := range callerSavedInt {
		if lc.isRegLive(r) {
			liveInt = append(liveInt, r)
		}
	}
	for i := int16(0); i < 14; i++ { // XMM0..XMM13 (14/15 are staging)
		r := goasm.REG_AMD64_X0 + i
		if lc.isRegLive(r) {
			liveFP = append(liveFP, r)
		}
	}

	saveSize := int64(len(liveInt)+len(liveFP)) * 8
	if saveSize > 0 {
		lc.emitRI2(x86.ASUBQ, saveSize, goasm.REG_AMD64_SP)
	}
	for i, r := range liveInt {
		lc.emitMR2(x86.AMOVQ, r, goasm.REG_AMD64_SP, int64(i)*8)
	}
	for i, r := range liveFP {
		lc.emitMR2(x86.AMOVSD, r, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8)
	}

	lc.loadImm(int64(sym.Addr), v2StgA)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = v2StgA
	lc.c.Append(p)

	for i, r := range liveInt {
		lc.emitRM2(x86.AMOVQ, goasm.REG_AMD64_SP, int64(i)*8, r)
	}
	for i, r := range liveFP {
		lc.emitRM2(x86.AMOVSD, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8, r)
	}
	if saveSize > 0 {
		lc.emitRI2(x86.AADDQ, saveSize, goasm.REG_AMD64_SP)
	}
}

// ── FP ops ──

func (lc *lowerCtxV2) v2FPBinop(ins *IRInstr, f64op, f32op obj.As) {
	a := lc.stageFP(ins.A, 0) // XMM15
	b := lc.stageFP(ins.B, 1) // XMM14
	op := f64op
	movOp := x86.AMOVSD
	if ins.T == F32 {
		op = f32op
		movOp = x86.AMOVSS
	}
	lc.emit2(op, b, a) // XMM15 = XMM15 OP XMM14
	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(movOp, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2FPUnary(ins *IRInstr, f64op, f32op obj.As) {
	a := lc.stageFP(ins.A, 0) // XMM15
	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	dst := lc.writeDstFP(ins.Dst)
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2FNeg(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0) // XMM15
	movOp := x86.AMOVSD
	if ins.T == F32 {
		movOp = x86.AMOVSS
	}
	_ = movOp

	// GPR round-trip to flip sign bit.
	lc.emit2(x86.AMOVQ, a, v2StgA) // R10 = XMM15 bits
	var mask int64
	if ins.T == F32 {
		mask = 1 << 31
	} else {
		mask = -1 << 63
	}
	lc.loadImm(mask, v2StgB)            // R11 = sign mask
	lc.emit2(x86.AXORQ, v2StgB, v2StgA) // R10 ^= R11
	lc.emit2(x86.AMOVQ, v2StgA, a)      // XMM15 = R10

	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVSD, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2FAbs(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0) // XMM15

	// GPR round-trip to clear sign bit.
	lc.emit2(x86.AMOVQ, a, v2StgA) // R10 = XMM15 bits
	var mask int64
	if ins.T == F32 {
		mask = 0x7FFFFFFF
	} else {
		mask = 0x7FFFFFFFFFFFFFFF
	}
	lc.loadImm(mask, v2StgB)            // R11 = abs mask
	lc.emit2(x86.AANDQ, v2StgB, v2StgA) // R10 &= R11
	lc.emit2(x86.AMOVQ, v2StgA, a)      // XMM15 = R10

	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVSD, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2FCmp(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0) // XMM15
	b := lc.stageFP(ins.B, 1) // XMM14

	cmpOp := x86.AUCOMISD
	if ins.T == F32 {
		cmpOp = x86.AUCOMISS
	}
	lc.emit2(cmpOp, b, a) // flags = XMM15 vs XMM14

	dst := lc.writeDst(ins.Dst)
	bReg := byteReg(dst)

	switch ins.Pred {
	case EQ:
		// SETE + SETNP → AND. Guaranteed distinct byte regs.
		p1 := lc.c.NewProg()
		p1.As = x86.ASETEQ
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(v2StgB) // R11L — always different from dst
		if dst == v2StgB {
			scrByte = byteReg(v2StgA)
		}
		p2 := lc.c.NewProg()
		p2.As = x86.ASETPC
		p2.To.Type = obj.TYPE_REG
		p2.To.Reg = scrByte
		lc.c.Append(p2)
		lc.emit2(x86.AANDB, scrByte, bReg)

	case NE:
		p1 := lc.c.NewProg()
		p1.As = x86.ASETNE
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(v2StgB)
		if dst == v2StgB {
			scrByte = byteReg(v2StgA)
		}
		p2 := lc.c.NewProg()
		p2.As = x86.ASETPS
		p2.To.Type = obj.TYPE_REG
		p2.To.Reg = scrByte
		lc.c.Append(p2)
		lc.emit2(x86.AORB, scrByte, bReg)

	default:
		setOp := predToFPSETcc(ins.Pred)
		p := lc.c.NewProg()
		p.As = setOp
		p.To.Type = obj.TYPE_REG
		p.To.Reg = bReg
		lc.c.Append(p)
	}

	lc.emit2(x86.AMOVBQZX, bReg, dst)
	lc.commitDst(ins.Dst, dst)
}

// ── FP conversions ──

func (lc *lowerCtxV2) v2FCvtToI(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0) // XMM15
	dst := lc.writeDst(ins.Dst)
	var cvtOp obj.As
	switch {
	case ins.U == F64 && ins.T == I64:
		cvtOp = x86.ACVTTSD2SQ
	case ins.U == F64:
		cvtOp = x86.ACVTTSD2SL
	case ins.U == F32 && ins.T == I64:
		cvtOp = x86.ACVTTSS2SQ
	case ins.U == F32:
		cvtOp = x86.ACVTTSS2SL
	default:
		cvtOp = x86.ACVTTSD2SQ
	}
	lc.emit2(cvtOp, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2FCvtFromI(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // R10
	dst := lc.writeDstFP(ins.Dst)
	var cvtOp obj.As
	switch {
	case ins.U == I64 && ins.T == F64:
		cvtOp = x86.ACVTSQ2SD
	case ins.T == F64:
		cvtOp = x86.ACVTSL2SD
	case ins.U == I64 && ins.T == F32:
		cvtOp = x86.ACVTSQ2SS
	case ins.T == F32:
		cvtOp = x86.ACVTSL2SS
	default:
		cvtOp = x86.ACVTSQ2SD
	}
	lc.emit2(cvtOp, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxV2) v2FCvtFF(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0) // XMM15
	dst := lc.writeDstFP(ins.Dst)
	op := x86.ACVTSS2SD
	if ins.U == F64 {
		op = x86.ACVTSD2SS
	}
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}
