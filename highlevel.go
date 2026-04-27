package riscv

// JitOKJalrMiss is the Result.Status value written by a JALR inline-cache
// miss stub. The Go dispatcher reads this to distinguish an IC miss from a
// normal jitOK return, and reads Result.FaultAddr (repurposed) as the
// site index for patching. Must not collide with any status constant in
// jit.go (jitOK=0, jitEcall=1, jitEbreak=2, jitLoadFault=3, jitStoreFault=4,
// jitIllegal=5).
const JitOKJalrMiss = 6

// JitMisalign is returned when a JIT block encounters a misaligned
// memory access. The dispatcher re-executes the faulting instruction
// via the interpreter (which handles misalignment with byte-by-byte
// reads/writes) and then continues JIT dispatch from the next PC.
const JitMisalign = 7


// MaskedLoad performs a bounds-checked guest memory load:
//
//	addr = base + off
//	if (addr | (addr + width-1)) & ~mask != 0: goto faultLabel
//	dst = *(T*)(memBase + (addr & mask))
//
// For signed sub-I64 loads, a sign-extend is included via Load.
//
// width=1 fast path: the page-cross clause `addr | (addr+width-1)`
// reduces to just `addr` (addr+0 == addr, addr|addr == addr), so we
// skip the two redundant IR ops — a small but constant savings on
// every byte load.
func (e *Emitter) MaskedLoad(dst, base, memBase, mask VReg, off int64, width int, signed bool, faultLabel Label) {
	addr := e.Tmp()
	e.AddImm(addr, base, off)
	e.MaskedLoadAddr(dst, addr, memBase, mask, width, signed, faultLabel)
}

// MaskedLoadAddr is MaskedLoad with the address VReg pre-computed by
// the caller. Used by the JIT emitter when it has already computed
// addr=base+off for fault-tail reporting via allocFaultLabel — avoids
// an otherwise-duplicate AddImm (and its spill+reload in host code).
func (e *Emitter) MaskedLoadAddr(dst, addr, memBase, mask VReg, width int, signed bool, faultLabel Label) {
	// OOB check: (addr | (addr + width-1)) & ~mask != 0
	endAddr := addr
	if width > 1 {
		endAddr = e.Tmp()
		e.AddImm(endAddr, addr, int64(width-1))
		e.Or(endAddr, addr, endAddr)
	}
	maskNot := e.Tmp()
	e.Not(maskNot, mask)
	oob := e.Tmp()
	e.And(oob, endAddr, maskNot)
	e.Branch(oob, VRegZero, NE, faultLabel)

	// Masked dereference.
	masked := e.Tmp()
	e.And(masked, addr, mask)
	host := e.Tmp()
	e.Add(host, memBase, masked)
	t := WidthToType(width)
	e.Load(dst, host, 0, t, signed)
}

// GuestStore performs a bounds-checked guest memory store:
//
//	addr = base + off
//	if (addr | (addr + width-1)) & ~mask != 0: goto faultLabel
//	*(T*)(memBase + (addr & mask)) = src
//
// width=1 fast path: same simplification as MaskedLoad.
func (e *Emitter) GuestStore(base, memBase, mask VReg, off int64, src VReg, width int, faultLabel Label) {
	addr := e.Tmp()
	e.AddImm(addr, base, off)
	e.GuestStoreAddr(addr, memBase, mask, src, width, faultLabel)
}

// GuestStoreAddr is GuestStore with the address VReg pre-computed —
// see MaskedLoadAddr for the rationale.
func (e *Emitter) GuestStoreAddr(addr, memBase, mask, src VReg, width int, faultLabel Label) {
	// OOB check.
	endAddr := addr
	if width > 1 {
		endAddr = e.Tmp()
		e.AddImm(endAddr, addr, int64(width-1))
		e.Or(endAddr, addr, endAddr)
	}
	maskNot := e.Tmp()
	e.Not(maskNot, mask)
	oob := e.Tmp()
	e.And(oob, endAddr, maskNot)
	e.Branch(oob, VRegZero, NE, faultLabel)

	// Masked store.
	masked := e.Tmp()
	e.And(masked, addr, mask)
	host := e.Tmp()
	e.Add(host, memBase, masked)
	t := WidthToType(width)
	e.Store(host, 0, src, t)
}

// WriteBackAll writes all dirty cached vregs back to the x[] and f[] arrays.
// Used before block exits. Does NOT clear dirty flags — multiple exit points
// in a block each need their own writeback sequence.
func (e *Emitter) WriteBackAll() {
	// Integer registers x1..x31 (VRegs 1..31).
	for vr := VReg(1); vr < 32; vr++ {
		if int(vr) < len(e.dirty) && e.dirty[vr] {
			e.Store(e.xBase, int64(vr)*8, vr, I64)
		}
	}
	// FP registers f0..f31 (VRegs 32..63).
	for vr := VReg(32); vr < 64; vr++ {
		if int(vr) < len(e.dirty) && e.dirty[vr] {
			e.Store(e.fBase, int64(vr-32)*8, vr, I64)
		}
	}
}

