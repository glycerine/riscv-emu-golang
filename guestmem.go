// Package riscv implements a RISC-V RV64IMAC emulator.
// This file defines GuestMemory: the fixed-size, mmap-backed memory
// slab that forms the address space of a single guest process.
//
// Sandbox invariant
// =================
// hostPtr(addr) is always within [base, base+size), regardless of addr,
// when sandbox mode is enabled, because (addr & mask) is in [0, size-1]
// by construction. This holds even if the bounds-check logic in check()
// has a bug: the mask is an absolute arithmetic containment guarantee
// independent of the check.
//
// GC invariant
// ============
// The slab is allocated with mmap(MAP_NORESERVE) and is invisible to the
// Go GC. The GC never scans, moves, or accounts for it. Physical pages
// are demand-faulted by the OS as the guest writes to them, so a 512 GB
// guest costs only as much RAM as its actual working set.
package riscv

import (
	"fmt"
	"unsafe"
)

// Guest memory size constants — all powers of two.
// Minimum is 1 MB: the midpoint guard page must be well above the
// highest ELF VA (~0x10000 for riscv-tests). The last 3 pages are
// reserved for the shadow register file, guard page, and sandbox stack.
const (
	Size16KB  uint64 = 1 << 14
	Size32KB  uint64 = 1 << 15
	Size64KB  uint64 = 1 << 16
	Size128KB uint64 = 1 << 17
	Size256KB uint64 = 1 << 18
	Size512KB uint64 = 1 << 19

	Size1MB   uint64 = 1 << 20
	Size4MB   uint64 = 1 << 22
	Size64MB  uint64 = 1 << 26
	Size128MB uint64 = 1 << 27
	Size256MB uint64 = 1 << 28
	Size512MB uint64 = 1 << 29
	Size1GB   uint64 = 1 << 30
	Size2GB   uint64 = 1 << 31
	Size4GB   uint64 = 1 << 32
	Size8GB   uint64 = 1 << 33
	Size16GB  uint64 = 1 << 34
	Size32GB  uint64 = 1 << 35
	Size64GB  uint64 = 1 << 36
	Size128GB uint64 = 1 << 37
	Size256GB uint64 = 1 << 38
	Size512GB uint64 = 1 << 39

	// MaxGuestMemory caps a single guest at 512 GB. The host user virtual
	// address space on Linux/amd64 is 128 TB, so many guests can coexist.
	MaxGuestMemory = Size512GB

	// Guest memory layout — top-of-mmap reservations.
	// For a guest mmap [base, base+guestSize):
	//   [b-4096, b)        shadow register file (520 bytes)
	//   [b-8192, b-4096)   guard page (catches stack underflow)
	//   [b-8192 ... down)  sandbox stack, grows downward
	// where b = base + guestSize.
	GuestPageSize = 4096
)

// FaultKind classifies a MemFault.
type FaultKind uint8

const (
	FaultLoad          FaultKind = iota // guest attempted an out-of-bounds load
	FaultStore                          // guest attempted an out-of-bounds store
	FaultFetch                          // guest PC pointed outside memory
	FaultMisalign                       // access was not naturally aligned
	FaultSandboxEscape                  // address was truncated by the sandbox mask
	FaultPageLoad                       // guest load hit a virtual-memory page fault
	FaultPageStore                      // guest store hit a virtual-memory page fault
	FaultPageFetch                      // guest fetch hit a virtual-memory page fault
)

// CheckSandboxBounds enables strict address checking. When true, any
// load/store where the mask changes the address (addr != addr & mask)
// returns FaultSandboxEscape instead of silently wrapping. Use for
// debugging aliasing bugs.
var CheckSandboxBounds bool = true

func (k FaultKind) String() string {
	switch k {
	case FaultLoad:
		return "load"
	case FaultStore:
		return "store"
	case FaultFetch:
		return "fetch"
	case FaultMisalign:
		return "misalign"
	case FaultSandboxEscape:
		return "sandbox_escape"
	case FaultPageLoad:
		return "page_load"
	case FaultPageStore:
		return "page_store"
	case FaultPageFetch:
		return "page_fetch"
	default:
		return "unknown"
	}
}

