//go:build none

package riscv

import "sort"

// ── Sort adapters (avoid sort.Slice reflection overhead) ──

type intervalsByStart []Interval

func (s intervalsByStart) Len() int           { return len(s) }
func (s intervalsByStart) Less(i, j int) bool { return s[i].Start < s[j].Start }
func (s intervalsByStart) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type iepByPoint []iep

func (s iepByPoint) Len() int { return len(s) }
func (s iepByPoint) Less(i, j int) bool {
	if s[i].Point != s[j].Point {
		return s[i].Point < s[j].Point
	}
	// Same point, same VReg/interval (point interval): start before end.
	if s[i].VReg == s[j].VReg && s[i].Interval == s[j].Interval {
		return s[i].IsStart && !s[j].IsStart
	}
	// Same point, different intervals: ends before starts (free regs first).
	if s[i].IsStart != s[j].IsStart {
		return !s[i].IsStart
	}
	return s[i].VReg < s[j].VReg
}
func (s iepByPoint) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// ── Interval representation (paper's I(s)) ──

// Interval is one contiguous live range [Start, End] for a symbolic register.
// A symbolic register may have multiple disjoint intervals (the "interval set").
type Interval struct {
	VReg  VReg
	Start int // instruction index (paper's program point P)
	End   int // instruction index (paper's program point Q)
}

// ── Allocation output ──

// AllocKind classifies a VReg's allocation.
type AllocKind uint8

const (
	AllocUnused AllocKind = iota // VReg never referenced
	AllocReg                     // assigned to a host register (possibly different per interval)
	AllocStack                   // totally spilled — all accesses via memory
)

// IntervalAlloc is the register assignment for one interval of a VReg.
// Paper's reg(s, [P,Q]) = r_j.
type IntervalAlloc struct {
	Interval Interval
	Host     int16 // physical register assigned for this interval
}

// Allocation is the output of the register allocator.
type Allocation struct {
	// Per-VReg summary: Kind indicates overall disposition.
	Kind      []AllocKind // indexed by VReg
	SpillSlot []int16     // indexed by VReg; valid when Kind == AllocStack

	// Per-interval register assignment (paper's reg(s,[P,Q])).
	IntervalMap []IntervalAlloc

	// Moves to insert at control flow edges (paper's step 6).
	Moves []RegMove

	StackSlots int // total 8-byte spill slots needed
}

// RegMove is a register-to-register move to insert at a control flow edge.
type RegMove struct {
	InsertAt int   // instruction index (before which to insert)
	From     int16 // source host register
	To       int16 // destination host register
}

// RegPool describes the available host registers, separated by class.
type RegPool struct {
	IntRegs []int16 // host register IDs for integer VRegs
	FPRegs  []int16 // host register IDs for FP VRegs
}

// ── Internal data structures ──

// intervalSet is the collection of intervals for one symbolic register.
// Paper's I(s). In practice, average ~2 intervals per VReg.
type intervalSet struct {
	VReg      VReg
	Intervals []Interval // sorted by Start, non-overlapping
}

// iep is an Interval EndPoint — a start or end of an interval.
// Paper's IEP = set of all interval endpoints.
type iep struct {
	Point    int  // instruction index
	IsStart  bool // true = interval starts here, false = ends here
	VReg     VReg
	Interval int // index into intervalSet.Intervals for this VReg
}

// allocState holds mutable state during the allocation algorithm.
type allocState struct {
	count          []int     // count[P] = number of live VRegs at point P
	availInt       []int16   // available integer host registers
	availFP        []int16   // available FP host registers
	isFP           []bool    // per-VReg: is it FP?
	spilled        []bool    // per-VReg: spill(s) = true if totally spilled
	spillCost      []float64 // per-VReg: totalSpillCost(s)
	intervals      []intervalSet
	lastReg        []int16 // per-VReg: last assigned host register
	spillStack     []VReg  // stack for spill resurrection
	intervalAllocs []IntervalAlloc
	moves          []RegMove
}

// ── Primary API ──

