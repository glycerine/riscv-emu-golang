package riscv

import (
	"riscv/internal/syscalls"
	"riscv/ir"
)

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

// inlineEcallEnabled gates the Phase 5-B "inline ECALL" codegen
// (Option D): when true, lowerSyscall emits an inline TESTQ+JNZ that
// chain-exits to the post-ECALL AOT block on dispatcher success
// (RAX==0) and falls through to the cold-path epilogue on fallback
// (RAX!=0). The emitter still terminates the IR block at ECALL —
// the post-ECALL code is a separate AOT block entered via chain
// exit, preserving the "one block = one dirty epoch" invariant.
//
// Flipping to false at any time restores today's unconditional-
// epilogue path for subsequent block emissions.
var inlineEcallEnabled bool = true

func init() {
	ir.InlineSyscall = inlineEcallEnabled
}

// SetInlineEcallEnabled toggles the inline-ECALL codegen for
// FUTURE block emissions. Already-compiled blocks retain whatever
// path they were compiled with.
func SetInlineEcallEnabled(on bool) {
	inlineEcallEnabled = on
	ir.InlineSyscall = on
}

// InlineEcallEnabled reports whether the JIT will inline ECALL
// into the host block (dispatcher CALL + TESTQ+JNZ + ChainExit)
// vs. the legacy unconditional-epilogue path.
func InlineEcallEnabled() bool { return inlineEcallEnabled }