// MemFault is a guest-level memory fault. It is never a host panic;
// the emulator catches it and delivers it to the guest as a trap.
type MemFault struct {
	Addr  uint64
	Width uint64
	Kind  FaultKind
}

func (f *MemFault) Error() string {
	return fmt.Sprintf("guest memory fault: kind=%s addr=0x%016x width=%d",
		f.Kind, f.Addr, f.Width)
}

// GuestMemory is a fixed-size, power-of-two, mmap-backed address space
// for a single guest. It is safe to use from multiple goroutines only if
// the caller enforces its own synchronization (the scheduler does this).
//
// Interpreter loads and stores are bounds-checked and alignment-checked.
// In sandbox mode, hostPtr cannot escape [base, base+size) by construction.
// In linear mode, hostPtr is base+addr after the normal bounds check.
type GuestMemory struct {
	// base is the host address of the first byte of the guest memory slab.
	// Stored as unsafe.Pointer so that pointer arithmetic via unsafe.Add
	// satisfies Go's unsafe.Pointer rules (rule #3). The underlying
	// memory is mmap, never traced or moved by the GC.
	base unsafe.Pointer

	// mask is size-1. Because size is a power of two, mask has all
	// bits set below the size boundary and all bits clear above it.
	// It serves dual purpose:
	//   containment: addr & mask ∈ [0, size-1] always
	//   bounds check: (addr | (addr+w-1)) & ^mask == 0 iff in-bounds
	mask uint64

	// size is kept for munmap and for ZeroRange bounds checking.
	size uint64

	// sandbox selects the containing address translation:
	//   sandbox: hostPtr = base + (addr & mask)
	//   linear:  hostPtr = base + addr
	sandbox bool

	// scratch is a reusable MemFault to avoid heap allocation on every fault.
	// Only one fault is ever live at a time (caller checks, then discards).
	scratch MemFault

	// execRegions tracks guest-VA ranges that contain executable code.
	// Used by the JIT's multi-segment dispatch to decide where to place
	// a new DecodedExecuteSegment on demand. Maintained by
	// AddExecRegion / RemoveExecRegion / FindExecRegion in guestmem_exec.go.
	// The list stays small (≤ handful of entries); linear scan is fine.
	execRegions []ExecRegion

	// loadedELFSize remembers the byte length of the most recent ELF loaded
	// into this memory. loadedELFImageSize remembers the summed PT_LOAD
	// in-memory footprint. The lazy native JIT sizes its single executable
	// code arena from this metadata, avoiding per-block executable mmaps.
	loadedELFSize      uint64
	loadedELFImageSize uint64

	// accessOverlay is an optional personality-owned permission layer.
	// It is nil for the normal bare-metal/test/JIT paths; jea9linux installs
	// one to model Linux brk/mmap/munmap/mprotect without changing the slab
	// containment invariant.
	accessOverlay guestMemoryAccessOverlay

	// mmio is the concrete bare-machine board device hook. It stays nil for
	// normal process/personality workloads; BIOS machine boot installs it for
	// devices described in the generated FDT.
	mmio *biosMMIO

	// TohostAddr is the address of the "tohost" symbol found during ELF
	// loading. Non-zero means the loaded binary uses the HTIF tohost
	// protocol and the JIT must be configured with a matching watchAddr.
	TohostAddr uint64
}

type guestMemoryAccessOverlay interface {
	CheckGuestAccess(addr, width uint64, kind FaultKind, size uint64) *MemFault
}

// NewGuestMemory allocates a guest address space of the given size.
// size must be a power of two and must not exceed MaxGuestMemory.
// Physical memory is demand-paged; only virtual address space is
// reserved immediately.
func NewGuestMemory(size uint64) (*GuestMemory, error) {
	return newGuestMemory(size, true)
}

// NewLinearGuestMemory allocates guest memory with zero-based guest virtual
// addresses translated as hostPtr=base+addr. It omits the sandbox mask but
// keeps normal interpreter bounds checks.
func NewLinearGuestMemory(size uint64) (*GuestMemory, error) {
	return newGuestMemory(size, false)
}

