package ir

import (
	"fmt"
	"sort"

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

// AMD64PoolNormal returns the AMD64 register pool for blocks without div/mul.
func AMD64PoolNormal() RegPool {
	return RegPool{
		IntRegs: []int16{
			goasm.REG_AMD64_AX, goasm.REG_AMD64_CX, goasm.REG_AMD64_DX,
			goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
			goasm.REG_AMD64_R8, goasm.REG_AMD64_R9,
		},
		FPRegs: amd64FPRegs(),
	}
}

// AMD64PoolDivMul returns the AMD64 register pool for blocks with div/mul
// (AX and DX reserved).
func AMD64PoolDivMul(_ *Block) RegPool {
	return RegPool{
		IntRegs: []int16{
			goasm.REG_AMD64_CX,
			goasm.REG_AMD64_SI,
			goasm.REG_AMD64_DI,
			goasm.REG_AMD64_R8,
			goasm.REG_AMD64_R9,
		},
		FPRegs: amd64FPRegs(),
	}
}

func amd64FPRegs() []int16 {
	return []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13, goasm.REG_AMD64_X14, goasm.REG_AMD64_X15,
	}
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

// ── Per-VReg interval lookup (replaces linear IntervalMap scan) ──

// regEntry is one interval's host register assignment, sorted by Start.
type regEntry struct {
	start, end int
	host       int16
}

// regIndex maps VReg → sorted list of regEntry for O(log N) host-register lookup.
type regIndex [][]regEntry

type regEntriesByStart []regEntry

func (s regEntriesByStart) Len() int           { return len(s) }
func (s regEntriesByStart) Less(i, j int) bool { return s[i].start < s[j].start }
func (s regEntriesByStart) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func buildRegIndex(alloc *Allocation) regIndex {
	maxVR := len(alloc.Kind)
	idx := make(regIndex, maxVR)

	// Count entries per VReg to pre-allocate a single flat backing array.
	counts := make([]int, maxVR)
	for i := range alloc.IntervalMap {
		vr := int(alloc.IntervalMap[i].Interval.VReg)
		if vr < maxVR {
			counts[vr]++
		}
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	flat := make([]regEntry, total)

	// Assign sub-slices from the flat array.
	off := 0
	for vr, c := range counts {
		if c > 0 {
			idx[vr] = flat[off : off : off+c] // len=0, cap=c
			off += c
		}
	}

	// Fill entries.
	for i := range alloc.IntervalMap {
		ia := &alloc.IntervalMap[i]
		vr := int(ia.Interval.VReg)
		if vr < maxVR {
			idx[vr] = append(idx[vr], regEntry{
				start: ia.Interval.Start,
				end:   ia.Interval.End,
				host:  ia.Host,
			})
		}
	}

	// Sort each VReg's entries by start for binary search.
	for vr := range idx {
		entries := idx[vr]
		if len(entries) > 1 {
			sort.Sort(regEntriesByStart(entries))
		}
	}
	return idx
}

// lookup returns the host register for VReg v at instruction index idx, or -1.
func (ri regIndex) lookup(v VReg, idx int) int16 {
	vr := int(v)
	if vr >= len(ri) {
		return -1
	}
	entries := ri[vr]
	// Binary search for the interval containing idx.
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if entries[mid].end < idx {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(entries) && entries[lo].start <= idx && idx <= entries[lo].end {
		return entries[lo].host
	}
	return -1
}

// ── lowerCtx holds mutable state during lowering ──

// chainExitInfo records a chain exit for post-assembly backpatching.
type chainExitInfo struct {
	targetPC uint64    // guest PC this exit targets
	movProg  *obj.Prog // MOVABS Prog — read Pc after assembly for patch offset
	stubProg *obj.Prog // first Prog of the slow exit stub
}

// ChainExitDesc describes a chain exit for the caller.
// After assembly, use MovProg.Pc and StubProg.Pc to compute byte offsets.
type ChainExitDesc struct {
	TargetPC uint64    // guest PC this exit targets
	MovProg  *obj.Prog // the MOVABS Prog — Pc gives instruction offset after assembly
	StubProg *obj.Prog // first Prog of the slow exit stub
}

// LowerResult holds chain-related metadata produced during lowering.
// After assembly, Prog.Pc fields contain byte offsets into the assembled code.
type LowerResult struct {
	ChainEntryProg *obj.Prog      // NOP at chain entry point
	ChainExits     []ChainExitDesc // chain exit descriptors
}

type lowerCtx struct {
	blk   *Block
	alloc *Allocation      // ELS path (nil when using fixed path)
	fixed *FixedAllocation // Fixed path (nil when using ELS path)
	c     *goasm.Ctx
	idx   int // current IR instruction index

	// Fast per-VReg host register lookup (ELS path only).
	rIdx     regIndex
	fpSet    map[VReg]bool // precomputed: is this VReg assigned to an XMM register?
	cxLive   []regEntry    // intervals where CX is live (sorted by start)

	// Label resolution.
	labelProg map[Label]*obj.Prog   // label → NOP prog at that point
	pending   map[Label][]*obj.Prog // forward-ref branches waiting for label

	// Frame layout.
	stackSlots int   // from Allocation.StackSlots
	frameSize  int64 // total bytes: stackSlots*8 + 8 (fcsr save)

	// Scratch cache: elides redundant spill loads when consecutive instructions
	// use the same spilled VReg. Index 0 tracks R10, index 1 tracks R11.
	scratchCache [2]scratchEntry

	// Block chaining.
	chainEntryProg *obj.Prog       // NOP marking chain entry point
	chainExits     []chainExitInfo // chain exit metadata for backpatching
}

// ── Exported API ──

// LowerAMD64 converts a register-allocated IR Block into x86-64 obj.Progs
// appended to ctx. After calling this, ctx.Assemble() produces native bytes.
// Returns a LowerResult with chain entry/exit metadata for block chaining.
//
// The caller must have already appended an ATEXT prog to ctx.
func LowerAMD64(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	if alloc == nil {
		return nil, fmt.Errorf("ir.LowerAMD64: nil allocation")
	}

	// Build fast lookup index for host register assignments.
	rIdx := buildRegIndex(alloc)

	// Precompute FP VReg set and CX live intervals.
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
	// Guest FP regs 32-63 are always FP.
	for vr := VReg(32); vr < 64; vr++ {
		fpSet[vr] = true
	}
	sort.Sort(regEntriesByStart(cxLive))

	lc := &lowerCtx{
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

	// Compute frame size: spill slots + fcsr save slot.
	lc.frameSize = int64(lc.stackSlots)*8 + 8
	if lc.stackSlots == 0 {
		lc.frameSize = 0 // no frame needed if no spills (fcsr stored separately)
	}

	lc.emitPrologue()

	for idx := range b.Instrs {
		lc.idx = idx
		if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
			return nil, err
		}
	}

	if len(lc.pending) > 0 {
		return nil, fmt.Errorf("ir.LowerAMD64: %d unresolved forward labels", len(lc.pending))
	}

	// Emit slow exit stubs for chain exits and build result.
	result := &LowerResult{
		ChainEntryProg: lc.chainEntryProg,
	}
	for i := range lc.chainExits {
		lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
		result.ChainExits = append(result.ChainExits, ChainExitDesc{
			TargetPC: lc.chainExits[i].targetPC,
			MovProg:  lc.chainExits[i].movProg,
			StubProg: lc.chainExits[i].stubProg,
		})
	}

	return result, nil
}

// LowerAMD64Fixed converts an IR Block with a FixedAllocation into x86-64
// obj.Progs. Zero-allocation fast path: no regIndex, no fpSet map, no cxLive.
// All lookups are O(1) array accesses into the FixedAllocation.
func LowerAMD64Fixed(ctx *goasm.Ctx, b *Block, fa *FixedAllocation) (*LowerResult, error) {
	if fa == nil {
		return nil, fmt.Errorf("ir.LowerAMD64Fixed: nil allocation")
	}

	lc := &lowerCtx{
		blk:        b,
		fixed:      fa,
		c:          ctx,
		labelProg:  make(map[Label]*obj.Prog),
		pending:    make(map[Label][]*obj.Prog),
		stackSlots: fa.StackSlots,
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
		return nil, fmt.Errorf("ir.LowerAMD64Fixed: %d unresolved forward labels", len(lc.pending))
	}

	result := &LowerResult{
		ChainEntryProg: lc.chainEntryProg,
	}
	for i := range lc.chainExits {
		lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
		result.ChainExits = append(result.ChainExits, ChainExitDesc{
			TargetPC: lc.chainExits[i].targetPC,
			MovProg:  lc.chainExits[i].movProg,
			StubProg: lc.chainExits[i].stubProg,
		})
	}

	return result, nil
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

// ── Block chaining ──

func (lc *lowerCtx) emitNOP() *obj.Prog {
	p := lc.c.NewProg()
	p.As = obj.ANOP
	lc.c.Append(p)
	return p
}

func (lc *lowerCtx) emitJmpReg(reg int16) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_REG
	p.To.Reg = reg
	lc.c.Append(p)
}

// lowerChainExit emits a chain exit sequence: dealloc spill frame,
// MOVABS R10 with a sentinel (to be backpatched), JMP R10.
func (lc *lowerCtx) lowerChainExit(ins *IRInstr) {
	// Deallocate spill frame.
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	// MOVABS R10, <sentinel> — 10-byte encoding for patching.
	// Sentinel > 32 bits forces the assembler to use the 10-byte MOVABS encoding.
	const sentinel = int64(0x7BADC0DE7BADC0DE)
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = sentinel
	p.To.Type = obj.TYPE_REG
	p.To.Reg = amd64Scratch1
	lc.c.Append(p)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  p,
	})
	// JMP R10
	lc.emitJmpReg(amd64Scratch1)
}

// emitSlowExitStub emits a slow exit stub that writes Result to sret and RETs.
// Called when a chain exit hasn't been patched yet.
func (lc *lowerCtx) emitSlowExitStub(targetPC uint64) *obj.Prog {
	// Record the first prog of the stub for offset computation.
	firstProg := lc.c.NewProg()
	firstProg.As = x86.AMOVQ
	firstProg.From.Type = obj.TYPE_CONST
	firstProg.From.Offset = int64(targetPC)
	firstProg.To.Type = obj.TYPE_REG
	firstProg.To.Reg = amd64Scratch1
	lc.c.Append(firstProg)

	lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0) // Result.PC
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)    // Result.IC
	lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 16)            // Result.Status = jitOK
	lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)            // Result.FaultAddr = 0
	// No spill frame to deallocate — already done before MOVABS in chain exit.
	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)
	return firstProg
}

