//go:build darwin || linux

package ir

import (
	"syscall"
	"testing"
	"unsafe"
)

// pageSize is the OS page size. All CoW tests use multiples of this.
var pageSize = uint64(syscall.Getpagesize())

// mustAllocateSource mmaps a private anonymous region of the given size
// (must be multiple of pageSize), fills every byte with fill, and registers
// munmap on cleanup. Returns the base address as uintptr.
func mustAllocateSource(t *testing.T, size uint64, fill byte) uintptr {
	t.Helper()
	b, err := syscall.Mmap(-1, 0, int(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatalf("syscall.Mmap: %v", err)
	}
	for i := range b {
		b[i] = fill
	}
	t.Cleanup(func() { _ = syscall.Munmap(b) })
	return uintptr(unsafe.Pointer(&b[0]))
}

// readByte reads a single byte at base+offset.
func readByte(base uintptr, offset uint64) byte {
	return *(*byte)(unsafe.Pointer(base + uintptr(offset)))
}

// writeByte writes a single byte at base+offset.
func writeByte(base uintptr, offset uint64, v byte) {
	*(*byte)(unsafe.Pointer(base + uintptr(offset))) = v
}

func TestCoW_BasicRemap(t *testing.T) {
	size := 2 * pageSize
	src := mustAllocateSource(t, size, 0xAA)

	child, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(child, size) })

	// Spot-check: bytes across the region should be 0xAA.
	for _, off := range []uint64{0, 1, pageSize - 1, pageSize, pageSize + 17, size - 1} {
		if got := readByte(child, off); got != 0xAA {
			t.Errorf("child[%d]=0x%02x, want 0xAA", off, got)
		}
	}
}

func TestCoW_WriteToChildIsolatesFromParent(t *testing.T) {
	size := 2 * pageSize
	src := mustAllocateSource(t, size, 0xAA)

	child, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(child, size) })

	// Write a distinct byte to the first byte of the child.
	writeByte(child, 0, 0xBB)

	// Parent must still read the original 0xAA — this is THE CoW
	// correctness invariant. (On darwin this would fail if the
	// mach_vm_remap copy flag were FALSE = shared.)
	if got := readByte(src, 0); got != 0xAA {
		t.Errorf("parent[0]=0x%02x after child write, want 0xAA (CoW violated)", got)
	}
	// And the child retains the new value.
	if got := readByte(child, 0); got != 0xBB {
		t.Errorf("child[0]=0x%02x, want 0xBB", got)
	}
	// Bytes the child didn't write remain 0xAA.
	if got := readByte(child, 1); got != 0xAA {
		t.Errorf("child[1]=0x%02x, want 0xAA", got)
	}
}

func TestCoW_ParentWriteAfterChildWroteStaysChildIsolated(t *testing.T) {
	size := 2 * pageSize
	src := mustAllocateSource(t, size, 0xAA)

	child, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(child, size) })

	// Child writes first — its page becomes private.
	writeByte(child, 0, 0xBB)

	// Parent then writes the same offset. Child must still read 0xBB.
	writeByte(src, 0, 0xCC)
	if got := readByte(child, 0); got != 0xBB {
		t.Errorf("child[0]=0x%02x after parent write, want 0xBB (page should be private)", got)
	}
	if got := readByte(src, 0); got != 0xCC {
		t.Errorf("parent[0]=0x%02x, want 0xCC", got)
	}
}

func TestCoW_ParentWriteBeforeChildWrite_DocumentBehavior(t *testing.T) {
	// Subtle case: the parent writes BEFORE the child writes to that page.
	// On typical CoW (memfd + MAP_PRIVATE on linux; mach_vm_remap with copy
	// on darwin), the semantics are "shared until either side writes".
	// linux MAP_PRIVATE: child sees the parent's pre-write snapshot IF the
	// page was already populated into the memfd at remap time (which it is,
	// via the SHARED->copy->unmap->PRIVATE dance). darwin mach_vm_remap with
	// copy=TRUE: child also sees fork-time snapshot.
	//
	// This test documents what actually happens rather than asserting a
	// particular choice — the correctness we require is post-child-write
	// isolation, covered by the other tests.
	size := 2 * pageSize
	src := mustAllocateSource(t, size, 0xAA)

	child, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(child, size) })

	// Parent writes before child touches this page.
	writeByte(src, 0, 0xCC)

	got := readByte(child, 0)
	t.Logf("parent-pre-write-then-child-read: child[0]=0x%02x (0xAA = fork-time snapshot, 0xCC = still shared)", got)
	// We require at least that the child is NOT corrupted and is readable.
	if got != 0xAA && got != 0xCC {
		t.Errorf("child[0]=0x%02x unexpected", got)
	}
}

func TestCoW_MultipleForks(t *testing.T) {
	size := 2 * pageSize
	src := mustAllocateSource(t, size, 0xAA)

	childA, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap A: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(childA, size) })

	childB, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap B: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(childB, size) })

	// Write distinct values to A, B, and parent.
	writeByte(childA, 0, 0x11)
	writeByte(childB, 0, 0x22)
	writeByte(src, 0, 0x33)

	if got := readByte(childA, 0); got != 0x11 {
		t.Errorf("childA[0]=0x%02x, want 0x11", got)
	}
	if got := readByte(childB, 0); got != 0x22 {
		t.Errorf("childB[0]=0x%02x, want 0x22", got)
	}
	if got := readByte(src, 0); got != 0x33 {
		t.Errorf("parent[0]=0x%02x, want 0x33", got)
	}
}

func TestCoW_ReleaseOneForkLeavesOthersValid(t *testing.T) {
	size := 2 * pageSize
	src := mustAllocateSource(t, size, 0xAA)

	childA, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap A: %v", err)
	}
	childB, err := COWRemap(size, src)
	if err != nil {
		t.Fatalf("COWRemap B: %v", err)
	}
	t.Cleanup(func() { _ = COWUnmap(childB, size) })

	writeByte(childA, 0, 0x11)
	writeByte(childB, 0, 0x22)

	// Release A.
	if err := COWUnmap(childA, size); err != nil {
		t.Fatalf("COWUnmap A: %v", err)
	}

	// Parent and B still readable and distinct.
	if got := readByte(src, 0); got != 0xAA {
		t.Errorf("parent[0]=0x%02x after A release, want 0xAA", got)
	}
	if got := readByte(childB, 0); got != 0x22 {
		t.Errorf("childB[0]=0x%02x after A release, want 0x22", got)
	}
}
