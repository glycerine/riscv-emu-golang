package riscv

import (
	"github.com/glycerine/riscv-emu-golang/abjit"
	"github.com/glycerine/riscv-emu-golang/internal/jitcall"
)

// abjitDispatch executes a JIT-compiled block via the abjit trampoline.
// Uses a persistent heap-allocated State buffer instead of the shadow
// register file page, eliminating the save/restore of guest memory.
//
// ── IC (instruction counter) boundary ──────────────────────────
//
// Inside JIT code, R15 is a RELATIVE instruction counter: it starts
// at 0 for each dispatch and increments (INCQ R15) once per guest
// instruction. Budget checks compare R15 against a fixed threshold,
// so the relative basis is essential for correctness.
//
// Outside JIT code (Go), cpu.riscvInstrBegun is the ABSOLUTE cumulative count
// of all guest instructions ever retired.
//
// The conversion happens here, at the dispatch boundary:
//
//	Go  →  s.IC = 0                        (relative origin)
//	       trampoline loads R15 from s.IC   (R15 = 0)
//	       ── JIT code: R15 relative ──
//	       SpillIC writes R15 back to s.IC
//	Go  ←  cpu.riscvInstrBegun += s.IC               (relative → absolute)
//
// Chain exits preserve R15 across blocks (no re-zeroing).
// Gocall sequences (SpillIC/LoadIC) preserve R15 across Go callbacks.
// The defer below guarantees the += fires even on panic (exit syscall).
func abjitDispatch(
	blk *compiledBlock,
	cpu *CPU,
	j *JIT,
	dcBase uintptr,
	dcMask, vBegin, segSize uint64,

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
	s.DCBase = dcBase
	s.DCMask = dcMask
	s.VAddrBegin = vBegin
	s.SegSize = segSize
	s.IC = 0 // relative origin — trampoline loads this into R15

	abjit.CallJIT(blk.fn, s.RegFileBase())

	cpu.riscvInstrBegun += s.IC // relative → absolute

	res := jitcall.Result{
		PC:        s.PC,
		ICdelta:   s.IC,
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
