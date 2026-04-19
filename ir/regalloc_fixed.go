package ir

// FixedStaticAllocator maps a fixed set of high-priority RISC-V registers
// to native host registers, spilling the rest to stack slots. No liveness
// analysis or interference graphs — just a hardcoded priority table.
//
// Zero heap allocations per block: guest-reg mappings are pre-computed once,
// temps are assigned from remaining pool slots via a counter, and the output
// is a FixedAllocation with fixed-size arrays reused across blocks.

// MaxFixedTemps is the maximum number of JIT temporaries supported by
// FixedAllocation. Typical blocks use 10-40; the largest observed ~200.
const MaxFixedTemps = 256

// FixedAllocation is the zero-allocation output of FixedStaticAllocator.
// All fields are fixed-size arrays — no heap allocation required.
// Pre-computed by AllocateFixed so the lowerer never computes anything.
type FixedAllocation struct {
	// Guest registers: index 0..63.
	GuestHost  [64]int16 // host register or -1 (spilled)
	GuestSpill [64]int16 // spill slot (valid when GuestHost == -1)
	GuestIsFP  [64]bool  // true for VRegs 32..63

	// Temps: index by (VReg - VRegTempStart), up to MaxFixedTemps.
	TempHost  [MaxFixedTemps]int16 // host register or -1 (spilled)
	TempSpill [MaxFixedTemps]int16 // spill slot (valid when TempHost == -1)
	TempIsFP  [MaxFixedTemps]bool  // classified by defining instruction

	// Pinned VRegs (parameters like xBase, fBase, etc.)
	PinnedHost [MaxFixedTemps]int16 // host register for pinned temps (-1 = not pinned)

	TempCount  int // number of temps in this block (max VReg - VRegTempStart + 1)
	StackSlots int // total 8-byte spill slots needed

	// Pre-computed for lowerer — CX liveness and host register liveness.
	// In fixed mapping, all assignments span [0, n-1], so these are
	// block-level constants, not per-instruction.
	CXAssigned bool // true if any VReg is assigned to CX

	// Host registers that are assigned to some VReg (live entire block).
	// Stored as a compact list for isHostLive linear scan.
	AssignedHosts    [48]int16 // max: 7 int + 14 FP + 5 pinned + ~20 temps
	NumAssignedHosts int
}

// HostReg returns the host register for VReg v, or -1 if spilled/unused.
func (fa *FixedAllocation) HostReg(v VReg) int16 {
	if v == VRegZero {
		return -1
	}
	if v < 64 {
		return fa.GuestHost[v]
	}
	ti := int(v) - int(VRegTempStart)
	if ti >= 0 && ti < fa.TempCount {
		if fa.PinnedHost[ti] != -1 {
			return fa.PinnedHost[ti]
		}
		return fa.TempHost[ti]
	}
	return -1
}

// AllocKind returns the allocation kind for VReg v.
func (fa *FixedAllocation) AllocKind(v VReg) AllocKind {
	if v == VRegZero {
		return AllocUnused
	}
	if v < 64 {
		if fa.GuestHost[v] >= 0 {
			return AllocReg
		}
		if fa.GuestSpill[v] >= 0 {
			return AllocStack
		}
		return AllocUnused
	}
	ti := int(v) - int(VRegTempStart)
	if ti >= 0 && ti < fa.TempCount {
		if fa.PinnedHost[ti] != -1 {
			return AllocReg
		}
		if fa.TempHost[ti] >= 0 {
			return AllocReg
		}
		return AllocStack
	}
	return AllocUnused
}

// SpillSlotOf returns the spill slot for VReg v.
func (fa *FixedAllocation) SpillSlotOf(v VReg) int16 {
	if v < 64 {
		return fa.GuestSpill[v]
	}
	ti := int(v) - int(VRegTempStart)
	if ti >= 0 && ti < fa.TempCount {
		return fa.TempSpill[ti]
	}
	return -1
}

