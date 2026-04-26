package riscv

// jit_emit_ir.go — Translates RISC-V basic blocks to IR (ir.Block).
// This replaces jit_emit.go's C source generation with IR emission.

import (
	"fmt"
	"os"
)

// testIterStart is set by tests to rotate gotoTargets iteration order.
// Zero means normal sorted order. Non-zero rotates by this offset.
//var testIterStart int

// emitResult holds the generated IR block and metadata.
type emitResult struct {
	block         *Block
	startPC       uint64
	endPC         uint64
	numInsns      int
	numChainExits int // number of IRChainExit instructions emitted
}

// deferredExit holds an external branch exit to emit at finalize time.
type deferredExit struct {
	label    Label
	targetPC uint64
}

// deferredFault holds a per-load/store fault exit to emit at finalize time.
// Each load/store registers one so the fault tail returns its own PC and the
// live VReg holding the faulting guest address (matches jit_emit.go behavior).
type deferredFault struct {
	label  Label
	pc     uint64 // PC of the faulting RISC-V instruction
	addrVR VReg   // VReg holding the computed guest address
	status int    // jitLoadFault or jitStoreFault
}

// emitter accumulates IR for a basic block.
//
// regsUsed tracks guest integer registers referenced by xreg (read)
// or xregDst (write) so the prologue can pre-load them from x[].
//
// NOTE: this is deliberately read/write-union (not "read-only"), even
// though dead prologue loads for write-only regs look wasteful. The
// invariant they preserve is subtle: WriteBackAll() at block exits
// stores *every* dirty VReg regardless of which control-flow path
// actually reached the xregDst. On paths where the dirty-making write
// did not execute (e.g. a bail-label for an unvisited goto-target —
// see finalize()), the prologue-load is what puts the correct
// block-entry value into the VReg, so the "garbage" that gets stored
// back is in fact the original x[i]. Skipping the load on purely
// write-only regs breaks that invariant and causes stale host-register
// values to leak into x[]. A proper fix requires path-sensitive
// WriteBackAll, which is out of scope for Phase 5-B.
type emitter struct {
	mem            *GuestMemory
	startPC        uint64
	pc             uint64
	irEm           *Emitter
	numInsns       int
	regsUsed       uint32 // bit i set iff xreg(i) or xregDst(i) was called
	terminated     bool
	visited        map[uint64]bool
	regionEnd      uint64
	gotoTargets    u64set
	pcLabels       u64labelmap
	icEmitted      bool
	deferredExits  []deferredExit
	deferredFaults []deferredFault
	exitIdx        int      // counter for chain exit indices
	jalrSiteIdx    int      // counter for JALR inline-cache site indices
	callStack      []uint64 // RAS: expected return addresses for inlined calls
}

// ── Register access helpers ────────────────────────────────────────────

// xreg returns the VReg for integer register r (source read) and
// marks it as used so the prologue pre-loads x[r].
func (e *emitter) xreg(r uint32) VReg {
	if r == 0 {
		return VRegZero
	}
	e.regsUsed |= uint32(1) << r
	return e.irEm.XReg(r)
}

// xregDst returns the VReg for integer register r (write destination),
// marks it used (prologue load) and dirty (WriteBackAll store). See
// the comment on the emitter struct's regsUsed field for why the
// prologue load is required even for write-only regs.
func (e *emitter) xregDst(r uint32) VReg {
	if r == 0 {
		return VRegZero
	}
	e.regsUsed |= uint32(1) << r
	vr := e.irEm.XReg(r)
	e.irEm.MarkDirty(vr)
	return vr
}

// freg returns the VReg for FP register r.
func (e *emitter) freg(r uint32) VReg {
	return e.irEm.FRegV(r)
}

// fregDst returns the VReg for FP register r (write destination).
// Marks the register dirty so WriteBackAll stores it at block exit.
func (e *emitter) fregDst(r uint32) VReg {
	vr := e.irEm.FRegV(r)
	e.irEm.MarkDirty(vr)
	return vr
}

// ── NaN canonicalization (RISC-V ISA §11.3) ──────────────────────────
//
// Any FP operation whose result is NaN must return the canonical
// quiet NaN (f32 0x7FC00000, f64 0x7FF8000000000000), not a payload-
// propagating NaN. x86 hardware (ADDSS, DIVSS, etc.) produces
// 0xFFC00000 for some inputs (e.g. -0/-0), which is spec-noncompliant.
// These helpers canonicalize the JIT-computed result before boxing.
//
// Strategy: bit-cast the float to its integer bits, test the IEEE 754
// NaN predicate `(bits & 0x7FFFFFFF) > 0x7F800000` (equivalently: the
// value is greater than +infinity in unsigned integer representation
// after clearing the sign), and if true replace with canonical.
//
// The check emits one conditional branch — well-predicted-not-taken
// on non-NaN inputs, so the fast-path cost is ~1 cycle.

// canonF32 returns an F32 VReg equal to val if non-NaN, else the
// canonical quiet NaN 0x7FC00000.
func (e *emitter) canonF32(val VReg) VReg {
	em := e.irEm
	// Bit-cast val (F32 XMM) to integer bits.
	intBits := em.Tmp()
	em.MovT(intBits, val, I64)
	// absBits = bits & 0x7FFFFFFF (clears sign, keeps exp+mantissa in low 32)
	absBits := em.Tmp()
	em.AndImm(absBits, intBits, 0x7FFFFFFF)
	// compare: absBits > 0x7F800000 ⇔ NaN (exp=all 1s AND mantissa != 0)
	infBits := em.Tmp()
	em.Const(infBits, 0x7F800000)
	// Merge: intResult = NaN ? canonical : original low 32.
	mergedInt := em.Tmp()
	notNaNLabel := em.NewLabel()
	doneLabel := em.NewLabel()
	em.Branch(absBits, infBits, LEU, notNaNLabel)
	// NaN path.
	em.Const(mergedInt, 0x7FC00000)
	em.Jump(doneLabel)
	// Non-NaN path: low 32 of original int bits.
	em.PlaceLabel(notNaNLabel)
	em.Zext(mergedInt, intBits, I32)
	em.PlaceLabel(doneLabel)
	// Bit-cast back to F32 XMM.
	out := em.Tmp()
	em.MovT(out, mergedInt, F32)
	return out
}

// canonF64 is the f64 counterpart.
func (e *emitter) canonF64(val VReg) VReg {
	em := e.irEm
	intBits := em.Tmp()
	em.MovT(intBits, val, I64)
	// absBits = bits & 0x7FFFFFFFFFFFFFFF
	absBits := em.Tmp()
	absMask := em.Tmp()
	em.Const(absMask, int64(0x7FFFFFFFFFFFFFFF))
	em.And(absBits, intBits, absMask)
	infBits := em.Tmp()
	em.Const(infBits, int64(0x7FF0000000000000))
	mergedInt := em.Tmp()
	notNaNLabel := em.NewLabel()
	doneLabel := em.NewLabel()
	em.Branch(absBits, infBits, LEU, notNaNLabel)
	// NaN path: canonical qNaN for f64.
	em.Const(mergedInt, int64(0x7FF8000000000000))
	em.Jump(doneLabel)
	em.PlaceLabel(notNaNLabel)
	em.Mov(mergedInt, intBits)
	em.PlaceLabel(doneLabel)
	out := em.Tmp()
	em.MovT(out, mergedInt, F64)
	return out
}

// ── NaN-boxing helpers ─────────────────────────────────────────────────
//
// Both helpers bypass the FP VReg `freg(rs)` / `fregDst(rd)` for reads
// and writes of the raw NaN-boxed 64-bit word, and instead access
// f[rs]/f[rd] directly as I64 via `Load`/`Store` through `FBase()`.
//
// Rationale: if we read the guest FP register through its VReg, the
// allocator's type-driven load selects MOVSS for F32-typed reads,
// which zero-extends the upper 32 bits into XMM and destroys the
// NaN-box signature. Subsequent MOVQ xmm→gpr then yields `upper=0`
// and unboxF32 classifies the input as malformed, returning canonical
// NaN for every read. Going through memory as I64 guarantees the
// full 64 bits survive.
//
// The FP result is materialized via a GPR→XMM bitcast (`MovT` with
// F32 type) so downstream FADD/FSUB/... lower to ADDSS/SUBSS.
//
// boxF32 writes the NaN-boxed word directly to f[rd] in memory and
// also invalidates any cached VReg — but since we avoid `fregDst`
// (which would mark the VReg dirty and trigger a stale writeback),
// the memory-write is the single source of truth.

// boxF32 NaN-boxes a 32-bit value into f[rd] (spec §11.2).
func (e *emitter) boxF32(rd uint32, val VReg) {
	em := e.irEm
	// Bit-cast val (F32-typed, in XMM) to an I64 GPR temp.
	intBits := em.Tmp()
	em.MovT(intBits, val, I64)
	// Mask to low 32 bits (clears any XMM garbage in high 32).
	low := em.Tmp()
	em.AndImm(low, intBits, 0xFFFFFFFF)
	// OR in the NaN-box mask.
	hi := em.Tmp()
	em.Const(hi, int64(-1)<<32) // 0xFFFFFFFF00000000
	boxed := em.Tmp()
	em.Or(boxed, low, hi)
	// Store the boxed word directly to f[rd] memory.
	em.Store(em.FBase(), int64(rd)*8, boxed, I64)
}

// unboxF32 extracts a 32-bit float from f[rs], returning canonical
// qNaN if the box is malformed (upper 32 bits not all-ones, spec
// §11.2). Returns an F32-typed VReg suitable for FAdd/FSub/...
func (e *emitter) unboxF32(rs uint32) VReg {
	em := e.irEm
	// Load the raw 64-bit word from f[rs] directly as I64. This
	// bypasses the FP VReg typing and its MOVSS-zero-extend hazard.
	srcInt := em.Tmp()
	em.Load(srcInt, em.FBase(), int64(rs)*8, I64, false)
	// Check upper 32 == 0xFFFFFFFF.
	upper := em.Tmp()
	em.ShrImm(upper, srcInt, 32)
	check := em.Tmp()
	em.Const(check, 0xFFFFFFFF)
	intResult := em.Tmp()
	okLabel := em.NewLabel()
	doneLabel := em.NewLabel()
	em.Branch(upper, check, EQ, okLabel)
	em.Const(intResult, 0x7FC00000) // malformed box → canonical qNaN
	em.Jump(doneLabel)
	em.PlaceLabel(okLabel)
	em.Zext(intResult, srcInt, I32)
	em.PlaceLabel(doneLabel)
	// Bit-cast the 32-bit integer bits into an F32 VReg (XMM).
	fpResult := em.Tmp()
	em.MovT(fpResult, intResult, F32)
	return fpResult
}

// ── Control flow helpers ───────────────────────────────────────────────

func (e *emitter) getOrCreateLabel(pc uint64) Label {
	if l, ok := e.pcLabels.get(pc); ok {
		return l
	}
	l := e.irEm.NewLabel()
	e.pcLabels.set(pc, l)
	return l
}

func (e *emitter) emitLabel() {
	e.irEm.PlaceLabel(e.getOrCreateLabel(e.pc))
}

func (e *emitter) emitIC() {
	e.irEm.AddImm(e.irEm.IC(), e.irEm.IC(), 1)
}

// peek32 fetches the 32-bit instruction at the given PC without
// advancing state. Returns (insn, true) on success.
func (e *emitter) peek32(pc uint64) (uint32, bool) {
	if pc >= e.regionEnd {
		return 0, false
	}
	insn, f := e.mem.Fetch32(pc)
	if f != nil {
		return 0, false
	}
	if insn&3 != 3 {
		return 0, false // compressed instruction, not a 32-bit pair
	}
	return insn, true
}

// emitLoadFused emits a guest memory load for a fused AUIPC+LOAD.
// The guest address is a compile-time constant.
func (e *emitter) emitLoadFused(rd uint32, guestAddr int64, funct3 uint32) {
	dst := e.xregDst(rd)
	memBase := e.irEm.MemBase()
	mask := e.irEm.MemMask()

	var width int
	var signed bool
	switch funct3 {
	case 0: // LB
		width, signed = 1, true
	case 1: // LH
		width, signed = 2, true
	case 2: // LW
		width, signed = 4, true
	case 3: // LD
		width, signed = 8, false
	case 4: // LBU
		width, signed = 1, false
	case 5: // LHU
		width, signed = 2, false
	case 6: // LWU
		width, signed = 4, false
	default:
		width = 8
	}

	fl := e.allocFaultLabel(VRegZero, jitLoadFault)
	addr := e.irEm.Tmp()
	e.irEm.Const(addr, guestAddr)
	e.irEm.MaskedLoadAddr(dst, addr, memBase, mask, width, signed, fl)
}

