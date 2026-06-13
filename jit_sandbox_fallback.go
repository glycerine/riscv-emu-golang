//go:build !cgo || !amd64

package riscv

import "github.com/glycerine/riscv-emu-golang/internal/jitcall"

const useRv8SandboxTrampoline = false

func sandboxRv8Call(fn uintptr, cpu *CPU,
	regFile, stackTop uintptr,
	dcBase uintptr, dcMask, vBegin, segSize uint64) jitcall.Result {

	if dcBase != 0 || dcMask != 0 || vBegin != 0 || segSize != 0 {
		return jitcall.CallAOT(fn, &cpu.x, &cpu.f, &cpu.fcsr,
			cpu.mem.Base(), cpu.mem.Mask(),
			dcBase, dcMask, vBegin, segSize)
	}
	return jitcall.Call(fn, &cpu.x, &cpu.f, &cpu.fcsr,
		cpu.mem.Base(), cpu.mem.Mask())
}
