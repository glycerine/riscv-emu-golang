//go:build none

package riscv

import "testing"

// FuzzELS_NoConflicts generates random IR blocks with branches, labels,
// and memory ops, then verifies the ELS allocator produces no register
// conflicts and valid spill slots.
func FuzzELS_NoConflicts(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06})
	f.Add([]byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80})
	f.Add([]byte{0xFF, 0x00, 0xAA, 0x55, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 {
			return
		}

		e := NewEmitter()

		// Pre-create labels (max 4).
		numLabels := int(data[0]%4) + 1
		labels := make([]Label, numLabels)
		for i := range labels {
			labels[i] = e.NewLabel()
		}

		// Use guest regs x1-x4 and temps.
		guestRegs := []VReg{e.XReg(1), e.XReg(2), e.XReg(3), e.XReg(4)}
		for _, vr := range guestRegs {
			e.Const(vr, 0)
			e.MarkDirty(vr)
		}

		// Emit random instructions from data[1:].
		labelsPlaced := make([]bool, numLabels)
		for i := 1; i+1 < len(data) && len(e.Block.Instrs) < 80; i += 2 {
			op := data[i] % 10
			reg := data[i+1]
			gr := guestRegs[int(reg)%len(guestRegs)]
			gr2 := guestRegs[int(reg/4)%len(guestRegs)]

			switch op {
			case 0: // Add
				tmp := e.Tmp()
				e.Add(tmp, gr, gr2)
			case 1: // AddImm
				e.AddImm(gr, gr, int64(reg))
				e.MarkDirty(gr)
			case 2: // Const
				e.Const(gr, int64(reg)*100)
				e.MarkDirty(gr)
			case 3: // Branch (forward to next unplaced label)
				for li := range labels {
					if !labelsPlaced[li] {
						tmp := e.Tmp()
						e.AddImm(tmp, gr, 0)
						e.Branch(tmp, VRegZero, NE, labels[li])
						break
					}
				}
			case 4: // Place a label
				for li := range labels {
					if !labelsPlaced[li] {
						e.PlaceLabel(labels[li])
						labelsPlaced[li] = true
						break
					}
				}
			case 5: // Mov
				e.Mov(gr, gr2)
				e.MarkDirty(gr)
			case 6: // Sub
				tmp := e.Tmp()
				e.Sub(tmp, gr, gr2)
			case 7: // Xor
				e.Xor(gr, gr, gr2)
				e.MarkDirty(gr)
			default: // And
				tmp := e.Tmp()
				e.And(tmp, gr, gr2)
			}
		}

		// Place remaining labels.
		for li := range labels {
			if !labelsPlaced[li] {
				e.PlaceLabel(labels[li])
			}
		}

		// Exit.
		e.WriteBackAll()
		e.Ret(0x1000, 0, VRegZero)

		if len(e.Block.Instrs) < 3 {
			return
		}

		pool := testPool(3, 0)
		alloc := NewAllocator().Allocate(e.Block, pool, nil, nil)

		// Invariant 1: no register conflicts.
		assertNoConflicts(t, alloc)

		// Invariant 2: every VReg with a computed interval is allocated.
		intervals := computeIntervalSets(e.Block)
		for vr := VReg(1); int(vr) < len(intervals) && int(vr) < len(alloc.Kind); vr++ {
			if len(intervals[vr].Intervals) > 0 && alloc.Kind[vr] == AllocUnused {
				t.Errorf("VReg %d has intervals %v but AllocUnused", vr, intervals[vr].Intervals)
			}
		}

		// Invariant 3: unique spill slots.
		slots := make(map[int16]VReg)
		for vr := VReg(1); int(vr) < len(alloc.Kind); vr++ {
			if alloc.Kind[vr] == AllocStack {
				slot := alloc.SpillSlot[vr]
				if slot < 0 {
					t.Errorf("VReg %d spilled with negative slot %d", vr, slot)
				}
				if prev, ok := slots[slot]; ok {
					t.Errorf("VReg %d and %d share slot %d", prev, vr, slot)
				}
				slots[slot] = vr
			}
		}

		// Invariant 4: StackSlots consistent.
		if len(slots) > alloc.StackSlots {
			t.Errorf("spilled %d VRegs but StackSlots=%d", len(slots), alloc.StackSlots)
		}
	})
}