func (e *emitter) advancePC(size uint64) {
	e.numInsns++
	e.pc += size
	if e.icEmitted {
		e.icEmitted = false
	} else {
		e.emitIC()
	}
}

func (e *emitter) emitReturn(pc uint64, status int) {
	e.irEm.WriteBackAll()
	e.irEm.Ret(pc, status, VRegZero)
}

// emitSyscall emits the ECALL fast path: writeback all dirty regs,
// then emit IRSyscall which CALLs the SysV dispatcher. The dispatcher
// returns 0 (handled → block exits with Status=jitOK) or 1 (fallback
// → Status=jitEcall, Go NoteChain layer handles it). Either way the
// JIT block returns after this.
//
// If the fast path is disabled (dispatcherAddr==0) this falls back
// to the legacy emitReturn(pc, jitEcall) behavior.
func (e *emitter) emitSyscall(resumePC uint64, dispatcherAddr uintptr) {
	if dispatcherAddr == 0 {
		e.emitReturn(resumePC, jitEcall)
		return
	}
	e.irEm.WriteBackAll()
	e.irEm.Syscall(resumePC, dispatcherAddr)
}

// allocFaultLabel allocates a per-call-site fault label and registers its
// (PC, addrVR, status) tuple so finalize() emits a tail returning the actual
// faulting instruction's PC and the live faulting address. Mirrors the TCC
// emitter's per-call-site `return (JITResult){pc, ic, status, addr}` pattern.
func (e *emitter) allocFaultLabel(addr VReg, status int) Label {
	l := e.irEm.NewLabel()
	e.deferredFaults = append(e.deferredFaults, deferredFault{
		label: l, pc: e.pc, addrVR: addr, status: status,
	})
	return l
}

// emitChainableReturn emits a chain exit for jitOK returns with a
// statically-known successor PC. The lowerer emits MOVABS R10, <sentinel>;
// JMP R10. jit_native backpatches the sentinel to the slow-exit stub; the
// Go dispatcher later patches it again to the target block's chainEntry,
// bypassing the Go round-trip on subsequent entries.
//
// For dynamic-target returns (JALR) use emitReturn / IRRetDyn instead —
// those stay as Go round-trips.
func (e *emitter) emitChainableReturn(pc uint64) {
	e.irEm.WriteBackAll()
	e.irEm.ChainExit(pc, e.exitIdx)
	e.exitIdx++
}

// lastIRWasTerminator reports whether the final IR instruction
// emitted into the current block is a terminator op whose lowered
// x86 unconditionally leaves the block. When true, finalize()'s
// fall-through emitChainableReturn is dead code and is skipped.
//
// Recognised terminators: IRRet, IRRetDyn, IRSyscall, IRChainExit,
// IRJalrIC. Any future IR op whose lowerer unconditionally exits
// the block must be added to this switch.
func (e *emitter) lastIRWasTerminator() bool {
	ins := e.irEm.Block.Instrs
	if len(ins) == 0 {
		return false
	}
	switch ins[len(ins)-1].Op {
	case IRRet, IRRetDyn, IRSyscall, IRChainExit, IRJalrIC:
		return true
	}
	return false
}

func (e *emitter) emitWriteBackAll() {
	e.irEm.WriteBackAll()
}

// emitDivGuarded emits a 64-bit DIV/DIVU/REM/REMU with zero-divisor and
// overflow guards per the RISC-V spec (no x86 fault on divide-by-zero).
func (e *emitter) emitDivGuarded(dst, a, b VReg, signed, wantRem bool) {
	em := e.irEm
	doneLabel := em.NewLabel()
	normalLabel := em.NewLabel()

	// Check divisor == 0.
	zeroLabel := em.NewLabel()
	em.Branch(b, VRegZero, EQ, zeroLabel)

	if signed {
		// Check signed overflow: a == INT64_MIN && b == -1.
		ovfLabel := em.NewLabel()
		tmin := em.Tmp()
		em.Const(tmin, -9223372036854775808) // INT64_MIN
		em.Branch(a, tmin, NE, normalLabel)
		tminus1 := em.Tmp()
		em.Const(tminus1, -1)
		em.Branch(b, tminus1, NE, normalLabel)
		// Overflow path.
		em.PlaceLabel(ovfLabel)
		if wantRem {
			em.Const(dst, 0) // REM overflow → 0
		} else {
			em.Mov(dst, a) // DIV overflow → dividend
		}
		em.Jump(doneLabel)
	} else {
		em.Jump(normalLabel)
	}

	// Zero-divisor path.
	em.PlaceLabel(zeroLabel)
	if wantRem {
		em.Mov(dst, a) // REM(x,0) → x
	} else {
		em.Const(dst, -1) // DIV(x,0) → all-ones
	}
	em.Jump(doneLabel)

	// Normal division.
	em.PlaceLabel(normalLabel)
	if signed {
		if wantRem {
			em.Rem(dst, a, b)
		} else {
			em.DivS(dst, a, b)
		}
	} else {
		if wantRem {
			em.RemU(dst, a, b)
		} else {
			em.DivU(dst, a, b)
		}
	}

	em.PlaceLabel(doneLabel)
}

// emitDivW emits a 32-bit DIVW/DIVUW/REMW/REMUW with guards.
// Operates on lower 32 bits, result is sign-extended to 64 bits.
func (e *emitter) emitDivW(dst, a, b VReg, signed, wantRem bool) {
	em := e.irEm
	doneLabel := em.NewLabel()
	normalLabel := em.NewLabel()

	// Truncate operands to 32 bits.
	a32 := em.Tmp()
	b32 := em.Tmp()
	if signed {
		em.Sext(a32, a, I32)
		em.Sext(b32, b, I32)
	} else {
		em.Zext(a32, a, I32)
		em.Zext(b32, b, I32)
	}

	// Check divisor == 0.
	zeroLabel := em.NewLabel()
	em.Branch(b32, VRegZero, EQ, zeroLabel)

	if signed {
		// Check signed overflow: a32 == INT32_MIN && b32 == -1.
		ovfLabel := em.NewLabel()
		tmin := em.Tmp()
		em.Const(tmin, -2147483648) // INT32_MIN (sign-extended to 64-bit)
		em.Branch(a32, tmin, NE, normalLabel)
		tminus1 := em.Tmp()
		em.Const(tminus1, -1)
		em.Branch(b32, tminus1, NE, normalLabel)
		em.PlaceLabel(ovfLabel)
		if wantRem {
			em.Const(dst, 0)
		} else {
			em.Sext(dst, a32, I32) // dividend, sign-extended
		}
		em.Jump(doneLabel)
	} else {
		em.Jump(normalLabel)
	}

	// Zero-divisor path.
	em.PlaceLabel(zeroLabel)
	if wantRem {
		em.Sext(dst, a32, I32) // REMW(x,0) → x, sign-extended
	} else {
		em.Const(dst, -1) // DIVW(x,0) → all-ones
	}
	em.Jump(doneLabel)

	// Normal.
	em.PlaceLabel(normalLabel)
	t := em.Tmp()
	if signed {
		if wantRem {
			em.Rem(t, a32, b32)
		} else {
			em.DivS(t, a32, b32)
		}
	} else {
		if wantRem {
			em.RemU(t, a32, b32)
		} else {
			em.DivU(t, a32, b32)
		}
	}
	em.Sext(dst, t, I32)

	em.PlaceLabel(doneLabel)
}

// emitFMinMax emits FMIN/FMAX with full RISC-V spec compliance:
//
//	§11.3 NaN: both-NaN → canonical qNaN; one-NaN → the non-NaN operand.
//	§11.6 signed-zero ordering: -0.0 < +0.0.
//
// Emits an FP-typed dst (XMM); caller is responsible for boxing.
func (e *emitter) emitFMinMax(dst, a, b VReg, t Type, isMax bool) {
	em := e.irEm
	lDone := em.NewLabel()

	// 1. NaN handling.
	aIsNum := em.Tmp()
	em.FCmp(aIsNum, a, a, EQ, t) // 1 if not NaN, 0 if NaN
	bIsNum := em.Tmp()
	em.FCmp(bIsNum, b, b, EQ, t)

	lAIsNum := em.NewLabel()
	em.Branch(aIsNum, VRegZero, NE, lAIsNum) // a is number
	// a is NaN.
	lBothNaN := em.NewLabel()
	em.Branch(bIsNum, VRegZero, EQ, lBothNaN) // b NaN too
	// a NaN, b number → dst = b.
	em.Mov(dst, b)
	em.Jump(lDone)
	em.PlaceLabel(lBothNaN)
	// Both NaN → canonical qNaN.
	canonInt := em.Tmp()
	if t == F32 {
		em.Const(canonInt, 0x7FC00000)
		em.MovT(dst, canonInt, F32)
	} else {
		em.Const(canonInt, int64(0x7FF8000000000000))
		em.MovT(dst, canonInt, F64)
	}
	em.Jump(lDone)

	em.PlaceLabel(lAIsNum)
	// a is number. Check b.
	lNumeric := em.NewLabel()
	em.Branch(bIsNum, VRegZero, NE, lNumeric) // both numbers
	// a number, b NaN → dst = a.
	em.Mov(dst, a)
	em.Jump(lDone)

	em.PlaceLabel(lNumeric)
	// 2. Both numeric. Handle ±0 specially, then numeric compare.
	//
	// Test "both operands are zero" via bit-level: (aBits | bBits)
	// with sign bit masked off == 0 ⇔ both are +0 or -0.
	aBits := em.Tmp()
	em.MovT(aBits, a, I64)
	bBits := em.Tmp()
	em.MovT(bBits, b, I64)
	orBits := em.Tmp()
	em.Or(orBits, aBits, bBits)
	absOr := em.Tmp()
	if t == F32 {
		em.AndImm(absOr, orBits, 0x7FFFFFFF)
	} else {
		mask63 := em.Tmp()
		em.Const(mask63, int64(0x7FFFFFFFFFFFFFFF))
		em.And(absOr, orBits, mask63)
	}
	lFPCmp := em.NewLabel()
	em.Branch(absOr, VRegZero, NE, lFPCmp)

	// Both operands are ±0. Spec orders -0 < +0:
	//   FMIN: return -0 if either has sign bit set; else +0.
	//     ⇔ result = aBits | bBits (sign bit set iff either has it).
	//   FMAX: return +0 if either has sign bit clear; else -0.
	//     ⇔ result = aBits & bBits.
	zeroRes := em.Tmp()
	if isMax {
		em.And(zeroRes, aBits, bBits)
	} else {
		em.Or(zeroRes, aBits, bBits)
	}
	em.MovT(dst, zeroRes, t)
	em.Jump(lDone)

	// 3. Regular numeric compare.
	em.PlaceLabel(lFPCmp)
	cmp := em.Tmp()
	if isMax {
		em.FCmp(cmp, a, b, GT, t)
	} else {
		em.FCmp(cmp, a, b, LT, t)
	}
	lPickA := em.NewLabel()
	em.Branch(cmp, VRegZero, NE, lPickA)
	em.Mov(dst, b)
	em.Jump(lDone)
	em.PlaceLabel(lPickA)
	em.Mov(dst, a)

	em.PlaceLabel(lDone)
}

func branchPred(funct3 uint32) (Pred, bool) {
	switch funct3 {
	case 0:
		return EQ, true
	case 1:
		return NE, true
	case 4:
		return LT, true
	case 5:
		return GE, true
	case 6:
		return LTU, true
	case 7:
		return GEU, true
	}
	return 0, false
}

// irLoadInfo returns width and signed for integer loads.
func irLoadInfo(funct3 uint32) (width int, signed bool) {
	switch funct3 {
	case 0:
		return 1, true // LB
	case 1:
		return 2, true // LH
	case 2:
		return 4, true // LW
	case 3:
		return 8, false // LD
	case 4:
		return 1, false // LBU
	case 5:
		return 2, false // LHU
	case 6:
		return 4, false // LWU
	}
	return 0, false
}

// irStoreWidth returns width for integer stores.
func irStoreWidth(funct3 uint32) int {
	switch funct3 {
	case 0:
		return 1
	case 1:
		return 2
	case 2:
		return 4
	case 3:
		return 8
	}
	return 0
}

// ── finalize ───────────────────────────────────────────────────────────

