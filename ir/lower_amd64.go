package ir

import (
	"fmt"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// ── AMD64 register convention ──
//
// Pinned (never allocated, callee-saved):
//   R12 = x[] pointer    (param VReg t64)
//   R13 = f[] pointer    (param VReg t65)
//   R14 = mem_base       (param VReg t67)
//   R15 = mem_mask       (param VReg t68)
//   RBX = sret buffer
//   RSP = stack pointer
//
// Scratch (never allocated, used by lowerer):
//   R10, R11
//
// Integer allocation pool (8 regs, or 6 if DIV/MUL present):
//   RAX, RCX, RDX, RBP, RSI, RDI, R8, R9
//
// FP allocation pool:
//   XMM0-XMM15

// Pinned host registers.
const (
	amd64RegXBase   int16 = goasm.REG_AMD64_R12
	amd64RegFBase   int16 = goasm.REG_AMD64_R13
	amd64RegMemBase int16 = goasm.REG_AMD64_R14
	amd64RegMemMask int16 = goasm.REG_AMD64_R15
	amd64RegIC      int16 = goasm.REG_AMD64_BP
	amd64RegSret    int16 = goasm.REG_AMD64_BX
	amd64Scratch1   int16 = goasm.REG_AMD64_R10
	amd64Scratch2   int16 = goasm.REG_AMD64_R11
)

// Parameter VRegs by convention (NewEmitter allocates these first).
const (
	VRXBase   = VReg(VRegTempStart + 0) // t64
	VRFBase   = VReg(VRegTempStart + 1) // t65
	VRIC      = VReg(VRegTempStart + 2) // t66 — pinned to RBP
	VRMemBase = VReg(VRegTempStart + 3) // t67
	VRMemMask = VReg(VRegTempStart + 4) // t68
)

// AMD64Pool returns the register pool for amd64 lowering.
// When the block contains DIV/MUL ops, RAX and RDX are excluded
// because IDIVQ/MULQ use them as implicit operands.
func AMD64Pool(b *Block) RegPool {
	intRegs := []int16{
		goasm.REG_AMD64_AX,
		goasm.REG_AMD64_CX,
		goasm.REG_AMD64_DX,
		goasm.REG_AMD64_SI,
		goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8,
		goasm.REG_AMD64_R9,
	}
	if BlockHasDivMul(b) {
		intRegs = []int16{
			goasm.REG_AMD64_CX,
			goasm.REG_AMD64_SI,
			goasm.REG_AMD64_DI,
			goasm.REG_AMD64_R8,
			goasm.REG_AMD64_R9,
		}
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13, goasm.REG_AMD64_X14, goasm.REG_AMD64_X15,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

// AMD64Pinned returns the pinned VReg → host register map for parameter VRegs.
// These are passed to Allocate() so the allocator fixes them in place.
func AMD64Pinned() map[VReg]int16 {
	return map[VReg]int16{
		VRXBase:   amd64RegXBase,
		VRFBase:   amd64RegFBase,
		VRIC:      amd64RegIC,
		VRMemBase: amd64RegMemBase,
		VRMemMask: amd64RegMemMask,
	}
}

// ── lowerCtx holds mutable state during lowering ──

type lowerCtx struct {
	blk   *Block
	alloc *Allocation
	c     *goasm.Ctx
	idx   int // current IR instruction index

	// Label resolution.
	labelProg map[Label]*obj.Prog   // label → NOP prog at that point
	pending   map[Label][]*obj.Prog // forward-ref branches waiting for label

	// Frame layout.
	stackSlots int   // from Allocation.StackSlots
	frameSize  int64 // total bytes: stackSlots*8 + 8 (fcsr save)
}

// ── Exported API ──

// LowerAMD64 converts a register-allocated IR Block into x86-64 obj.Progs
// appended to ctx. After calling this, ctx.Assemble() produces native bytes.
//
// The caller must have already appended an ATEXT prog to ctx.
func LowerAMD64(ctx *goasm.Ctx, b *Block, alloc *Allocation) error {
	if alloc == nil {
		return fmt.Errorf("ir.LowerAMD64: nil allocation")
	}

	lc := &lowerCtx{
		blk:        b,
		alloc:      alloc,
		c:          ctx,
		labelProg:  make(map[Label]*obj.Prog),
		pending:    make(map[Label][]*obj.Prog),
		stackSlots: alloc.StackSlots,
	}

	// Compute frame size: spill slots + fcsr save slot.
	lc.frameSize = int64(lc.stackSlots)*8 + 8
	if lc.stackSlots == 0 {
		lc.frameSize = 0 // no frame needed if no spills (fcsr stored separately)
	}

	lc.emitPrologue()

	for idx := range b.Instrs {
		lc.idx = idx
		if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
			return err
		}
	}

	if len(lc.pending) > 0 {
		return fmt.Errorf("ir.LowerAMD64: %d unresolved forward labels", len(lc.pending))
	}

	return nil
}

// ── Prologue / Epilogue ──

func (lc *lowerCtx) emitPrologue() {
	// Move SysV ABI args to pinned callee-saved registers.
	// Entry ABI: RDI=sret, RSI=x[], RDX=f[], RCX=fcsr, R8=memBase, R9=memMask
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_SI, amd64RegXBase)   // RSI → R12
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DX, amd64RegFBase)   // RDX → R13
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_R8, amd64RegMemBase) // R8  → R14
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_R9, amd64RegMemMask) // R9  → R15
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DI, amd64RegSret)    // RDI → RBX

	// Allocate spill frame if needed.
	if lc.frameSize > 0 {
		lc.emitRI(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)
		// Save fcsr pointer at top of frame.
		lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_CX, goasm.REG_AMD64_SP, int64(lc.stackSlots)*8)
	}

	// Initialize IC to 0 (pinned to RBP).
	lc.emitRR(x86.AXORQ, amd64RegIC, amd64RegIC)
}

