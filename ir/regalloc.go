package ir

import "sort"

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
	count     []int     // count[P] = number of live VRegs at point P
	availInt  []int16   // available integer host registers
	availFP   []int16   // available FP host registers
	isFP      []bool    // per-VReg: is it FP?
	spilled   []bool    // per-VReg: spill(s) = true if totally spilled
	spillCost []float64 // per-VReg: totalSpillCost(s)
	intervals []intervalSet
	lastReg   []int16  // per-VReg: last assigned host register
	spillStack []VReg  // stack for spill resurrection
	intervalAllocs []IntervalAlloc
	moves     []RegMove
}

// ── Primary API ──

// Allocate performs Extended Linear Scan register allocation on the block.
func Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
	if len(b.Instrs) == 0 {
		return &Allocation{
			Kind:      []AllocKind{AllocUnused},
			SpillSlot: []int16{0},
		}
	}

	// Step 1: compute interval sets.
	intervals := computeIntervalSets(b)

	// Step 2: classify VRegs as int or FP.
	mv := maxVReg(b)
	isFP := classifyVRegs(b, intervals)

	// Step 3: compute count[P] at each program point.
	count := computeCount(intervals, len(b.Instrs))

	// Determine k (number of registers) per class.
	kInt := len(pool.IntRegs)
	kFP := len(pool.FPRegs)

	// Build allocState.
	st := &allocState{
		count:     count,
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
		freq = make([]float64, len(b.Instrs))
		for i := range freq {
			freq[i] = 1.0
		}
	}
	st.spillCost = computeSpillCosts(b, intervals, freq)

	// Step 5: ELS_1 spill identification — spill until count[P] <= k everywhere.
	// We handle int and FP separately, using their respective k values.
	// For now, use the combined maximum. This will be refined.
	k := kInt
	if kFP > k {
		k = kFP
	}
	spillIdentify(st, k, count, freq)

	// Step 6: spill resurrection.
	spillResurrect(st, k, count)

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
		IRCall, IRRet,
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
	case IRLabel, IRJump, IRWriteback, IRCall:
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

// maxVReg returns the highest VReg number referenced in any instruction of b.
func maxVReg(b *Block) VReg {
	var mx VReg
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		for _, vr := range []VReg{ins.Dst, ins.A, ins.B} {
			if vr > mx {
				mx = vr
			}
		}
	}
	return mx
}

// computeIntervalSets computes the interval set I(s) for each VReg.
// Uses a backward scan to build precise intervals with holes.
func computeIntervalSets(b *Block) []intervalSet {
	if len(b.Instrs) == 0 {
		return nil
	}

	mv := maxVReg(b)
	n := len(b.Instrs)

	// Per-VReg: track whether currently live during backward scan,
	// and the current interval's end point.
	type liveInfo struct {
		live     bool
		curEnd   int
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

	// Sort intervals by Start for each VReg and merge adjacent/overlapping.
	for vr := range result {
		ivals := result[vr].Intervals
		if len(ivals) <= 1 {
			continue
		}
		sort.Slice(ivals, func(a, b int) bool {
			return ivals[a].Start < ivals[b].Start
		})
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
	for vr := 1; vr <= 63 && vr < len(result); vr++ {
		ivals := result[vr].Intervals
		if len(ivals) > 0 {
			ivals[len(ivals)-1].End = n - 1
		}
	}

	return result
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
	sort.Slice(endpoints, func(a, b int) bool {
		if endpoints[a].Point != endpoints[b].Point {
			return endpoints[a].Point < endpoints[b].Point
		}
		// At same point, same VReg/interval (point interval): start before end.
		if endpoints[a].VReg == endpoints[b].VReg && endpoints[a].Interval == endpoints[b].Interval {
			return endpoints[a].IsStart && !endpoints[b].IsStart
		}
		// At same point, different intervals: ends before starts (free regs first).
		if endpoints[a].IsStart != endpoints[b].IsStart {
			return !endpoints[a].IsStart
		}
		return endpoints[a].VReg < endpoints[b].VReg
	})
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
	mv := maxVReg(b)
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
		case IRDivS, IRDivU, IRRem, IRMulHS, IRMulHU, IRMulHSU:
			return true
		}
	}
	return false
}

// ── ELS_1: Spill Cost ──

// computeSpillCosts computes totalSpillCost(s) for each VReg.
func computeSpillCosts(b *Block, intervals []intervalSet, freq []float64) []float64 {
	mv := maxVReg(b)
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

func spillIdentify(st *allocState, k int, count []int, freq []float64) {
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
			break // No pressure point exceeds k.
		}

		// Find the live VReg at P with smallest totalSpillCost(s)/iDegree(s,P).
		iDegree := count[maxP] - 1
		if iDegree <= 0 {
			iDegree = 1
		}

		bestVReg := VReg(0)
		bestScore := -1.0
		for _, is := range st.intervals {
			vr := is.VReg
			if vr == VRegZero || int(vr) >= len(st.spilled) || st.spilled[vr] {
				continue
			}
			// Check if vr is live at maxP.
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
			break // No candidates to spill.
		}

		// Spill bestVReg.
		st.spilled[bestVReg] = true
		st.spillStack = append(st.spillStack, bestVReg)

		// Decrement count at all points where bestVReg is live.
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

func spillResurrect(st *allocState, k int, count []int) {
	for len(st.spillStack) > 0 {
		// Pop from stack (LIFO).
		vr := st.spillStack[len(st.spillStack)-1]
		st.spillStack = st.spillStack[:len(st.spillStack)-1]

		// Check if resurrecting vr would cause count[Q] > k at any point.
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
			// Increment count at all live points.
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
	}
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
			// No register available — this shouldn't happen after spill phase.
			// Defensive: skip this interval.
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