func (e *emitter) finalize() *emitResult {
	// Fall-through return. Emitted only when the last IR is not already a
	// terminator. Blocks that hit a hard terminator (ECALL, EBREAK, JAL
	// rd!=0, JALR, branch patterns) already left via IRSyscall/IRRet/
	// IRChainExit/IRJalrIC/IRRetDyn; adding another fall-through would
	// emit ~47 bytes of unreachable code (store + MOVABS+JMP + slow-exit
	// stub) after the cold RET. Blocks that terminated without emitting a
	// terminator IR (CSR/unknown SYSTEM, unknown opcode, unknown RVC
	// quad) still get a return here so the interpreter fallback path
	// keeps working.
	if !e.lastIRWasTerminator() {
		e.emitChainableReturn(e.pc)
	}

	// Bail labels: goto targets not emitted. Deterministic order is critical —
	// random order changes IR indices, affecting register allocation.
	e.gotoTargets.each(func(target uint64) {
		if !e.visited[target] {
			e.irEm.PlaceLabel(e.getOrCreateLabel(target))
			e.emitChainableReturn(target)
		}
	})

	// Deferred external exits.
	for _, de := range e.deferredExits {
		e.irEm.PlaceLabel(de.label)
		e.emitChainableReturn(de.targetPC)
	}

	// Per-call-site fault tails. Each load/store registered its own
	// (label, pc, addr, status); emit one tail apiece so the dispatch loop
	// receives the faulting instruction's PC and the actual fault address.
	for _, df := range e.deferredFaults {
		e.irEm.PlaceLabel(df.label)
		e.irEm.WriteBackAll()
		e.irEm.Ret(df.pc, df.status, df.addrVR)
	}

	return &emitResult{
		block:         e.irEm.Block,
		startPC:       e.startPC,
		endPC:         e.pc,
		numInsns:      e.numInsns,
		numChainExits: e.exitIdx,
	}
}

// scanUsedRegs does a lightweight decode pass to identify which integer
// registers are referenced (read or written) in the block. Stops at
// terminator instructions (SYSTEM/JALR/JAL-with-link) to match the
// emitter's termination points — scanning past them would mark registers
// from instructions the emitter never emits.
func scanUsedRegs(mem *GuestMemory, startPC, endPC uint64, used *[32]bool) {
	pc := startPC
	for pc < endPC {
		half, fh := mem.Fetch16(pc)
		if fh != nil {
			break
		}
		if half&0x3 != 0x3 {
			// RVC: extract compressed register fields
			insn := uint16(half)
			quad := insn & 0x3
			funct3 := insn >> 13
			switch quad {
			case 0x0:
				rd := 8 + ((insn >> 2) & 7)
				rs1 := 8 + ((insn >> 7) & 7)
				used[rd] = true
				used[rs1] = true
				if funct3 >= 0b101 { // stores: rs2
					rs2 := 8 + ((insn >> 2) & 7)
					used[rs2] = true
				}
			case 0x1:
				switch funct3 {
				case 0b000, 0b001, 0b010: // ADDI/ADDIW/LI
					rd := (insn >> 7) & 0x1F
					if rd != 0 {
						used[rd] = true
					}
				case 0b011: // ADDI16SP/LUI
					rd := (insn >> 7) & 0x1F
					if rd != 0 {
						used[rd] = true
					}
					if rd == 2 {
						used[2] = true
					}
				case 0b100: // MISC-ALU
					rs1 := 8 + ((insn >> 7) & 7)
					rs2 := 8 + ((insn >> 2) & 7)
					used[rs1] = true
					used[rs2] = true
				case 0b110, 0b111: // BEQZ/BNEZ
					rs1 := 8 + ((insn >> 7) & 7)
					used[rs1] = true
				}
			case 0x2:
				rd := (insn >> 7) & 0x1F
				rs2 := (insn >> 2) & 0x1F
				if rd != 0 {
					used[rd] = true
				}
				if rs2 != 0 {
					used[rs2] = true
				}
				used[2] = true // sp used by LWSP/LDSP/SWSP/SDSP
				// C.JR / C.JALR / C.EBREAK terminate the block
				if funct3 == 0b100 {
					bit12 := (insn >> 12) & 1
					if bit12 == 0 && rs2 == 0 { // C.JR
						return
					}
					if bit12 == 1 && rs2 == 0 { // C.JALR or C.EBREAK
						return
					}
				}
			}
			pc += 2
		} else {
			insn, f := mem.Fetch32(pc)
			if f != nil {
				insn, f = mem.Fetch32U(pc)
				if f != nil {
					break
				}
			}
			opcode := insn & 0x7F
			rd := (insn >> 7) & 0x1F
			rs1 := (insn >> 15) & 0x1F
			rs2 := (insn >> 20) & 0x1F
			// Mark rd for opcodes that have a destination register.
			// BRANCH (0x63), STORE (0x23), FP-STORE (0x27) use bits[11:7] for
			// immediate, not rd.
			switch opcode {
			case 0x63, 0x23, 0x27:
				// no rd
			default:
				if rd != 0 {
					used[rd] = true
				}
			}
			// Mark rs1 (most formats use bits[19:15] for rs1).
			// FENCE (0x0F), LUI (0x37), AUIPC (0x17), JAL (0x6F) don't have rs1.
			switch opcode {
			case 0x0F, 0x37, 0x17, 0x6F:
				// no rs1
			default:
				if rs1 != 0 {
					used[rs1] = true
				}
			}
			// Mark rs2 for formats that use it.
			switch opcode {
			case 0x33, 0x3B: // OP, OP-32
				if rs2 != 0 {
					used[rs2] = true
				}
			case 0x63: // BRANCH
				if rs1 != 0 {
					used[rs1] = true
				}
				if rs2 != 0 {
					used[rs2] = true
				}
			case 0x23: // STORE
				if rs1 != 0 {
					used[rs1] = true
				}
				if rs2 != 0 {
					used[rs2] = true
				}
			case 0x27: // FP-STORE
				if rs1 != 0 {
					used[rs1] = true
				}
				// rs2 is FP register index, not int
			case 0x43, 0x47, 0x4B, 0x4F: // FMA
				if rs2 != 0 {
					used[rs2] = true
				}
				rs3 := insn >> 27
				if rs3 != 0 {
					used[rs3] = true
				}
			}
			// Stop at instructions the emitter terminates on.
			switch opcode {
			case 0x73: // SYSTEM (ECALL, EBREAK, CSR) — emitter terminates
				return
			case 0x67: // JALR — emitter terminates
				return
			case 0x6F: // JAL — emitter terminates if rd != 0
				if rd != 0 {
					return
				}
			}
			pc += 4
		}
	}
}

// ── emitBlock ──────────────────────────────────────────────────────────

func (j *JIT) emitBlock(mem *GuestMemory, pc uint64) *emitResult {
	region := scanRegion(mem, pc)
	if region.pcCount == 0 {
		if debugJIT {
			fmt.Fprintf(os.Stderr, "BAIL pc=0x%x reason=scanRegion_empty\n", pc)
		}
		return nil
	}
	return j.emitBlockRange(mem, pc, region.endPC)
}

// emitBlockLinear emits IR for the range [startPC, endPC) without
// running scanRegion's BFS. Used by the AOT linear-scan path where
// block boundaries are already known from enumerateBlockRanges. If
// the range is empty or emission produces zero instructions, returns
// nil (caller's decoder_cache slot stays zero → lazy fallback at run
// time).
func (j *JIT) emitBlockLinear(mem *GuestMemory, startPC, endPC uint64) *emitResult {
	if startPC >= endPC {
		return nil
	}
	return j.emitBlockRange(mem, startPC, endPC)
}

// emitBlockRange walks instructions sequentially from startPC until
// endPC or the first terminator, emitting  Shared between
// emitBlock (BFS-driven endPC via scanRegion) and emitBlockLinear
// (explicit endPC from the AOT enumeration).
func (j *JIT) emitBlockRange(mem *GuestMemory, pc, endPC uint64) *emitResult {
	irEm := NewEmitter(j)

	gt := newU64setSized(256)
	//gt.IterStart = testIterStart

	e := &emitter{
		mem:         mem,
		startPC:     pc,
		pc:          pc,
		irEm:        irEm,
		visited:     make(map[uint64]bool),
		regionEnd:   endPC,
		gotoTargets: gt,
		pcLabels:    newU64labelmap(),
	}

	// Emit IR (populates regsUsed via xreg/xregDst calls).
	for !e.terminated && e.pc < e.regionEnd {
		if e.visited[e.pc] {
			e.irEm.Jump(e.getOrCreateLabel(e.pc))
			e.gotoTargets.add(e.pc)
			e.terminated = true
			break
		}
		e.visited[e.pc] = true

		half, fh := mem.Fetch16(e.pc)
		if fh != nil {
			if debugJIT && e.numInsns == 0 {
				fmt.Fprintf(os.Stderr, "BAIL pc=0x%x reason=fetch16_fault\n", e.pc)
			}
			break
		}

		if half&0x3 != 0x3 {
			e.emitRVC(uint16(half))
		} else {
			insn, f := mem.Fetch32(e.pc)
			if f != nil {
				if f.Kind == FaultMisalign {
					insn, f = mem.Fetch32U(e.pc)
				}
				if f != nil {
					if debugJIT && e.numInsns == 0 {
						fmt.Fprintf(os.Stderr, "BAIL pc=0x%x reason=fetch32_fault\n", e.pc)
					}
					break
				}
			}
			e.emit32(insn)
		}
	}

	if e.numInsns == 0 {
		if debugJIT {
			half, _ := mem.Fetch16(pc)
			fmt.Fprintf(os.Stderr, "BAIL pc=0x%x reason=numInsns_zero regionEnd=0x%x half=0x%04x terminated=%v\n",
				pc, endPC, half, e.terminated)
		}
		return nil
	}

	// Prepend loads for every register referenced during emission.
	// This deliberately loads some regs whose block-entry value is dead
	// (e.g. `li a7, 93` with no prior use of a7) — see the comment on
	// emitter.regsUsed for why the apparent waste is load-bearing.
	// Must run AFTER emission so regsUsed is fully populated.
	var loads []IRInstr
	for i := uint32(1); i < 32; i++ {
		if e.regsUsed&(uint32(1)<<i) != 0 {
			loads = append(loads, IRInstr{
				Op: IRLoad, T: I64,
				Dst: VReg(i), A: irEm.XBase(),
				Imm: int64(i) * 8,
			})
		}
	}
	if len(loads) > 0 {
		e.irEm.Block.Instrs = append(loads, e.irEm.Block.Instrs...)
		// Fix label indices (they shifted by len(loads)).
		for lab, idx := range e.irEm.Block.Labels {
			e.irEm.Block.Labels[lab] = idx + len(loads)
		}
		MaxVReg(e.irEm.Block)
	}

	return e.finalize()
}

// ── 32-bit instruction emitter ─────────────────────────────────────────

