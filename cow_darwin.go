//go:build darwin

package riscv

// Copy-on-write remapping on Darwin via the Mach VM API.
//
// mach_vm_remap duplicates a range of virtual memory from one
// address to another within the current task. With copy=TRUE
// the new mapping is copy-on-write: writes on either side
// trigger the kernel to allocate a private page; the other side
// retains its fork-time view. With copy=FALSE the mapping is
// shared (changes visible both ways). For Phase 2c Machine.Clone
// we always use the CoW form.
//
// The matching deallocator is mach_vm_deallocate. POSIX munmap
// also works (Darwin's munmap calls mach_vm_deallocate for Mach
// VM regions), but using the matched API is cleaner.

/*
#include <mach/mach.h>
#include <mach/mach_vm.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// COWRemap creates a copy-on-write remap of [sourceAddr, sourceAddr+size)
// within the current task. Returns the target virtual address on success.
//
// Writes through the returned address are private to the clone; the source
// region is unaffected. After either side writes to a given page, the pages
// are decoupled — writes to the source from that point on are not observed
// by the clone and vice versa.
//
// Release with COWUnmap when done.
func COWRemap(size uint64, sourceAddr unsafe.Pointer) (unsafe.Pointer, error) {
	var targetAddr C.mach_vm_address_t
	var curProt, maxProt C.vm_prot_t

	kr := C.mach_vm_remap(
		C.mach_task_self_,
		&targetAddr,
		C.mach_vm_size_t(size),
		0,                    // alignment mask: 0 = no alignment constraint
		C.VM_FLAGS_ANYWHERE,  // let the kernel pick the target address
		C.mach_task_self_,
		C.mach_vm_address_t(uintptr(sourceAddr)),
		C.boolean_t(1),       // copy=TRUE ⇒ copy-on-write (not shared)
		&curProt,
		&maxProt,
		C.VM_INHERIT_DEFAULT,
	)

	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("mach_vm_remap failed with error code: %d", int(kr))
	}

	return unsafe.Pointer(uintptr(targetAddr)), nil
}

// COWUnmap releases a region returned by COWRemap.
func COWUnmap(addr unsafe.Pointer, size uint64) error {
	kr := C.mach_vm_deallocate(
		C.mach_task_self_,
		C.mach_vm_address_t(uintptr(addr)),
		C.mach_vm_size_t(size),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("mach_vm_deallocate failed with error code: %d", int(kr))
	}
	return nil
}
