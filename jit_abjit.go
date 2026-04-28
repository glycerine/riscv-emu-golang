package riscv

import (
	"riscv/abjit"
	"riscv/internal/jitcall"
)

// abjitDispatch executes a JIT-compiled block via the abjit trampoline.
// Uses a persistent heap-allocated State buffer instead of the shadow
// register file page, eliminating the save/restore of guest memory.
func abjitDispatch(
	blk *compiledBlock,
	cpu *CPU,
	j *JIT,
	dcBase uintptr,
	dcMask, vBegin, segSize uint64,

) jitcall.Result {

	//vv("top abjitDispatch()")
	//defer vv("end abjitDispatch()")

	if j.abjitState == nil {
		j.abjitState = abjit.NewState()
	}
	s := j.abjitState

	s.X = cpu.x
	if blk.hasFP {
		s.F = cpu.f
	}
	s.FCSR = cpu.fcsr
	s.MemBase = cpu.mem.Base()
	s.MemMask = cpu.mem.Mask()
	s.DCBase = dcBase
	s.DCMask = dcMask
	s.VAddrBegin = vBegin
	s.SegSize = segSize
	s.IC = cpu.cycle

	//vv("about to call abjit.CallJIT, the assembly trampoline")

	abjit.CallJIT(blk.fn, s.RegFileBase())

	res := jitcall.Result{
		PC:        s.PC,
		IC:        s.IC,
		Status:    s.Status,
		FaultAddr: s.FaultAddr,
		Cycles:    s.Cycles,
	}

	//vv("back from abjit.CallJIT, the call to the assembly trampoline. res = '%v'", &res)

	cpu.x = s.X
	if blk.hasFP {
		cpu.f = s.F
	}
	cpu.fcsr = s.FCSR

	return res
}