func (e *emitter) emit32(insn uint32) {
	opcode := insn & 0x7F
	rd := (insn >> 7) & 0x1F
	funct3 := (insn >> 12) & 0x7
	rs1 := (insn >> 15) & 0x1F
	rs2 := (insn >> 20) & 0x1F
	funct7 := insn >> 25
	iimm := int64(int32(insn)) >> 20

	e.emitLabel()

	switch opcode {
	case 0x37: // LUI
		uimm := int64(int32(insn & 0xFFFFF000))
		if rd != 0 {
			e.irEm.Const(e.xregDst(rd), uimm)
		}
		e.advancePC(4)

	case 0x17: // AUIPC
		uimm := int64(int32(insn & 0xFFFFF000))
		addr := int64(e.pc) + uimm

		// Macro-op fusion: peek at the next instruction for AUIPC+X pairs.
		if rd != 0 {
			if next, ok := e.peek32(e.pc + 4); ok {
				nextOp := next & 0x7F
				nextRd := (next >> 7) & 0x1F
				nextRs1 := (next >> 15) & 0x1F
				nextImm := int64(int32(next)) >> 20

				switch {
				case nextOp == 0x13 && (next>>12)&7 == 0 && nextRd == rd && nextRs1 == rd:
					// AUIPC+ADDI → la (load address): single Const.
					e.irEm.Const(e.xregDst(rd), addr+nextImm)
					e.advancePC(4)
					e.advancePC(4)
					return

				case nextOp == 0x67 && nextRs1 == rd:
					// AUIPC+JALR → direct call with known target.
					target := addr + nextImm
					e.irEm.Const(e.xregDst(rd), int64(e.pc)+4)
					e.advancePC(4)
					e.emitJALR(nextRd, rd, target-int64(e.pc), 4)
					return

				case nextOp == 0x03 && nextRs1 == rd && nextRd == rd:
					// AUIPC+LOAD → load from absolute guest address.
					e.advancePC(4)
					e.advancePC(4)
					funct3 := (next >> 12) & 7
					e.emitLoadFused(rd, addr+nextImm, funct3)
					return
				}
			}
			e.irEm.Const(e.xregDst(rd), addr)
		}
		e.advancePC(4)

	case 0x6F: // JAL
		jimm := jImm(insn)
		e.emitJAL(rd, jimm, 4)

	case 0x67: // JALR
		e.emitJALR(rd, rs1, iimm, 4)

	case 0x63: // BRANCH
		bimm := bImm(insn)
		_, ok := branchPred(funct3)
		if !ok {
			e.terminated = true
			e.advancePC(4)
			break
		}
		e.emitBranch(rs1, rs2, funct3, bimm)
		e.advancePC(4)

	case 0x03: // LOAD
		e.emitLoad(rd, rs1, iimm, funct3)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x23: // STORE
		simm := sImm(insn)
		e.emitStore(rs1, rs2, simm, funct3)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x13: // OP-IMM
		e.emitOpImm(rd, rs1, iimm, funct3, funct7)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x1B: // OP-IMM-32
		e.emitOpImm32(rd, rs1, iimm, funct3, funct7)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x33: // OP
		e.emitOp(rd, rs1, rs2, funct3, funct7)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x3B: // OP-32
		e.emitOp32(rd, rs1, rs2, funct3, funct7)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x07: // FP LOAD
		e.emitFPLoad(rd, rs1, iimm, funct3)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x27: // FP STORE
		simm := sImm(insn)
		e.emitFPStore(rs1, rs2, simm, funct3)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x43, 0x47, 0x4B, 0x4F: // FMA
		rs3 := insn >> 27
		fpfmt := (insn >> 25) & 0x3
		e.emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x53: // FP-OP
		funct5 := insn >> 27
		fpfmt := (insn >> 25) & 0x3
		e.emitFPOp(rd, rs1, rs2, funct3, funct5, fpfmt)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x0F: // FENCE
		e.advancePC(4)

	case 0x73: // SYSTEM
		switch insn {
		case 0x00000073: // ECALL — always terminates the block.
			// Under Option D the post-ECALL PC is a separate AOT block
			// entry (registered by aot.go's termFT), and lowerSyscall
			// chain-exits into it on the hot path when the flag is on.
			e.advancePC(4)
			e.emitSyscall(e.pc, currentSyscallDispatcherAddr())
			e.terminated = true
		case 0x00100073: // EBREAK
			e.advancePC(4)
			e.emitReturn(e.pc, jitEbreak)
			e.terminated = true
		default:
			// CSR or unknown SYSTEM — end block before this instruction.
			// The interpreter will handle it via fallback.
			e.terminated = true
		}

	default:
		// Unknown opcode — end block before this instruction.
		e.terminated = true
	}
}

// ── OP-IMM ─────────────────────────────────────────────────────────────

func (e *emitter) emitOpImm(rd, rs1 uint32, imm int64, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	shamt := imm & 63

	switch funct3 {
	case 0: // ADDI
		if imm == 0 && rs1 == 0 {
			e.irEm.Const(e.xregDst(rd), 0)
		} else if imm == 0 {
			src := e.xreg(rs1) // read before write (aliasing!)
			e.irEm.Mov(e.xregDst(rd), src)
		} else if rs1 == 0 {
			e.irEm.Const(e.xregDst(rd), imm)
		} else {
			src := e.xreg(rs1) // read before write (aliasing!)
			e.irEm.AddImm(e.xregDst(rd), src, imm)
		}
	case 1: // SLLI / BSETI / BCLRI / BINVI / CLZ/CTZ/CPOP/SEXT
		funct6 := funct7 >> 1
		switch funct6 {
		case 0x00: // SLLI
			// Fusion: SLLI rd, rs1, 32; SRLI rd, rd, 32 → zext.w
			if shamt == 32 && rd == rs1 {
				if next, ok := e.peek32(e.pc + 4); ok {
					nOp := next & 0x7F
					nRd := (next >> 7) & 0x1F
					nRs1 := (next >> 15) & 0x1F
					nF3 := (next >> 12) & 7
					nF6 := next >> 26
					nShamt := int64((next >> 20) & 0x3F)
					if nOp == 0x13 && nF3 == 5 && nF6 == 0 && nShamt == 32 && nRd == rd && nRs1 == rd {
						e.irEm.Zext(e.xregDst(rd), e.xreg(rs1), I32)
						e.advancePC(4)
						e.advancePC(4)
						return
					}
				}
			}
			src := e.xreg(rs1)
			e.irEm.ShlImm(e.xregDst(rd), src, shamt)
		case 0x0A: // BSETI
			src := e.xreg(rs1)
			t := e.irEm.Tmp()
			e.irEm.Const(t, int64(1)<<shamt)
			e.irEm.Or(e.xregDst(rd), src, t)
		case 0x12: // BCLRI
			src := e.xreg(rs1)
			e.irEm.AndImm(e.xregDst(rd), src, ^(int64(1) << shamt))
		case 0x1A: // BINVI
			src := e.xreg(rs1)
			t := e.irEm.Tmp()
			e.irEm.Const(t, int64(1)<<shamt)
			e.irEm.Xor(e.xregDst(rd), src, t)
		case 0x30: // CLZ/CTZ/CPOP/SEXT.B/SEXT.H
			switch shamt {
			case 0: // CLZ
				src := e.xreg(rs1)
				e.irEm.Clz(e.xregDst(rd), src, I64)
			case 1: // CTZ
				src := e.xreg(rs1)
				e.irEm.Ctz(e.xregDst(rd), src, I64)
			case 2: // CPOP
				src := e.xreg(rs1)
				e.irEm.Popcount(e.xregDst(rd), src, I64)
			case 0x22: // SEXT.B
				src := e.xreg(rs1)
				e.irEm.Sext(e.xregDst(rd), src, I8)
			case 0x23: // SEXT.H
				src := e.xreg(rs1)
				e.irEm.Sext(e.xregDst(rd), src, I16)
			default:
				e.terminated = true
			}
		default:
			e.terminated = true
		}
	case 2: // SLTI
		src := e.xreg(rs1)
		e.irEm.SetImm(e.xregDst(rd), src, imm, LT)
	case 3: // SLTIU
		src := e.xreg(rs1)
		e.irEm.SetImm(e.xregDst(rd), src, imm, LTU)
	case 4: // XORI
		src := e.xreg(rs1)
		e.irEm.XorImm(e.xregDst(rd), src, imm)
	case 5: // SRLI/SRAI / BEXTI / RORI / ORC.B / REV8 / ZEXT.H
		funct6 := funct7 >> 1
		switch funct6 {
		case 0x00: // SRLI
			src := e.xreg(rs1)
			e.irEm.ShrImm(e.xregDst(rd), src, shamt)
		case 0x10: // SRAI
			src := e.xreg(rs1)
			e.irEm.SarImm(e.xregDst(rd), src, shamt)
		case 0x12: // BEXTI
			t := e.irEm.Tmp()
			e.irEm.ShrImm(t, e.xreg(rs1), shamt)
			e.irEm.AndImm(e.xregDst(rd), t, 1)
		case 0x18: // RORI
			t1 := e.irEm.Tmp()
			e.irEm.ShrImm(t1, e.xreg(rs1), shamt)
			t2 := e.irEm.Tmp()
			e.irEm.ShlImm(t2, e.xreg(rs1), 64-shamt)
			e.irEm.Or(e.xregDst(rd), t1, t2)
		case 0x0A: // ORC.B — each byte becomes 0xFF if nonzero, 0x00 if zero
			src := e.xreg(rs1)
			dst := e.xregDst(rd)
			e.irEm.Const(dst, 0)
			for i := 0; i < 8; i++ {
				byteVal := e.irEm.Tmp()
				e.irEm.ShrImm(byteVal, src, int64(i*8))
				e.irEm.AndImm(byteVal, byteVal, 0xFF)
				mask := e.irEm.Tmp()
				// mask = (byteVal != 0) ? 0xFF : 0
				ne := e.irEm.Tmp()
				e.irEm.Set(ne, byteVal, VRegZero, NE)
				e.irEm.Neg(mask, ne)            // 0→0, 1→-1 (0xFFFF...)
				e.irEm.AndImm(mask, mask, 0xFF) // keep only low byte = 0xFF or 0
				e.irEm.ShlImm(mask, mask, int64(i*8))
				e.irEm.Or(dst, dst, mask)
			}
		case 0x1A: // REV8 — byte-swap via BSWAP
			src := e.xreg(rs1)
			e.irEm.Bswap(e.xregDst(rd), src)
		case 0x02: // ZEXT.H
			src := e.xreg(rs1)
			e.irEm.Zext(e.xregDst(rd), src, I16)
		default:
			e.terminated = true
		}
	case 6: // ORI
		src := e.xreg(rs1)
		e.irEm.OrImm(e.xregDst(rd), src, imm)
	case 7: // ANDI
		src := e.xreg(rs1)
		e.irEm.AndImm(e.xregDst(rd), src, imm)
	}
}

// ── OP-IMM-32 ──────────────────────────────────────────────────────────

func (e *emitter) emitOpImm32(rd, rs1 uint32, imm int64, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	shamt := imm & 31

	switch funct3 {
	case 0: // ADDIW
		// Fusion: ADDIW rd,rs1,imm; SLLI rd,rd,32; SRLI rd,rd,32 → addiwz
		// (32-bit add with zero-extension instead of sign-extension)
		if n1, ok1 := e.peek32(e.pc + 4); ok1 {
			if n2, ok2 := e.peek32(e.pc + 8); ok2 {
				n1Op, n1Rd, n1Rs1 := n1&0x7F, (n1>>7)&0x1F, (n1>>15)&0x1F
				n1F3, n1F6 := (n1>>12)&7, n1>>26
				n1Shamt := int64((n1 >> 20) & 0x3F)
				n2Op, n2Rd, n2Rs1 := n2&0x7F, (n2>>7)&0x1F, (n2>>15)&0x1F
				n2F3, n2F6 := (n2>>12)&7, n2>>26
				n2Shamt := int64((n2 >> 20) & 0x3F)
				if n1Op == 0x13 && n1F3 == 1 && n1F6 == 0 && n1Shamt == 32 &&
					n1Rd == rd && n1Rs1 == rd &&
					n2Op == 0x13 && n2F3 == 5 && n2F6 == 0 && n2Shamt == 32 &&
					n2Rd == rd && n2Rs1 == rd {
					src := e.xreg(rs1)
					dst := e.xregDst(rd)
					if imm == 0 {
						e.irEm.Zext(dst, src, I32)
					} else {
						t := e.irEm.Tmp()
						e.irEm.AddImm(t, src, imm)
						e.irEm.Zext(dst, t, I32)
					}
					e.advancePC(4) // consumed SLLI
					e.advancePC(4) // consumed SRLI
					return
				}
			}
		}
		src := e.xreg(rs1)
		dst := e.xregDst(rd)
		if imm == 0 {
			// SEXT.W
			e.irEm.Sext(dst, src, I32)
		} else {
			t := e.irEm.Tmp()
			e.irEm.AddImm(t, src, imm)
			e.irEm.Sext(dst, t, I32)
		}
	case 1: // SLLIW / SLLI.UW
		if funct7 == 0x04 { // SLLI.UW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32)
			e.irEm.ShlImm(e.xregDst(rd), t, shamt)
		} else { // SLLIW
			t := e.irEm.Tmp()
			e.irEm.ShlImm(t, e.xreg(rs1), shamt)
			e.irEm.Sext(e.xregDst(rd), t, I32)
		}
	case 5: // SRLIW / SRAIW / RORIW
		switch funct7 >> 1 {
		case 0x00: // SRLIW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32) // zero-extend to get uint32
			e.irEm.ShrImm(t, t, shamt)
			e.irEm.Sext(e.xregDst(rd), t, I32)
		case 0x10: // SRAIW
			t := e.irEm.Tmp()
			e.irEm.Sext(t, e.xreg(rs1), I32) // sign-extend to get int32
			e.irEm.SarImm(t, t, shamt)
			e.irEm.Sext(e.xregDst(rd), t, I32)
		case 0x30: // RORIW — word rotate right immediate
			src := e.xreg(rs1)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, src, I32)
			t1 := e.irEm.Tmp()
			e.irEm.ShrImm(t1, t, shamt)
			t2 := e.irEm.Tmp()
			e.irEm.ShlImm(t2, t, 32-shamt)
			e.irEm.Or(t1, t1, t2)
			e.irEm.Sext(e.xregDst(rd), t1, I32)
		default:
			e.terminated = true
		}
	default:
		e.terminated = true
	}
}

