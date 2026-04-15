// Package fuzzoracle compares our CPU against libriscv instruction-by-instruction.
// CGO is always enabled — this package requires libriscv to be built first:
//
//	make bench-setup
//
// Then run the fuzz targets with:
//
//	make fuzz-oracle
//	make fuzz-stores
package fuzzoracle

/*
#include <libriscv.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <time.h>

static void null_error(void *o, int t, const char *m, long d) {
    (void)o; (void)t; (void)m; (void)d;
}
static void null_stdout(void *o, const char *m, unsigned n) {
    (void)o; (void)m; (void)n;
}

static RISCVMachine *new_machine(const void *elf, size_t elf_size) {
    RISCVOptions opts;
    libriscv_set_defaults(&opts);
    opts.max_memory     = 64 * 1024 * 1024;
    opts.strict_sandbox = 1;
    opts.error          = null_error;
    opts.output         = null_stdout;
    return libriscv_new(elf, (unsigned)elf_size, &opts);
}

static void delete_machine(RISCVMachine *m) {
    libriscv_delete(m);
}

// get_counter returns the current instruction counter.
static uint64_t get_counter(RISCVMachine *m) {
    return libriscv_instruction_counter(m);
}
// run_to_ecall runs the machine to completion (ECALL/exit or fault).
// In non-instrumented mode libriscv_run ignores the instruction limit
// and runs until a halt condition, so we use UINT64_MAX.
static int run_to_ecall(RISCVMachine *m) {
    return libriscv_run(m, UINT64_MAX);
}

static void snapshot_regs(RISCVMachine *m, uint64_t *dst) {
    RISCVRegisters *r = libriscv_get_registers(m);
    memcpy(dst, r->r, 32 * sizeof(uint64_t));
    dst[32] = r->pc;
}

static int snapshot_mem(RISCVMachine *m, uint64_t gva, void *dst, unsigned len) {
    return libriscv_copy_from_guest(m, dst, gva, len);
}

static void set_regs_and_pc(RISCVMachine *m, const uint64_t *xregs, uint64_t pc) {
    RISCVRegisters *r = libriscv_get_registers(m);
    memcpy(&r->r[1], xregs+1, 31 * sizeof(uint64_t));
    libriscv_jump(m, pc);
}

static int write_guest(RISCVMachine *m, uint64_t gva, const void *src, unsigned len) {
    return libriscv_copy_to_guest(m, gva, src, len);
}
*/
import "C"
import "unsafe"

// Machine wraps a libriscv RISCVMachine.
type Machine struct {
	m *C.RISCVMachine
}

// NewMachine creates a libriscv machine from an in-memory ELF.
// Returns nil if libriscv rejects the ELF.
func NewMachine(elf []byte) *Machine {
	if len(elf) == 0 {
		return nil
	}
	m := C.new_machine(unsafe.Pointer(&elf[0]), C.size_t(len(elf)))
	if m == nil {
		return nil
	}
	return &Machine{m: m}
}

// Close frees the machine.
func (m *Machine) Close() {
	if m.m != nil {
		C.delete_machine(m.m)
		m.m = nil
	}
}

// RunToEcall runs the machine until ECALL/exit or fault.
func (m *Machine) RunToEcall() int { return int(C.run_to_ecall(m.m)) }

// SnapshotRegs returns x0..x31 and PC as [33]uint64.
func (m *Machine) SnapshotRegs() [33]uint64 {
	var dst [33]uint64
	C.snapshot_regs(m.m, (*C.uint64_t)(unsafe.Pointer(&dst[0])))
	return dst
}

// SnapshotMem reads length bytes of guest memory at gva. Returns nil on failure.
func (m *Machine) SnapshotMem(gva uint64, length uint) []byte {
	if length == 0 {
		return []byte{}
	}
	buf := make([]byte, length)
	if C.snapshot_mem(m.m, C.uint64_t(gva), unsafe.Pointer(&buf[0]), C.uint(length)) != 0 {
		return nil
	}
	return buf
}

// SetRegsAndPC sets x1..x31 and PC.
func (m *Machine) SetRegsAndPC(xregs [32]uint64, pc uint64) {
	C.set_regs_and_pc(m.m, (*C.uint64_t)(unsafe.Pointer(&xregs[0])), C.uint64_t(pc))
}

// WriteGuest copies src into guest memory at gva. Returns true on success.
func (m *Machine) WriteGuest(gva uint64, src []byte) bool {
	if len(src) == 0 {
		return true
	}
	return C.write_guest(m.m, C.uint64_t(gva), unsafe.Pointer(&src[0]), C.uint(len(src))) == 0
}