func (lc *lowerCtx) emitEpilogue() {
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)
}

// ── Instruction lowering dispatch ──

func (lc *lowerCtx) lowerInstr(ins *IRInstr) error {
	switch ins.Op {
	case IROpInvalid:
		return fmt.Errorf("ir.LowerAMD64: invalid op at index %d", lc.idx)

	// Data movement
	case IRMov:
		lc.lowerMov(ins)
	case IRConst:
		lc.lowerConst(ins)
	case IRSext:
		lc.lowerSext(ins)
	case IRZext:
		lc.lowerZext(ins)

	// Integer ALU
	case IRAdd:
		lc.lowerBinop(ins, x86.AADDQ, true)
	case IRAddImm:
		lc.lowerBinopImm(ins, x86.AADDQ, x86.AINCQ, x86.ADECQ)
	case IRSub:
		lc.lowerBinop(ins, x86.ASUBQ, false)
	case IRSubImm:
		lc.lowerBinopImm(ins, x86.ASUBQ, 0, 0)
	case IRMul:
		lc.lowerBinop(ins, x86.AIMULQ, true)
	case IRNeg:
		lc.lowerUnary(ins, x86.ANEGQ)

	// DIV/MUL high
	case IRDivS:
		lc.lowerDiv(ins, true, false)
	case IRDivU:
		lc.lowerDiv(ins, false, false)
	case IRRem:
		lc.lowerDiv(ins, true, true)
	case IRRemU:
		lc.lowerDiv(ins, false, true)
	case IRMulHS:
		lc.lowerMulHigh(ins, true)
	case IRMulHU:
		lc.lowerMulHigh(ins, false)
	case IRMulHSU:
		lc.lowerMulHSU(ins)

	// Shifts
	case IRShl:
		lc.lowerShift(ins, x86.ASHLQ)
	case IRShlImm:
		lc.lowerShiftImm(ins, x86.ASHLQ)
	case IRShr:
		lc.lowerShift(ins, x86.ASHRQ)
	case IRShrImm:
		lc.lowerShiftImm(ins, x86.ASHRQ)
	case IRSar:
		lc.lowerShift(ins, x86.ASARQ)
	case IRSarImm:
		lc.lowerShiftImm(ins, x86.ASARQ)

	// Bitwise
	case IRAnd:
		lc.lowerBinop(ins, x86.AANDQ, true)
	case IRAndImm:
		lc.lowerBinopImm(ins, x86.AANDQ, 0, 0)
	case IROr:
		lc.lowerBinop(ins, x86.AORQ, true)
	case IROrImm:
		lc.lowerBinopImm(ins, x86.AORQ, 0, 0)
	case IRXor:
		lc.lowerBinop(ins, x86.AXORQ, true)
	case IRXorImm:
		lc.lowerBinopImm(ins, x86.AXORQ, 0, 0)
	case IRNot:
		lc.lowerUnary(ins, x86.ANOTQ)

	// Comparison
	case IRSet:
		lc.lowerSet(ins)
	case IRSetImm:
		lc.lowerSetImm(ins)

	// Memory
	case IRLoad:
		lc.lowerLoad(ins)
	case IRStore:
		lc.lowerStore(ins)
	case IRLoadX:
		lc.lowerLoadX(ins)
	case IRStoreX:
		lc.lowerStoreX(ins)

	// Control flow
	case IRLabel:
		lc.placeLabel(Label(ins.Imm))
	case IRBranch:
		lc.lowerBranch(ins)
	case IRBranchImm:
		lc.lowerBranchImm(ins)
	case IRJump:
		lc.lowerJump(ins)
	case IRRet:
		lc.lowerRet(ins)
	case IRRetDyn:
		lc.lowerRetDyn(ins)
	case IRCall:
		lc.lowerCall(ins)

	// FP arithmetic
	case IRFAdd:
		lc.lowerFPBinop(ins, x86.AADDSD, x86.AADDSS)
	case IRFSub:
		lc.lowerFPBinop(ins, x86.ASUBSD, x86.ASUBSS)
	case IRFMul:
		lc.lowerFPBinop(ins, x86.AMULSD, x86.AMULSS)
	case IRFDiv:
		lc.lowerFPBinop(ins, x86.ADIVSD, x86.ADIVSS)
	case IRFSqrt:
		lc.lowerFPUnary(ins, x86.ASQRTSD, x86.ASQRTSS)
	case IRFNeg:
		lc.lowerFNeg(ins)
	case IRFAbs:
		lc.lowerFAbs(ins)
	case IRFCmp:
		lc.lowerFCmp(ins)

	// FP conversions
	case IRFCvtToI:
		lc.lowerFCvtToI(ins)
	case IRFCvtToU:
		lc.lowerFCvtToU(ins)
	case IRFCvtFromI:
		lc.lowerFCvtFromI(ins)
	case IRFCvtFromU:
		lc.lowerFCvtFromU(ins)
	case IRFCvtFF:
		lc.lowerFCvtFF(ins)

	// Pseudo-ops — no code emitted.
	case IRMarkLive, IRMarkDead, IRWriteback:
		// no-op

	default:
		return fmt.Errorf("ir.LowerAMD64: unhandled op %v at index %d", ins.Op, lc.idx)
	}
	return nil
}

