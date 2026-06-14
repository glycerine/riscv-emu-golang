package riscv

import "sort"

// FixedStaticAllocator maps a fixed set of high-priority RISC-V registers
// to native host registers, spilling the rest to stack slots. No liveness
// analysis or interference graphs — just a hardcoded priority table.
//
// This mirrors what QEMU's TCG and other production binary translators do:
// piggyback on the source compiler's register allocation, mapping the most
// frequently used RISC-V registers to the limited x86-64 register file.
type FixedStaticAllocator struct{}

func NewFixedStaticAllocator() *FixedStaticAllocator {
	return &FixedStaticAllocator{}
}

type fixedLiveInterval struct {
	vr       VReg
	start    int
	end      int
	isFP     bool
	spillKey int
}

// intPriority is the RISC-V integer register priority order.
// rv8-faithful: the first 12 entries match the rv8 static register mapping
// (ra, sp, t0, t1, a0-a7). Remaining registers spill to [RBP+r*8].
var intPriority = []VReg{
	1, 2, 5, 6, // ra, sp, t0, t1
	10, 11, 12, 13, 14, 15, 16, 17, // a0-a7
	8, 9, // s0/fp, s1
	7, 28, 29, 30, 31, // t2-t6
	3, 4, // gp, tp
	18, 19, 20, 21, 22, 23, 24, 25, 26, 27, // s2-s11
}

// fpPriority is the RISC-V FP register priority order.
// fa0-fa7 first (argument/return), then ft temporaries, then fs callee-saved.
var fpPriority = []VReg{
	32 + 10, 32 + 11, 32 + 12, 32 + 13, 32 + 14, 32 + 15, 32 + 16, 32 + 17, // fa0-fa7
	32 + 0, 32 + 1, 32 + 2, 32 + 3, 32 + 4, 32 + 5, 32 + 6, 32 + 7, // ft0-ft7
	32 + 28, 32 + 29, 32 + 30, 32 + 31, // ft8-ft11
	32 + 8, 32 + 9, // fs0, fs1
	32 + 18, 32 + 19, 32 + 20, 32 + 21, 32 + 22, 32 + 23, 32 + 24, 32 + 25, 32 + 26, 32 + 27, // fs2-fs11
}

