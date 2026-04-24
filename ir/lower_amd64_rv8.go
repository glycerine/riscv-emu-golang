package ir

// lower_amd64_rv8.go — rv8-faithful AMD64 lowerer.
//
// Matches the CARRV 2017 paper's register layout:
//   RBP       = register file base (&cpu.x[0])
//   RAX, RCX  = translator temps (staging)
//   RSP       = host stack pointer
//   12 allocatable GPRs: RDX,RBX,RSI,RDI,R8-R15
//
// Sret pointer is stashed on the stack, freeing RBX for RISC-V sp.
// Spill addressing: int [RBP+r*8], FP [RBP+256+r*8].

import (
	"fmt"
	"sort"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// rv8 staging register constants.
const (
	rv8StgA  int16 = goasm.REG_AMD64_AX  // integer staging slot A
	rv8StgB  int16 = goasm.REG_AMD64_CX  // integer staging slot B
	rv8StgFA int16 = goasm.REG_AMD64_X15 // FP staging slot A
	rv8StgFB int16 = goasm.REG_AMD64_X14 // FP staging slot B
)

// rv8 register file offsets (relative to RBP).
const (
	rv8IntRegOffset = 0   // x[r] at [RBP + r*8]
	rv8FPRegOffset  = 256 // f[r] at [RBP + 256 + r*8]
)

type lowerCtxRV8 struct {
	blk   *Block
	alloc *Allocation
	c     *goasm.Ctx
	idx   int

	rIdx   regIndex
	fpSet  map[VReg]bool
	cxLive []regEntry

	labelProg map[Label]*obj.Prog
	pending   map[Label][]*obj.Prog

	stackSlots int
	frameSize  int64 // total frame bytes: sret(8) + spillSlots*8
	sretOffset int64 // offset of sret pointer within frame

	chainEntryProg *obj.Prog
	chainExits     []chainExitInfo
	jalrICs        []jalrICInfo
}

// LowerAMD64_RV8 converts a register-allocated IR Block into x86-64 machine
// code using the rv8-faithful register layout.
func LowerAMD64_RV8(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	if alloc == nil {
		return nil, fmt.Errorf("ir.LowerAMD64_RV8: nil allocation")
	}

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

	lc := &lowerCtxRV8{
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

	// Frame layout:
	//   [RSP+0 .. spillSlots*8-1] = spill slots
	//   [RSP+spillSlots*8]        = sret pointer (8 bytes)
	//   [RSP+spillSlots*8+8]      = scratch A (8 bytes, for DIV/MUL RDX save, ret IC save)
	//   [RSP+spillSlots*8+16]     = scratch B (8 bytes, for retDyn PC save)
	// Total = spillSlots*8 + 24.
	lc.sretOffset = int64(lc.stackSlots) * 8
	lc.frameSize = lc.sretOffset + 24

	lc.emitPrologue()

	for idx := range b.Instrs {
		lc.idx = idx
		if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
			return nil, err
		}
	}

	if len(lc.pending) > 0 {
		return nil, fmt.Errorf("ir.LowerAMD64_RV8: %d unresolved forward labels", len(lc.pending))
	}

	// Emit slow exit stubs for chain exits that aren't resolved at link time.
	for i := range lc.chainExits {
		lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
	}

	result := &LowerResult{
		ChainEntryProg: lc.chainEntryProg,
	}
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

// ── Prologue / Epilogue ──

func (lc *lowerCtxRV8) emitPrologue() {
	// ── First-entry path ──
	// RBP = register file base, RSI by trampoline ABI.
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_SI, goasm.REG_AMD64_BP)

	// Publish memBase (R8) and memMask (R9) into the sret buffer so they
	// survive across chained blocks. Uses RDI (sret) directly — must
	// happen BEFORE the IC zero which may clobber DI.
	lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_R8, goasm.REG_AMD64_DI, 128)
	lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_R9, goasm.REG_AMD64_DI, 136)

	// Zero sret.IC for first entry. On chain entry, sret.IC holds the
	// previous block's IC value (written by rv8ChainExit).
	lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_DI, 8)

	// Copy sret from RDI to RAX so first-entry and chain-entry share
	// the same code path below (chain entry arrives with RAX=sret).
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DI, rv8StgA)

	// ── Chain entry point ──
	// Chained blocks JMP here with RAX=sret, RBP already set.
	// Both first-entry (falls through) and chain-entry execute
	// everything below.
	lc.chainEntryProg = lc.c.NewProg()
	lc.chainEntryProg.As = obj.ANOP
	lc.c.Append(lc.chainEntryProg)

	// ── Shared path (first + chain) ──
	// Allocate frame.
	lc.emitRI(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)

	// Stash sret from RAX.
	lc.emitMR(x86.AMOVQ, rv8StgA, goasm.REG_AMD64_SP, lc.sretOffset)

	// Load allocated RISC-V integer registers from register file.
	for vr := VReg(1); vr < 32; vr++ {
		if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
			host := lc.rIdx.lookup(vr, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, int64(vr)*8, host)
			}
		}
	}

	// Load allocated FP registers from register file.
	for vr := VReg(32); vr < 64; vr++ {
		if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
			host := lc.rIdx.lookup(vr, 0)
			if host >= 0 {
				off := int64(rv8FPRegOffset) + int64(vr-32)*8
				lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_BP, off, host)
			}
		}
	}

	// Initialize parameter VRegs that can't be resolved statically.
	// VRXBase/VRFBase/VRRegFile are handled in stageInt (always RBP-based).
	// VRIC, VRMemBase, VRMemMask need explicit initialization here.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgB) // RCX = sret

	// Load IC from sret.IC. On first entry this is 0 (zeroed in the
	// first-entry path). On chain entry this carries forward from the
	// previous block (written by rv8ChainExit before frame dealloc).
	// Must happen AFTER RISC-V reg loads since those may clobber IC's
	// host register.
	if int(VRIC) < len(lc.alloc.Kind) {
		lc.emitRM(x86.AMOVQ, rv8StgB, 8, rv8StgA) // RAX = sret.IC
		switch lc.alloc.Kind[VRIC] {
		case AllocReg:
			host := lc.rIdx.lookup(VRIC, 0)
			if host >= 0 {
				lc.emit2(x86.AMOVQ, rv8StgA, host)
			}
		case AllocStack:
			lc.storeSpill(rv8StgA, lc.alloc.SpillSlot[VRIC])
		}
	}

	// Load VRMemBase/VRMemMask from sret buffer AFTER RISC-V regs,
	// because a RISC-V reg may share the same host register and we
	// need memBase/memMask to win.
	if int(VRMemBase) < len(lc.alloc.Kind) {
		switch lc.alloc.Kind[VRMemBase] {
		case AllocReg:
			host := lc.rIdx.lookup(VRMemBase, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, rv8StgB, 128, host)
			}
		case AllocStack:
			lc.emitRM(x86.AMOVQ, rv8StgB, 128, rv8StgA)
			lc.storeSpill(rv8StgA, lc.alloc.SpillSlot[VRMemBase])
		}
	}
	if int(VRMemMask) < len(lc.alloc.Kind) {
		switch lc.alloc.Kind[VRMemMask] {
		case AllocReg:
			host := lc.rIdx.lookup(VRMemMask, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, rv8StgB, 136, host)
			}
		case AllocStack:
			lc.emitRM(x86.AMOVQ, rv8StgB, 136, rv8StgA)
			lc.storeSpill(rv8StgA, lc.alloc.SpillSlot[VRMemMask])
		}
	}
}

