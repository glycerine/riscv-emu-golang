package riscv

// lower_amd64_ops.go — Shared per-op AMD64 lowering.
//
// Contains the lowerOps struct and all instruction-lowering methods
// that are identical between rv8 and abjit lowerers. Backend-specific
// ops (ret, retDyn, chainExit, jalrIC, call, syscall) are NOT here;
// each lowerer handles those in its own file.

import (
	"fmt"
	"sort"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// Staging register constants (shared across lowerers).
const (
	stgA  int16 = goasm.REG_AMD64_AX  // integer staging slot A
	stgB  int16 = goasm.REG_AMD64_CX  // integer staging slot B
	stgFA int16 = goasm.REG_AMD64_X15 // FP staging slot A
	stgFB int16 = goasm.REG_AMD64_X14 // FP staging slot B
)

// Register file offsets (relative to RBP).
const (
	intRegOffset = 0   // x[r] at [RBP + r*8]
	fpRegOffset  = 256 // f[r] at [RBP + 256 + r*8]
)

// Memory base/mask offsets in register file.
const (
	rfMemBaseOff = 520
	rfMemMaskOff = 528
)

// 6x faster on Intel. tiny bit slower on AMD. Clearly
// we need to set FAST=true to avoid catastrophic
// pipeline stalls on Intel.
// See the discussion in plans/PLAN076_conclude_microarchitecture_issue.md
const FAST = true

type lowerOps struct {
	blk        *Block
	alloc      *Allocation
	c          *goasm.Ctx
	idx        int
	rIdx       regIndex
	fpSet      map[VReg]bool
	cxLive     []regEntry
	labelProg  map[Label]*obj.Prog
	pending    map[Label][]*obj.Prog
	stackSlots int
	frameSize  int64
	sretOffset int64 // offset of sret pointer within frame (= stackSlots*8)
	chainEntryProg *obj.Prog
	chainExits     []chainExitInfo
	jalrICs        []jalrICInfo
}

// ── Emission helpers ──

func (lc *lowerOps) emit2(op obj.As, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerOps) emitRI(op obj.As, imm int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerOps) emitRM(op obj.As, base int16, off int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = off
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerOps) emitMR(op obj.As, src int16, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerOps) emitMI(op obj.As, imm int64, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

func (lc *lowerOps) emitUnary(op obj.As, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerOps) emitCmpRI(reg int16, imm int64) {
	p := lc.c.NewProg()
	p.As = x86.ACMPQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = reg
	p.To.Type = obj.TYPE_CONST
	p.To.Offset = imm
	lc.c.Append(p)
}

func (lc *lowerOps) loadImm(imm int64, dst int16) {
	if imm == 0 {
		lc.emit2(x86.AXORQ, dst, dst)
		return
	}
	lc.emitRI(x86.AMOVQ, imm, dst)
}

func (lc *lowerOps) loadSpill(slot int16, dst int16) {
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

func (lc *lowerOps) storeSpill(src int16, slot int16) {
	lc.emitMR(x86.AMOVQ, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

func (lc *lowerOps) loadFPSpill(slot int16, dst int16) {
	lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

func (lc *lowerOps) storeFPSpill(src int16, slot int16) {
	lc.emitMR(x86.AMOVSD, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// emitRMMovzx emits MOVZX dst, byte [base+off].
func (lc *lowerOps) emitRMMovzx(base int16, off int64, dst int16) {
	p := lc.c.NewProg()
	p.As = x86.AMOVBQZX
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = off
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

// emitMRByte emits MOV byte [base+off], src (8-bit store).
func (lc *lowerOps) emitMRByte(src, base int16, off int64) {
	p := lc.c.NewProg()
	p.As = x86.AMOVB
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = off
	lc.c.Append(p)
}

// ── Staging helpers ──

func (lc *lowerOps) stageInt(v VReg, idx int) int16 {
	stg := stgA
	if idx != 0 {
		stg = stgB
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
		p.From.Offset = int64(fpRegOffset)
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

func (lc *lowerOps) stageFP(v VReg, idx int) int16 {
	stg := stgFA
	if idx != 0 {
		stg = stgFB
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

func (lc *lowerOps) writeDst(v VReg) int16 {
	if v == VRegZero {
		return stgA
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		return hr
	}
	return stgA
}

func (lc *lowerOps) writeDstFP(v VReg) int16 {
	if v == VRegZero {
		return stgFA
	}
	hr := lc.hostReg(v)
	if hr >= 0 {
		return hr
	}
	return stgFA
}

func (lc *lowerOps) commitDst(v VReg, hostReg int16) {
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

// ── Register resolution ──

func (lc *lowerOps) hostReg(v VReg) int16 {
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
func (lc *lowerOps) directReg(v VReg) int16 {
	if v == VRegZero {
		return -1
	}
	switch v {
	case VRXBase, VRFBase, VRMemBase, VRMemMask, VRRegFile:
		return -1
	}
	hr := lc.hostReg(v)
	if hr >= 0 && isXMMReg(hr) {
		return -1
	}
	return hr
}

func (lc *lowerOps) isVRegFP(v VReg) bool {
	if v == VRegZero {
		return false
	}
	return lc.fpSet[v]
}

func (lc *lowerOps) isRegLive(hostReg int16) bool {
	for vr := 0; vr < len(lc.rIdx); vr++ {
		for _, e := range lc.rIdx[VReg(vr)] {
			if e.host == hostReg && e.start <= lc.idx && lc.idx <= e.end {
				return true
			}
		}
	}
	return false
}

// regFileOff returns the register-file offset relative to RBP for
// RISC-V integer (VReg 1-31) and FP (VReg 32-63) registers.
// Returns -1 for VRegZero, parameter VRegs, and temps.
func regFileOff(v VReg) int64 {
	if v >= 1 && v < 32 {
		return int64(v) * 8
	}
	if v >= 32 && v < 64 {
		return int64(fpRegOffset) + int64(v-32)*8
	}
	return -1
}

// spilledRegFileOff returns the register-file offset for a VReg that
// is a spilled RISC-V register (not allocated to a host register).
// Returns -1 if the VReg is in a host register, is a temp, or is VRegZero.
func (lc *lowerOps) spilledRegFileOff(v VReg) int64 {
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
func (lc *lowerOps) spilledMemOp(v VReg) (base int16, off int64, ok bool) {
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

// ── Register writeback ──

// storeRegsBack writes AllocReg RISC-V registers back to the register
// file at [RBP + vr*8]. AllocStack registers are NOT written back here;
// the allocator must ensure they were already flushed via IRWriteback
// instructions before any exit point that calls storeRegsBack.
func (lc *lowerOps) storeRegsBack() {
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
				off := int64(fpRegOffset) + int64(vr-32)*8
				lc.emitMR(x86.AMOVSD, host, goasm.REG_AMD64_BP, off)
			}
		}
	}
}

// ── Label management ──

func (lc *lowerOps) placeLabel(l Label) {
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

func (lc *lowerOps) bindLabel(l Label, branch *obj.Prog) {
	if target, ok := lc.labelProg[l]; ok {
		branch.To.SetTarget(target)
	} else {
		lc.pending[l] = append(lc.pending[l], branch)
	}
}

// ── Data movement ──

func (lc *lowerOps) opsMov(ins *IRInstr) {
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

func (lc *lowerOps) opsConst(ins *IRInstr) {
	dst := lc.writeDst(ins.Dst)
	lc.loadImm(ins.Imm, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerOps) opsSext(ins *IRInstr) {
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

func (lc *lowerOps) opsZext(ins *IRInstr) {
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

func (lc *lowerOps) opsBinop(ins *IRInstr, op obj.As) {
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

func (lc *lowerOps) opsBinopImm(ins *IRInstr, op obj.As) {
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
			lc.loadImm(imm, stgB)
			lc.emit2(op, stgB, dstHR)
		}
		lc.commitDst(ins.Dst, dstHR)
		return
	}

	if ins.Dst == ins.A {
		// this is the switch that really matters for performance.
		//vv("opsBinopImm: ins.Dst == ins.A") // seen about 155 times in "make bench"
		if FAST { // FAST means 3516 MIPS on the "make bench" benchmark.
			if off := lc.spilledRegFileOff(ins.Dst); off >= 0 { // FAST
				imm := ins.Imm
				if imm >= -(1<<31) && imm < (1<<31) {
					lc.emitMI(op, imm, goasm.REG_AMD64_BP, off) // FAST
					return
				}
			}
		} else {
			// !FAST, slow: 523.0 MIPS on the "make bench" benchmark.
			if base, off, ok := lc.spilledMemOp(ins.Dst); ok {
				imm := ins.Imm
				if imm >= -(1<<31) && imm < (1<<31) {
					lc.emitMI(op, imm, base, off)
					return
				}
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
				lc.loadImm(imm, stgB)
				lc.emit2(op, stgB, dstHR)
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
		lc.loadImm(imm, stgB)
		lc.emit2(op, stgB, a)
	}
	dst := lc.writeDst(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVQ, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerOps) opsUnary(ins *IRInstr, op obj.As) {
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
// CX is a staging reg, so it's always available for shifts — no save needed.

func (lc *lowerOps) opsShift(ins *IRInstr, op obj.As) {
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

func (lc *lowerOps) opsShiftImm(ins *IRInstr, op obj.As) {
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
		if true { // FAST {
			// is slow no matter what we set this to, if earlier one is on slow.
			// => it is the other one that really matters.
			if off := lc.spilledRegFileOff(ins.Dst); off >= 0 {
				lc.emitMI(op, ins.Imm, goasm.REG_AMD64_BP, off)
				return
			}
		} else {
			// !FAST, slow.
			if base, off, ok := lc.spilledMemOp(ins.Dst); ok {
				lc.emitMI(op, ins.Imm, base, off)
				return
			}
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

func (lc *lowerOps) opsDiv(ins *IRInstr, signed, wantRem bool) {
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

func (lc *lowerOps) opsMulHigh(ins *IRInstr, signed bool) {
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

func (lc *lowerOps) opsMulHSU(ins *IRInstr) {
	a := lc.stageInt(ins.A, 0) // RAX = signed operand
	b := lc.stageInt(ins.B, 1) // RCX = unsigned operand

	rdxLive := lc.isRegLive(goasm.REG_AMD64_DX)
	if rdxLive {
		lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_SP, lc.sretOffset+8)
	}

	// Compute correction = (A < 0) ? B : 0 using RDX as temp.
	// A is in RAX — keep it there for MULQ.
	lc.emit2(x86.AMOVQ, a, goasm.REG_AMD64_DX)   // RDX = A
	lc.emitRI(x86.ASARQ, 63, goasm.REG_AMD64_DX) // RDX = sign(A)
	lc.emit2(x86.AANDQ, b, goasm.REG_AMD64_DX)   // RDX = correction

	lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_SP, lc.sretOffset+16) // save correction

	// Unsigned multiply: RDX:RAX = A * B (A still in RAX).
	p := lc.c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = b
	lc.c.Append(p)

	// Subtract correction from high bits.
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, lc.sretOffset+16, stgA)
	lc.emit2(x86.ASUBQ, stgA, goasm.REG_AMD64_DX)

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

func (lc *lowerOps) opsSet(ins *IRInstr) {
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

func (lc *lowerOps) opsSetImm(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	a := aHR
	if a < 0 {
		a = lc.stageInt(ins.A, 0)
	}
	if ins.Imm >= -(1<<31) && ins.Imm < (1<<31) {
		lc.emitCmpRI(a, ins.Imm)
	} else {
		lc.loadImm(ins.Imm, stgB)
		lc.emit2(x86.ACMPQ, a, stgB)
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

func (lc *lowerOps) opsLoad(ins *IRInstr) {
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

func (lc *lowerOps) opsStore(ins *IRInstr) {
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

func (lc *lowerOps) opsLoadX(ins *IRInstr) {
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

func (lc *lowerOps) opsStoreX(ins *IRInstr) {
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
	p.To.Reg = stgB
	lc.c.Append(p)

	src := ins.Dst
	if lc.isVRegFP(src) {
		srcReg := lc.stageFP(src, 0)
		lc.emitMR(storeOp(ins.T), srcReg, stgB, 0)
	} else {
		srcReg := lc.stageInt(src, 0)
		lc.emitMR(storeOp(ins.T), srcReg, stgB, 0)
	}
}

// ── Control flow ──

func (lc *lowerOps) opsBranch(ins *IRInstr) {
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

func (lc *lowerOps) opsBranchImm(ins *IRInstr) {
	aHR := lc.directReg(ins.A)
	a := aHR
	if a < 0 {
		a = lc.stageInt(ins.A, 0)
	}
	if ins.Imm2 >= -(1<<31) && ins.Imm2 < (1<<31) {
		lc.emitCmpRI(a, ins.Imm2)
	} else {
		lc.loadImm(ins.Imm2, stgB)
		lc.emit2(x86.ACMPQ, a, stgB)
	}
	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerOps) opsJump(ins *IRInstr) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

// ── Misaligned access (inline expansion) ──
//
// The lowerer expands IRMisalignLoad/IRMisalignStore into inline
// byte-by-byte access using two scratch registers (stgA = RAX,
// stgB = RCX). memBase and memMask are read from [RBP+520] and
// [RBP+528] respectively (stored there by jit_sandbox.c before entry).

func (lc *lowerOps) opsMisalignLoad(ins *IRInstr) {
	width := typeWidth(ins.T)
	dstReg := lc.writeDst(ins.Dst)

	// addr is either in a pool register (directReg) or spilled.
	// RAX/RCX are never pool registers, so directReg never returns them.
	// Strategy: accumulate in RAX, use CX as scratch. Final MOV RAX→dst.
	accum := stgA // RAX
	scr := stgB   // RCX

	addrPool := lc.directReg(ins.A)
	addrSpillBase, addrSpillOff, addrSpilled := lc.spilledMemOp(ins.A)

	for i := 0; i < width; i++ {
		// scr = addr + i
		if addrPool >= 0 {
			lc.emit2(x86.AMOVQ, addrPool, scr)
		} else if addrSpilled {
			lc.emitRM(x86.AMOVQ, addrSpillBase, addrSpillOff, scr)
		} else {
			lc.stageInt(ins.A, 1) // loads into CX
		}
		if i > 0 {
			lc.emitRI(x86.AADDQ, int64(i), scr)
		}
		lc.emitRM(x86.AANDQ, goasm.REG_AMD64_BP, rfMemMaskOff, scr)
		lc.emitRM(x86.AADDQ, goasm.REG_AMD64_BP, rfMemBaseOff, scr)
		lc.emitRMMovzx(scr, 0, scr) // scr = byte
		if i == 0 {
			lc.emit2(x86.AMOVQ, scr, accum) // accum = byte0
		} else {
			lc.emitRI(x86.ASHLQ, int64(i*8), scr)
			lc.emit2(x86.AORQ, scr, accum)
		}
	}

	lc.emit2(x86.AMOVQ, accum, dstReg)
	lc.commitDst(ins.Dst, dstReg)
}

func (lc *lowerOps) opsMisalignStore(ins *IRInstr) {
	width := typeWidth(ins.T)

	// addr and val are in pool registers or spilled.
	// RAX/RCX are never pool regs, so we use them freely as scratch.
	scrAddr := stgA // RAX — host address
	scrVal := stgB  // RCX — byte value

	addrPool := lc.directReg(ins.A)
	addrSpillBase, addrSpillOff, addrSpilled := lc.spilledMemOp(ins.A)
	valPool := lc.directReg(ins.B)
	valSpillBase, valSpillOff, valSpilled := lc.spilledMemOp(ins.B)

	for i := 0; i < width; i++ {
		// scrVal = (value >> (i*8))
		if valPool >= 0 {
			lc.emit2(x86.AMOVQ, valPool, scrVal)
		} else if valSpilled {
			lc.emitRM(x86.AMOVQ, valSpillBase, valSpillOff, scrVal)
		} else {
			lc.stageInt(ins.B, 1)
		}
		if i > 0 {
			lc.emitRI(x86.ASHRQ, int64(i*8), scrVal)
		}

		// scrAddr = memBase + ((addr + i) & mask)
		if addrPool >= 0 {
			lc.emit2(x86.AMOVQ, addrPool, scrAddr)
		} else if addrSpilled {
			lc.emitRM(x86.AMOVQ, addrSpillBase, addrSpillOff, scrAddr)
		} else {
			lc.stageInt(ins.A, 0)
		}
		if i > 0 {
			lc.emitRI(x86.AADDQ, int64(i), scrAddr)
		}
		lc.emitRM(x86.AANDQ, goasm.REG_AMD64_BP, rfMemMaskOff, scrAddr)
		lc.emitRM(x86.AADDQ, goasm.REG_AMD64_BP, rfMemBaseOff, scrAddr)

		lc.emitMRByte(scrVal, scrAddr, 0)
	}
}

// ── FP ops ──

func (lc *lowerOps) opsFPBinop(ins *IRInstr, f64op, f32op obj.As) {
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

func (lc *lowerOps) opsFPUnary(ins *IRInstr, f64op, f32op obj.As) {
	a := lc.stageFP(ins.A, 0)
	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	dst := lc.writeDstFP(ins.Dst)
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerOps) opsFNeg(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	lc.emit2(x86.AMOVQ, a, stgA)
	var mask int64
	if ins.T == F32 {
		mask = 1 << 31
	} else {
		mask = -1 << 63
	}
	lc.loadImm(mask, stgB)
	lc.emit2(x86.AXORQ, stgB, stgA)
	lc.emit2(x86.AMOVQ, stgA, a)
	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVSD, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerOps) opsFAbs(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	lc.emit2(x86.AMOVQ, a, stgA)
	var mask int64
	if ins.T == F32 {
		mask = 0x7FFFFFFF
	} else {
		mask = 0x7FFFFFFFFFFFFFFF
	}
	lc.loadImm(mask, stgB)
	lc.emit2(x86.AANDQ, stgB, stgA)
	lc.emit2(x86.AMOVQ, stgA, a)
	dst := lc.writeDstFP(ins.Dst)
	if dst != a {
		lc.emit2(x86.AMOVSD, a, dst)
	}
	lc.commitDst(ins.Dst, dst)
}

func (lc *lowerOps) opsFCmp(ins *IRInstr) {
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
		scrByte := byteReg(stgB)
		if dst == stgB {
			scrByte = byteReg(stgA)
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
		scrByte := byteReg(stgB)
		if dst == stgB {
			scrByte = byteReg(stgA)
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

func (lc *lowerOps) opsFCvtToI(ins *IRInstr) {
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

func (lc *lowerOps) opsFCvtFromI(ins *IRInstr) {
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

func (lc *lowerOps) opsFCvtFF(ins *IRInstr) {
	a := lc.stageFP(ins.A, 0)
	dst := lc.writeDstFP(ins.Dst)
	op := x86.ACVTSS2SD
	if ins.U == F64 {
		op = x86.ACVTSD2SS
	}
	lc.emit2(op, a, dst)
	lc.commitDst(ins.Dst, dst)
}

// ── Common instruction dispatch ──

// lowerInstrCommon handles all shared IR ops. Returns (true, nil) if
// the instruction was handled, (true, err) on error, or (false, nil)
// if the op should be handled by the backend-specific lowerer.
func (lc *lowerOps) lowerInstrCommon(ins *IRInstr) (bool, error) {
	switch ins.Op {
	case IROpInvalid:
		return true, fmt.Errorf("invalid op at index %d", lc.idx)

	// Data movement
	case IRMov:
		lc.opsMov(ins)
	case IRConst:
		lc.opsConst(ins)
	case IRSext:
		lc.opsSext(ins)
	case IRZext:
		lc.opsZext(ins)

	// Integer ALU
	case IRAdd:
		lc.opsBinop(ins, x86.AADDQ)
	case IRAddImm:
		lc.opsBinopImm(ins, x86.AADDQ)
	case IRSub:
		lc.opsBinop(ins, x86.ASUBQ)
	case IRSubImm:
		lc.opsBinopImm(ins, x86.ASUBQ)
	case IRMul:
		lc.opsBinop(ins, x86.AIMULQ)
	case IRNeg:
		lc.opsUnary(ins, x86.ANEGQ)

	// DIV/MUL high
	case IRDivS:
		lc.opsDiv(ins, true, false)
	case IRDivU:
		lc.opsDiv(ins, false, false)
	case IRRem:
		lc.opsDiv(ins, true, true)
	case IRRemU:
		lc.opsDiv(ins, false, true)
	case IRMulHS:
		lc.opsMulHigh(ins, true)
	case IRMulHU:
		lc.opsMulHigh(ins, false)
	case IRMulHSU:
		lc.opsMulHSU(ins)

	// Shifts
	case IRShl:
		lc.opsShift(ins, x86.ASHLQ)
	case IRShlImm:
		lc.opsShiftImm(ins, x86.ASHLQ)
	case IRShr:
		lc.opsShift(ins, x86.ASHRQ)
	case IRShrImm:
		lc.opsShiftImm(ins, x86.ASHRQ)
	case IRSar:
		lc.opsShift(ins, x86.ASARQ)
	case IRSarImm:
		lc.opsShiftImm(ins, x86.ASARQ)

	// Bitwise
	case IRAnd:
		lc.opsBinop(ins, x86.AANDQ)
	case IRAndImm:
		lc.opsBinopImm(ins, x86.AANDQ)
	case IROr:
		lc.opsBinop(ins, x86.AORQ)
	case IROrImm:
		lc.opsBinopImm(ins, x86.AORQ)
	case IRXor:
		lc.opsBinop(ins, x86.AXORQ)
	case IRXorImm:
		lc.opsBinopImm(ins, x86.AXORQ)
	case IRNot:
		lc.opsUnary(ins, x86.ANOTQ)

	// Bit manipulation
	case IRClz:
		if ins.T == I32 {
			lc.opsUnary(ins, x86.ALZCNTL)
		} else {
			lc.opsUnary(ins, x86.ALZCNTQ)
		}
	case IRCtz:
		if ins.T == I32 {
			lc.opsUnary(ins, x86.ATZCNTL)
		} else {
			lc.opsUnary(ins, x86.ATZCNTQ)
		}
	case IRPopcount:
		if ins.T == I32 {
			lc.opsUnary(ins, x86.APOPCNTL)
		} else {
			lc.opsUnary(ins, x86.APOPCNTQ)
		}
	case IRBswap:
		lc.opsUnary(ins, x86.ABSWAPQ)

	// Comparison
	case IRSet:
		lc.opsSet(ins)
	case IRSetImm:
		lc.opsSetImm(ins)

	// Memory
	case IRLoad:
		lc.opsLoad(ins)
	case IRStore:
		lc.opsStore(ins)
	case IRLoadX:
		lc.opsLoadX(ins)
	case IRStoreX:
		lc.opsStoreX(ins)

	// Control flow (common)
	case IRLabel:
		lc.placeLabel(Label(ins.Imm))
	case IRBranch:
		lc.opsBranch(ins)
	case IRBranchImm:
		lc.opsBranchImm(ins)
	case IRJump:
		lc.opsJump(ins)
	case IRMisalignLoad:
		lc.opsMisalignLoad(ins)
	case IRMisalignStore:
		lc.opsMisalignStore(ins)

	// FP arithmetic
	case IRFAdd:
		lc.opsFPBinop(ins, x86.AADDSD, x86.AADDSS)
	case IRFSub:
		lc.opsFPBinop(ins, x86.ASUBSD, x86.ASUBSS)
	case IRFMul:
		lc.opsFPBinop(ins, x86.AMULSD, x86.AMULSS)
	case IRFDiv:
		lc.opsFPBinop(ins, x86.ADIVSD, x86.ADIVSS)
	case IRFSqrt:
		lc.opsFPUnary(ins, x86.ASQRTSD, x86.ASQRTSS)
	case IRFNeg:
		lc.opsFNeg(ins)
	case IRFAbs:
		lc.opsFAbs(ins)
	case IRFCmp:
		lc.opsFCmp(ins)

	// FP conversions
	case IRFCvtToI:
		lc.opsFCvtToI(ins)
	case IRFCvtToU:
		lc.opsFCvtToI(ins)
	case IRFCvtFromI:
		lc.opsFCvtFromI(ins)
	case IRFCvtFromU:
		lc.opsFCvtFromI(ins)
	case IRFCvtFF:
		lc.opsFCvtFF(ins)

	case IRStopperLoad:
		lc.opsStopperLoad(ins)
	case IRMemAdd:
		lc.opsMemAdd(ins)
	case IRMemBudget:
		lc.opsMemBudget(ins)

	case IRZeroIC:
		lc.opsZeroIC()
	case IRIncIC:
		lc.opsIncIC()
	case IRDecIC:
		lc.opsDecIC()
	case IRSpillIC:
		lc.opsSpillIC()
	case IRRegBudget:
		lc.opsRegBudget(ins)

	// Pseudo-ops
	case IRMarkLive, IRMarkDead, IRWriteback:
		// no-op

	// Backend-specific ops — not handled here.
	case IRRet, IRRetDyn, IRChainExit, IRJalrIC, IRCall, IRSyscall:
		return false, nil

	default:
		return false, nil
	}
	return true, nil
}

// opsStopperLoad emits the guard-page probe for backward-branch preemption.
// On amd64: MOVQ imm64,RAX; TESTQ RAX,(RAX). The TESTQ reads [RAX] and
// ANDs with RAX, writing only EFLAGS — no GP register is dirtied beyond the
// staging register (RAX) which is always scratch.
//
// ARM64 note: use LDR XZR, [Xn] — loads into the zero register, discards
// the value. Full TLB/page-table walk still occurs, so PROT_NONE faults.
func (lc *lowerOps) opsStopperLoad(ins *IRInstr) {
	lc.loadImm(ins.Imm, stgA)
	// TESTQ RAX, (RAX): From=reg, To=mem(base=RAX, offset=0).
	p := lc.c.NewProg()
	p.As = x86.ATESTQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = stgA
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = stgA
	p.To.Offset = 0
	lc.c.Append(p)
}

// abjitStateICOffset is the byte offset of IC in abjit.State.
// Must agree with abjit/abjit.go and TestStateLayout.
const abjitStateICOffset = 600

func (lc *lowerOps) opsMemAdd(ins *IRInstr) {
	p := lc.c.NewProg()
	p.As = x86.AADDQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = ins.Imm2 // delta
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = goasm.REG_AMD64_BP
	p.To.Offset = ins.Imm // offset
	lc.c.Append(p)
}

func (lc *lowerOps) opsMemBudget(ins *IRInstr) {
	off := int64(abjitStateICOffset)

	// ADD QWORD [RBP + off], delta
	p1 := lc.c.NewProg()
	p1.As = x86.AADDQ
	p1.From.Type = obj.TYPE_CONST
	p1.From.Offset = ins.Imm // delta
	p1.To.Type = obj.TYPE_MEM
	p1.To.Reg = goasm.REG_AMD64_BP
	p1.To.Offset = off
	lc.c.Append(p1)

	// CMP QWORD [RBP + off], budget
	p2 := lc.c.NewProg()
	p2.As = x86.ACMPQ
	p2.From.Type = obj.TYPE_MEM
	p2.From.Reg = goasm.REG_AMD64_BP
	p2.From.Offset = off
	p2.To.Type = obj.TYPE_CONST
	p2.To.Offset = ins.Imm2 // budget
	lc.c.Append(p2)

	// JGE overflow label
	p3 := lc.c.NewProg()
	p3.As = x86.AJGE
	p3.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p3)
	lc.bindLabel(Label(ins.Dst), p3)
}

func (lc *lowerOps) opsZeroIC() {
	p := lc.c.NewProg()
	p.As = x86.AXORQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = goasm.REG_AMD64_R15
	p.To.Type = obj.TYPE_REG
	p.To.Reg = goasm.REG_AMD64_R15
	lc.c.Append(p)
}

func (lc *lowerOps) opsIncIC() {
	p := lc.c.NewProg()
	p.As = x86.AINCQ
	p.To.Type = obj.TYPE_REG
	p.To.Reg = goasm.REG_AMD64_R15
	lc.c.Append(p)
}

func (lc *lowerOps) opsDecIC() {
	p := lc.c.NewProg()
	p.As = x86.ADECQ
	p.To.Type = obj.TYPE_REG
	p.To.Reg = goasm.REG_AMD64_R15
	lc.c.Append(p)
}

func (lc *lowerOps) opsSpillIC() {
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = goasm.REG_AMD64_R15
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = goasm.REG_AMD64_BP
	p.To.Offset = abjitStateICOffset
	lc.c.Append(p)
}

func (lc *lowerOps) opsRegBudget(ins *IRInstr) {
	p1 := lc.c.NewProg()
	p1.As = x86.ACMPQ
	p1.From.Type = obj.TYPE_REG
	p1.From.Reg = goasm.REG_AMD64_R15
	p1.To.Type = obj.TYPE_CONST
	p1.To.Offset = ins.Imm2
	lc.c.Append(p1)

	p2 := lc.c.NewProg()
	p2.As = x86.AJGE
	p2.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p2)
	lc.bindLabel(Label(ins.Dst), p2)
}

// Ensure imports are used.
var _ = sort.Sort
var _ = fmt.Errorf
