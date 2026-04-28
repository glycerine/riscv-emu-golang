package riscv

// lower_amd64_abjit.go — abjit AMD64 lowerer.
//
// Emits x86-64 code for the abjit trampoline calling convention:
//   callJIT(code, regFileBase uintptr)
//   RBP = regFileBase (set by trampoline)
//   Results written to State at [RBP+536..560]
//
// Per-op lowering is in lower_amd64_ops.go (lowerOps).
// This file contains abjit-specific code: prologue, exit thunk,
// chain exits, JALR IC, syscall (cold path), ret, retDyn.

import (
	"fmt"
	"sort"

	"riscv/abjit"
	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// State field offsets relative to RBP (must match abjit.State layout).
const (
	abjitMemBaseOff    = 520
	abjitMemMaskOff    = 528
	abjitPCOff         = 536
	abjitStatusOff     = 544
	abjitFaultAddrOff  = 552
	abjitDCBaseOff     = 560
	abjitDCMaskOff     = 568
	abjitVAddrBeginOff = 576
	abjitSegSizeOff    = 584
	abjitCyclesOff     = 592
)

type lowerCtxABJIT struct {
	lowerOps
	exitThunk      *obj.Prog
	gocallResumes  []GocallResumeDesc
}

// LowerAMD64_ABJIT converts a register-allocated IR Block into x86-64
// machine code compatible with the abjit trampoline.
func LowerAMD64_ABJIT(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	if alloc == nil {
		return nil, fmt.Errorf("ir.LowerAMD64_ABJIT: nil allocation")
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

	lc := &lowerCtxABJIT{
		lowerOps: lowerOps{
			blk:        b,
			alloc:      alloc,
			c:          ctx,
			rIdx:       rIdx,
			fpSet:      fpSet,
			cxLive:     cxLive,
			labelProg:  make(map[Label]*obj.Prog),
			pending:    make(map[Label][]*obj.Prog),
			stackSlots: alloc.StackSlots,
		},
	}

	// Frame layout matches rv8 (shared opsDiv/opsMulHigh use sretOffset+8
	// and sretOffset+16 as scratch slots):
	//   [RSP+0 .. spillSlots*8-1]   = spill slots
	//   [RSP+spillSlots*8]          = unused (sret slot in rv8)
	//   [RSP+spillSlots*8+8]        = scratch A (DIV/MUL RDX save)
	//   [RSP+spillSlots*8+16]       = scratch B (retDyn PC save)
	//   Total = spillSlots*8 + 24.
	lc.sretOffset = int64(lc.stackSlots) * 8
	lc.frameSize = lc.sretOffset + 24
	// Trampoline leaves RSP ≡ 8 (mod 16). SUB frameSize must yield
	// RSP ≡ 0 (mod 16) so that CALLs in abjitSyscall/abjitCall
	// satisfy the SysV ABI 16-byte alignment requirement.
	if lc.frameSize%16 == 0 {
		lc.frameSize += 8
	}

	// Pre-create exit thunk NOP so forward JMP references work.
	lc.exitThunk = lc.c.NewProg()
	lc.exitThunk.As = obj.ANOP

	lc.emitPrologue()

	for idx := range b.Instrs {
		lc.idx = idx
		if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
			return nil, err
		}
	}

	if len(lc.pending) > 0 {
		return nil, fmt.Errorf("ir.LowerAMD64_ABJIT: %d unresolved labels", len(lc.pending))
	}

	for i := range lc.chainExits {
		lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
	}

	lc.emitExitThunk()

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
	result.GocallResumes = lc.gocallResumes
	return result, nil
}

// ── Prologue ──

func (lc *lowerCtxABJIT) emitPrologue() {
	// Chain entry point (also first entry — identical in abjit).
	// RBP already = regFileBase (set by trampoline or preserved
	// from previous chained block).
	lc.chainEntryProg = lc.c.NewProg()
	lc.chainEntryProg.As = obj.ANOP
	lc.c.Append(lc.chainEntryProg)

	// Allocate lowerer's spill frame.
	lc.emitRI(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)

	// Load allocated RISC-V integer registers from register file.
	for vr := VReg(1); vr < 32; vr++ {
		if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
			host := lc.rIdx.lookup(vr, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, int64(vr)*8, host)
			}
		}
	}

	// Load allocated FP registers.
	for vr := VReg(32); vr < 64; vr++ {
		if int(vr) < len(lc.alloc.Kind) && lc.alloc.Kind[vr] == AllocReg {
			host := lc.rIdx.lookup(vr, 0)
			if host >= 0 {
				off := int64(fpRegOffset) + int64(vr-32)*8
				lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_BP, off, host)
			}
		}
	}

	// Load memBase/memMask from State (AFTER regs, so they win on
	// host register conflicts).
	if int(VRMemBase) < len(lc.alloc.Kind) {
		switch lc.alloc.Kind[VRMemBase] {
		case AllocReg:
			host := lc.rIdx.lookup(VRMemBase, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemBaseOff, host)
			}
		case AllocStack:
			lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemBaseOff, stgA)
			lc.storeSpill(stgA, lc.alloc.SpillSlot[VRMemBase])
		}
	}
	if int(VRMemMask) < len(lc.alloc.Kind) {
		switch lc.alloc.Kind[VRMemMask] {
		case AllocReg:
			host := lc.rIdx.lookup(VRMemMask, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemMaskOff, host)
			}
		case AllocStack:
			lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitMemMaskOff, stgA)
			lc.storeSpill(stgA, lc.alloc.SpillSlot[VRMemMask])
		}
	}
}

