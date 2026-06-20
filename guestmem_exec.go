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
// No host-side mprotect is performed here. The flat guest mmap retains
// its sandboxing invariant (hostPtr = base + (addr & mask)); this table
// is purely guest-VA metadata.

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

// ExecPageGeneration is a snapshot of the code generation for one guest page.
type ExecPageGeneration struct {
	Page       uint64
	Generation uint64
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
	m.BumpExecGeneration(begin, end)
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
	m.BumpExecGeneration(begin, end)
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

// ExecPageGeneration returns the code generation for addr's guest page.
func (m *GuestMemory) ExecPageGeneration(addr uint64) uint64 {
	if len(m.execPageGenerations) == 0 {
		return 0
	}
	return m.execPageGenerations[execPageBase(addr)]
}

// BumpExecGeneration advances the code generation for every guest page touched
// by [begin, end). This is the invalidation primitive used for executable
// stores, FENCE.I synchronization, mapping changes, and future native block
// cache validation.
func (m *GuestMemory) BumpExecGeneration(begin, end uint64) {
	if begin >= end {
		return
	}
	if m.execPageGenerations == nil {
		m.execPageGenerations = make(map[uint64]uint64)
	}
	for page, last := execPageBase(begin), execPageBase(end-1); ; page += GuestPageSize {
		next := m.execPageGenerations[page] + 1
		if next == 0 {
			next = 1
		}
		m.execPageGenerations[page] = next
		if page == last {
			return
		}
	}
}

// ExecPageGenerations returns a generation snapshot for every guest page
// touched by [begin, end). Missing pages report generation zero.
func (m *GuestMemory) ExecPageGenerations(begin, end uint64) []ExecPageGeneration {
	if begin >= end {
		return nil
	}
	out := make([]ExecPageGeneration, 0, (end-begin+GuestPageSize-1)/GuestPageSize)
	for page, last := execPageBase(begin), execPageBase(end-1); ; page += GuestPageSize {
		out = append(out, ExecPageGeneration{
			Page:       page,
			Generation: m.ExecPageGeneration(page),
		})
		if page == last {
			return out
		}
	}
}

func execPageBase(addr uint64) uint64 {
	return addr &^ (GuestPageSize - 1)
}

func (m *GuestMemory) bumpExecGenerationForStore(addr, width uint64) {
	if width == 0 || len(m.execRegions) == 0 {
		return
	}
	end := addr + width
	if end < addr {
		end = ^uint64(0)
	}
	for _, r := range m.execRegions {
		if addr < r.VAddrEnd && r.VAddrBegin < end {
			m.BumpExecGeneration(addr, end)
			return
		}
	}
}
