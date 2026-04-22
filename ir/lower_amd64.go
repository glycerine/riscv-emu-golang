package ir

import (
	"fmt"
	"sort"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/x86"
)

// ── AMD64 register convention ──
//
// Pinned (never allocated, callee-saved):
//   R12 = x[] pointer    (param VReg t64)
//   R13 = f[] pointer    (param VReg t65)
//   R14 = mem_base       (param VReg t67)
//   R15 = mem_mask       (param VReg t68)
//   RBX = sret buffer
//   RSP = stack pointer
//
// Scratch (never allocated, used by lowerer):
//   R10, R11
//
// Integer allocation pool (8 regs, or 6 if DIV/MUL present):
//   RAX, RCX, RDX, RBP, RSI, RDI, R8, R9
//
// FP allocation pool:
//   XMM0-XMM15

// Pinned host registers.
const (
	amd64RegXBase   int16 = goasm.REG_AMD64_R12
	amd64RegFBase   int16 = goasm.REG_AMD64_R13
	amd64RegMemBase int16 = goasm.REG_AMD64_R14
	amd64RegMemMask int16 = goasm.REG_AMD64_R15
	amd64RegIC      int16 = goasm.REG_AMD64_BP
	amd64RegSret    int16 = goasm.REG_AMD64_BX
	amd64Scratch1   int16 = goasm.REG_AMD64_R10
	amd64Scratch2   int16 = goasm.REG_AMD64_R11
)

// amd64SretFcsrOffset is the byte offset, relative to RBX (the sret
// pointer passed by jitcall.Call), where the host-side fcsr pointer is
// stored. The jitcall trampoline writes it once per Call, so callee
// code can read *[RBX+80] safely on first entry AND on chained entries
// (where RCX has been released to the regalloc pool and may hold
// anything). Must agree with internal/jitcall/call_amd64.s.
const amd64SretFcsrOffset = 80

// Parameter VRegs by convention (NewEmitter allocates these first).
const (
	VRXBase   = VReg(VRegTempStart + 0) // t64
	VRFBase   = VReg(VRegTempStart + 1) // t65
	VRIC      = VReg(VRegTempStart + 2) // t66 — pinned to RBP
	VRMemBase = VReg(VRegTempStart + 3) // t67
	VRMemMask = VReg(VRegTempStart + 4) // t68
)

// AMD64Pool returns the register pool for amd64 lowering.
// When the block contains DIV/MUL ops, RAX and RDX are excluded
// because IDIVQ/MULQ use them as implicit operands.
func AMD64Pool(b *Block) RegPool {
	intRegs := []int16{
		goasm.REG_AMD64_AX,
		goasm.REG_AMD64_CX,
		goasm.REG_AMD64_DX,
		goasm.REG_AMD64_SI,
		goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8,
		goasm.REG_AMD64_R9,
	}
	if BlockHasDivMul(b) {
		intRegs = []int16{
			goasm.REG_AMD64_CX,
			goasm.REG_AMD64_SI,
			goasm.REG_AMD64_DI,
			goasm.REG_AMD64_R8,
			goasm.REG_AMD64_R9,
		}
	}
	fpRegs := []int16{
		goasm.REG_AMD64_X0, goasm.REG_AMD64_X1, goasm.REG_AMD64_X2, goasm.REG_AMD64_X3,
		goasm.REG_AMD64_X4, goasm.REG_AMD64_X5, goasm.REG_AMD64_X6, goasm.REG_AMD64_X7,
		goasm.REG_AMD64_X8, goasm.REG_AMD64_X9, goasm.REG_AMD64_X10, goasm.REG_AMD64_X11,
		goasm.REG_AMD64_X12, goasm.REG_AMD64_X13, goasm.REG_AMD64_X14, goasm.REG_AMD64_X15,
	}
	return RegPool{IntRegs: intRegs, FPRegs: fpRegs}
}

// AMD64Pinned returns the pinned VReg → host register map for parameter VRegs.
// These are passed to Allocate() so the allocator fixes them in place.
func AMD64Pinned() map[VReg]int16 {
	return map[VReg]int16{
		VRXBase:   amd64RegXBase,
		VRFBase:   amd64RegFBase,
		VRIC:      amd64RegIC,
		VRMemBase: amd64RegMemBase,
		VRMemMask: amd64RegMemMask,
	}
}

// ── Per-VReg interval lookup (replaces linear IntervalMap scan) ──

// regEntry is one interval's host register assignment, sorted by Start.
type regEntry struct {
	start, end int
	host       int16
}

// regIndex maps VReg → sorted list of regEntry for O(log N) host-register lookup.
type regIndex [][]regEntry

type regEntriesByStart []regEntry

func (s regEntriesByStart) Len() int           { return len(s) }
func (s regEntriesByStart) Less(i, j int) bool { return s[i].start < s[j].start }
func (s regEntriesByStart) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func buildRegIndex(alloc *Allocation) regIndex {
	maxVR := len(alloc.Kind)
	idx := make(regIndex, maxVR)

	// Count entries per VReg to pre-allocate a single flat backing array.
	counts := make([]int, maxVR)
	for i := range alloc.IntervalMap {
		vr := int(alloc.IntervalMap[i].Interval.VReg)
		if vr < maxVR {
			counts[vr]++
		}
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	flat := make([]regEntry, total)

	// Assign sub-slices from the flat array.
	off := 0
	for vr, c := range counts {
		if c > 0 {
			idx[vr] = flat[off : off : off+c] // len=0, cap=c
			off += c
		}
	}

	// Fill entries.
	for i := range alloc.IntervalMap {
		ia := &alloc.IntervalMap[i]
		vr := int(ia.Interval.VReg)
		if vr < maxVR {
			idx[vr] = append(idx[vr], regEntry{
				start: ia.Interval.Start,
				end:   ia.Interval.End,
				host:  ia.Host,
			})
		}
	}

	// Sort each VReg's entries by start for binary search.
	for vr := range idx {
		entries := idx[vr]
		if len(entries) > 1 {
			sort.Sort(regEntriesByStart(entries))
		}
	}
	return idx
}

// lookup returns the host register for VReg v at instruction index idx, or -1.
func (ri regIndex) lookup(v VReg, idx int) int16 {
	vr := int(v)
	if vr >= len(ri) {
		return -1
	}
	entries := ri[vr]
	// Binary search for the interval containing idx.
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if entries[mid].end < idx {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(entries) && entries[lo].start <= idx && idx <= entries[lo].end {
		return entries[lo].host
	}
	return -1
}

// ── lowerCtx holds mutable state during lowering ──

// chainExitInfo records a chain exit for post-assembly backpatching.
type chainExitInfo struct {
	targetPC uint64    // guest PC this exit targets
	movProg  *obj.Prog // MOVABS Prog — read Pc after assembly for patch offset
	stubProg *obj.Prog // first Prog of the slow exit stub
}

// ChainExitDesc describes a chain exit for the caller.
// After assembly, use MovProg.Pc and StubProg.Pc to compute byte offsets.
type ChainExitDesc struct {
	TargetPC uint64    // guest PC this exit targets
	MovProg  *obj.Prog // the MOVABS Prog — Pc gives instruction offset after assembly
	StubProg *obj.Prog // first Prog of the slow exit stub
}

// jalrICInfo records a 2-way JALR inline-cache site for post-assembly
// backpatching. Slot indexing: [0] = recent target (checked first, JEQ
// taken on hit0); [1] = older target (checked second, fall-through on
// hit1). See plan "Priority 1.5 — 2-way JALR inline cache".
type jalrICInfo struct {
	siteIdx  int
	pcMov    [2]*obj.Prog // MOVABS for cache_pc[0], cache_pc[1]
	fnMov    [2]*obj.Prog // MOVABS for cache_fn[0], cache_fn[1]
	jeq0Prog *obj.Prog    // JEQ → .hit0; target set inline during lowering
	jne1Prog *obj.Prog    // JNE → .miss;  target set during finalize
	hit0Prog *obj.Prog    // NOP marking .hit0 label
	stubProg *obj.Prog    // first Prog of the miss stub (set during finalize)
}

// JalrICDesc describes a JALR inline-cache site for the caller.
// After assembly, use PcMov[k].Pc + 2 and FnMov[k].Pc + 2 to compute
// patch offsets into the imm64 fields; StubProg.Pc gives the miss
// stub address.
type JalrICDesc struct {
	SiteIdx  int
	PcMov    [2]*obj.Prog // MOVABS for cache_pc[0], cache_pc[1]
	FnMov    [2]*obj.Prog // MOVABS for cache_fn[0], cache_fn[1]
	StubProg *obj.Prog    // first Prog of the miss stub
}

// LowerResult holds chain-related metadata produced during lowering.
// After assembly, Prog.Pc fields contain byte offsets into the assembled code.
type LowerResult struct {
	ChainEntryProg *obj.Prog       // NOP at chain entry point
	ChainExits     []ChainExitDesc // chain exit descriptors
	JalrICs        []JalrICDesc    // JALR inline-cache descriptors
}

type lowerCtx struct {
	blk   *Block
	alloc *Allocation
	c     *goasm.Ctx
	idx   int // current IR instruction index

	// Fast per-VReg host register lookup (replaces linear IntervalMap scan).
	rIdx   regIndex
	fpSet  map[VReg]bool // precomputed: is this VReg assigned to an XMM register?
	cxLive []regEntry    // intervals where CX is live (sorted by start)

	// Label resolution.
	labelProg map[Label]*obj.Prog   // label → NOP prog at that point
	pending   map[Label][]*obj.Prog // forward-ref branches waiting for label

	// Frame layout.
	stackSlots int   // from Allocation.StackSlots
	frameSize  int64 // total bytes: stackSlots*8 (fcsr pointer lives at [RBX+80], not on the frame)

	// Scratch cache: elides redundant spill loads when consecutive instructions
	// use the same spilled VReg. Index 0 tracks R10, index 1 tracks R11.
	scratchCache [2]scratchEntry

	// Block chaining.
	chainEntryProg *obj.Prog       // NOP marking chain entry point
	chainExits     []chainExitInfo // chain exit metadata for backpatching
	jalrICs        []jalrICInfo    // JALR IC site metadata for backpatching

	// AOT decoder_cache dispatch. When true, JALR lowering emits a
	// mask-bounded lookup into the trampoline-published decoder_cache
	// (sret[88..112]) before falling through to the 2-way IC. The
	// caller (JIT) sets this when an AOT segment is installed and
	// clears it for lazy-compile paths that use plain jitcall.Call
	// (which doesn't populate sret[88..112]).
	emitDecoderCacheAttempt bool
}

// ── Exported API ──

// LowerAMD64 converts a register-allocated IR Block into x86-64 obj.Progs
// appended to ctx. After calling this, ctx.Assemble() produces native bytes.
// Returns a LowerResult with chain entry/exit metadata for block chaining.
//
// The caller must have already appended an ATEXT prog to ctx.
//
// JALR emission uses the 2-way IC only (no AOT decoder_cache
// lookup). Callers wanting the AOT fast path use LowerAMD64AOT.
func LowerAMD64(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	return lowerAMD64Impl(ctx, b, alloc, false)
}

// LowerAMD64AOT is LowerAMD64 with the AOT-aware JALR lowering: each
// JALR site emits a decoder_cache attempt against the segment state
// published by jitcall.CallAOT into sret[88..112] before falling
// through to the existing 2-way IC sequence. Intended for blocks
// that will be invoked via CallAOT (i.e., compiled as part of an
// AOT segment).
func LowerAMD64AOT(ctx *goasm.Ctx, b *Block, alloc *Allocation) (*LowerResult, error) {
	return lowerAMD64Impl(ctx, b, alloc, true)
}

func lowerAMD64Impl(ctx *goasm.Ctx, b *Block, alloc *Allocation, aot bool) (*LowerResult, error) {
	if alloc == nil {
		return nil, fmt.Errorf("ir.LowerAMD64: nil allocation")
	}

	// Build fast lookup index for host register assignments.
	rIdx := buildRegIndex(alloc)

	// Precompute FP VReg set and CX live intervals.
	fpSet := make(map[VReg]bool)
	var cxLive []regEntry
	for i := range alloc.IntervalMap {
		ia := &alloc.IntervalMap[i]
		if isXMMReg(ia.Host) {
			fpSet[ia.Interval.VReg] = true
		}
		if ia.Host == goasm.REG_AMD64_CX {
			cxLive = append(cxLive, regEntry{
				start: ia.Interval.Start,
				end:   ia.Interval.End,
				host:  ia.Host,
			})
		}
	}
	// Guest FP regs 32-63 are always FP.
	for vr := VReg(32); vr < 64; vr++ {
		fpSet[vr] = true
	}
	sort.Sort(regEntriesByStart(cxLive))

	lc := &lowerCtx{
		blk:                     b,
		alloc:                   alloc,
		c:                       ctx,
		rIdx:                    rIdx,
		fpSet:                   fpSet,
		cxLive:                  cxLive,
		labelProg:               make(map[Label]*obj.Prog),
		pending:                 make(map[Label][]*obj.Prog),
		stackSlots:              alloc.StackSlots,
		emitDecoderCacheAttempt: aot,
	}

	// Compute frame size: spill slots only. The fcsr pointer lives in
	// the caller's sret buffer at [RBX+80] (see amd64SretFcsrOffset),
	// so it survives across chained block entries.
	lc.frameSize = int64(lc.stackSlots) * 8

	lc.emitPrologue()

	for idx := range b.Instrs {
		lc.idx = idx
		if err := lc.lowerInstr(&b.Instrs[idx]); err != nil {
			return nil, err
		}
	}

	if len(lc.pending) > 0 {
		return nil, fmt.Errorf("ir.LowerAMD64: %d unresolved forward labels", len(lc.pending))
	}

	// Post-lowering peephole: remove redundant spill/reload and self-
	// move MOVQs left by the FixedStaticAllocator. Runs before slow-
	// exit stub emission so the stubs (small, no spill patterns) don't
	// need to be protected individually.
	lc.peepholeHostSpills()

	// Emit slow exit stubs for chain exits and build result.
	result := &LowerResult{
		ChainEntryProg: lc.chainEntryProg,
	}
	for i := range lc.chainExits {
		lc.chainExits[i].stubProg = lc.emitSlowExitStub(lc.chainExits[i].targetPC)
		result.ChainExits = append(result.ChainExits, ChainExitDesc{
			TargetPC: lc.chainExits[i].targetPC,
			MovProg:  lc.chainExits[i].movProg,
			StubProg: lc.chainExits[i].stubProg,
		})
	}

	// Emit miss stubs for JALR IC sites and wire JNE targets.
	for i := range lc.jalrICs {
		lc.jalrICs[i].stubProg = lc.emitJalrMissStub(lc.jalrICs[i].siteIdx)
		lc.jalrICs[i].jne1Prog.To.SetTarget(lc.jalrICs[i].stubProg)
		result.JalrICs = append(result.JalrICs, JalrICDesc{
			SiteIdx:  lc.jalrICs[i].siteIdx,
			PcMov:    lc.jalrICs[i].pcMov,
			FnMov:    lc.jalrICs[i].fnMov,
			StubProg: lc.jalrICs[i].stubProg,
		})
	}

	return result, nil
}

// ── Prologue / Epilogue ──

func (lc *lowerCtx) emitPrologue() {
	// Move SysV ABI args to pinned callee-saved registers.
	// Entry ABI: RDI=sret, RSI=x[], RDX=f[], RCX=fcsr, R8=memBase, R9=memMask.
	// RCX is NOT stashed here — the jitcall trampoline already published
	// the fcsr pointer at [RBX+amd64SretFcsrOffset], which remains valid
	// on chained entries. Code that needs fcsr reads it from there.
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_SI, amd64RegXBase)   // RSI → R12
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DX, amd64RegFBase)   // RDX → R13
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_R8, amd64RegMemBase) // R8  → R14
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_R9, amd64RegMemMask) // R9  → R15
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DI, amd64RegSret)    // RDI → RBX

	// Initialize IC to 0 — first-entry only (pinned to RBP).
	lc.emitRR(x86.AXORQ, amd64RegIC, amd64RegIC)

	// Chain entry marker: NOP placed after arg setup and IC zero but
	// BEFORE frame allocation. Inbound chain JMPs land here so they
	// skip XORQ RBP, RBP (IC accumulates across chained blocks) yet
	// still execute the frame SubQ below — every block allocates its
	// own spill frame, matched by each chain exit's ADDQ. jit_native
	// records this Prog's Pc as blk.chainEntry.
	lc.chainEntryProg = lc.emitNOP()

	// Allocate spill frame if needed. Runs on every entry (first and
	// chained), matched by each chain exit's ADDQ.
	if lc.frameSize > 0 {
		lc.emitRI(x86.ASUBQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
}

func (lc *lowerCtx) emitEpilogue() {
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)
}

