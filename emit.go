package riscv

import (
// "sync/atomic"
)

// Emitter wraps a Block and exposes helper methods for building IR.
type Emitter struct {
	j               *JIT
	lastLabelSerial *int64
	Block           *Block
	dirty           []bool // dirty[vr] = true if vr written but not written back
	nextTmp         VReg   // next temporary VReg to allocate

	// Parameter VRegs — set during NewEmitter, represent the JIT block's
	// function arguments (pinned to host regs by the register allocator).
	xBase   VReg // pointer to x[32] array
	fBase   VReg // pointer to f[32] array
	ic      VReg // instruction counter
	memBase VReg // guest memory base
	memMask VReg // guest memory mask
}

const initialDirtySize = 128

// NewEmitter creates an Emitter with a fresh Block and pre-allocated
// parameter VRegs for the JIT block's function arguments.
func NewEmitter(j *JIT) *Emitter {
	e := &Emitter{
		j:       j,
		Block:   NewBlock(),
		dirty:   make([]bool, initialDirtySize),
		nextTmp: VRegTempStart,
	}
	if j == nil {
		var lastSerialTestProxy int64
		e.lastLabelSerial = &lastSerialTestProxy
	} else {
		e.lastLabelSerial = &(j.lastLabelSerial)
	}

	// Pre-allocate parameter VRegs. These correspond to the JIT block's
	// function signature: block_entry(x[], f[], fcsr, mem_base, mem_mask).
	e.xBase = e.Tmp()   // t64 = VRXBase
	e.fBase = e.Tmp()   // t65 = VRFBase
	e.ic = e.Tmp()      // t66 = VRIC
	e.memBase = e.Tmp() // t67 = VRMemBase
	e.memMask = e.Tmp() // t68 = VRMemMask
	e.Tmp()             // t69 = VRRegFile (reserved, pinned to RBP)
	return e
}

// ── VReg management ──

// Tmp allocates a fresh temporary VReg.
func (e *Emitter) Tmp() VReg {
	vr := e.nextTmp
	e.nextTmp++
	e.growDirty(int(e.nextTmp))
	return vr
}

// XReg returns the VReg for guest integer register x[i] (0..31).
func (e *Emitter) XReg(i uint32) VReg {
	if i > 31 {
		panic("ir.Emitter.XReg: index > 31")
	}
	return VReg(i)
}

// FRegV returns the VReg for guest FP register f[i] (0..31).
func (e *Emitter) FRegV(i uint32) VReg {
	if i > 31 {
		panic("ir.Emitter.FRegV: index > 31")
	}
	return VReg(32 + i)
}

// XBase returns the VReg holding the pointer to the x[32] register array.
func (e *Emitter) XBase() VReg { return e.xBase }

// FBase returns the VReg holding the pointer to the f[32] register array.
func (e *Emitter) FBase() VReg { return e.fBase }

// IC returns the VReg holding the instruction counter.
func (e *Emitter) IC() VReg { return e.ic }

// MemBase returns the VReg holding the guest memory base pointer.
func (e *Emitter) MemBase() VReg { return e.memBase }

// MemMask returns the VReg holding the guest memory mask.
func (e *Emitter) MemMask() VReg { return e.memMask }

// ── Integer ALU ──

func (e *Emitter) Add(dst, a, b VReg)            { e.op3(IRAdd, I64, dst, a, b) }
func (e *Emitter) AddT(dst, a, b VReg, t Type)   { e.op3(IRAdd, t, dst, a, b) }
func (e *Emitter) AddImm(dst, a VReg, imm int64) { e.op2i(IRAddImm, I64, dst, a, imm) }
func (e *Emitter) Sub(dst, a, b VReg)            { e.op3(IRSub, I64, dst, a, b) }
func (e *Emitter) SubImm(dst, a VReg, imm int64) { e.op2i(IRSubImm, I64, dst, a, imm) }
func (e *Emitter) Mul(dst, a, b VReg)            { e.op3(IRMul, I64, dst, a, b) }
func (e *Emitter) DivS(dst, a, b VReg)           { e.op3(IRDivS, I64, dst, a, b) }
func (e *Emitter) DivU(dst, a, b VReg)           { e.op3(IRDivU, I64, dst, a, b) }
func (e *Emitter) Rem(dst, a, b VReg)            { e.op3(IRRem, I64, dst, a, b) }
func (e *Emitter) RemU(dst, a, b VReg)           { e.op3(IRRemU, I64, dst, a, b) }
func (e *Emitter) MulHS(dst, a, b VReg)          { e.op3(IRMulHS, I64, dst, a, b) }
func (e *Emitter) MulHU(dst, a, b VReg)          { e.op3(IRMulHU, I64, dst, a, b) }
func (e *Emitter) MulHSU(dst, a, b VReg)         { e.op3(IRMulHSU, I64, dst, a, b) }
func (e *Emitter) Neg(dst, a VReg)               { e.op2(IRNeg, I64, dst, a) }