// RegAllocator is the interface for pluggable register allocation strategies.
type RegAllocator interface {
	Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation
}

// Allocator implements Extended Linear Scan register allocation.
type Allocator struct {
}

func NewAllocator() *Allocator {
	return &Allocator{}
}

// Allocate performs Extended Linear Scan register allocation on the block.
func (s *Allocator) Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
	if len(b.Instrs) == 0 {
		return &Allocation{
			Kind:      []AllocKind{AllocUnused},
			SpillSlot: []int16{0},
		}
	}

	// Step 1: compute interval sets.
	intervals := computeIntervalSets(b)

	// Step 2: classify VRegs as int or FP.
	mv := b.maxVreg
	// Ensure mv covers all pinned VRegs (they may not appear in instructions).
	for vr := range pinned {
		if vr > mv {
			mv = vr
		}
	}
	// Extend intervals slice to cover pinned VRegs.
	if need := int(mv) + 1; len(intervals) < need {
		ext := make([]intervalSet, need)
		copy(ext, intervals)
		for i := len(intervals); i < need; i++ {
			ext[i] = intervalSet{VReg: VReg(i)}
		}
		intervals = ext
	}
	isFP := classifyVRegs(b, intervals)
	// Extend isFP to cover pinned VRegs.
	if need := int(mv) + 1; len(isFP) < need {
		ext := make([]bool, need)
		copy(ext, isFP)
		isFP = ext
	}

	// Step 3: compute separate int and FP counts, excluding pinned VRegs.
	// Pinned VRegs have pre-assigned host registers that are already removed
	// from the pool, so they don't compete for pool registers.
	pinnedSet := make(map[VReg]bool, len(pinned))
	for vr := range pinned {
		pinnedSet[vr] = true
	}

	n := len(b.Instrs)
	countInt := make([]int, n)
	countFP := make([]int, n)
	for _, is := range intervals {
		vr := is.VReg
		if vr == VRegZero || pinnedSet[vr] {
			continue
		}
		fp := int(vr) < len(isFP) && isFP[vr]
		for _, iv := range is.Intervals {
			for p := iv.Start; p <= iv.End && p < n; p++ {
				if fp {
					countFP[p]++
				} else {
					countInt[p]++
				}
			}
		}
	}

	kInt := len(pool.IntRegs)
	kFP := len(pool.FPRegs)

	// Build allocState.
	st := &allocState{
		availInt:  make([]int16, 0, kInt),
		availFP:   make([]int16, 0, kFP),
		isFP:      isFP,
		spilled:   make([]bool, int(mv)+1),
		intervals: intervals,
		lastReg:   make([]int16, int(mv)+1),
	}
	for i := range st.lastReg {
		st.lastReg[i] = -1
	}

	// Handle pinned VRegs: pre-assign them.
	for vr, hostReg := range pinned {
		if int(vr) < len(st.lastReg) {
			st.lastReg[vr] = hostReg
		}
	}

	// Step 4: compute spill costs.
	if freq == nil {
		freq = make([]float64, n)
		for i := range freq {
			freq[i] = 1.0
		}
	}
	st.spillCost = computeSpillCosts(b, intervals, freq)

	// Step 5: ELS_1 spill identification — handle int and FP separately.
	// Pinned VRegs are never spill candidates (they're excluded from counts
	// and marked in pinnedSet).
	spillIdentify(st, kInt, countInt, freq, isFP, false, pinnedSet)
	spillIdentify(st, kFP, countFP, freq, isFP, true, pinnedSet)

	// Step 6: spill resurrection — also per-class.
	spillResurrect(st, kInt, countInt, isFP, false)
	spillResurrect(st, kFP, countFP, isFP, true)

	// Step 7: ELS_0 register assignment for non-spilled VRegs.
	assignRegisters(st, pool, pinned)

	// Step 8: register move insertion.
	insertMoves(st, b)

	// Build output Allocation.
	alloc := &Allocation{
		Kind:        make([]AllocKind, int(mv)+1),
		SpillSlot:   make([]int16, int(mv)+1),
		IntervalMap: st.intervalAllocs,
		Moves:       st.moves,
	}

	nextSlot := int16(0)
	for i := range alloc.SpillSlot {
		alloc.SpillSlot[i] = -1
	}

	for vr := VReg(0); vr <= mv; vr++ {
		if vr == VRegZero {
			alloc.Kind[vr] = AllocUnused
			continue
		}
		if int(vr) < len(st.spilled) && st.spilled[vr] {
			alloc.Kind[vr] = AllocStack
			alloc.SpillSlot[vr] = nextSlot
			nextSlot++
			continue
		}
		// Check if this VReg has any interval allocations.
		hasAlloc := false
		for _, ia := range st.intervalAllocs {
			if ia.Interval.VReg == vr {
				hasAlloc = true
				break
			}
		}
		if hasAlloc {
			alloc.Kind[vr] = AllocReg
		} else {
			alloc.Kind[vr] = AllocUnused
		}
	}
	alloc.StackSlots = int(nextSlot)

	return alloc
}