// ── Register resolution ──

// hostRegFor returns the x86 register constant for VReg v at instruction idx.
// Returns -1 if the VReg is unused or on stack.
func (lc *lowerCtx) hostRegFor(v VReg, idx int) int16 {
	if v == VRegZero {
		return -1
	}
	if int(v) >= len(lc.alloc.Kind) {
		return -1
	}
	if lc.alloc.Kind[v] != AllocReg {
		return -1
	}
	for i := range lc.alloc.IntervalMap {
		ia := &lc.alloc.IntervalMap[i]
		if ia.Interval.VReg == v && ia.Interval.Start <= idx && idx <= ia.Interval.End {
			return ia.Host
		}
	}
	return -1
}

// isXMMReg returns true if the register constant is an XMM register.
func isXMMReg(r int16) bool {
	return r >= goasm.REG_AMD64_X0 && r <= goasm.REG_AMD64_X15
}

// isVRegFP returns true if the VReg is a floating-point register.
// Guest FP regs are 32-63; temps are FP if their defining instruction is FP-typed.
// We detect this by checking if the allocator assigned it an XMM register.
func (lc *lowerCtx) isVRegFP(v VReg) bool {
	if v >= 32 && v < 64 {
		return true // guest FP register
	}
	// Check if any interval for this VReg is assigned to an XMM register.
	for i := range lc.alloc.IntervalMap {
		ia := &lc.alloc.IntervalMap[i]
		if ia.Interval.VReg == v && isXMMReg(ia.Host) {
			return true
		}
	}
	return false
}