// ── Shifts ──

func (e *Emitter) Shl(dst, a, b VReg)            { e.op3(IRShl, I64, dst, a, b) }
func (e *Emitter) ShlImm(dst, a VReg, imm int64) { e.op2i(IRShlImm, I64, dst, a, imm) }
func (e *Emitter) Shr(dst, a, b VReg)            { e.op3(IRShr, I64, dst, a, b) }
func (e *Emitter) ShrImm(dst, a VReg, imm int64) { e.op2i(IRShrImm, I64, dst, a, imm) }
func (e *Emitter) Sar(dst, a, b VReg)            { e.op3(IRSar, I64, dst, a, b) }
func (e *Emitter) SarImm(dst, a VReg, imm int64) { e.op2i(IRSarImm, I64, dst, a, imm) }

// ── Bitwise ──

func (e *Emitter) And(dst, a, b VReg)            { e.op3(IRAnd, I64, dst, a, b) }
func (e *Emitter) AndImm(dst, a VReg, imm int64) { e.op2i(IRAndImm, I64, dst, a, imm) }
func (e *Emitter) Or(dst, a, b VReg)             { e.op3(IROr, I64, dst, a, b) }
func (e *Emitter) OrImm(dst, a VReg, imm int64)  { e.op2i(IROrImm, I64, dst, a, imm) }
func (e *Emitter) Xor(dst, a, b VReg)            { e.op3(IRXor, I64, dst, a, b) }
func (e *Emitter) XorImm(dst, a VReg, imm int64) { e.op2i(IRXorImm, I64, dst, a, imm) }
func (e *Emitter) Not(dst, a VReg)               { e.op2(IRNot, I64, dst, a) }
func (e *Emitter) Clz(dst, a VReg, t Type)       { e.op2(IRClz, t, dst, a) }
func (e *Emitter) Ctz(dst, a VReg, t Type)       { e.op2(IRCtz, t, dst, a) }
func (e *Emitter) Popcount(dst, a VReg, t Type)  { e.op2(IRPopcount, t, dst, a) }
func (e *Emitter) Bswap(dst, a VReg)             { e.op2(IRBswap, I64, dst, a) }

// ── Comparison ──

func (e *Emitter) Set(dst, a, b VReg, p Pred)            { e.opSet(IRSet, dst, a, b, p) }
func (e *Emitter) SetImm(dst, a VReg, imm int64, p Pred) { e.opSetImm(IRSetImm, dst, a, imm, p) }

// ── Data movement ──

func (e *Emitter) Mov(dst, a VReg) { e.op2(IRMov, I64, dst, a) }

// MovT is Mov with an explicit type. Use F32/F64 when the move crosses
// integer↔FP register classes so the regalloc places dst in the matching
// register file (XMM for F32/F64). The lowerer emits MOVQ for cross-class
// transfers regardless of T.
func (e *Emitter) MovT(dst, a VReg, t Type)     { e.op2(IRMov, t, dst, a) }
func (e *Emitter) Const(dst VReg, imm int64)    { e.opConst(dst, imm) }
func (e *Emitter) Sext(dst, a VReg, fromT Type) { e.opExt(IRSext, dst, a, fromT) }
func (e *Emitter) Zext(dst, a VReg, fromT Type) { e.opExt(IRZext, dst, a, fromT) }

// ── Memory ──