// ── Liveness & Interval Computation ──

// instrDefs returns the VReg defined by this instruction, or VRegZero if none.
func instrDefs(ins *IRInstr) VReg {
	switch ins.Op {
	case IRStore, IRStoreX,
		IRLabel, IRBranch, IRBranchImm, IRJump,
		IRCall, IRRet, IRRetDyn, IRChainExit, IRSyscall,
		IRMarkLive, IRMarkDead, IRWriteback:
		return VRegZero
	default:
		return ins.Dst
	}
}

// instrUses returns all VRegs used (read) by this instruction.
// Filters out VRegZero.
func instrUses(ins *IRInstr) []VReg {
	var uses []VReg
	switch ins.Op {
	case IRConst:
		// No source VRegs — immediate only.
	case IRLabel, IRJump, IRWriteback, IRCall, IRChainExit, IRSyscall:
		// No VReg uses.
	case IRStoreX:
		// A = base, B = index, Dst = value (repurposed)
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
		if ins.B != VRegZero {
			uses = append(uses, ins.B)
		}
		if ins.Dst != VRegZero {
			uses = append(uses, ins.Dst)
		}
	case IRRet:
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
	case IRRetDyn:
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
		if ins.B != VRegZero {
			uses = append(uses, ins.B)
		}
	case IRMarkLive, IRMarkDead:
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
	case IRStore:
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
		if ins.B != VRegZero {
			uses = append(uses, ins.B)
		}
	case IRBranch:
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
		if ins.B != VRegZero {
			uses = append(uses, ins.B)
		}
	case IRBranchImm:
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
	case IRFma, IRFmsub, IRFnmadd, IRFnmsub:
		// Ternary FP ops: A, B, C are all uses.
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
		if ins.B != VRegZero {
			uses = append(uses, ins.B)
		}
		if ins.C != VRegZero {
			uses = append(uses, ins.C)
		}
	default:
		// Standard ops: A and B are uses.
		if ins.A != VRegZero {
			uses = append(uses, ins.A)
		}
		if ins.B != VRegZero {
			uses = append(uses, ins.B)
		}
	}
	return uses
}

// MaxVReg returns the highest VReg number referenced in any instruction of b.
// Sets b.maxVreg to this value too.
func MaxVReg(b *Block) VReg {
	var mx VReg
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		for _, vr := range []VReg{ins.Dst, ins.A, ins.B, ins.C} {
			if vr > mx {
				mx = vr
			}
		}
	}
	b.maxVreg = mx
	return mx
}