// ── Exit thunk ──

func (lc *lowerCtxABJIT) emitExitThunk() {
	// Append pre-created NOP (forward references already point here).
	lc.c.Append(lc.exitThunk)

	// Restore callee-saves from trampoline frame.
	// RSP is at trampoline level (caller already deallocated spill frame).
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 8, goasm.REG_AMD64_BX)
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 24, goasm.REG_AMD64_R12)
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 32, goasm.REG_AMD64_R13)
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 40, goasm.REG_AMD64_R15)

	// Undo Go prologue: ADD RSP, 0xFFF8.
	lc.emitRI(x86.AADDQ, 0xFFF8, goasm.REG_AMD64_SP)

	// POP RBP (restore caller's frame pointer).
	// Equivalent to: MOV (SP), BP; ADD $8, SP
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, 0, goasm.REG_AMD64_BP)
	lc.emitRI(x86.AADDQ, 8, goasm.REG_AMD64_SP)

	ret := lc.c.NewProg()
	ret.As = obj.ARET
	lc.c.Append(ret)
}

// jmpExitThunk emits ADD RSP, frameSize; JMP exitThunk.
func (lc *lowerCtxABJIT) jmpExitThunk() {
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_BRANCH
	jp.To.SetTarget(lc.exitThunk)
	lc.c.Append(jp)
}

// ── Instruction dispatch ──

func (lc *lowerCtxABJIT) lowerInstr(ins *IRInstr) error {
	if handled, err := lc.lowerOps.lowerInstrCommon(ins); handled || err != nil {
		return err
	}
	switch ins.Op {
	case IRRet:
		lc.abjitRet(ins)
	case IRRetDyn:
		lc.abjitRetDyn(ins)
	case IRChainExit:
		lc.abjitChainExit(ins)
	case IRJalrIC:
		lc.abjitJalrIC(ins)
	case IRCall:
		return lc.abjitCall(ins)
	case IRSyscall:
		lc.abjitSyscall(ins)
	case IRSetPC:
		lc.abjitSetPC(ins)
	case IRRetBudget:
		lc.abjitRetBudget()
	default:
		return fmt.Errorf("ir.LowerAMD64_ABJIT: unhandled op %v at index %d",
			ins.Op, lc.idx)
	}
	return nil
}

// ── Ret ──

func (lc *lowerCtxABJIT) abjitRet(ins *IRInstr) {

	lc.storeRegsBack()

	lc.loadImm(ins.Imm, stgB)
	lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)

	lc.emitMI(x86.AMOVQ, ins.Imm2, goasm.REG_AMD64_BP, abjitStatusOff)

	if ins.A != VRegZero {
		fa := lc.stageInt(ins.A, 1)
		lc.emitMR(x86.AMOVQ, fa, goasm.REG_AMD64_BP, abjitFaultAddrOff)
	} else {
		lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)
	}

	lc.jmpExitThunk()
}

