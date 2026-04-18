//go:build !tcc

package riscv

// jit_emit_ir.go — Translates RISC-V basic blocks to IR (ir.Block).
// This replaces jit_emit.go's C source generation with IR emission.

import "riscv/ir"

// testIterStart is set by tests to rotate gotoTargets iteration order.
// Zero means normal sorted order. Non-zero rotates by this offset.
var testIterStart int

// emitResult holds the generated IR block and metadata.
type emitResult struct {
	block    *ir.Block
	startPC  uint64
	endPC    uint64
	numInsns int
	regsUsed [32]bool
}

// deferredExit holds an external branch exit to emit at finalize time.
type deferredExit struct {
	label    ir.Label
	targetPC uint64
}

// emitter accumulates IR for a basic block.
type emitter struct {
	mem        *GuestMemory
	startPC    uint64
	pc         uint64
	irEm       *ir.Emitter
	numInsns   int
	regsUsed   [32]bool
	terminated bool
	visited     u64set
	regionEnd   uint64
	gotoTargets u64set
	pcLabels    u64labelmap
	icEmitted  bool
	loadFaultLabel  ir.Label
	storeFaultLabel ir.Label
	deferredExits []deferredExit
}

// ── Register access helpers ────────────────────────────────────────────

// xreg returns the VReg for integer register r and marks it as used.
func (e *emitter) xreg(r uint32) ir.VReg {
	if r == 0 {
		return ir.VRegZero
	}
	e.regsUsed[r] = true
	return e.irEm.XReg(r)
}

// xregDst returns the VReg for integer register r (write destination).
// Marks the register dirty so WriteBackAll stores it at block exit.
func (e *emitter) xregDst(r uint32) ir.VReg {
	if r == 0 {
		return ir.VRegZero
	}
	e.regsUsed[r] = true
	vr := e.irEm.XReg(r)
	e.irEm.MarkDirty(vr)
	return vr
}

// freg returns the VReg for FP register r.
func (e *emitter) freg(r uint32) ir.VReg {
	return e.irEm.FRegV(r)
}

// fregDst returns the VReg for FP register r (write destination).
// Marks the register dirty so WriteBackAll stores it at block exit.
func (e *emitter) fregDst(r uint32) ir.VReg {
	vr := e.irEm.FRegV(r)
	e.irEm.MarkDirty(vr)
	return vr
}

// ── NaN-boxing helpers ─────────────────────────────────────────────────

// boxF32 NaN-boxes a 32-bit value into f[rd].
func (e *emitter) boxF32(rd uint32, val ir.VReg) {
	dst := e.fregDst(rd)
	masked := e.irEm.Tmp()
	e.irEm.AndImm(masked, val, 0xFFFFFFFF)
	hi := e.irEm.Tmp()
	e.irEm.Const(hi, -4294967296) // 0xFFFFFFFF00000000 as int64
	e.irEm.Or(dst, masked, hi)
}

// unboxF32 extracts 32-bit float bits; canonical NaN if improperly boxed.
func (e *emitter) unboxF32(rs uint32) ir.VReg {
	src := e.freg(rs)
	upper := e.irEm.Tmp()
	e.irEm.ShrImm(upper, src, 32)
	check := e.irEm.Tmp()
	e.irEm.Const(check, 0xFFFFFFFF)
	result := e.irEm.Tmp()
	okLabel := e.irEm.NewLabel()
	doneLabel := e.irEm.NewLabel()
	e.irEm.Branch(upper, check, ir.EQ, okLabel)
	e.irEm.Const(result, 0x7FC00000) // canonical NaN
	e.irEm.Jump(doneLabel)
	e.irEm.PlaceLabel(okLabel)
	e.irEm.Zext(result, src, ir.I32)
	e.irEm.PlaceLabel(doneLabel)
	return result
}

// ── Control flow helpers ───────────────────────────────────────────────

func (e *emitter) getOrCreateLabel(pc uint64) ir.Label {
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
	e.irEm.Ret(pc, status, ir.VRegZero)
}

func (e *emitter) emitWriteBackAll() {
	e.irEm.WriteBackAll()
}

