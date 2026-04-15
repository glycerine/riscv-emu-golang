package riscv

import (
	"fmt"
	"strings"
)

// os.go — Guest OS personality layer.
//
// An OS personality is a NoteHandler that intercepts ECALL notes and
// dispatches them to registered syscall handlers. It installs itself
// on the CPU's NoteChain and can be stacked with other handlers.
//
// Syscall ABI (Linux RISC-V convention):
//
//	a7 (x17) = syscall number
//	a0 (x10) = arg0 / return value
//	a1 (x11) = arg1
//	a2 (x12) = arg2
//	a3 (x13) = arg3
//	a4 (x14) = arg4
//	a5 (x15) = arg5
//
// This matches the Linux/RISC-V syscall convention used by riscv-tests
// (syscall 93 = exit, a0=exit code) and musl libc.

// SyscallArgs holds the arguments for a guest syscall.
type SyscallArgs struct {
	Num  uint64 // syscall number (a7)
	A0   uint64 // arg0 (a0)
	A1   uint64 // arg1 (a1)
	A2   uint64 // arg2 (a2)
	A3   uint64 // arg3 (a3)
	A4   uint64 // arg4 (a4)
	A5   uint64 // arg5 (a5)
}

// SyscallResult is the return value written back to a0.
// Negative values signal errors (Linux errno convention).
type SyscallResult int64

// SyscallHandler handles one specific syscall number.
// It returns the value to be placed in a0, and whether it handled the call.
// If handled=false, the OS layer tries the next registered handler.
type SyscallHandler func(cpu *CPU, args SyscallArgs) (result SyscallResult, handled bool)

// EcallHandler handles any ECALL that doesn't match a specific syscall —
// e.g. for RISC-V environments that use ECALL for pass/fail signalling
// rather than Linux syscalls.
type EcallHandler func(cpu *CPU, args SyscallArgs) NoteDisposition

// OS is a guest OS personality. It intercepts ECALL notes and dispatches
// to registered syscall handlers. Unrecognised ECALLs are forwarded down
// the NoteChain.
//
// Install it by pushing its Handle method onto a CPU's NoteChain:
//
//	os := riscv.NewOS()
//	os.HandleSyscall(93, exitHandler)
//	cpu.Notes.Push(os.Handle)
//
// The OS can be stacked with other personalities — e.g. a debug layer above
// and a guest kernel below — because each layer uses NoteForward for notes
// it doesn't own.
type OS struct {
	syscalls map[uint64]SyscallHandler // keyed by syscall number
	ecall    EcallHandler              // fallback for unrecognised syscall numbers
}

// NewOS creates an OS personality with no handlers installed.
func NewOS() *OS {
	return &OS{syscalls: make(map[uint64]SyscallHandler)}
}

// HandleSyscall registers a handler for a specific syscall number.
// Overwrites any previously registered handler for that number.
func (o *OS) HandleSyscall(num uint64, h SyscallHandler) {
	o.syscalls[num] = h
}

// HandleEcall registers a fallback handler for ECALLs whose syscall number
// has no specific handler. Replaces any previously set fallback.
func (o *OS) HandleEcall(h EcallHandler) {
	o.ecall = h
}

// Handle is the NoteHandler method for this OS personality.
// Install it with: cpu.Notes.Push(os.Handle)
//
// It intercepts ECALL notes (CauseEcallU/S/M), dispatches to the registered
// syscall handler, writes the result back to a0, and returns NoteHandled.
// All other notes (faults, breakpoints, illegal instructions) are forwarded.
func (o *OS) Handle(cpu *CPU, n Note) NoteDisposition {
	switch n.Cause {
	case CauseEcallU, CauseEcallS, CauseEcallM:
		// fall through to dispatch
	default:
		return NoteForward // not our business
	}

	args := SyscallArgs{
		Num: cpu.Reg(17), // a7
		A0:  cpu.Reg(10), // a0
		A1:  cpu.Reg(11),
		A2:  cpu.Reg(12),
		A3:  cpu.Reg(13),
		A4:  cpu.Reg(14),
		A5:  cpu.Reg(15),
	}

	// Try specific syscall handler first
	if h, ok := o.syscalls[args.Num]; ok {
		result, handled := h(cpu, args)
		if handled {
			cpu.SetReg(10, uint64(result))
			return NoteHandled
		}
	}

	// Try fallback ecall handler
	if o.ecall != nil {
		d := o.ecall(cpu, args)
		if d != NoteForward {
			return d // handler claimed it (NoteHandled or NoteFatal)
		}
		// fallback forwarded — fall through to ENOSYS
	}

	// Unknown syscall — return -ENOSYS and continue
	cpu.SetReg(10, ^uint64(37)) // -ENOSYS = -38 as two's complement uint64
	return NoteHandled
}