// ── OP (R-type) ────────────────────────────────────────────────────────

func (e *emitter) emitOp(rd, rs1, rs2, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	a := e.xreg(rs1)
	b := e.xreg(rs2)
	dst := e.xregDst(rd)

	switch funct7 {
	case 0x00: // base RV64I
		switch funct3 {
		case 0:
			e.irEm.Add(dst, a, b)
		case 1:
			e.irEm.Shl(dst, a, b)
		case 2:
			e.irEm.Set(dst, a, b, LT)
		case 3:
			e.irEm.Set(dst, a, b, LTU)
		case 4:
			e.irEm.Xor(dst, a, b)
		case 5:
			e.irEm.Shr(dst, a, b)
		case 6:
			e.irEm.Or(dst, a, b)
		case 7:
			e.irEm.And(dst, a, b)
		}
	case 0x20: // SUB / SRA / Zbb
		switch funct3 {
		case 0:
			e.irEm.Sub(dst, a, b) // SUB
		case 5:
			e.irEm.Sar(dst, a, b) // SRA
		case 4: // XNOR
			t := e.irEm.Tmp()
			e.irEm.Xor(t, a, b)
			e.irEm.Not(dst, t)
		case 6: // ORN
			t := e.irEm.Tmp()
			e.irEm.Not(t, b)
			e.irEm.Or(dst, a, t)
		case 7: // ANDN
			t := e.irEm.Tmp()
			e.irEm.Not(t, b)
			e.irEm.And(dst, a, t)
		}
	case 0x01: // M extension
		switch funct3 {
		case 0:
			e.irEm.Mul(dst, a, b)
		case 1:
			e.irEm.MulHS(dst, a, b) // no longer bails!
		case 2:
			e.irEm.MulHSU(dst, a, b)
		case 3:
			e.irEm.MulHU(dst, a, b)
		case 4: // DIV — guarded: div-by-zero → -1, overflow → dividend
			e.emitDivGuarded(dst, a, b, true, false)
		case 5: // DIVU — guarded: div-by-zero → MAX_UINT
			e.emitDivGuarded(dst, a, b, false, false)
		case 6: // REM — guarded: div-by-zero → dividend, overflow → 0
			e.emitDivGuarded(dst, a, b, true, true)
		case 7: // REMU — guarded: div-by-zero → dividend
			e.emitDivGuarded(dst, a, b, false, true)
		}
	case 0x04: // Zbb: ZEXT.H (R-type encoding funct7=0x04, funct3 can vary)
		e.irEm.Zext(dst, a, I16)
	case 0x05: // MIN/MAX (Zbb) + CLMUL (Zbc)
		switch funct3 {
		case 4: // MIN
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, LT)
			e.irEm.Branch(t, VRegZero, NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 5: // MINU
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, LTU)
			e.irEm.Branch(t, VRegZero, NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 6: // MAX
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, GT)
			e.irEm.Branch(t, VRegZero, NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 7: // MAXU
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, GTU)
			e.irEm.Branch(t, VRegZero, NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		default:
			e.terminated = true
		}
	case 0x07: // Zicond
		switch funct3 {
		case 5: // CZERO.EQZ: d = (b == 0) ? 0 : a
			skip := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			e.irEm.Branch(b, VRegZero, NE, skip)
			e.irEm.Const(dst, 0)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(skip)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 7: // CZERO.NEZ: d = (b != 0) ? 0 : a
			skip := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			e.irEm.Branch(b, VRegZero, EQ, skip)
			e.irEm.Const(dst, 0)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(skip)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		default:
			e.terminated = true
		}
	case 0x10: // Zba: SH1ADD/SH2ADD/SH3ADD
		switch funct3 {
		case 2: // SH1ADD
			t := e.irEm.Tmp()
			e.irEm.ShlImm(t, a, 1)
			e.irEm.Add(dst, b, t)
		case 4: // SH2ADD
			t := e.irEm.Tmp()
			e.irEm.ShlImm(t, a, 2)
			e.irEm.Add(dst, b, t)
		case 6: // SH3ADD
			t := e.irEm.Tmp()
			e.irEm.ShlImm(t, a, 3)
			e.irEm.Add(dst, b, t)
		default:
			e.terminated = true
		}
	case 0x14: // Zbs: BSET
		switch funct3 {
		case 1: // BSET
			t := e.irEm.Tmp()
			one := e.irEm.Tmp()
			e.irEm.Const(one, 1)
			e.irEm.Shl(t, one, b)
			e.irEm.Or(dst, a, t)
		default:
			e.terminated = true
		}
	case 0x24: // Zbs: BCLR/BEXT
		switch funct3 {
		case 1: // BCLR
			t := e.irEm.Tmp()
			one := e.irEm.Tmp()
			e.irEm.Const(one, 1)
			e.irEm.Shl(t, one, b)
			e.irEm.Not(t, t)
			e.irEm.And(dst, a, t)
		case 5: // BEXT
			t := e.irEm.Tmp()
			e.irEm.Shr(t, a, b)
			e.irEm.AndImm(dst, t, 1)
		default:
			e.terminated = true
		}
	case 0x30: // Zbb: ROL/ROR
		switch funct3 {
		case 1: // ROL
			t1 := e.irEm.Tmp()
			e.irEm.Shl(t1, a, b)
			sub := e.irEm.Tmp()
			e.irEm.Const(sub, 64)
			e.irEm.Sub(sub, sub, b)
			t2 := e.irEm.Tmp()
			e.irEm.Shr(t2, a, sub)
			e.irEm.Or(dst, t1, t2)
		case 5: // ROR
			t1 := e.irEm.Tmp()
			e.irEm.Shr(t1, a, b)
			sub := e.irEm.Tmp()
			e.irEm.Const(sub, 64)
			e.irEm.Sub(sub, sub, b)
			t2 := e.irEm.Tmp()
			e.irEm.Shl(t2, a, sub)
			e.irEm.Or(dst, t1, t2)
		default:
			e.terminated = true
		}
	case 0x34: // Zbs: BINV
		t := e.irEm.Tmp()
		one := e.irEm.Tmp()
		e.irEm.Const(one, 1)
		e.irEm.Shl(t, one, b)
		e.irEm.Xor(dst, a, t)
	default:
		e.terminated = true
	}
}

// ── OP-32 ──────────────────────────────────────────────────────────────

func (e *emitter) emitOp32(rd, rs1, rs2, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	a := e.xreg(rs1)
	b := e.xreg(rs2)
	dst := e.xregDst(rd)

	switch funct7 {
	case 0x00:
		switch funct3 {
		case 0: // ADDW
			t := e.irEm.Tmp()
			e.irEm.Add(t, a, b)
			e.irEm.Sext(dst, t, I32)
		case 1: // SLLW — shift amount masked to 5 bits (not 6)
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.Shl(t, t, shamt)
			e.irEm.Sext(dst, t, I32)
		case 5: // SRLW — shift amount masked to 5 bits
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.Shr(t, t, shamt)
			e.irEm.Sext(dst, t, I32)
		default:
			e.terminated = true
		}
	case 0x20:
		switch funct3 {
		case 0: // SUBW
			t := e.irEm.Tmp()
			e.irEm.Sub(t, a, b)
			e.irEm.Sext(dst, t, I32)
		case 5: // SRAW — shift amount masked to 5 bits
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			t := e.irEm.Tmp()
			e.irEm.Sext(t, a, I32)
			e.irEm.Sar(t, t, shamt)
			e.irEm.Sext(dst, t, I32)
		default:
			e.terminated = true
		}
	case 0x01: // M extension (word)
		switch funct3 {
		case 0: // MULW
			t := e.irEm.Tmp()
			e.irEm.Mul(t, a, b)
			e.irEm.Sext(dst, t, I32)
		case 4: // DIVW — guarded 32-bit signed division
			e.emitDivW(dst, a, b, true, false)
		case 5: // DIVUW — guarded 32-bit unsigned division
			e.emitDivW(dst, a, b, false, false)
		case 6: // REMW — guarded 32-bit signed remainder
			e.emitDivW(dst, a, b, true, true)
		case 7: // REMUW — guarded 32-bit unsigned remainder
			e.emitDivW(dst, a, b, false, true)
		default:
			e.terminated = true
		}
	case 0x04: // Zba: ADD.UW
		t := e.irEm.Tmp()
		e.irEm.Zext(t, a, I32)
		e.irEm.Add(dst, b, t)
	case 0x30: // Zbb: ROLW/RORW
		switch funct3 {
		case 1: // ROLW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			t1 := e.irEm.Tmp()
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			e.irEm.Shl(t1, t, shamt)
			sub := e.irEm.Tmp()
			e.irEm.Const(sub, 32)
			e.irEm.Sub(sub, sub, shamt)
			t2 := e.irEm.Tmp()
			e.irEm.Shr(t2, t, sub)
			e.irEm.Or(t1, t1, t2)
			e.irEm.Sext(dst, t1, I32)
		case 5: // RORW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			t1 := e.irEm.Tmp()
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			e.irEm.Shr(t1, t, shamt)
			sub := e.irEm.Tmp()
			e.irEm.Const(sub, 32)
			e.irEm.Sub(sub, sub, shamt)
			t2 := e.irEm.Tmp()
			e.irEm.Shl(t2, t, sub)
			e.irEm.Or(t1, t1, t2)
			e.irEm.Sext(dst, t1, I32)
		default:
			e.terminated = true
		}
	case 0x60: // Zbb: CLZW/CTZW/CPOPW
		switch funct3 {
		case 0: // CLZW (rs2 encoding = 0)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.Clz(dst, t, I32)
		case 1: // CTZW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.Ctz(dst, t, I32)
		case 2: // CPOPW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.Popcount(dst, t, I32)
		default:
			e.terminated = true
		}
	case 0x10: // Zba: SH1ADD.UW / SH2ADD.UW / SH3ADD.UW
		switch funct3 {
		case 2:
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.ShlImm(t, t, 1)
			e.irEm.Add(dst, b, t)
		case 4:
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.ShlImm(t, t, 2)
			e.irEm.Add(dst, b, t)
		case 6:
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.ShlImm(t, t, 3)
			e.irEm.Add(dst, b, t)
		default:
			e.terminated = true
		}
	default:
		e.terminated = true
	}
}

// ── LOAD ───────────────────────────────────────────────────────────────

func (e *emitter) emitLoad(rd, rs1 uint32, imm int64, funct3 uint32) {
	width, signed := irLoadInfo(funct3)
	if width == 0 {
		e.terminated = true
		return
	}
	if rd == 0 {
		return // load to x0 is a NOP
	}
	base := e.xreg(rs1)
	dst := e.xregDst(rd)

	addr := e.irEm.Tmp()
	e.irEm.AddImm(addr, base, imm)
	faultLabel := e.allocFaultLabel(addr, jitLoadFault)
	if width > 1 {
		alignedLabel := e.irEm.NewLabel()
		doneLabel := e.irEm.NewLabel()
		alignBits := e.irEm.Tmp()
		e.irEm.AndImm(alignBits, addr, int64(width-1))
		e.irEm.Branch(alignBits, VRegZero, EQ, alignedLabel)
		// OOB check for misaligned path (same as MaskedLoadAddr does for aligned).
		if CheckSandboxBounds {
			e.emitOOBCheck(addr, width, faultLabel)
		}
		t := WidthToType(width)
		e.irEm.MisalignedLoad(dst, addr, t)
		if signed {
			switch width {
			case 2:
				e.irEm.Sext(dst, dst, I16)
			case 4:
				e.irEm.Sext(dst, dst, I32)
			}
		}
		e.irEm.Jump(doneLabel)
		e.irEm.PlaceLabel(alignedLabel)
		e.irEm.MaskedLoadAddr(dst, addr, e.irEm.MemBase(), e.irEm.MemMask(), width, signed, faultLabel)
		e.irEm.PlaceLabel(doneLabel)
	} else {
		e.irEm.MaskedLoadAddr(dst, addr, e.irEm.MemBase(), e.irEm.MemMask(), width, signed, faultLabel)
	}
}