// abjitSetPC emits MOV QWORD [RBP+PC_offset], $imm.
// Used by budget cold paths to write the unexecuted instruction's PC.
func (lc *lowerCtxABJIT) abjitSetPC(ins *IRInstr) {
	lc.emitMI(x86.AMOVQ, ins.Imm, goasm.REG_AMD64_BP, abjitPCOff)
}

// abjitRetBudget emits the shared budget exit: status=0, exitinfo=0,
// restore SP, jump to exit thunk. PC is already in State (set by SetPC).
func (lc *lowerCtxABJIT) abjitRetBudget() {
	lc.storeRegsBack()
	lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitStatusOff)
	lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)
	lc.jmpExitThunk()
}

func (lc *lowerCtxABJIT) abjitRetDyn(ins *IRInstr) {
	var pcStaged bool
	if ins.A != VRegZero {
		hr := lc.hostReg(ins.A)
		if hr >= 0 {
			lc.emitMR(x86.AMOVQ, hr, goasm.REG_AMD64_SP, lc.sretOffset+16)
			pcStaged = true
		}
	}


	lc.storeRegsBack()

	if pcStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+16, stgB)
		lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)
	} else if ins.A != VRegZero {
		pcReg := lc.stageInt(ins.A, 1)
		lc.emitMR(x86.AMOVQ, pcReg, goasm.REG_AMD64_BP, abjitPCOff)
	} else {
		lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitPCOff)
	}

	lc.emitMI(x86.AMOVQ, ins.Imm, goasm.REG_AMD64_BP, abjitStatusOff)

	if ins.B != VRegZero {
		fa := lc.stageInt(ins.B, 1)
		lc.emitMR(x86.AMOVQ, fa, goasm.REG_AMD64_BP, abjitFaultAddrOff)
	} else {
		lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)
	}

	lc.jmpExitThunk()
}

// ── Chain exit ──

func (lc *lowerCtxABJIT) abjitChainExit(ins *IRInstr) {

	lc.storeRegsBack()

	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

	const sentinel = int64(0x7BADC0DE7BADC0DE)
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = sentinel
	p.To.Type = obj.TYPE_REG
	p.To.Reg = stgB
	lc.c.Append(p)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  p,
	})

	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = stgB
	lc.c.Append(jp)
}

func (lc *lowerCtxABJIT) emitSlowExitStub(targetPC uint64) *obj.Prog {
	first := lc.c.NewProg()
	first.As = obj.ANOP
	lc.c.Append(first)

	lc.loadImm(int64(targetPC), stgB)
	lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)

	lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitStatusOff)
	lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)

	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_BRANCH
	jp.To.SetTarget(lc.exitThunk)
	lc.c.Append(jp)

	return first
}

// ── Syscall (inline dispatch with cold fallback) ──

