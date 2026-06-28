//go:build !cgo || !amd64 || windows

package riscv

import "github.com/glycerine/riscv-emu-golang/internal/jitcall"

const useRv8SandboxTrampoline = false

func sandboxRv8Call(fn uintptr, cpu *CPU,
	regFile, stackTop uintptr,
	dcBase uintptr, dcMask, vBegin, segSize uint64,
	budget uint64) jitcall.Result {

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