// Allocate produces a register assignment using fixed static mapping.
func (f *FixedStaticAllocator) Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {

	//vv("FixedStaticAllocator.Allocate() top. b.maxVreg=%d numInstrs=%d VRegTempStart=%d", b.maxVreg, len(b.Instrs), VRegTempStart)
	//defer vv("FixedStaticAllocator.Allocate() done.")

	if len(b.Instrs) == 0 {
		return &Allocation{
			Kind:      []AllocKind{AllocUnused},
			SpillSlot: []int16{0},
		}
	}

	n := len(b.Instrs)
	mv := b.maxVreg
	for vr := range pinned {
		if vr > mv {
			mv = vr
		}
	}
	numVRegs := int(mv) + 1
	//vv("Allocate: mv=%d numVRegs=%d VRegTempStart=%d loopIters=%d", mv, numVRegs, VRegTempStart, numVRegs-int(VRegTempStart))

	// Discover which VRegs are actually used in the block.
	used := make([]bool, numVRegs)
	isFP := make([]bool, numVRegs)
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		ins.forEachVReg(func(vr VReg) {
			if int(vr) < numVRegs {
				used[vr] = true
			}
		})
	}
	// Classify FP VRegs.
	for vr := VReg(32); vr < 64 && int(vr) < numVRegs; vr++ {
		isFP[vr] = true
	}
	// Also classify by instruction type usage.
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		producesFP := ins.T == F32 || ins.T == F64
		switch ins.Op {
		case IRFCmp, IRFCvtToI, IRFCvtToU:
			producesFP = false
		}
		if producesFP {
			if ins.Dst != VRegZero && int(ins.Dst) < numVRegs {
				isFP[ins.Dst] = true
			}
		}
	}

	// Mark pinned VRegs as used.
	for vr := range pinned {
		if int(vr) < numVRegs {
			used[vr] = true
		}
	}

	// Build the allocation.
	kind := make([]AllocKind, numVRegs)
	spillSlot := make([]int16, numVRegs)
	var intervalAllocs []IntervalAlloc

	// 1. Handle pinned VRegs first — they get their fixed host registers.
	for vr, host := range pinned {
		vi := int(vr)
		if vi >= numVRegs {
			continue
		}
		kind[vi] = AllocReg
		intervalAllocs = append(intervalAllocs, IntervalAlloc{
			Interval: Interval{VReg: vr, Start: 0, End: n - 1},
			Host:     host,
		})
	}

	// 2. Assign integer VRegs by priority.
	intPoolIdx := 0
	for _, vr := range intPriority {
		if int(vr) >= numVRegs || !used[vr] || isFP[vr] {
			continue
		}
		if _, isPinned := pinned[vr]; isPinned {
			continue
		}
		if intPoolIdx >= len(pool.IntRegs) {
			break
		}
		kind[vr] = AllocReg
		intervalAllocs = append(intervalAllocs, IntervalAlloc{
			Interval: Interval{VReg: vr, Start: 0, End: n - 1},
			Host:     pool.IntRegs[intPoolIdx],
		})
		intPoolIdx++
	}

	// 3. Assign guest FP VRegs by priority unless the backend wants f0..f31
	// to remain memory-authoritative. FP temporaries are still eligible below.
	fpPoolIdx := 0
	if !pool.NoArchFP {
		for _, vr := range fpPriority {
			if int(vr) >= numVRegs || !used[vr] || !isFP[vr] {
				continue
			}
			if _, isPinned := pinned[vr]; isPinned {
				continue
			}
			if fpPoolIdx >= len(pool.FPRegs) {
				break
			}
			kind[vr] = AllocReg
			intervalAllocs = append(intervalAllocs, IntervalAlloc{
				Interval: Interval{VReg: vr, Start: 0, End: n - 1},
				Host:     pool.FPRegs[fpPoolIdx],
			})
			fpPoolIdx++
		}
	}

	// 4. Handle temps (VReg >= VRegTempStart) that aren't pinned.
	//    These are JIT-internal temporaries. Unlike guest registers, temps
	//    use their actual first/last reference, so non-overlapping temps can
	//    reuse the same host register inside one block.
	stackSlots := int16(0)
	if pool.TempIntervals {
		var intTemps, fpTemps []fixedLiveInterval
		first, last := fixedFirstLast(b, numVRegs)
		for vr := VRegTempStart; int(vr) < numVRegs; vr++ {
			vi := int(vr)
			if !used[vi] {
				continue
			}
			if _, isPinned := pinned[vr]; isPinned {
				continue
			}
			iv := fixedLiveInterval{
				vr:    vr,
				start: first[vi],
				end:   last[vi],
				isFP:  isFP[vi],
			}
			if iv.start < 0 {
				iv.start = 0
				iv.end = n - 1
			}
			if iv.isFP {
				fpTemps = append(fpTemps, iv)
			} else {
				intTemps = append(intTemps, iv)
			}
		}
		intRegsForTemps := append([]int16(nil), pool.IntRegs[intPoolIdx:]...)
		fpRegsForTemps := append([]int16(nil), pool.FPRegs[fpPoolIdx:]...)
		stackSlots = fixedAssignIntervals(intTemps, intRegsForTemps, kind, spillSlot, &intervalAllocs, stackSlots)
		stackSlots = fixedAssignIntervals(fpTemps, fpRegsForTemps, kind, spillSlot, &intervalAllocs, stackSlots)
	} else {
		for vr := VRegTempStart; int(vr) < numVRegs; vr++ {
			if !used[vr] {
				continue
			}
			if _, isPinned := pinned[vr]; isPinned {
				continue
			}
			if isFP[vr] {
				if fpPoolIdx < len(pool.FPRegs) {
					kind[vr] = AllocReg
					intervalAllocs = append(intervalAllocs, IntervalAlloc{
						Interval: Interval{VReg: vr, Start: 0, End: n - 1},
						Host:     pool.FPRegs[fpPoolIdx],
					})
					fpPoolIdx++
					continue
				}
			} else {
				if intPoolIdx < len(pool.IntRegs) {
					kind[vr] = AllocReg
					intervalAllocs = append(intervalAllocs, IntervalAlloc{
						Interval: Interval{VReg: vr, Start: 0, End: n - 1},
						Host:     pool.IntRegs[intPoolIdx],
					})
					intPoolIdx++
					continue
				}
			}
			kind[vr] = AllocStack
			spillSlot[vr] = stackSlots
			stackSlots++
		}
	}
	//vv("finsihed, on to 5.")

	// 5. Spill all remaining used VRegs that weren't assigned.
	for vr := 1; vr < numVRegs; vr++ {
		if !used[vr] || kind[vr] != AllocUnused {
			continue
		}
		if _, isPinned := pinned[VReg(vr)]; isPinned {
			continue
		}
		kind[vr] = AllocStack
		spillSlot[vr] = stackSlots
		stackSlots++
	}

	return &Allocation{
		Kind:        kind,
		SpillSlot:   spillSlot,
		IntervalMap: intervalAllocs,
		StackSlots:  int(stackSlots),
	}
}

func fixedFirstLast(b *Block, numVRegs int) ([]int, []int) {
	first := make([]int, numVRegs)
	last := make([]int, numVRegs)
	for i := range first {
		first[i] = -1
		last[i] = -1
	}
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		ins.forEachVReg(func(vr VReg) {
			if int(vr) >= numVRegs {
				return
			}
			vi := int(vr)
			if first[vi] < 0 {
				first[vi] = i
			}
			last[vi] = i
		})
	}
	return first, last
}

func fixedAssignIntervals(
	intervals []fixedLiveInterval,
	regs []int16,
	kind []AllocKind,
	spillSlot []int16,
	intervalAllocs *[]IntervalAlloc,
	stackSlots int16,
) int16 {
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start != intervals[j].start {
			return intervals[i].start < intervals[j].start
		}
		if intervals[i].end != intervals[j].end {
			return intervals[i].end < intervals[j].end
		}
		return intervals[i].vr < intervals[j].vr
	})
	for _, iv := range intervals {
		host, ok := fixedPickHostForInterval(iv, regs, *intervalAllocs)
		vi := int(iv.vr)
		if ok {
			kind[vi] = AllocReg
			*intervalAllocs = append(*intervalAllocs, IntervalAlloc{
				Interval: Interval{VReg: iv.vr, Start: iv.start, End: iv.end},
				Host:     host,
			})
			continue
		}
		kind[vi] = AllocStack
		spillSlot[vi] = stackSlots
		stackSlots++
	}
	return stackSlots
}

func fixedPickHostForInterval(iv fixedLiveInterval, regs []int16, assigned []IntervalAlloc) (int16, bool) {
	for _, candidate := range regs {
		conflict := false
		for i := range assigned {
			a := assigned[i]
			if a.Host != candidate {
				continue
			}
			if fixedIntervalsOverlap(iv.start, iv.end, a.Interval.Start, a.Interval.End) {
				conflict = true
				break
			}
		}
		if !conflict {
			return candidate, true
		}
	}
	return 0, false
}

func fixedIntervalsOverlap(aStart, aEnd, bStart, bEnd int) bool {
	return aStart <= bEnd && bStart <= aEnd
}