func (lc *lowerCtxABJIT) abjitSyscall(ins *IRInstr) {
	// No CTab entry → cold path only.
	if int(ins.Imm2) < 0 || int(ins.Imm2) >= len(lc.blk.CTab) {
		lc.abjitRet(&IRInstr{Op: IRRet, Imm: ins.Imm, Imm2: 1, A: VRegZero})
		return
	}
	sym := lc.blk.CTab[ins.Imm2]

	// Spill R15 (IC) to State before Go call (Go clobbers R15).
	lc.opsSpillIC()

	// Set up SysV args: RDI=xBase(RBP), RSI=memBase, RDX=memMask.
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_BP, goasm.REG_AMD64_DI)

	memBaseHost := lc.hostReg(VRMemBase)
	if memBaseHost >= 0 {
		lc.emit2(x86.AMOVQ, memBaseHost, goasm.REG_AMD64_SI)
	} else if int(VRMemBase) < len(lc.alloc.Kind) && lc.alloc.Kind[VRMemBase] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[VRMemBase], goasm.REG_AMD64_SI)
	} else {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, int64(abjitMemBaseOff), goasm.REG_AMD64_SI)
	}

	memMaskHost := lc.hostReg(VRMemMask)
	if memMaskHost >= 0 {
		lc.emit2(x86.AMOVQ, memMaskHost, goasm.REG_AMD64_DX)
	} else if int(VRMemMask) < len(lc.alloc.Kind) && lc.alloc.Kind[VRMemMask] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[VRMemMask], goasm.REG_AMD64_DX)
	} else {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, int64(abjitMemMaskOff), goasm.REG_AMD64_DX)
	}

	// Gocall sequence: deallocate JIT spill frame so RSP is at the
	// trampoline level, store resume address at [RSP+0] (trampoline
	// slot 0), load func into R10, JMP to gocall. The trampoline does
	// CALL R10 (return PC is valid Go code) then JMP (RSP) back to
	// resume. At resume, re-allocate the spill frame.
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

	const gocallSentinel = int64(0x7CAFFE7CAFFE7CAF)
	addrMov := lc.c.NewProg()
	addrMov.As = x86.AMOVQ
	addrMov.From.Type = obj.TYPE_CONST
	addrMov.From.Offset = gocallSentinel
	addrMov.To.Type = obj.TYPE_REG
	addrMov.To.Reg = stgA
	lc.c.Append(addrMov)
	lc.emitMR(x86.AMOVQ, stgA, goasm.REG_AMD64_SP, 0)

	lc.loadImm(int64(sym.Addr), goasm.REG_AMD64_R10)
	lc.loadImm(int64(abjit.GocallAddr()), stgA)
	jp0 := lc.c.NewProg()
	jp0.As = obj.AJMP
	jp0.To.Type = obj.TYPE_REG
	jp0.To.Reg = stgA
	lc.c.Append(jp0)

	// Resume: trampoline's JMP (SP) lands here. Re-allocate spill frame.
	resumeNop := lc.c.NewProg()
	resumeNop.As = obj.ANOP
	lc.c.Append(resumeNop)
	lc.emitRI(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)

	lc.gocallResumes = append(lc.gocallResumes, GocallResumeDesc{
		AddrMov:    addrMov,
		ResumeProg: resumeNop,
	})

	// Restore R15 (IC) from State after Go call.
	lc.opsLoadIC()

	// RAX = 0 → handled (hot path), non-zero → fallback (cold path).
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_AX, goasm.REG_AMD64_AX)
	slowJmp := lc.c.NewProg()
	slowJmp.As = x86.AJNE
	slowJmp.To.Type = obj.TYPE_BRANCH
	lc.c.Append(slowJmp)

	// ── Hot path: chain exit to post-ECALL block ──
	// Guest registers were already written back by IRWriteback ops
	// emitted before IRSyscall. No storeRegsBack needed.
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

	const sentinel = int64(0x7BADC0DE7BADC0DE)
	movProg := lc.c.NewProg()
	movProg.As = x86.AMOVQ
	movProg.From.Type = obj.TYPE_CONST
	movProg.From.Offset = sentinel
	movProg.To.Type = obj.TYPE_REG
	movProg.To.Reg = stgB
	lc.c.Append(movProg)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  movProg,
	})

	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = stgB
	lc.c.Append(jp)

	// ── Cold path: return to Go with jitEcall ──
	slowNop := lc.c.NewProg()
	slowNop.As = obj.ANOP
	lc.c.Append(slowNop)
	slowJmp.To.SetTarget(slowNop)

	lc.loadImm(ins.Imm, stgB)
	lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)
	lc.emitMI(x86.AMOVQ, 1, goasm.REG_AMD64_BP, abjitStatusOff)
	lc.emitMI(x86.AMOVQ, 0, goasm.REG_AMD64_BP, abjitFaultAddrOff)
	lc.jmpExitThunk()
}

// ── JALR IC (decoder-cache lookup; the old 2-slot IC is deprecated) ──

