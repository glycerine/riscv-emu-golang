package ir

// JitOKJalrMiss is the Result.Status value written by a JALR inline-cache
// miss stub. The Go dispatcher reads this to distinguish an IC miss from a
// normal jitOK return, and reads Result.FaultAddr (repurposed) as the
// site index for patching. Must not collide with any status constant in
// jit.go (jitOK=0, jitEcall=1, jitEbreak=2, jitLoadFault=3, jitStoreFault=4,
// jitIllegal=5).
const JitOKJalrMiss = 6

// MaxIC is the maximum instruction count before a backward branch forces
// a block exit. This ensures GC preemption windows and prevents infinite loops
// inside a single JIT block.
const MaxIC = 4096

// MaskedLoad performs a bounds-checked guest memory load:
//
//	addr = base + off
//	if (addr | (addr + width-1)) & ~mask != 0: goto faultLabel
//	dst = *(T*)(memBase + (addr & mask))
//
// For signed sub-I64 loads, a sign-extend is included via Load.
func (e *Emitter) MaskedLoad(dst, base, memBase, mask VReg, off int64, width int, signed bool, faultLabel Label) {
	addr := e.Tmp()
	e.AddImm(addr, base, off)

	// OOB check: (addr | (addr + width-1)) & ~mask != 0
	tmp1 := e.Tmp()
	e.AddImm(tmp1, addr, int64(width-1))
	e.Or(tmp1, addr, tmp1)
	maskNot := e.Tmp()
	e.Not(maskNot, mask)
	e.And(tmp1, tmp1, maskNot)
	e.Branch(tmp1, VRegZero, NE, faultLabel)

	// Masked dereference.
	masked := e.Tmp()
	e.And(masked, addr, mask)
	host := e.Tmp()
	e.Add(host, memBase, masked)
	t := widthToType(width)
	e.Load(dst, host, 0, t, signed)
}

// GuestStore performs a bounds-checked guest memory store:
//
//	addr = base + off
//	if (addr | (addr + width-1)) & ~mask != 0: goto faultLabel
//	*(T*)(memBase + (addr & mask)) = src
func (e *Emitter) GuestStore(base, memBase, mask VReg, off int64, src VReg, width int, faultLabel Label) {
	addr := e.Tmp()
	e.AddImm(addr, base, off)

	// OOB check.
	tmp1 := e.Tmp()
	e.AddImm(tmp1, addr, int64(width-1))
	e.Or(tmp1, addr, tmp1)
	maskNot := e.Tmp()
	e.Not(maskNot, mask)
	e.And(tmp1, tmp1, maskNot)
	e.Branch(tmp1, VRegZero, NE, faultLabel)

	// Masked store.
	masked := e.Tmp()
	e.And(masked, addr, mask)
	host := e.Tmp()
	e.Add(host, memBase, masked)
	t := widthToType(width)
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

// BudgetCheck emits a backward-branch budget check:
//
//	if (ic >= MaxIC) { writeback; return(targetPC, 0, 0) }
//	goto target
func (e *Emitter) BudgetCheck(target Label, targetPC uint64) {
	tooBig := e.NewLabel()
	e.BranchImm(e.ic, int64(MaxIC), GE, tooBig)
	e.Jump(target)
	e.PlaceLabel(tooBig)
	e.WriteBackAll()
	e.Ret(targetPC, 0, VRegZero)
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
