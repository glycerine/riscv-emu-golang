//go:build cgo && amd64 && !windows

package riscv

/*
#include "jit_sandbox.h"
*/
import "C"
import (
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/internal/jitcall"
)

// TEMPORARY: bypass sandbox to isolate old-vs-new divergence.
// Set to true to use the CGO sandbox trampoline; false uses the old Go asm trampoline.
const useRv8SandboxTrampoline = true

func sandboxRv8Call(fn uintptr, cpu *CPU,
	regFile, stackTop uintptr,
	dcBase uintptr, dcMask, vBegin, segSize uint64,
	budget uint64) jitcall.Result {

	if !useRv8SandboxTrampoline {
		resvAddr := cpu.resvAddr
		var resvValid uint64
		if cpu.resvValid {
			resvValid = 1
		}
		var res jitcall.Result
		if dcBase != 0 || dcMask != 0 || vBegin != 0 || segSize != 0 {
			res = jitcall.CallAOTResv(fn, &cpu.x, &cpu.f, &cpu.fcsr,
				cpu.mem.Base(), cpu.mem.Mask(),
				dcBase, dcMask, vBegin, segSize,
				&resvAddr, &resvValid, budget)
		} else {
			res = jitcall.CallResv(fn, &cpu.x, &cpu.f, &cpu.fcsr,
				cpu.mem.Base(), cpu.mem.Mask(),
				&resvAddr, &resvValid, budget)
		}
		cpu.resvAddr = resvAddr
		cpu.resvValid = resvValid != 0
		res.SourceBlock = 0
		return res
	}

	resvAddr := C.uint64_t(cpu.resvAddr)
	var resvValid C.uint64_t
	if cpu.resvValid {
		resvValid = 1
	}
	r := C.jit_sandbox_call(
		C.uintptr_t(fn),
		(*C.uint64_t)(unsafe.Pointer(&cpu.x[0])),
		(*C.uint64_t)(unsafe.Pointer(&cpu.f[0])),
		(*C.uint32_t)(unsafe.Pointer(&cpu.fcsr)),
		C.uintptr_t(cpu.mem.Base()), C.uint64_t(cpu.mem.Mask()),
		C.uintptr_t(regFile), C.uintptr_t(stackTop),
		C.uintptr_t(dcBase), C.uint64_t(dcMask),
		C.uint64_t(vBegin), C.uint64_t(segSize),
		(*C.uint64_t)(unsafe.Pointer(&resvAddr)),
		(*C.uint64_t)(unsafe.Pointer(&resvValid)),
		C.uint64_t(budget),
	)
	cpu.resvAddr = uint64(resvAddr)
	cpu.resvValid = resvValid != 0
	return jitcall.Result{
		PC:        uint64(r.pc),
		Status:    uint64(r.status),
		FaultAddr: uint64(r.fault_addr),
		ICdelta:   uint64(r.ic),
	}
}