// Load emits a load: dst = *(T*)(base + imm).
// For signed sub-I64 types, a sign-extend is appended.
// For unsigned sub-I64 types, a zero-extend is appended.
func (e *Emitter) Load(dst, base VReg, imm int64, t Type, signed bool) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRLoad, T: t, Dst: dst, A: base, Imm: imm})
	// Sub-I64 integer loads need sign/zero extension to 64 bits.
	if t <= I32 {
		if signed {
			e.emit(IRInstr{Op: IRSext, T: t, Dst: dst, A: dst})
		} else {
			e.emit(IRInstr{Op: IRZext, T: t, Dst: dst, A: dst})
		}
	}
	e.MarkDirty(dst)
}

// Store emits a store: *(T*)(base + imm) = src.
func (e *Emitter) Store(base VReg, imm int64, src VReg, t Type) {
	e.emit(IRInstr{Op: IRStore, T: t, A: base, B: src, Imm: imm})
}

// LoadX emits an indexed load: dst = *(T*)(base + idx*scale).
func (e *Emitter) LoadX(dst, base, idx VReg, scale uint8, t Type, signed bool) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRLoadX, T: t, Dst: dst, A: base, B: idx, Scale: scale})
	if t <= I32 {
		if signed {
			e.emit(IRInstr{Op: IRSext, T: t, Dst: dst, A: dst})
		} else {
			e.emit(IRInstr{Op: IRZext, T: t, Dst: dst, A: dst})
		}
	}
	e.MarkDirty(dst)
}

// StoreX emits an indexed store: *(T*)(base + idx*scale) = src.
func (e *Emitter) StoreX(base, idx VReg, scale uint8, src VReg, t Type) {
	e.emit(IRInstr{Op: IRStoreX, T: t, A: base, B: idx, Dst: src, Scale: scale})
}

// ── Control flow ──

// NewLabel allocates a label ID without placing it. Use PlaceLabel to emit it.
func (e *Emitter) NewLabel() Label {
	//l := e.Block.NextLabel
	//e.Block.NextLabel++
	//return l
	(*e.lastLabelSerial)++
	sn := *(e.lastLabelSerial)
	return Label(sn)
}

// PlaceLabel emits an IRLabel for a previously allocated label.
func (e *Emitter) PlaceLabel(l Label) {
	e.emit(IRInstr{Op: IRLabel, Imm: int64(l)})
}

// Branch emits a conditional branch: if (a pred b) goto target.
func (e *Emitter) Branch(a, b VReg, p Pred, target Label) {
	e.emit(IRInstr{Op: IRBranch, A: a, B: b, Pred: p, Imm: int64(target)})
}

// BranchImm emits a compare-immediate branch: if (a pred imm) goto target.
func (e *Emitter) BranchImm(a VReg, imm int64, p Pred, target Label) {
	e.emit(IRInstr{Op: IRBranchImm, A: a, Pred: p, Imm: int64(target), Imm2: imm})
}

// Jump emits an unconditional jump to target.
func (e *Emitter) Jump(target Label) {
	e.emit(IRInstr{Op: IRJump, Imm: int64(target)})
}

// Call registers an external C ABI symbol and emits an IRCall.
// Returns the CTab index for the symbol.
func (e *Emitter) Call(sym string, addr uintptr) int {
	// Check if already registered.
	for i, cs := range e.Block.CTab {
		if cs.Name == sym {
			if cs.Addr != addr {
				panic("ir.Emitter.Call: symbol " + sym + " registered with different address")
			}
			e.emit(IRInstr{Op: IRCall, Imm: int64(i)})
			return i
		}
	}
	idx := len(e.Block.CTab)
	e.Block.CTab = append(e.Block.CTab, CSym{Name: sym, Addr: addr})
	e.emit(IRInstr{Op: IRCall, Imm: int64(idx)})
	return idx
}

// MisalignedLoad emits a byte-by-byte load for a misaligned guest address.
// The lowerer expands inline using [RBP+520]/[RBP+528] for memBase/memMask.
func (e *Emitter) MisalignedLoad(dst, addr VReg, t Type) {
	e.emit(IRInstr{Op: IRMisalignLoad, T: t, Dst: dst, A: addr})
	e.MarkDirty(dst)
}

