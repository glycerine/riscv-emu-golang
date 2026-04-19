// Package riscv implements a sandboxed RISC-V RV64IMAC emulator.
// This file defines GuestMemory: the fixed-size, C mmap-backed memory
// slab that forms the address space of a single guest process.
//
// Security invariant
// ==================
// hostPtr(addr) is always within [base, base+size), regardless of addr,
// because (addr & mask) ∈ [0, size-1] by construction. This holds even
// if the bounds-check logic in check() has a bug — the mask is an
// absolute arithmetic containment guarantee independent of the check.
//
// GC invariant
// ============
// The slab is allocated with mmap(MAP_NORESERVE) and is invisible to the
// Go GC. The GC never scans, moves, or accounts for it. Physical pages
// are demand-faulted by the OS as the guest writes to them, so a 512 GB
// guest costs only as much RAM as its actual working set.
package riscv

/*
#include <sys/mman.h>
#include <stdint.h>
#include <stdlib.h>
#include <errno.h>

// guest_alloc allocates size bytes of virtual address space with
// MAP_NORESERVE. Physical pages are allocated on first access (demand
// paging). Returns NULL on failure and sets errno.
static void* guest_alloc(size_t size) {
    void* p = mmap(NULL, size,
                   PROT_READ | PROT_WRITE,
                   MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE,
                   -1, 0);
    if (p == MAP_FAILED) return NULL;
    return p;
}

// guest_free releases a slab previously returned by guest_alloc.
static void guest_free(void* p, size_t size) {
    munmap(p, size);
}

// guest_zero_range returns physical pages in [p, p+size) to the OS and
// re-zeros them on next access. Virtual address reservation is retained.
// Use after a guest process exits to reclaim RAM without reallocating
// the virtual address space.
static void guest_zero_range(void* p, size_t size) {
    madvise(p, size, MADV_DONTNEED);
}
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// Guest memory size constants — all powers of two.
const (
	Size16KB  uint64 = 1 << 14
	Size32KB  uint64 = 1 << 15
	Size64KB  uint64 = 1 << 16
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
)

// FaultKind classifies a MemFault.
type FaultKind uint8

const (
	FaultLoad     FaultKind = iota // guest attempted an out-of-bounds load
	FaultStore                     // guest attempted an out-of-bounds store
	FaultFetch                     // guest PC pointed outside memory
	FaultMisalign                  // access was not naturally aligned
)

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

// GuestMemory is a fixed-size, power-of-two, C mmap-backed address space
// for a single guest. It is safe to use from multiple goroutines only if
// the caller enforces its own synchronization (the scheduler does this).
//
// All loads and stores are:
//   - bounds-checked (one branch, near-never taken)
//   - alignment-checked (folded into the same expression)
//   - sandboxed (hostPtr cannot escape [base, base+size) by construction)
type GuestMemory struct {
	// base is the host address of the first byte of the slab.
	// Stored as uintptr because it is C memory: not a Go pointer,
	// never traced or moved by the GC. Arithmetic on it is safe for
	// the lifetime of the allocation.
	base uintptr

	// mask is size-1. Because size is a power of two, mask has all
	// bits set below the size boundary and all bits clear above it.
	// It serves dual purpose:
	//   containment: addr & mask ∈ [0, size-1] always
	//   bounds check: (addr | (addr+w-1)) & ^mask == 0 iff in-bounds
	mask uint64

	// size is kept for munmap and for ZeroRange bounds checking.
	size uint64

	// scratch is a reusable MemFault to avoid heap allocation on every fault.
	// Only one fault is ever live at a time (caller checks, then discards).
	scratch MemFault
}

// NewGuestMemory allocates a guest address space of the given size.
// size must be a power of two and must not exceed MaxGuestMemory.
// Physical memory is demand-paged; only virtual address space is
// reserved immediately.
func NewGuestMemory(size uint64) (*GuestMemory, error) {
	if size == 0 || size&(size-1) != 0 {
		return nil, fmt.Errorf("guestmem: size must be a non-zero power of two, got %d", size)
	}
	if size > MaxGuestMemory {
		return nil, fmt.Errorf("guestmem: size %d exceeds maximum %d", size, MaxGuestMemory)
	}

	ptr := C.guest_alloc(C.size_t(size))
	if ptr == nil {
		return nil, fmt.Errorf("guestmem: mmap failed for size %d", size)
	}

	m := &GuestMemory{
		base: uintptr(ptr),
		mask: size - 1,
		size: size,
	}

	// Finalizer ensures the C slab is released if the caller forgets
	// to call Free(). Explicit Free() is preferred in production code.
	runtime.SetFinalizer(m, (*GuestMemory).finalize)

	return m, nil
}

func (m *GuestMemory) finalize() {
	if m.base != 0 {
		C.guest_free(unsafe.Pointer(m.base), C.size_t(m.size))
		m.base = 0
	}
}

// Free releases the slab immediately. Safe to call multiple times.
// After Free, any use of the GuestMemory is undefined behavior.
func (m *GuestMemory) Free() {
	if m.base != 0 {
		C.guest_free(unsafe.Pointer(m.base), C.size_t(m.size))
		m.base = 0
		runtime.SetFinalizer(m, nil)
	}
}

// Size returns the guest address space size in bytes.
func (m *GuestMemory) Size() uint64 { return m.size }

// Mask returns size-1. ANDing any uint64 address with Mask() produces a
// valid in-bounds guest address by construction — the same guarantee the
// memory system uses internally for its containment invariant.
func (m *GuestMemory) Mask() uint64 { return m.mask }
func (m *GuestMemory) Base() uintptr { return m.base }

// RawSlice returns a zero-copy byte slice over the entire guest memory slab.
func (m *GuestMemory) RawSlice() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(m.base)), m.size)
}

// ZeroRange returns physical pages in [addr, addr+length) to the OS.
// Virtual address space is retained. Use to reclaim RAM after a guest
// process exits without reallocating the address space for reuse.
func (m *GuestMemory) ZeroRange(addr, length uint64) *MemFault {
	end := addr + length
	if end > m.size || end < addr { // second condition catches wraparound
		return &MemFault{addr, length, FaultStore}
	}
	C.guest_zero_range(unsafe.Pointer(m.base+uintptr(addr)), C.size_t(length))
	return nil
}

// ---------------------------------------------------------------------------
// Internal primitives — not exported; used only by load/store functions.
// ---------------------------------------------------------------------------

// check returns 0 if [addr, addr+width) is a valid naturally-aligned access,
// nonzero otherwise. Single expression combining both checks; no branch.
//
// Term 1: addr & (width-1)
//   Zero iff addr is a multiple of width (natural alignment).
//   Nonzero iff misaligned.
//
// Term 2: (addr | (addr+width-1)) & ^mask
//   addr+width-1 is the address of the last byte of the access.
//   OR-ing with addr propagates any out-of-range high bits from either end.
//   ANDing with ^mask isolates bits above the address space.
//   Zero iff both addr and addr+width-1 are within [0, size).
//
// Combined with OR: zero iff aligned AND in-bounds.
//
//go:nosplit
func (m *GuestMemory) check(addr, width uint64) uint64 {
	return (addr & (width - 1)) |
		((addr | (addr + width - 1)) & ^m.mask)
}

// hostPtr returns a host pointer to guest address addr.
// The mask guarantees the result is always within [base, base+size):
//
//	addr & mask ∈ [0, size-1]  →  base + (addr&mask) ∈ [base, base+size-1]
//
// Callers must have verified check(addr, width)==0 before dereferencing.
// The unsafe.Pointer cast is syntactically required by Go; it generates
// no machine code. This is safe because base is C memory and never moves.
//
//go:nosplit
func (m *GuestMemory) hostPtr(addr uint64) unsafe.Pointer {
	return unsafe.Pointer(m.base + uintptr(addr&m.mask))
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
	if m.check(addr, 1) != 0 {
		return 0, m.fault(addr, 1, FaultLoad)
	}
	return *(*uint8)(m.hostPtr(addr)), nil
}

// Load16 loads a 16-bit little-endian value from addr. addr must be 2-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Load16(addr uint64) (uint16, *MemFault) {
	if m.check(addr, 2) != 0 {
		return 0, m.fault(addr, 2, FaultLoad)
	}
	return *(*uint16)(m.hostPtr(addr)), nil
}

// Load32 loads a 32-bit little-endian value from addr. addr must be 4-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Load32(addr uint64) (uint32, *MemFault) {
	if m.check(addr, 4) != 0 {
		return 0, m.fault(addr, 4, FaultLoad)
	}
	return *(*uint32)(m.hostPtr(addr)), nil
}

// Load64 loads a 64-bit little-endian value from addr. addr must be 8-byte aligned.
//
//go:nosplit
func (m *GuestMemory) Load64(addr uint64) (uint64, *MemFault) {
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
	if f != nil { return 0, f }
	b1, f := m.Load8(addr + 1)
	if f != nil { return 0, f }
	return uint16(b0) | uint16(b1)<<8, nil
}

// Load32U loads a 32-bit value from an unaligned address.
func (m *GuestMemory) Load32U(addr uint64) (uint32, *MemFault) {
	v := uint32(0)
	for i := uint64(0); i < 4; i++ {
		b, f := m.Load8(addr + i)
		if f != nil { return 0, f }
		v |= uint32(b) << (i * 8)
	}
	return v, nil
}

// Load64U loads a 64-bit value from an unaligned address.
func (m *GuestMemory) Load64U(addr uint64) (uint64, *MemFault) {
	v := uint64(0)
	for i := uint64(0); i < 8; i++ {
		b, f := m.Load8(addr + i)
		if f != nil { return 0, f }
		v |= uint64(b) << (i * 8)
	}
	return v, nil
}

// Store16U stores a 16-bit value at an unaligned address.
func (m *GuestMemory) Store16U(addr uint64, v uint16) *MemFault {
	if f := m.Store8(addr, uint8(v)); f != nil { return f }
	return m.Store8(addr+1, uint8(v>>8))
}

// Store32U stores a 32-bit value at an unaligned address.
func (m *GuestMemory) Store32U(addr uint64, v uint32) *MemFault {
	for i := uint64(0); i < 4; i++ {
		if f := m.Store8(addr+i, uint8(v>>(i*8))); f != nil { return f }
	}
	return nil
}

// Store64U stores a 64-bit value at an unaligned address.
func (m *GuestMemory) Store64U(addr uint64, v uint64) *MemFault {
	for i := uint64(0); i < 8; i++ {
		if f := m.Store8(addr+i, uint8(v>>(i*8))); f != nil { return f }
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
	if m.check(addr, 2) != 0 {
		return 0, m.fault(addr, 2, FaultFetch)
	}
	return *(*uint16)(m.hostPtr(addr)), nil
}

// Fetch32 fetches a 32-bit instruction word. Requires 4-byte alignment.
//
//go:nosplit
func (m *GuestMemory) Fetch32(addr uint64) (uint32, *MemFault) {
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
	if f != nil { return 0, f }
	hi, f := m.Fetch16(addr + 2)
	if f != nil { return 0, f }
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
	end := addr + length
	if end > m.size || end < addr { // end < addr catches uint64 wraparound
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
	end := addr + length
	if end > m.size || end < addr {
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
	end := addr + length
	if end > m.size || end < addr {
		return &MemFault{addr, length, FaultStore}
	}
	dst := unsafe.Slice((*byte)(m.hostPtr(addr)), length)
	clear(dst)
	return nil
}