// IsFP returns true if VReg v is floating-point.
func (fa *FixedAllocation) IsFP(v VReg) bool {
	if v < 64 {
		return fa.GuestIsFP[v]
	}
	ti := int(v) - int(VRegTempStart)
	if ti >= 0 && ti < MaxFixedTemps {
		return fa.TempIsFP[ti]
	}
	return false
}

// IsHostLive returns true if a host register is assigned to any VReg.
// In fixed mapping all assignments span the entire block.
func (fa *FixedAllocation) IsHostLive(hostReg int16) bool {
	for i := 0; i < fa.NumAssignedHosts; i++ {
		if fa.AssignedHosts[i] == hostReg {
			return true
		}
	}
	return false
}

// addAssignedHost records a host register as assigned (for IsHostLive).
func (fa *FixedAllocation) addAssignedHost(h int16) {
	if fa.NumAssignedHosts < len(fa.AssignedHosts) {
		fa.AssignedHosts[fa.NumAssignedHosts] = h
		fa.NumAssignedHosts++
	}
}

// intPriority is the RISC-V integer register priority order.
// ABI-driven: argument/return registers first, then ABI-critical, then temps.
var intPriority = []VReg{
	10, 11, 12, 13, 14, 15, // a0-a5: argument/return
	2, 1, 8, 9, // sp, ra, s0/fp, s1
	5, 6, 7, 28, // t0, t1, t2, t3
	16, 17, 18, 19, 20, 21, 22, 23, // s2-s9
	29, 30, 31, // t4, t5, t6
	3, 4, 24, 25, 26, 27, // gp, tp, s8-s11
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

// cachedGuestMapping holds a pre-computed guest register → host register
// mapping for one pool variant. Two are pre-computed: normal and divmul.
type cachedGuestMapping struct {
	guestHost         [64]int16
	guestSpill        [64]int16
	intPoolStart      int   // index into pool.IntRegs where temp assignment starts
	fpPoolStart       int   // index into pool.FPRegs where temp assignment starts
	guestSpillCount   int   // number of guest spill slots
	cxAssigned        bool  // CX assigned to any guest reg
	assignedHosts     [48]int16
	numAssignedHosts  int
}

// FixedStaticAllocator pre-computes guest register mappings for both pool
// variants (normal and divmul) and reuses a scratch FixedAllocation across
// blocks for zero heap allocation per block.
type FixedStaticAllocator struct {
	normal cachedGuestMapping // pool without div/mul (7 int regs)
	divmul cachedGuestMapping // pool with div/mul (5 int regs, no AX/DX)

	// FP classification is the same for both variants.
	guestIsFP [64]bool

	// CX register constant (set by InitWithCX).
	cxReg int16

	// Pool sizes for cache selection.
	normalIntCount int

	// Reusable output — returned by AllocateFixed, overwritten each call.
	scratch FixedAllocation

	initialized bool
}

func NewFixedStaticAllocator() *FixedStaticAllocator {
	return &FixedStaticAllocator{cxReg: -1}
}

// Initialized returns true if InitFixed has been called.
func (f *FixedStaticAllocator) Initialized() bool {
	return f.initialized
}

// InitFixed configures the allocator with both pool variants and the CX
// register constant. Must be called once before AllocateFixed.
// normalPool is the pool for blocks without div/mul;
// divmulPool is the pool for blocks with div/mul (AX/DX removed).
func (f *FixedStaticAllocator) InitFixed(cxReg int16, normalPool, divmulPool RegPool, pinned map[VReg]int16) {
	f.cxReg = cxReg
	f.initBoth(normalPool, divmulPool, pinned)
}

// initBoth pre-computes guest mappings for both pool variants.
// Called lazily on first AllocateFixed. Needs the normal pool + pinned;
// derives the divmul pool by removing AX and DX from IntRegs.
func (f *FixedStaticAllocator) initBoth(normalPool RegPool, divmulPool RegPool, pinned map[VReg]int16) {
	if f.initialized {
		return
	}
	f.initialized = true
	f.normalIntCount = len(normalPool.IntRegs)

	// FP classification (shared).
	for i := 32; i < 64; i++ {
		f.guestIsFP[i] = true
	}

	f.buildCache(&f.normal, normalPool, pinned)
	f.buildCache(&f.divmul, divmulPool, pinned)
}

// buildCache fills one cachedGuestMapping from a pool + pinned set.
func (f *FixedStaticAllocator) buildCache(c *cachedGuestMapping, pool RegPool, pinned map[VReg]int16) {
	for i := range c.guestHost {
		c.guestHost[i] = -1
		c.guestSpill[i] = -1
	}
	c.cxAssigned = false
	c.numAssignedHosts = 0

	// Pinned guest regs.
	for vr, host := range pinned {
		if vr < 64 {
			c.guestHost[vr] = host
			if c.numAssignedHosts < len(c.assignedHosts) {
				c.assignedHosts[c.numAssignedHosts] = host
				c.numAssignedHosts++
			}
			if host == f.cxReg {
				c.cxAssigned = true
			}
		}
	}

	// Integer guest regs by priority.
	intIdx := 0
	for _, vr := range intPriority {
		if _, isPinned := pinned[vr]; isPinned {
			continue
		}
		if intIdx >= len(pool.IntRegs) {
			break
		}
		h := pool.IntRegs[intIdx]
		c.guestHost[vr] = h
		if c.numAssignedHosts < len(c.assignedHosts) {
			c.assignedHosts[c.numAssignedHosts] = h
			c.numAssignedHosts++
		}
		if h == f.cxReg {
			c.cxAssigned = true
		}
		intIdx++
	}
	c.intPoolStart = intIdx

	// FP guest regs by priority.
	fpIdx := 0
	for _, vr := range fpPriority {
		if _, isPinned := pinned[vr]; isPinned {
			continue
		}
		if fpIdx >= len(pool.FPRegs) {
			break
		}
		h := pool.FPRegs[fpIdx]
		c.guestHost[vr] = h
		if c.numAssignedHosts < len(c.assignedHosts) {
			c.assignedHosts[c.numAssignedHosts] = h
			c.numAssignedHosts++
		}
		fpIdx++
	}
	c.fpPoolStart = fpIdx

	// Spill slots for unmapped guest regs.
	slot := int16(0)
	for vr := VReg(1); vr < 64; vr++ {
		if c.guestHost[vr] == -1 {
			if _, isPinned := pinned[vr]; !isPinned {
				c.guestSpill[vr] = slot
				slot++
			}
		}
	}
	c.guestSpillCount = int(slot)
}

// AllocateFixed produces a FixedAllocation with zero heap allocations.
// The returned pointer is reused across calls — caller must consume
// the result before calling AllocateFixed again.
func (f *FixedStaticAllocator) AllocateFixed(b *Block, pool RegPool, pinned map[VReg]int16) *FixedAllocation {
	// Pick the right cached mapping based on pool size.
	c := &f.normal
	if len(pool.IntRegs) != f.normalIntCount {
		c = &f.divmul
	}

	fa := &f.scratch

	// Copy cached guest mapping.
	fa.GuestHost = c.guestHost
	fa.GuestSpill = c.guestSpill
	fa.GuestIsFP = f.guestIsFP
	fa.StackSlots = c.guestSpillCount
	fa.CXAssigned = c.cxAssigned
	fa.NumAssignedHosts = c.numAssignedHosts
	copy(fa.AssignedHosts[:c.numAssignedHosts], c.assignedHosts[:c.numAssignedHosts])

	// Clear temp arrays.
	fa.TempCount = 0
	for i := range fa.PinnedHost {
		fa.PinnedHost[i] = -1
	}

	// Find max temp VReg used in the block.
	maxTemp := VReg(0)
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		for _, vr := range [3]VReg{ins.Dst, ins.A, ins.B} {
			if vr >= VRegTempStart && vr > maxTemp {
				maxTemp = vr
			}
		}
	}

	// Handle pinned VRegs that are temps.
	for vr, host := range pinned {
		if vr >= VRegTempStart {
			ti := int(vr) - int(VRegTempStart)
			if ti < MaxFixedTemps {
				fa.PinnedHost[ti] = host
				if ti+1 > fa.TempCount {
					fa.TempCount = ti + 1
				}
				fa.addAssignedHost(host)
				if host == f.cxReg {
					fa.CXAssigned = true
				}
			}
		}
	}

	if maxTemp < VRegTempStart {
		return fa
	}

	tempCount := int(maxTemp) - int(VRegTempStart) + 1
	if tempCount > MaxFixedTemps {
		tempCount = MaxFixedTemps
	}
	if tempCount > fa.TempCount {
		fa.TempCount = tempCount
	}

	// Classify temp FP-ness from defining instruction type.
	for i := 0; i < fa.TempCount; i++ {
		fa.TempIsFP[i] = false
	}
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		if ins.Dst >= VRegTempStart {
			ti := int(ins.Dst) - int(VRegTempStart)
			if ti < fa.TempCount {
				switch ins.T {
				case F32, F64:
					fa.TempIsFP[ti] = true
				}
			}
		}
	}

	// Assign temps from remaining pool registers.
	intIdx := c.intPoolStart
	fpIdx := c.fpPoolStart
	stackSlots := int16(c.guestSpillCount)

	for ti := 0; ti < fa.TempCount; ti++ {
		if fa.PinnedHost[ti] != -1 {
			fa.TempHost[ti] = -1
			fa.TempSpill[ti] = -1
			continue
		}
		if fa.TempIsFP[ti] {
			if fpIdx < len(pool.FPRegs) {
				h := pool.FPRegs[fpIdx]
				fa.TempHost[ti] = h
				fa.TempSpill[ti] = -1
				fa.addAssignedHost(h)
				fpIdx++
				continue
			}
		} else {
			if intIdx < len(pool.IntRegs) {
				h := pool.IntRegs[intIdx]
				fa.TempHost[ti] = h
				fa.TempSpill[ti] = -1
				fa.addAssignedHost(h)
				if h == f.cxReg {
					fa.CXAssigned = true
				}
				intIdx++
				continue
			}
		}
		// Spill.
		fa.TempHost[ti] = -1
		fa.TempSpill[ti] = stackSlots
		stackSlots++
	}

	fa.StackSlots = int(stackSlots)
	return fa
}