func newGuestMemory(size uint64, sandbox bool) (*GuestMemory, error) {
	if size == 0 || size&(size-1) != 0 {
		return nil, fmt.Errorf("guestmem: size must be a non-zero power of two, got %d", size)
	}
	if size > MaxGuestMemory {
		return nil, fmt.Errorf("guestmem: size %d exceeds maximum %d", size, MaxGuestMemory)
	}

	ptr, err := guestAlloc(size)
	if err != nil {
		return nil, fmt.Errorf("guestmem: mmap failed for size %d: %w", size, err)
	}

	m := &GuestMemory{
		base:    unsafe.Pointer(ptr),
		mask:    size - 1,
		size:    size,
		sandbox: sandbox,
	}

	// Install guard pages (PROT_NONE):
	//   1. Midpoint: divides heap (grows up) from stack (grows down)
	//   2. Between sandbox stack and register file: catches stack underflow
	// Page 0 is NOT guarded — riscv-tests ELFs load code at VA 0x0000.
	// The mask already contains all guest accesses, so a guest null
	// dereference just hits offset 0 of the mmap (harmless to the host).
	pg := uintptr(GuestPageSize)
	//guestGuard(ptr, GuestPageSize)
	//guestGuard(unsafe.Pointer(uintptr(ptr)+uintptr(size/2)), GuestPageSize)             // midpoint
	_ = guestGuard(unsafe.Pointer(uintptr(ptr)+uintptr(size)-2*pg), GuestPageSize) // stack/regfile // unexpected fault address 0x15783f000 on go test -v -run TestFusion_SLLI_SRLI_ZextW (intermittant).

	return m, nil
}

// Free releases the slab immediately. Safe to call multiple times.
// After Free, any use of the GuestMemory is undefined behavior.
func (m *GuestMemory) Free() {
	if m.base != nil {
		_ = guestFree(m.base, m.size)
		m.base = nil
	}
}

// CowClone returns a new GuestMemory whose contents are a copy-on-write
// view of m's. Shared physical pages stay shared until either side
// writes; after a side writes to a given page, that side's page is
// private. Used by Machine.Clone to fork sandboxes cheaply.
//
// Unlike NewGuestMemory, the returned *GuestMemory has NO runtime
// finalizer — the caller must call Free() explicitly to release the
// CoW mapping. (A finalizer would race with the parent's Free and
// with CPU.mem value-copies that still reference the underlying
// mapping.)
//
// The child inherits m's size, mask, and a copy of its execRegions.
// The child's scratch MemFault is fresh (zero value).
func (m *GuestMemory) CowClone() (*GuestMemory, error) {
	// Temporarily unguard so COWRemap can read the entire mmap.
	guardOff := uintptr(m.size) - 2*GuestPageSize
	_ = guestUnguard(unsafe.Add(m.base, guardOff), GuestPageSize)

	newBase, err := COWRemap(m.size, m.base)

	// Re-guard the parent.
	_ = guestGuard(unsafe.Add(m.base, guardOff), GuestPageSize)

	if err != nil {
		return nil, fmt.Errorf("guestmem: CowClone: %w", err)
	}

	// Guard the child's stack/regfile page too.
	_ = guestGuard(unsafe.Add(newBase, guardOff), GuestPageSize)

	var execRegionsCopy []ExecRegion
	if len(m.execRegions) > 0 {
		execRegionsCopy = append(execRegionsCopy, m.execRegions...)
	}
	return &GuestMemory{
		base:               newBase,
		mask:               m.mask,
		size:               m.size,
		sandbox:            m.sandbox,
		execRegions:        execRegionsCopy,
		loadedELFSize:      m.loadedELFSize,
		loadedELFImageSize: m.loadedELFImageSize,
	}, nil
}

// Size returns the guest address space size in bytes.
func (m *GuestMemory) Size() uint64 { return m.size }

func (m *GuestMemory) LoadedELFSize() uint64 { return m.loadedELFSize }

func (m *GuestMemory) LoadedELFImageSize() uint64 { return m.loadedELFImageSize }

func (m *GuestMemory) Sandbox() bool { return m.sandbox }

func (m *GuestMemory) GuestStart() uint64 {
	return 0
}

func (m *GuestMemory) GuestEnd() uint64 {
	return m.GuestStart() + m.size
}

func (m *GuestMemory) GuestAddr(offset uint64) uint64 {
	return offset
}

