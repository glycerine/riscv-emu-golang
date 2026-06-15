package riscv

import (
	"fmt"
	"os"

	"github.com/glycerine/riscv-emu-golang/abjit"
	"github.com/glycerine/riscv-emu-golang/internal/jitcall"
)

// abjitDispatch executes a JIT-compiled block via the abjit trampoline.
// Uses a persistent heap-allocated State buffer instead of the shadow
// register file page, eliminating the save/restore of guest memory.
//
// ── Native budget boundary ─────────────────────────────────────
//
// Inside JIT code, R15 is always the REMAINING guest-instruction budget for
// this dispatch. Native code reserves one instruction, or an entire fused group,
// before executing guest work. The Go boundary converts
// initialBudget-finalBudget into the retired-instruction delta.
//
// Outside JIT code (Go), cpu.riscvInstrBegun is the ABSOLUTE cumulative count
// of all guest instructions ever retired.
//
// The conversion happens here, at the dispatch boundary:
//
//	Go  →  s.IC = budget
//	       trampoline loads R15 from s.IC
//	       ── JIT code: R15 remaining budget ──
//	       SpillIC writes R15 back to s.IC
//	Go  ←  cpu.riscvInstrBegun += initialBudget - s.IC
//
// Chain exits preserve R15 across blocks (no re-zeroing).
// Gocall sequences (SpillIC/LoadIC) preserve R15 across Go callbacks.
func abjitDispatch(
	blk *compiledBlock,
	cpu *CPU,
	j *JIT,
	dcBase uintptr,
	dcMask, vBegin, segSize uint64,
	budget uint64,

) jitcall.Result {

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
	if debugJIT {
		fmt.Fprintf(os.Stderr, "ABJIT_STATE memBase=0x%x memMask=0x%x pc=0x%x fn=0x%x\n",
			s.MemBase, s.MemMask, cpu.pc, blk.fn)
	}
	s.DCBase = dcBase
	s.DCMask = dcMask
	s.VAddrBegin = vBegin
	s.SegSize = segSize
	initialBudget := budget
	s.IC = initialBudget

	abjit.CallJIT(blk.fn, s.RegFileBase())

	var icDelta uint64
	if s.IC <= initialBudget {
		icDelta = initialBudget - s.IC
	}
	cpu.riscvInstrBegun += icDelta

	res := jitcall.Result{
		PC:        s.PC,
		ICdelta:   icDelta,
		Status:    s.Status,
		FaultAddr: s.FaultAddr,
	}

	//vv("back from abjit.CallJIT, the call to the assembly trampoline. res = '%v'", &res)

	cpu.x = s.X
	if blk.hasFP {
		cpu.f = s.F
	}
	cpu.fcsr = s.FCSR

	return res
}