// storeRegsBack writes all allocated RISC-V registers back to the
// register file at [RBP + vr*8]. Called before returning.
func (lc *lowerCtxRV8) storeRegsBack() {
	for vr := VReg(1); vr < 32; vr++ {
		if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
			host := lc.rIdx.lookup(vr, lc.idx)
			if host >= 0 {
				lc.emitMR(x86.AMOVQ, host, goasm.REG_AMD64_BP, int64(vr)*8)
			}
		}
	}
	for vr := VReg(32); vr < 64; vr++ {
		if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
			host := lc.rIdx.lookup(vr, lc.idx)
			if host >= 0 {
				off := int64(rv8FPRegOffset) + int64(vr-32)*8
				lc.emitMR(x86.AMOVSD, host, goasm.REG_AMD64_BP, off)
			}
		}
	}
}

func (lc *lowerCtxRV8) emitEpilogue() {
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)
}

// ── Instruction dispatch ──

func (lc *lowerCtxRV8) lowerInstr(ins *IRInstr) error {
	switch ins.Op {
	case IROpInvalid:
		return fmt.Errorf("ir.LowerAMD64_RV8: invalid op at index %d", lc.idx)

	// Data movement
	case IRMov:
		lc.rv8Mov(ins)
	case IRConst:
		lc.rv8Const(ins)
	case IRSext:
		lc.rv8Sext(ins)
	case IRZext:
		lc.rv8Zext(ins)

	// Integer ALU
	case IRAdd:
		lc.rv8Binop(ins, x86.AADDQ)
	case IRAddImm:
		lc.rv8BinopImm(ins, x86.AADDQ)
	case IRSub:
		lc.rv8Binop(ins, x86.ASUBQ)
	case IRSubImm:
		lc.rv8BinopImm(ins, x86.ASUBQ)
	case IRMul:
		lc.rv8Binop(ins, x86.AIMULQ)
	case IRNeg:
		lc.rv8Unary(ins, x86.ANEGQ)

	// DIV/MUL high
	case IRDivS:
		lc.rv8Div(ins, true, false)
	case IRDivU:
		lc.rv8Div(ins, false, false)
	case IRRem:
		lc.rv8Div(ins, true, true)
	case IRRemU:
		lc.rv8Div(ins, false, true)
	case IRMulHS:
		lc.rv8MulHigh(ins, true)
	case IRMulHU:
		lc.rv8MulHigh(ins, false)
	case IRMulHSU:
		lc.rv8MulHSU(ins)

	// Shifts
	case IRShl:
		lc.rv8Shift(ins, x86.ASHLQ)
	case IRShlImm:
		lc.rv8ShiftImm(ins, x86.ASHLQ)
	case IRShr:
		lc.rv8Shift(ins, x86.ASHRQ)
	case IRShrImm:
		lc.rv8ShiftImm(ins, x86.ASHRQ)
	case IRSar:
		lc.rv8Shift(ins, x86.ASARQ)
	case IRSarImm:
		lc.rv8ShiftImm(ins, x86.ASARQ)

	// Bitwise
	case IRAnd:
		lc.rv8Binop(ins, x86.AANDQ)
	case IRAndImm:
		lc.rv8BinopImm(ins, x86.AANDQ)
	case IROr:
		lc.rv8Binop(ins, x86.AORQ)
	case IROrImm:
		lc.rv8BinopImm(ins, x86.AORQ)
	case IRXor:
		lc.rv8Binop(ins, x86.AXORQ)
	case IRXorImm:
		lc.rv8BinopImm(ins, x86.AXORQ)
	case IRNot:
		lc.rv8Unary(ins, x86.ANOTQ)

	// Bit manipulation
	case IRClz:
		if ins.T == I32 {
			lc.rv8Unary(ins, x86.ALZCNTL)
		} else {
			lc.rv8Unary(ins, x86.ALZCNTQ)
		}
	case IRCtz:
		if ins.T == I32 {
			lc.rv8Unary(ins, x86.ATZCNTL)
		} else {
			lc.rv8Unary(ins, x86.ATZCNTQ)
		}
	case IRPopcount:
		if ins.T == I32 {
			lc.rv8Unary(ins, x86.APOPCNTL)
		} else {
			lc.rv8Unary(ins, x86.APOPCNTQ)
		}
	case IRBswap:
		lc.rv8Unary(ins, x86.ABSWAPQ)

	// Comparison
	case IRSet:
		lc.rv8Set(ins)
	case IRSetImm:
		lc.rv8SetImm(ins)

	// Memory
	case IRLoad:
		lc.rv8Load(ins)
	case IRStore:
		lc.rv8Store(ins)
	case IRLoadX:
		lc.rv8LoadX(ins)
	case IRStoreX:
		lc.rv8StoreX(ins)

	// Control flow
	case IRLabel:
		lc.placeLabel(Label(ins.Imm))
	case IRBranch:
		lc.rv8Branch(ins)
	case IRBranchImm:
		lc.rv8BranchImm(ins)
	case IRJump:
		lc.rv8Jump(ins)
	case IRRet:
		lc.rv8Ret(ins)
	case IRRetDyn:
		lc.rv8RetDyn(ins)
	case IRChainExit:
		lc.rv8ChainExit(ins)
	case IRJalrIC:
		lc.rv8JalrIC(ins)
	case IRCall:
		lc.rv8Call(ins)
	case IRSyscall:
		lc.rv8Syscall(ins)

	// FP arithmetic
	case IRFAdd:
		lc.rv8FPBinop(ins, x86.AADDSD, x86.AADDSS)
	case IRFSub:
		lc.rv8FPBinop(ins, x86.ASUBSD, x86.ASUBSS)
	case IRFMul:
		lc.rv8FPBinop(ins, x86.AMULSD, x86.AMULSS)
	case IRFDiv:
		lc.rv8FPBinop(ins, x86.ADIVSD, x86.ADIVSS)
	case IRFSqrt:
		lc.rv8FPUnary(ins, x86.ASQRTSD, x86.ASQRTSS)
	case IRFNeg:
		lc.rv8FNeg(ins)
	case IRFAbs:
		lc.rv8FAbs(ins)
	case IRFCmp:
		lc.rv8FCmp(ins)

	// FP conversions
	case IRFCvtToI:
		lc.rv8FCvtToI(ins)
	case IRFCvtToU:
		lc.rv8FCvtToI(ins)
	case IRFCvtFromI:
		lc.rv8FCvtFromI(ins)
	case IRFCvtFromU:
		lc.rv8FCvtFromI(ins)
	case IRFCvtFF:
		lc.rv8FCvtFF(ins)

	// Pseudo-ops
	case IRMarkLive, IRMarkDead, IRWriteback:
		// no-op

	default:
		return fmt.Errorf("ir.LowerAMD64_RV8: unhandled op %v at index %d", ins.Op, lc.idx)
	}
	return nil
}