func (m *GuestMemory) GuestOffset(addr uint64) (uint64, bool) {
	if addr >= m.size {
		return 0, false
	}
	return addr, true
}

func (m *GuestMemory) guestRangeOffset(addr, length uint64) (uint64, bool) {
	if length == 0 {
		return 0, true
	}
	end := addr + length
	if end < addr {
		return 0, false
	}
	if end > m.size {
		return 0, false
	}
	return addr, true
}

// Mask returns size-1. ANDing any uint64 address with Mask() produces a
// valid in-bounds guest address by construction — the same guarantee the
// memory system uses internally for its containment invariant.
func (m *GuestMemory) Mask() uint64  { return m.mask }
func (m *GuestMemory) Base() uintptr { return uintptr(m.base) }

func (m *GuestMemory) setAccessOverlay(o guestMemoryAccessOverlay) {
	m.accessOverlay = o
}

func (m *GuestMemory) clearAccessOverlay(o guestMemoryAccessOverlay) {
	if m.accessOverlay == o {
		m.accessOverlay = nil
	}
}

func (m *GuestMemory) SetMMIO(mmio *biosMMIO) {
	m.mmio = mmio
}

func (m *GuestMemory) ClearMMIO(mmio *biosMMIO) {
	if m.mmio == mmio {
		m.mmio = nil
	}
}

func (m *GuestMemory) RegFileBase() uintptr { return uintptr(m.base) + uintptr(m.size) - GuestPageSize }
func (m *GuestMemory) StackTop() uintptr    { return uintptr(m.base) + uintptr(m.size) - 2*GuestPageSize }

// RawSlice returns a zero-copy byte slice over the entire guest memory slab.
func (m *GuestMemory) RawSlice() []byte {
	return unsafe.Slice((*byte)(m.base), m.size)
}

// ZeroRange returns physical pages in [addr, addr+length) to the OS.
// Virtual address space is retained. Use to reclaim RAM after a guest
// process exits without reallocating the address space for reuse.
func (m *GuestMemory) ZeroRange(addr, length uint64) *MemFault {
	off, ok := m.guestRangeOffset(addr, length)
	if !ok {
		return &MemFault{addr, length, FaultStore}
	}
	_ = guestZeroRange(unsafe.Add(m.base, uintptr(off)), length)
	return nil
}

// ---------------------------------------------------------------------------
// Internal primitives — not exported; used only by load/store functions.
// ---------------------------------------------------------------------------

// check returns 0 if [addr, addr+width) is a valid naturally-aligned access,
// nonzero otherwise. Single expression combining both checks; no branch.
//
// Term 1: addr & (width-1)
//
//	Zero iff addr is a multiple of width (natural alignment).
//	Nonzero iff misaligned.
//
// Term 2: (addr | (addr+width-1)) & ^mask
//
//	addr+width-1 is the address of the last byte of the access.
//	OR-ing with addr propagates any out-of-range high bits from either end.
//	ANDing with ^mask isolates bits above the address space.
//	Zero iff both addr and addr+width-1 are within [0, size).
//
// Combined with OR: zero iff aligned AND in-bounds.
//
//go:nosplit
func (m *GuestMemory) check(addr, width uint64) uint64 {
	return (addr & (width - 1)) |
		((addr | (addr + width - 1)) & ^m.mask)
}

// checkSandboxEscape returns a non-nil MemFault if CheckSandboxBounds is
// enabled and the sandbox mask would change the address (addr != addr & mask).
// This detects addresses that would segfault on real hardware but silently
// wrap in the emulator.
func (m *GuestMemory) checkSandboxEscape(addr, width uint64, kind FaultKind) *MemFault {
	if !m.sandbox {
		return nil
	}
	if CheckSandboxBounds && (addr & ^m.mask) != 0 {
		return &MemFault{Addr: addr, Width: width, Kind: FaultSandboxEscape}
	}
	return nil
}

func (m *GuestMemory) checkAccessOverlay(addr, width uint64, kind FaultKind) *MemFault {
	if m.accessOverlay == nil {
		return nil
	}
	return m.accessOverlay.CheckGuestAccess(addr, width, kind, m.size)
}