// emitDivGuarded emits a 64-bit DIV/DIVU/REM/REMU with zero-divisor and
// overflow guards per the RISC-V spec (no x86 fault on divide-by-zero).
func (e *emitter) emitDivGuarded(dst, a, b ir.VReg, signed, wantRem bool) {
	em := e.irEm
	doneLabel := em.NewLabel()
	normalLabel := em.NewLabel()

	// Check divisor == 0.
	zeroLabel := em.NewLabel()
	em.Branch(b, ir.VRegZero, ir.EQ, zeroLabel)

	if signed {
		// Check signed overflow: a == INT64_MIN && b == -1.
		ovfLabel := em.NewLabel()
		tmin := em.Tmp()
		em.Const(tmin, -9223372036854775808) // INT64_MIN
		em.Branch(a, tmin, ir.NE, normalLabel)
		tminus1 := em.Tmp()
		em.Const(tminus1, -1)
		em.Branch(b, tminus1, ir.NE, normalLabel)
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
func (e *emitter) emitDivW(dst, a, b ir.VReg, signed, wantRem bool) {
	em := e.irEm
	doneLabel := em.NewLabel()
	normalLabel := em.NewLabel()

	// Truncate operands to 32 bits.
	a32 := em.Tmp()
	b32 := em.Tmp()
	if signed {
		em.Sext(a32, a, ir.I32)
		em.Sext(b32, b, ir.I32)
	} else {
		em.Zext(a32, a, ir.I32)
		em.Zext(b32, b, ir.I32)
	}

	// Check divisor == 0.
	zeroLabel := em.NewLabel()
	em.Branch(b32, ir.VRegZero, ir.EQ, zeroLabel)

	if signed {
		// Check signed overflow: a32 == INT32_MIN && b32 == -1.
		ovfLabel := em.NewLabel()
		tmin := em.Tmp()
		em.Const(tmin, -2147483648) // INT32_MIN (sign-extended to 64-bit)
		em.Branch(a32, tmin, ir.NE, normalLabel)
		tminus1 := em.Tmp()
		em.Const(tminus1, -1)
		em.Branch(b32, tminus1, ir.NE, normalLabel)
		em.PlaceLabel(ovfLabel)
		if wantRem {
			em.Const(dst, 0)
		} else {
			em.Sext(dst, a32, ir.I32) // dividend, sign-extended
		}
		em.Jump(doneLabel)
	} else {
		em.Jump(normalLabel)
	}

	// Zero-divisor path.
	em.PlaceLabel(zeroLabel)
	if wantRem {
		em.Sext(dst, a32, ir.I32) // REMW(x,0) → x, sign-extended
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
	em.Sext(dst, t, ir.I32)

	em.PlaceLabel(doneLabel)
}

// emitFMinMax emits FMIN or FMAX with NaN handling per RISC-V spec:
// if either operand is NaN, return the other (canonical NaN if both are NaN).
// For FMIN: return the smaller; for FMAX: return the larger.
func (e *emitter) emitFMinMax(dst, a, b ir.VReg, t ir.Type, isMax bool) {
	em := e.irEm
	// Check if a is NaN: a != a
	aNaN := em.Tmp()
	em.FCmp(aNaN, a, a, ir.EQ, t) // 0 if NaN, 1 if not
	// Check if b is NaN: b != b
	bNaN := em.Tmp()
	em.FCmp(bNaN, b, b, ir.EQ, t) // 0 if NaN, 1 if not

	doneLabel := em.NewLabel()
	aIsNaN := em.NewLabel()
	bIsNaN := em.NewLabel()
	pickA := em.NewLabel()

	em.Branch(aNaN, ir.VRegZero, ir.EQ, aIsNaN)
	em.Branch(bNaN, ir.VRegZero, ir.EQ, bIsNaN)

	// Both are numbers — compare.
	cmp := em.Tmp()
	if isMax {
		em.FCmp(cmp, a, b, ir.GT, t)
	} else {
		em.FCmp(cmp, a, b, ir.LT, t)
	}
	em.Branch(cmp, ir.VRegZero, ir.NE, pickA)
	// Pick b.
	em.Mov(dst, b)
	em.Jump(doneLabel)

	em.PlaceLabel(pickA)
	em.Mov(dst, a)
	em.Jump(doneLabel)

	em.PlaceLabel(aIsNaN)
	em.Mov(dst, b) // a is NaN → return b
	em.Jump(doneLabel)

	em.PlaceLabel(bIsNaN)
	em.Mov(dst, a) // b is NaN → return a

	em.PlaceLabel(doneLabel)
}

func branchPred(funct3 uint32) (ir.Pred, bool) {
	switch funct3 {
	case 0:
		return ir.EQ, true
	case 1:
		return ir.NE, true
	case 4:
		return ir.LT, true
	case 5:
		return ir.GE, true
	case 6:
		return ir.LTU, true
	case 7:
		return ir.GEU, true
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
	// Fall-through return. Always emitted — for blocks that already have an
	// explicit Ret (ECALL, EBREAK, JAL) this is dead code. For blocks that
	// terminated due to untranslatable instructions (CSR, unknown opcode),
	// this IS the return path.
	e.irEm.WriteBackAll()
	e.irEm.Ret(e.pc, jitOK, ir.VRegZero)

	// Bail labels: goto targets not emitted. Deterministic order is critical —
	// random order changes IR indices, affecting register allocation.
	e.gotoTargets.each(func(target uint64) {
		if !e.visited.has(target) {
			e.irEm.PlaceLabel(e.getOrCreateLabel(target))
			e.irEm.WriteBackAll()
			e.irEm.Ret(target, jitOK, ir.VRegZero)
		}
	})

	// Deferred external exits.
	for _, de := range e.deferredExits {
		e.irEm.PlaceLabel(de.label)
		e.irEm.WriteBackAll()

		e.irEm.Ret(de.targetPC, jitOK, ir.VRegZero)
	}

	return &emitResult{
		block:    e.irEm.Block,
		startPC:  e.startPC,
		endPC:    e.pc,
		numInsns: e.numInsns,
		regsUsed: e.regsUsed,
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
					if rd != 0 { used[rd] = true }
				case 0b011: // ADDI16SP/LUI
					rd := (insn >> 7) & 0x1F
					if rd != 0 { used[rd] = true }
					if rd == 2 { used[2] = true }
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
				if rd != 0 { used[rd] = true }
				if rs2 != 0 { used[rs2] = true }
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
				if rd != 0 { used[rd] = true }
			}
			// Mark rs1 (most formats use bits[19:15] for rs1).
			// FENCE (0x0F), LUI (0x37), AUIPC (0x17), JAL (0x6F) don't have rs1.
			switch opcode {
			case 0x0F, 0x37, 0x17, 0x6F:
				// no rs1
			default:
				if rs1 != 0 { used[rs1] = true }
			}
			// Mark rs2 for formats that use it.
			switch opcode {
			case 0x33, 0x3B: // OP, OP-32
				if rs2 != 0 { used[rs2] = true }
			case 0x63: // BRANCH
				if rs1 != 0 { used[rs1] = true }
				if rs2 != 0 { used[rs2] = true }
			case 0x23: // STORE
				if rs1 != 0 { used[rs1] = true }
				if rs2 != 0 { used[rs2] = true }
			case 0x27: // FP-STORE
				if rs1 != 0 { used[rs1] = true }
				// rs2 is FP register index, not int
			case 0x43, 0x47, 0x4B, 0x4F: // FMA
				if rs2 != 0 { used[rs2] = true }
				rs3 := insn >> 27
				if rs3 != 0 { used[rs3] = true }
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

func emitBlock(mem *GuestMemory, pc uint64) *emitResult {
	region := scanRegion(mem, pc)
	if region.pcCount == 0 {
		return nil
	}

	irEm := ir.NewEmitter()

	gt := newU64set()
	gt.IterStart = testIterStart

	e := &emitter{
		mem:         mem,
		startPC:     pc,
		pc:          pc,
		irEm:        irEm,
		visited:     newU64set(),
		regionEnd:   region.endPC,
		gotoTargets: gt,
		pcLabels:    newU64labelmap(),
	}

	// Pre-allocate fault labels.
	e.loadFaultLabel = irEm.NewLabel()
	e.storeFaultLabel = irEm.NewLabel()

	// Emit IR (populates regsUsed via xreg/xregDst calls).
	const maxBlockInsns = 3
	for e.numInsns < maxBlockInsns && !e.terminated && e.pc < e.regionEnd {
		if e.visited.has(e.pc) {
			e.irEm.Jump(e.getOrCreateLabel(e.pc))
			e.gotoTargets.add(e.pc)
			e.terminated = true
			break
		}
		e.visited.add(e.pc)

		half, fh := mem.Fetch16(e.pc)
		if fh != nil {
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
					break
				}
			}
			e.emit32(insn)
		}
	}

	if e.numInsns == 0 {
		return nil
	}

	// Prepend loads for registers that were actually used during emission.
	// This must happen AFTER emission so regsUsed is fully populated.
	var loads []ir.IRInstr
	for i := uint32(1); i < 32; i++ {
		if e.regsUsed[i] {
			loads = append(loads, ir.IRInstr{
				Op: ir.IRLoad, T: ir.I64,
				Dst: ir.VReg(i), A: irEm.XBase(),
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
		if rd != 0 {
			e.irEm.Const(e.xregDst(rd), int64(e.pc)+uimm)
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
		case 0x00000073: // ECALL
			e.advancePC(4)
			e.emitReturn(e.pc, jitEcall)
			e.terminated = true
		case 0x00100073: // EBREAK
			e.advancePC(4)
			e.emitReturn(e.pc, jitEbreak)
			e.terminated = true
		default:
			// CSR or unknown — end block before this instruction.
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
				e.irEm.Clz(e.xregDst(rd), src, ir.I64)
			case 1: // CTZ
				src := e.xreg(rs1)
				e.irEm.Ctz(e.xregDst(rd), src, ir.I64)
			case 2: // CPOP
				src := e.xreg(rs1)
				e.irEm.Popcount(e.xregDst(rd), src, ir.I64)
			case 0x22: // SEXT.B
				src := e.xreg(rs1)
				e.irEm.Sext(e.xregDst(rd), src, ir.I8)
			case 0x23: // SEXT.H
				src := e.xreg(rs1)
				e.irEm.Sext(e.xregDst(rd), src, ir.I16)
			default:
				e.terminated = true
			}
		default:
			e.terminated = true
		}
	case 2: // SLTI
		src := e.xreg(rs1)
		e.irEm.SetImm(e.xregDst(rd), src, imm, ir.LT)
	case 3: // SLTIU
		src := e.xreg(rs1)
		e.irEm.SetImm(e.xregDst(rd), src, imm, ir.LTU)
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
				e.irEm.Set(ne, byteVal, ir.VRegZero, ir.NE)
				e.irEm.Neg(mask, ne)           // 0→0, 1→-1 (0xFFFF...)
				e.irEm.AndImm(mask, mask, 0xFF) // keep only low byte = 0xFF or 0
				e.irEm.ShlImm(mask, mask, int64(i*8))
				e.irEm.Or(dst, dst, mask)
			}
		case 0x1A: // REV8 — byte-swap via BSWAP
			src := e.xreg(rs1)
			e.irEm.Bswap(e.xregDst(rd), src)
		case 0x02: // ZEXT.H
			src := e.xreg(rs1)
			e.irEm.Zext(e.xregDst(rd), src, ir.I16)
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
		src := e.xreg(rs1)
		dst := e.xregDst(rd)
		if imm == 0 {
			// SEXT.W
			e.irEm.Sext(dst, src, ir.I32)
		} else {
			t := e.irEm.Tmp()
			e.irEm.AddImm(t, src, imm)
			e.irEm.Sext(dst, t, ir.I32)
		}
	case 1: // SLLIW / SLLI.UW
		if funct7 == 0x04 { // SLLI.UW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), ir.I32)
			e.irEm.ShlImm(e.xregDst(rd), t, shamt)
		} else { // SLLIW
			t := e.irEm.Tmp()
			e.irEm.ShlImm(t, e.xreg(rs1), shamt)
			e.irEm.Sext(e.xregDst(rd), t, ir.I32)
		}
	case 5: // SRLIW / SRAIW / RORIW
		switch funct7 >> 1 {
		case 0x00: // SRLIW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, e.xreg(rs1), ir.I32) // zero-extend to get uint32
			e.irEm.ShrImm(t, t, shamt)
			e.irEm.Sext(e.xregDst(rd), t, ir.I32)
		case 0x10: // SRAIW
			t := e.irEm.Tmp()
			e.irEm.Sext(t, e.xreg(rs1), ir.I32) // sign-extend to get int32
			e.irEm.SarImm(t, t, shamt)
			e.irEm.Sext(e.xregDst(rd), t, ir.I32)
		case 0x30: // RORIW — word rotate right immediate
			src := e.xreg(rs1)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, src, ir.I32)
			t1 := e.irEm.Tmp()
			e.irEm.ShrImm(t1, t, shamt)
			t2 := e.irEm.Tmp()
			e.irEm.ShlImm(t2, t, 32-shamt)
			e.irEm.Or(t1, t1, t2)
			e.irEm.Sext(e.xregDst(rd), t1, ir.I32)
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
			e.irEm.Set(dst, a, b, ir.LT)
		case 3:
			e.irEm.Set(dst, a, b, ir.LTU)
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
		e.irEm.Zext(dst, a, ir.I16)
	case 0x05: // MIN/MAX (Zbb) + CLMUL (Zbc)
		switch funct3 {
		case 4: // MIN
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, ir.LT)
			e.irEm.Branch(t, ir.VRegZero, ir.NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 5: // MINU
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, ir.LTU)
			e.irEm.Branch(t, ir.VRegZero, ir.NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 6: // MAX
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, ir.GT)
			e.irEm.Branch(t, ir.VRegZero, ir.NE, takeA)
			e.irEm.Mov(dst, b)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(takeA)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 7: // MAXU
			takeA := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			t := e.irEm.Tmp()
			e.irEm.Set(t, a, b, ir.GTU)
			e.irEm.Branch(t, ir.VRegZero, ir.NE, takeA)
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
			e.irEm.Branch(b, ir.VRegZero, ir.NE, skip)
			e.irEm.Const(dst, 0)
			e.irEm.Jump(done)
			e.irEm.PlaceLabel(skip)
			e.irEm.Mov(dst, a)
			e.irEm.PlaceLabel(done)
		case 7: // CZERO.NEZ: d = (b != 0) ? 0 : a
			skip := e.irEm.NewLabel()
			done := e.irEm.NewLabel()
			e.irEm.Branch(b, ir.VRegZero, ir.EQ, skip)
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
			e.irEm.Sext(dst, t, ir.I32)
		case 1: // SLLW — shift amount masked to 5 bits (not 6)
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.Shl(t, t, shamt)
			e.irEm.Sext(dst, t, ir.I32)
		case 5: // SRLW — shift amount masked to 5 bits
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.Shr(t, t, shamt)
			e.irEm.Sext(dst, t, ir.I32)
		default:
			e.terminated = true
		}
	case 0x20:
		switch funct3 {
		case 0: // SUBW
			t := e.irEm.Tmp()
			e.irEm.Sub(t, a, b)
			e.irEm.Sext(dst, t, ir.I32)
		case 5: // SRAW — shift amount masked to 5 bits
			shamt := e.irEm.Tmp()
			e.irEm.AndImm(shamt, b, 31)
			t := e.irEm.Tmp()
			e.irEm.Sext(t, a, ir.I32)
			e.irEm.Sar(t, t, shamt)
			e.irEm.Sext(dst, t, ir.I32)
		default:
			e.terminated = true
		}
	case 0x01: // M extension (word)
		switch funct3 {
		case 0: // MULW
			t := e.irEm.Tmp()
			e.irEm.Mul(t, a, b)
			e.irEm.Sext(dst, t, ir.I32)
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
		e.irEm.Zext(t, a, ir.I32)
		e.irEm.Add(dst, b, t)
	case 0x30: // Zbb: ROLW/RORW
		switch funct3 {
		case 1: // ROLW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
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
			e.irEm.Sext(dst, t1, ir.I32)
		case 5: // RORW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
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
			e.irEm.Sext(dst, t1, ir.I32)
		default:
			e.terminated = true
		}
	case 0x60: // Zbb: CLZW/CTZW/CPOPW
		switch funct3 {
		case 0: // CLZW (rs2 encoding = 0)
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.Clz(dst, t, ir.I32)
		case 1: // CTZW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.Ctz(dst, t, ir.I32)
		case 2: // CPOPW
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.Popcount(dst, t, ir.I32)
		default:
			e.terminated = true
		}
	case 0x10: // Zba: SH1ADD.UW / SH2ADD.UW / SH3ADD.UW
		switch funct3 {
		case 2:
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.ShlImm(t, t, 1)
			e.irEm.Add(dst, b, t)
		case 4:
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
			e.irEm.ShlImm(t, t, 2)
			e.irEm.Add(dst, b, t)
		case 6:
			t := e.irEm.Tmp()
			e.irEm.Zext(t, a, ir.I32)
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

	if width > 1 {
		addr := e.irEm.Tmp()
		e.irEm.AddImm(addr, base, imm)
		misalignLabel := e.irEm.NewLabel()
		alignBits := e.irEm.Tmp()
		e.irEm.AndImm(alignBits, addr, int64(width-1))
		e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, misalignLabel)
		e.irEm.MaskedLoad(dst, base, e.irEm.MemBase(), e.irEm.MemMask(), imm, width, signed, e.loadFaultLabel)
		doneLabel := e.irEm.NewLabel()
		e.irEm.Jump(doneLabel)
		e.irEm.PlaceLabel(misalignLabel)
		e.irEm.WriteBackAll()
		e.irEm.Ret(e.pc, jitOK, ir.VRegZero) // bail to interp
		e.irEm.PlaceLabel(doneLabel)
	} else {
		e.irEm.MaskedLoad(dst, base, e.irEm.MemBase(), e.irEm.MemMask(), imm, 1, signed, e.loadFaultLabel)
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

	if width > 1 {
		addr := e.irEm.Tmp()
		e.irEm.AddImm(addr, base, imm)
		misalignLabel := e.irEm.NewLabel()
		alignBits := e.irEm.Tmp()
		e.irEm.AndImm(alignBits, addr, int64(width-1))
		e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, misalignLabel)
		e.irEm.GuestStore(base, e.irEm.MemBase(), e.irEm.MemMask(), imm, src, width, e.storeFaultLabel)
		doneLabel := e.irEm.NewLabel()
		e.irEm.Jump(doneLabel)
		e.irEm.PlaceLabel(misalignLabel)
		e.irEm.WriteBackAll()
		e.irEm.Ret(e.pc, jitOK, ir.VRegZero)
		e.irEm.PlaceLabel(doneLabel)
	} else {
		e.irEm.GuestStore(base, e.irEm.MemBase(), e.irEm.MemMask(), imm, src, 1, e.storeFaultLabel)
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
	// FP loads fault on misalignment — combine alignment + OOB into one check.
	addr := e.irEm.Tmp()
	e.irEm.AddImm(addr, base, imm)
	alignBits := e.irEm.Tmp()
	e.irEm.AndImm(alignBits, addr, int64(width-1))
	e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, e.loadFaultLabel)

	if funct3 == 2 { // FLW — load 32 bits, NaN-box
		tmp := e.irEm.Tmp()
		e.irEm.MaskedLoad(tmp, base, e.irEm.MemBase(), e.irEm.MemMask(), imm, 4, false, e.loadFaultLabel)
		e.boxF32(rd, tmp)
	} else { // FLD
		e.irEm.MaskedLoad(e.fregDst(rd), base, e.irEm.MemBase(), e.irEm.MemMask(), imm, 8, false, e.loadFaultLabel)
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
	alignBits := e.irEm.Tmp()
	e.irEm.AndImm(alignBits, addr, int64(width-1))
	e.irEm.Branch(alignBits, ir.VRegZero, ir.NE, e.storeFaultLabel)

	if funct3 == 2 { // FSW — extract low 32 bits
		tmp := e.irEm.Tmp()
		e.irEm.Zext(tmp, e.freg(rs2), ir.I32)
		e.irEm.GuestStore(base, e.irEm.MemBase(), e.irEm.MemMask(), imm, tmp, 4, e.storeFaultLabel)
	} else { // FSD
		e.irEm.GuestStore(base, e.irEm.MemBase(), e.irEm.MemMask(), imm, e.freg(rs2), 8, e.storeFaultLabel)
	}
}

// ── FMA family ─────────────────────────────────────────────────────────

func (e *emitter) emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt uint32) {
	if fpfmt > 1 {
		e.terminated = true
		return
	}

	if fpfmt == 0 { // single
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		c := e.unboxF32(rs3)
		mul := e.irEm.Tmp()
		e.irEm.FMul(mul, a, b, ir.F32)

		var result ir.VReg
		switch opcode {
		case 0x43: // FMADD: a*b + c
			result = e.irEm.Tmp()
			e.irEm.FAdd(result, mul, c, ir.F32)
		case 0x47: // FMSUB: a*b - c
			result = e.irEm.Tmp()
			e.irEm.FSub(result, mul, c, ir.F32)
		case 0x4B: // FNMSUB: -(a*b) + c = c - a*b
			result = e.irEm.Tmp()
			e.irEm.FSub(result, c, mul, ir.F32)
		case 0x4F: // FNMADD: -(a*b) - c
			neg := e.irEm.Tmp()
			e.irEm.FNeg(neg, mul, ir.F32)
			result = e.irEm.Tmp()
			e.irEm.FSub(result, neg, c, ir.F32)
		}
		e.boxF32(rd, result)
	} else { // double
		a := e.freg(rs1)
		b := e.freg(rs2)
		c := e.freg(rs3)
		mul := e.irEm.Tmp()
		e.irEm.FMul(mul, a, b, ir.F64)
		dst := e.fregDst(rd)

		switch opcode {
		case 0x43:
			e.irEm.FAdd(dst, mul, c, ir.F64)
		case 0x47:
			e.irEm.FSub(dst, mul, c, ir.F64)
		case 0x4B:
			e.irEm.FSub(dst, c, mul, ir.F64)
		case 0x4F:
			neg := e.irEm.Tmp()
			e.irEm.FNeg(neg, mul, ir.F64)
			e.irEm.FSub(dst, neg, c, ir.F64)
		}
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
		e.irEm.FAdd(result, a, b, ir.F32)
		e.boxF32(rd, result)
	case 0x01: // FSUB.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FSub(result, a, b, ir.F32)
		e.boxF32(rd, result)
	case 0x02: // FMUL.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FMul(result, a, b, ir.F32)
		e.boxF32(rd, result)
	case 0x03: // FDIV.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.irEm.FDiv(result, a, b, ir.F32)
		e.boxF32(rd, result)
	case 0x0B: // FSQRT.S
		a := e.unboxF32(rs1)
		result := e.irEm.Tmp()
		e.irEm.FSqrt(result, a, ir.F32)
		e.boxF32(rd, result)
	case 0x04: // FSGNJ.S / FSGNJN.S / FSGNJX.S
		e.emitFsgnjS(rd, rs1, rs2, funct3)
	case 0x05: // FMIN.S / FMAX.S
		a := e.unboxF32(rs1)
		b := e.unboxF32(rs2)
		result := e.irEm.Tmp()
		e.emitFMinMax(result, a, b, ir.F32, funct3 == 1) // funct3=0: MIN, 1: MAX
		e.boxF32(rd, result)
	case 0x08: // FCVT.S.D
		a := e.freg(rs1)
		result := e.irEm.Tmp()
		e.irEm.FCvtFF(result, a, ir.F64, ir.F32)
		e.boxF32(rd, result)
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
				e.irEm.Sext(e.xregDst(rd), e.freg(rs1), ir.I32)
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

func (e *emitter) emitFPOpD(rd, rs1, rs2, funct3, funct5 uint32) {
	switch funct5 {
	case 0x00: // FADD.D
		a := e.freg(rs1)
		b := e.freg(rs2)
		e.irEm.FAdd(e.fregDst(rd), a, b, ir.F64)
	case 0x01: // FSUB.D
		a := e.freg(rs1)
		b := e.freg(rs2)
		e.irEm.FSub(e.fregDst(rd), a, b, ir.F64)
	case 0x02: // FMUL.D
		a := e.freg(rs1)
		b := e.freg(rs2)
		e.irEm.FMul(e.fregDst(rd), a, b, ir.F64)
	case 0x03: // FDIV.D
		a := e.freg(rs1)
		b := e.freg(rs2)
		e.irEm.FDiv(e.fregDst(rd), a, b, ir.F64)
	case 0x0B: // FSQRT.D
		a := e.freg(rs1)
		e.irEm.FSqrt(e.fregDst(rd), a, ir.F64)
	case 0x04: // FSGNJ.D
		e.emitFsgnjD(rd, rs1, rs2, funct3)
	case 0x05: // FMIN.D / FMAX.D
		a := e.freg(rs1)
		b := e.freg(rs2)
		e.emitFMinMax(e.fregDst(rd), a, b, ir.F64, funct3 == 1)
	case 0x08: // FCVT.D.S
		a := e.unboxF32(rs1)
		e.irEm.FCvtFF(e.fregDst(rd), a, ir.F32, ir.F64)
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
		e.irEm.FCmp(dst, a, b, ir.LE, ir.F32)
	case 1:
		e.irEm.FCmp(dst, a, b, ir.LT, ir.F32)
	case 2:
		e.irEm.FCmp(dst, a, b, ir.EQ, ir.F32)
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
		e.irEm.FCmp(dst, a, b, ir.LE, ir.F64)
	case 1:
		e.irEm.FCmp(dst, a, b, ir.LT, ir.F64)
	case 2:
		e.irEm.FCmp(dst, a, b, ir.EQ, ir.F64)
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
		e.irEm.FCvtToI(t, a, ir.F32, ir.I32)
		e.irEm.Sext(dst, t, ir.I32)
	case 1: // FCVT.WU.S
		t := e.irEm.Tmp()
		e.irEm.FCvtToU(t, a, ir.F32, ir.I32)
		e.irEm.Sext(dst, t, ir.I32)
	case 2: // FCVT.L.S
		e.irEm.FCvtToI(dst, a, ir.F32, ir.I64)
	case 3: // FCVT.LU.S
		e.irEm.FCvtToU(dst, a, ir.F32, ir.I64)
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
		e.irEm.FCvtToI(t, a, ir.F64, ir.I32)
		e.irEm.Sext(dst, t, ir.I32)
	case 1:
		t := e.irEm.Tmp()
		e.irEm.FCvtToU(t, a, ir.F64, ir.I32)
		e.irEm.Sext(dst, t, ir.I32)
	case 2:
		e.irEm.FCvtToI(dst, a, ir.F64, ir.I64)
	case 3:
		e.irEm.FCvtToU(dst, a, ir.F64, ir.I64)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtFromIntS(rd, rs1, rs2 uint32) {
	s := e.xreg(rs1)
	switch rs2 {
	case 0: // FCVT.S.W
		t := e.irEm.Tmp()
		e.irEm.Sext(t, s, ir.I32)
		result := e.irEm.Tmp()
		e.irEm.FCvtFromI(result, t, ir.I32, ir.F32)
		e.boxF32(rd, result)
	case 1: // FCVT.S.WU
		t := e.irEm.Tmp()
		e.irEm.Zext(t, s, ir.I32)
		result := e.irEm.Tmp()
		e.irEm.FCvtFromU(result, t, ir.I32, ir.F32)
		e.boxF32(rd, result)
	case 2: // FCVT.S.L
		result := e.irEm.Tmp()
		e.irEm.FCvtFromI(result, s, ir.I64, ir.F32)
		e.boxF32(rd, result)
	case 3: // FCVT.S.LU
		result := e.irEm.Tmp()
		e.irEm.FCvtFromU(result, s, ir.I64, ir.F32)
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
		e.irEm.Sext(t, s, ir.I32)
		e.irEm.FCvtFromI(dst, t, ir.I32, ir.F64)
	case 1:
		t := e.irEm.Tmp()
		e.irEm.Zext(t, s, ir.I32)
		e.irEm.FCvtFromU(dst, t, ir.I32, ir.F64)
	case 2:
		e.irEm.FCvtFromI(dst, s, ir.I64, ir.F64)
	case 3:
		e.irEm.FCvtFromU(dst, s, ir.I64, ir.F64)
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
		if target < origPC {
			e.irEm.BudgetCheck(targetLabel, target)
		} else {
			e.irEm.Jump(targetLabel)
		}
		e.gotoTargets.add(target)
		e.pc = target
		return
	}
	e.emitReturn(target, jitOK)
	e.terminated = true
}

func (e *emitter) emitJALR(rd, rs1 uint32, imm int64, insnSize uint64) {
	tgt := e.irEm.Tmp()
	e.irEm.AddImm(tgt, e.xreg(rs1), imm)
	e.irEm.AndImm(tgt, tgt, ^int64(1))
	if rd != 0 {
		e.irEm.Const(e.xregDst(rd), int64(e.pc+insnSize))
	}
	e.advancePC(insnSize)
	e.irEm.WriteBackAll()
	e.irEm.RetDyn(tgt, jitOK, ir.VRegZero)
	e.terminated = true
}

func (e *emitter) emitBranch(rs1, rs2, funct3 uint32, offset int64) {
	target := e.pc + uint64(offset)
	pred, _ := branchPred(funct3)

	e.emitIC()
	e.icEmitted = true

	a := e.xreg(rs1)
	b := e.xreg(rs2)

	internal := e.visited.has(target) ||
		(e.regionEnd > 0 && target >= e.startPC && target < e.regionEnd)

	if internal {
		targetLabel := e.getOrCreateLabel(target)
		if target < e.pc {
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