// regFileOff returns the register-file offset relative to RBP for
// RISC-V integer (VReg 1-31) and FP (VReg 32-63) registers.
// Returns -1 for VRegZero, parameter VRegs, and temps.
func regFileOff(v VReg) int64 {
	if v >= 1 && v < 32 {
		return int64(v) * 8
	}
	if v >= 32 && v < 64 {
		return int64(rv8FPRegOffset) + int64(v-32)*8
	}
	return -1
}

// spilledRegFileOff returns the register-file offset for a VReg that
// is a spilled RISC-V register (not allocated to a host register).
// Returns -1 if the VReg is in a host register, is a temp, or is VRegZero.
func (lc *lowerCtxRV8) spilledRegFileOff(v VReg) int64 {
	if v == VRegZero {
		return -1
	}
	off := regFileOff(v)
	if off < 0 {
		return -1
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		return off
	}
	return -1
}

// spilledMemOp returns the base register and offset for any spilled
// integer VReg: RISC-V int registers use [RBP+r*8], temps use [RSP+slot*8].
// Excludes FP VRegs (32-63) and FP-typed temps.
func (lc *lowerCtxRV8) spilledMemOp(v VReg) (base int16, off int64, ok bool) {
	if v == VRegZero {
		return 0, 0, false
	}
	if v >= 32 && v < 64 {
		return 0, 0, false
	}
	if lc.isVRegFP(v) {
		return 0, 0, false
	}
	if int(v) >= len(lc.alloc.Kind) || lc.alloc.Kind[v] != AllocStack {
		return 0, 0, false
	}
	if rfOff := regFileOff(v); rfOff >= 0 {
		return goasm.REG_AMD64_BP, rfOff, true
	}
	return goasm.REG_AMD64_SP, int64(lc.alloc.SpillSlot[v]) * 8, true
}

// ── Staging helpers ──

func (lc *lowerCtxRV8) stageInt(v VReg, idx int) int16 {
	stg := rv8StgA
	if idx != 0 {
		stg = rv8StgB
	}
	if v == VRegZero {
		lc.emit2(x86.AXORQ, stg, stg)
		return stg
	}
	// VRXBase and VRRegFile are the register file base, always in RBP.
	if v == VRXBase || v == VRRegFile {
		lc.emit2(x86.AMOVQ, goasm.REG_AMD64_BP, stg)
		return stg
	}
	// VRFBase is the FP register file base at RBP+256.
	if v == VRFBase {
		p := lc.c.NewProg()
		p.As = x86.ALEAQ
		p.From.Type = obj.TYPE_MEM
		p.From.Reg = goasm.REG_AMD64_BP
		p.From.Offset = int64(rv8FPRegOffset)
		p.To.Type = obj.TYPE_REG
		p.To.Reg = stg
		lc.c.Append(p)
		return stg
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		if hr != stg {
			lc.emit2(x86.AMOVQ, hr, stg)
		}
		return stg
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if off := regFileOff(v); off >= 0 {
			lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, off, stg)
		} else {
			lc.loadSpill(lc.alloc.SpillSlot[v], stg)
		}
		return stg
	}
	lc.emit2(x86.AXORQ, stg, stg)
	return stg
}

func (lc *lowerCtxRV8) stageFP(v VReg, idx int) int16 {
	stg := rv8StgFA
	if idx != 0 {
		stg = rv8StgFB
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
		if off := regFileOff(v); off >= 0 {
			lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_BP, off, stg)
		} else {
			lc.loadFPSpill(lc.alloc.SpillSlot[v], stg)
		}
		return stg
	}
	lc.emit2(x86.APXOR, stg, stg)
	return stg
}

func (lc *lowerCtxRV8) writeDst(v VReg) int16 {
	if v == VRegZero {
		return rv8StgA
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		return hr
	}
	return rv8StgA
}

func (lc *lowerCtxRV8) writeDstFP(v VReg) int16 {
	if v == VRegZero {
		return rv8StgFA
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		return hr
	}
	return rv8StgFA
}

func (lc *lowerCtxRV8) commitDst(v VReg, hostReg int16) {
	if v == VRegZero {
		return
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if off := regFileOff(v); off >= 0 {
			if isXMMReg(hostReg) {
				lc.emitMR(x86.AMOVSD, hostReg, goasm.REG_AMD64_BP, off)
			} else {
				lc.emitMR(x86.AMOVQ, hostReg, goasm.REG_AMD64_BP, off)
			}
		} else if isXMMReg(hostReg) {
			lc.storeFPSpill(hostReg, lc.alloc.SpillSlot[v])
		} else {
			lc.storeSpill(hostReg, lc.alloc.SpillSlot[v])
		}
	}
}

// stageICToScratch saves the instruction counter to [RSP+sretOffset+8]
// before storeRegsBack, which might clobber the host register.
// Returns true if the IC was staged (caller should write it to sret).
func (lc *lowerCtxRV8) stageICToScratch() bool {
	if int(VRIC) >= len(lc.alloc.Kind) {
		return false
	}
	switch lc.alloc.Kind[VRIC] {
	case AllocReg:
		hr := lc.hostReg(VRIC)
		if hr >= 0 {
			lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_SP, lc.sretOffset+8)
			return true
		}
	case AllocStack:
		lc.loadSpill(lc.alloc.SpillSlot[VRIC], rv8StgA)
		lc.emitMR(x86.AMOVQ, rv8StgA, goasm.REG_AMD64_SP, lc.sretOffset+8)
		return true
	}
	return false
}

// ── Register resolution ──

func (lc *lowerCtxRV8) hostReg(v VReg) int16 {
	if v == VRegZero || int(v) >= len(lc.alloc.Kind) {
		return -1
	}
	if lc.alloc.Kind[v] != AllocReg {
		return -1
	}
	return lc.rIdx.lookup(v, lc.idx)
}

// directReg returns the host GPR for v only if it's safe to use
// directly (without staging). Excludes parameter VRegs (which have
// special handling in stageInt) and XMM registers (which need FP
// staging paths, not integer MOVQ).
func (lc *lowerCtxRV8) directReg(v VReg) int16 {
	if v == VRegZero {
		return -1
	}
	switch v {
	case VRXBase, VRFBase, VRIC, VRMemBase, VRMemMask, VRRegFile:
		return -1
	}
	hr := lc.hostReg(v)
	if hr >= 0 && isXMMReg(hr) {
		return -1
	}
	return hr
}

