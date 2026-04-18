package ir

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
// Used before block exits.
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