// ── Built-in syscall handlers ─────────────────────────────────────────────

// ExitError is returned by RunWithChain when the guest calls exit().
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.Code)
}

// LinuxExit handles syscall 93 (exit) and 94 (exit_group).
// When the guest calls exit(code), Run() returns an *ExitError.
// Install with: os.HandleSyscall(93, riscv.LinuxExit)
//              os.HandleSyscall(94, riscv.LinuxExit)
func LinuxExit(cpu *CPU, args SyscallArgs) (SyscallResult, bool) {
	// We signal exit by panicking with a sentinel value that RunWithChain
	// catches. This avoids threading an exit channel through the call stack.
	panic(&ExitError{Code: int(int32(args.A0))})
}

// RiscvTestsEcall handles the riscv-tests pass/fail ECALL convention:
//
//	a7 = 93, a0 = 0       → PASS (returns ExitError{Code: 0})
//	a7 = 93, a0 = non-zero → FAIL (returns ExitError{Code: a0})
//
// The test number is encoded as (a0 >> 1) when a0 is odd.
// Install as the ecall fallback:
//
//	os.HandleEcall(riscv.RiscvTestsEcall)
func RiscvTestsEcall(cpu *CPU, args SyscallArgs) NoteDisposition {
	if args.Num == 93 {
		panic(&ExitError{Code: int(args.A0)})
	}
	return NoteForward
}

// ── LinuxWrite — syscall 64 ───────────────────────────────────────────────

// WriteFunc is called by LinuxWriteHandler when the guest writes to stdout
// or stderr. buf is the guest data. Returns number of bytes written.
type WriteFunc func(fd int, buf []byte) int

// LinuxWriteHandler returns a SyscallHandler for syscall 64 (write) that
// calls w for fd 1 (stdout) and fd 2 (stderr), and ignores all other fds.
func LinuxWriteHandler(w WriteFunc) SyscallHandler {
	return func(cpu *CPU, args SyscallArgs) (SyscallResult, bool) {
		fd  := int(args.A0)
		gva := args.A1
		n   := args.A2
		if fd != 1 && fd != 2 {
			return SyscallResult(n), true // pretend success for other fds
		}
		buf := make([]byte, n)
		if f := cpu.mem.ReadBytes(gva, buf); f != nil {
			return -1, true
		}
		written := w(fd, buf)
		return SyscallResult(written), true
	}
}

// ── RunWithOS — convenience entry point ───────────────────────────────────

// RunWithOS is a convenience wrapper that creates a minimal Linux-like OS
// personality supporting exit (syscall 93/94) and the riscv-tests ECALL
// convention, installs it, runs the CPU, and returns nil on clean exit
// or an *ExitError on guest exit().
//
// For finer control, construct an OS manually and push it onto cpu.Notes.
func RunWithOS(cpu *CPU) (exitCode int, err error) {
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	defer cpu.Notes.Pop()

	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*ExitError); ok {
				exitCode = ex.Code
				err = nil
				return
			}
			panic(r) // re-panic anything that isn't an ExitError
		}
	}()

	err = RunWithChain(cpu, &cpu.Notes)
	return
}

// ── Fault personality — optional fault note formatting ────────────────────

// FaultHandler returns a NoteHandler that formats memory fault notes as
// human-readable strings and calls onFault. Useful for debugging.
// Returns NoteForward after calling onFault so the chain continues.
func FaultHandler(onFault func(n Note)) NoteHandler {
	return func(cpu *CPU, n Note) NoteDisposition {
		switch n.Cause {
		case CauseLoadFault, CauseStoreFault, CauseLoadMisalign,
			CauseMisalignStore, CauseInsnFault, CauseInsnAddrMisalign:
			onFault(n)
		}
		return NoteForward
	}
}

// ── Debug personality — log all notes ────────────────────────────────────

// LogHandler returns a NoteHandler that logs every note by calling log,
// then forwards unconditionally. Useful as the outermost layer for tracing.
func LogHandler(log func(n Note)) NoteHandler {
	return func(cpu *CPU, n Note) NoteDisposition {
		log(n)
		return NoteForward
	}
}

// ── NoteText helpers — for handler authors ────────────────────────────────

// IsEcall reports whether the note is an ECALL of any privilege level.
func IsEcall(n Note) bool {
	return n.Cause == CauseEcallU || n.Cause == CauseEcallS || n.Cause == CauseEcallM
}

// IsFault reports whether the note is a memory fault of any kind.
func IsFault(n Note) bool {
	return strings.HasPrefix(n.Text, "fault:")
}

// IsBreakpoint reports whether the note is an EBREAK.
func IsBreakpoint(n Note) bool {
	return n.Cause == CauseBreakpoint
}
