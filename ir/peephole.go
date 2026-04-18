package ir

// PeepholeSz is the sliding-window depth for the online peephole optimizer.
// Tunable: larger windows catch more patterns but cost more per emit.
const PeepholeSz = 4

// tryPeephole is called after each emit. It examines the last few instructions
// and rewrites if any pattern matches. Returns true if a rewrite occurred;
// the caller re-checks for cascading rewrites.
func (e *Emitter) tryPeephole() bool {
	n := len(e.Block.Instrs)
	if n == 0 {
		return false
	}

	last := &e.Block.Instrs[n-1]

	// Pattern 1: IRMov a, a -> delete (self-move is no-op).
	if last.Op == IRMov && last.Dst == last.A {
		e.Block.Instrs = e.Block.Instrs[:n-1]
		return true
	}

	// Pattern 2: IRAddImm dst, a, 0 -> simplify.
	if last.Op == IRAddImm && last.Imm == 0 {
		return e.simplifyIdentity(n)
	}

	// Pattern 3: IRShlImm dst, a, 0 -> simplify.
	if last.Op == IRShlImm && last.Imm == 0 {
		return e.simplifyIdentity(n)
	}

	// Pattern 4: IRShrImm dst, a, 0 -> simplify.
	if last.Op == IRShrImm && last.Imm == 0 {
		return e.simplifyIdentity(n)
	}

	// Pattern 5: IRSarImm dst, a, 0 -> simplify.
	if last.Op == IRSarImm && last.Imm == 0 {
		return e.simplifyIdentity(n)
	}

	// Pattern 6: IRAndImm dst, a, -1 -> simplify (AND with all-ones is identity).
	if last.Op == IRAndImm && last.Imm == -1 {
		return e.simplifyIdentity(n)
	}

	// Pattern 7: IROrImm dst, a, 0 -> simplify (OR with 0 is identity).
	if last.Op == IROrImm && last.Imm == 0 {
		return e.simplifyIdentity(n)
	}

	// Pattern 8: IRXorImm dst, a, 0 -> simplify (XOR with 0 is identity).
	if last.Op == IRXorImm && last.Imm == 0 {
		return e.simplifyIdentity(n)
	}

	// Pattern 9: IRConst tmp, 0 + IRStore -> fold to store-zero.
	// Only fold when the const target is a fresh temp not used elsewhere.
	if n >= 2 {
		prev := &e.Block.Instrs[n-2]
		if prev.Op == IRConst && prev.Imm == 0 && prev.Dst >= VRegTempStart &&
			last.Op == IRStore && last.B == prev.Dst && !e.vregUsedLater(prev.Dst, n-1) {
			// Replace: keep Store but source becomes VRegZero; delete the Const.
			last.B = VRegZero
			e.Block.Instrs[n-2] = *last
			e.Block.Instrs = e.Block.Instrs[:n-1]
			return true
		}
	}

	return false
}

// simplifyIdentity handles the common case where an *Imm op is an identity
// (e.g. AddImm +0, ShlImm +0, AndImm -1). If dst==a, delete; else rewrite to IRMov.
func (e *Emitter) simplifyIdentity(n int) bool {
	last := &e.Block.Instrs[n-1]
	if last.Dst == last.A {
		e.Block.Instrs = e.Block.Instrs[:n-1]
	} else {
		*last = IRInstr{Op: IRMov, T: I64, Dst: last.Dst, A: last.A}
	}
	return true
}

// vregUsedLater checks if vr is referenced after index startIdx.
// Conservative: guest regs (< VRegTempStart) are assumed live.
// Temps (>= VRegTempStart) at the end of the current window are assumed dead.
func (e *Emitter) vregUsedLater(vr VReg, startIdx int) bool {
	if vr < VRegTempStart {
		return true // guest regs may be used later or at block exit
	}
	// For temps at the end of the peephole window, they're dead.
	return false
}