// ── Block chaining ──

func (lc *lowerCtx) emitNOP() *obj.Prog {
	p := lc.c.NewProg()
	p.As = obj.ANOP
	lc.c.Append(p)
	return p
}

func (lc *lowerCtx) emitJmpReg(reg int16) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_REG
	p.To.Reg = reg
	lc.c.Append(p)
}

// lowerChainExit emits a chain exit sequence: dealloc spill frame,
// MOVABS R10 with a sentinel (to be backpatched), JMP R10.
func (lc *lowerCtx) lowerChainExit(ins *IRInstr) {
	// Deallocate spill frame.
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	// MOVABS R10, <sentinel> — 10-byte encoding for patching.
	// Sentinel > 32 bits forces the assembler to use the 10-byte MOVABS encoding.
	const sentinel = int64(0x7BADC0DE7BADC0DE)
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = sentinel
	p.To.Type = obj.TYPE_REG
	p.To.Reg = amd64Scratch1
	lc.c.Append(p)

	lc.chainExits = append(lc.chainExits, chainExitInfo{
		targetPC: uint64(ins.Imm),
		movProg:  p,
	})
	// JMP R10
	lc.emitJmpReg(amd64Scratch1)
}

// lowerJalrIC emits a 2-way JALR-site inline cache. Layout:
//
//	MOVQ   tgt, 0(RBX)                 ; sret.PC (harmless on hit)
//	MOVABS R10, <pc0_sentinel>         ; imm64 = cache_pc[0]
//	CMPQ   tgt, R10
//	JEQ    .hit0
//	MOVABS R10, <pc1_sentinel>         ; imm64 = cache_pc[1]
//	CMPQ   tgt, R10
//	JNE    .miss                       ; set to missStub in finalize
//	ADDQ   $frameSize, RSP             ; hit1 path (fall-through)
//	MOVABS R10, <fn1_sentinel>         ; imm64 = cache_fn[1]
//	JMP    R10                          ; → target chainEntry
//	.hit0:
//	ADDQ   $frameSize, RSP             ; hit0 path (reached via JEQ)
//	MOVABS R10, <fn0_sentinel>         ; imm64 = cache_fn[0]
//	JMP    R10                          ; → target chainEntry
//
// backpatchJalrICs initializes all four imm64 slots: pc slots to an
// unmatchable sentinel (0xFFFFFFFFFFFFFFFF) so the first CMPQ misses;
// fn slots to the miss stub so any stray JMP would land somewhere
// valid (never actually reached while pc is unmatchable).
//
// On each miss, tryPatchJalrIC uses shift semantics: slot 1 ← slot 0
// (demote), slot 0 ← new target (promote). For a bi-modal site with
// targets {A,B,A,B,…}, converges to {A,B} → 100% hit rate after 2
// misses. For rotating {A,B,C,…}, still thrashes but no worse than
// 1-way.
func (lc *lowerCtx) lowerJalrIC(ins *IRInstr) {
	// Load target PC. Use slot 1 (R11) so spilled VRegs don't clobber R10,
	// which we need for MOVABS.
	tgt := lc.use(ins.A, 1)

	// ── Decoder_cache attempt (AOT fast path) ──
	// If the trampoline published AOT state at [RBX+88..112], try a
	// mask-bounded lookup. On hit, JMP directly to the target block's
	// chainEntry. On miss (bounds fail OR slot holds zero), fall through
	// into the 2-way IC sequence below.
	//
	// Scratch usage at this point: R10 and R11 are both free. tgt is in
	// an allocated reg (guaranteed not R10/R11 by the register pool).
	// This sequence overwrites R10 and R11; the IC stash below
	// re-initializes R11 to tgt, and MOVABS re-initializes R10.
	//
	// When AOT is not installed, sret[88..112] is whatever the trampoline
	// left (jitcall.Call zeroes nothing there). For safety, callers that
	// don't install AOT must use jitcall.Call (which doesn't write
	// 88..112) — the sequence then reads stale/uninitialized bytes.
	// This is *only* a problem if those bytes happen to form a valid
	// (segSize, mask, base) tuple that passes the bounds check and
	// matches a guest PC. In practice: with size=0 default on
	// CallAOT-passing-zeros, the bounds check always fails → fallback.
	// With plain Call → undefined behavior in the decoder_cache sequence.
	// We therefore condition emission on a flag set by the JIT.
	if lc.emitDecoderCacheAttempt {
		emitDecoderCacheLookup(lc, tgt)
	}

	// Stash tgt into R11 (amd64Scratch2). The hit path no longer writes
	// sret.PC — target block doesn't consume it. The miss stub reads R11
	// to populate sret.PC on its way out. If tgt is already in R11 (e.g.
	// because it was spilled and lc.use loaded it there), the MOVQ is a
	// self-copy and the assembler emits a 3-byte NOP-equivalent.
	lc.emitRR(x86.AMOVQ, tgt, amd64Scratch2)

	const sentinel = int64(0x7BADC0DE7BADC0DE)

	// Stage 0: MOVABS R10, <pc0>; CMPQ tgt, R10; JEQ .hit0.
	pcMov0 := lc.emitMovabsSentinel(sentinel)
	lc.emitRR(x86.ACMPQ, tgt, amd64Scratch1)
	jeq0 := lc.c.NewProg()
	jeq0.As = x86.AJEQ
	jeq0.To.Type = obj.TYPE_BRANCH
	lc.c.Append(jeq0)

	// Stage 1: MOVABS R10, <pc1>; CMPQ tgt, R10; JNE .miss.
	pcMov1 := lc.emitMovabsSentinel(sentinel)
	lc.emitRR(x86.ACMPQ, tgt, amd64Scratch1)
	jne1 := lc.c.NewProg()
	jne1.As = x86.AJNE
	jne1.To.Type = obj.TYPE_BRANCH
	lc.c.Append(jne1)

	// Hit-1 path (fall-through from stage 1): dealloc frame, load fn1, JMP R10.
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	fnMov1 := lc.emitMovabsSentinel(sentinel)
	lc.emitJmpReg(amd64Scratch1)

	// .hit0: NOP label. Control reaches here only via the JEQ taken branch.
	hit0 := lc.emitNOP()
	jeq0.To.SetTarget(hit0)

	// Hit-0 path: dealloc frame, load fn0, JMP R10.
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	fnMov0 := lc.emitMovabsSentinel(sentinel)
	lc.emitJmpReg(amd64Scratch1)

	lc.jalrICs = append(lc.jalrICs, jalrICInfo{
		siteIdx:  int(ins.Imm),
		pcMov:    [2]*obj.Prog{pcMov0, pcMov1},
		fnMov:    [2]*obj.Prog{fnMov0, fnMov1},
		jeq0Prog: jeq0,
		jne1Prog: jne1,
		hit0Prog: hit0,
	})
}

