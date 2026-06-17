package riscv

import "fmt"

// note.go — Plan 9-inspired virtual note system for guest exception delivery.
//
// Design principles (from Plan 9 notes):
//
//  1. Cooperative delivery — notes are delivered synchronously at instruction
//     boundaries (between step() calls), never asynchronously. No host OS
//     signals are used or disturbed. The Go runtime's own signal handlers
//     (SIGSEGV for nil-ptr, SIGURG for goroutine preemption, etc.) are
//     completely untouched.
//
//  2. Normal execution context — handlers run as ordinary Go functions in
//     the calling goroutine's stack, which is growable. There is no fixed
//     signal-stack size limit, no sigaltstack, no gsignal goroutine. Nesting
//     is bounded only by the goroutine stack (~1 GB by default).
//
//  3. String identity (Plan 9 conformance) — Note.Text is a human-readable
//     string that uniquely identifies the exception. Handlers that don't
//     recognise a Text can forward it without knowing its structure, giving
//     an infinite namespace and enabling independent handler layers to compose
//     without recompilation. Note.Cause gives O(1) dispatch for common cases.
//
//  4. Stack discipline — NoteChain delivers innermost-first. Handlers push
//     themselves onto the chain and pop on exit, mirroring Plan 9's manual
//     chaining but enforced by the chain rather than by each handler author.
//
//  5. Infinite nesting — a handler may call cpu.Run() or cpu.Step() (e.g. to
//     execute a guest signal trampoline), producing further notes, which are
//     delivered through the same chain. Depth is unbounded.
//
// Divergence from Plan 9:
//
//   Plan 9's noted(NCONT) is a syscall that restores execution from a
//   kernel-maintained register save area. We don't need it: step() returns
//   a Go error at a cooperative point, so resuming is just returning
//   NoteHandled and calling step() again. The CPU state lives in our CPU
//   struct, not in a kernel save area.

// ── RISC-V mcause values ──────────────────────────────────────────────────

const (
	CauseInsnAddrMisalign uint64 = 0
	CauseInsnFault        uint64 = 1
	CauseIllegalInsn      uint64 = 2
	CauseBreakpoint       uint64 = 3
	CauseLoadMisalign     uint64 = 4
	CauseLoadFault        uint64 = 5
	CauseMisalignStore    uint64 = 6
	CauseStoreFault       uint64 = 7
	CauseEcallU           uint64 = 8
	CauseEcallS           uint64 = 9
	CauseEcallM           uint64 = 11
)

// ── Note ──────────────────────────────────────────────────────────────────

// Note describes a guest exception delivered to a NoteChain.
//
// Text is the canonical Plan 9-style identity of the note. It is a
// human-readable string that uniquely describes the exception. Handlers that
// don't recognise a Text should return NoteForward without inspecting Cause,
// Tval, or PC — this allows independent handler layers to compose correctly
// even when new exception types are added without recompilation.
//
// Cause is the RISC-V mcause value for efficient dispatch on common cases.
// Tval is the mtval value (faulting address, bad instruction word, etc.).
// PC is the guest program counter at the time the exception occurred.
// InsnLen is the trapping instruction length when known; ECALL/EBREAK handlers
// use it to distinguish 32-bit EBREAK from 16-bit C.EBREAK.
type Note struct {
	Text    string // Plan 9-style string identity — forward if unrecognised
	Cause   uint64 // RISC-V mcause
	Tval    uint64 // RISC-V mtval
	PC      uint64 // guest PC at time of exception
	InsnLen uint8  // trapping instruction length, or 0 when unknown
}

func (n Note) String() string { return n.Text }

// noteFromStepErr converts a step() error into a Note.
// MemFault maps to the appropriate load/store mcause.
// ErrEbreak maps to CauseBreakpoint.
// ErrIllegalInstruction maps to CauseIllegalInsn.
// Any other error produces a note with Cause=^0 and the error string as Text.
// Static note text constants — avoid fmt.Sprintf on the hot path.
const (
	noteTextBreakpoint = "breakpoint"
	noteTextEcall      = "ecall"
	noteTextIllegal    = "illegal instruction"
	noteTextUnknown    = "unknown"
)

func noteFromStepErr(err error, pc uint64) Note {
	return noteFromStepErrWithTrap(err, pc, 0, 0)
}

func noteFromCPUError(cpu *CPU, err error) Note {
	if cpu == nil {
		return noteFromStepErr(err, 0)
	}
	return noteFromStepErrWithTrap(err, cpu.PC(), cpu.lastTrapCause, cpu.lastTrapInsnLen)
}

func noteFromStepErrWithTrap(err error, pc uint64, trapCause uint64, trapInsnLen uint8) Note {
	switch e := err.(type) {
	case *MemFault:
		cause, text := faultCauseAndText(e)
		return Note{
			Text:  text,
			Cause: cause,
			Tval:  e.Addr,
			PC:    pc,
		}
	}
	switch err {
	case ErrEbreak:
		if trapCause != CauseBreakpoint {
			trapCause = CauseBreakpoint
		}
		if trapInsnLen == 0 {
			trapInsnLen = 4
		}
		return Note{
			Text:    noteTextBreakpoint,
			Cause:   trapCause,
			PC:      pc,
			InsnLen: trapInsnLen,
		}
	case ErrEcall:
		if trapCause != CauseEcallU && trapCause != CauseEcallS && trapCause != CauseEcallM {
			trapCause = CauseEcallU
		}
		if trapInsnLen == 0 {
			trapInsnLen = 4
		}
		return Note{
			Text:    noteTextEcall,
			Cause:   trapCause,
			PC:      pc,
			InsnLen: trapInsnLen,
		}
	case ErrIllegalInstruction:
		return Note{
			Text:  noteTextIllegal,
			Cause: CauseIllegalInsn,
			PC:    pc,
		}
	default:
		return Note{
			Text:  noteTextUnknown,
			Cause: ^uint64(0),
			PC:    pc,
		}
	}
}