// hostPtr returns a host pointer to guest address addr. In sandbox mode,
// the mask guarantees the result is always within [base, base+size):
//
//	addr & mask ∈ [0, size-1]  →  base + (addr&mask) ∈ [base, base+size-1]
//
// Callers must have verified check(addr, width)==0 before dereferencing.
// unsafe.Add from an unsafe.Pointer satisfies Go's pointer rule #3.
//
//go:nosplit
func (m *GuestMemory) hostPtr(addr uint64) unsafe.Pointer {
	if m.sandbox {
		return unsafe.Add(m.base, addr&m.mask)
	}
	return unsafe.Add(m.base, uintptr(addr))
}

// fault constructs a MemFault, distinguishing misalignment from OOB.
//
//go:nosplit
func (m *GuestMemory) fault(addr, width uint64, defaultKind FaultKind) *MemFault {
	kind := defaultKind
	if addr&(width-1) != 0 {
		kind = FaultMisalign
	}
	m.scratch = MemFault{addr, width, kind}
	return &m.scratch
}

// ---------------------------------------------------------------------------
// Loads
// ---------------------------------------------------------------------------

// Load8 loads one byte from guest address addr. No alignment requirement.
//
//go:nosplit
func (m *GuestMemory) Load8(addr uint64) (uint8, *MemFault) {
	if f := m.checkSandboxEscape(addr, 1, FaultLoad); f != nil {
		return 0, f
	}
	if f := m.checkAccessOverlay(addr, 1, FaultLoad); f != nil {
		return 0, f
	}
	if m.mmio != nil {
		if v, ok, f := m.mmio.Load(addr, 1); ok || f != nil {
			return uint8(v), f
		}
	}
	if m.check(addr, 1) != 0 {
		return 0, m.fault(addr, 1, FaultLoad)
	}
	return *(*uint8)(m.hostPtr(addr)), nil
}

// Load16 loads a 16-bit little-endian value from addr. addr must be 2-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Load16(addr uint64) (uint16, *MemFault) {
	if f := m.checkSandboxEscape(addr, 2, FaultLoad); f != nil {
		return 0, f
	}
	if f := m.checkAccessOverlay(addr, 2, FaultLoad); f != nil {
		return 0, f
	}
	if m.mmio != nil {
		if v, ok, f := m.mmio.Load(addr, 2); ok || f != nil {
			return uint16(v), f
		}
	}
	if m.check(addr, 2) != 0 {
		return 0, m.fault(addr, 2, FaultLoad)
	}
	return *(*uint16)(m.hostPtr(addr)), nil
}

// Load32 loads a 32-bit little-endian value from addr. addr must be 4-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Load32(addr uint64) (uint32, *MemFault) {
	if f := m.checkSandboxEscape(addr, 4, FaultLoad); f != nil {
		return 0, f
	}
	if f := m.checkAccessOverlay(addr, 4, FaultLoad); f != nil {
		return 0, f
	}
	if m.mmio != nil {
		if v, ok, f := m.mmio.Load(addr, 4); ok || f != nil {
			return uint32(v), f
		}
	}
	if m.check(addr, 4) != 0 {
		return 0, m.fault(addr, 4, FaultLoad)
	}
	return *(*uint32)(m.hostPtr(addr)), nil
}

// Load64 loads a 64-bit little-endian value from addr. addr must be 8-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Load64(addr uint64) (uint64, *MemFault) {
	if f := m.checkSandboxEscape(addr, 8, FaultLoad); f != nil {
		return 0, f
	}
	if f := m.checkAccessOverlay(addr, 8, FaultLoad); f != nil {
		return 0, f
	}
	if m.mmio != nil {
		if v, ok, f := m.mmio.Load(addr, 8); ok || f != nil {
			return v, f
		}
	}
	if m.check(addr, 8) != 0 {
		return 0, m.fault(addr, 8, FaultLoad)
	}
	return *(*uint64)(m.hostPtr(addr)), nil
}

// ---------------------------------------------------------------------------
// Stores
// ---------------------------------------------------------------------------

// Store8 stores one byte at guest address addr.
//
//go:nosplit
func (m *GuestMemory) Store8(addr uint64, v uint8) *MemFault {
	if f := m.checkSandboxEscape(addr, 1, FaultStore); f != nil {
		return f
	}
	if f := m.checkAccessOverlay(addr, 1, FaultStore); f != nil {
		return f
	}
	if m.mmio != nil {
		if ok, f := m.mmio.Store(addr, 1, uint64(v)); ok || f != nil {
			return f
		}
	}
	if m.check(addr, 1) != 0 {
		return m.fault(addr, 1, FaultStore)
	}
	*(*uint8)(m.hostPtr(addr)) = v
	return nil
}

