#include "jit_sandbox.h"
#include <string.h>

#define SHADOW_REG_SIZE 536
#define RF_MEMBASE_OFF  520
#define RF_MEMMASK_OFF  528

extern void jit_trampoline_asm(
	void *fn, void *sret, void *regfile,
	uintptr_t memBase, uint64_t memMask,
	void *sandbox_sp);

JitResult jit_sandbox_call(
	uintptr_t fn,
	uint64_t *go_x, uint64_t *go_f, uint32_t *go_fcsr,
	uintptr_t mem_base, uint64_t mem_mask,
	uintptr_t reg_file, uintptr_t sandbox_stack_top,
	uintptr_t dc_base, uint64_t dc_mask,
	uint64_t vaddr_begin, uint64_t seg_size)
{
	char *rf = (char*)reg_file;

	/* Save guest memory under the shadow before overwriting. */
	char saved[SHADOW_REG_SIZE];
	memcpy(saved, rf, SHADOW_REG_SIZE);

	/* Copy Go registers → shadow register file. */
	memcpy(rf, go_x, 256);
	memcpy(rf + 256, go_f, 256);
	*(uint32_t*)(rf + 512) = *go_fcsr;
	*(uintptr_t*)(rf + RF_MEMBASE_OFF) = mem_base;
	*(uint64_t*)(rf + RF_MEMMASK_OFF) = mem_mask;

	/* 144-byte sret buffer at top of sandbox stack. */
	char *sret = (char*)sandbox_stack_top - 144;
	memset(sret, 0, 144);
	*(uint64_t*)(sret + 80) = reg_file + 512;
	*(uint64_t*)(sret + 88)  = dc_base;
	*(uint64_t*)(sret + 96)  = dc_mask;
	*(uint64_t*)(sret + 104) = vaddr_begin;
	*(uint64_t*)(sret + 112) = seg_size;

	jit_trampoline_asm((void*)fn, sret, (void*)reg_file,
		mem_base, mem_mask, sret);

	JitResult result;
	result.pc         = *(uint64_t*)(sret + 0);
	result.status     = *(uint64_t*)(sret + 8);
	result.fault_addr = *(uint64_t*)(sret + 16);

	/* Copy shadow register file → Go registers. */
	memcpy(go_x, rf, 256);
	memcpy(go_f, rf + 256, 256);
	*go_fcsr = *(uint32_t*)(rf + 512);

	/* Restore guest memory that was under the shadow. */
	memcpy(rf, saved, SHADOW_REG_SIZE);

	return result;
}

