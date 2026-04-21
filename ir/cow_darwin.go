//go:build darwin

package ir

// The Mach VM mach_vm_remap approach on MacOS/darwin
// to get CoW memory.
//
// If you are looking for true Copy-on-Write (CoW)
// between specific memory regions without using a
// File Descriptor at all, Darwin’s Mach kernel
// provides a specialized API called mach_vm_remap.
//
// This is what Darwin uses internally for high-performance
// memory sharing. It allows you to "copy" a range of
// virtual memory from one address to another (or one
// process to another) using CoW, so physical pages
// are only duplicated when one side writes.

/*
#include <mach/mach.h>
#include <mach/mach_vm.h>
*/
import "C"

import (
	"fmt"
	//"log"
	//"unsafe"
)

func MachVMRemap(size uint64, sourceAddr uintptr) (uintptr, error) {
	var targetAddr C.mach_vm_address_t
	var curProt, maxProt C.vm_prot_t

	// mach_vm_remap arguments:
	// 1. Target task (self)
	// 2. Pointer to target address (0 + VM_FLAGS_ANYWHERE means kernel picks)
	// 3. Size of region
	// 4. Alignment mask
	// 5. Flags (ANYWHERE to let OS find a slot)
	// 6. Source task (self)
	// 7. Source address to copy from
	// 8. Copy flag (FALSE = Shared, TRUE = Copy/CoW)
	// 9. Pointer to return current protection
	// 10. Pointer to return max protection
	// 11. Inheritance (default)

	kr := C.mach_vm_remap(
		C.mach_task_self_,
		&targetAddr,
		C.mach_vm_size_t(size),
		0,
		C.VM_FLAGS_ANYWHERE,
		C.mach_task_self_,
		C.mach_vm_address_t(sourceAddr),
		C.boolean_t(0), // Set to 1 for an actual physical copy; 0 for CoW/Shared
		&curProt,
		&maxProt,
		C.VM_INHERIT_DEFAULT,
	)

	if kr != C.KERN_SUCCESS {
		return 0, fmt.Errorf("mach_vm_remap failed with error code: %d", int(kr))
	}

	return uintptr(targetAddr), nil
}

/* example:
func main() {
	// Example: Imagine we have some memory at a pointer
	// This is a conceptual demonstration
	data := []byte("Hello, Mach CoW!")
	sourcePtr := uintptr(unsafe.Pointer(&data[0]))
	size := uint64(len(data))

	remappedAddr, err := MachVMRemap(size, sourcePtr)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Source Address: %p\n", unsafe.Pointer(sourcePtr))
	fmt.Printf("Remapped Address: 0x%x\n", remappedAddr)
}
*/
