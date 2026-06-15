package riscv

import (
	"reflect"
	"sync"

	"github.com/glycerine/riscv-emu-golang/abjit"
)

const (
	abjitEcallContinue = 0
	abjitEcallExit     = 1
)

type abjitEcallContext struct {
	cpu           *CPU
	state         *abjit.State
	initialBudget uint64
	accountedIC   uint64
}

var (
	abjitEcallMu     sync.Mutex
	abjitEcallActive *abjitEcallContext
)

func jitInlineEcallAddr() uintptr {
	return reflect.ValueOf(jitInlineEcall).Pointer()
}

func (c *abjitEcallContext) accountToState() {
	if c == nil || c.cpu == nil || c.state == nil {
		return
	}
	if c.state.IC > c.initialBudget {
		return
	}
	retired := c.initialBudget - c.state.IC
	if retired <= c.accountedIC {
		return
	}
	c.cpu.riscvInstrBegun += retired - c.accountedIC
	c.accountedIC = retired
}

// jitInlineEcall is called from ABJIT native code through the gocall
// trampoline. It runs the installed guest OS, then tells native code whether
// it can continue at pc+4 or must return to Go.
func jitInlineEcall() {
	ctx := abjitEcallActive
	if ctx == nil || ctx.cpu == nil || ctx.state == nil {
		return
	}
	ctx.accountToState()

	cpu := ctx.cpu
	s := ctx.state
	resumePC := s.PC

	cpu.x = s.X
	cpu.x[0] = 0
	cpu.f = s.F
	cpu.fcsr = s.FCSR
	cpu.pc = resumePC

	n := noteFromStepErr(ErrEcall, resumePC)
	disp := cpu.Notes.Deliver(cpu, n)

	cpu.x[0] = 0
	s.X = cpu.x
	s.F = cpu.f
	s.FCSR = cpu.fcsr
	s.PC = cpu.pc

	switch disp {
	case NoteHandled:
		if cpu.pc == resumePC {
			s.Status = jitOK
			s.EcallAction = abjitEcallContinue
			return
		}
		s.Status = jitOK
		s.EcallAction = abjitEcallExit
	case NoteExit:
		s.Status = jitEcallExit
		s.EcallAction = abjitEcallExit
	default:
		s.Status = jitEcall
		s.EcallAction = abjitEcallExit
	}
}