func faultCauseAndText(f *MemFault) (cause uint64, text string) {
	switch f.Kind {
	case FaultLoad:
		return CauseLoadFault,
			fmt.Sprintf("fault: load addr=0x%016X width=%d", f.Addr, f.Width)
	case FaultStore:
		return CauseStoreFault,
			fmt.Sprintf("fault: store addr=0x%016X width=%d", f.Addr, f.Width)
	case FaultFetch:
		return CauseInsnFault,
			fmt.Sprintf("fault: fetch addr=0x%016X width=%d", f.Addr, f.Width)
	default: // FaultMisalign — distinguish by context if we had it; use load
		return CauseLoadMisalign,
			fmt.Sprintf("fault: misalign addr=0x%016X width=%d", f.Addr, f.Width)
	}
}

// ── NoteDisposition ───────────────────────────────────────────────────────

// NoteDisposition is the value a NoteHandler returns to the NoteChain.
type NoteDisposition int

const (
	// NoteHandled means the handler resolved the exception. The CPU's pc
	// and registers may have been modified. Run() will resume execution
	// from cpu.PC(). (Analogous to Plan 9's noted(NCONT).)
	NoteHandled NoteDisposition = iota

	// NoteForward means this handler does not recognise the note and passes
	// it to the next handler toward the bottom of the chain. Handlers MUST
	// return NoteForward for any Text they do not recognise — this is the
	// Plan 9 contract that makes independent layers compose correctly.
	NoteForward

	// NoteFatal means the exception is unrecoverable. Run() returns the
	// original step() error to the caller. (Analogous to Plan 9's noted(NDFLT).)
	NoteFatal

	// NoteExit means the guest called exit(). cpu.ExitCode has the code.
	// Run loops return &ExitError{Code: cpu.ExitCode} to the caller.
	NoteExit
)

// ── NoteHandler ───────────────────────────────────────────────────────────

// NoteHandler is a function that handles a guest exception.
// It receives the CPU (whose registers it may freely modify) and the Note.
// It must return NoteHandled, NoteForward, or NoteFatal.
//
// A handler may call cpu.Step() or cpu.Run() to execute further guest
// instructions (e.g. a guest signal trampoline). Any exceptions raised
// during that execution are delivered through the same NoteChain, producing
// arbitrarily deep nesting.
type NoteHandler func(cpu *CPU, n Note) NoteDisposition

// ── NoteChain ─────────────────────────────────────────────────────────────

// NoteChain is an ordered stack of NoteHandlers.
// Exceptions are delivered innermost-first (most recently pushed first).
// If no handler claims the note, Deliver returns NoteFatal.
//
// NoteChain is not safe for concurrent use from multiple goroutines.
// Each CPU should have its own NoteChain.
type NoteChain struct {
	handlers []NoteHandler
}

// Push adds a handler as the new innermost handler.
// It will receive notes before any handler already in the chain.
func (nc *NoteChain) Push(h NoteHandler) {
	nc.handlers = append(nc.handlers, h)
}

// Pop removes the innermost handler.
// Panics if the chain is empty.
func (nc *NoteChain) Pop() {
	if len(nc.handlers) == 0 {
		panic("riscv: NoteChain.Pop on empty chain")
	}
	nc.handlers = nc.handlers[:len(nc.handlers)-1]
}

// Len returns the number of handlers currently in the chain.
func (nc *NoteChain) Len() int { return len(nc.handlers) }

// Deliver dispatches n to handlers innermost-first until one returns
// NoteHandled or NoteFatal, or all handlers return NoteForward.
// Returns NoteFatal if the chain is empty or all handlers forwarded.
func (nc *NoteChain) Deliver(cpu *CPU, n Note) NoteDisposition {
	for i := len(nc.handlers) - 1; i >= 0; i-- {
		if d := nc.handlers[i](cpu, n); d != NoteForward {
			return d
		}
	}
	return NoteFatal
}

// ── RunWithChain ──────────────────────────────────────────────────────────

// RunWithChain executes the CPU until a note is delivered and not handled,
// or until the chain returns NoteFatal. It is the bridge between step()'s
// typed errors and the NoteChain delivery protocol.
//
// On NoteHandled, execution resumes from cpu.PC() (which the handler may
// have modified, e.g. to skip the faulting instruction or jump to a
// trampoline). This loop continues indefinitely until NoteFatal or an
// unhandled error.
//
// The returned error is the original step() error that produced the fatal
// note, or nil if the CPU halted cleanly (which currently never happens —
// Run loops forever until a note goes unhandled).
func RunWithChain(cpu *CPU, nc *NoteChain) error {
	for {
		err := cpu.step()
		cpu.riscvInstrBegun++
		// Tohost polling: check after every instruction.
		// When watchAddr == 0 (the common case), this is a single
		// predicted-not-taken branch — negligible overhead.
		if cpu.watchAddr != 0 {
			if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
				return &ExitError{Code: tohostExitCode(v)}
			}
		}
		if err == nil {
			continue
		}
		n := noteFromCPUError(cpu, err)
		switch nc.Deliver(cpu, n) {
		case NoteHandled:
			continue
		case NoteExit:
			return &ExitError{Code: cpu.ExitCode}
		default:
			return err
		}
	}
}