// WriteBackReg writes a single vreg back to the x[] or f[] array and marks it clean.
func (e *Emitter) WriteBackReg(vr VReg) {
	if vr == VRegZero {
		return
	}
	if vr < 32 {
		e.Store(e.xBase, int64(vr)*8, vr, I64)
	} else if vr < 64 {
		e.Store(e.fBase, int64(vr-32)*8, vr, I64)
	}
	if int(vr) < len(e.dirty) {
		e.dirty[vr] = false
	}
}

// FaultExit emits writeback of all dirty vregs followed by a return with fault info.
func (e *Emitter) FaultExit(pc uint64, status int, faultAddr VReg) {
	e.WriteBackAll()
	e.Ret(pc, status, faultAddr)
}

// ChainableRet emits writeback of all dirty vregs followed by a chain exit.
// Used for jitOK exits that can be patched for block chaining.
func (e *Emitter) ChainableRet(targetPC uint64, exitIdx int) {
	e.WriteBackAll()
	e.ChainExit(targetPC, exitIdx)
}

// DynChainableRet emits writeback of all dirty vregs followed by a JALR
// inline-cache dispatch. Used for JALR exits where the target PC is
// computed at runtime. On IC hit, jumps directly to the target block's
// chainEntry. On miss, returns to Go with siteIdx so Go can patch.
func (e *Emitter) DynChainableRet(target VReg, siteIdx int) {
	e.WriteBackAll()
	e.JalrIC(target, siteIdx)
}

// StopperLoad emits a guard-page probe at a backward branch. The lowerer
// emits TESTQ RAX,(RAX) on amd64 which reads from the stopper page without
// dirtying any GP register. If the page is armed (PROT_NONE) the read faults
// and RunJIT's defer/recover returns ErrPreempted.
//
// ARM64 note: use LDR XZR, [Xn] — loads into the zero register, discards
// the value. Full TLB/page-table walk still occurs, so PROT_NONE faults.
func (e *Emitter) StopperLoad(addr int64) {
	e.emit(IRInstr{Op: IRStopperLoad, Imm: addr})
}

// MemAdd emits ADD QWORD [RBP+offset], delta. No GP registers modified.
func (e *Emitter) MemAdd(offset int64, delta int64) {
	e.emit(IRInstr{Op: IRMemAdd, Imm: offset, Imm2: delta})
}

// MemBudget emits a batched IC budget check for lockstep mode.
// delta is the instruction count to add. budget is the limit.
// overflowLabel is jumped to when budget is exceeded.
func (e *Emitter) MemBudget(delta int, budget int64, overflowLabel Label) {
	e.emit(IRInstr{
		Op:   IRMemBudget,
		Imm:  int64(delta),
		Imm2: budget,
		Dst:  VReg(overflowLabel),
	})
}

// ZeroIC emits XOR R15, R15 to zero the IC register at block entry.
func (e *Emitter) ZeroIC() { e.emit(IRInstr{Op: IRZeroIC}) }

// IncIC emits INC R15 to count one RISC-V instruction.
func (e *Emitter) IncIC() { e.emit(IRInstr{Op: IRIncIC}) }

// SpillIC emits MOV [RBP+IC_offset], R15 to write the IC register to State.
func (e *Emitter) SpillIC() { e.emit(IRInstr{Op: IRSpillIC}) }

// RegBudget emits CMP R15, budget; JGE overflowLabel.
func (e *Emitter) RegBudget(budget int64, overflowLabel Label) {
	e.emit(IRInstr{Op: IRRegBudget, Imm2: budget, Dst: VReg(overflowLabel)})
}

// SetPC emits MOV [RBP+PC_offset], $pc — used by budget cold paths.
func (e *Emitter) SetPC(pc uint64) {
	e.emit(IRInstr{Op: IRSetPC, Imm: int64(pc)})
}

// RetBudget emits the shared budget exit: status=0, exitinfo=0, restore SP, return.
func (e *Emitter) RetBudget() { e.emit(IRInstr{Op: IRRetBudget}) }

// ClearDirtySyscallRegs clears dirty flags for a0 (x10) and a1 (x11)
// only. Called before IRSyscall so the subsequent ReloadSyscallRegs
// picks up the dispatcher's return values from x[].
func (e *Emitter) ClearDirtySyscallRegs() {
	for _, vr := range []VReg{10, 11} {
		if int(vr) < len(e.dirty) {
			e.dirty[vr] = false
		}
	}
}

// ReloadSyscallRegs reloads a0 (x10) and a1 (x11) from the x[] array.
// Matches libriscv's LOAD_SYS_REGS (tr_emit.cpp:2218-2224, regs 10..11).
// The syscall dispatcher only writes x[10] (return value); callee-saved
// registers are preserved by ECALL per RISC-V ABI.
func (e *Emitter) ReloadSyscallRegs() {
	for _, vr := range []VReg{10, 11} {
		e.Load(vr, e.xBase, int64(vr)*8, I64, false)
		e.MarkDirty(vr)
	}
}

// MarkDirty records that vr has been written. No-op for VRegZero.
func (e *Emitter) MarkDirty(vr VReg) {
	if vr == VRegZero {
		return
	}
	e.growDirty(int(vr) + 1)
	e.dirty[vr] = true
}

// IsDirty returns whether the given VReg has been written but not written back.
func (e *Emitter) IsDirty(vr VReg) bool {
	if int(vr) >= len(e.dirty) {
		return false
	}
	return e.dirty[vr]
}
