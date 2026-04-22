package riscv

import "riscv/internal/syscalls"

// syscall_dispatcher address — obtained once at init. The JIT's IR
// emitter references this when lowering ECALL so the JIT block can
// directly CALL the SysV-ABI dispatcher defined in internal/syscalls.
//
// See Plan: "Fast Guest Syscalls — Hello World via Direct SYSCALL".
var syscallDispatcherAddr = syscalls.DispatchAddr()

// directSyscallDisabled lets benchmarks and tests force ECALL to
// fall back to the Go NoteChain path (emitReturn(pc+4, jitEcall))
// so we can compare the two side by side.
//
// This affects FUTURE block emissions only — already-compiled blocks
// retain whatever path they were compiled with. Callers that want
// a clean comparison should create a fresh JIT after toggling.
var directSyscallDisabled bool

// DisableDirectSyscall turns off the native ECALL fast path for the
// rest of the process (or until EnableDirectSyscall is called).
// Blocks compiled after this use the legacy Go ECALL path.
func DisableDirectSyscall() { directSyscallDisabled = true }

// EnableDirectSyscall turns the fast path back on.
func EnableDirectSyscall() { directSyscallDisabled = false }

// DirectSyscallEnabled reports whether new JIT blocks will use the
// native dispatcher.
func DirectSyscallEnabled() bool {
	return !directSyscallDisabled && syscallDispatcherAddr != 0
}

// currentSyscallDispatcherAddr returns the address to emit, or 0
// if the fast path is disabled.
func currentSyscallDispatcherAddr() uintptr {
	if directSyscallDisabled {
		return 0
	}
	return syscallDispatcherAddr
}

// inlineEcallEnabled gates the "inline ECALL" codegen introduced in
// Phase 5-B: when true, ECALL no longer terminates the JIT block.
// Instead the emitter continues past it, and the lowerer adds an
// in-block TESTQ+JZ on the dispatcher's return code (0 = continue,
// 1 = cold fallback to jitEcall via emitEpilogue + RET).
//
// Default off: Steps 1-5 wire the flag through every call site
// without changing emitted code. Step 6 flips the default.
// Flipping to false at any time restores today's behavior for
// subsequent blocks.
var inlineEcallEnabled bool

// SetInlineEcallEnabled toggles the inline-ECALL codegen for
// FUTURE block emissions. Already-compiled blocks retain whatever
// path they were compiled with.
func SetInlineEcallEnabled(on bool) { inlineEcallEnabled = on }

// InlineEcallEnabled reports whether the JIT will inline ECALL
// into the host block (dispatcher CALL + TESTQ+JZ) vs. the legacy
// unconditional-epilogue path.
func InlineEcallEnabled() bool { return inlineEcallEnabled }