func (lc *lowerCtxRV8) isVRegFP(v VReg) bool {
	if v == VRegZero {
		return false
	}
	return lc.fpSet[v]
}

func (lc *lowerCtxRV8) isRegLive(hostReg int16) bool {
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

func (lc *lowerCtxRV8) emit2(op obj.As, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) emitRI(op obj.As, imm int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) emitRM(op obj.As, base int16, off int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = off
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) emitMR(op obj.As, src int16, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) emitMI(op obj.As, imm int64, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) emitUnary(op obj.As, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) emitCmpRI(reg int16, imm int64) {
	p := lc.c.NewProg()
	p.As = x86.ACMPQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = reg
	p.To.Type = obj.TYPE_CONST
	p.To.Offset = imm
	lc.c.Append(p)
}

func (lc *lowerCtxRV8) loadImm(imm int64, dst int16) {
	if imm == 0 {
		lc.emit2(x86.AXORQ, dst, dst)
		return
	}
	lc.emitRI(x86.AMOVQ, imm, dst)
}

func (lc *lowerCtxRV8) loadSpill(slot int16, dst int16) {
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

func (lc *lowerCtxRV8) storeSpill(src int16, slot int16) {
	lc.emitMR(x86.AMOVQ, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

func (lc *lowerCtxRV8) loadFPSpill(slot int16, dst int16) {
	lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

func (lc *lowerCtxRV8) storeFPSpill(src int16, slot int16) {
	lc.emitMR(x86.AMOVSD, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// ── Label resolution ──

func (lc *lowerCtxRV8) placeLabel(l Label) {
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

func (lc *lowerCtxRV8) bindLabel(l Label, branch *obj.Prog) {
	if target, ok := lc.labelProg[l]; ok {
		branch.To.SetTarget(target)
	} else {
		lc.pending[l] = append(lc.pending[l], branch)
	}
}

// ── Data movement ──

func (lc *lowerCtxRV8) rv8Mov(ins *IRInstr) {
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	if dstHR >= 0 && aHR >= 0 {
		if dstHR != aHR {
			lc.emit2(x86.AMOVQ, aHR, dstHR)
		}
		lc.commitDst(ins.Dst, dstHR)
		return
	}
	if dstHR >= 0 {
		if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
			lc.emitRM(x86.AMOVQ, aBase, aOff, dstHR)
			lc.commitDst(ins.Dst, dstHR)
			return
		}
	}
	if aHR >= 0 {
		if dBase, dOff, ok := lc.spilledMemOp(ins.Dst); ok {
			lc.emitMR(x86.AMOVQ, aHR, dBase, dOff)
			return
		}
	}

	a := lc.stageInt(ins.A, 0)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8Const(ins *IRInstr) {
	dst := lc.writeDst(ins.Dst)
	lc.loadImm(ins.Imm, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8Sext(ins *IRInstr) {
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
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	if dstHR >= 0 && aHR >= 0 {
		lc.emit2(op, aHR, dstHR)
		lc.commitDst(ins.Dst, dstHR)
		return
	}
	if dstHR >= 0 {
		if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
			lc.emitRM(op, aBase, aOff, dstHR)
			lc.commitDst(ins.Dst, dstHR)
			return
		}
	}

	a := lc.stageInt(ins.A, 0)
	dst := lc.writeDst(ins.Dst)
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8Zext(ins *IRInstr) {
	var op obj.As
	switch ins.T {
	case I8:
		op = x86.AMOVBQZX
	case I16:
		op = x86.AMOVWQZX
	case I32:
		op = x86.AMOVL
	default:
		op = x86.AMOVQ
	}
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	if dstHR >= 0 && aHR >= 0 {
		lc.emit2(op, aHR, dstHR)
		lc.commitDst(ins.Dst, dstHR)
		return
	}
	if dstHR >= 0 {
		if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
			lc.emitRM(op, aBase, aOff, dstHR)
			lc.commitDst(ins.Dst, dstHR)
			return
		}
	}

	a := lc.stageInt(ins.A, 0)
	dst := lc.writeDst(ins.Dst)
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

// ── Integer ALU ──

func (lc *lowerCtxRV8) rv8Binop(ins *IRInstr, op obj.As) {
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	bHR := lc.directReg(ins.B)

	if dstHR >= 0 && aHR >= 0 && (dstHR != bHR || dstHR == aHR) {
		if dstHR != aHR {
			lc.emit2(x86.AMOVQ, aHR, dstHR)
		}
		if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
			lc.emitRM(op, bBase, bOff, dstHR)
		} else if bHR >= 0 {
			lc.emit2(op, bHR, dstHR)
		} else {
			b := lc.stageInt(ins.B, 1)
			lc.emit2(op, b, dstHR)
		}
		lc.commitDst(ins.Dst, dstHR)
		return
	}

	a := lc.stageInt(ins.A, 0)
	if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
		lc.emitRM(op, bBase, bOff, a)
	} else {
		b := lc.stageInt(ins.B, 1)
		lc.emit2(op, b, a)
	}
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8BinopImm(ins *IRInstr, op obj.As) {
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)

	if dstHR >= 0 && aHR >= 0 {
		if dstHR != aHR {
			lc.emit2(x86.AMOVQ, aHR, dstHR)
		}
		imm := ins.Imm
		if imm >= -(1<<31) && imm < (1<<31) {
			lc.emitRI(op, imm, dstHR)
		} else {
			lc.loadImm(imm, rv8StgB)
			lc.emit2(op, rv8StgB, dstHR)
		}
		lc.commitDst(ins.Dst, dstHR)
		return
	}

	if ins.Dst == ins.A {
		if off := lc.spilledRegFileOff(ins.Dst); off >= 0 {
			imm := ins.Imm
			if imm >= -(1<<31) && imm < (1<<31) {
				lc.emitMI(op, imm, goasm.REG_AMD64_BP, off)
				return
			}
		}
	}

	if dstHR >= 0 {
		if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
			lc.emitRM(x86.AMOVQ, aBase, aOff, dstHR)
			imm := ins.Imm
			if imm >= -(1<<31) && imm < (1<<31) {
				lc.emitRI(op, imm, dstHR)
			} else {
				lc.loadImm(imm, rv8StgB)
				lc.emit2(op, rv8StgB, dstHR)
			}
			lc.commitDst(ins.Dst, dstHR)
			return
		}
	}

	a := lc.stageInt(ins.A, 0)
	imm := ins.Imm
	if imm >= -(1<<31) && imm < (1<<31) {
		lc.emitRI(op, imm, a)
	} else {
		lc.loadImm(imm, rv8StgB)
		lc.emit2(op, rv8StgB, a)
	}
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8Unary(ins *IRInstr, op obj.As) {
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	if dstHR >= 0 && aHR >= 0 {
		if dstHR != aHR {
			lc.emit2(x86.AMOVQ, aHR, dstHR)
		}
		lc.emitUnary(op, dstHR)
		lc.commitDst(ins.Dst, dstHR)
		return
	}

	a := lc.stageInt(ins.A, 0)
	lc.emitUnary(op, a)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── Shifts ──
// CX is a staging reg in rv8, so it's always available for shifts — no save needed.

func (lc *lowerCtxRV8) rv8Shift(ins *IRInstr, op obj.As) {
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	if dstHR >= 0 && aHR >= 0 {
		lc.stageInt(ins.B, 1) // B → RCX (before MOV A→Dst to avoid clobber)
		if dstHR != aHR {
			lc.emit2(x86.AMOVQ, aHR, dstHR)
		}
		lc.emit2(op, goasm.REG_AMD64_CX, dstHR)
		lc.commitDst(ins.Dst, dstHR)
		return
	}

	a := lc.stageInt(ins.A, 0)
	lc.stageInt(ins.B, 1)
	lc.emit2(op, goasm.REG_AMD64_CX, a)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8ShiftImm(ins *IRInstr, op obj.As) {
	dstHR := lc.directReg(ins.Dst)
	aHR := lc.directReg(ins.A)
	if dstHR >= 0 && aHR >= 0 {
		if dstHR != aHR {
			lc.emit2(x86.AMOVQ, aHR, dstHR)
		}
		lc.emitRI(op, ins.Imm, dstHR)
		lc.commitDst(ins.Dst, dstHR)
		return
	}

	if ins.Dst == ins.A {
		if off := lc.spilledRegFileOff(ins.Dst); off >= 0 {
			lc.emitMI(op, ins.Imm, goasm.REG_AMD64_BP, off)
			return
		}
	}
	if dstHR >= 0 {
		if aBase, aOff, ok := lc.spilledMemOp(ins.A); ok {
			lc.emitRM(x86.AMOVQ, aBase, aOff, dstHR)
			lc.emitRI(op, ins.Imm, dstHR)
			lc.commitDst(ins.Dst, dstHR)
			return
		}
	}

	a := lc.stageInt(ins.A, 0)
	lc.emitRI(op, ins.Imm, a)
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── Division ──
// RAX is staging reg A, always available. RDX may hold a live RISC-V reg
// (ra), so we save/restore it locally.

func (lc *lowerCtxRV8) rv8Div(ins *IRInstr, signed, wantRem bool) {
	a := lc.stageInt(ins.A, 0) // RAX = dividend
	b := lc.stageInt(ins.B, 1) // RCX = divisor

	// Save RDX if it holds a live RISC-V register.
	rdxLive := lc.isRegLive(goasm.REG_AMD64_DX)
	if rdxLive {
		lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_SP, lc.sretOffset+8)
	}

	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_AX)

	if signed {
		p := lc.c.NewProg()
		p.As = x86.ACQO
		lc.c.Append(p)
		p = lc.c.NewProg()
		p.As = x86.AIDIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = b
		lc.c.Append(p)
	} else {
		lc.emit2(x86.AXORQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
		p := lc.c.NewProg()
		p.As = x86.ADIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = b
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

	// Restore RDX.
	if rdxLive {
		if dst == goasm.REG_AMD64_DX {
			// Result went into RDX — we've committed it above.
		} else {
			lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, goasm.REG_AMD64_DX)
		}
	}
	lc.commitDst(ins.Dst, dst)
}

// ── MulHigh ──

func (lc *lowerCtxRV8) rv8MulHigh(ins *IRInstr, signed bool) {
	a := lc.stageInt(ins.A, 0)
	b := lc.stageInt(ins.B, 1)

	rdxLive := lc.isRegLive(goasm.REG_AMD64_DX)
	if rdxLive {
		lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_SP, lc.sretOffset+8)
	}

	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_AX)

	p := lc.c.NewProg()
	if signed {
		p.As = x86.AIMULQ
	} else {
		p.As = x86.AMULQ
	}
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	lc.c.Append(p)

	dst := lc.writeDst(ins.Dst)
	if dst != goasm.REG_AMD64_DX {
		lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}

	if rdxLive && dst != goasm.REG_AMD64_DX {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, goasm.REG_AMD64_DX)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8MulHSU(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // RAX = signed operand
	b := lc.stageInt(ins.B, 1) // RCX = unsigned operand

	rdxLive := lc.isRegLive(goasm.REG_AMD64_DX)
	if rdxLive {
		lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_SP, lc.sretOffset+8)
	}

	// Compute correction = (A < 0) ? B : 0 using RDX as temp.
	// A is in RAX — keep it there for MULQ.
	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_DX)       // RDX = A
	lc.emitRI(x86.ASARQ, 63, goasm.REG_AMD64_DX)      // RDX = sign(A)
	lc.emit2(x86.AANDQ, b, goasm.REG_AMD64_DX)        // RDX = correction
	lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_SP, lc.sretOffset+16) // save correction

	// Unsigned multiply: RDX:RAX = A * B (A still in RAX).
	p := lc.c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	lc.c.Append(p)

	// Subtract correction from high bits.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+16, rv8StgA)
	lc.emit2(x86.ASUBQ, rv8StgA, goasm.REG_AMD64_DX)

	dst := lc.writeDst(ins.Dst)
	if dst != goasm.REG_AMD64_DX {
		lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}

	if rdxLive && dst != goasm.REG_AMD64_DX {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, goasm.REG_AMD64_DX)
	}
	lc.commitDst(ins.Dst, dst)
}

// ── Comparison ──

func (lc *lowerCtxRV8) rv8Set(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	bHR := lc.directReg(ins.B)
	if aHR >= 0 {
		if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
			lc.emitMR(x86.ACMPQ, aHR, bBase, bOff)
		} else if bHR >= 0 {
			lc.emit2(x86.ACMPQ, aHR, bHR)
		} else {
			b := lc.stageInt(ins.B, 1)
			lc.emit2(x86.ACMPQ, aHR, b)
		}
	} else {
		a := lc.stageInt(ins.A, 0)
		if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
			lc.emitMR(x86.ACMPQ, a, bBase, bOff)
		} else {
			b := lc.stageInt(ins.B, 1)
			lc.emit2(x86.ACMPQ, a, b)
		}
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

func (lc *lowerCtxRV8) rv8SetImm(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	a := aHR
	if a < 0 {
		a = lc.stageInt(ins.A, 0)
	}
	if ins.Imm >= -(1<<31) && ins.Imm < (1<<31) {
		lc.emitCmpRI(a, ins.Imm)
	} else {
		lc.loadImm(ins.Imm, rv8StgB)
		lc.emit2(x86.ACMPQ, a, rv8StgB)
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

func (lc *lowerCtxRV8) rv8Load(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	base := aHR
	if base < 0 {
		base = lc.stageInt(ins.A, 0)
	}
	dst := lc.writeDst(ins.Dst)
	if lc.isVRegFP(ins.Dst) {
		dst = lc.writeDstFP(ins.Dst)
	}
	lc.emitRM(loadOp(ins.T), base, ins.Imm, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8Store(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	base := aHR
	if base < 0 {
		base = lc.stageInt(ins.A, 0)
	}
	if lc.isVRegFP(ins.B) {
		src := lc.stageFP(ins.B, 1)
		lc.emitMR(storeOp(ins.T), src, base, ins.Imm)
	} else {
		bHR := lc.directReg(ins.B)
		if bHR >= 0 && bHR != base {
			lc.emitMR(storeOp(ins.T), bHR, base, ins.Imm)
		} else {
			src := lc.stageInt(ins.B, 1)
			lc.emitMR(storeOp(ins.T), src, base, ins.Imm)
		}
	}
}

func (lc *lowerCtxRV8) rv8LoadX(ins *IRInstr) {
	base := lc.directReg(ins.A)
	if base < 0 {
		base = lc.stageInt(ins.A, 0)
	}
	idx := lc.directReg(ins.B)
	if idx < 0 {
		idx = lc.stageInt(ins.B, 1)
	}
	dst := lc.writeDst(ins.Dst)
	if lc.isVRegFP(ins.Dst) {
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

func (lc *lowerCtxRV8) rv8StoreX(ins *IRInstr) {
	base := lc.directReg(ins.A)
	if base < 0 {
		base = lc.stageInt(ins.A, 0)
	}
	idx := lc.directReg(ins.B)
	if idx < 0 {
		idx = lc.stageInt(ins.B, 1)
	}

	p := lc.c.NewProg()
	p.As = x86.ALEAQ
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Index = idx
	p.From.Scale = int16(ins.Scale)
	p.To.Type = obj.TYPE_REG
	p.To.Reg = rv8StgB
	lc.c.Append(p)

	src := ins.Dst
	if lc.isVRegFP(src) {
		srcReg := lc.stageFP(src, 0)
		lc.emitMR(storeOp(ins.T), srcReg, rv8StgB, 0)
	} else {
		srcReg := lc.stageInt(src, 0)
		lc.emitMR(storeOp(ins.T), srcReg, rv8StgB, 0)
	}
}

// ── Control flow ──

func (lc *lowerCtxRV8) rv8Branch(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	bHR := lc.directReg(ins.B)
	if aHR >= 0 {
		if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
			lc.emitMR(x86.ACMPQ, aHR, bBase, bOff)
		} else if bHR >= 0 {
			lc.emit2(x86.ACMPQ, aHR, bHR)
		} else {
			b := lc.stageInt(ins.B, 1)
			lc.emit2(x86.ACMPQ, aHR, b)
		}
	} else {
		a := lc.stageInt(ins.A, 0)
		if bBase, bOff, ok := lc.spilledMemOp(ins.B); ok {
			lc.emitMR(x86.ACMPQ, a, bBase, bOff)
		} else {
			b := lc.stageInt(ins.B, 1)
			lc.emit2(x86.ACMPQ, a, b)
		}
	}
	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtxRV8) rv8BranchImm(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	a := aHR
	if a < 0 {
		a = lc.stageInt(ins.A, 0)
	}
	if ins.Imm2 >= -(1<<31) && ins.Imm2 < (1<<31) {
		lc.emitCmpRI(a, ins.Imm2)
	} else {
		lc.loadImm(ins.Imm2, rv8StgB)
		lc.emit2(x86.ACMPQ, a, rv8StgB)
	}
	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtxRV8) rv8Jump(ins *IRInstr) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtxRV8) rv8Ret(ins *IRInstr) {
	icStaged := lc.stageICToScratch()

	lc.storeRegsBack()

	// Load sret pointer from stack.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA) // RAX = sret

	// Write Result.PC
	lc.loadImm(ins.Imm, rv8StgB)
	lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 0)

	// Write Result.IC
	if icStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, rv8StgB)
		lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 8)
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 8)
	}

	// Write Result.Status
	lc.emitMI(x86.AMOVQ, ins.Imm2, rv8StgA, 16)

	// Write Result.FaultAddr
	if ins.A != VRegZero {
		fa := lc.stageInt(ins.A, 1) // RCX
		lc.emitMR(x86.AMOVQ, fa, rv8StgA, 24)
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 24)
	}

	lc.emitEpilogue()
}

func (lc *lowerCtxRV8) rv8RetDyn(ins *IRInstr) {
	// Stage dynamic PC from VReg A.
	var pcStaged bool
	var pcSaveOff int64 = lc.sretOffset + 16
	if ins.A != VRegZero {
		hr := lc.hostReg(ins.A)
		if hr >= 0 {
			lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_SP, pcSaveOff)
			pcStaged = true
		}
	}

	icStaged := lc.stageICToScratch()

	lc.storeRegsBack()

	// Load sret.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA)

	// Result.PC
	if pcStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, pcSaveOff, rv8StgB)
		lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 0)
	} else if ins.A != VRegZero {
		pcReg := lc.stageInt(ins.A, 1)
		lc.emitMR(x86.AMOVQ, pcReg, rv8StgA, 0)
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 0)
	}

	// Result.IC
	if icStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, rv8StgB)
		lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 8)
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 8)
	}

	// Result.Status
	lc.emitMI(x86.AMOVQ, ins.Imm, rv8StgA, 16)

	// Result.FaultAddr
	if ins.B != VRegZero {
		fa := lc.stageInt(ins.B, 1)
		lc.emitMR(x86.AMOVQ, fa, rv8StgA, 24)
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 24)
	}

	lc.emitEpilogue()
}