// Allocate satisfies the RegAllocator interface for compatibility with ELS.
// It wraps AllocateFixed into the Allocation struct the ELS lowerer path accepts.
func (f *FixedStaticAllocator) Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation {
	fa := f.AllocateFixed(b, pool, pinned)

	// Convert to Allocation for the ELS-compatible lowerer path.
	n := len(b.Instrs)
	mv := maxVReg(b)
	for vr := range pinned {
		if vr > mv {
			mv = vr
		}
	}
	numVRegs := int(mv) + 1

	kind := make([]AllocKind, numVRegs)
	spillSlot := make([]int16, numVRegs)
	var intervalAllocs []IntervalAlloc

	for vr := VReg(0); vr < VReg(numVRegs); vr++ {
		k := fa.AllocKind(vr)
		kind[vr] = k
		if k == AllocStack {
			spillSlot[vr] = fa.SpillSlotOf(vr)
		}
		if k == AllocReg {
			intervalAllocs = append(intervalAllocs, IntervalAlloc{
				Interval: Interval{VReg: vr, Start: 0, End: n - 1},
				Host:     fa.HostReg(vr),
			})
		}
	}

	return &Allocation{
		Kind:        kind,
		SpillSlot:   spillSlot,
		IntervalMap: intervalAllocs,
		StackSlots:  fa.StackSlots,
	}
}