// emitOOBCheck emits (addr | (addr+width-1)) & ~mask != 0 → goto faultLabel.
// Only emitted when CheckSandboxBounds is on; the fast path skips this.
func (e *emitter) emitOOBCheck(addr VReg, width int, faultLabel Label) {
	mask := e.irEm.MemMask()
	endAddr := addr
	if width > 1 {
		endAddr = e.irEm.Tmp()
		e.irEm.AddImm(endAddr, addr, int64(width-1))
		e.irEm.Or(endAddr, addr, endAddr)
	}
	maskNot := e.irEm.Tmp()
	e.irEm.Not(maskNot, mask)
	oob := e.irEm.Tmp()
	e.irEm.And(oob, endAddr, maskNot)
	e.irEm.Branch(oob, VRegZero, NE, faultLabel)
}

// emitMisalignedLoad emits byte-by-byte loads for a misaligned address.
// addr is a VReg holding the guest virtual address (already computed).
// faultLabel is the per-call-site fault tail (already registered with addr).
func (e *emitter) emitMisalignedLoad(dst VReg, addr VReg, width int, signed bool, faultLabel Label) {
	memBase := e.irEm.MemBase()
	mask := e.irEm.MemMask()

	// OOB check for the full range: (addr | (addr+width-1)) & ~mask
	tmp1 := e.irEm.Tmp()
	e.irEm.AddImm(tmp1, addr, int64(width-1))
	e.irEm.Or(tmp1, addr, tmp1)
	maskNot := e.irEm.Tmp()
	e.irEm.Not(maskNot, mask)
	e.irEm.And(tmp1, tmp1, maskNot)
	e.irEm.Branch(tmp1, VRegZero, NE, faultLabel)

	// Load byte 0.
	m0 := e.irEm.Tmp()
	e.irEm.And(m0, addr, mask)
	h0 := e.irEm.Tmp()
	e.irEm.Add(h0, memBase, m0)
	e.irEm.Load(dst, h0, 0, I8, false) // zero-extend byte

	// Load remaining bytes and OR them in.
	for i := 1; i < width; i++ {
		ai := e.irEm.Tmp()
		e.irEm.AddImm(ai, addr, int64(i))
		mi := e.irEm.Tmp()
		e.irEm.And(mi, ai, mask)
		hi := e.irEm.Tmp()
		e.irEm.Add(hi, memBase, mi)
		bi := e.irEm.Tmp()
		e.irEm.Load(bi, hi, 0, I8, false)
		shifted := e.irEm.Tmp()
		e.irEm.ShlImm(shifted, bi, int64(i*8))
		e.irEm.Or(dst, dst, shifted)
	}

	// Sign-extend if needed.
	if signed {
		switch width {
		case 2:
			e.irEm.Sext(dst, dst, I16)
		case 4:
			e.irEm.Sext(dst, dst, I32)
		}
	}
}

// ── STORE ──────────────────────────────────────────────────────────────

func (e *emitter) emitStore(rs1, rs2 uint32, imm int64, funct3 uint32) {
	width := irStoreWidth(funct3)
	if width == 0 {
		e.terminated = true
		return
	}
	base := e.xreg(rs1)
	src := e.xreg(rs2)

	addr := e.irEm.Tmp()
	e.irEm.AddImm(addr, base, imm)
	faultLabel := e.allocFaultLabel(addr, jitStoreFault)
	if width > 1 {
		alignedLabel := e.irEm.NewLabel()
		doneLabel := e.irEm.NewLabel()
		alignBits := e.irEm.Tmp()
		e.irEm.AndImm(alignBits, addr, int64(width-1))
		e.irEm.Branch(alignBits, VRegZero, EQ, alignedLabel)
		if CheckSandboxBounds {
			e.emitOOBCheck(addr, width, faultLabel)
		}
		t := WidthToType(width)
		e.irEm.MisalignedStore(addr, src, t)
		e.irEm.Jump(doneLabel)
		e.irEm.PlaceLabel(alignedLabel)
		e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), src, width, faultLabel)
		e.irEm.PlaceLabel(doneLabel)
	} else {
		e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), src, width, faultLabel)
	}
}

// emitMisalignedStore emits byte-by-byte stores for a misaligned address.
// faultLabel is the per-call-site fault tail (already registered with addr).
func (e *emitter) emitMisalignedStore(addr, src VReg, width int, faultLabel Label) {
	memBase := e.irEm.MemBase()
	mask := e.irEm.MemMask()

	// OOB check for the full range.
	tmp1 := e.irEm.Tmp()
	e.irEm.AddImm(tmp1, addr, int64(width-1))
	e.irEm.Or(tmp1, addr, tmp1)
	maskNot := e.irEm.Tmp()
	e.irEm.Not(maskNot, mask)
	e.irEm.And(tmp1, tmp1, maskNot)
	e.irEm.Branch(tmp1, VRegZero, NE, faultLabel)

	// Store each byte. Byte 0 reuses addr directly (no AddImm by 0).
	for i := 0; i < width; i++ {
		ai := addr
		if i > 0 {
			ai = e.irEm.Tmp()
			e.irEm.AddImm(ai, addr, int64(i))
		}
		mi := e.irEm.Tmp()
		e.irEm.And(mi, ai, mask)
		hi := e.irEm.Tmp()
		e.irEm.Add(hi, memBase, mi)
		bi := e.irEm.Tmp()
		if i == 0 {
			e.irEm.AndImm(bi, src, 0xFF)
		} else {
			e.irEm.ShrImm(bi, src, int64(i*8))
			e.irEm.AndImm(bi, bi, 0xFF)
		}
		e.irEm.Store(hi, 0, bi, I8)
	}
}

// ── FP LOAD ────────────────────────────────────────────────────────────

func (e *emitter) emitFPLoad(rd, rs1 uint32, imm int64, funct3 uint32) {
	var width int
	switch funct3 {
	case 2:
		width = 4
	case 3:
		width = 8
	default:
		e.terminated = true
		return
	}

	base := e.xreg(rs1)
	addr := e.irEm.Tmp()
	e.irEm.AddImm(addr, base, imm)
	faultLabel := e.allocFaultLabel(addr, jitLoadFault)

	alignedLabel := e.irEm.NewLabel()
	doneLabel := e.irEm.NewLabel()
	alignBits := e.irEm.Tmp()
	e.irEm.AndImm(alignBits, addr, int64(width-1))
	e.irEm.Branch(alignBits, VRegZero, EQ, alignedLabel)

	// Misaligned path: OOB check, then byte-by-byte load, then NaN-box if FLW.
	if CheckSandboxBounds {
		e.emitOOBCheck(addr, width, faultLabel)
	}
	t := WidthToType(width)
	tmp := e.irEm.Tmp()
	e.irEm.MisalignedLoad(tmp, addr, t)
	if funct3 == 2 {
		e.boxF32(rd, tmp)
	} else {
		e.irEm.Mov(e.fregDst(rd), tmp)
	}
	e.irEm.Jump(doneLabel)

	// Aligned path.
	e.irEm.PlaceLabel(alignedLabel)
	if funct3 == 2 {
		tmp2 := e.irEm.Tmp()
		e.irEm.MaskedLoadAddr(tmp2, addr, e.irEm.MemBase(), e.irEm.MemMask(), 4, false, faultLabel)
		e.boxF32(rd, tmp2)
	} else {
		e.irEm.MaskedLoadAddr(e.fregDst(rd), addr, e.irEm.MemBase(), e.irEm.MemMask(), 8, false, faultLabel)
	}
	e.irEm.PlaceLabel(doneLabel)
}

// emitMisalignedFPLoad emits a byte-by-byte FP load (FLW or FLD) at a
// misaligned address. faultLabel is the per-call-site fault tail.
func (e *emitter) emitMisalignedFPLoad(rd uint32, addr VReg, width int, faultLabel Label) {
	tmp := e.irEm.Tmp()
	e.emitMisalignedLoad(tmp, addr, width, false, faultLabel)
	if width == 4 { // FLW: NaN-box low 32 bits into f[rd]
		e.boxF32(rd, tmp)
	} else { // FLD: copy raw bits to f[rd]
		e.irEm.Mov(e.fregDst(rd), tmp)
	}
}

// ── FP STORE ───────────────────────────────────────────────────────────

func (e *emitter) emitFPStore(rs1, rs2 uint32, imm int64, funct3 uint32) {
	var width int
	switch funct3 {
	case 2:
		width = 4
	case 3:
		width = 8
	default:
		e.terminated = true
		return
	}

	base := e.xreg(rs1)
	addr := e.irEm.Tmp()
	e.irEm.AddImm(addr, base, imm)
	faultLabel := e.allocFaultLabel(addr, jitStoreFault)

	alignedLabel := e.irEm.NewLabel()
	doneLabel := e.irEm.NewLabel()
	alignBits := e.irEm.Tmp()
	e.irEm.AndImm(alignBits, addr, int64(width-1))
	e.irEm.Branch(alignBits, VRegZero, EQ, alignedLabel)

	// Misaligned path: OOB check first.
	if CheckSandboxBounds {
		e.emitOOBCheck(addr, width, faultLabel)
	}
	t := WidthToType(width)
	if funct3 == 2 {
		tmp := e.irEm.Tmp()
		e.irEm.Zext(tmp, e.freg(rs2), I32)
		e.irEm.MisalignedStore(addr, tmp, t)
	} else {
		e.irEm.MisalignedStore(addr, e.freg(rs2), t)
	}
	e.irEm.Jump(doneLabel)

	// Aligned path.
	e.irEm.PlaceLabel(alignedLabel)
	if funct3 == 2 {
		tmp := e.irEm.Tmp()
		e.irEm.Zext(tmp, e.freg(rs2), I32)
		e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), tmp, 4, faultLabel)
	} else {
		e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), e.freg(rs2), 8, faultLabel)
	}
	e.irEm.PlaceLabel(doneLabel)
}

// emitMisalignedFPStore emits a byte-by-byte FP store (FSW or FSD) at a
// misaligned address. faultLabel is the per-call-site fault tail.
func (e *emitter) emitMisalignedFPStore(rs2 uint32, addr VReg, width int, faultLabel Label) {
	if width == 4 { // FSW: extract low 32 bits, then byte-by-byte
		src := e.irEm.Tmp()
		e.irEm.Zext(src, e.freg(rs2), I32)
		e.emitMisalignedStore(addr, src, 4, faultLabel)
	} else { // FSD: store all 64 bits
		e.emitMisalignedStore(addr, e.freg(rs2), 8, faultLabel)
	}
}

// ── FMA family ─────────────────────────────────────────────────────────
//
// RISC-V spec §11.6: FMADD/FMSUB/FNMADD/FNMSUB perform a*b (+|-) c
// with a SINGLE IEEE 754 rounding. We emit IRFma / IRFmsub / IRFnmadd /
// IRFnmsub which the amd64 lowerer turns into VFMADD213/VFMSUB213/
// VFNMADD213/VFNMSUB213 — native hardware FMA, single-rounded.
//
// RISC-V semantics:
//   FMADD   = a*b + c       → IRFma
//   FMSUB   = a*b - c       → IRFmsub
//   FNMADD  = -(a*b + c)    → IRFnmadd
//   FNMSUB  = -(a*b - c) = -a*b + c → IRFnmsub
//
// Post-op: canonicalize NaN per §11.3 and NaN-box (boxF32/boxF64).

func (e *emitter) emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt uint32) {
	if fpfmt > 1 {
		e.terminated = true
		return
	}

	if fpfmt == 0 { // single precision
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		c := e.unboxF32(rs3)
		result := e.irEm.Tmp()
		switch opcode {
		case 0x43: // FMADD.S
			e.irEm.FMA(result, a, b, c, F32)
		case 0x47: // FMSUB.S
			e.irEm.Fmsub(result, a, b, c, F32)
		case 0x4F: // FNMADD.S
			e.irEm.Fnmadd(result, a, b, c, F32)
		case 0x4B: // FNMSUB.S
			e.irEm.Fnmsub(result, a, b, c, F32)
		}
		e.boxF32(rd, e.canonF32(result))
	} else { // double precision
		a := e.unboxF64(rs1)
		b := e.unboxF64(rs2)
		c := e.unboxF64(rs3)
		result := e.irEm.Tmp()
		switch opcode {
		case 0x43: // FMADD.D
			e.irEm.FMA(result, a, b, c, F64)
		case 0x47: // FMSUB.D
			e.irEm.Fmsub(result, a, b, c, F64)
		case 0x4F: // FNMADD.D
			e.irEm.Fnmadd(result, a, b, c, F64)
		case 0x4B: // FNMSUB.D
			e.irEm.Fnmsub(result, a, b, c, F64)
		}
		e.boxF64(rd, e.canonF64(result))
	}
}

// ── FP-OP ──────────────────────────────────────────────────────────────