// computeIntervalSets computes the interval set I(s) for each VReg.
// Uses a backward scan to build precise intervals with holes.
func computeIntervalSets(b *Block) []intervalSet {
	if len(b.Instrs) == 0 {
		return nil
	}

	mv := b.maxVreg
	n := len(b.Instrs)

	// Per-VReg: track whether currently live during backward scan,
	// and the current interval's end point.
	type liveInfo struct {
		live   bool
		curEnd int
	}
	info := make([]liveInfo, int(mv)+1)
	result := make([]intervalSet, int(mv)+1)
	for i := range result {
		result[i].VReg = VReg(i)
	}

	// Backward scan.
	for i := n - 1; i >= 0; i-- {
		ins := &b.Instrs[i]

		// Process uses: if VReg not live, start a new interval ending here.
		for _, vr := range instrUses(ins) {
			if vr == VRegZero {
				continue
			}
			idx := int(vr)
			if idx >= len(info) {
				continue
			}
			if !info[idx].live {
				info[idx].live = true
				info[idx].curEnd = i
			}
		}

		// Process defs: if VReg is live, close the interval starting here.
		def := instrDefs(ins)
		if def != VRegZero && int(def) < len(info) {
			idx := int(def)
			if info[idx].live {
				// Close interval: [i, curEnd]
				result[idx].Intervals = append(result[idx].Intervals, Interval{
					VReg:  def,
					Start: i,
					End:   info[idx].curEnd,
				})
				info[idx].live = false
			} else {
				// Dead def: point interval [i, i]
				result[idx].Intervals = append(result[idx].Intervals, Interval{
					VReg:  def,
					Start: i,
					End:   i,
				})
			}
		}
	}

	// Any VReg still live at the top of the block (used before defined,
	// e.g. parameter VRegs) gets an interval starting at 0.
	for vr := 1; vr < len(info); vr++ {
		if info[vr].live {
			result[vr].Intervals = append(result[vr].Intervals, Interval{
				VReg:  VReg(vr),
				Start: 0,
				End:   info[vr].curEnd,
			})
		}
	}

	// Extend live ranges across backward branches (loops).
	// A linear backward scan doesn't know that a Jump/Branch at index S
	// transfers control to a label at index T < S. VRegs that are used
	// inside the loop body [T, S] must be live for the entire range, so
	// that the register allocator doesn't reuse their host registers in
	// the gap between the loop header and their first use in the body.
	extendLoopLiveRanges(b, result)
	extendForwardBranchRanges(b, result)

	// Sort intervals by Start for each VReg and merge adjacent/overlapping.
	for vr := range result {
		ivals := result[vr].Intervals
		if len(ivals) <= 1 {
			continue
		}
		sort.Sort(intervalsByStart(ivals))
		// Merge overlapping/adjacent.
		merged := ivals[:1]
		for _, iv := range ivals[1:] {
			last := &merged[len(merged)-1]
			if iv.Start <= last.End+1 {
				if iv.End > last.End {
					last.End = iv.End
				}
			} else {
				merged = append(merged, iv)
			}
		}
		result[vr].Intervals = merged
	}

	// Guest regs (1-63): extend last interval's End to len(Instrs)-1.
	// Guest registers persist across blocks and are written back at exits,
	// so the last interval must reach the end of the block.
	for vr := 1; vr <= 63 && vr < len(result); vr++ {
		ivals := result[vr].Intervals
		if len(ivals) > 0 {
			ivals[len(ivals)-1].End = n - 1
		}
	}

	return result
}

// extendLoopLiveRanges finds backward edges in the IR (Jump/Branch to an
// earlier label) and extends live ranges so that every VReg referenced in
// the loop body [target, source] has a contiguous interval covering the
// whole range. Without this, the linear backward scan leaves gaps where a
// VReg is "dead" between the loop header and its first use in the body,
// allowing the allocator to reuse its host register for another VReg.
func extendLoopLiveRanges(b *Block, result []intervalSet) {
	for i := range b.Instrs {
		ins := &b.Instrs[i]

		// Identify backward edges: jumps/branches to earlier labels.
		var targetLabel Label
		switch ins.Op {
		case IRJump:
			targetLabel = Label(ins.Imm)
		case IRBranch, IRBranchImm:
			targetLabel = Label(ins.Imm)
		default:
			continue
		}

		targetIdx, ok := b.Labels[targetLabel]
		if !ok || targetIdx >= i {
			continue // forward edge or unknown label
		}

		// Backward edge from i to targetIdx.
		// Collect all VRegs used or defined in [targetIdx, i].
		touched := make(map[VReg]bool)
		for j := targetIdx; j <= i; j++ {
			for _, vr := range instrUses(&b.Instrs[j]) {
				if vr != VRegZero {
					touched[vr] = true
				}
			}
			def := instrDefs(&b.Instrs[j])
			if def != VRegZero {
				touched[def] = true
			}
		}

		// For each touched VReg, add a synthetic interval [targetIdx, i].
		// The merge step that follows will unify it with existing intervals.
		for vr := range touched {
			idx := int(vr)
			if idx >= len(result) {
				continue
			}
			result[idx].Intervals = append(result[idx].Intervals, Interval{
				VReg:  vr,
				Start: targetIdx,
				End:   i,
			})
		}
	}
}