// Store16 stores a 16-bit little-endian value at addr. addr must be 2-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Store16(addr uint64, v uint16) *MemFault {
	if f := m.checkSandboxEscape(addr, 2, FaultStore); f != nil {
		return f
	}
	if f := m.checkAccessOverlay(addr, 2, FaultStore); f != nil {
		return f
	}
	if m.mmio != nil {
		if ok, f := m.mmio.Store(addr, 2, uint64(v)); ok || f != nil {
			return f
		}
	}
	if m.check(addr, 2) != 0 {
		return m.fault(addr, 2, FaultStore)
	}
	*(*uint16)(m.hostPtr(addr)) = v
	return nil
}

// Store32 stores a 32-bit little-endian value at addr. addr must be 4-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Store32(addr uint64, v uint32) *MemFault {
	if f := m.checkSandboxEscape(addr, 4, FaultStore); f != nil {
		return f
	}
	if f := m.checkAccessOverlay(addr, 4, FaultStore); f != nil {
		return f
	}
	if m.mmio != nil {
		if ok, f := m.mmio.Store(addr, 4, uint64(v)); ok || f != nil {
			return f
		}
	}
	if m.check(addr, 4) != 0 {
		return m.fault(addr, 4, FaultStore)
	}
	*(*uint32)(m.hostPtr(addr)) = v
	return nil
}

// Store64 stores a 64-bit little-endian value at addr. addr must be 8-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Store64(addr uint64, v uint64) *MemFault {
	if f := m.checkSandboxEscape(addr, 8, FaultStore); f != nil {
		return f
	}
	if f := m.checkAccessOverlay(addr, 8, FaultStore); f != nil {
		return f
	}
	if m.mmio != nil {
		if ok, f := m.mmio.Store(addr, 8, v); ok || f != nil {
			return f
		}
	}
	if m.check(addr, 8) != 0 {
		return m.fault(addr, 8, FaultStore)
	}
	*(*uint64)(m.hostPtr(addr)) = v
	return nil
}

// ---------------------------------------------------------------------------
// Unaligned loads — read byte-by-byte, little-endian. No alignment requirement.
// Used by cpu.go when a naturally-aligned load hits FaultMisalign.
// ---------------------------------------------------------------------------

// Load16U loads a 16-bit value from an unaligned address.
func (m *GuestMemory) Load16U(addr uint64) (uint16, *MemFault) {
	b0, f := m.Load8(addr)
	if f != nil {
		return 0, f
	}
	b1, f := m.Load8(addr + 1)
	if f != nil {
		return 0, f
	}
	return uint16(b0) | uint16(b1)<<8, nil
}

// Load32U loads a 32-bit value from an unaligned address.
func (m *GuestMemory) Load32U(addr uint64) (uint32, *MemFault) {
	v := uint32(0)
	for i := uint64(0); i < 4; i++ {
		b, f := m.Load8(addr + i)
		if f != nil {
			return 0, f
		}
		v |= uint32(b) << (i * 8)
	}
	return v, nil
}

// Load64U loads a 64-bit value from an unaligned address.
func (m *GuestMemory) Load64U(addr uint64) (uint64, *MemFault) {
	v := uint64(0)
	for i := uint64(0); i < 8; i++ {
		b, f := m.Load8(addr + i)
		if f != nil {
			return 0, f
		}
		v |= uint64(b) << (i * 8)
	}
	return v, nil
}

// Store16U stores a 16-bit value at an unaligned address.
func (m *GuestMemory) Store16U(addr uint64, v uint16) *MemFault {
	if f := m.Store8(addr, uint8(v)); f != nil {
		return f
	}
	return m.Store8(addr+1, uint8(v>>8))
}

// Store32U stores a 32-bit value at an unaligned address.
func (m *GuestMemory) Store32U(addr uint64, v uint32) *MemFault {
	for i := uint64(0); i < 4; i++ {
		if f := m.Store8(addr+i, uint8(v>>(i*8))); f != nil {
			return f
		}
	}
	return nil
}

