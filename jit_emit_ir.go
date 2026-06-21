package riscv

// jit_emit_ir.go — Translates RISC-V basic blocks to IR (ir.Block).
// This replaces jit_emit.go's C source generation with IR emission.

import (
	"fmt"
	"os"
)

// maxBlockIRInsns limits the total IR instructions per lazy block. Each
// RISC-V instruction expands to several IR ops; very large blocks hit O(N*L)
// in the register allocator's spill phase where L is average interval length.
// 4096 IR instructions gives lazy translation a libriscv-style chunk without
// letting one cold miss monopolize the process.
const maxBlockIRInsns = 4096

// PerBlockCapTimeToSplit is the soft cap on guest instructions per JIT
// block. After exceeding this threshold, the emitter terminates the block
// at the next natural break point (JALR, ECALL, unconditional jump, etc.)
// as determined by classifyFlow(). Set to 0 to disable the cap entirely.
// Follows the libriscv model (ITS_TIME_TO_SPLIT = 5000 for TCC).
var PerBlockCapTimeToSplit int64 = 5000

// isLazySplitStoppingFlow mirrors libriscv's binary-translation split point:
// after the chunk is already large, wait for a hard stop rather than cutting
// at ordinary branches or direct calls.
func isLazySplitStoppingFlow(fc flowClass) bool {
	return fc == flowTerm
}

// emitResult holds the generated IR block and metadata.
type emitResult struct {
	block          *Block
	startPC        uint64
	endPC          uint64
	numInsns       int
	numChainExits  int // number of IRChainExit instructions emitted
	fpStaticNonRNE bool
}

// deferredExit holds an external branch exit to emit at finalize time.
type deferredExit struct {
	label    Label
	targetPC uint64
}

// budgetExit holds a per-instruction budget cold path for lockstep mode.
type budgetExit struct {
	label Label
	pc    uint64 // the instruction we did NOT execute — resume here
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
	mem              *GuestMemory
	startPC          uint64
	pc               uint64
	irEm             *Emitter
	numInsns         int
	regsUsed         uint32 // bit i set iff xreg(i) or xregDst(i) was called
	terminated       bool
	visited          map[uint64]bool
	regionEnd        uint64
	gotoTargets      u64set
	pcLabels         u64labelmap
	stopperAddr      int64  // InfiniteLoopStopperPage address for backward-branch probes
	watchAddr        uint64 // tohost address; stores here trigger a block exit
	budgetExits      []budgetExit
	sharedBudgetExit Label
	deferredExits    []deferredExit
	deferredFaults   []deferredFault
	fpStaticNonRNE   bool
	exitIdx          int      // counter for chain exit indices
	jalrSiteIdx      int      // counter for JALR inline-cache site indices
	callStack        []uint64 // RAS: expected return addresses for inlined calls
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

// ── FP register bit helpers ────────────────────────────────────────────
//
// Guest FP registers are architecturally 64-bit raw bit patterns. Keep those
// bits in the f[] backing store and use FP VRegs only for transient host FP
// values. Mixing the two as persistent sources is unsafe: a same-block FLD or
// boxed FP result can update f[] while an already-materialized FP VReg still
// contains the block-entry value.
//
// The raw f[] path also preserves NaN-boxed single-precision values. Loading a
// guest FP register as an F32 VReg can select MOVSS and zero the upper 32 bits,
// which makes a valid NaN-box look malformed. Load/store raw bits as I64, then
// bit-cast with MovT only when an arithmetic instruction needs an XMM value.

func (e *emitter) loadFRegBits(r uint32) VReg {
	raw := e.irEm.Tmp()
	e.irEm.Load(raw, e.irEm.FBase(), int64(r)*8, I64, false)
	return raw
}

func (e *emitter) storeFRegBits(r uint32, raw VReg) {
	e.irEm.Store(e.irEm.FBase(), int64(r)*8, raw, I64)
}

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
	e.storeFRegBits(rd, boxed)
}