// extendForwardBranchRanges handles forward conditional branches.
// When a conditional branch at instruction i targets a label at instruction
// t > i, any VReg live at t must also be live at i — because the branch can
// transfer control from i to t. The linear backward scan doesn't follow
// forward edges, so we add synthetic intervals [i, t] for each VReg live
// at the target. The subsequent merge step unifies overlapping intervals.
//
// This is the forward-edge counterpart to extendLoopLiveRanges (backward edges).
// Together they ensure correct liveness for all intra-block control flow.
func extendForwardBranchRanges(b *Block, result []intervalSet) {
	for i := range b.Instrs {
		ins := &b.Instrs[i]

		// Only conditional branches create forward edges that matter.
		// Unconditional jumps (IRJump) make the fall-through unreachable,
		// so they don't require liveness extension.
		switch ins.Op {
		case IRBranch, IRBranchImm:
			// ok
		default:
			continue
		}

		targetLabel := Label(ins.Imm)
		targetIdx, ok := b.Labels[targetLabel]
		if !ok || targetIdx <= i {
			continue // backward or unknown — handled by extendLoopLiveRanges
		}

		// Forward conditional branch from i to targetIdx.
		// Any VReg live at targetIdx must also be live at i.
		for vr := range result {
			if result[vr].VReg == VRegZero {
				continue
			}
			for _, iv := range result[vr].Intervals {
				if iv.Start <= targetIdx && targetIdx <= iv.End {
					result[vr].Intervals = append(result[vr].Intervals, Interval{
						VReg:  result[vr].VReg,
						Start: i,
						End:   targetIdx,
					})
					break
				}
			}
		}
	}
}

// buildIEP collects all interval endpoints, sorted by program point.
// At the same point, ends are processed before starts (to free registers
// before allocating new ones).
func buildIEP(intervals []intervalSet) []iep {
	var endpoints []iep
	for _, is := range intervals {
		if is.VReg == VRegZero {
			continue
		}
		for idx, iv := range is.Intervals {
			endpoints = append(endpoints, iep{
				Point:    iv.Start,
				IsStart:  true,
				VReg:     is.VReg,
				Interval: idx,
			})
			endpoints = append(endpoints, iep{
				Point:    iv.End,
				IsStart:  false,
				VReg:     is.VReg,
				Interval: idx,
			})
		}
	}
	sort.Sort(iepByPoint(endpoints))
	return endpoints
}

// computeCount computes count[P] for each program point P.
func computeCount(intervals []intervalSet, numInstrs int) []int {
	count := make([]int, numInstrs)
	for _, is := range intervals {
		if is.VReg == VRegZero {
			continue
		}
		for _, iv := range is.Intervals {
			for p := iv.Start; p <= iv.End && p < numInstrs; p++ {
				count[p]++
			}
		}
	}
	return count
}

// ── VReg Classification ──

// classifyVRegs determines which VRegs are FP (true) vs integer (false).
func classifyVRegs(b *Block, intervals []intervalSet) []bool {
	mv := b.maxVreg
	isFP := make([]bool, int(mv)+1)

	// Guest FP regs (32-63) are trivially FP.
	for vr := 32; vr <= 63 && vr < len(isFP); vr++ {
		isFP[vr] = true
	}

	// For temps (>= VRegTempStart), classify by the defining instruction's type.
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		def := instrDefs(ins)
		if def >= VRegTempStart && int(def) < len(isFP) {
			if ins.T == F32 || ins.T == F64 {
				// Exception: FCvtToI/FCvtToU produce integers despite FP source.
				if ins.Op == IRFCvtToI || ins.Op == IRFCvtToU {
					continue
				}
				isFP[def] = true
			}
		}
	}

	return isFP
}