// MisalignedStore emits a byte-by-byte store for a misaligned guest address.
func (e *Emitter) MisalignedStore(addr, value VReg, t Type) {
	e.emit(IRInstr{Op: IRMisalignStore, T: t, A: addr, B: value})
}

// Ret emits a block return: {pc=pc, status=status, faultAddr=faultAddr}.
func (e *Emitter) Ret(pc uint64, status int, faultAddr VReg) {
	e.emit(IRInstr{Op: IRRet, Imm: int64(pc), Imm2: int64(status), A: faultAddr})
}

// RetDyn emits a block return with a runtime-computed PC from a VReg.
// Used by JALR where the target address is computed at runtime.
func (e *Emitter) RetDyn(pcVReg VReg, status int, faultAddr VReg) {
	e.emit(IRInstr{Op: IRRetDyn, A: pcVReg, Imm: int64(status), B: faultAddr})
}

// ChainExit emits a chain exit. The lowerer will emit a MOVABS+JMP sequence
// initially targeting a slow exit stub, which Go can later patch to jump
// directly to the target block's chain entry.
func (e *Emitter) ChainExit(targetPC uint64, exitIdx int) {
	e.emit(IRInstr{Op: IRChainExit, Imm: int64(targetPC), Imm2: int64(exitIdx)})
}

// Syscall emits an ECALL fast-path dispatch to the SysV-ABI
// dispatcher at dispatcherAddr (obtained from
// riscv/internal/syscalls.DispatchAddr()). resumePC is where execution
// continues (pc+4). The caller must emit WriteBackAll before Syscall
// so the dispatcher sees fresh guest registers in x[].
//
// The emitter always terminates the IR block at Syscall — post-ECALL
// code lives in a separate AOT block entered via chain exit from
// lowerSyscall. dirty[] is therefore never mutated here.
func (e *Emitter) Syscall(resumePC uint64, dispatcherAddr uintptr) {
	const sym = "syscall_dispatcher"
	idx := -1
	for i, cs := range e.Block.CTab {
		if cs.Name == sym {
			if cs.Addr != dispatcherAddr {
				panic("ir.Emitter.Syscall: dispatcher address changed")
			}
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = len(e.Block.CTab)
		e.Block.CTab = append(e.Block.CTab, CSym{Name: sym, Addr: dispatcherAddr})
	}
	e.emit(IRInstr{Op: IRSyscall, Imm: int64(resumePC), Imm2: int64(idx)})
}

// JalrIC emits a JALR-site inline cache. The lowerer emits a
// compare-and-direct-jump sequence with two MOVABS-imm64 patch slots
// (cache_pc, cache_fn). On first execution the compare misses, control
// flows to a per-site miss stub that returns to Go with the site idx in
// sret.FaultAddr. Go patches both slots; subsequent dispatches with the
// same target PC jump directly to the target block's chainEntry.
//
// Caller must emit WriteBackAll before JalrIC — block-to-block state
// transfer goes through x[]/f[] arrays, not pinned host regs.
func (e *Emitter) JalrIC(target VReg, siteIdx int) {
	e.emit(IRInstr{Op: IRJalrIC, A: target, Imm: int64(siteIdx)})
}

// ── Floating point ──

func (e *Emitter) FAdd(dst, a, b VReg, t Type) { e.op3(IRFAdd, t, dst, a, b) }

// FMA emits Dst = A*B + C with a single IEEE 754 rounding (fused).
// Used for RISC-V FMADD.{S,D} — spec §11.6. Lowered to VFMADD213SS/SD.
func (e *Emitter) FMA(dst, a, b, c VReg, t Type) {
	e.emit(IRInstr{Op: IRFma, T: t, Dst: dst, A: a, B: b, C: c})
	e.MarkDirty(dst)
}

// Fmsub: Dst = A*B - C. RISC-V FMSUB.{S,D}. Lowered to VFMSUB213SS/SD.
func (e *Emitter) Fmsub(dst, a, b, c VReg, t Type) {
	e.emit(IRInstr{Op: IRFmsub, T: t, Dst: dst, A: a, B: b, C: c})
	e.MarkDirty(dst)
}

// Fnmadd: Dst = -(A*B + C) — RISC-V FNMADD. Lowered to VFNMSUB213SS/SD
// because x86's VFNMSUB computes -(a*b - c) = -a*b + c — wait, let me
// state the actual x86 semantics carefully:
//
//	VFMADD213SS  dst = dst*b + c      (uses dst as a)
//	VFMSUB213SS  dst = dst*b - c
//	VFNMADD213SS dst = -(dst*b) + c
//	VFNMSUB213SS dst = -(dst*b) - c
//
// Mapping to RISC-V:
//
//	RISC-V FMADD   = a*b + c        → VFMADD213SS
//	RISC-V FMSUB   = a*b - c        → VFMSUB213SS
//	RISC-V FNMADD  = -(a*b) - c     → VFNMSUB213SS
//	RISC-V FNMSUB  = -(a*b) + c     → VFNMADD213SS
//
// We'll use the semantic names (IRFnmadd, IRFnmsub) and the lowerer
// does the RISC-V→x86 opcode mapping.
func (e *Emitter) Fnmadd(dst, a, b, c VReg, t Type) {
	e.emit(IRInstr{Op: IRFnmadd, T: t, Dst: dst, A: a, B: b, C: c})
	e.MarkDirty(dst)
}

// Fnmsub: Dst = -(A*B - C) = -A*B + C. RISC-V FNMSUB.
func (e *Emitter) Fnmsub(dst, a, b, c VReg, t Type) {
	e.emit(IRInstr{Op: IRFnmsub, T: t, Dst: dst, A: a, B: b, C: c})
	e.MarkDirty(dst)
}
func (e *Emitter) FSub(dst, a, b VReg, t Type) { e.op3(IRFSub, t, dst, a, b) }
func (e *Emitter) FMul(dst, a, b VReg, t Type) { e.op3(IRFMul, t, dst, a, b) }
func (e *Emitter) FDiv(dst, a, b VReg, t Type) { e.op3(IRFDiv, t, dst, a, b) }
func (e *Emitter) FSqrt(dst, a VReg, t Type)   { e.op2(IRFSqrt, t, dst, a) }
func (e *Emitter) FNeg(dst, a VReg, t Type)    { e.op2(IRFNeg, t, dst, a) }
func (e *Emitter) FAbs(dst, a VReg, t Type)    { e.op2(IRFAbs, t, dst, a) }

// FCmp emits a floating-point comparison: Dst = (A pred B) ? 1 : 0.
func (e *Emitter) FCmp(dst, a, b VReg, p Pred, t Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRFCmp, T: t, Dst: dst, A: a, B: b, Pred: p})
	e.MarkDirty(dst)
}

