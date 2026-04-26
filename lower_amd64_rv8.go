package riscv

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
//
// Per-op lowering is in lower_amd64_ops.go (lowerOps).
// This file contains rv8-specific code: prologue, epilogue, chain
// exits, JALR IC, syscall, ret, retDyn, call.

import (
	"fmt"
	"sort"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

type lowerCtxRV8 struct {
	lowerOps
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

	// Frame layout:
	//   [RSP+0 .. spillSlots*8-1] = spill slots
	//   [RSP+spillSlots*8]        = sret pointer (8 bytes)
	//   [RSP+spillSlots*8+8]      = scratch A (8 bytes, for DIV/MUL RDX save, ret IC save)
	//   [RSP+spillSlots*8+16]     = scratch B (8 bytes, for retDyn PC save)
	// Total = spillSlots*8 + 24.
	lc.sretOffset = int64(lc.stackSlots) * 8
	lc.frameSize = lc.sretOffset + 24
	// Sandbox trampoline leaves RSP ≡ 8 (mod 16). SUB frameSize must
	// yield RSP ≡ 0 (mod 16) so inline CALLs (syscall dispatch)
	// satisfy the SysV ABI 16-byte alignment requirement.
	if lc.frameSize%16 == 0 {
		lc.frameSize += 8
	}

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


	// Copy sret from RDI to RAX so first-entry and chain-entry share
	// the same code path below (chain entry arrives with RAX=sret).
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_DI, stgA)

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
	lc.emitMR(x86.AMOVQ, stgA, goasm.REG_AMD64_SP, lc.sretOffset)

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
				off := int64(fpRegOffset) + int64(vr-32)*8
				lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_BP, off, host)
			}
		}
	}

	// Initialize parameter VRegs that can't be resolved statically.
	// VRXBase/VRFBase/VRRegFile are handled in stageInt (always RBP-based).
	// VRMemBase, VRMemMask need explicit initialization here.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgB) // RCX = sret

	// Load VRMemBase/VRMemMask from sret buffer AFTER RISC-V regs,
	// because a RISC-V reg may share the same host register and we
	// need memBase/memMask to win.
	if int(VRMemBase) < len(lc.alloc.Kind) {
		switch lc.alloc.Kind[VRMemBase] {
		case AllocReg:
			host := lc.rIdx.lookup(VRMemBase, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, stgB, 128, host)
			}
		case AllocStack:
			lc.emitRM(x86.AMOVQ, stgB, 128, stgA)
			lc.storeSpill(stgA, lc.alloc.SpillSlot[VRMemBase])
		}
	}
	if int(VRMemMask) < len(lc.alloc.Kind) {
		switch lc.alloc.Kind[VRMemMask] {
		case AllocReg:
			host := lc.rIdx.lookup(VRMemMask, 0)
			if host >= 0 {
				lc.emitRM(x86.AMOVQ, stgB, 136, host)
			}
		case AllocStack:
			lc.emitRM(x86.AMOVQ, stgB, 136, stgA)
			lc.storeSpill(stgA, lc.alloc.SpillSlot[VRMemMask])
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
	if handled, err := lc.lowerOps.lowerInstrCommon(ins); handled || err != nil {
		return err
	}
	switch ins.Op {
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
	default:
		return fmt.Errorf("ir.LowerAMD64_RV8: unhandled op %v at index %d", ins.Op, lc.idx)
	}
	return nil
}

// ── rv8-specific ops ──

func (lc *lowerCtxRV8) rv8Ret(ins *IRInstr) {
	lc.storeRegsBack()

	// Load sret pointer from stack.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA) // RAX = sret

	// Write Result.PC
	lc.loadImm(ins.Imm, stgB)
	lc.emitMR(x86.AMOVQ, stgB, stgA, 0)

	// Write Result.Status
	lc.emitMI(x86.AMOVQ, ins.Imm2, stgA, 8)

	// Write Result.FaultAddr
	if ins.A != VRegZero {
		fa := lc.stageInt(ins.A, 1) // RCX
		lc.emitMR(x86.AMOVQ, fa, stgA, 16)
	} else {
		lc.emitMI(x86.AMOVQ, 0, stgA, 16)
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

	lc.storeRegsBack()

	// Load sret.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA)

	// Result.PC
	if pcStaged {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, pcSaveOff, stgB)
		lc.emitMR(x86.AMOVQ, stgB, stgA, 0)
	} else if ins.A != VRegZero {
		pcReg := lc.stageInt(ins.A, 1)
		lc.emitMR(x86.AMOVQ, pcReg, stgA, 0)
	} else {
		lc.emitMI(x86.AMOVQ, 0, stgA, 0)
	}

	// Result.Status
	lc.emitMI(x86.AMOVQ, ins.Imm, stgA, 8)

	// Result.FaultAddr
	if ins.B != VRegZero {
		fa := lc.stageInt(ins.B, 1)
		lc.emitMR(x86.AMOVQ, fa, stgA, 16)
	} else {
		lc.emitMI(x86.AMOVQ, 0, stgA, 16)
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
// reload allocated regs from [RBP+r*8].

func (lc *lowerCtxRV8) rv8ChainExit(ins *IRInstr) {
	lc.storeRegsBack()

	// Load sret from stack into RAX — carry it to the next block.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA)

	// Deallocate frame.
	lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)

	// MOVABS RCX, <sentinel> — 10-byte encoding for backpatching.
	const sentinel = int64(0x7BADC0DE7BADC0DE)
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = sentinel
	p.To.Type = obj.TYPE_REG
	p.To.Reg = stgB // RCX
	lc.c.Append(p)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  p,
	})

	// JMP RCX
	jp := lc.c.NewProg()
	jp.As = obj.AJMP
	jp.To.Type = obj.TYPE_REG
	jp.To.Reg = stgB
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
	// by the chain exit before it deallocated the frame.

	// Result.PC
	lc.loadImm(int64(targetPC), stgB)
	lc.emitMR(x86.AMOVQ, stgB, stgA, 0)

	// Result.Status = 0
	lc.emitMI(x86.AMOVQ, 0, stgA, 8)

	// Result.FaultAddr = 0
	lc.emitMI(x86.AMOVQ, 0, stgA, 16)

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

	lc.loadImm(int64(sym.Addr), stgA)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = stgA
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

	// Set up SysV args: RDI=xBase(RBP), RSI=memBase, RDX=memMask.
	lc.emit2(x86.AMOVQ, goasm.REG_AMD64_BP, goasm.REG_AMD64_DI)

	memBaseHost := lc.hostReg(VRMemBase)
	if memBaseHost >= 0 {
		lc.emit2(x86.AMOVQ, memBaseHost, goasm.REG_AMD64_SI)
	} else if int(VRMemBase) < len(lc.alloc.Kind) && lc.alloc.Kind[VRMemBase] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[VRMemBase], goasm.REG_AMD64_SI)
	} else {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA)
		lc.emitRM(x86.AMOVQ, stgA, 128, goasm.REG_AMD64_SI)
	}

	memMaskHost := lc.hostReg(VRMemMask)
	if memMaskHost >= 0 {
		lc.emit2(x86.AMOVQ, memMaskHost, goasm.REG_AMD64_DX)
	} else if int(VRMemMask) < len(lc.alloc.Kind) && lc.alloc.Kind[VRMemMask] == AllocStack {
		lc.loadSpill(lc.alloc.SpillSlot[VRMemMask], goasm.REG_AMD64_DX)
	} else {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA)
		lc.emitRM(x86.AMOVQ, stgA, 136, goasm.REG_AMD64_DX)
	}

	lc.loadImm(int64(sym.Addr), stgA)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = stgA
	lc.c.Append(p)

	// RAX = dispatcher return: 0=jitOK, non-zero=jitEcall.
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_AX, goasm.REG_AMD64_AX)
	slowPath := lc.c.NewProg()
	slowPath.As = x86.AJNE
	slowPath.To.Type = obj.TYPE_BRANCH
	lc.c.Append(slowPath)

	// Fast path (status=0): chain exit.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA) // RAX = sret
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

	// Slow path (status!=0): return with jitEcall.
	slowNop := lc.c.NewProg()
	slowNop.As = obj.ANOP
	lc.c.Append(slowNop)
	slowPath.To.SetTarget(slowNop)

	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA)
	lc.loadImm(ins.Imm, stgB)
	lc.emitMR(x86.AMOVQ, stgB, stgA, 0) // Result.PC = resumePC
	lc.emitMI(x86.AMOVQ, 1, stgA, 8)    // Result.Status = jitEcall
	lc.emitMI(x86.AMOVQ, 0, stgA, 16)   // Result.FaultAddr = 0
	lc.emitEpilogue()
}