// FuzzELS_ForwardBranchLiveness generates blocks with forward conditional
// branches and verifies VRegs live at the target are also live at the branch.
func FuzzELS_ForwardBranchLiveness(f *testing.F) {
	f.Add([]byte{0x03, 0x05, 0x07, 0x0A})
	f.Add([]byte{0x01, 0x02, 0x04, 0x08, 0x10, 0x20})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 3 {
			return
		}

		e := NewEmitter()
		label := e.NewLabel()

		x1 := e.XReg(1)
		x2 := e.XReg(2)
		e.Const(x1, int64(data[0]))
		e.Const(x2, int64(data[1]))
		e.MarkDirty(x1)
		e.MarkDirty(x2)

		// Emit some instructions before the branch.
		for i := 2; i < len(data) && i < 8; i++ {
			tmp := e.Tmp()
			e.AddImm(tmp, x1, int64(data[i]))
		}

		// Forward branch.
		branchTmp := e.Tmp()
		e.AddImm(branchTmp, x1, 0)
		branchIdx := len(e.Block.Instrs)
		e.Branch(branchTmp, VRegZero, NE, label)

		// Fall-through: more instructions.
		for i := 0; i < int(data[0]%4)+1; i++ {
			tmp := e.Tmp()
			e.Add(tmp, x1, x2)
		}
		e.WriteBackAll()
		e.Ret(0x1000, 0, VRegZero)

		// Branch target.
		e.PlaceLabel(label)
		e.WriteBackAll() // uses x1, x2
		e.Ret(0x2000, 0, VRegZero)

		intervals := computeIntervalSets(e.Block)

		// For each guest reg used in WriteBackAll at the label target,
		// check it's live at the branch point.
		for _, vr := range []VReg{1, 2} {
			if int(vr) >= len(intervals) {
				continue
			}
			live := false
			for _, iv := range intervals[vr].Intervals {
				if iv.Start <= branchIdx && branchIdx <= iv.End {
					live = true
					break
				}
			}
			if !live {
				t.Errorf("x%d not live at forward branch point %d; intervals: %v",
					vr, branchIdx, intervals[vr].Intervals)
			}
		}
	})
}

// FuzzELS_GuestRegExtension verifies that the last interval of each used
// guest reg extends to n-1 (block end).
func FuzzELS_GuestRegExtension(f *testing.F) {
	f.Add([]byte{0x01, 0x42, 0x03})
	f.Add([]byte{0x10, 0x20, 0x30, 0x40})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		e := NewEmitter()

		// Use some guest regs based on data.
		numRegs := int(data[0]%6) + 1
		for i := 0; i < numRegs && i+1 < len(data); i++ {
			vr := e.XReg(uint32(i + 1))
			e.Const(vr, int64(data[i+1]))
			e.MarkDirty(vr)
		}

		// Add some dummy ops.
		for i := numRegs + 1; i < len(data) && len(e.Block.Instrs) < 30; i++ {
			tmp := e.Tmp()
			e.Const(tmp, int64(data[i]))
		}

		e.WriteBackAll()
		e.Ret(0x1000, 0, VRegZero)

		n := len(e.Block.Instrs)
		intervals := computeIntervalSets(e.Block)

		for vr := VReg(1); vr <= 63 && int(vr) < len(intervals); vr++ {
			ivals := intervals[vr].Intervals
			if len(ivals) == 0 {
				continue
			}
			last := ivals[len(ivals)-1]
			if last.End != n-1 {
				t.Errorf("guest VReg %d last interval ends at %d, want %d (n-1)",
					vr, last.End, n-1)
			}
		}
	})
}

// FuzzELS_SpillSlotUniqueness generates high-pressure blocks and verifies
// spill slots are unique.
func FuzzELS_SpillSlotUniqueness(f *testing.F) {
	f.Add([]byte{0x05, 0x0A, 0x0F, 0x14, 0x19, 0x1E})
	f.Add([]byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB, 0xFA, 0xF9, 0xF8})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 {
			return
		}

		e := NewEmitter()

		// Create many temps to force spilling.
		temps := make([]VReg, 0, len(data))
		for i, d := range data {
			if len(e.Block.Instrs) > 60 {
				break
			}
			tmp := e.Tmp()
			e.Const(tmp, int64(d)*int64(i+1))
			temps = append(temps, tmp)
		}

		// Use pairs to create overlapping live ranges.
		for i := 0; i+1 < len(temps) && len(e.Block.Instrs) < 80; i++ {
			out := e.Tmp()
			e.Add(out, temps[i], temps[i+1])
			temps = append(temps, out)
		}

		e.Ret(0x1000, 0, VRegZero)

		// Small pool → lots of spills.
		pool := testPool(2, 0)
		alloc := NewAllocator().Allocate(e.Block, pool, nil, nil)

		slots := make(map[int16]VReg)
		spillCount := 0
		for vr := VReg(1); int(vr) < len(alloc.Kind); vr++ {
			if alloc.Kind[vr] == AllocStack {
				spillCount++
				slot := alloc.SpillSlot[vr]
				if slot < 0 || int(slot) >= alloc.StackSlots {
					t.Errorf("VReg %d: slot %d out of range [0, %d)", vr, slot, alloc.StackSlots)
				}
				if prev, ok := slots[slot]; ok {
					t.Errorf("VReg %d and %d share slot %d", prev, vr, slot)
				}
				slots[slot] = vr
			}
		}

		if spillCount > alloc.StackSlots {
			t.Errorf("%d spilled VRegs but StackSlots=%d", spillCount, alloc.StackSlots)
		}
	})
}