// FCvtToI converts FP to signed integer: dst(toT) = convert(a(fromT)).
func (e *Emitter) FCvtToI(dst, a VReg, fromT, toT Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRFCvtToI, T: toT, U: fromT, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// FCvtToU converts FP to unsigned integer.
func (e *Emitter) FCvtToU(dst, a VReg, fromT, toT Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRFCvtToU, T: toT, U: fromT, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// FCvtFromI converts signed integer to FP.
func (e *Emitter) FCvtFromI(dst, a VReg, fromT, toT Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRFCvtFromI, T: toT, U: fromT, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// FCvtFromU converts unsigned integer to FP.
func (e *Emitter) FCvtFromU(dst, a VReg, fromT, toT Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRFCvtFromU, T: toT, U: fromT, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// FCvtFF converts between FP types (F32 <-> F64).
func (e *Emitter) FCvtFF(dst, a VReg, fromT, toT Type) {
	if dst == VRegZero {
		return
	}
	e.emit(IRInstr{Op: IRFCvtFF, T: toT, U: fromT, Dst: dst, A: a})
	e.MarkDirty(dst)
}

// ── Internal helpers ──

// growDirty ensures the dirty slice can hold index need-1.
func (e *Emitter) growDirty(need int) {
	if need <= len(e.dirty) {
		return
	}
	old := e.dirty
	e.dirty = make([]bool, need*2)
	copy(e.dirty, old)
}