// ── Chain exit/entry ──
//
// Chain exit: store regs back, load sret from stack into RAX, dealloc frame,
// MOVABS RCX=sentinel, JMP RCX. The next block's chain entry receives
// RAX=sret.
//
// Chain entry: MOV [RSP+sretOffset], RAX (re-stash sret from RAX), then
// reload allocated regs from [RBP+r*8]. IC is NOT zeroed (accumulates
// across chained blocks).

func (lc *lowerCtxRV8) rv8ChainExit(ins *IRInstr) {
	// Save IC to sret.IC BEFORE storeRegsBack/dealloc — the slow exit
	// stub reads it from sret after the frame is gone.
	icStaged := lc.stageICToScratch()

	lc.storeRegsBack()

	// Load sret from stack into RAX — carry it to the next block.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA)

	// Write IC to sret.IC while the frame is still alive.
	if icStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, rv8StgB)
		lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 8)
	}

	// Deallocate frame.
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

	// MOVABS RCX, <sentinel> — 10-byte encoding for backpatching.
	const sentinel = int64(0x7BADC0DE7BADC0DE)
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = sentinel
	p.To.Type = obj.TYPE_REG
	p.To.Reg = rv8StgB // RCX
	lc.c.Append(p)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  p,
	})

	// JMP RCX
	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = rv8StgB
	lc.c.Append(jp)
}