// emitDecoderCacheLookup emits the AOT decoder_cache dispatch
// sequence at the top of a JALR site. On hit, JMPs directly to the
// target block's chainEntry (the guest-PC → native-entry mapping
// built at AOT load). On miss (bounds fail or slot is zero), falls
// through to the existing 2-way IC sequence emitted by the caller.
//
// Reads segment state from the sret-buffer extension published by
// jitcall.CallAOT (trampoline at internal/jitcall/call_amd64.s):
//   [RBX+88]   decoder_cache base (host pointer)
//   [RBX+96]   decoder_cache mask (power-of-two size - 1)
//   [RBX+104]  vaddrBegin (current segment's guest-VA start)
//   [RBX+112]  segSize    (current segment's guest-VA size)
//
// Scratch usage: R10 only. R11 is intentionally untouched because
// the caller's `tgt` may live in R11 when the VReg was loaded from a
// spill slot via lc.use(_, 1). If we clobbered R11 and `tgt` aliased
// it, the IC fallback below would then re-stash a garbage value.
//
// Sandboxing: the decoder_cache load is `base + (idx & mask)` —
// identical in shape to a guest `lw` — so a wild `tgt` from a
// broken guest cannot read past the decoder_cache mmap region.
// A range check on `tgt` vs segSize is emitted first for
// correctness (so out-of-segment targets take the fallback instead
// of aliased-slot dispatch).
func emitDecoderCacheLookup(lc *lowerCtx, tgt int16) {
	// MOVQ tgt, R10                ; R10 = working copy of tgt
	lc.emitRR(x86.AMOVQ, tgt, amd64Scratch1)

	// SUBQ [RBX+104], R10          ; R10 = tgt - vaddrBegin (may underflow)
	sub := lc.c.NewProg()
	sub.As = x86.ASUBQ
	sub.From.Type = obj.TYPE_MEM
	sub.From.Reg = amd64RegSret
	sub.From.Offset = 104
	sub.To.Type = obj.TYPE_REG
	sub.To.Reg = amd64Scratch1
	lc.c.Append(sub)

	// CMPQ R10, [RBX+112]          ; unsigned: R10 < segSize ?
	cmpSize := lc.c.NewProg()
	cmpSize.As = x86.ACMPQ
	cmpSize.From.Type = obj.TYPE_REG
	cmpSize.From.Reg = amd64Scratch1
	cmpSize.To.Type = obj.TYPE_MEM
	cmpSize.To.Reg = amd64RegSret
	cmpSize.To.Offset = 112
	lc.c.Append(cmpSize)

	// JAE .dc_miss                  ; unsigned above-or-equal → out of range
	jae := lc.c.NewProg()
	jae.As = x86.AJCC
	jae.To.Type = obj.TYPE_BRANCH
	lc.c.Append(jae)

	// SHRQ $1, R10                 ; R10 = insn-slot index
	lc.emitRI(x86.ASHRQ, 1, amd64Scratch1)
	// SHLQ $3, R10                 ; R10 = byte offset (idx * 8)
	lc.emitRI(x86.ASHLQ, 3, amd64Scratch1)
	// ANDQ [RBX+96], R10           ; R10 &= decoder_cache mask
	andMask := lc.c.NewProg()
	andMask.As = x86.AANDQ
	andMask.From.Type = obj.TYPE_MEM
	andMask.From.Reg = amd64RegSret
	andMask.From.Offset = 96
	andMask.To.Type = obj.TYPE_REG
	andMask.To.Reg = amd64Scratch1
	lc.c.Append(andMask)

	// ADDQ [RBX+88], R10           ; R10 += decoder_cache base → slot address
	addBase := lc.c.NewProg()
	addBase.As = x86.AADDQ
	addBase.From.Type = obj.TYPE_MEM
	addBase.From.Reg = amd64RegSret
	addBase.From.Offset = 88
	addBase.To.Type = obj.TYPE_REG
	addBase.To.Reg = amd64Scratch1
	lc.c.Append(addBase)

	// MOVQ (R10), R10              ; R10 = chainEntry  (or 0 if slot empty)
	loadEntry := lc.c.NewProg()
	loadEntry.As = x86.AMOVQ
	loadEntry.From.Type = obj.TYPE_MEM
	loadEntry.From.Reg = amd64Scratch1
	loadEntry.From.Offset = 0
	loadEntry.To.Type = obj.TYPE_REG
	loadEntry.To.Reg = amd64Scratch1
	lc.c.Append(loadEntry)

	// TESTQ R10, R10               ; set ZF if R10 == 0
	lc.emitRR(x86.ATESTQ, amd64Scratch1, amd64Scratch1)

	// JZ .dc_miss                  ; slot empty → fallback
	jz := lc.c.NewProg()
	jz.As = x86.AJEQ
	jz.To.Type = obj.TYPE_BRANCH
	lc.c.Append(jz)

	// ADDQ $frameSize, RSP         ; dealloc our spill frame (if any)
	if lc.frameSize > 0 {
		lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
	}
	// JMP R10                      ; direct jump to target's chainEntry
	lc.emitJmpReg(amd64Scratch1)

	// .dc_miss landing: NOP. Targets for JAE and JZ. Execution falls
	// through from here into the existing 2-way IC sequence that the
	// caller emits next.
	dcMiss := lc.emitNOP()
	jae.To.SetTarget(dcMiss)
	jz.To.SetTarget(dcMiss)
}

// emitMovabsSentinel emits a `MOVABS R10, 0x7BADC0DE7BADC0DE` — the
// 10-byte encoding the assembler picks when the immediate doesn't fit
// in sign-extended int32. Returns the Prog so callers can record
// Prog.Pc for post-assembly patch-offset math (imm64 lives at Pc+2).
func (lc *lowerCtx) emitMovabsSentinel(sentinel int64) *obj.Prog {
	p := lc.c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = sentinel
	p.To.Type = obj.TYPE_REG
	p.To.Reg = amd64Scratch1
	lc.c.Append(p)
	return p
}

// emitJalrMissStub emits the per-site miss stub, appended after the
// block's main code. It writes the miss status + site index to sret
// and RETs. The Go dispatcher reads sret.Status == JitOKJalrMiss and
// sret.FaultAddr (repurposed) to find the site to patch.
//
// On entry sret.PC was already written by lowerJalrIC.
//
//	ADDQ   $frameSize, RSP          ; if frameSize > 0
//	MOVQ   RBP, 8(RBX)              ; sret.IC
//	MOVQ   $JitOKJalrMiss, 16(RBX)  ; sret.Status
//	MOVQ   $siteIdx, 24(RBX)        ; sret.FaultAddr (repurposed for siteIdx)
//	RET
func (lc *lowerCtx) emitJalrMissStub(siteIdx int) *obj.Prog {
	var firstProg *obj.Prog
	if lc.frameSize > 0 {
		p := lc.c.NewProg()
		p.As = x86.AADDQ
		p.From.Type = obj.TYPE_CONST
		p.From.Offset = lc.frameSize
		p.To.Type = obj.TYPE_REG
		p.To.Reg = goasm.REG_AMD64_SP
		lc.c.Append(p)
		firstProg = p
	}

	// MOVQ R11, 0(RBX) — sret.PC (target PC was stashed in R11 by lowerJalrIC).
	pcWrite := lc.c.NewProg()
	pcWrite.As = x86.AMOVQ
	pcWrite.From.Type = obj.TYPE_REG
	pcWrite.From.Reg = amd64Scratch2
	pcWrite.To.Type = obj.TYPE_MEM
	pcWrite.To.Reg = amd64RegSret
	pcWrite.To.Offset = 0
	lc.c.Append(pcWrite)
	if firstProg == nil {
		firstProg = pcWrite
	}

	// MOVQ RBP, 8(RBX) — sret.IC
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)

	// MOVQ $JitOKJalrMiss, 16(RBX) — sret.Status
	lc.emitMI(x86.AMOVQ, JitOKJalrMiss, amd64RegSret, 16)
	// MOVQ $siteIdx, 24(RBX) — sret.FaultAddr repurposed
	lc.emitMI(x86.AMOVQ, int64(siteIdx), amd64RegSret, 24)

	ret := lc.c.NewProg()
	ret.As = obj.ARET
	lc.c.Append(ret)

	return firstProg
}

// emitSlowExitStub emits a slow exit stub that writes Result to sret and RETs.
// Called when a chain exit hasn't been patched yet.
func (lc *lowerCtx) emitSlowExitStub(targetPC uint64) *obj.Prog {
	// Record the first prog of the stub for offset computation.
	firstProg := lc.c.NewProg()
	firstProg.As = x86.AMOVQ
	firstProg.From.Type = obj.TYPE_CONST
	firstProg.From.Offset = int64(targetPC)
	firstProg.To.Type = obj.TYPE_REG
	firstProg.To.Reg = amd64Scratch1
	lc.c.Append(firstProg)

	lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0) // Result.PC
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)    // Result.IC
	lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 16)            // Result.Status = jitOK
	lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)            // Result.FaultAddr = 0
	// No spill frame to deallocate — already done before MOVABS in chain exit.
	p := lc.c.NewProg()
	p.As = obj.ARET
	lc.c.Append(p)
	return firstProg
}

// ── Instruction lowering dispatch ──