// unboxF32 extracts a 32-bit float from f[rs], returning canonical
// qNaN if the box is malformed (upper 32 bits not all-ones, spec
// §11.2). Returns an F32-typed VReg suitable for FAdd/FSub/...
func (e *emitter) unboxF32(rs uint32) VReg {
	em := e.irEm
	// Load the raw 64-bit word from f[rs] directly as I64. This
	// bypasses the FP VReg typing and its MOVSS-zero-extend hazard.
	srcInt := e.loadFRegBits(rs)
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

func (e *emitter) allowInstructionFusion() bool {
	return true
}

func (e *emitter) nativeReservationStateAvailable() bool {
	return e.irEm.j != nil && (e.irEm.j.useABJIT || e.irEm.j.regPolicy.Name == "rv8")
}

func (e *emitter) reservationState() (base VReg, addrOff, validOff int64) {
	if e.irEm.j != nil && e.irEm.j.useABJIT {
		return e.irEm.XBase(), abjitStateResvAddrOffset, abjitStateResvValidOffset
	}
	return e.irEm.SRetBase(), rv8SRetResvAddrOffset, rv8SRetResvValidOffset
}

func (e *emitter) emitBudgetCheck() {
	e.emitBudgetReserve(1)
}

func (e *emitter) emitBudgetReserve(n int) {
	if n <= 0 {
		return
	}
	coldLabel := e.irEm.NewLabel()
	e.irEm.BudgetReserve(int64(n), coldLabel)
	e.budgetExits = append(e.budgetExits, budgetExit{coldLabel, e.pc})
}

func (e *emitter) undoBudgetCheck() {
	e.irEm.IncIC()
}

func (e *emitter) markFPRounding(rm uint32) {
	if rm != uint32(rmRNE) && rm != uint32(rmDYN) {
		e.fpStaticNonRNE = true
	}
}

func fpOpUsesRM(funct5 uint32) bool {
	switch funct5 {
	case 0x00, 0x01, 0x02, 0x03, 0x0B, 0x08, 0x18, 0x1A:
		return true
	default:
		return false
	}
}

// peek32 fetches the 32-bit instruction at the given PC without
// advancing state. Returns (insn, true) on success.
func (e *emitter) peek32(pc uint64) (uint32, bool) {
	if pc >= e.regionEnd {
		return 0, false
	}
	insn, f := e.mem.Fetch32U(pc)
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
}

func (e *emitter) countInsn() {
	e.numInsns++
}

func (e *emitter) spillIC() {
	e.irEm.SpillIC()
}

func (e *emitter) emitReturn(pc uint64, status int) {
	e.spillIC()
	e.irEm.WriteBackAll()
	e.irEm.Ret(pc, status, VRegZero)
}

// emitSyscall emits a guest ECALL trap. The CPU-side trap PC is passed through
// to Go; the installed OS decides if and when to resume at trapPC+4.
func (e *emitter) emitSyscall(trapPC uint64) {
	e.spillIC()
	e.irEm.WriteBackAll()
	e.irEm.Syscall(trapPC, 0)
}

// allocFaultLabel allocates a per-call-site fault label and registers its
// (PC, addrVR, status) tuple so finalize() emits a tail returning the actual
// faulting instruction's PC and the live faulting address. Mirrors the TCC
// emitter's per-call-site `return (JITResult){pc, ic, status, addr}` pattern.
func (e *emitter) allocFaultLabel(addr VReg, status int) Label {
	return e.allocFaultLabelAt(e.pc, addr, status)
}

func (e *emitter) allocFaultLabelAt(pc uint64, addr VReg, status int) Label {
	l := e.irEm.NewLabel()
	e.deferredFaults = append(e.deferredFaults, deferredFault{
		label: l, pc: pc, addrVR: addr, status: status,
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
	e.spillIC()
	e.irEm.WriteBackAll()
	e.irEm.ChainExit(pc, e.exitIdx)
	e.exitIdx++
}

// lastIRWasTerminator reports whether the final IR instruction
// emitted into the current block is a terminator op whose lowered
// x86 unconditionally leaves the block. When true, finalize()'s
// fall-through emitChainableReturn is dead code and is skipped.
//
// Recognised terminators: IRRet, IRRetDyn, IRChainExit, IRJalrIC, IRSyscall.
func (e *emitter) lastIRWasTerminator() bool {
	ins := e.irEm.Block.Instrs
	if len(ins) == 0 {
		return false
	}
	switch ins[len(ins)-1].Op {
	case IRRet, IRRetDyn, IRChainExit, IRJalrIC, IRSyscall:
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
	// terminator. Blocks that hit a hard terminator (EBREAK, JAL rd!=0,
	// JALR, branch patterns) already left via IRRet/IRChainExit/IRJalrIC/
	// IRRetDyn; adding another fall-through would
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
		e.spillIC()
		e.irEm.WriteBackAll()
		e.irEm.Ret(df.pc, df.status, df.addrVR)
	}

	// Per-instruction budget cold paths + shared exit stub (lockstep mode).
	if len(e.budgetExits) > 0 {
		for _, be := range e.budgetExits {
			e.irEm.PlaceLabel(be.label)
			e.irEm.SetPC(be.pc)
			e.irEm.Jump(e.sharedBudgetExit)
		}
		e.irEm.PlaceLabel(e.sharedBudgetExit)
		e.irEm.SpillIC()
		e.irEm.WriteBackAll()
		e.irEm.RetBudget()
	}

	//if e.lockstepMode {
	//	vv("LOCKSTEP emit: startPC=0x%x endPC=0x%x numInsns=%d budgetExits=%d",
	//		e.startPC, e.pc, e.numInsns, len(e.budgetExits))
	//}

	return &emitResult{
		block:          e.irEm.Block,
		startPC:        e.startPC,
		endPC:          e.pc,
		numInsns:       e.numInsns,
		numChainExits:  e.exitIdx,
		fpStaticNonRNE: e.fpStaticNonRNE,
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
			case 0x33, 0x3B, 0x2F: // OP, OP-32, AMO
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
	region := scanLazyBlock(mem, pc)
	if region.pcCount == 0 {
		if debugJIT {
			fmt.Fprintf(os.Stderr, "BAIL pc=0x%x reason=scanLazyBlock_empty\n", pc)
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
		stopperAddr: int64(j.stopperPage),
		watchAddr:   j.watchAddr,
	}

	// The remaining instruction budget is loaded into R15 by the trampoline.
	// No IRZeroIC here — chain entries must preserve the remaining budget.
	e.sharedBudgetExit = e.irEm.NewLabel()

	// Emit IR (populates regsUsed via xreg/xregDst calls).
	for !e.terminated && e.pc < e.regionEnd {
		if len(irEm.Block.Instrs) >= maxBlockIRInsns {
			break
		}

		// Block size cap (libriscv hybrid model): after exceeding the
		// soft cap, stop at the next hard break point. Hard cap at 2x
		// prevents unbounded growth if no natural break appears.
		if PerBlockCapTimeToSplit > 0 &&
			int64(e.numInsns) >= PerBlockCapTimeToSplit {

			if int64(e.numInsns) >= PerBlockCapTimeToSplit*2 {
				break
			}
			fc, _, _ := classifyFlow(e.mem, e.pc)
			if isLazySplitStoppingFlow(fc) {
				break
			}
		}

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
					// Strict Spike-style behavior would leave this as a fault.
					// The JIT mirrors the interpreter's intentional permissive
					// bytewise fetch path for compatibility tests.
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

	savedNumInsns := e.numInsns
	savedBudgetExits := len(e.budgetExits)
	e.emitLabel()
	lateBudget := opcode == 0x17 || opcode == 0x13 || opcode == 0x1B || opcode == 0x2F
	if !lateBudget {
		e.emitBudgetCheck()
	}

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
		//vv("AUIPC entry: pc=0x%x rd=%d addr=0x%x watchAddr=0x%x startPC=0x%x", e.pc, rd, addr, e.watchAddr, e.startPC)

		// Macro-op fusion: peek at the next instruction for AUIPC+X pairs.
		if rd != 0 && e.allowInstructionFusion() {
			next, ok := e.peek32(e.pc + 4)
			//vv("AUIPC peek: pc=0x%x peek_ok=%v next=0x%x", e.pc, ok, next)
			if ok {
				nextOp := next & 0x7F
				nextRd := (next >> 7) & 0x1F
				nextRs1 := (next >> 15) & 0x1F
				nextImm := int64(int32(next)) >> 20

				//vv("AUIPC fusion: pc=0x%x rd=%d addr=0x%x nextOp=0x%x nextRs1=%d e.watchAddr=0x%x", e.pc, rd, addr, nextOp, nextRs1, e.watchAddr)

				switch {
				case nextOp == 0x13 && (next>>12)&7 == 0 && nextRd == rd && nextRs1 == rd:
					// AUIPC+ADDI → la (load address): single Const.
					e.emitBudgetReserve(2)
					e.irEm.Const(e.xregDst(rd), addr+nextImm)
					e.advancePC(4)
					e.advancePC(4)
					return

				case nextOp == 0x67 && nextRs1 == rd:
					// AUIPC+JALR: preserve AUIPC's architectural
					// destination, then let emitJALR apply the JALR link
					// write for nextRd. This matters for linker trampolines
					// like "auipc t6; jalr x0, imm(t6)", where t6 must
					// keep the AUIPC result after the jump.
					e.emitBudgetReserve(2)
					e.irEm.Const(e.xregDst(rd), addr)
					e.advancePC(4)
					e.emitJALR(nextRd, rd, nextImm, 4)
					return

				case nextOp == 0x03 && nextRs1 == rd && nextRd == rd:
					// AUIPC+LOAD → load from absolute guest address.
					e.emitBudgetReserve(2)
					e.advancePC(4)
					e.advancePC(4)
					funct3 := (next >> 12) & 7
					e.emitLoadFused(rd, addr+nextImm, funct3)
					return

				case nextOp == 0x23 && nextRs1 == rd && e.watchAddr != 0:
					// AUIPC+STORE where base == AUIPC result.
					// Compute the store's effective address at compile time.
					nextFunct3 := (next >> 12) & 7
					storeImm := sImm(next)
					storeAddr := addr + storeImm
					//vv("w.watchAddr = '%p'", e.watchAddr)

					if storeAddr == int64(e.watchAddr) {
						//vv("recognized 'tohost'! check if non-zero write...")
						e.emitBudgetReserve(2)
						nextRs2 := (next >> 20) & 0x1F
						e.irEm.Const(e.xregDst(rd), addr)
						e.advancePC(4)
						e.emitStore(rd, nextRs2, storeImm, nextFunct3)
						e.advancePC(4)
						// tohost written: if stored value != 0, exit block.
						if nextRs2 != 0 {
							skip := e.irEm.NewLabel()
							src := e.xreg(nextRs2)
							e.irEm.Branch(src, VRegZero, EQ, skip)
							e.spillIC()
							e.irEm.WriteBackAll()
							e.irEm.Ret(e.pc, jitOK, VRegZero)
							e.irEm.PlaceLabel(skip)
						}
						return
					}
				}
			}
		}
		e.emitBudgetCheck()
		if rd != 0 {
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
		if e.tryEmitOpImmFusion(rd, rs1, iimm, funct3, funct7) {
			return
		}
		e.emitBudgetCheck()
		e.emitOpImm(rd, rs1, iimm, funct3, funct7)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x1B: // OP-IMM-32
		if e.tryEmitOpImm32Fusion(rd, rs1, iimm, funct3, funct7) {
			return
		}
		e.emitBudgetCheck()
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

	case 0x2F: // AMO / LR / SC
		funct5 := insn >> 27
		e.emitAMO(rd, rs1, rs2, funct3, funct5)

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
		e.markFPRounding(funct3)
		e.emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x53: // FP-OP
		funct5 := insn >> 27
		fpfmt := (insn >> 25) & 0x3
		if fpfmt <= 1 && fpOpUsesRM(funct5) {
			e.markFPRounding(funct3)
		}
		e.emitFPOp(rd, rs1, rs2, funct3, funct5, fpfmt)
		if !e.terminated {
			e.advancePC(4)
		}

	case 0x0F: // FENCE
		e.advancePC(4)

	case 0x73: // SYSTEM
		switch insn {
		case 0x00000073: // ECALL
			e.countInsn()
			e.emitSyscall(e.pc)
			e.terminated = true
		case 0x00100073: // EBREAK
			e.countInsn()
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

	if e.terminated && e.numInsns == savedNumInsns && len(e.budgetExits) > savedBudgetExits {
		e.undoBudgetCheck()
	}
}

// ── OP-IMM ─────────────────────────────────────────────────────────────

func (e *emitter) tryEmitOpImmFusion(rd, rs1 uint32, imm int64, funct3, funct7 uint32) bool {
	if !e.allowInstructionFusion() || rd == 0 {
		return false
	}
	shamt := imm & 63
	if funct3 != 1 || funct7>>1 != 0x00 || shamt != 32 || rd != rs1 {
		return false
	}
	next, ok := e.peek32(e.pc + 4)
	if !ok {
		return false
	}
	nOp := next & 0x7F
	nRd := (next >> 7) & 0x1F
	nRs1 := (next >> 15) & 0x1F
	nF3 := (next >> 12) & 7
	nF6 := next >> 26
	nShamt := int64((next >> 20) & 0x3F)
	if nOp != 0x13 || nF3 != 5 || nF6 != 0 || nShamt != 32 || nRd != rd || nRs1 != rd {
		return false
	}
	e.emitBudgetReserve(2)
	e.irEm.Zext(e.xregDst(rd), e.xreg(rs1), I32)
	e.advancePC(4)
	e.advancePC(4)
	return true
}

func (e *emitter) emitOpImm(rd, rs1 uint32, imm int64, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	imm12 := uint32(imm) & 0xFFF
	funct6 := imm12 >> 6
	shamt := int64(imm12 & 63)

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
		switch funct6 {
		case 0x00: // SLLI
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
		case 0x18: // CLZ/CTZ/CPOP/SEXT.B/SEXT.H
			switch imm12 {
			case 0x600: // CLZ
				src := e.xreg(rs1)
				e.irEm.Clz(e.xregDst(rd), src, I64)
			case 0x601: // CTZ
				src := e.xreg(rs1)
				e.irEm.Ctz(e.xregDst(rd), src, I64)
			case 0x602: // CPOP
				src := e.xreg(rs1)
				e.irEm.Popcount(e.xregDst(rd), src, I64)
			case 0x604: // SEXT.B
				src := e.xreg(rs1)
				e.irEm.Sext(e.xregDst(rd), src, I8)
			case 0x605: // SEXT.H
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
	case 5: // SRLI/SRAI / BEXTI / RORI / ORC.B / REV8
		switch {
		case funct6 == 0x00: // SRLI
			src := e.xreg(rs1)
			e.irEm.ShrImm(e.xregDst(rd), src, shamt)
		case funct6 == 0x10: // SRAI
			src := e.xreg(rs1)
			e.irEm.SarImm(e.xregDst(rd), src, shamt)
		case funct6 == 0x12: // BEXTI
			t := e.irEm.Tmp()
			e.irEm.ShrImm(t, e.xreg(rs1), shamt)
			e.irEm.AndImm(e.xregDst(rd), t, 1)
		case funct6 == 0x18: // RORI
			src := e.xreg(rs1)
			if shamt == 0 {
				e.irEm.Mov(e.xregDst(rd), src)
			} else {
				t1 := e.irEm.Tmp()
				e.irEm.ShrImm(t1, src, shamt)
				t2 := e.irEm.Tmp()
				e.irEm.ShlImm(t2, src, 64-shamt)
				e.irEm.Or(e.xregDst(rd), t1, t2)
			}
		case imm12 == 0x287: // ORC.B — each byte becomes 0xFF if nonzero, 0x00 if zero
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
		case imm12 == 0x6B8: // REV8 — byte-swap via BSWAP
			src := e.xreg(rs1)
			e.irEm.Bswap(e.xregDst(rd), src)
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

func (e *emitter) tryEmitOpImm32Fusion(rd, rs1 uint32, imm int64, funct3, funct7 uint32) bool {
	if !e.allowInstructionFusion() || rd == 0 || funct3 != 0 || funct7 != 0 {
		return false
	}
	n1, ok1 := e.peek32(e.pc + 4)
	if !ok1 {
		return false
	}
	n2, ok2 := e.peek32(e.pc + 8)
	if !ok2 {
		return false
	}
	n1Op, n1Rd, n1Rs1 := n1&0x7F, (n1>>7)&0x1F, (n1>>15)&0x1F
	n1F3, n1F6 := (n1>>12)&7, n1>>26
	n1Shamt := int64((n1 >> 20) & 0x3F)
	n2Op, n2Rd, n2Rs1 := n2&0x7F, (n2>>7)&0x1F, (n2>>15)&0x1F
	n2F3, n2F6 := (n2>>12)&7, n2>>26
	n2Shamt := int64((n2 >> 20) & 0x3F)
	if n1Op != 0x13 || n1F3 != 1 || n1F6 != 0 || n1Shamt != 32 ||
		n1Rd != rd || n1Rs1 != rd ||
		n2Op != 0x13 || n2F3 != 5 || n2F6 != 0 || n2Shamt != 32 ||
		n2Rd != rd || n2Rs1 != rd {
		return false
	}
	e.emitBudgetReserve(3)
	src := e.xreg(rs1)
	dst := e.xregDst(rd)
	if imm == 0 {
		e.irEm.Zext(dst, src, I32)
	} else {
		t := e.irEm.Tmp()
		e.irEm.AddImm(t, src, imm)
		e.irEm.Zext(dst, t, I32)
	}
	e.advancePC(4) // consumed ADDIW
	e.advancePC(4) // consumed SLLI
	e.advancePC(4) // consumed SRLI
	return true
}

func (e *emitter) emitOpImm32(rd, rs1 uint32, imm int64, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	imm12 := uint32(imm) & 0xFFF
	funct6 := imm12 >> 6
	shamt := int64(imm12 & 31)
	shamt6 := int64(imm12 & 63)

	switch funct3 {
	case 0: // ADDIW
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
	case 1: // SLLIW / SLLI.UW / CLZW / CTZW / CPOPW
		switch {
		case funct7 == 0x00: // SLLIW
			t := e.irEm.Tmp()
			e.irEm.ShlImm(t, e.xreg(rs1), shamt)
			e.irEm.Sext(e.xregDst(rd), t, I32)
		case funct6 == 0x02: // SLLI.UW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32)
			e.irEm.ShlImm(e.xregDst(rd), t, shamt6)
		case imm12 == 0x600: // CLZW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32)
			e.irEm.Clz(e.xregDst(rd), t, I32)
		case imm12 == 0x601: // CTZW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32)
			e.irEm.Ctz(e.xregDst(rd), t, I32)
		case imm12 == 0x602: // CPOPW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32)
			e.irEm.Popcount(e.xregDst(rd), t, I32)
		default:
			e.terminated = true
		}
	case 5: // SRLIW / SRAIW / RORIW
		switch funct7 {
		case 0x00: // SRLIW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), I32) // zero-extend to get uint32
			e.irEm.ShrImm(t, t, shamt)
			e.irEm.Sext(e.xregDst(rd), t, I32)
		case 0x20: // SRAIW
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

func (e *emitter) emitClmul(dst, a, b VReg, mode uint32) {
	acc := e.irEm.Tmp()
	e.irEm.Const(acc, 0)
	for i := 0; i < 64; i++ {
		var shift int
		switch mode {
		case 1: // CLMUL
			shift = i
		case 2: // CLMULR
			shift = 63 - i
		case 3: // CLMULH
			if i == 0 {
				continue
			}
			shift = 64 - i
		default:
			e.terminated = true
			return
		}

		bit := e.irEm.Tmp()
		if i == 0 {
			e.irEm.Mov(bit, b)
		} else {
			e.irEm.ShrImm(bit, b, int64(i))
		}
		e.irEm.AndImm(bit, bit, 1)

		mask := e.irEm.Tmp()
		e.irEm.Neg(mask, bit)

		term := e.irEm.Tmp()
		switch mode {
		case 1:
			if shift == 0 {
				e.irEm.Mov(term, a)
			} else {
				e.irEm.ShlImm(term, a, int64(shift))
			}
		case 2, 3:
			if shift == 0 {
				e.irEm.Mov(term, a)
			} else {
				e.irEm.ShrImm(term, a, int64(shift))
			}
		}
		e.irEm.And(term, term, mask)
		e.irEm.Xor(acc, acc, term)
	}
	e.irEm.Mov(dst, acc)
}

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
		default:
			e.terminated = true
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
	case 0x04: // RV32-style PACK/ZEXT.H encoding; RV64 Zbb uses PACKW below.
		if funct3 == 4 && rs2 == 0 {
			e.irEm.Zext(dst, a, I16)
		} else {
			e.terminated = true
		}
	case 0x05: // MIN/MAX (Zbb) + CLMUL (Zbc)
		switch funct3 {
		case 1: // CLMUL
			e.emitClmul(dst, a, b, 1)
		case 2: // CLMULR
			e.emitClmul(dst, a, b, 2)
		case 3: // CLMULH
			e.emitClmul(dst, a, b, 3)
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
		if funct3 != 1 {
			e.terminated = true
			return
		}
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
	case 0x04:
		switch funct3 {
		case 0: // Zba: ADD.UW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, I32)
			e.irEm.Add(dst, b, t)
		case 4: // Zbb: ZEXT.H (RV64 canonical PACKW rd, rs1, x0)
			if rs2 == 0 {
				e.irEm.Zext(dst, a, I16)
			} else {
				e.terminated = true
			}
		default:
			e.terminated = true
		}
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

// ── AMO / LR / SC ──────────────────────────────────────────────────────

func amoWidth(funct3 uint32) int {
	switch funct3 {
	case 0b010:
		return 4
	case 0b011:
		return 8
	default:
		return 0
	}
}

func amoOpSupported(funct5 uint32) bool {
	switch funct5 {
	case 0b00000, // AMOADD
		0b00001, // AMOSWAP
		0b00010, // LR
		0b00011, // SC
		0b00100, // AMOXOR
		0b01000, // AMOOR
		0b01100, // AMOAND
		0b10000, // AMOMIN
		0b10100, // AMOMAX
		0b11000, // AMOMINU
		0b11100: // AMOMAXU
		return true
	default:
		return false
	}
}

func (e *emitter) emitAMO(rd, rs1, rs2, funct3, funct5 uint32) {
	width := amoWidth(funct3)
	if width == 0 || !amoOpSupported(funct5) || !e.nativeReservationStateAvailable() {
		e.terminated = true
		return
	}
	if funct5 == 0b00010 && e.tryEmitLRSCFusion(rd, rs1, funct3) {
		return
	}

	e.emitBudgetCheck()
	addr := e.irEm.Tmp()
	e.irEm.Mov(addr, e.xreg(rs1))

	switch funct5 {
	case 0b00010: // LR.W / LR.D
		e.emitLR(rd, addr, width)
	case 0b00011: // SC.W / SC.D
		e.emitSC(rd, rs2, addr, width)
	default:
		e.emitOrdinaryAMO(rd, rs2, addr, width, funct5)
	}
	if !e.terminated {
		e.advancePC(4)
	}
}

func (e *emitter) tryEmitLRSCFusion(lrRD, rs1, lrFunct3 uint32) bool {
	if !e.allowInstructionFusion() || lrRD == rs1 {
		return false
	}
	next, ok := e.peek32(e.pc + 4)
	if !ok || next&0x7F != 0x2F {
		return false
	}
	scRD := (next >> 7) & 0x1F
	scFunct3 := (next >> 12) & 0x7
	scRS1 := (next >> 15) & 0x1F
	scRS2 := (next >> 20) & 0x1F
	scFunct5 := next >> 27
	if scFunct5 != 0b00011 || scFunct3 != lrFunct3 || scRS1 != rs1 {
		return false
	}
	width := amoWidth(lrFunct3)
	if width == 0 {
		return false
	}
	e.emitFusedLRSC(lrRD, scRD, rs1, scRS2, width)
	return true
}

func (e *emitter) emitFusedLRSC(lrRD, scRD, rs1, scRS2 uint32, width int) {
	e.emitBudgetReserve(2)
	addr := e.irEm.Tmp()
	e.irEm.Mov(addr, e.xreg(rs1))

	lrFault := e.allocFaultLabelAt(e.pc, addr, jitLoadFault)
	lrDst := e.irEm.Tmp()
	if lrRD != 0 {
		lrDst = e.xregDst(lrRD)
	}
	e.emitAMOAlignedLoad(lrDst, addr, width, width == 4, lrFault)

	storeVal := e.xreg(scRS2)
	scFault := e.allocFaultLabelAt(e.pc+4, addr, jitStoreFault)
	e.emitAMOAlignedStore(addr, storeVal, width, scFault)

	if scRD != 0 {
		e.irEm.Const(e.xregDst(scRD), 0)
	}
	e.clearReservation()
	e.advancePC(4)
	e.advancePC(4)
}

func (e *emitter) emitLR(rd uint32, addr VReg, width int) {
	faultLabel := e.allocFaultLabel(addr, jitLoadFault)
	dst := e.irEm.Tmp()
	if rd != 0 {
		dst = e.xregDst(rd)
	}
	e.emitAMOAlignedLoad(dst, addr, width, width == 4, faultLabel)
	base, addrOff, _ := e.reservationState()
	e.irEm.Store(base, addrOff, addr, I64)
	e.setReservationValid(true)
}

func (e *emitter) emitSC(rd, rs2 uint32, addr VReg, width int) {
	storeVal := e.xreg(rs2)
	failLabel := e.irEm.NewLabel()
	doneLabel := e.irEm.NewLabel()

	valid := e.irEm.Tmp()
	base, addrOff, validOff := e.reservationState()
	e.irEm.Load(valid, base, validOff, I64, false)
	e.irEm.Branch(valid, VRegZero, EQ, failLabel)
	resvAddr := e.irEm.Tmp()
	e.irEm.Load(resvAddr, base, addrOff, I64, false)
	e.irEm.Branch(resvAddr, addr, NE, failLabel)

	storeFault := e.allocFaultLabel(addr, jitStoreFault)
	e.emitAMOAlignedStore(addr, storeVal, width, storeFault)
	if rd != 0 {
		e.irEm.Const(e.xregDst(rd), 0)
	}
	e.clearReservation()
	e.irEm.Jump(doneLabel)

	e.irEm.PlaceLabel(failLabel)
	if rd != 0 {
		e.irEm.Const(e.xregDst(rd), 1)
	}
	e.clearReservation()
	e.irEm.PlaceLabel(doneLabel)
}

func (e *emitter) emitOrdinaryAMO(rd, rs2 uint32, addr VReg, width int, funct5 uint32) {
	loadFault := e.allocFaultLabel(addr, jitLoadFault)
	old := e.irEm.Tmp()
	e.emitAMOAlignedLoad(old, addr, width, false, loadFault)

	src := e.xreg(rs2)
	storeVal := e.emitAMOResultValue(old, src, width, funct5)
	storeFault := e.allocFaultLabel(addr, jitStoreFault)
	e.emitAMOAlignedStore(addr, storeVal, width, storeFault)

	if rd != 0 {
		dst := e.xregDst(rd)
		if width == 4 {
			e.irEm.Sext(dst, old, I32)
		} else {
			e.irEm.Mov(dst, old)
		}
	}
	e.clearReservation()
}

func (e *emitter) emitAMOResultValue(old, src VReg, width int, funct5 uint32) VReg {
	srcVal := src
	if width == 4 {
		srcVal = e.irEm.Tmp()
		e.irEm.Zext(srcVal, src, I32)
	}

	dst := e.irEm.Tmp()
	switch funct5 {
	case 0b00001: // AMOSWAP
		e.irEm.Mov(dst, srcVal)
	case 0b00000: // AMOADD
		e.irEm.Add(dst, old, srcVal)
	case 0b00100: // AMOXOR
		e.irEm.Xor(dst, old, srcVal)
	case 0b01100: // AMOAND
		e.irEm.And(dst, old, srcVal)
	case 0b01000: // AMOOR
		e.irEm.Or(dst, old, srcVal)
	case 0b10000: // AMOMIN
		if width == 4 {
			oldS := e.irEm.Tmp()
			srcS := e.irEm.Tmp()
			e.irEm.Sext(oldS, old, I32)
			e.irEm.Sext(srcS, srcVal, I32)
			e.emitSelect(dst, oldS, srcS, LT, old, srcVal)
		} else {
			e.emitSelect(dst, old, srcVal, LT, old, srcVal)
		}
	case 0b10100: // AMOMAX
		if width == 4 {
			oldS := e.irEm.Tmp()
			srcS := e.irEm.Tmp()
			e.irEm.Sext(oldS, old, I32)
			e.irEm.Sext(srcS, srcVal, I32)
			e.emitSelect(dst, oldS, srcS, GT, old, srcVal)
		} else {
			e.emitSelect(dst, old, srcVal, GT, old, srcVal)
		}
	case 0b11000: // AMOMINU
		e.emitSelect(dst, old, srcVal, LTU, old, srcVal)
	case 0b11100: // AMOMAXU
		e.emitSelect(dst, old, srcVal, GTU, old, srcVal)
	default:
		e.irEm.Mov(dst, old)
	}
	return dst
}

func (e *emitter) emitSelect(dst, cmpA, cmpB VReg, pred Pred, trueVal, falseVal VReg) {
	takeTrue := e.irEm.NewLabel()
	done := e.irEm.NewLabel()
	e.irEm.Branch(cmpA, cmpB, pred, takeTrue)
	e.irEm.Mov(dst, falseVal)
	e.irEm.Jump(done)
	e.irEm.PlaceLabel(takeTrue)
	e.irEm.Mov(dst, trueVal)
	e.irEm.PlaceLabel(done)
}

func (e *emitter) emitAMOAlignedLoad(dst, addr VReg, width int, signed bool, faultLabel Label) {
	e.emitAMOAlignmentCheck(addr, width, faultLabel)
	e.irEm.MaskedLoadAddr(dst, addr, e.irEm.MemBase(), e.irEm.MemMask(), width, signed, faultLabel)
}

func (e *emitter) emitAMOAlignedStore(addr, src VReg, width int, faultLabel Label) {
	e.emitAMOAlignmentCheck(addr, width, faultLabel)
	e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), src, width, faultLabel)
}

func (e *emitter) emitAMOAlignmentCheck(addr VReg, width int, faultLabel Label) {
	if width <= 1 {
		return
	}
	alignBits := e.irEm.Tmp()
	e.irEm.AndImm(alignBits, addr, int64(width-1))
	e.irEm.Branch(alignBits, VRegZero, NE, faultLabel)
}

func (e *emitter) setReservationValid(valid bool) {
	v := e.irEm.Tmp()
	if valid {
		e.irEm.Const(v, 1)
	} else {
		e.irEm.Const(v, 0)
	}
	base, _, validOff := e.reservationState()
	e.irEm.Store(base, validOff, v, I64)
}

func (e *emitter) clearReservation() {
	e.setReservationValid(false)
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
	if e.staticPageZeroFault(rs1, imm, width) {
		e.irEm.Jump(faultLabel)
		e.terminated = true
		return
	}
	if width > 1 {
		alignedLabel := e.irEm.NewLabel()
		doneLabel := e.irEm.NewLabel()
		alignBits := e.irEm.Tmp()
		e.irEm.AndImm(alignBits, addr, int64(width-1))
		e.irEm.Branch(alignBits, VRegZero, EQ, alignedLabel)
		// Strict Spike-style behavior would return jitMisalign here. GoCPU
		// intentionally emits bytewise accesses because compatibility tests
		// rely on permissive misaligned scalar loads.
		// OOB check for misaligned path (same as MaskedLoadAddr does for aligned).
		if CheckSandboxBounds && e.irEm.SandboxMem() {
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
	if e.irEm.LinearMem() {
		return
	}
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

func (e *emitter) staticPageZeroFault(rs1 uint32, imm int64, width int) bool {
	if e.irEm.j == nil || !e.irEm.j.faultPageZero || rs1 != 0 || imm < 0 {
		return false
	}
	return uint64(imm)+uint64(width) <= GuestPageSize
}

// emitMisalignedLoad emits byte-by-byte loads for a misaligned address.
// addr is a VReg holding the guest virtual address (already computed).
// faultLabel is the per-call-site fault tail (already registered with addr).
func (e *emitter) emitMisalignedLoad(dst VReg, addr VReg, width int, signed bool, faultLabel Label) {
	if e.irEm.LinearMem() {
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
		return
	}

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
	if e.staticPageZeroFault(rs1, imm, width) {
		e.irEm.Jump(faultLabel)
		e.terminated = true
		return
	}
	if width > 1 {
		alignedLabel := e.irEm.NewLabel()
		doneLabel := e.irEm.NewLabel()
		alignBits := e.irEm.Tmp()
		e.irEm.AndImm(alignBits, addr, int64(width-1))
		e.irEm.Branch(alignBits, VRegZero, EQ, alignedLabel)
		// Strict Spike-style behavior would return jitMisalign here. The
		// bytewise store path is intentionally kept for compatibility.
		if CheckSandboxBounds && e.irEm.SandboxMem() {
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
	if e.irEm.LinearMem() {
		t := WidthToType(width)
		e.irEm.MisalignedStore(addr, src, t)
		return
	}

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
	if CheckSandboxBounds && e.irEm.SandboxMem() {
		e.emitOOBCheck(addr, width, faultLabel)
	}
	t := WidthToType(width)
	tmp := e.irEm.Tmp()
	e.irEm.MisalignedLoad(tmp, addr, t)
	if funct3 == 2 {
		e.boxF32(rd, tmp)
	} else {
		e.storeFRegBits(rd, tmp)
	}
	e.irEm.Jump(doneLabel)

	// Aligned path.
	e.irEm.PlaceLabel(alignedLabel)
	if funct3 == 2 {
		tmp2 := e.irEm.Tmp()
		e.irEm.MaskedLoadAddr(tmp2, addr, e.irEm.MemBase(), e.irEm.MemMask(), 4, false, faultLabel)
		e.boxF32(rd, tmp2)
	} else {
		tmp2 := e.irEm.Tmp()
		e.irEm.MaskedLoadAddr(tmp2, addr, e.irEm.MemBase(), e.irEm.MemMask(), 8, false, faultLabel)
		e.storeFRegBits(rd, tmp2)
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
		e.storeFRegBits(rd, tmp)
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
	if CheckSandboxBounds && e.irEm.SandboxMem() {
		e.emitOOBCheck(addr, width, faultLabel)
	}
	t := WidthToType(width)
	src := e.loadFRegBits(rs2)
	if funct3 == 2 {
		tmp := e.irEm.Tmp()
		e.irEm.Zext(tmp, src, I32)
		e.irEm.MisalignedStore(addr, tmp, t)
	} else {
		e.irEm.MisalignedStore(addr, src, t)
	}
	e.irEm.Jump(doneLabel)

	// Aligned path.
	e.irEm.PlaceLabel(alignedLabel)
	src2 := e.loadFRegBits(rs2)
	if funct3 == 2 {
		tmp := e.irEm.Tmp()
		e.irEm.Zext(tmp, src2, I32)
		e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), tmp, 4, faultLabel)
	} else {
		e.irEm.GuestStoreAddr(addr, e.irEm.MemBase(), e.irEm.MemMask(), src2, 8, faultLabel)
	}
	e.irEm.PlaceLabel(doneLabel)
}

// emitMisalignedFPStore emits a byte-by-byte FP store (FSW or FSD) at a
// misaligned address. faultLabel is the per-call-site fault tail.
func (e *emitter) emitMisalignedFPStore(rs2 uint32, addr VReg, width int, faultLabel Label) {
	srcBits := e.loadFRegBits(rs2)
	if width == 4 { // FSW: extract low 32 bits, then byte-by-byte
		src := e.irEm.Tmp()
		e.irEm.Zext(src, srcBits, I32)
		e.emitMisalignedStore(addr, src, 4, faultLabel)
	} else { // FSD: store all 64 bits
		e.emitMisalignedStore(addr, srcBits, 8, faultLabel)
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
		a := e.unboxF64(rs1)
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
				e.irEm.Sext(e.xregDst(rd), e.loadFRegBits(rs1), I32)
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
	e.storeFRegBits(rd, intBits)
}

// unboxF64 loads the 64-bit double from f[rs] as F64 suitable for
// ADDSD/SUBSD/etc. Avoids the FP-VReg typing hazard.
func (e *emitter) unboxF64(rs uint32) VReg {
	em := e.irEm
	intBits := e.loadFRegBits(rs)
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
				e.irEm.Mov(e.xregDst(rd), e.loadFRegBits(rs1))
			}
		default:
			e.terminated = true
		}
	case 0x1E: // FMV.D.X
		e.storeFRegBits(rd, e.xreg(rs1))
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
	a := e.loadFRegBits(rs1)
	b := e.loadFRegBits(rs2)
	signMask := e.irEm.Tmp()
	e.irEm.Const(signMask, -9223372036854775808) // 0x8000000000000000 as int64 (math.MinInt64)
	absMask := e.irEm.Tmp()
	e.irEm.Const(absMask, 9223372036854775807) // 0x7FFFFFFFFFFFFFFF as int64 (math.MaxInt64)
	result := e.irEm.Tmp()

	switch funct3 {
	case 0: // FSGNJ.D
		t1 := e.irEm.Tmp()
		e.irEm.And(t1, a, absMask)
		t2 := e.irEm.Tmp()
		e.irEm.And(t2, b, signMask)
		e.irEm.Or(result, t1, t2)
	case 1: // FSGNJN.D
		t1 := e.irEm.Tmp()
		e.irEm.And(t1, a, absMask)
		t2 := e.irEm.Tmp()
		e.irEm.Not(t2, b)
		e.irEm.And(t2, t2, signMask)
		e.irEm.Or(result, t1, t2)
	case 2: // FSGNJX.D
		t2 := e.irEm.Tmp()
		e.irEm.And(t2, b, signMask)
		e.irEm.Xor(result, a, t2)
	default:
		e.terminated = true
		return
	}
	e.storeFRegBits(rd, result)
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
	a := e.unboxF64(rs1)
	b := e.unboxF64(rs2)
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
	a := e.unboxF64(rs1)
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
	switch rs2 {
	case 0:
		t := e.irEm.Tmp()
		e.irEm.Sext(t, s, I32)
		result := e.irEm.Tmp()
		e.irEm.FCvtFromI(result, t, I32, F64)
		e.boxF64(rd, result)
	case 1:
		t := e.irEm.Tmp()
		e.irEm.Zext(t, s, I32)
		result := e.irEm.Tmp()
		e.irEm.FCvtFromU(result, t, I32, F64)
		e.boxF64(rd, result)
	case 2:
		result := e.irEm.Tmp()
		e.irEm.FCvtFromI(result, s, I64, F64)
		e.boxF64(rd, result)
	case 3:
		result := e.irEm.Tmp()
		e.irEm.FCvtFromU(result, s, I64, F64)
		e.boxF64(rd, result)
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
			e.irEm.StopperLoad(e.stopperAddr)
			e.irEm.Jump(targetLabel)
			e.gotoTargets.add(target)
			e.terminated = true
			return
		}
		e.irEm.Jump(targetLabel)
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
		e.spillIC()
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
	e.spillIC()
	e.irEm.DynChainableRet(tgt, e.jalrSiteIdx)
	e.jalrSiteIdx++
	e.terminated = true
}

func (e *emitter) emitBranch(rs1, rs2, funct3 uint32, offset int64) {
	target := e.pc + uint64(offset)
	pred, _ := branchPred(funct3)

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
			e.irEm.Jump(continueLabel)
			e.irEm.PlaceLabel(takenLabel)
			e.irEm.StopperLoad(e.stopperAddr)
			e.irEm.Jump(targetLabel)
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
	savedNumInsns := e.numInsns
	e.emitLabel()
	e.emitBudgetCheck()
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

	if e.terminated && e.numInsns == savedNumInsns {
		e.undoBudgetCheck()
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
			e.emitOpImm(rs1, rs1, 0x400|shamt, 5, 0x20)
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
				e.countInsn()
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