// BlockHasDivMul scans b.Instrs for division/multiplication ops that require
// special register handling on amd64 (RAX/RDX).
func BlockHasDivMul(b *Block) bool {
	for i := range b.Instrs {
		switch b.Instrs[i].Op {
		case IRDivS, IRDivU, IRRem, IRRemU, IRMulHS, IRMulHU, IRMulHSU:
			return true
		}
	}
	return false
}

// ── ELS_1: Spill Cost ──

// computeSpillCosts computes totalSpillCost(s) for each VReg.
func computeSpillCosts(b *Block, intervals []intervalSet, freq []float64) []float64 {
	mv := b.maxVreg
	costs := make([]float64, int(mv)+1)

	for i := range b.Instrs {
		ins := &b.Instrs[i]
		f := 1.0
		if i < len(freq) {
			f = freq[i]
		}

		// Reads.
		for _, vr := range instrUses(ins) {
			if vr != VRegZero && int(vr) < len(costs) {
				costs[vr] += f
			}
		}
		// Writes.
		def := instrDefs(ins)
		if def != VRegZero && int(def) < len(costs) {
			costs[def] += f
		}
	}
	return costs
}

// ── ELS_1: Spill Identification (paper Figure 6, step 1) ──

// spillIdentify performs ELS_1 step 1 for one register class.
// isFP and wantFP filter which VRegs this call considers.
// pinnedSet marks VRegs that must never be spilled.
func spillIdentify(st *allocState, k int, count []int, freq []float64, isFP []bool, wantFP bool, pinnedSet map[VReg]bool) {
	if k <= 0 {
		return
	}
	for {
		// Find program point with count[P] > k and largest freq[P].
		maxP := -1
		maxFreq := -1.0
		for p, c := range count {
			if c > k {
				f := 1.0
				if p < len(freq) {
					f = freq[p]
				}
				if maxP == -1 || f > maxFreq {
					maxP = p
					maxFreq = f
				}
			}
		}
		if maxP == -1 {
			break
		}

		iDegree := count[maxP] - 1
		if iDegree <= 0 {
			iDegree = 1
		}

		bestVReg := VReg(0)
		bestScore := -1.0
		for _, is := range st.intervals {
			vr := is.VReg
			if vr == VRegZero || pinnedSet[vr] {
				continue
			}
			if int(vr) >= len(st.spilled) || st.spilled[vr] {
				continue
			}
			// Filter by class.
			vrIsFP := int(vr) < len(isFP) && isFP[vr]
			if vrIsFP != wantFP {
				continue
			}
			for _, iv := range is.Intervals {
				if iv.Start <= maxP && maxP <= iv.End {
					score := st.spillCost[vr] / float64(iDegree)
					if bestVReg == VRegZero || score < bestScore {
						bestVReg = vr
						bestScore = score
					}
					break
				}
			}
		}

		if bestVReg == VRegZero {
			break
		}

		st.spilled[bestVReg] = true
		st.spillStack = append(st.spillStack, bestVReg)

		for _, is := range st.intervals {
			if is.VReg != bestVReg {
				continue
			}
			for _, iv := range is.Intervals {
				for p := iv.Start; p <= iv.End && p < len(count); p++ {
					count[p]--
				}
			}
		}
	}
}

// ── ELS_1: Spill Resurrection (paper Figure 6, step 2) ──