func (lc *lowerCtx) lowerInstr(ins *IRInstr) error {
	// Invalidate scratch cache for ops that clobber scratch registers (R10/R11)
	// outside the standard use/def/defCommit path, or at control flow boundaries.
	switch ins.Op {
	case IRDivS, IRDivU, IRRem, IRRemU, IRMulHS, IRMulHU, IRMulHSU, // R10/R11 for operand shuffling
		IRShl, IRShr, IRSar, // may save CX to R11
		IRLoadX, IRStoreX, // R10/R11 for address computation
		IRLabel, IRBranch, IRBranchImm, IRJump, // control flow
		IRRet, IRRetDyn, IRChainExit, IRJalrIC, IRCall, IRSyscall, // exits/calls
		IRFAdd, IRFSub, IRFMul, IRFDiv, IRFSqrt, IRFNeg, IRFAbs, IRFCmp, // FP uses FP scratch
		IRFma, IRFmsub, IRFnmadd, IRFnmsub,                               // FP ternary (FMA family)
		IRFCvtToI, IRFCvtToU, IRFCvtFromI, IRFCvtFromU, IRFCvtFF: // FP conversions
		lc.invalidateScratchCache()
	}
	switch ins.Op {
	case IROpInvalid:
		return fmt.Errorf("ir.LowerAMD64: invalid op at index %d", lc.idx)

	// Data movement
	case IRMov:
		lc.lowerMov(ins)
	case IRConst:
		lc.lowerConst(ins)
	case IRSext:
		lc.lowerSext(ins)
	case IRZext:
		lc.lowerZext(ins)

	// Integer ALU
	case IRAdd:
		lc.lowerBinop(ins, x86.AADDQ, true)
	case IRAddImm:
		lc.lowerBinopImm(ins, x86.AADDQ, x86.AINCQ, x86.ADECQ)
	case IRSub:
		lc.lowerBinop(ins, x86.ASUBQ, false)
	case IRSubImm:
		lc.lowerBinopImm(ins, x86.ASUBQ, 0, 0)
	case IRMul:
		lc.lowerBinop(ins, x86.AIMULQ, true)
	case IRNeg:
		lc.lowerUnary(ins, x86.ANEGQ)

	// DIV/MUL high
	case IRDivS:
		lc.lowerDiv(ins, true, false)
	case IRDivU:
		lc.lowerDiv(ins, false, false)
	case IRRem:
		lc.lowerDiv(ins, true, true)
	case IRRemU:
		lc.lowerDiv(ins, false, true)
	case IRMulHS:
		lc.lowerMulHigh(ins, true)
	case IRMulHU:
		lc.lowerMulHigh(ins, false)
	case IRMulHSU:
		lc.lowerMulHSU(ins)

	// Shifts
	case IRShl:
		lc.lowerShift(ins, x86.ASHLQ)
	case IRShlImm:
		lc.lowerShiftImm(ins, x86.ASHLQ)
	case IRShr:
		lc.lowerShift(ins, x86.ASHRQ)
	case IRShrImm:
		lc.lowerShiftImm(ins, x86.ASHRQ)
	case IRSar:
		lc.lowerShift(ins, x86.ASARQ)
	case IRSarImm:
		lc.lowerShiftImm(ins, x86.ASARQ)

	// Bitwise
	case IRAnd:
		lc.lowerBinop(ins, x86.AANDQ, true)
	case IRAndImm:
		lc.lowerBinopImm(ins, x86.AANDQ, 0, 0)
	case IROr:
		lc.lowerBinop(ins, x86.AORQ, true)
	case IROrImm:
		lc.lowerBinopImm(ins, x86.AORQ, 0, 0)
	case IRXor:
		lc.lowerBinop(ins, x86.AXORQ, true)
	case IRXorImm:
		lc.lowerBinopImm(ins, x86.AXORQ, 0, 0)
	case IRNot:
		lc.lowerUnary(ins, x86.ANOTQ)

	// Bit manipulation
	case IRClz:
		if ins.T == I32 {
			lc.lowerUnary(ins, x86.ALZCNTL)
		} else {
			lc.lowerUnary(ins, x86.ALZCNTQ)
		}
	case IRCtz:
		if ins.T == I32 {
			lc.lowerUnary(ins, x86.ATZCNTL)
		} else {
			lc.lowerUnary(ins, x86.ATZCNTQ)
		}
	case IRPopcount:
		if ins.T == I32 {
			lc.lowerUnary(ins, x86.APOPCNTL)
		} else {
			lc.lowerUnary(ins, x86.APOPCNTQ)
		}
	case IRBswap:
		lc.lowerUnary(ins, x86.ABSWAPQ)

	// Comparison
	case IRSet:
		lc.lowerSet(ins)
	case IRSetImm:
		lc.lowerSetImm(ins)

	// Memory
	case IRLoad:
		lc.lowerLoad(ins)
	case IRStore:
		lc.lowerStore(ins)
	case IRLoadX:
		lc.lowerLoadX(ins)
	case IRStoreX:
		lc.lowerStoreX(ins)

	// Control flow
	case IRLabel:
		lc.placeLabel(Label(ins.Imm))
	case IRBranch:
		lc.lowerBranch(ins)
	case IRBranchImm:
		lc.lowerBranchImm(ins)
	case IRJump:
		lc.lowerJump(ins)
	case IRRet:
		lc.lowerRet(ins)
	case IRRetDyn:
		lc.lowerRetDyn(ins)
	case IRChainExit:
		lc.lowerChainExit(ins)
	case IRJalrIC:
		lc.lowerJalrIC(ins)
	case IRCall:
		lc.lowerCall(ins)
	case IRSyscall:
		lc.lowerSyscall(ins)

	// FP arithmetic
	case IRFAdd:
		lc.lowerFPBinop(ins, x86.AADDSD, x86.AADDSS)
	case IRFSub:
		lc.lowerFPBinop(ins, x86.ASUBSD, x86.ASUBSS)
	case IRFMul:
		lc.lowerFPBinop(ins, x86.AMULSD, x86.AMULSS)
	case IRFDiv:
		lc.lowerFPBinop(ins, x86.ADIVSD, x86.ADIVSS)
	case IRFSqrt:
		lc.lowerFPUnary(ins, x86.ASQRTSD, x86.ASQRTSS)
	case IRFma:
		lc.lowerFMA(ins, x86.AVFMADD213SD, x86.AVFMADD213SS)
	case IRFmsub:
		lc.lowerFMA(ins, x86.AVFMSUB213SD, x86.AVFMSUB213SS)
	case IRFnmadd:
		// RISC-V FNMADD = -(a*b + c). x86 VFNMSUB213 = -(a*b) - c. Match.
		lc.lowerFMA(ins, x86.AVFNMSUB213SD, x86.AVFNMSUB213SS)
	case IRFnmsub:
		// RISC-V FNMSUB = -(a*b - c) = -a*b + c. x86 VFNMADD213 = -(a*b) + c. Match.
		lc.lowerFMA(ins, x86.AVFNMADD213SD, x86.AVFNMADD213SS)
	case IRFNeg:
		lc.lowerFNeg(ins)
	case IRFAbs:
		lc.lowerFAbs(ins)
	case IRFCmp:
		lc.lowerFCmp(ins)

	// FP conversions
	case IRFCvtToI:
		lc.lowerFCvtToI(ins)
	case IRFCvtToU:
		lc.lowerFCvtToU(ins)
	case IRFCvtFromI:
		lc.lowerFCvtFromI(ins)
	case IRFCvtFromU:
		lc.lowerFCvtFromU(ins)
	case IRFCvtFF:
		lc.lowerFCvtFF(ins)

	// Pseudo-ops — no code emitted.
	case IRMarkLive, IRMarkDead, IRWriteback:
		// no-op

	default:
		return fmt.Errorf("ir.LowerAMD64: unhandled op %v at index %d", ins.Op, lc.idx)
	}
	return nil
}

// ── Register resolution ──

// hostRegFor returns the x86 register constant for VReg v at instruction idx.
// Returns -1 if the VReg is unused or on stack.
func (lc *lowerCtx) hostRegFor(v VReg, idx int) int16 {
	if v == VRegZero {
		return -1
	}
	if int(v) >= len(lc.alloc.Kind) {
		return -1
	}
	if lc.alloc.Kind[v] != AllocReg {
		return -1
	}
	return lc.rIdx.lookup(v, idx)
}

// isXMMReg returns true if the register constant is an XMM register.
func isXMMReg(r int16) bool {
	return r >= goasm.REG_AMD64_X0 && r <= goasm.REG_AMD64_X15
}

// isVRegFP returns true if the VReg is a floating-point register.
// Uses a precomputed set built at the start of lowering.
func (lc *lowerCtx) isVRegFP(v VReg) bool {
	return lc.fpSet[v]
}