// emitSlowExitStub emits a fallback stub for chain exits that can't be
// resolved at link time. The stub stores regs back, writes the target PC
// to the sret result, and returns to the trampoline.
func (lc *lowerCtxRV8) emitSlowExitStub(targetPC uint64) *obj.Prog {
	first := lc.c.NewProg()
	first.As = obj.ANOP
	lc.c.Append(first)

	// RAX holds sret from the chain exit path (loaded before frame dealloc).
	// RBP still points at the register file. Regs were already stored back
	// by the chain exit before it deallocated the frame. sret.IC was
	// written by rv8ChainExit before dealloc — don't overwrite it.

	// Result.PC
	lc.loadImm(int64(targetPC), rv8StgB)
	lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 0)

	// Result.IC — already set by rv8ChainExit; leave it.

	// Result.Status = 0
	lc.emitMI(x86.AMOVQ, 0, rv8StgA, 16)

	// Result.FaultAddr = 0
	lc.emitMI(x86.AMOVQ, 0, rv8StgA, 24)

	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)

	return first
}

func (lc *lowerCtxRV8) rv8Call(ins *IRInstr) {
	if int(ins.Imm) >= len(lc.blk.CTab) {
		return
	}
	sym := lc.blk.CTab[ins.Imm]

	callerSavedInt := []int16{
		goasm.REG_AMD64_DX, goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8, goasm.REG_AMD64_R9, goasm.REG_AMD64_R10,
		goasm.REG_AMD64_R11,
	}
	var liveInt, liveFP []int16
	for _, r := range callerSavedInt {
		if lc.isRegLive(r) {
			liveInt = append(liveInt, r)
		}
	}
	for i := int16(0); i < 14; i++ {
		r := goasm.REG_AMD64_X0 + i
		if lc.isRegLive(r) {
			liveFP = append(liveFP, r)
		}
	}

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

	lc.loadImm(int64(sym.Addr), rv8StgA)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = rv8StgA
	lc.c.Append(p)

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

// ── Syscall ──
//
// IRSyscall: call the SysV dispatcher with (xBase, memBase, memMask).
// On return, RAX holds the status: 0=jitOK (chain-exit to resumePC),
// non-zero=jitEcall (return to Go dispatcher).
// WriteBackAll was already emitted by the emitter before IRSyscall.

func (lc *lowerCtxRV8) rv8Syscall(ins *IRInstr) {
	if int(ins.Imm2) >= len(lc.blk.CTab) {
		lc.rv8Ret(&IRInstr{Op: IRRet, Imm: ins.Imm, Imm2: 1, A: VRegZero})
		return
	}
	sym := lc.blk.CTab[ins.Imm2]

	// Stage IC to scratch BEFORE the CALL — the call clobbers caller-saved regs.
	icStaged := lc.stageICToScratch()

	// Set up SysV args: RDI=xBase(RBP), RSI=memBase, RDX=memMask.
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_BP, goasm.REG_AMD64_DI)

	memBaseHost := lc.hostReg(VRMemBase)
	if memBaseHost >= 0 {
		lc.emit2(x86.AMOVQ, memBaseHost, goasm.REG_AMD64_SI)
	} else if int(VRMemBase) < len(lc.alloc.Kind) && lc.alloc.Kind[VRMemBase] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[VRMemBase], goasm.REG_AMD64_SI)
	} else {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA)
		lc.emitRM(x86.AMOVQ, rv8StgA, 128, goasm.REG_AMD64_SI)
	}

	memMaskHost := lc.hostReg(VRMemMask)
	if memMaskHost >= 0 {
		lc.emit2(x86.AMOVQ, memMaskHost, goasm.REG_AMD64_DX)
	} else if int(VRMemMask) < len(lc.alloc.Kind) && lc.alloc.Kind[VRMemMask] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[VRMemMask], goasm.REG_AMD64_DX)
	} else {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA)
		lc.emitRM(x86.AMOVQ, rv8StgA, 136, goasm.REG_AMD64_DX)
	}

	lc.loadImm(int64(sym.Addr), rv8StgA)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = rv8StgA
	lc.c.Append(p)

	// RAX = dispatcher return: 0=jitOK, non-zero=jitEcall.
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_AX, goasm.REG_AMD64_AX)
	slowPath := lc.c.NewProg()
	slowPath.As = x86.AJNE
	slowPath.To.Type = obj.TYPE_BRANCH
	lc.c.Append(slowPath)

	// Fast path (status=0): write IC to sret, then chain exit.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA) // RAX = sret
	if icStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, rv8StgB)
		lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 8) // sret.IC = saved IC
	}
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

	const sentinel = int64(0x7BADC0DE7BADC0DE)
	movProg := lc.c.NewProg()
	movProg.As = x86.AMOVQ
	movProg.From.Type = obj.TYPE_CONST
	movProg.From.Offset = sentinel
	movProg.To.Type = obj.TYPE_REG
	movProg.To.Reg = rv8StgB
	lc.c.Append(movProg)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  movProg,
	})

	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = rv8StgB
	lc.c.Append(jp)

	// Slow path (status!=0): return with jitEcall + IC.
	slowNop := lc.c.NewProg()
	slowNop.As = obj.ANOP
	lc.c.Append(slowNop)
	slowPath.To.SetTarget(slowNop)

	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA)
	lc.loadImm(ins.Imm, rv8StgB)
	lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 0)  // Result.PC = resumePC
	if icStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, rv8StgB)
		lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 8) // Result.IC = saved IC
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 8)
	}
	lc.emitMI(x86.AMOVQ, 1, rv8StgA, 16)        // Result.Status = jitEcall
	lc.emitMI(x86.AMOVQ, 0, rv8StgA, 24)        // Result.FaultAddr = 0
	lc.emitEpilogue()
}