func (e *emitter) emitFPOp(rd, rs1, rs2, funct3, funct5, fpfmt uint32) {
	if fpfmt == 0 {
		e.emitFPOpS(rd, rs1, rs2, funct3, funct5)
	} else if fpfmt == 1 {
		e.emitFPOpD(rd, rs1, rs2, funct3, funct5)
	} else {
		e.terminated = true
	}
}

func (e *emitter) emitFPOpS(rd, rs1, rs2, funct3, funct5 uint32) {
	switch funct5 {
	case 0x00: // FADD.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FAdd(result, a, b, F32)
		e.boxF32(rd, e.canonF32(result))
	case 0x01: // FSUB.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FSub(result, a, b, F32)
		e.boxF32(rd, e.canonF32(result))
	case 0x02: // FMUL.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FMul(result, a, b, F32)
		e.boxF32(rd, e.canonF32(result))
	case 0x03: // FDIV.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FDiv(result, a, b, F32)
		e.boxF32(rd, e.canonF32(result))
	case 0x0B: // FSQRT.S
		a := e.unboxF32(rs1)
		result := e.irEm.Tmp()
		e.irEm.FSqrt(result, a, F32)
		e.boxF32(rd, e.canonF32(result))
	case 0x04: // FSGNJ.S / FSGNJN.S / FSGNJX.S
		e.emitFsgnjS(rd, rs1, rs2, funct3)
	case 0x05: // FMIN.S / FMAX.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.emitFMinMax(result, a, b, F32, funct3 == 1) // funct3=0: MIN, 1: MAX
		e.boxF32(rd, result)                          // FMinMax emits canon on the two-NaN path internally
	case 0x08: // FCVT.S.D
		a := e.freg(rs1)
		result := e.irEm.Tmp()
		e.irEm.FCvtFF(result, a, F64, F32)
		e.boxF32(rd, e.canonF32(result))
	case 0x14: // FEQ.S / FLT.S / FLE.S
		e.emitFcmpS(rd, rs1, rs2, funct3)
	case 0x18: // FCVT.{W,WU,L,LU}.S
		e.emitFcvtToIntS(rd, rs1, rs2)
	case 0x1A: // FCVT.S.{W,WU,L,LU}
		e.emitFcvtFromIntS(rd, rs1, rs2)
	case 0x1C: // FMV.X.W / FCLASS.S
		switch funct3 {
		case 0: // FMV.X.W
			if rd != 0 {
				e.irEm.Sext(e.xregDst(rd), e.freg(rs1), I32)
			}
		default:
			e.terminated = true
		}
	case 0x1E: // FMV.W.X
		e.boxF32(rd, e.xreg(rs1))
	default:
		e.terminated = true
	}
}

// boxF64 writes a 64-bit double directly into f[rd]. No NaN-boxing is
// needed for f64 (all 64 bits are the value) but we route through
// memory to keep the allocator from typed-loading with MOVSD before a
// possible later re-read: the store-to-f[rd]-as-I64 makes it clean.
func (e *emitter) boxF64(rd uint32, val VReg) {
	em := e.irEm
	intBits := em.Tmp()
	em.MovT(intBits, val, I64)
	em.Store(em.FBase(), int64(rd)*8, intBits, I64)
}

// unboxF64 loads the 64-bit double from f[rs] as F64 suitable for
// ADDSD/SUBSD/etc. Avoids the FP-VReg typing hazard.
func (e *emitter) unboxF64(rs uint32) VReg {
	em := e.irEm
	intBits := em.Tmp()
	em.Load(intBits, em.FBase(), int64(rs)*8, I64, false)
	out := em.Tmp()
	em.MovT(out, intBits, F64)
	return out
}

func (e *emitter) emitFPOpD(rd, rs1, rs2, funct3, funct5 uint32) {
	switch funct5 {
	case 0x00: // FADD.D
		a := e.unboxF64(rs1)
		b := e.unboxF64(rs2)
		result := e.irEm.Tmp()
		e.irEm.FAdd(result, a, b, F64)
		e.boxF64(rd, e.canonF64(result))
	case 0x01: // FSUB.D
		a := e.unboxF64(rs1)
		b := e.unboxF64(rs2)
		result := e.irEm.Tmp()
		e.irEm.FSub(result, a, b, F64)
		e.boxF64(rd, e.canonF64(result))
	case 0x02: // FMUL.D
		a := e.unboxF64(rs1)
		b := e.unboxF64(rs2)
		result := e.irEm.Tmp()
		e.irEm.FMul(result, a, b, F64)
		e.boxF64(rd, e.canonF64(result))
	case 0x03: // FDIV.D
		a := e.unboxF64(rs1)
		b := e.unboxF64(rs2)
		result := e.irEm.Tmp()
		e.irEm.FDiv(result, a, b, F64)
		e.boxF64(rd, e.canonF64(result))
	case 0x0B: // FSQRT.D
		a := e.unboxF64(rs1)
		result := e.irEm.Tmp()
		e.irEm.FSqrt(result, a, F64)
		e.boxF64(rd, e.canonF64(result))
	case 0x04: // FSGNJ.D
		e.emitFsgnjD(rd, rs1, rs2, funct3)
	case 0x05: // FMIN.D / FMAX.D
		a := e.unboxF64(rs1)
		b := e.unboxF64(rs2)
		result := e.irEm.Tmp()
		e.emitFMinMax(result, a, b, F64, funct3 == 1)
		e.boxF64(rd, result)
	case 0x08: // FCVT.D.S
		a := e.unboxF32(rs1)
		result := e.irEm.Tmp()
		e.irEm.FCvtFF(result, a, F32, F64)
		e.boxF64(rd, e.canonF64(result))
	case 0x14: // FEQ.D / FLT.D / FLE.D
		e.emitFcmpD(rd, rs1, rs2, funct3)
	case 0x18: // FCVT.{W,WU,L,LU}.D
		e.emitFcvtToIntD(rd, rs1, rs2)
	case 0x1A: // FCVT.D.{W,WU,L,LU}
		e.emitFcvtFromIntD(rd, rs1, rs2)
	case 0x1C: // FMV.X.D / FCLASS.D
		switch funct3 {
		case 0:
			if rd != 0 {
				e.irEm.Mov(e.xregDst(rd), e.freg(rs1))
			}
		default:
			e.terminated = true
		}
	case 0x1E: // FMV.D.X
		e.irEm.Mov(e.fregDst(rd), e.xreg(rs1))
	default:
		e.terminated = true
	}
}

// ── FP sign injection ──────────────────────────────────────────────────

