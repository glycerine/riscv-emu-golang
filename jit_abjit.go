package riscv

import (
	"riscv/abjit"
	"riscv/internal/jitcall"
	"unsafe"
)

const abjitShadowSize = 568

// abjitDispatch executes a JIT-compiled block via the abjit trampoline.
// It copies CPU state to the shadow register file page, executes the
// block, reads the result, copies state back, and restores the guest
// memory that was under the shadow.
func abjitDispatch(fn uintptr, cpu *CPU) jitcall.Result {
	rf := cpu.mem.RegFileBase()

	var saved [abjitShadowSize]byte
	saved = *(*[abjitShadowSize]byte)(unsafe.Pointer(rf))

	*(*[32]uint64)(unsafe.Pointer(rf)) = cpu.x
	*(*[32]uint64)(unsafe.Pointer(rf + 256)) = cpu.f
	*(*uint32)(unsafe.Pointer(rf + 512)) = cpu.fcsr
	*(*uintptr)(unsafe.Pointer(rf + 520)) = cpu.mem.Base()
	*(*uint64)(unsafe.Pointer(rf + 528)) = cpu.mem.Mask()

	abjit.CallJIT(fn, rf)

	res := jitcall.Result{
		PC:        *(*uint64)(unsafe.Pointer(rf + 536)),
		IC:        *(*uint64)(unsafe.Pointer(rf + 544)),
		Status:    *(*uint64)(unsafe.Pointer(rf + 552)),
		FaultAddr: *(*uint64)(unsafe.Pointer(rf + 560)),
	}

	cpu.x = *(*[32]uint64)(unsafe.Pointer(rf))
	cpu.f = *(*[32]uint64)(unsafe.Pointer(rf + 256))
	cpu.fcsr = *(*uint32)(unsafe.Pointer(rf + 512))

	*(*[abjitShadowSize]byte)(unsafe.Pointer(rf)) = saved

	return res
}