func (lc *lowerCtxABJIT) abjitJalrIC(ins *IRInstr) {
	// Save target PC to scratch before storeRegsBack clobbers registers.
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


	lc.storeRegsBack()

	// Load target into RCX.
	if ins.A != VRegZero {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, pcSaveOff, stgB) // RCX = target
	} else {
		lc.emit2(x86.AXORQ, stgB, stgB)
	}

	// Write State.PC = target (needed on both hit and miss).
	lc.emitMR(x86.AMOVQ, stgB, goasm.REG_AMD64_BP, abjitPCOff)

	// --- Decoder cache lookup (L1 cache) ---
	// Check dcBase != 0.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitDCBaseOff, goasm.REG_AMD64_DX) // DX = dcBase
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
	missJmp1 := lc.c.NewProg()
	missJmp1.As = x86.AJEQ
	missJmp1.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp1)

	// Bounds check: (target - vaddrBegin) < segSize (unsigned).
	lc.emit2(x86.AMOVQ, stgB, goasm.REG_AMD64_DX)                                  // DX = target
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitVAddrBeginOff, stgA)              // RAX = vaddrBegin
	lc.emit2(x86.ASUBQ, stgA, goasm.REG_AMD64_DX)                                   // DX = target - vaddrBegin
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitSegSizeOff, stgA)                 // RAX = segSize
	lc.emit2(x86.ACMPQ, goasm.REG_AMD64_DX, stgA)                                   // cmp offset, segSize
	missJmp2 := lc.c.NewProg()
	missJmp2.As = x86.AJCC // JAE unsigned
	missJmp2.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp2)

	// Index: ((target - vaddrBegin) * 4) & dcMask.
	lc.emitRI(x86.ASHLQ, 2, goasm.REG_AMD64_DX)
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitDCMaskOff, stgA) // RAX = dcMask
	lc.emit2(x86.AANDQ, stgA, goasm.REG_AMD64_DX)

	// Load entry: DX = *(dcBase + DX).
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_BP, abjitDCBaseOff, stgA) // RAX = dcBase
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = stgA
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

	// HIT: dealloc frame, jump to cached chainEntry.
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = goasm.REG_AMD64_DX
	lc.c.Append(jp)

	// --- 2-slot inline cache (fallback for lazy blocks where dcBase=0) ---
	// All decoder-cache miss paths land here. stgB (RCX) still holds
	// the target PC.
	try2SlotNop := lc.c.NewProg()
	try2SlotNop.As = obj.ANOP
	lc.c.Append(try2SlotNop)
	missJmp1.To.SetTarget(try2SlotNop)
	missJmp2.To.SetTarget(try2SlotNop)
	missJmp3.To.SetTarget(try2SlotNop)

	const sentinel = int64(0x7BADC0DE7BADC0DE)

	// Slot 0: MOVABS RAX, <cache_pc0>; CMP target, RAX; JEQ hit0
	pcMov0 := lc.c.NewProg()
	pcMov0.As = x86.AMOVQ
	pcMov0.From.Type = obj.TYPE_CONST
	pcMov0.From.Offset = sentinel
	pcMov0.To.Type = obj.TYPE_REG
	pcMov0.To.Reg = stgA
	lc.c.Append(pcMov0)

	lc.emit2(x86.ACMPQ, stgB, stgA)
	jeq0 := lc.c.NewProg()
	jeq0.As = x86.AJEQ
	jeq0.To.Type = obj.TYPE_BRANCH
	lc.c.Append(jeq0)

	// Slot 1: MOVABS RAX, <cache_pc1>; CMP target, RAX; JNE miss
	pcMov1 := lc.c.NewProg()
	pcMov1.As = x86.AMOVQ
	pcMov1.From.Type = obj.TYPE_CONST
	pcMov1.From.Offset = sentinel
	pcMov1.To.Type = obj.TYPE_REG
	pcMov1.To.Reg = stgA
	lc.c.Append(pcMov1)

	lc.emit2(x86.ACMPQ, stgB, stgA)
	jne1 := lc.c.NewProg()
	jne1.As = x86.AJNE
	jne1.To.Type = obj.TYPE_BRANCH
	lc.c.Append(jne1)

	// Hit slot 1: dealloc frame, MOVABS RAX = cache_fn1, JMP RAX
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	fnMov1 := lc.c.NewProg()
	fnMov1.As = x86.AMOVQ
	fnMov1.From.Type = obj.TYPE_CONST
	fnMov1.From.Offset = sentinel
	fnMov1.To.Type = obj.TYPE_REG
	fnMov1.To.Reg = stgA
	lc.c.Append(fnMov1)
	jmpHit1 := lc.c.NewProg()
	jmpHit1.As = obj.AJMP
	jmpHit1.To.Type = obj.TYPE_REG
	jmpHit1.To.Reg = stgA
	lc.c.Append(jmpHit1)

	// Hit slot 0:
	hit0Nop := lc.c.NewProg()
	hit0Nop.As = obj.ANOP
	lc.c.Append(hit0Nop)
	jeq0.To.SetTarget(hit0Nop)

	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	fnMov0 := lc.c.NewProg()
	fnMov0.As = x86.AMOVQ
	fnMov0.From.Type = obj.TYPE_CONST
	fnMov0.From.Offset = sentinel
	fnMov0.To.Type = obj.TYPE_REG
	fnMov0.To.Reg = stgA
	lc.c.Append(fnMov0)
	jmpHit0 := lc.c.NewProg()
	jmpHit0.As = obj.AJMP
	jmpHit0.To.Type = obj.TYPE_REG
	jmpHit0.To.Reg = stgA
	lc.c.Append(jmpHit0)

	// MISS: both slots missed — return to Go.
	missNop := lc.c.NewProg()
	missNop.As = obj.ANOP
	lc.c.Append(missNop)
	jne1.To.SetTarget(missNop)

	lc.jalrICs = append(lc.jalrICs, jalrICInfo{
		siteIdx:  int(ins.Imm),
		pcMov:    [2]*obj.Prog{pcMov0, pcMov1},
		fnMov:    [2]*obj.Prog{fnMov0, fnMov1},
		stubProg: missNop,
	})

	lc.emitMI(x86.AMOVQ, int64(JitOKJalrMiss), goasm.REG_AMD64_BP, abjitStatusOff)
	lc.emitMI(x86.AMOVQ, int64(ins.Imm), goasm.REG_AMD64_BP, abjitFaultAddrOff)
	lc.jmpExitThunk()
}