// ── Instruction lowering dispatch ──

func (lc *lowerCtx) lowerInstr(ins *IRInstr) error {
	// Invalidate scratch cache for ops that clobber scratch registers (R10/R11)
	// outside the standard use/def/defCommit path, or at control flow boundaries.
	switch ins.Op {
	case IRDivS, IRDivU, IRRem, IRRemU, IRMulHS, IRMulHU, IRMulHSU, // R10/R11 for operand shuffling
		IRShl, IRShr, IRSar, // may save CX to R11
		IRLoadX, IRStoreX, // R10/R11 for address computation
		IRLabel, IRBranch, IRBranchImm, IRJump, // control flow
		IRRet, IRRetDyn, IRChainExit, IRCall, // exits/calls
		IRFAdd, IRFSub, IRFMul, IRFDiv, IRFSqrt, IRFNeg, IRFAbs, IRFCmp, // FP uses FP scratch
		IRFCvtToI, IRFCvtToU, IRFCvtFromI, IRFCvtFromU, IRFCvtFF: // FP conversions
		lc.invalidateScratchCache()
	}
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

	// Bit manipulation
	case IRClz:
		if ins.T == I32 {
			lc.lowerUnary(ins, x86.ALZCNTL)
		} else {
			lc.lowerUnary(ins, x86.ALZCNTQ)
		}
	case IRCtz:
		if ins.T == I32 {
			lc.lowerUnary(ins, x86.ATZCNTL)
		} else {
			lc.lowerUnary(ins, x86.ATZCNTQ)
		}
	case IRPopcount:
		if ins.T == I32 {
			lc.lowerUnary(ins, x86.APOPCNTL)
		} else {
			lc.lowerUnary(ins, x86.APOPCNTQ)
		}
	case IRBswap:
		lc.lowerUnary(ins, x86.ABSWAPQ)

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
	case IRChainExit:
		lc.lowerChainExit(ins)
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

// allocKind returns the allocation kind for VReg v.
func (lc *lowerCtx) allocKind(v VReg) AllocKind {
	if lc.fixed != nil {
		return lc.fixed.AllocKind(v)
	}
	if int(v) < len(lc.alloc.Kind) {
		return lc.alloc.Kind[v]
	}
	return AllocUnused
}

// spillSlot returns the spill slot for VReg v.
func (lc *lowerCtx) spillSlot(v VReg) int16 {
	if lc.fixed != nil {
		return lc.fixed.SpillSlotOf(v)
	}
	if int(v) < len(lc.alloc.SpillSlot) {
		return lc.alloc.SpillSlot[v]
	}
	return -1
}

// hostRegFor returns the x86 register constant for VReg v at instruction idx.
// Returns -1 if the VReg is unused or on stack.
func (lc *lowerCtx) hostRegFor(v VReg, idx int) int16 {
	if v == VRegZero {
		return -1
	}
	if lc.fixed != nil {
		return lc.fixed.HostReg(v)
	}
	if int(v) >= len(lc.alloc.Kind) {
		return -1
	}
	if lc.alloc.Kind[v] != AllocReg {
		return -1
	}
	return lc.rIdx.lookup(v, idx)
}

// isXMMReg returns true if the register constant is an XMM register.
func isXMMReg(r int16) bool {
	return r >= goasm.REG_AMD64_X0 && r <= goasm.REG_AMD64_X15
}

// isVRegFP returns true if the VReg is a floating-point register.
func (lc *lowerCtx) isVRegFP(v VReg) bool {
	if lc.fixed != nil {
		return lc.fixed.IsFP(v)
	}
	return lc.fpSet[v]
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
	kind := lc.allocKind(v)
	if kind == AllocReg {
		r := lc.hostRegFor(v, lc.idx)
		if r >= 0 {
			return r
		}
	}
	if kind == AllocStack {
		slot := lc.spillSlot(v)
		if lc.isVRegFP(v) {
			scr := lc.fpScratch(scratchIdx)
			lc.fpSpillLoad(slot, scr)
			return scr
		}
		scr := lc.scratch(scratchIdx)
		if lc.scratchCache[scratchIdx].valid && lc.scratchCache[scratchIdx].vr == v {
			return scr // peephole: skip redundant spill load
		}
		lc.spillLoad(slot, scr)
		lc.scratchCache[scratchIdx] = scratchEntry{v, true}
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
	if lc.allocKind(v) == AllocReg {
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
	if lc.allocKind(v) == AllocStack {
		slot := lc.spillSlot(v)
		if isXMMReg(hostReg) {
			lc.fpSpillStore(hostReg, slot)
		} else {
			lc.spillStore(hostReg, slot)
			// Update scratch cache: this scratch now holds v's new value.
			if hostReg == amd64Scratch1 {
				lc.scratchCache[0] = scratchEntry{v, true}
				if lc.scratchCache[1].valid && lc.scratchCache[1].vr == v {
					lc.scratchCache[1].valid = false
				}
			} else if hostReg == amd64Scratch2 {
				lc.scratchCache[1] = scratchEntry{v, true}
				if lc.scratchCache[0].valid && lc.scratchCache[0].vr == v {
					lc.scratchCache[0].valid = false
				}
			}
		}
	}
}

// scratchEntry tracks which spilled VReg's value is resident in a scratch register.
type scratchEntry struct {
	vr    VReg
	valid bool
}

func (lc *lowerCtx) invalidateScratchCache() {
	lc.scratchCache[0].valid = false
	lc.scratchCache[1].valid = false
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
		// If A was spilled, use(A,1) returned R11; dst may be R11 too.
		scr := amd64Scratch2
		if dst == amd64Scratch2 {
			scr = amd64Scratch1
		}
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

	// Guard: if b is in RAX or RDX, save it to scratch before we clobber
	// those registers. (Currently the pool excludes RAX/RDX when DIV is
	// present, but this is a safety net.)
	bEff := b
	if b == goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch1)
		bEff = amd64Scratch1
	} else if b == goasm.REG_AMD64_DX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch1)
		bEff = amd64Scratch1
	}

	// Move dividend to RAX.
	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	if signed {
		// CQO: sign-extend RAX to RDX:RAX
		p := lc.c.NewProg()
		p.As = x86.ACQO
		lc.c.Append(p)
		// IDIVQ bEff
		p = lc.c.NewProg()
		p.As = x86.AIDIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = bEff
		lc.c.Append(p)
	} else {
		// XORQ RDX, RDX
		lc.emitRR(x86.AXORQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
		// DIVQ bEff
		p := lc.c.NewProg()
		p.As = x86.ADIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = bEff
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

	// Guard: if b is in RAX, MOV a→RAX would clobber it.
	bEff := b
	if b == goasm.REG_AMD64_AX && a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch1)
		bEff = amd64Scratch1
	}

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
	p.From.Reg = bEff
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

	// Guard: if b is in RAX, MOV a→RAX would clobber it.
	bEff := b
	if b == goasm.REG_AMD64_AX && a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch2) // R11 = b
		bEff = amd64Scratch2
	}

	// Move a to RAX.
	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	// Compute sign correction: if a < 0, correction = b, else 0.
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_AX, amd64Scratch1) // R10 = a (from RAX)
	lc.emitRI(x86.ASARQ, 63, amd64Scratch1)                  // R10 = sign(a) replicated
	lc.emitRR(x86.AANDQ, bEff, amd64Scratch1)                // R10 = (a<0) ? b : 0
	p := lc.c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = bEff
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

	// x86 variable shifts require count in CL.
	//
	// Hazard: use(B,1) may return R11 (scratch2) for a spill-loaded count.
	// If we then save CX to R11 with a plain MOV, we clobber the count.
	// Fix: use XCHG when b occupies a scratch register — this atomically
	// swaps the CX save and count load without destroying either value.

	needCXSave := b != goasm.REG_AMD64_CX && dst != goasm.REG_AMD64_CX && lc.isCXLive()
	bInScratch := (b == amd64Scratch1 || b == amd64Scratch2)

	aSavedToR11 := false
	cxSaveReg := int16(-1) // where old CX is saved (-1 = not saved)

	if a == goasm.REG_AMD64_CX && b != goasm.REG_AMD64_CX {
		// CX holds 'a'; moving b→CX will destroy it.
		if bInScratch {
			// b is in a scratch reg (from spill). XCHG swaps: CX→scratch (save a),
			// scratch→CX (load count). One instruction, no clobber.
			lc.emitRR(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
			cxSaveReg = b
			aSavedToR11 = true
			b = goasm.REG_AMD64_CX // count is now in CX
		} else {
			// b is in a regular allocated register. Safe to save a to R11.
			lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
			aSavedToR11 = true
			cxSaveReg = amd64Scratch2
		}
	} else if needCXSave {
		// CX holds a live VReg (not a, not b, not dst). Must save it.
		if bInScratch {
			// b is in a scratch reg (from spill). XCHG saves CX and loads
			// count into CX in one step.
			lc.emitRR(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
			cxSaveReg = b
			b = goasm.REG_AMD64_CX // count is now in CX
		} else {
			// b is in a regular register. Safe to save CX to R11.
			lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
			cxSaveReg = amd64Scratch2
		}
	}

	// Move count (b) into CX if not already there.
	if b != goasm.REG_AMD64_CX {
		lc.emitRR(x86.AMOVQ, b, goasm.REG_AMD64_CX)
	}

	// Effective location of 'a': saved location if we saved it, else original.
	aEff := a
	if aSavedToR11 {
		aEff = cxSaveReg // wherever the XCHG/MOV put it
	}

	// Move value (a) into dst and shift.
	if dst == goasm.REG_AMD64_CX {
		// dst IS CX — we just put the count there. Use scratch for the
		// shift, then move result back to CX.
		scr := amd64Scratch1
		if aEff != scr {
			lc.emitRR(x86.AMOVQ, aEff, scr)
		}
		lc.emitRR(op, goasm.REG_AMD64_CX, scr)
		lc.emitRR(x86.AMOVQ, scr, dst)
	} else {
		if dst != aEff {
			lc.emitRR(x86.AMOVQ, aEff, dst)
		}
		lc.emitRR(op, goasm.REG_AMD64_CX, dst)
	}

	// Restore CX if we saved it. Even when aSavedToR11 (a was in CX), the
	// VReg may still be live for future instructions — reading it now doesn't
	// make it dead, so CX must be restored.
	if cxSaveReg >= 0 && needCXSave {
		lc.emitRR(x86.AMOVQ, cxSaveReg, goasm.REG_AMD64_CX)
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
	if lc.fixed != nil {
		return lc.fixed.CXAssigned
	}
	// Binary search the precomputed sorted CX intervals.
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
	srcInReg := src != VRegZero && lc.allocKind(src) == AllocReg
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
			lc.spillLoad(lc.spillSlot(src), amd64Scratch1)
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
	if lc.fixed != nil {
		return lc.fixed.IsHostLive(hostReg)
	}
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
		// SETE + SETNP → AND result. Must use distinct byte regs.
		p1 := lc.c.NewProg()
		p1.As = x86.ASETEQ
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(amd64Scratch1)
		if dst == amd64Scratch1 {
			scrByte = byteReg(amd64Scratch2)
		}
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
		if dst == amd64Scratch1 {
			scrByte = byteReg(amd64Scratch2)
		}
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
