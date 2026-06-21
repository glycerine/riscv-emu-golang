package riscv

// guestmem_exec.go — ExecRegion table on GuestMemory.
//
// An ExecRegion is a guest-VA range that the guest has marked executable
// (typically an ELF PT_LOAD R-X segment at load time, or a region the
// guest has mapped +X at runtime via a future mmap/mprotect syscall hook).
//
// The JIT's multi-segment dispatch consults this table on JALR-miss to
// decide whether to build a new DecodedExecuteSegment around the target
// PC (yes → exec region exists) or fall through to lazy compile (no →
// target is in data/bss/unmapped).
//
// No host-side mprotect is performed here. This table is purely guest-VA
// metadata; MemoryModel selects whether compiled memory references use
// base+addr or the sandboxed base+(addr&mask) path.

// ExecRegion represents a guest-VA range that holds executable code.
//
// IsLikelyJIT is true when the region is also writable (RW+X) — i.e.,
// the guest may overwrite code within it (LuaJIT-style). Phase 2b does
// not yet use this flag, but records it for Phase 2c (FENCE.I opt-in,
// stale-on-mprotect, etc.).
type ExecRegion struct {
	VAddrBegin  uint64
	VAddrEnd    uint64 // exclusive
	IsLikelyJIT bool
}

// Contains reports whether pc falls inside the region.
func (r *ExecRegion) Contains(pc uint64) bool {
	return pc >= r.VAddrBegin && pc < r.VAddrEnd
}

// AddExecRegion records that [begin, end) is executable guest memory.
// Overlapping adds are coalesced into a single entry; IsLikelyJIT is
// last-writer-wins on overlap.
//
// begin >= end is a no-op.
func (m *GuestMemory) AddExecRegion(begin, end uint64, isJIT bool) {
	if begin >= end {
		return
	}
	newReg := ExecRegion{VAddrBegin: begin, VAddrEnd: end, IsLikelyJIT: isJIT}
	out := m.execRegions[:0]
	for _, r := range m.execRegions {
		if r.VAddrEnd <= newReg.VAddrBegin || r.VAddrBegin >= newReg.VAddrEnd {
			out = append(out, r)
			continue
		}
		// Overlap — absorb into newReg.
		if r.VAddrBegin < newReg.VAddrBegin {
			newReg.VAddrBegin = r.VAddrBegin
		}
		if r.VAddrEnd > newReg.VAddrEnd {
			newReg.VAddrEnd = r.VAddrEnd
		}
	}
	m.execRegions = append(out, newReg)
}

// RemoveExecRegion removes (or shrinks) any exec region overlap with
// [begin, end). Disjoint regions are preserved; partial overlaps are
// truncated; fully-contained regions are dropped.
//
// begin >= end is a no-op.
func (m *GuestMemory) RemoveExecRegion(begin, end uint64) {
	if begin >= end {
		return
	}
	out := m.execRegions[:0]
	for _, r := range m.execRegions {
		if r.VAddrEnd <= begin || r.VAddrBegin >= end {
			out = append(out, r)
			continue
		}
		if r.VAddrBegin < begin {
			out = append(out, ExecRegion{
				VAddrBegin:  r.VAddrBegin,
				VAddrEnd:    begin,
				IsLikelyJIT: r.IsLikelyJIT,
			})
		}
		if r.VAddrEnd > end {
			out = append(out, ExecRegion{
				VAddrBegin:  end,
				VAddrEnd:    r.VAddrEnd,
				IsLikelyJIT: r.IsLikelyJIT,
			})
		}
	}
	m.execRegions = out
}

// FindExecRegion returns the exec region containing pc, or nil.
//
// Returned pointer aliases into m.execRegions and is valid only until
// the next AddExecRegion / RemoveExecRegion call. Callers that need to
// retain the data should copy the ExecRegion value.
func (m *GuestMemory) FindExecRegion(pc uint64) *ExecRegion {
	for i := range m.execRegions {
		if m.execRegions[i].Contains(pc) {
			return &m.execRegions[i]
		}
	}
	return nil
}

// ExecRegions returns a snapshot copy of the registered exec regions.
// Primarily for tests and diagnostics.
func (m *GuestMemory) ExecRegions() []ExecRegion {
	out := make([]ExecRegion, len(m.execRegions))
	copy(out, m.execRegions)
	return out
}