func (e *emitter) emitFsgnjS(rd, rs1, rs2, funct3 uint32) {
	s1 := e.unboxF32(rs1)
	s2 := e.unboxF32(rs2)
	switch funct3 {
	case 0: // FSGNJ.S
		t1 := e.irEm.Tmp()
		e.irEm.AndImm(t1, s1, 0x7FFFFFFF)
		t2 := e.irEm.Tmp()
		e.irEm.AndImm(t2, s2, int64(0x80000000))
		result := e.irEm.Tmp()
		e.irEm.Or(result, t1, t2)
		e.boxF32(rd, result)
	case 1: // FSGNJN.S
		t1 := e.irEm.Tmp()
		e.irEm.AndImm(t1, s1, 0x7FFFFFFF)
		t2 := e.irEm.Tmp()
		e.irEm.Not(t2, s2)
		e.irEm.AndImm(t2, t2, int64(0x80000000))
		result := e.irEm.Tmp()
		e.irEm.Or(result, t1, t2)
		e.boxF32(rd, result)
	case 2: // FSGNJX.S
		result := e.irEm.Tmp()
		t2 := e.irEm.Tmp()
		e.irEm.AndImm(t2, s2, int64(0x80000000))
		e.irEm.Xor(result, s1, t2)
		e.boxF32(rd, result)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFsgnjD(rd, rs1, rs2, funct3 uint32) {
	a := e.freg(rs1)
	b := e.freg(rs2)
	dst := e.fregDst(rd)
	signMask := e.irEm.Tmp()
	e.irEm.Const(signMask, -9223372036854775808) // 0x8000000000000000 as int64 (math.MinInt64)
	absMask := e.irEm.Tmp()
	e.irEm.Const(absMask, 9223372036854775807) // 0x7FFFFFFFFFFFFFFF as int64 (math.MaxInt64)

	switch funct3 {
	case 0: // FSGNJ.D
		t1 := e.irEm.Tmp()
		e.irEm.And(t1, a, absMask)
		t2 := e.irEm.Tmp()
		e.irEm.And(t2, b, signMask)
		e.irEm.Or(dst, t1, t2)
	case 1: // FSGNJN.D
		t1 := e.irEm.Tmp()
		e.irEm.And(t1, a, absMask)
		t2 := e.irEm.Tmp()
		e.irEm.Not(t2, b)
		e.irEm.And(t2, t2, signMask)
		e.irEm.Or(dst, t1, t2)
	case 2: // FSGNJX.D
		t2 := e.irEm.Tmp()
		e.irEm.And(t2, b, signMask)
		e.irEm.Xor(dst, a, t2)
	default:
		e.terminated = true
	}
}

// ── FP comparison ──────────────────────────────────────────────────────

func (e *emitter) emitFcmpS(rd, rs1, rs2, funct3 uint32) {
	if rd == 0 {
		return
	}
	a := e.unboxF32(rs1)
	b := e.unboxF32(rs2)
	dst := e.xregDst(rd)
	switch funct3 {
	case 0:
		e.irEm.FCmp(dst, a, b, LE, F32)
	case 1:
		e.irEm.FCmp(dst, a, b, LT, F32)
	case 2:
		e.irEm.FCmp(dst, a, b, EQ, F32)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcmpD(rd, rs1, rs2, funct3 uint32) {
	if rd == 0 {
		return
	}
	a := e.freg(rs1)
	b := e.freg(rs2)
	dst := e.xregDst(rd)
	switch funct3 {
	case 0:
		e.irEm.FCmp(dst, a, b, LE, F64)
	case 1:
		e.irEm.FCmp(dst, a, b, LT, F64)
	case 2:
		e.irEm.FCmp(dst, a, b, EQ, F64)
	default:
		e.terminated = true
	}
}

// ── FP conversions ─────────────────────────────────────────────────────

func (e *emitter) emitFcvtToIntS(rd, rs1, rs2 uint32) {
	if rd == 0 {
		return
	}
	a := e.unboxF32(rs1)
	dst := e.xregDst(rd)
	switch rs2 {
	case 0: // FCVT.W.S
		t := e.irEm.Tmp()
		e.irEm.FCvtToI(t, a, F32, I32)
		e.irEm.Sext(dst, t, I32)
	case 1: // FCVT.WU.S
		t := e.irEm.Tmp()
		e.irEm.FCvtToU(t, a, F32, I32)
		e.irEm.Sext(dst, t, I32)
	case 2: // FCVT.L.S
		e.irEm.FCvtToI(dst, a, F32, I64)
	case 3: // FCVT.LU.S
		e.irEm.FCvtToU(dst, a, F32, I64)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtToIntD(rd, rs1, rs2 uint32) {
	if rd == 0 {
		return
	}
	a := e.freg(rs1)
	dst := e.xregDst(rd)
	switch rs2 {
	case 0:
		t := e.irEm.Tmp()
		e.irEm.FCvtToI(t, a, F64, I32)
		e.irEm.Sext(dst, t, I32)
	case 1:
		t := e.irEm.Tmp()
		e.irEm.FCvtToU(t, a, F64, I32)
		e.irEm.Sext(dst, t, I32)
	case 2:
		e.irEm.FCvtToI(dst, a, F64, I64)
	case 3:
		e.irEm.FCvtToU(dst, a, F64, I64)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtFromIntS(rd, rs1, rs2 uint32) {
	s := e.xreg(rs1)
	switch rs2 {
	case 0: // FCVT.S.W
		t := e.irEm.Tmp()
		e.irEm.Sext(t, s, I32)
		result := e.irEm.Tmp()
		e.irEm.FCvtFromI(result, t, I32, F32)
		e.boxF32(rd, result)
	case 1: // FCVT.S.WU
		t := e.irEm.Tmp()
		e.irEm.Zext(t, s, I32)
		result := e.irEm.Tmp()
		e.irEm.FCvtFromU(result, t, I32, F32)
		e.boxF32(rd, result)
	case 2: // FCVT.S.L
		result := e.irEm.Tmp()
		e.irEm.FCvtFromI(result, s, I64, F32)
		e.boxF32(rd, result)
	case 3: // FCVT.S.LU
		result := e.irEm.Tmp()
		e.irEm.FCvtFromU(result, s, I64, F32)
		e.boxF32(rd, result)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtFromIntD(rd, rs1, rs2 uint32) {
	s := e.xreg(rs1)
	dst := e.fregDst(rd)
	switch rs2 {
	case 0:
		t := e.irEm.Tmp()
		e.irEm.Sext(t, s, I32)
		e.irEm.FCvtFromI(dst, t, I32, F64)
	case 1:
		t := e.irEm.Tmp()
		e.irEm.Zext(t, s, I32)
		e.irEm.FCvtFromU(dst, t, I32, F64)
	case 2:
		e.irEm.FCvtFromI(dst, s, I64, F64)
	case 3:
		e.irEm.FCvtFromU(dst, s, I64, F64)
	default:
		e.terminated = true
	}
}

// ── JAL / JALR / BRANCH ───────────────────────────────────────────────

func (e *emitter) emitJAL(rd uint32, offset int64, insnSize uint64) {
	target := e.pc + uint64(offset)
	if rd != 0 {
		e.irEm.Const(e.xregDst(rd), int64(e.pc+insnSize))
	}
	e.advancePC(insnSize)

	if rd == 0 {
		origPC := e.pc - insnSize
		targetLabel := e.getOrCreateLabel(target)
		// Same backward detection as emitBranch: check both PC ordering
		// and whether the target was already emitted (visited).
		backward := target < origPC || e.visited[target]
		if backward {
			e.irEm.BudgetCheck(targetLabel, target)
		} else {
			e.irEm.Jump(targetLabel)
		}
		e.gotoTargets.add(target)
		e.pc = target
		return
	}

	// RAS: for JAL ra, try to inline the callee. The return address
	// is already stored in rd above; push it so emitJALR can predict
	// the return and avoid the decoder_cache lookup.
	if rd == 1 && len(e.callStack) < 4 &&
		target < e.regionEnd && !e.visited[target] {
		e.callStack = append(e.callStack, e.pc)
		e.pc = target
		return
	}

	e.emitChainableReturn(target)
	e.terminated = true
}

func (e *emitter) emitJALR(rd, rs1 uint32, imm int64, insnSize uint64) {
	// RAS: if this is JALR zero, 0(ra) and we have a predicted return
	// address from a prior inlined JAL ra, emit a fast-path comparison.
	// On match, jump directly to the return site (already in this block).
	// On mismatch, fall through to the decoder_cache JALR IC.
	if rd == 0 && rs1 == 1 && imm == 0 && len(e.callStack) > 0 {
		expectedAddr := e.callStack[len(e.callStack)-1]
		e.callStack = e.callStack[:len(e.callStack)-1]

		returnLabel := e.getOrCreateLabel(expectedAddr)
		e.irEm.BranchImm(e.xreg(rs1), int64(expectedAddr), EQ, returnLabel)

		// Mismatch: fall through to JALR IC (terminal).
		tgt := e.irEm.Tmp()
		e.irEm.AddImm(tgt, e.xreg(rs1), 0)
		e.irEm.AndImm(tgt, tgt, ^int64(1))
		e.advancePC(insnSize)
		e.irEm.DynChainableRet(tgt, e.jalrSiteIdx)
		e.jalrSiteIdx++

		// Continue emitting at the predicted return site.
		e.pc = expectedAddr
		return
	}

	tgt := e.irEm.Tmp()
	e.irEm.AddImm(tgt, e.xreg(rs1), imm)
	e.irEm.AndImm(tgt, tgt, ^int64(1))
	if rd != 0 {
		e.irEm.Const(e.xregDst(rd), int64(e.pc+insnSize))
	}
	e.advancePC(insnSize)
	e.irEm.DynChainableRet(tgt, e.jalrSiteIdx)
	e.jalrSiteIdx++
	e.terminated = true
}

func (e *emitter) emitBranch(rs1, rs2, funct3 uint32, offset int64) {
	target := e.pc + uint64(offset)
	pred, _ := branchPred(funct3)

	e.emitIC()
	e.icEmitted = true

	a := e.xreg(rs1)
	b := e.xreg(rs2)

	internal := e.visited[target] ||
		(e.regionEnd > 0 && target >= e.startPC && target < e.regionEnd)

	if internal {
		targetLabel := e.getOrCreateLabel(target)
		// A branch is "backward" if the target was already emitted (visited).
		// We cannot simply check target < e.pc because scanRegion may have
		// followed a forward JAL past the target, causing the target to be
		// emitted at a higher PC but earlier in the  Jumping to an
		// already-emitted label re-executes code → potential infinite loop
		// → requires BudgetCheck.
		backward := target < e.pc || e.visited[target]
		if backward {
			takenLabel := e.irEm.NewLabel()
			continueLabel := e.irEm.NewLabel()
			e.irEm.Branch(a, b, pred, takenLabel)
			e.irEm.Jump(continueLabel) // not taken: skip budget check
			e.irEm.PlaceLabel(takenLabel)
			e.irEm.BudgetCheck(targetLabel, target)
			e.irEm.PlaceLabel(continueLabel)
		} else {
			e.irEm.Branch(a, b, pred, targetLabel)
		}
		e.gotoTargets.add(target)
	} else {
		exitLabel := e.irEm.NewLabel()
		e.irEm.Branch(a, b, pred, exitLabel)
		e.deferredExits = append(e.deferredExits, deferredExit{exitLabel, target})
	}
}

// ── RVC ────────────────────────────────────────────────────────────────

func (e *emitter) emitRVC(insn uint16) {
	e.emitLabel()
	quad := insn & 0x3
	funct3 := insn >> 13

	switch quad {
	case 0x0:
		e.emitRVC_Q0(insn, funct3)
	case 0x1:
		e.emitRVC_Q1(insn, funct3)
	case 0x2:
		e.emitRVC_Q2(insn, funct3)
	default:
		e.terminated = true
	}
	if !e.terminated {
		e.advancePC(2)
	}
}

func (e *emitter) emitRVC_Q0(insn uint16, funct3 uint16) {
	rd := uint32(8 + ((insn >> 2) & 7))
	rs1 := uint32(8 + ((insn >> 7) & 7))

	switch funct3 {
	case 0b000: // C.ADDI4SPN
		nzuimm := int64(((insn>>11)&3)<<4 | ((insn>>7)&0xF)<<6 |
			((insn>>6)&1)<<2 | ((insn>>5)&1)<<3)
		if nzuimm == 0 {
			e.terminated = true
			return
		}
		e.emitOpImm(rd, 2, nzuimm, 0, 0)
	case 0b001: // C.FLD
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitFPLoad(rd, rs1, uimm, 3)
	case 0b010: // C.LW
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
		e.emitLoad(rd, rs1, uimm, 2)
	case 0b011: // C.LD
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitLoad(rd, rs1, uimm, 3)
	case 0b101: // C.FSD
		rs2 := uint32(8 + ((insn >> 2) & 7))
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitFPStore(rs1, rs2, uimm, 3)
	case 0b110: // C.SW
		rs2 := uint32(8 + ((insn >> 2) & 7))
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
		e.emitStore(rs1, rs2, uimm, 2)
	case 0b111: // C.SD
		rs2 := uint32(8 + ((insn >> 2) & 7))
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitStore(rs1, rs2, uimm, 3)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitRVC_Q1(insn uint16, funct3 uint16) {
	switch funct3 {
	case 0b000: // C.NOP / C.ADDI
		rd := uint32((insn >> 7) & 0x1F)
		imm := rvcSignedImm6(insn)
		e.emitOpImm(rd, rd, imm, 0, 0)
	case 0b001: // C.ADDIW
		rd := uint32((insn >> 7) & 0x1F)
		imm := rvcSignedImm6(insn)
		e.emitOpImm32(rd, rd, imm, 0, 0)
	case 0b010: // C.LI
		rd := uint32((insn >> 7) & 0x1F)
		imm := rvcSignedImm6(insn)
		e.emitOpImm(rd, 0, imm, 0, 0)
	case 0b011: // C.ADDI16SP / C.LUI
		rd := uint32((insn >> 7) & 0x1F)
		if rd == 2 {
			nzimm := int64(((insn>>12)&1)<<9 | ((insn>>6)&1)<<4 |
				((insn>>5)&1)<<6 | ((insn>>3)&3)<<7 | ((insn>>2)&1)<<5)
			if (insn>>12)&1 != 0 {
				nzimm |= -512
			}
			if nzimm == 0 {
				e.terminated = true
				return
			}
			e.emitOpImm(2, 2, nzimm, 0, 0)
		} else if rd != 0 {
			nzimm := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			if (insn>>12)&1 != 0 {
				nzimm |= -32
			}
			if nzimm == 0 {
				e.terminated = true
				return
			}
			uimm := nzimm << 12
			e.irEm.Const(e.xregDst(rd), uimm)
		}
	case 0b100: // C.MISC-ALU
		rs1 := uint32(8 + ((insn >> 7) & 7))
		rs2 := uint32(8 + ((insn >> 2) & 7))
		funct2 := (insn >> 10) & 3
		switch funct2 {
		case 0b00:
			shamt := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			e.emitOpImm(rs1, rs1, shamt, 5, 0)
		case 0b01:
			shamt := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			e.emitOpImm(rs1, rs1, shamt, 5, 0x20)
		case 0b10:
			imm := rvcSignedImm6(insn)
			e.emitOpImm(rs1, rs1, imm, 7, 0)
		case 0b11:
			bit12 := (insn >> 12) & 1
			op := (insn >> 5) & 3
			if bit12 == 0 {
				switch op {
				case 0b00:
					e.emitOp(rs1, rs1, rs2, 0, 0x20)
				case 0b01:
					e.emitOp(rs1, rs1, rs2, 4, 0)
				case 0b10:
					e.emitOp(rs1, rs1, rs2, 6, 0)
				case 0b11:
					e.emitOp(rs1, rs1, rs2, 7, 0)
				}
			} else {
				switch op {
				case 0b00:
					e.emitOp32(rs1, rs1, rs2, 0, 0x20)
				case 0b01:
					e.emitOp32(rs1, rs1, rs2, 0, 0)
				default:
					e.terminated = true
				}
			}
		}
	case 0b101: // C.J
		off := rvcJOffset(insn)
		e.emitJAL(0, off, 2)
	case 0b110: // C.BEQZ
		rs1 := uint32(8 + ((insn >> 7) & 7))
		off := rvcBOffset(insn)
		e.emitBranch(rs1, 0, 0, off)
	case 0b111: // C.BNEZ
		rs1 := uint32(8 + ((insn >> 7) & 7))
		off := rvcBOffset(insn)
		e.emitBranch(rs1, 0, 1, off)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitRVC_Q2(insn uint16, funct3 uint16) {
	rd := uint32((insn >> 7) & 0x1F)
	rs2 := uint32((insn >> 2) & 0x1F)

	switch funct3 {
	case 0b000: // C.SLLI
		shamt := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
		e.emitOpImm(rd, rd, shamt, 1, 0)
	case 0b001: // C.FLDSP
		uimm := int64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
		e.emitFPLoad(rd, 2, uimm, 3)
	case 0b010: // C.LWSP
		uimm := int64(((insn>>12)&1)<<5 | ((insn>>4)&7)<<2 | ((insn>>2)&3)<<6)
		e.emitLoad(rd, 2, uimm, 2)
	case 0b011: // C.LDSP
		uimm := int64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
		e.emitLoad(rd, 2, uimm, 3)
	case 0b100:
		bit12 := (insn >> 12) & 1
		if bit12 == 0 {
			if rs2 == 0 {
				if rd == 0 {
					e.terminated = true
					return
				}
				e.emitJALR(0, rd, 0, 2)
			} else {
				e.emitOpImm(rd, rs2, 0, 0, 0)
			}
		} else {
			if rd == 0 && rs2 == 0 {
				e.advancePC(2)
				e.emitReturn(e.pc, jitEbreak)
				e.terminated = true
			} else if rs2 == 0 {
				e.emitJALR(1, rd, 0, 2)
			} else {
				e.emitOp(rd, rd, rs2, 0, 0)
			}
		}
	case 0b101: // C.FSDSP
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
		e.emitFPStore(2, rs2, uimm, 3)
	case 0b110: // C.SWSP
		uimm := int64(((insn>>9)&0xF)<<2 | ((insn>>7)&3)<<6)
		e.emitStore(2, rs2, uimm, 2)
	case 0b111: // C.SDSP
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
		e.emitStore(2, rs2, uimm, 3)
	default:
		e.terminated = true
	}
}
