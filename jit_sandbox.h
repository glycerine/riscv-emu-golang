#ifndef JIT_SANDBOX_H
#define JIT_SANDBOX_H

#include <stdint.h>

typedef struct {
	uint64_t pc;
	uint64_t status;
	uint64_t fault_addr;
	uint64_t ic; /* guest-instruction delta for this dispatch */
} JitResult;

JitResult jit_sandbox_call(
	uintptr_t fn,
	uint64_t *go_x, uint64_t *go_f, uint32_t *go_fcsr,
	uintptr_t mem_base, uint64_t mem_mask,
	uintptr_t reg_file, uintptr_t sandbox_stack_top,
	uintptr_t dc_base, uint64_t dc_mask,
	uint64_t vaddr_begin, uint64_t seg_size,
	uint64_t *resv_addr, uint64_t *resv_valid,
	uint64_t budget);

#endif