// ── JALR inline cache (decoder_cache lookup) ──
//
// IRJalrIC: target PC in VReg A, site index in Imm.
// Performs an inline decoder_cache lookup using params published in the
// sret buffer by CallAOT:
//   [sret+88]  = decoderCacheBase
//   [sret+96]  = decoderCacheMask
//   [sret+104] = vaddrBegin
//   [sret+112] = segSize
//
// On hit: chain-exit to the cached chainEntry (no Go round-trip).
// On miss: return to Go with Status=jitOKJalrMiss so the dispatcher
// can compile/lookup and re-enter.

func (lc *lowerCtxRV8) rv8JalrIC(ins *IRInstr) {
	// Save target PC before storeRegsBack clobbers host registers.
	pcSaveOff := lc.sretOffset + 16
	if ins.A != VRegZero {
		hr := lc.hostReg(ins.A)
		if hr >= 0 {
			lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_SP, pcSaveOff)
		} else {
			a := lc.stageInt(ins.A, 0)
			lc.emitMR(x86.AMOVQ, a, goasm.REG_AMD64_SP, pcSaveOff)
		}
	}

	icStaged := lc.stageICToScratch()
	lc.storeRegsBack()

	// Load sret and target PC.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, rv8StgA) // RAX = sret
	if ins.A != VRegZero {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, pcSaveOff, rv8StgB) // RCX = target
	} else {
		lc.emit2(x86.AXORQ, rv8StgB, rv8StgB)
	}

	// Write Result.PC = target (needed on both hit and miss paths).
	lc.emitMR(x86.AMOVQ, rv8StgB, rv8StgA, 0)

	// Write Result.IC.
	if icStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+8, goasm.REG_AMD64_DX)
		lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, rv8StgA, 8)
	} else {
		lc.emitMI(x86.AMOVQ, 0, rv8StgA, 8)
	}

	// Check if decoder_cache is available (dcBase != 0).
	lc.emitRM(x86.AMOVQ, rv8StgA, 88, goasm.REG_AMD64_DX) // DX = dcBase
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
	missJmp := lc.c.NewProg()
	missJmp.As = x86.AJEQ
	missJmp.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp)

	// Bounds check: (target - vaddrBegin) < segSize (unsigned).
	// RCX = target, RAX = sret, DX = dcBase (will reload as needed).
	lc.emit2(x86.AMOVQ, rv8StgB, goasm.REG_AMD64_DX) // DX = target (copy)
	lc.emitRM(x86.AMOVQ, rv8StgA, 104, rv8StgB)      // RCX = vaddrBegin
	lc.emit2(x86.ASUBQ, rv8StgB, goasm.REG_AMD64_DX)  // DX = target - vaddrBegin
	lc.emitRM(x86.AMOVQ, rv8StgA, 112, rv8StgB)       // RCX = segSize
	lc.emit2(x86.ACMPQ, goasm.REG_AMD64_DX, rv8StgB)  // cmp (target-vaddrBegin), segSize
	missJmp2 := lc.c.NewProg()
	missJmp2.As = x86.AJCC // JAE (unsigned >=) → out of bounds
	missJmp2.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp2)

	// Compute byte offset: ((target - vaddrBegin) >> 1) << 3 = * 4.
	// DX = target - vaddrBegin.
	lc.emitRI(x86.ASHLQ, 2, goasm.REG_AMD64_DX) // DX = (target-vaddrBegin)*4
	// Mask: DX &= dcMask.
	lc.emitRM(x86.AMOVQ, rv8StgA, 96, rv8StgB) // RCX = dcMask
	lc.emit2(x86.AANDQ, rv8StgB, goasm.REG_AMD64_DX)

	// Load entry: DX = *(dcBase + DX).
	lc.emitRM(x86.AMOVQ, rv8StgA, 88, rv8StgB) // RCX = dcBase
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = rv8StgB
	p.From.Index = goasm.REG_AMD64_DX
	p.From.Scale = 1
	p.To.Type = obj.TYPE_REG
	p.To.Reg = goasm.REG_AMD64_DX
	lc.c.Append(p)

	// Check entry != 0.
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
	missJmp3 := lc.c.NewProg()
	missJmp3.As = x86.AJEQ
	missJmp3.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp3)

	// HIT: chain exit to cached chainEntry.
	// RAX = sret, DX = chainEntry. Dealloc frame and JMP.
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = goasm.REG_AMD64_DX
	lc.c.Append(jp)

	// MISS: return to Go with jitOKJalrMiss.
	missNop := lc.c.NewProg()
	missNop.As = obj.ANOP
	lc.c.Append(missNop)
	missJmp.To.SetTarget(missNop)
	missJmp2.To.SetTarget(missNop)
	missJmp3.To.SetTarget(missNop)

	lc.emitMI(x86.AMOVQ, int64(JitOKJalrMiss), rv8StgA, 16)
	lc.emitMI(x86.AMOVQ, int64(ins.Imm), rv8StgA, 24)
	lc.emitEpilogue()
}