// ── JALR inline cache (decoder_cache lookup) ──
//
// IRJalrIC: target PC in VReg A, site index in Imm.
// Performs an inline decoder_cache lookup (the old 2-slot IC is
// deprecated) using params published in the sret buffer by CallAOT:
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

	lc.storeRegsBack()

	// Load sret and target PC.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset, stgA) // RAX = sret
	if ins.A != VRegZero {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, pcSaveOff, stgB) // RCX = target
	} else {
		lc.emit2(x86.AXORQ, stgB, stgB)
	}

	// Write Result.PC = target (needed on both hit and miss paths).
	lc.emitMR(x86.AMOVQ, stgB, stgA, 0)

	// Check if decoder_cache is available (dcBase != 0).
	lc.emitRM(x86.AMOVQ, stgA, 88, goasm.REG_AMD64_DX) // DX = dcBase
	lc.emit2(x86.ATESTQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
	missJmp := lc.c.NewProg()
	missJmp.As = x86.AJEQ
	missJmp.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp)

	// Bounds check: (target - vaddrBegin) < segSize (unsigned).
	// RCX = target, RAX = sret, DX = dcBase (will reload as needed).
	lc.emit2(x86.AMOVQ, stgB, goasm.REG_AMD64_DX) // DX = target (copy)
	lc.emitRM(x86.AMOVQ, stgA, 104, stgB)         // RCX = vaddrBegin
	lc.emit2(x86.ASUBQ, stgB, goasm.REG_AMD64_DX) // DX = target - vaddrBegin
	lc.emitRM(x86.AMOVQ, stgA, 112, stgB)         // RCX = segSize
	lc.emit2(x86.ACMPQ, goasm.REG_AMD64_DX, stgB) // cmp (target-vaddrBegin), segSize
	missJmp2 := lc.c.NewProg()
	missJmp2.As = x86.AJCC // JAE (unsigned >=) → out of bounds
	missJmp2.To.Type = obj.TYPE_BRANCH
	lc.c.Append(missJmp2)

	// Compute byte offset: ((target - vaddrBegin) >> 1) << 3 = * 4.
	// DX = target - vaddrBegin.
	lc.emitRI(x86.ASHLQ, 2, goasm.REG_AMD64_DX) // DX = (target-vaddrBegin)*4
	// Mask: DX &= dcMask.
	lc.emitRM(x86.AMOVQ, stgA, 96, stgB) // RCX = dcMask
	lc.emit2(x86.AANDQ, stgB, goasm.REG_AMD64_DX)

	// Load entry: DX = *(dcBase + DX).
	lc.emitRM(x86.AMOVQ, stgA, 88, stgB) // RCX = dcBase
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = stgB
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

	lc.emitMI(x86.AMOVQ, int64(JitOKJalrMiss), stgA, 8)
	lc.emitMI(x86.AMOVQ, int64(ins.Imm), stgA, 16)
	lc.emitEpilogue()
}

// Ensure imports are used.
var _ = sort.Sort