// ── Call (not supported in Phase 3) ──

func (lc *lowerCtxABJIT) abjitCall(ins *IRInstr) error {
	if int(ins.Imm) >= len(lc.blk.CTab) {
		return fmt.Errorf("ir.abjitCall: CTab index %d out of range (len=%d)", ins.Imm, len(lc.blk.CTab))
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
	if saveSize%16 != 0 {
		saveSize += 8
	}
	if saveSize > 0 {
		lc.emitRI(x86.ASUBQ, saveSize, goasm.REG_AMD64_SP)
	}
	for i, r := range liveInt {
		lc.emitMR(x86.AMOVQ, r, goasm.REG_AMD64_SP, int64(i)*8)
	}
	for i, r := range liveFP {
		lc.emitMR(x86.AMOVSD, r, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8)
	}

	// Spill R15 (IC) before Go call.
	lc.opsSpillIC()

	// Gocall: store resume address at [SP+saveSize] (which is the
	// trampoline's [SP+0] since we SUB'd saveSize from SP above).
	// Actually, the trampoline frame's [SP+0] is at the ORIGINAL SP
	// before we pushed saves. We need to write to [RSP + saveSize].
	const gocallSentinel = int64(0x7CAFFE7CAFFE7CAF)
	addrMov := lc.c.NewProg()
	addrMov.As = x86.AMOVQ
	addrMov.From.Type = obj.TYPE_CONST
	addrMov.From.Offset = gocallSentinel
	addrMov.To.Type = obj.TYPE_REG
	addrMov.To.Reg = stgA
	lc.c.Append(addrMov)
	lc.emitMR(x86.AMOVQ, stgA, goasm.REG_AMD64_SP, saveSize)

	lc.loadImm(int64(sym.Addr), goasm.REG_AMD64_R10)
	lc.loadImm(int64(abjit.GocallAddr()), stgA)
	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = stgA
	lc.c.Append(jp)

	resumeNop := lc.c.NewProg()
	resumeNop.As = obj.ANOP
	lc.c.Append(resumeNop)

	lc.gocallResumes = append(lc.gocallResumes, GocallResumeDesc{
		AddrMov:    addrMov,
		ResumeProg: resumeNop,
	})

	// Restore R15 (IC) after Go call.
	lc.opsLoadIC()

	for i, r := range liveInt {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(i)*8, r)
	}
	for i, r := range liveFP {
		lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8, r)
	}
	if saveSize > 0 {
		lc.emitRI(x86.AADDQ, saveSize, goasm.REG_AMD64_SP)
	}
	return nil
}
