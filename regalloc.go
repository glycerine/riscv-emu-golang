package riscv

// ── Interval representation ──

// Interval is one contiguous live range [Start, End] for a symbolic register.
// A symbolic register may have multiple disjoint intervals (the "interval set").
type Interval struct {
	VReg  VReg
	Start int // instruction index
	End   int // instruction index
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
type IntervalAlloc struct {
	Interval Interval
	Host     int16 // physical register assigned for this interval
}

// Allocation is the output of the register allocator.
type Allocation struct {
	// Per-VReg summary: Kind indicates overall disposition.
	Kind      []AllocKind // indexed by VReg
	SpillSlot []int16     // indexed by VReg; valid when Kind == AllocStack

	// Per-interval register assignment.
	IntervalMap []IntervalAlloc

	// Moves to insert at control flow edges.
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
	IntRegs       []int16 // host register IDs for integer VRegs
	FPRegs        []int16 // host register IDs for FP VRegs
	NoArchFP      bool    // leave guest f0..f31 memory-backed; FP temps may still use FPRegs
	TempIntervals bool    // allocate temps by first/last use instead of whole block
}

// ── Allocator interface ──

// RegAllocator is the interface for pluggable register allocation strategies.
type RegAllocator interface {
	Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation
}

// ── Shared helpers ──

// MaxVReg returns the highest VReg number referenced in any instruction of b.
// Sets b.maxVreg to this value too.
func MaxVReg(b *Block) VReg {
	var mx VReg
	for i := range b.Instrs {
		ins := &b.Instrs[i]
		ins.forEachVReg(func(vr VReg) {
			if vr > mx {
				mx = vr
			}
		})
	}
	b.maxVreg = mx
	return mx
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