// spillResurrect performs ELS_1 step 2 for one register class.
func spillResurrect(st *allocState, k int, count []int, isFP []bool, wantFP bool) {
	if k <= 0 {
		return
	}
	// Process spill stack in LIFO order, but only for the matching class.
	remaining := st.spillStack[:0]
	for i := len(st.spillStack) - 1; i >= 0; i-- {
		vr := st.spillStack[i]
		vrIsFP := int(vr) < len(isFP) && isFP[vr]
		if vrIsFP != wantFP {
			remaining = append(remaining, vr)
			continue
		}

		canResurrect := true
		for _, is := range st.intervals {
			if is.VReg != vr {
				continue
			}
			for _, iv := range is.Intervals {
				for p := iv.Start; p <= iv.End && p < len(count); p++ {
					if count[p]+1 > k {
						canResurrect = false
						break
					}
				}
				if !canResurrect {
					break
				}
			}
		}

		if canResurrect {
			st.spilled[vr] = false
			for _, is := range st.intervals {
				if is.VReg != vr {
					continue
				}
				for _, iv := range is.Intervals {
					for p := iv.Start; p <= iv.End && p < len(count); p++ {
						count[p]++
					}
				}
			}
		}
		// If not resurrected, don't put back on stack (it stays spilled).
	}
	st.spillStack = remaining
}

// ── ELS_0: Register Assignment (paper Figure 4, steps 4-5) ──

func assignRegisters(st *allocState, pool RegPool, pinned map[VReg]int16) {
	// Initialize avail with all pool registers.
	st.availInt = append(st.availInt[:0], pool.IntRegs...)
	st.availFP = append(st.availFP[:0], pool.FPRegs...)

	// Remove pinned host regs from avail.
	for _, hostReg := range pinned {
		st.availInt = removeReg(st.availInt, hostReg)
		st.availFP = removeReg(st.availFP, hostReg)
	}

	// Pre-assign pinned VRegs.
	for vr, hostReg := range pinned {
		is := st.intervals[int(vr)]
		for _, iv := range is.Intervals {
			st.intervalAllocs = append(st.intervalAllocs, IntervalAlloc{
				Interval: iv,
				Host:     hostReg,
			})
		}
		if int(vr) < len(st.lastReg) {
			st.lastReg[vr] = hostReg
		}
	}

	// Build IEP for non-spilled, non-pinned VRegs.
	var filteredIntervals []intervalSet
	for _, is := range st.intervals {
		vr := is.VReg
		if vr == VRegZero {
			continue
		}
		if int(vr) < len(st.spilled) && st.spilled[vr] {
			continue
		}
		if _, isPinned := pinned[vr]; isPinned {
			continue
		}
		if len(is.Intervals) > 0 {
			filteredIntervals = append(filteredIntervals, is)
		}
	}

	endpoints := buildIEP(filteredIntervals)

	// Track which host reg is assigned to each interval.
	// Map: (VReg, interval index) -> host reg.
	type ivKey struct {
		VReg VReg
		Idx  int
	}
	assigned := make(map[ivKey]int16)

	for _, ep := range endpoints {
		vr := ep.VReg
		isFP := int(vr) < len(st.isFP) && st.isFP[vr]

		if !ep.IsStart {
			// End of interval: return register to avail.
			key := ivKey{vr, ep.Interval}
			if reg, ok := assigned[key]; ok {
				if isFP {
					st.availFP = append(st.availFP, reg)
				} else {
					st.availInt = append(st.availInt, reg)
				}
				delete(assigned, key)
			}
			continue
		}

		// Start of interval: assign register from avail.
		avail := &st.availInt
		if isFP {
			avail = &st.availFP
		}

		if len(*avail) == 0 {
			// No register available — spill this VReg (ELS₁ Figure 6, step 3b.ii).
			if int(vr) < len(st.spilled) {
				st.spilled[vr] = true
			}
			continue
		}

		// Heuristic: prefer the register previously assigned to this VReg.
		chosenIdx := 0
		if int(vr) < len(st.lastReg) && st.lastReg[vr] >= 0 {
			for j, r := range *avail {
				if r == st.lastReg[vr] {
					chosenIdx = j
					break
				}
			}
		}

		reg := (*avail)[chosenIdx]
		*avail = append((*avail)[:chosenIdx], (*avail)[chosenIdx+1:]...)

		key := ivKey{vr, ep.Interval}
		assigned[key] = reg
		if int(vr) < len(st.lastReg) {
			st.lastReg[vr] = reg
		}

		// Find the actual interval.
		var iv Interval
		if int(vr) < len(st.intervals) {
			ivals := st.intervals[int(vr)].Intervals
			if ep.Interval < len(ivals) {
				iv = ivals[ep.Interval]
			}
		}

		st.intervalAllocs = append(st.intervalAllocs, IntervalAlloc{
			Interval: iv,
			Host:     reg,
		})
	}
}

