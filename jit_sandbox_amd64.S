/*
 * jit_trampoline_asm — switches RSP to the sandbox stack (C mmap),
 * calls a JIT-compiled native block, then switches back.
 *
 * Called from jit_sandbox.c on the system stack (g0) after the CGO
 * transition. Callee-saved registers are saved/restored on the system
 * stack (safe — this is C's stack, not the Go goroutine stack).
 *
 * The system RSP is stashed at sret[32] (an unused slot in the sret
 * buffer, inside C mmap). This is necessary because ALL callee-saved
 * registers (RBX, RBP, R12-R15) are in the JIT's allocatable pool
 * and will be clobbered during JIT execution. After the JIT's RET,
 * RSP equals the sret address, so [RSP+32] recovers the system RSP.
 *
 * System V AMD64 ABI arguments:
 *   RDI = fn          (JIT function pointer)
 *   RSI = sret        (sret buffer, in guest mmap)
 *   RDX = regfile     (shadow register file, in guest mmap)
 *   RCX = memBase     (guest memory base)
 *   R8  = memMask     (guest memory mask)
 *   R9  = sandbox_sp  (sandbox stack pointer = sret address)
 *
 * The JIT function expects (by ir/lower_amd64_rv8.go convention):
 *   RDI = sret
 *   RSI = regfile  (becomes RBP in the JIT prologue)
 *   R8  = memBase
 *   R9  = memMask
 */

#ifdef __x86_64__

.text
.globl jit_trampoline_asm
#ifdef __APPLE__
.globl _jit_trampoline_asm
_jit_trampoline_asm:
#endif
jit_trampoline_asm:
	/* Save callee-saved registers on system stack. */
	pushq	%rbx
	pushq	%rbp
	pushq	%r12
	pushq	%r13
	pushq	%r14
	pushq	%r15

	/* Save system RSP (will be stashed in sret[32] after RSP switch). */
	movq	%rsp, %rax

	/* Switch RSP to sandbox stack (C mmap). */
	movq	%r9, %rsp

	/* Stash system RSP at sret[32]. JIT code never touches [32..79]. */
	movq	%rax, 32(%rsp)

	/*
	 * Shuffle registers for JIT calling convention.
	 * Input:  RDI=fn RSI=sret RDX=regfile RCX=memBase R8=memMask
	 * Output: RDI=sret RSI=regfile R8=memBase R9=memMask  fn→RAX
	 */
	movq	%rdi, %rax	/* RAX = fn */
	movq	%rsi, %rdi	/* RDI = sret */
	movq	%rdx, %rsi	/* RSI = regfile */
	movq	%rcx, %r11	/* R11 = memBase (temp) */
	xorq	%rdx, %rdx	/* RDX = 0 (unused by JIT) */
	xorq	%rcx, %rcx	/* RCX = 0 (unused; fcsr via sret[80]) */
	movq	%r8, %r9	/* R9  = memMask */
	movq	%r11, %r8	/* R8  = memBase */

	/* Call JIT-compiled native code. */
	callq	*%rax

	/*
	 * After JIT's RET: RSP = sret address (CALL pushed, RET popped).
	 * Recover system RSP from sret[32].
	 */
	movq	32(%rsp), %rsp

	/* Restore callee-saved registers from system stack. */
	popq	%r15
	popq	%r14
	popq	%r13
	popq	%r12
	popq	%rbp
	popq	%rbx

	retq

#endif /* __x86_64__ */

#if defined(__linux__) && defined(__ELF__)
.section .note.GNU-stack,"",@progbits
#endif