// ── FP ops ──

func (lc *lowerCtxRV8) rv8FPBinop(ins *IRInstr, f64op, f32op obj.As) {
	a := lc.stageFP(ins.A, 0)
	b := lc.stageFP(ins.B, 1)
	op := f64op
	movOp := x86.AMOVSD
	if ins.T == F32 {
		op = f32op
		movOp = x86.AMOVSS
	}
	lc.emit2(op, b, a)
	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(movOp, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8FPUnary(ins *IRInstr, f64op, f32op obj.As) {
	a := lc.stageFP(ins.A, 0)
	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	dst := lc.writeDstFP(ins.Dst)
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8FNeg(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	lc.emit2(x86.AMOVQ, a, rv8StgA)
	var mask int64
	if ins.T == F32 {
		mask = 1 << 31
	} else {
		mask = -1 << 63
	}
	lc.loadImm(mask, rv8StgB)
	lc.emit2(x86.AXORQ, rv8StgB, rv8StgA)
	lc.emit2(x86.AMOVQ, rv8StgA, a)
	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVSD, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8FAbs(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	lc.emit2(x86.AMOVQ, a, rv8StgA)
	var mask int64
	if ins.T == F32 {
		mask = 0x7FFFFFFF
	} else {
		mask = 0x7FFFFFFFFFFFFFFF
	}
	lc.loadImm(mask, rv8StgB)
	lc.emit2(x86.AANDQ, rv8StgB, rv8StgA)
	lc.emit2(x86.AMOVQ, rv8StgA, a)
	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVSD, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerCtxRV8) rv8FCmp(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	b := lc.stageFP(ins.B, 1)
	cmpOp := x86.AUCOMISD
	if ins.T == F32 {
		cmpOp = x86.AUCOMISS
	}
	lc.emit2(cmpOp, b, a)

	dst := lc.writeDst(ins.Dst)
	bReg := byteReg(dst)

	switch ins.Pred {
	case EQ:
		p1 := lc.c.NewProg()
		p1.As = x86.ASETEQ
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(rv8StgB)
		if dst == rv8StgB {
			scrByte = byteReg(rv8StgA)
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
		scrByte := byteReg(rv8StgB)
		if dst == rv8StgB {
			scrByte = byteReg(rv8StgA)
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

func (lc *lowerCtxRV8) rv8FCvtToI(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
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

func (lc *lowerCtxRV8) rv8FCvtFromI(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0)
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

func (lc *lowerCtxRV8) rv8FCvtFF(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	dst := lc.writeDstFP(ins.Dst)
	op := x86.ACVTSS2SD
	if ins.U == F64 {
		op = x86.ACVTSD2SS
	}
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}