// use loads VReg v into a host register for reading. Returns the host register.
// If v is in a register, returns it directly.
// If v is on stack, emits a reload into the specified scratch register.
func (lc *lowerCtx) use(v VReg, scratchIdx int) int16 {
	if v == VRegZero {
		// Need a register containing 0. Use scratch and XOR it.
		scr := lc.scratch(scratchIdx)
		lc.emitRR(x86.AXORQ, scr, scr)
		return scr
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocReg {
		r := lc.hostRegFor(v, lc.idx)
		if r >= 0 {
			return r
		}
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if lc.isVRegFP(v) {
			scr := lc.fpScratch(scratchIdx)
			lc.fpSpillLoad(lc.alloc.SpillSlot[v], scr)
			return scr
		}
		scr := lc.scratch(scratchIdx)
		if lc.scratchCache[scratchIdx].valid && lc.scratchCache[scratchIdx].vr == v {
			return scr // peephole: skip redundant spill load
		}
		lc.spillLoad(lc.alloc.SpillSlot[v], scr)
		lc.scratchCache[scratchIdx] = scratchEntry{v, true}
		return scr
	}
	// VReg not found in allocation — shouldn't happen for valid IR.
	return lc.scratch(scratchIdx)
}

// def returns the host register to write VReg v into.
// If v is in a register, returns it. If on stack, returns scratch.
// Caller must call defCommit() after writing.
func (lc *lowerCtx) def(v VReg) int16 {
	if v == VRegZero {
		return lc.scratch(0) // writes discarded
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocReg {
		r := lc.hostRegFor(v, lc.idx)
		if r >= 0 {
			return r
		}
	}
	if lc.isVRegFP(v) {
		return lc.fpScratch(0)
	}
	return lc.scratch(0)
}

// defCommit writes back to the spill slot if v is stack-allocated.
func (lc *lowerCtx) defCommit(v VReg, hostReg int16) {
	if v == VRegZero {
		return
	}
	if int(v) < len(lc.alloc.Kind) && lc.alloc.Kind[v] == AllocStack {
		if isXMMReg(hostReg) {
			lc.fpSpillStore(hostReg, lc.alloc.SpillSlot[v])
		} else {
			lc.spillStore(hostReg, lc.alloc.SpillSlot[v])
			// Update scratch cache: this scratch now holds v's new value.
			if hostReg == amd64Scratch1 {
				lc.scratchCache[0] = scratchEntry{v, true}
				if lc.scratchCache[1].valid && lc.scratchCache[1].vr == v {
					lc.scratchCache[1].valid = false
				}
			} else if hostReg == amd64Scratch2 {
				lc.scratchCache[1] = scratchEntry{v, true}
				if lc.scratchCache[0].valid && lc.scratchCache[0].vr == v {
					lc.scratchCache[0].valid = false
				}
			}
		}
	}
}

// scratchEntry tracks which spilled VReg's value is resident in a scratch register.
type scratchEntry struct {
	vr    VReg
	valid bool
}

func (lc *lowerCtx) invalidateScratchCache() {
	lc.scratchCache[0].valid = false
	lc.scratchCache[1].valid = false
}

func (lc *lowerCtx) scratch(idx int) int16 {
	if idx == 0 {
		return amd64Scratch1
	}
	return amd64Scratch2
}

// fpScratch returns an XMM scratch register for FP spill/reload.
// Uses XMM15 (scratch0) and XMM14 (scratch1). These are at the end of the
// FP pool; the allocator should not assign them when they're needed for scratch.
// TODO: formally reserve these from the FP pool.
func (lc *lowerCtx) fpScratch(idx int) int16 {
	if idx == 0 {
		return goasm.REG_AMD64_X15
	}
	return goasm.REG_AMD64_X14
}

// fpSpillLoad loads from spill slot into an XMM register.
func (lc *lowerCtx) fpSpillLoad(slot int16, dst int16) {
	lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

// fpSpillStore stores from an XMM register into a spill slot.
func (lc *lowerCtx) fpSpillStore(src int16, slot int16) {
	lc.emitMR(x86.AMOVSD, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// ── Prog emission helpers ──

func (lc *lowerCtx) emitRR(op obj.As, src, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) emitRI(op obj.As, imm int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) emitRM(op obj.As, base int16, disp int64, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = disp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) emitMR(op obj.As, src int16, base int16, disp int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = disp
	lc.c.Append(p)
}

func (lc *lowerCtx) emitMI(op obj.As, imm int64, base int16, disp int64) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = disp
	lc.c.Append(p)
}

// emitUnary emits a single-operand instruction (e.g., NEGQ, NOTQ, INCQ).
func (lc *lowerCtx) emitUnary(op obj.As, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

// emitCmpRI emits CMPQ reg, $imm. CMP has reversed operand convention
// vs ADD/SUB in the Go assembler: ycmpl expects From=reg, To=const,
// while yaddl expects From=const, To=reg.
func (lc *lowerCtx) emitCmpRI(reg int16, imm int64) {
	p := lc.c.NewProg()
	p.As = x86.ACMPQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = reg
	p.To.Type = obj.TYPE_CONST
	p.To.Offset = imm
	lc.c.Append(p)
}

// byteReg maps a 64-bit register constant to its byte variant.
// In the Go assembler's x86 register numbering, byte registers (AL..R15B)
// are at offset 0-15 from RBaseAMD64, and 64-bit registers (AX..R15) are
// at offset 16-31. So byteReg = fullReg - 16.
func byteReg(r int16) int16 {
	return r - 16
}

// spillLoad loads from spill slot into dst.
func (lc *lowerCtx) spillLoad(slot int16, dst int16) {
	lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(slot)*8, dst)
}

// spillStore stores src into spill slot.
func (lc *lowerCtx) spillStore(src int16, slot int16) {
	lc.emitMR(x86.AMOVQ, src, goasm.REG_AMD64_SP, int64(slot)*8)
}

// loadImm64 loads a 64-bit immediate into a register.
func (lc *lowerCtx) loadImm64(imm int64, dst int16) {
	switch {
	case imm == 0:
		lc.emitRR(x86.AXORQ, dst, dst)
	case imm > 0 && uint64(imm) <= 0xFFFFFFFF:
		// MOVL (32-bit) auto-zero-extends
		lc.emitRI(x86.AMOVL, imm, dst)
	default:
		lc.emitRI(x86.AMOVQ, imm, dst)
	}
}

// ── Label resolution ──

func (lc *lowerCtx) placeLabel(l Label) {
	p := lc.c.NewProg()
	p.As = obj.ANOP
	lc.c.Append(p)
	lc.labelProg[l] = p
	// Resolve pending forward references.
	for _, bp := range lc.pending[l] {
		bp.To.SetTarget(p)
	}
	delete(lc.pending, l)
}

func (lc *lowerCtx) bindLabel(l Label, p *obj.Prog) {
	if target, ok := lc.labelProg[l]; ok {
		p.To.SetTarget(target)
	} else {
		lc.pending[l] = append(lc.pending[l], p)
	}
}

// ── Per-op lowering stubs (Phase B-G implementations) ──

func (lc *lowerCtx) lowerMov(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerConst(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	lc.loadImm64(ins.Imm, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerSext(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	var op obj.As
	switch ins.T {
	case I32:
		op = x86.AMOVLQSX // MOVSXD
	case I16:
		op = x86.AMOVWQSX
	case I8:
		op = x86.AMOVBQSX
	default:
		op = x86.AMOVQ // I64 → I64 = nop move
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerZext(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	var op obj.As
	switch ins.T {
	case I32:
		op = x86.AMOVL // auto-zeros upper 32
	case I16:
		op = x86.AMOVWQZX
	case I8:
		op = x86.AMOVBQZX
	default:
		op = x86.AMOVQ
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

// lowerBinop handles two-register binary ops (ADD, SUB, AND, OR, XOR, IMUL).
// If commutative and dst==b, swaps operands to avoid an extra MOV.
func (lc *lowerCtx) lowerBinop(ins *IRInstr, op obj.As, commutative bool) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	if dst == a {
		// dst = dst OP b
		lc.emitRR(op, b, dst)
	} else if commutative && dst == b {
		// dst = dst OP a (commutative, so a OP b == b OP a)
		lc.emitRR(op, a, dst)
	} else if dst == b {
		// Non-commutative and dst == b: MOV a,dst would destroy b.
		// Use scratch: MOV b,scratch; MOV a,dst; OP scratch,dst.
		scr := lc.scratch(0)
		if scr == a {
			scr = lc.scratch(1)
		}
		lc.emitRR(x86.AMOVQ, b, scr)
		lc.emitRR(x86.AMOVQ, a, dst)
		lc.emitRR(op, scr, dst)
	} else {
		// dst != a, dst != b: MOV a,dst; OP b,dst
		lc.emitRR(x86.AMOVQ, a, dst)
		lc.emitRR(op, b, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

// lowerBinopImm handles register-immediate binary ops.
// incOp/decOp are used for +1/-1 (pass 0 to disable).
func (lc *lowerCtx) lowerBinopImm(ins *IRInstr, op obj.As, incOp, decOp obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}

	imm := ins.Imm
	switch {
	case incOp != 0 && imm == 1:
		lc.emitUnary(incOp, dst)
	case decOp != 0 && imm == -1:
		lc.emitUnary(decOp, dst)
	case imm >= -(1<<31) && imm < (1<<31):
		lc.emitRI(op, imm, dst)
	default:
		// Large immediate: load into scratch, then op.
		// If A was spilled, use(A,1) returned R11; dst may be R11 too.
		scr := amd64Scratch2
		if dst == amd64Scratch2 {
			scr = amd64Scratch1
		}
		lc.loadImm64(imm, scr)
		lc.emitRR(op, scr, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerUnary(ins *IRInstr, op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}
	lc.emitUnary(op, dst)
	lc.defCommit(ins.Dst, dst)
}

// ── DIV/MUL stubs ──

func (lc *lowerCtx) lowerDiv(ins *IRInstr, signed, wantRem bool) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// Guard: if b is in RAX or RDX, save it to scratch before we clobber
	// those registers. (Currently the pool excludes RAX/RDX when DIV is
	// present, but this is a safety net.)
	bEff := b
	if b == goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch1)
		bEff = amd64Scratch1
	} else if b == goasm.REG_AMD64_DX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch1)
		bEff = amd64Scratch1
	}

	// Move dividend to RAX.
	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	if signed {
		// CQO: sign-extend RAX to RDX:RAX
		p := lc.c.NewProg()
		p.As = x86.ACQO
		lc.c.Append(p)
		// IDIVQ bEff
		p = lc.c.NewProg()
		p.As = x86.AIDIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = bEff
		lc.c.Append(p)
	} else {
		// XORQ RDX, RDX
		lc.emitRR(x86.AXORQ, goasm.REG_AMD64_DX, goasm.REG_AMD64_DX)
		// DIVQ bEff
		p := lc.c.NewProg()
		p.As = x86.ADIVQ
		p.From.Type = obj.TYPE_REG
		p.From.Reg = bEff
		lc.c.Append(p)
	}

	// Result: quotient in RAX, remainder in RDX.
	var result int16 = goasm.REG_AMD64_AX
	if wantRem {
		result = goasm.REG_AMD64_DX
	}
	if dst != result {
		lc.emitRR(x86.AMOVQ, result, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerMulHigh(ins *IRInstr, signed bool) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// Guard: if b is in RAX, MOV a→RAX would clobber it.
	bEff := b
	if b == goasm.REG_AMD64_AX && a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch1)
		bEff = amd64Scratch1
	}

	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	// One-operand MUL/IMUL: RDX:RAX = RAX * operand
	p := lc.c.NewProg()
	if signed {
		p.As = x86.AIMULQ
	} else {
		p.As = x86.AMULQ
	}
	p.From.Type = obj.TYPE_REG
	p.From.Reg = bEff
	lc.c.Append(p)

	// High result is in RDX.
	if dst != goasm.REG_AMD64_DX {
		lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerMulHSU(ins *IRInstr) {
	// MULHSU: high 64 bits of (signed a) * (unsigned b).
	// Approach: unsigned mul + correction when a is negative.
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// Guard: if b is in RAX, MOV a→RAX would clobber it.
	bEff := b
	if b == goasm.REG_AMD64_AX && a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, b, amd64Scratch2) // R11 = b
		bEff = amd64Scratch2
	}

	// Move a to RAX.
	if a != goasm.REG_AMD64_AX {
		lc.emitRR(x86.AMOVQ, a, goasm.REG_AMD64_AX)
	}

	// Compute sign correction: if a < 0, correction = b, else 0.
	lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_AX, amd64Scratch1) // R10 = a (from RAX)
	lc.emitRI(x86.ASARQ, 63, amd64Scratch1)                 // R10 = sign(a) replicated
	lc.emitRR(x86.AANDQ, bEff, amd64Scratch1)               // R10 = (a<0) ? b : 0
	p := lc.c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = bEff
	lc.c.Append(p)

	// Adjust: if a was negative, subtract b from high result.
	lc.emitRR(x86.ASUBQ, amd64Scratch1, goasm.REG_AMD64_DX)

	if dst != goasm.REG_AMD64_DX {
		lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_DX, dst)
	}
	lc.defCommit(ins.Dst, dst)
}

// ── Shifts ──

func (lc *lowerCtx) lowerShift(ins *IRInstr, op obj.As) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	dst := lc.def(ins.Dst)

	// x86 variable shifts require count in CL.
	//
	// Hazard: use(B,1) may return R11 (scratch2) for a spill-loaded count.
	// If we then save CX to R11 with a plain MOV, we clobber the count.
	// Fix: use XCHG when b occupies a scratch register — this atomically
	// swaps the CX save and count load without destroying either value.

	needCXSave := b != goasm.REG_AMD64_CX && dst != goasm.REG_AMD64_CX && lc.isCXLive()
	bInScratch := (b == amd64Scratch1 || b == amd64Scratch2)

	aSavedToR11 := false
	cxSaveReg := int16(-1) // where old CX is saved (-1 = not saved)

	if a == goasm.REG_AMD64_CX && b != goasm.REG_AMD64_CX {
		// CX holds 'a'; moving b→CX will destroy it.
		if bInScratch {
			// b is in a scratch reg (from spill). XCHG swaps: CX→scratch (save a),
			// scratch→CX (load count). One instruction, no clobber.
			lc.emitRR(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
			cxSaveReg = b
			aSavedToR11 = true
			b = goasm.REG_AMD64_CX // count is now in CX
		} else {
			// b is in a regular allocated register. Safe to save a to R11.
			lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
			aSavedToR11 = true
			cxSaveReg = amd64Scratch2
		}
	} else if needCXSave {
		// CX holds a live VReg (not a, not b, not dst). Must save it.
		if bInScratch {
			// b is in a scratch reg (from spill). XCHG saves CX and loads
			// count into CX in one step.
			lc.emitRR(x86.AXCHGQ, b, goasm.REG_AMD64_CX)
			cxSaveReg = b
			b = goasm.REG_AMD64_CX // count is now in CX
		} else {
			// b is in a regular register. Safe to save CX to R11.
			lc.emitRR(x86.AMOVQ, goasm.REG_AMD64_CX, amd64Scratch2)
			cxSaveReg = amd64Scratch2
		}
	}

	// Move count (b) into CX if not already there.
	if b != goasm.REG_AMD64_CX {
		lc.emitRR(x86.AMOVQ, b, goasm.REG_AMD64_CX)
	}

	// Effective location of 'a': saved location if we saved it, else original.
	aEff := a
	if aSavedToR11 {
		aEff = cxSaveReg // wherever the XCHG/MOV put it
	}

	// Move value (a) into dst and shift.
	if dst == goasm.REG_AMD64_CX {
		// dst IS CX — we just put the count there. Use scratch for the
		// shift, then move result back to CX.
		scr := amd64Scratch1
		if aEff != scr {
			lc.emitRR(x86.AMOVQ, aEff, scr)
		}
		lc.emitRR(op, goasm.REG_AMD64_CX, scr)
		lc.emitRR(x86.AMOVQ, scr, dst)
	} else {
		if dst != aEff {
			lc.emitRR(x86.AMOVQ, aEff, dst)
		}
		lc.emitRR(op, goasm.REG_AMD64_CX, dst)
	}

	// Restore CX if we saved it. Even when aSavedToR11 (a was in CX), the
	// VReg may still be live for future instructions — reading it now doesn't
	// make it dead, so CX must be restored.
	if cxSaveReg >= 0 && needCXSave {
		lc.emitRR(x86.AMOVQ, cxSaveReg, goasm.REG_AMD64_CX)
	}

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerShiftImm(ins *IRInstr, op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)
	if dst != a {
		lc.emitRR(x86.AMOVQ, a, dst)
	}
	lc.emitRI(op, ins.Imm, dst)
	lc.defCommit(ins.Dst, dst)
}

// isCXLive returns true if RCX currently holds a live allocated VReg.
func (lc *lowerCtx) isCXLive() bool {
	// Binary search the precomputed sorted CX intervals.
	idx := lc.idx
	lo, hi := 0, len(lc.cxLive)
	for lo < hi {
		mid := (lo + hi) / 2
		if lc.cxLive[mid].end < idx {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo < len(lc.cxLive) && lc.cxLive[lo].start <= idx && idx <= lc.cxLive[lo].end
}

// ── Comparison ──

func (lc *lowerCtx) lowerSet(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	// CMPQ a, b — Go asm CMPQ computes From - To, so From=a, To=b gives a - b.
	lc.emitRR(x86.ACMPQ, a, b)

	// SETcc into byte register, then zero-extend.
	setOp := predToSETcc(ins.Pred)
	bReg := byteReg(dst)
	p := lc.c.NewProg()
	p.As = setOp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = bReg
	lc.c.Append(p)

	lc.emitRR(x86.AMOVBQZX, bReg, dst)

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerSetImm(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)

	// CMP a, $imm — Go asm CMP expects From=reg, To=const for ycmpl.
	lc.emitCmpRI(a, ins.Imm)

	setOp := predToSETcc(ins.Pred)
	bReg := byteReg(dst)
	p := lc.c.NewProg()
	p.As = setOp
	p.To.Type = obj.TYPE_REG
	p.To.Reg = bReg
	lc.c.Append(p)

	lc.emitRR(x86.AMOVBQZX, bReg, dst)
	lc.defCommit(ins.Dst, dst)
}

func predToSETcc(p Pred) obj.As {
	switch p {
	case EQ:
		return x86.ASETEQ
	case NE:
		return x86.ASETNE
	case LT:
		return x86.ASETLT
	case LE:
		return x86.ASETLE
	case GT:
		return x86.ASETGT
	case GE:
		return x86.ASETGE
	case LTU:
		return x86.ASETCS
	case LEU:
		return x86.ASETLS
	case GTU:
		return x86.ASETHI
	case GEU:
		return x86.ASETCC
	default:
		return x86.ASETEQ
	}
}

// ── Memory ──

func (lc *lowerCtx) lowerLoad(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	base := lc.use(ins.A, 1)
	op := loadOp(ins.T)
	lc.emitRM(op, base, ins.Imm, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerStore(ins *IRInstr) {
	base := lc.use(ins.A, 0)
	src := lc.use(ins.B, 1)
	op := storeOp(ins.T)
	lc.emitMR(op, src, base, ins.Imm)
}

func (lc *lowerCtx) lowerLoadX(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	base := lc.use(ins.A, 0)
	idx := lc.use(ins.B, 1)
	op := loadOp(ins.T)

	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Index = idx
	p.From.Scale = int16(ins.Scale)
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerStoreX(ins *IRInstr) {
	// In IRStoreX, Dst field is repurposed as the value to store.
	// 3 operands (base, idx, src) but only 2 scratch regs.
	// Strategy: load base(scratch0) and idx(scratch1), then collapse
	// base+idx*scale via LEA into scratch1 before loading src.
	base := lc.use(ins.A, 0)
	idx := lc.use(ins.B, 1)

	// Check if src needs a scratch register (i.e., is spilled).
	src := ins.Dst // the VReg, not yet resolved
	srcInReg := src != VRegZero &&
		int(src) < len(lc.alloc.Kind) && lc.alloc.Kind[src] == AllocReg
	srcReg := lc.hostRegFor(src, lc.idx)

	if !srcInReg || srcReg < 0 {
		// src is spilled (or VRegZero) — collapse base+idx into R11 via LEA,
		// then load src into R10, then store R10 → 0(R11).
		p := lc.c.NewProg()
		p.As = x86.ALEAQ
		p.From.Type = obj.TYPE_MEM
		p.From.Reg = base
		p.From.Index = idx
		p.From.Scale = int16(ins.Scale)
		p.To.Type = obj.TYPE_REG
		p.To.Reg = amd64Scratch2
		lc.c.Append(p)

		if src == VRegZero {
			lc.emitRR(x86.AXORQ, amd64Scratch1, amd64Scratch1)
		} else {
			lc.spillLoad(lc.alloc.SpillSlot[src], amd64Scratch1)
		}
		lc.emitMR(storeOp(ins.T), amd64Scratch1, amd64Scratch2, 0)
		return
	}

	// src is in a register — no scratch conflict.
	op := storeOp(ins.T)
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = srcReg
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Index = idx
	p.To.Scale = int16(ins.Scale)
	lc.c.Append(p)
}

func loadOp(t Type) obj.As {
	switch t {
	case I8:
		return x86.AMOVBQZX
	case I16:
		return x86.AMOVWQZX
	case I32:
		return x86.AMOVL
	case I64:
		return x86.AMOVQ
	case F32:
		return x86.AMOVSS
	case F64:
		return x86.AMOVSD
	default:
		return x86.AMOVQ
	}
}

func storeOp(t Type) obj.As {
	switch t {
	case I8:
		return x86.AMOVB
	case I16:
		return x86.AMOVW
	case I32:
		return x86.AMOVL
	case I64:
		return x86.AMOVQ
	case F32:
		return x86.AMOVSS
	case F64:
		return x86.AMOVSD
	default:
		return x86.AMOVQ
	}
}

// ── Control flow ──

func (lc *lowerCtx) lowerBranch(ins *IRInstr) {
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	// CMPQ a, b — Go asm CMPQ computes From - To, so From=a, To=b gives a - b.
	lc.emitRR(x86.ACMPQ, a, b)

	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtx) lowerBranchImm(ins *IRInstr) {
	a := lc.use(ins.A, 0)

	// CMP a, $imm2 — Go asm CMP: From=reg, To=const.
	if ins.Imm2 >= -(1<<31) && ins.Imm2 < (1<<31) {
		lc.emitCmpRI(a, ins.Imm2)
	} else {
		lc.loadImm64(ins.Imm2, amd64Scratch2)
		lc.emitRR(x86.ACMPQ, a, amd64Scratch2)
	}

	jOp := predToJcc(ins.Pred)
	p := lc.c.NewProg()
	p.As = jOp
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func (lc *lowerCtx) lowerJump(ins *IRInstr) {
	p := lc.c.NewProg()
	p.As = obj.AJMP
	p.To.Type = obj.TYPE_BRANCH
	lc.c.Append(p)
	lc.bindLabel(Label(ins.Imm), p)
}

func predToJcc(p Pred) obj.As {
	switch p {
	case EQ:
		return x86.AJEQ
	case NE:
		return x86.AJNE
	case LT:
		return x86.AJLT
	case LE:
		return x86.AJLE
	case GT:
		return x86.AJGT
	case GE:
		return x86.AJGE
	case LTU:
		return x86.AJCS
	case LEU:
		return x86.AJLS
	case GTU:
		return x86.AJHI
	case GEU:
		return x86.AJCC
	default:
		return x86.AJEQ
	}
}

func (lc *lowerCtx) lowerRet(ins *IRInstr) {
	// Write JITResult to sret buffer (RBX).
	// Offset 0: pc (from Imm)
	lc.loadImm64(ins.Imm, amd64Scratch1)
	lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0)

	// Offset 8: ic (pinned to RBP)
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)

	// Offset 16: status (from Imm2)
	lc.emitMI(x86.AMOVQ, ins.Imm2, amd64RegSret, 16)

	// Offset 24: faultAddr (from VReg A)
	if ins.A != VRegZero {
		fa := lc.use(ins.A, 0)
		lc.emitMR(x86.AMOVQ, fa, amd64RegSret, 24)
	} else {
		lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)
	}

	lc.emitEpilogue()
}

// lowerRetDyn handles IRRetDyn: return with runtime-computed PC from VReg A.
// Layout: {pc=A, status=Imm, faultAddr=B}.
func (lc *lowerCtx) lowerRetDyn(ins *IRInstr) {
	// Offset 0: pc (from VReg A)
	if ins.A != VRegZero {
		pcReg := lc.use(ins.A, 0)
		lc.emitMR(x86.AMOVQ, pcReg, amd64RegSret, 0)
	} else {
		lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 0)
	}

	// Offset 8: ic (pinned to RBP)
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)

	// Offset 16: status (from Imm)
	lc.emitMI(x86.AMOVQ, ins.Imm, amd64RegSret, 16)

	// Offset 24: faultAddr (from VReg B)
	if ins.B != VRegZero {
		fa := lc.use(ins.B, 1)
		lc.emitMR(x86.AMOVQ, fa, amd64RegSret, 24)
	} else {
		lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)
	}

	lc.emitEpilogue()
}

func (lc *lowerCtx) lowerCall(ins *IRInstr) {
	// Look up the symbol address.
	if int(ins.Imm) >= len(lc.blk.CTab) {
		return
	}
	sym := lc.blk.CTab[ins.Imm]

	// Save all live caller-saved registers. SysV ABI clobbers:
	// RAX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, XMM0-XMM15.
	// Pinned callee-saved regs (R12-R15, RBX) survive the call naturally.
	liveInt, liveFP := lc.liveCallerSaved()

	// Push live int regs to stack (using SUB RSP + MOV sequence).
	saveSize := int64(len(liveInt)+len(liveFP)) * 8
	if saveSize > 0 {
		lc.emitRI(x86.ASUBQ, saveSize, goasm.REG_AMD64_SP)
	}
	for i, r := range liveInt {
		lc.emitMR(x86.AMOVQ, r, goasm.REG_AMD64_SP, int64(i)*8)
	}
	for i, r := range liveFP {
		lc.emitMR(x86.AMOVSD, r, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8)
	}

	// Load symbol address into scratch and CALL.
	lc.loadImm64(int64(sym.Addr), amd64Scratch1)
	p := lc.c.NewProg()
	p.As = obj.ACALL
	p.To.Type = obj.TYPE_REG
	p.To.Reg = amd64Scratch1
	lc.c.Append(p)

	// Restore live regs.
	for i, r := range liveInt {
		lc.emitRM(x86.AMOVQ, goasm.REG_AMD64_SP, int64(i)*8, r)
	}
	for i, r := range liveFP {
		lc.emitRM(x86.AMOVSD, goasm.REG_AMD64_SP, int64(len(liveInt)+i)*8, r)
	}
	if saveSize > 0 {
		lc.emitRI(x86.AADDQ, saveSize, goasm.REG_AMD64_SP)
	}
}

// lowerSyscall lowers IRSyscall — the ECALL fast path.
//
// Imm  = pc+4 (where to resume after the syscall)
// Imm2 = CTab index for the dispatcher symbol (Addr = SysV-ABI entry)
//
// The emitter is required to have emitted WriteBack for all dirty
// VRegs beforehand so the dispatcher sees fresh values in x[]. The
// dispatcher may overwrite x[10] (a0 = return value / -errno).
//
// Emits: set up SysV args from pinned regs, CALL dispatcher, write
// sret with Status = RAX, emit epilogue. Terminator.
func (lc *lowerCtx) lowerSyscall(ins *IRInstr) {
	if int(ins.Imm2) < 0 || int(ins.Imm2) >= len(lc.blk.CTab) {
		return
	}
	sym := lc.blk.CTab[ins.Imm2]

	// Move pinned-callee-saved JIT context to SysV caller-saved arg regs.
	//   RDI = xBase (R12)
	//   RSI = memBase (R14)
	//   RDX = memMask (R15)
	lc.emitRR(x86.AMOVQ, amd64RegXBase, goasm.REG_AMD64_DI)
	lc.emitRR(x86.AMOVQ, amd64RegMemBase, goasm.REG_AMD64_SI)
	lc.emitRR(x86.AMOVQ, amd64RegMemMask, goasm.REG_AMD64_DX)

	// MOVABS $dispatcher, scratch; CALL scratch.
	lc.loadImm64(int64(sym.Addr), amd64Scratch1)
	callProg := lc.c.NewProg()
	callProg.As = obj.ACALL
	callProg.To.Type = obj.TYPE_REG
	callProg.To.Reg = amd64Scratch1
	lc.c.Append(callProg)

	// Inline fast path (Option D): when InlineSyscall is on, check the
	// dispatcher's return (0=handled, nonzero=fallback). On 0, chain-
	// exit to the post-ECALL block. On nonzero, fall through into the
	// cold-path sret+RET below (today's behavior).
	//
	//   TESTQ RAX, RAX
	//   JNE   L_cold
	//   <dealloc frame>
	//   MOVABS R10, <sentinel>   ; patched to post-ECALL chainEntry
	//   JMP    R10
	// L_cold:
	//   ... existing sret write + emitEpilogue ...
	if InlineSyscall {
		lc.emitRR(x86.ATESTQ, goasm.REG_AMD64_AX, goasm.REG_AMD64_AX)

		jneCold := lc.c.NewProg()
		jneCold.As = x86.AJNE
		jneCold.To.Type = obj.TYPE_BRANCH
		lc.c.Append(jneCold)

		// Hot path: dealloc spill frame, MOVABS sentinel, JMP R10.
		// Mirrors lowerChainExit; registers a chainExitInfo so finalize
		// emits the slow-exit stub and backpatching resolves the target.
		if lc.frameSize > 0 {
			lc.emitRI(x86.AADDQ, lc.frameSize, goasm.REG_AMD64_SP)
		}
		const sentinel = int64(0x7BADC0DE7BADC0DE)
		movProg := lc.emitMovabsSentinel(sentinel)
		lc.chainExits = append(lc.chainExits, chainExitInfo{
			targetPC: uint64(ins.Imm),
			movProg:  movProg,
		})
		lc.emitJmpReg(amd64Scratch1)

		// L_cold label. Control reaches here only via the JNE taken branch.
		coldLabel := lc.emitNOP()
		jneCold.To.SetTarget(coldLabel)
	}

	// Cold path (the only path when !InlineSyscall):
	//   [RBX+ 0] = pc+4 (Imm)
	//   [RBX+ 8] = ic  (RBP)
	//   [RBX+16] = status (RAX — dispatcher's return: 0=jitOK or 1=jitEcall)
	//   [RBX+24] = 0
	lc.loadImm64(ins.Imm, amd64Scratch1)
	lc.emitMR(x86.AMOVQ, amd64Scratch1, amd64RegSret, 0)
	lc.emitMR(x86.AMOVQ, amd64RegIC, amd64RegSret, 8)
	lc.emitMR(x86.AMOVQ, goasm.REG_AMD64_AX, amd64RegSret, 16)
	lc.emitMI(x86.AMOVQ, 0, amd64RegSret, 24)

	lc.emitEpilogue()
}

// liveCallerSaved returns the caller-saved registers that hold live VRegs
// at the current instruction index. These must be saved/restored around CALLs.
func (lc *lowerCtx) liveCallerSaved() (intRegs, fpRegs []int16) {
	// Caller-saved integer registers (all pool regs are caller-saved).
	callerSavedInt := []int16{
		goasm.REG_AMD64_AX, goasm.REG_AMD64_CX, goasm.REG_AMD64_DX,
		goasm.REG_AMD64_BP, goasm.REG_AMD64_SI, goasm.REG_AMD64_DI,
		goasm.REG_AMD64_R8, goasm.REG_AMD64_R9,
	}
	for _, r := range callerSavedInt {
		if lc.isRegLive(r) {
			intRegs = append(intRegs, r)
		}
	}
	// All XMM registers are caller-saved.
	for i := int16(0); i < 16; i++ {
		r := goasm.REG_AMD64_X0 + i
		if lc.isRegLive(r) {
			fpRegs = append(fpRegs, r)
		}
	}
	return
}

// isRegLive returns true if a host register holds a live allocated VReg
// at the current instruction index.
func (lc *lowerCtx) isRegLive(hostReg int16) bool {
	for i := range lc.alloc.IntervalMap {
		ia := &lc.alloc.IntervalMap[i]
		if ia.Host == hostReg &&
			ia.Interval.Start <= lc.idx && lc.idx <= ia.Interval.End {
			return true
		}
	}
	return false
}

// ── FP ops ──

// lowerFMA lowers a ternary FP op (IRFma, IRFmsub, IRFnmadd, IRFnmsub)
// to x86 VFMADD213/VFMSUB213/VFNMADD213/VFNMSUB213.
//
// Semantics: Dst = A op B (+ | -) C with a single rounding.
//
// x86 VFMADD213SS uses the encoding `VFMADD213SS dst, src1, src2`
// meaning `dst = dst*src1 + src2`. So we must stage A into dst first:
//   if dst != a: MOVSS/MOVSD a → dst
// Then: VFMADD213 dst, B, C.
func (lc *lowerCtx) lowerFMA(ins *IRInstr, f64op, f32op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)
	c := lc.use(ins.C, 2)

	op := f64op
	movOp := x86.AMOVSD
	if ins.T == F32 {
		op = f32op
		movOp = x86.AMOVSS
	}

	// Stage A into dst if needed.
	if dst != a {
		lc.emitRR(movOp, a, dst)
	}
	// VFMADD213 variants take (src1, src2, dst) in AT&T-order — in Go's
	// obj package that's (from1, from2, to) = (c, b, dst). The emitter
	// below mirrors AVX VEX encoding: result goes in dst, sources are
	// b (2nd operand from the instruction's perspective) and c.
	lc.emitVFMA3(op, c, b, dst)

	lc.defCommit(ins.Dst, dst)
}

// emitVFMA3 emits a 3-operand AVX FMA instruction. Go's obj x86
// representation for VFMADD213SS is: From=op3, From3=op2, To=op1
// where the Intel syntax is VFMADD213SS op1, op2, op3.
func (lc *lowerCtx) emitVFMA3(op obj.As, src1, src2, dst int16) {
	p := lc.c.NewProg()
	p.As = op
	p.From.Type = obj.TYPE_REG
	p.From.Reg = src1
	p.AddRestSourceReg(src2)
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dst
	lc.c.Append(p)
}

func (lc *lowerCtx) lowerFPBinop(ins *IRInstr, f64op, f32op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	op := f64op
	movOp := x86.AMOVSD
	if ins.T == F32 {
		op = f32op
		movOp = x86.AMOVSS
	}

	if dst != a {
		lc.emitRR(movOp, a, dst)
	}
	lc.emitRR(op, b, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFPUnary(ins *IRInstr, f64op, f32op obj.As) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	op := f64op
	if ins.T == F32 {
		op = f32op
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFNeg(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	movOp := x86.AMOVSD
	if ins.T == F32 {
		movOp = x86.AMOVSS
	}
	if dst != a {
		lc.emitRR(movOp, a, dst)
	}

	// Negate by flipping the sign bit via GPR round-trip.
	// MOVQ xmm→gpr, XOR with sign mask, MOVQ gpr→xmm.
	var mask int64
	if ins.T == F32 {
		mask = 1 << 31
	} else {
		mask = -1 << 63 // 0x8000000000000000
	}
	lc.emitRR(x86.AMOVQ, dst, amd64Scratch1)           // XMM → GPR (R10)
	lc.loadImm64(mask, amd64Scratch2)                  // R11 = sign mask
	lc.emitRR(x86.AXORQ, amd64Scratch2, amd64Scratch1) // R10 ^= R11
	lc.emitRR(x86.AMOVQ, amd64Scratch1, dst)           // GPR → XMM

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFAbs(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	movOp := x86.AMOVSD
	if ins.T == F32 {
		movOp = x86.AMOVSS
	}
	if dst != a {
		lc.emitRR(movOp, a, dst)
	}

	// Clear sign bit.
	var mask int64
	if ins.T == F32 {
		mask = 0x7FFFFFFF
	} else {
		mask = 0x7FFFFFFFFFFFFFFF
	}
	lc.emitRR(x86.AMOVQ, dst, amd64Scratch1)
	lc.loadImm64(mask, amd64Scratch2)
	lc.emitRR(x86.AANDQ, amd64Scratch2, amd64Scratch1)
	lc.emitRR(x86.AMOVQ, amd64Scratch1, dst)

	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFCmp(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 0)
	b := lc.use(ins.B, 1)

	// UCOMISD/UCOMISS sets CF/ZF/PF flags (unsigned-style), not SF/OF.
	// Go asm UCOMISD From, To computes To vs From (To - From for flags).
	// We want a vs b, so From=b, To=a → flags reflect a vs b.
	cmpOp := x86.AUCOMISD
	if ins.T == F32 {
		cmpOp = x86.AUCOMISS
	}
	lc.emitRR(cmpOp, b, a)

	bReg := byteReg(dst)

	// UCOMISD sets CF/ZF/PF. Must use unsigned condition codes.
	// EQ/NE need special NaN handling (PF=1 when unordered).
	switch ins.Pred {
	case EQ:
		// a == b: ZF=1 AND PF=0 (exclude NaN).
		// SETE + SETNP → AND result. Must use distinct byte regs.
		p1 := lc.c.NewProg()
		p1.As = x86.ASETEQ
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(amd64Scratch1)
		if dst == amd64Scratch1 {
			scrByte = byteReg(amd64Scratch2)
		}
		p2 := lc.c.NewProg()
		p2.As = x86.ASETPC // SETNP = SETPC (parity clear)
		p2.To.Type = obj.TYPE_REG
		p2.To.Reg = scrByte
		lc.c.Append(p2)
		lc.emitRR(x86.AANDB, scrByte, bReg)
	case NE:
		// a != b: ZF=0 OR PF=1 (NaN → not equal).
		p1 := lc.c.NewProg()
		p1.As = x86.ASETNE
		p1.To.Type = obj.TYPE_REG
		p1.To.Reg = bReg
		lc.c.Append(p1)
		scrByte := byteReg(amd64Scratch1)
		if dst == amd64Scratch1 {
			scrByte = byteReg(amd64Scratch2)
		}
		p2 := lc.c.NewProg()
		p2.As = x86.ASETPS // SETP = SETPS (parity set)
		p2.To.Type = obj.TYPE_REG
		p2.To.Reg = scrByte
		lc.c.Append(p2)
		lc.emitRR(x86.AORB, scrByte, bReg)
	default:
		// LT/LE/GT/GE use unsigned cc after UCOMISD.
		setOp := predToFPSETcc(ins.Pred)
		p := lc.c.NewProg()
		p.As = setOp
		p.To.Type = obj.TYPE_REG
		p.To.Reg = bReg
		lc.c.Append(p)
	}

	lc.emitRR(x86.AMOVBQZX, bReg, dst)
	lc.defCommit(ins.Dst, dst)
}

// predToFPSETcc maps comparison predicates to SETcc after UCOMISD/UCOMISS.
// UCOMISD sets CF/ZF/PF (like unsigned integer compare), not SF/OF.
func predToFPSETcc(p Pred) obj.As {
	switch p {
	case LT:
		return x86.ASETCS // CF=1 → a < b (below)
	case LE:
		return x86.ASETLS // CF=1 || ZF=1 → a <= b (below or equal)
	case GT:
		return x86.ASETHI // CF=0 && ZF=0 → a > b (above)
	case GE:
		return x86.ASETCC // CF=0 → a >= b (above or equal)
	default:
		return x86.ASETEQ // fallback
	}
}

// ── FP conversions ──

func (lc *lowerCtx) lowerFCvtToI(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	var cvtOp obj.As
	switch {
	case ins.U == F64 && ins.T == I64:
		cvtOp = x86.ACVTTSD2SQ
	case ins.U == F64 && (ins.T == I32 || ins.T == I16 || ins.T == I8):
		cvtOp = x86.ACVTTSD2SL
	case ins.U == F32 && ins.T == I64:
		cvtOp = x86.ACVTTSS2SQ
	case ins.U == F32 && (ins.T == I32 || ins.T == I16 || ins.T == I8):
		cvtOp = x86.ACVTTSS2SL
	default:
		cvtOp = x86.ACVTTSD2SQ
	}
	lc.emitRR(cvtOp, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFCvtToU(ins *IRInstr) {
	// x86 lacks unsigned FP→int conversion.
	// For now, use signed conversion (works for values < 2^63).
	// Full unsigned support requires range-check fixup.
	lc.lowerFCvtToI(ins)
}

func (lc *lowerCtx) lowerFCvtFromI(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	var cvtOp obj.As
	switch {
	case ins.U == I64 && ins.T == F64:
		cvtOp = x86.ACVTSQ2SD
	case (ins.U == I32 || ins.U == I16 || ins.U == I8) && ins.T == F64:
		cvtOp = x86.ACVTSL2SD
	case ins.U == I64 && ins.T == F32:
		cvtOp = x86.ACVTSQ2SS
	case (ins.U == I32 || ins.U == I16 || ins.U == I8) && ins.T == F32:
		cvtOp = x86.ACVTSL2SS
	default:
		cvtOp = x86.ACVTSQ2SD
	}
	lc.emitRR(cvtOp, a, dst)
	lc.defCommit(ins.Dst, dst)
}

func (lc *lowerCtx) lowerFCvtFromU(ins *IRInstr) {
	// x86 lacks unsigned int→FP conversion.
	// For now, use signed conversion (works for values < 2^63).
	lc.lowerFCvtFromI(ins)
}

func (lc *lowerCtx) lowerFCvtFF(ins *IRInstr) {
	dst := lc.def(ins.Dst)
	a := lc.use(ins.A, 1)

	var op obj.As
	if ins.U == F32 && ins.T == F64 {
		op = x86.ACVTSS2SD
	} else {
		op = x86.ACVTSD2SS
	}
	lc.emitRR(op, a, dst)
	lc.defCommit(ins.Dst, dst)
}

// ── Post-lowering peephole ──

// peepholeHostSpills removes redundant MOVQ pairs produced by the
// FixedStaticAllocator's spill-everything strategy. Patterns deleted
// (curr is the one unlinked; prev is kept intact):
//
//  1. MOVQ A, A               (self-move: degenerate no-op).
//  2. MOVQ A, B ; MOVQ B, A   (mirror: second restores what first wrote).
//
// Runs to a fixed point — deletions can expose new adjacent pairs.
//
// Safety: a pre-pass builds the set of Progs that MUST NOT be deleted —
// the chain-entry NOP, chain-exit MOVABS sentinels, JALR IC metadata
// Progs, label-NOP anchors (labelProg), and any Prog referenced as
// another Prog's To.Target. The matched patterns are MOVQ with REG or
// MEM operands only (not CONST), so MOVABS sentinels are naturally
// excluded from matching, but we keep them in the protected set as a
// belt-and-braces check in case the encoder adds variants.
func (lc *lowerCtx) peepholeHostSpills() int {
	protected := map[*obj.Prog]bool{}
	if lc.chainEntryProg != nil {
		protected[lc.chainEntryProg] = true
	}
	for _, ce := range lc.chainExits {
		if ce.movProg != nil {
			protected[ce.movProg] = true
		}
	}
	for _, ic := range lc.jalrICs {
		for _, p := range ic.pcMov {
			if p != nil {
				protected[p] = true
			}
		}
		for _, p := range ic.fnMov {
			if p != nil {
				protected[p] = true
			}
		}
		if ic.jeq0Prog != nil {
			protected[ic.jeq0Prog] = true
		}
		if ic.jne1Prog != nil {
			protected[ic.jne1Prog] = true
		}
		if ic.hit0Prog != nil {
			protected[ic.hit0Prog] = true
		}
	}
	for _, p := range lc.labelProg {
		if p != nil {
			protected[p] = true
		}
	}
	// Any Prog used as a branch target in the chain.
	for p := lc.c.First(); p != nil; p = p.Link {
		if p.To.Type == obj.TYPE_BRANCH {
			if t := p.To.Target(); t != nil {
				protected[t] = true
			}
		}
	}

	total := 0
	for {
		mutated := false
		removed := lc.c.Peephole(func(prev, curr *obj.Prog) bool {
			if protected[curr] {
				return false
			}
			// Pattern 1: MOVQ A, A — self-move.
			if curr.As == x86.AMOVQ &&
				curr.From.Type == obj.TYPE_REG &&
				curr.To.Type == obj.TYPE_REG &&
				curr.From.Reg == curr.To.Reg {
				return true
			}
			// Pattern 2: MOVQ A, B ; MOVQ B, A — mirror pair. curr is a
			// no-op because B received A from prev and we're storing it
			// back unchanged.
			if prev.As == x86.AMOVQ && curr.As == x86.AMOVQ &&
				addrEqual(prev.From, curr.To) &&
				addrEqual(prev.To, curr.From) {
				return true
			}
			// Pattern 3 (rewrite, not delete): MOVQ A, B ; MOVQ B, C →
			// MOVQ A, C. Same prog count but breaks the chain through B,
			// often enabling Pattern 1 or 2 to fire on the next pass.
			// Skip when the rewrite would produce an illegal mem→mem
			// MOVQ or leave curr unchanged (A == C).
			if prev.As == x86.AMOVQ && curr.As == x86.AMOVQ && !protected[prev] &&
				addrEqual(prev.To, curr.From) &&
				!addrEqual(prev.From, curr.From) {
				if prev.From.Type == obj.TYPE_MEM && curr.To.Type == obj.TYPE_MEM {
					return false // would be illegal mem→mem
				}
				if prev.From.Type != obj.TYPE_REG && prev.From.Type != obj.TYPE_MEM {
					return false // don't rewrite using CONST etc.
				}
				curr.From = prev.From
				mutated = true
				return false // mutated, not deleted
			}
			// Pattern 4 (dead store): MOVQ X, R ; MOVQ Y, R — curr
			// overwrites prev's destination before it can be read
			// (adjacent, nothing reads R between). Transform prev into
			// a self-move (MOVQ R, R) so Pattern 1 eliminates it on the
			// next pass. Skip when prev is already a self-move (prevents
			// infinite-mutation loop) or is protected (label target,
			// chain-exit MOVABS, etc). Safe because stack-slot reads
			// don't fault in our JIT, so prev's load side-effects (if
			// any) can be discarded.
			if prev.As == x86.AMOVQ && curr.As == x86.AMOVQ && !protected[prev] &&
				prev.To.Type == obj.TYPE_REG &&
				addrEqual(prev.To, curr.To) &&
				!addrEqual(prev.From, prev.To) {
				prev.From = prev.To
				mutated = true
				return false
			}
			return false
		})
		total += removed
		if removed == 0 && !mutated {
			break
		}
	}
	return total
}

// addrEqual reports whether two obj.Addr operands refer to the same
// register or memory location. Only REG and MEM types are compared;
// other types (CONST, BRANCH, etc.) always return false.
func addrEqual(a, b obj.Addr) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case obj.TYPE_REG:
		return a.Reg == b.Reg
	case obj.TYPE_MEM:
		return a.Reg == b.Reg &&
			a.Offset == b.Offset &&
			a.Index == b.Index &&
			a.Scale == b.Scale &&
			a.Name == b.Name
	}
	return false
}