// Store64U stores a 64-bit value at an unaligned address.
func (m *GuestMemory) Store64U(addr uint64, v uint64) *MemFault {
	for i := uint64(0); i < 8; i++ {
		if f := m.Store8(addr+i, uint8(v>>(i*8))); f != nil {
			return f
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Instruction fetch — separate entry points to report FaultFetch kind.
// ---------------------------------------------------------------------------

// Fetch16 fetches a 16-bit instruction halfword. Requires 2-byte alignment.
// Used to probe for compressed (RVC) vs standard (32-bit) instructions.
//
//go:nosplit
func (m *GuestMemory) Fetch16(addr uint64) (uint16, *MemFault) {
	if f := m.checkAccessOverlay(addr, 2, FaultFetch); f != nil {
		return 0, f
	}
	if m.check(addr, 2) != 0 {
		return 0, m.fault(addr, 2, FaultFetch)
	}
	return *(*uint16)(m.hostPtr(addr)), nil
}

// Fetch32 fetches a 32-bit instruction word. Requires 4-byte alignment.
//
//go:nosplit
func (m *GuestMemory) Fetch32(addr uint64) (uint32, *MemFault) {
	if f := m.checkAccessOverlay(addr, 4, FaultFetch); f != nil {
		return 0, f
	}
	if m.check(addr, 4) != 0 {
		return 0, m.fault(addr, 4, FaultFetch)
	}
	return *(*uint32)(m.hostPtr(addr)), nil
}

// Fetch32U fetches a 32-bit instruction word without alignment requirement.
// Used to read 32-bit instructions at 2-byte-aligned addresses (C extension).
//
//go:nosplit
func (m *GuestMemory) Fetch32U(addr uint64) (uint32, *MemFault) {
	lo, f := m.Fetch16(addr)
	if f != nil {
		return 0, f
	}
	hi, f := m.Fetch16(addr + 2)
	if f != nil {
		return 0, f
	}
	return uint32(lo) | uint32(hi)<<16, nil
}

// ---------------------------------------------------------------------------
// Bulk operations — used by ELF loader and syscall data transfer.
// These use range arithmetic rather than the mask, so they correctly
// reject accesses that would wrap rather than silently truncating.
// ---------------------------------------------------------------------------

// ReadBytes copies len(dst) bytes from guest address addr into dst.
// No alignment requirement. Used by syscall handlers to safely read
// guest strings and structs.
func (m *GuestMemory) ReadBytes(addr uint64, dst []byte) *MemFault {
	length := uint64(len(dst))
	if length == 0 {
		return nil
	}
	if f := m.checkAccessOverlay(addr, length, FaultLoad); f != nil {
		return f
	}
	if _, ok := m.guestRangeOffset(addr, length); !ok {
		return &MemFault{addr, length, FaultLoad}
	}
	// src is a slice header pointing into C memory.
	// Go's copy handles this correctly.
	src := unsafe.Slice((*byte)(m.hostPtr(addr)), length)
	copy(dst, src)
	return nil
}

// WriteBytes copies src into guest memory starting at addr.
// No alignment requirement. Used by the ELF loader and syscall return data.
func (m *GuestMemory) WriteBytes(addr uint64, src []byte) *MemFault {
	length := uint64(len(src))
	if length == 0 {
		return nil
	}
	if f := m.checkAccessOverlay(addr, length, FaultStore); f != nil {
		return f
	}
	if _, ok := m.guestRangeOffset(addr, length); !ok {
		return &MemFault{addr, length, FaultStore}
	}
	dst := unsafe.Slice((*byte)(m.hostPtr(addr)), length)
	copy(dst, src)
	return nil
}

// Zero zeroes length bytes starting at addr in guest memory.
// Used by the ELF loader to initialise BSS segments.
func (m *GuestMemory) Zero(addr, length uint64) *MemFault {
	if length == 0 {
		return nil
	}
	if _, ok := m.guestRangeOffset(addr, length); !ok {
		return &MemFault{addr, length, FaultStore}
	}
	dst := unsafe.Slice((*byte)(m.hostPtr(addr)), length)
	clear(dst)
	return nil
}