// use loads VReg v into a host register for reading. Returns the host register.
// If v is in a register, returns it directly.
// If v is on stack, emits a reload into the specified scratch register.
func (lc *lowerCtx) use(v VReg, scratchIdx int) int16 {
	if v == VRegZero {
		// Need a register containing 0. Use scratch and XOR it.
		scr := lc.scratch(scratchIdx)
		lc.emitRR(x86.AXORQ, scr, scr)
		return scr
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocReg {
		r := lc.hostRegFor(v, lc.idx)
		if r >= 0 {
			return r
		}
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if lc.isVRegFP(v) {
			scr := lc.fpScratch(scratchIdx)
			lc.fpSpillLoad(lc.alloc.SpillSlot[v], scr)
			return scr
		}
		scr := lc.scratch(scratchIdx)
		lc.spillLoad(lc.alloc.SpillSlot[v], scr)
		return scr
	}
	// VReg not found in allocation — shouldn't happen for valid IR.
	return lc.scratch(scratchIdx)
}

// def returns the host register to write VReg v into.
// If v is in a register, returns it. If on stack, returns scratch.
// Caller must call defCommit() after writing.
func (lc *lowerCtx) def(v VReg) int16 {
	if v == VRegZero {
		return lc.scratch(0) // writes discarded
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocReg {
		r := lc.hostRegFor(v, lc.idx)
		if r >= 0 {
			return r
		}
	}
	if lc.isVRegFP(v) {
		return lc.fpScratch(0)
	}
	return lc.scratch(0)
}

// defCommit writes back to the spill slot if v is stack-allocated.
func (lc *lowerCtx) defCommit(v VReg, hostReg int16) {
	if v == VRegZero {
		return
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if isXMMReg(hostReg) {
			lc.fpSpillStore(hostReg, lc.alloc.SpillSlot[v])
		} else {
			lc.spillStore(hostReg, lc.alloc.SpillSlot[v])
		}
	}
}

func (lc *lowerCtx) scratch(idx int) int16 {
	if idx == 0 {
		return amd64Scratch1
	}
	return amd64Scratch2
}

// fpScratch returns an XMM scratch register for FP spill/reload.
// Uses XMM15 (scratch0) and XMM14 (scratch1). These are at the end of the
// FP pool; the allocator should not assign them when they're needed for scratch.
// TODO: formally reserve these from the FP pool.
func (lc *lowerCtx) fpScratch(idx int) int16 {
	if idx == 0 {
		return goasm.REG_AMD64_X15
	}
	return goasm.REG_AMD64_X14
}

// fpSpillLoad loads from spill slot into an XMM register.
func (lc *lowerCtx) fpSpillLoad(slot int16, dst int16) {
	lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

// fpSpillStore stores from an XMM register into a spill slot.
func (lc *lowerCtx) fpSpillStore(src int16, slot int16) {
	lc.emitMR(x86.AMOVSD, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// ── Prog emission helpers ──

func (lc *lowerCtx) emitRR(op obj.As, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) emitRI(op obj.As, imm int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) emitRM(op obj.As, base int16, disp int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = disp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) emitMR(op obj.As, src int16, base int16, disp int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = disp
	lc.c.Append(p)
}

func (lc *lowerCtx) emitMI(op obj.As, imm int64, base int16, disp int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = disp
	lc.c.Append(p)
}

// emitUnary emits a single-operand instruction (e.g., NEGQ, NOTQ, INCQ).
func (lc *lowerCtx) emitUnary(op obj.As, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

// emitCmpRI emits CMPQ reg, $imm. CMP has reversed operand convention
// vs ADD/SUB in the Go assembler: ycmpl expects From=reg, To=const,
// while yaddl expects From=const, To=reg.
func (lc *lowerCtx) emitCmpRI(reg int16, imm int64) {
	p := lc.c.NewProg()
	p.As = x86.ACMPQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = reg
	p.To.Type = obj.TYPE_CONST
	p.To.Offset = imm
	lc.c.Append(p)
}

// byteReg maps a 64-bit register constant to its byte variant.
// In the Go assembler's x86 register numbering, byte registers (AL..R15B)
// are at offset 0-15 from RBaseAMD64, and 64-bit registers (AX..R15) are
// at offset 16-31. So byteReg = fullReg - 16.
func byteReg(r int16) int16 {
	return r - 16
}

// spillLoad loads from spill slot into dst.
func (lc *lowerCtx) spillLoad(slot int16, dst int16) {
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

// spillStore stores src into spill slot.
func (lc *lowerCtx) spillStore(src int16, slot int16) {
	lc.emitMR(x86.AMOVQ, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// loadImm64 loads a 64-bit immediate into a register.
func (lc *lowerCtx) loadImm64(imm int64, dst int16) {
	switch {
	case imm == 0:
		lc.emitRR(x86.AXORQ, dst, dst)
	case imm > 0 && uint64(imm) <= 0xFFFFFFFF:
		// MOVL (32-bit) auto-zero-extends
		lc.emitRI(x86.AMOVL, imm, dst)
	default:
		lc.emitRI(x86.AMOVQ, imm, dst)
	}
}

// ── Label resolution ──

func (lc *lowerCtx) placeLabel(l Label) {
	p := lc.c.NewProg()
	p.As = obj.ANOP
	lc.c.Append(p)
	lc.labelProg[l] = p
	// Resolve pending forward references.
	for _, bp := range lc.pending[l] {
		bp.To.SetTarget(p)
	}
	delete(lc.pending, l)
}

func (lc *lowerCtx) bindLabel(l Label, p *obj.Prog) {
	if target, ok := lc.labelProg[l]; ok {
		p.To.SetTarget(target)
	} else {
		lc.pending[l] = append(lc.pending[l], p)
	}
}

// ── Per-op lowering stubs (Phase B-G implementations) ──

func (lc *lowerCtx) lowerMov(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerConst(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	lc.loadImm64(ins.Imm, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerSext(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	var op obj.As
	switch ins.T {
	case I32:
		op = x86.AMOVLQSX // MOVSXD
	case I16:
		op = x86.AMOVWQSX
	case I8:
		op = x86.AMOVBQSX
	default:
		op = x86.AMOVQ // I64 → I64 = nop move
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerZext(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	var op obj.As
	switch ins.T {
	case I32:
		op = x86.AMOVL // auto-zeros upper 32
	case I16:
		op = x86.AMOVWQZX
	case I8:
		op = x86.AMOVBQZX
	default:
		op = x86.AMOVQ
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

// lowerBinop handles two-register binary ops (ADD, SUB, AND, OR, XOR, IMUL).
// If commutative and dst==b, swaps operands to avoid an extra MOV.
func (lc *lowerCtx) lowerBinop(ins *IRInstr, op obj.As, commutative bool) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	if dst == a {
		// dst = dst OP b
		lc.emitRR(op, b, dst)
	} else if commutative && dst == b {
		// dst = dst OP a (commutative, so a OP b == b OP a)
		lc.emitRR(op, a, dst)
	} else if dst == b {
		// Non-commutative and dst == b: MOV a,dst would destroy b.
		// Use scratch: MOV b,scratch; MOV a,dst; OP scratch,dst.
		scr := lc.scratch(0)
		if scr == a {
			scr = lc.scratch(1)
		}
		lc.emitRR(x86.AMOVQ, b, scr)
		lc.emitRR(x86.AMOVQ, a, dst)
		lc.emitRR(op, scr, dst)
	} else {
		// dst != a, dst != b: MOV a,dst; OP b,dst
		lc.emitRR(x86.AMOVQ, a, dst)
		lc.emitRR(op, b, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

// lowerBinopImm handles register-immediate binary ops.
// incOp/decOp are used for +1/-1 (pass 0 to disable).
func (lc *lowerCtx) lowerBinopImm(ins *IRInstr, op obj.As, incOp, decOp obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}

	imm := ins.Imm
	switch {
	case incOp != 0 && imm == 1:
		lc.emitUnary(incOp, dst)
	case decOp != 0 && imm == -1:
		lc.emitUnary(decOp, dst)
	case imm >= -(1<<31) && imm < (1<<31):
		lc.emitRI(op, imm, dst)
	default:
		// Large immediate: load into scratch, then op.
		scr := amd64Scratch2
		lc.loadImm64(imm, scr)
		lc.emitRR(op, scr, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerUnary(ins *IRInstr, op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}
	lc.emitUnary(op, dst)
	lc.defCommit(ins.Dst, dst)
}

// ── DIV/MUL stubs ──

func (lc *lowerCtx) lowerDiv(ins *IRInstr, signed, wantRem bool) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// Move dividend to RAX.
	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	if signed {
		// CQO: sign-extend RAX to RDX:RAX
		p := lc.c.NewProg()
		p.As = x86.ACQO
		lc.c.Append(p)
		// IDIVQ b
		p = lc.c.NewProg()
		p.As = x86.AIDIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = b
		lc.c.Append(p)
	} else {
		// XORQ RDX, RDX
		lc.emitRR(x86.AXORQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
		// DIVQ b
		p := lc.c.NewProg()
		p.As = x86.ADIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = b
		lc.c.Append(p)
	}

	// Result: quotient in RAX, remainder in RDX.
	var result int16 = goasm.REG_AMD64_AX
	if wantRem {
		result = goasm.REG_AMD64_DX
	}
	if dst != result {
		lc.emitRR(x86.AMOVQ, result, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerMulHigh(ins *IRInstr, signed bool) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	// One-operand MUL/IMUL: RDX:RAX = RAX * operand
	p := lc.c.NewProg()
	if signed {
		p.As = x86.AIMULQ
	} else {
		p.As = x86.AMULQ
	}
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	lc.c.Append(p)

	// High result is in RDX.
	if dst != goasm.REG_AMD64_DX {
		lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerMulHSU(ins *IRInstr) {
	// MULHSU: high 64 bits of (signed a) * (unsigned b).
	// Approach: unsigned mul + correction when a is negative.
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// Move a to RAX first (before computing sign mask, which clobbers scratch).
	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	// Compute sign correction: if a < 0, correction = b, else 0.
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_AX, amd64Scratch1) // R10 = a (from RAX)
	lc.emitRI(x86.ASARQ, 63, amd64Scratch1)                  // R10 = sign(a) replicated
	lc.emitRR(x86.AANDQ, b, amd64Scratch1)                   // R10 = (a<0) ? b : 0
	p := lc.c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	lc.c.Append(p)

	// Adjust: if a was negative, subtract b from high result.
	lc.emitRR(x86.ASUBQ, amd64Scratch1, goasm.REG_AMD64_DX)

	if dst != goasm.REG_AMD64_DX {
		lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

// ── Shifts ──

func (lc *lowerCtx) lowerShift(ins *IRInstr, op obj.As) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// x86 variable shifts require count in CL. Multiple aliasing hazards
	// exist (a==CX, b==dst, dst==CX, etc.). Safe strategy: use scratch
	// registers to break all conflicts.
	//
	// Algorithm:
	//   1. Get shift count into CX (saving CX first if live).
	//   2. Get shift value into dst.
	//   3. SHL/SHR/SAR CL, dst.
	//   4. Restore CX if saved.

	needCXSave := b != goasm.REG_AMD64_CX && lc.isCXLive()
	if needCXSave {
		lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
	}

	// Get count (b) into CX. Use scratch if b==dst would be clobbered
	// by the subsequent a→dst move, or if b is already in CX.
	if b == goasm.REG_AMD64_CX {
		// Already there.
	} else if b == dst && dst != a {
		// b lives in dst. Moving a→dst later would clobber it.
		// Save b to CX first.
		lc.emitRR(x86.AMOVQ, b, goasm.REG_AMD64_CX)
	} else {
		// No conflict or b!=dst. Safe to move b→CX.
		lc.emitRR(x86.AMOVQ, b, goasm.REG_AMD64_CX)
	}

	// Get value (a) into dst.
	if dst == goasm.REG_AMD64_CX {
		// dst IS CX. We just put count there. Use scratch for dst,
		// shift, then move result to CX at the end.
		scr := amd64Scratch1
		if a != scr {
			lc.emitRR(x86.AMOVQ, a, scr)
		}
		lc.emitRR(op, goasm.REG_AMD64_CX, scr)
		lc.emitRR(x86.AMOVQ, scr, dst)
	} else {
		if dst != a {
			lc.emitRR(x86.AMOVQ, a, dst)
		}
		lc.emitRR(op, goasm.REG_AMD64_CX, dst)
	}

	if needCXSave {
		lc.emitRR(x86.AMOVQ, amd64Scratch2, goasm.REG_AMD64_CX)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerShiftImm(ins *IRInstr, op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}
	lc.emitRI(op, ins.Imm, dst)
	lc.defCommit(ins.Dst, dst)
}

// isCXLive returns true if RCX currently holds a live allocated VReg.
func (lc *lowerCtx) isCXLive() bool {
	for i := range lc.alloc.IntervalMap {
		ia := &lc.alloc.IntervalMap[i]
		if ia.Host == goasm.REG_AMD64_CX &&
			ia.Interval.Start <= lc.idx && lc.idx <= ia.Interval.End {
			return true
		}
	}
	return false
}

// ── Comparison ──

func (lc *lowerCtx) lowerSet(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	// CMPQ a, b — Go asm CMPQ computes From - To, so From=a, To=b gives a - b.
	lc.emitRR(x86.ACMPQ, a, b)

	// SETcc into byte register, then zero-extend.
	setOp := predToSETcc(ins.Pred)
	bReg := byteReg(dst)
	p := lc.c.NewProg()
	p.As = setOp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = bReg
	lc.c.Append(p)

	lc.emitRR(x86.AMOVBQZX, bReg, dst)

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerSetImm(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)

	// CMP a, $imm — Go asm CMP expects From=reg, To=const for ycmpl.
	lc.emitCmpRI(a, ins.Imm)

	setOp := predToSETcc(ins.Pred)
	bReg := byteReg(dst)
	p := lc.c.NewProg()
	p.As = setOp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = bReg
	lc.c.Append(p)

	lc.emitRR(x86.AMOVBQZX, bReg, dst)
	lc.defCommit(ins.Dst, dst)
}

func predToSETcc(p Pred) obj.As {
	switch p {
	case EQ:
		return x86.ASETEQ
	case NE:
		return x86.ASETNE
	case LT:
		return x86.ASETLT
	case LE:
		return x86.ASETLE
	case GT:
		return x86.ASETGT
	case GE:
		return x86.ASETGE
	case LTU:
		return x86.ASETCS
	case LEU:
		return x86.ASETLS
	case GTU:
		return x86.ASETHI
	case GEU:
		return x86.ASETCC
	default:
		return x86.ASETEQ
	}
}

// ── Memory ──

func (lc *lowerCtx) lowerLoad(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	base := lc.use(ins.A, 1)
	op := loadOp(ins.T)
	lc.emitRM(op, base, ins.Imm, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerStore(ins *IRInstr) {
	base := lc.use(ins.A, 0)
	src := lc.use(ins.B, 1)
	op := storeOp(ins.T)
	lc.emitMR(op, src, base, ins.Imm)
}

func (lc *lowerCtx) lowerLoadX(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	base := lc.use(ins.A, 0)
	idx := lc.use(ins.B, 1)
	op := loadOp(ins.T)

	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Index = idx
	p.From.Scale = int16(ins.Scale)
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerStoreX(ins *IRInstr) {
	// In IRStoreX, Dst field is repurposed as the value to store.
	// 3 operands (base, idx, src) but only 2 scratch regs.
	// Strategy: load base(scratch0) and idx(scratch1), then collapse
	// base+idx*scale via LEA into scratch1 before loading src.
	base := lc.use(ins.A, 0)
	idx := lc.use(ins.B, 1)

	// Check if src needs a scratch register (i.e., is spilled).
	src := ins.Dst // the VReg, not yet resolved
	srcInReg := src != VRegZero &&
		int(src) < len(lc.alloc.Kind) && lc.alloc.Kind[src] == AllocReg
	srcReg := lc.hostRegFor(src, lc.idx)

	if !srcInReg || srcReg < 0 {
		// src is spilled (or VRegZero) — collapse base+idx into R11 via LEA,
		// then load src into R10, then store R10 → 0(R11).
		p := lc.c.NewProg()
		p.As = x86.ALEAQ
		p.From.Type = obj.TYPE_MEM
		p.From.Reg = base
		p.From.Index = idx
		p.From.Scale = int16(ins.Scale)
		p.To.Type = obj.TYPE_REG
		p.To.Reg = amd64Scratch2
		lc.c.Append(p)

		if src == VRegZero {
			lc.emitRR(x86.AXORQ, amd64Scratch1, amd64Scratch1)
		} else {
			lc.spillLoad(lc.alloc.SpillSlot[src], amd64Scratch1)
		}
		lc.emitMR(storeOp(ins.T), amd64Scratch1, amd64Scratch2, 0)
		return
	}

	// src is in a register — no scratch conflict.
	op := storeOp(ins.T)
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = srcReg
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Index = idx
	p.To.Scale = int16(ins.Scale)
	lc.c.Append(p)
}

func loadOp(t Type) obj.As {
	switch t {
	case I8:
		return x86.AMOVBQZX
	case I16:
		return x86.AMOVWQZX
	case I32:
		return x86.AMOVL
	case I64:
		return x86.AMOVQ
	case F32:
		return x86.AMOVSS
	case F64:
		return x86.AMOVSD
	default:
		return x86.AMOVQ
	}
}

func storeOp(t Type) obj.As {
	switch t {
	case I8:
		return x86.AMOVB
	case I16:
		return x86.AMOVW
	case I32:
		return x86.AMOVL
	case I64:
		return x86.AMOVQ
	case F32:
		return x86.AMOVSS
	case F64:
		return x86.AMOVSD
	default:
		return x86.AMOVQ
	}
}

// ── Control flow ──

func (lc *lowerCtx) lowerBranch(ins *IRInstr) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	// CMPQ a, b — Go asm CMPQ computes From - To, so From=a, To=b gives a - b.
	lc.emitRR(x86.ACMPQ, a, b)

	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtx) lowerBranchImm(ins *IRInstr) {
	a := lc.use(ins.A, 0)

	// CMP a, $imm2 — Go asm CMP: From=reg, To=const.
	if ins.Imm2 >= -(1<<31) && ins.Imm2 < (1<<31) {
		lc.emitCmpRI(a, ins.Imm2)
	} else {
		lc.loadImm64(ins.Imm2, amd64Scratch2)
		lc.emitRR(x86.ACMPQ, a, amd64Scratch2)
	}

	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtx) lowerJump(ins *IRInstr) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func predToJcc(p Pred) obj.As {
	switch p {
	case EQ:
		return x86.AJEQ
	case NE:
		return x86.AJNE
	case LT:
		return x86.AJLT
	case LE:
		return x86.AJLE
	case GT:
		return x86.AJGT
	case GE:
		return x86.AJGE
	case LTU:
		return x86.AJCS
	case LEU:
		return x86.AJLS
	case GTU:
		return x86.AJHI
	case GEU:
		return x86.AJCC
	default:
		return x86.AJEQ
	}
}

func (lc *lowerCtx) lowerRet(ins *IRInstr) {
	// Write JITResult to sret buffer (RBX).
	// Offset 0: pc (from Imm)
	lc.loadImm64(ins.Imm, amd64Scratch1)
	lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0)

	// Offset 8: ic (pinned to RBP)
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)

	// Offset 16: status (from Imm2)
	lc.emitMI(x86.AMOVQ, ins.Imm2, amd64RegSret, 16)

	// Offset 24: faultAddr (from VReg A)
	if ins.A != VRegZero {
		fa := lc.use(ins.A, 0)
		lc.emitMR(x86.AMOVQ, fa, amd64RegSret, 24)
	} else {
		lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)
	}

	lc.emitEpilogue()
}

// lowerRetDyn handles IRRetDyn: return with runtime-computed PC from VReg A.
// Layout: {pc=A, status=Imm, faultAddr=B}.
func (lc *lowerCtx) lowerRetDyn(ins *IRInstr) {
	// Offset 0: pc (from VReg A)
	if ins.A != VRegZero {
		pcReg := lc.use(ins.A, 0)
		lc.emitMR(x86.AMOVQ, pcReg, amd64RegSret, 0)
	} else {
		lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 0)
	}

	// Offset 8: ic (pinned to RBP)
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)

	// Offset 16: status (from Imm)
	lc.emitMI(x86.AMOVQ, ins.Imm, amd64RegSret, 16)

	// Offset 24: faultAddr (from VReg B)
	if ins.B != VRegZero {
		fa := lc.use(ins.B, 1)
		lc.emitMR(x86.AMOVQ, fa, amd64RegSret, 24)
	} else {
		lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)
	}

	lc.emitEpilogue()
}

func (lc *lowerCtx) lowerCall(ins *IRInstr) {
	// Look up the symbol address.
	if int(ins.Imm) >= len(lc.blk.CTab) {
		return
	}
	sym := lc.blk.CTab[ins.Imm]

	// Save all live caller-saved registers. SysV ABI clobbers:
	// RAX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, XMM0-XMM15.
	// Pinned callee-saved regs (R12-R15, RBX) survive the call naturally.
	liveInt, liveFP := lc.liveCallerSaved()

	// Push live int regs to stack (using SUB RSP + MOV sequence).
	saveSize := int64(len(liveInt)+len(liveFP)) * 8
	if saveSize > 0 {
		lc.emitRI(x86.ASUBQ, saveSize, goasm.REG_AMD64_SP)
	}
	for i, r := range liveInt {
		lc.emitMR(x86.AMOVQ, r, goasm.REG_AMD64_SP, int64(i)*8)
	}
	for i, r := range liveFP {
		lc.emitMR(x86.AMOVSD, r, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8)
	}

	// Load symbol address into scratch and CALL.
	lc.loadImm64(int64(sym.Addr), amd64Scratch1)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = amd64Scratch1
	lc.c.Append(p)

	// Restore live regs.
	for i, r := range liveInt {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(i)*8, r)
	}
	for i, r := range liveFP {
		lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8, r)
	}
	if saveSize > 0 {
		lc.emitRI(x86.AADDQ, saveSize, goasm.REG_AMD64_SP)
	}
}

// liveCallerSaved returns the caller-saved registers that hold live VRegs
// at the current instruction index. These must be saved/restored around CALLs.
func (lc *lowerCtx) liveCallerSaved() (intRegs, fpRegs []int16) {
	// Caller-saved integer registers (all pool regs are caller-saved).
	callerSavedInt := []int16{
		goasm.REG_AMD64_AX, goasm.REG_AMD64_CX, goasm.REG_AMD64_DX,
		goasm.REG_AMD64_BP, goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8, goasm.REG_AMD64_R9,
	}
	for _, r := range callerSavedInt {
		if lc.isRegLive(r) {
			intRegs = append(intRegs, r)
		}
	}
	// All XMM registers are caller-saved.
	for i := int16(0); i < 16; i++ {
		r := goasm.REG_AMD64_X0 + i
		if lc.isRegLive(r) {
			fpRegs = append(fpRegs, r)
		}
	}
	return
}

// isRegLive returns true if a host register holds a live allocated VReg
// at the current instruction index.
func (lc *lowerCtx) isRegLive(hostReg int16) bool {
	for i := range lc.alloc.IntervalMap {
		ia := &lc.alloc.IntervalMap[i]
		if ia.Host == hostReg &&
			ia.Interval.Start <= lc.idx && lc.idx <= ia.Interval.End {
			return true
		}
	}
	return false
}

// ── FP ops ──

func (lc *lowerCtx) lowerFPBinop(ins *IRInstr, f64op, f32op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	op := f64op
	movOp := x86.AMOVSD
	if ins.T == F32 {
		op = f32op
		movOp = x86.AMOVSS
	}

	if dst != a {
		lc.emitRR(movOp, a, dst)
	}
	lc.emitRR(op, b, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFPUnary(ins *IRInstr, f64op, f32op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFNeg(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	movOp := x86.AMOVSD
	if ins.T == F32 {
		movOp = x86.AMOVSS
	}
	if dst != a {
		lc.emitRR(movOp, a, dst)
	}

	// Negate by flipping the sign bit via GPR round-trip.
	// MOVQ xmm→gpr, XOR with sign mask, MOVQ gpr→xmm.
	var mask int64
	if ins.T == F32 {
		mask = 1 << 31
	} else {
		mask = -1 << 63 // 0x8000000000000000
	}
	lc.emitRR(x86.AMOVQ, dst, amd64Scratch1)                // XMM → GPR (R10)
	lc.loadImm64(mask, amd64Scratch2)                        // R11 = sign mask
	lc.emitRR(x86.AXORQ, amd64Scratch2, amd64Scratch1)      // R10 ^= R11
	lc.emitRR(x86.AMOVQ, amd64Scratch1, dst)                // GPR → XMM

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFAbs(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	movOp := x86.AMOVSD
	if ins.T == F32 {
		movOp = x86.AMOVSS
	}
	if dst != a {
		lc.emitRR(movOp, a, dst)
	}

	// Clear sign bit.
	var mask int64
	if ins.T == F32 {
		mask = 0x7FFFFFFF
	} else {
		mask = 0x7FFFFFFFFFFFFFFF
	}
	lc.emitRR(x86.AMOVQ, dst, amd64Scratch1)
	lc.loadImm64(mask, amd64Scratch2)
	lc.emitRR(x86.AANDQ, amd64Scratch2, amd64Scratch1)
	lc.emitRR(x86.AMOVQ, amd64Scratch1, dst)

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFCmp(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	// UCOMISD/UCOMISS sets CF/ZF/PF flags (unsigned-style), not SF/OF.
	// Go asm UCOMISD From, To computes To vs From (To - From for flags).
	// We want a vs b, so From=b, To=a → flags reflect a vs b.
	cmpOp := x86.AUCOMISD
	if ins.T == F32 {
		cmpOp = x86.AUCOMISS
	}
	lc.emitRR(cmpOp, b, a)

	bReg := byteReg(dst)

	// UCOMISD sets CF/ZF/PF. Must use unsigned condition codes.
	// EQ/NE need special NaN handling (PF=1 when unordered).
	switch ins.Pred {
	case EQ:
		// a == b: ZF=1 AND PF=0 (exclude NaN).
		// SETE + SETNP → AND result.
		p1 := lc.c.NewProg()
		p1.As = x86.ASETEQ
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		// Use scratch byte reg for SETNP.
		scrByte := byteReg(amd64Scratch1)
		p2 := lc.c.NewProg()
		p2.As = x86.ASETPC // SETNP = SETPC (parity clear)
		p2.To.Type = obj.TYPE_REG
		p2.To.Reg = scrByte
		lc.c.Append(p2)
		lc.emitRR(x86.AANDB, scrByte, bReg)
	case NE:
		// a != b: ZF=0 OR PF=1 (NaN → not equal).
		p1 := lc.c.NewProg()
		p1.As = x86.ASETNE
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(amd64Scratch1)
		p2 := lc.c.NewProg()
		p2.As = x86.ASETPS // SETP = SETPS (parity set)
		p2.To.Type = obj.TYPE_REG
		p2.To.Reg = scrByte
		lc.c.Append(p2)
		lc.emitRR(x86.AORB, scrByte, bReg)
	default:
		// LT/LE/GT/GE use unsigned cc after UCOMISD.
		setOp := predToFPSETcc(ins.Pred)
		p := lc.c.NewProg()
		p.As = setOp
		p.To.Type = obj.TYPE_REG
		p.To.Reg = bReg
		lc.c.Append(p)
	}

	lc.emitRR(x86.AMOVBQZX, bReg, dst)
	lc.defCommit(ins.Dst, dst)
}

// predToFPSETcc maps comparison predicates to SETcc after UCOMISD/UCOMISS.
// UCOMISD sets CF/ZF/PF (like unsigned integer compare), not SF/OF.
func predToFPSETcc(p Pred) obj.As {
	switch p {
	case LT:
		return x86.ASETCS // CF=1 → a < b (below)
	case LE:
		return x86.ASETLS // CF=1 || ZF=1 → a <= b (below or equal)
	case GT:
		return x86.ASETHI // CF=0 && ZF=0 → a > b (above)
	case GE:
		return x86.ASETCC // CF=0 → a >= b (above or equal)
	default:
		return x86.ASETEQ // fallback
	}
}

// ── FP conversions ──

func (lc *lowerCtx) lowerFCvtToI(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	var cvtOp obj.As
	switch {
	case ins.U == F64 && ins.T == I64:
		cvtOp = x86.ACVTTSD2SQ
	case ins.U == F64 && (ins.T == I32 || ins.T == I16 || ins.T == I8):
		cvtOp = x86.ACVTTSD2SL
	case ins.U == F32 && ins.T == I64:
		cvtOp = x86.ACVTTSS2SQ
	case ins.U == F32 && (ins.T == I32 || ins.T == I16 || ins.T == I8):
		cvtOp = x86.ACVTTSS2SL
	default:
		cvtOp = x86.ACVTTSD2SQ
	}
	lc.emitRR(cvtOp, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFCvtToU(ins *IRInstr) {
	// x86 lacks unsigned FP→int conversion.
	// For now, use signed conversion (works for values < 2^63).
	// Full unsigned support requires range-check fixup.
	lc.lowerFCvtToI(ins)
}

func (lc *lowerCtx) lowerFCvtFromI(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	var cvtOp obj.As
	switch {
	case ins.U == I64 && ins.T == F64:
		cvtOp = x86.ACVTSQ2SD
	case (ins.U == I32 || ins.U == I16 || ins.U == I8) && ins.T == F64:
		cvtOp = x86.ACVTSL2SD
	case ins.U == I64 && ins.T == F32:
		cvtOp = x86.ACVTSQ2SS
	case (ins.U == I32 || ins.U == I16 || ins.U == I8) && ins.T == F32:
		cvtOp = x86.ACVTSL2SS
	default:
		cvtOp = x86.ACVTSQ2SD
	}
	lc.emitRR(cvtOp, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFCvtFromU(ins *IRInstr) {
	// x86 lacks unsigned int→FP conversion.
	// For now, use signed conversion (works for values < 2^63).
	lc.lowerFCvtFromI(ins)
}

func (lc *lowerCtx) lowerFCvtFF(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	var op obj.As
	if ins.U == F32 && ins.T == F64 {
		op = x86.ACVTSS2SD
	} else {
		op = x86.ACVTSD2SS
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}
