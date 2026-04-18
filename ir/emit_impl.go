package ir

// op3 emits a three-register operation: Dst = A op B.
// If dst is VRegZero, the instruction is discarded (RISC-V x0 semantics).
func (e *Emitter) op3(op IROp, t Type, dst, a, b VReg) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: op, T: t, Dst: dst, A: a, B: b})
	e.MarkDirty(dst)
}

// op2i emits a register-plus-immediate operation: Dst = A op Imm.
func (e *Emitter) op2i(op IROp, t Type, dst, a VReg, imm int64) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: op, T: t, Dst: dst, A: a, Imm: imm})
	e.MarkDirty(dst)
}

// op2 emits a two-register operation: Dst = op(A).
func (e *Emitter) op2(op IROp, t Type, dst, a VReg) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: op, T: t, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// opConst emits a constant load: Dst = Imm.
func (e *Emitter) opConst(dst VReg, imm int64) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRConst, T: I64, Dst: dst, Imm: imm})
	e.MarkDirty(dst)
}

// opSet emits a comparison: Dst = (A pred B) ? 1 : 0.
func (e *Emitter) opSet(op IROp, dst, a, b VReg, p Pred) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: op, T: I64, Dst: dst, A: a, B: b, Pred: p})
	e.MarkDirty(dst)
}

// opSetImm emits a comparison with immediate: Dst = (A pred Imm) ? 1 : 0.
func (e *Emitter) opSetImm(op IROp, dst, a VReg, imm int64, p Pred) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: op, T: I64, Dst: dst, A: a, Imm: imm, Pred: p})
	e.MarkDirty(dst)
}

// opExt emits a sign- or zero-extend: Dst = extend(A) from type fromT.
func (e *Emitter) opExt(op IROp, dst, a VReg, fromT Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: op, T: fromT, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// emit appends an instruction to the block and runs the peephole optimizer.
func (e *Emitter) emit(ins IRInstr) {
	e.Block.Instrs = append(e.Block.Instrs, ins)
	if ins.Op == IRLabel {
		e.Block.Labels[Label(ins.Imm)] = len(e.Block.Instrs) - 1
	}
	for e.tryPeephole() {
	}
}