// ── ELS_0: Register Move Insertion (paper Figure 4, step 6) ──

func insertMoves(st *allocState, b *Block) {
	// For each control flow edge in the block (branches/jumps to labels),
	// check if any VReg has different register assignments across the edge.
	// Insert register moves as needed.

	// Build a map from interval to its assigned host register.
	type ivKey struct {
		VReg VReg
		Idx  int
	}
	regMap := make(map[ivKey]int16)
	for _, ia := range st.intervalAllocs {
		// Find the interval index.
		vr := ia.Interval.VReg
		if int(vr) < len(st.intervals) {
			for idx, iv := range st.intervals[int(vr)].Intervals {
				if iv.Start == ia.Interval.Start && iv.End == ia.Interval.End {
					regMap[ivKey{vr, idx}] = ia.Host
					break
				}
			}
		}
	}

	// For each VReg with multiple intervals, check if adjacent intervals
	// have different host registers. If so, insert a move.
	for _, is := range st.intervals {
		if len(is.Intervals) <= 1 {
			continue
		}
		vr := is.VReg
		if int(vr) < len(st.spilled) && st.spilled[vr] {
			continue
		}
		for i := 0; i+1 < len(is.Intervals); i++ {
			reg1, ok1 := regMap[ivKey{vr, i}]
			reg2, ok2 := regMap[ivKey{vr, i + 1}]
			if ok1 && ok2 && reg1 != reg2 {
				st.moves = append(st.moves, RegMove{
					InsertAt: is.Intervals[i+1].Start,
					From:     reg1,
					To:       reg2,
				})
			}
		}
	}

	// TODO: SCC detection for circular move dependencies.
	// For now, moves are emitted as simple register-to-register copies.
	// Circular cases (r1->r2, r2->r1) will be resolved with XOR swaps
	// when we encounter them in practice.
}

// removeReg removes the first occurrence of reg from the slice.
func removeReg(regs []int16, reg int16) []int16 {
	for i, r := range regs {
		if r == reg {
			return append(regs[:i], regs[i+1:]...)
		}
	}
	return regs
}

// findSCCs finds strongly connected components in a move dependency graph.
// Returns groups of move indices that form cycles.
func findSCCs(moves []RegMove) [][]int {
	// Build adjacency: move i depends on move j if move i reads the register
	// that move j writes to.
	n := len(moves)
	adj := make([][]int, n)
	for i := range moves {
		for j := range moves {
			if i != j && moves[i].From == moves[j].To {
				adj[i] = append(adj[i], j)
			}
		}
	}

	// Tarjan's SCC algorithm.
	index := 0
	stack := []int{}
	onStack := make([]bool, n)
	indices := make([]int, n)
	lowlinks := make([]int, n)
	defined := make([]bool, n)
	var sccs [][]int

	var strongconnect func(v int)
	strongconnect = func(v int) {
		indices[v] = index
		lowlinks[v] = index
		index++
		defined[v] = true
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if !defined[w] {
				strongconnect(w)
				if lowlinks[w] < lowlinks[v] {
					lowlinks[v] = lowlinks[w]
				}
			} else if onStack[w] {
				if indices[w] < lowlinks[v] {
					lowlinks[v] = indices[w]
				}
			}
		}

		if lowlinks[v] == indices[v] {
			var scc []int
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	for v := 0; v < n; v++ {
		if !defined[v] {
			strongconnect(v)
		}
	}

	return sccs
}
