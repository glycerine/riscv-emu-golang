//go:build libriscv

// Package libriscv_bench provides benchmark calibration and fuzz oracle
// against libriscv.
package libriscv_bench

/*
#cgo CFLAGS: -I${SRCDIR}/../../vendor/libriscv/c

#include <libriscv.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <time.h>

// ── silent callbacks ───────────────────────────────────────────────────────

static void null_error(void *o, int t, const char *m, long d) {
    (void)o; (void)t; (void)m; (void)d;
}
static void null_stdout(void *o, const char *m, unsigned n) {
    (void)o; (void)m; (void)n;
}

// ── timing ─────────────────────────────────────────────────────────────────

static int64_t mono_ns(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (int64_t)t.tv_sec * 1000000000LL + t.tv_nsec;
}

// ── machine lifecycle ──────────────────────────────────────────────────────

static RISCVMachine *new_bench_machine(const void *elf, size_t elf_size,
                                        uint64_t memory_bytes) {
    RISCVOptions opts;
    libriscv_set_defaults(&opts);
    opts.max_memory     = memory_bytes;
    opts.strict_sandbox = 1;
    opts.error          = null_error;
    opts.output         = null_stdout; // renamed: stdout is a macro on macOS
    return libriscv_new(elf, (unsigned)elf_size, &opts);
}

static RISCVMachine *new_bench_machine_no_jit(const void *elf, size_t elf_size,
                                               uint64_t memory_bytes) {
    RISCVOptions opts;
    libriscv_set_defaults(&opts);
    opts.max_memory     = memory_bytes;
    opts.strict_sandbox = 1;
    opts.no_translate   = 1;
    opts.error          = null_error;
    opts.output         = null_stdout;
    return libriscv_new(elf, (unsigned)elf_size, &opts);
}

static void delete_machine(RISCVMachine *m) {
    libriscv_delete(m);
}

static uint64_t run_to_completion(RISCVMachine *m, uint64_t insn_limit) {
    int r = libriscv_run(m, insn_limit);
    if (r < 0) return 0;
    return libriscv_instruction_counter(m);
}

// ── memory benchmark ───────────────────────────────────────────────────────

static int64_t mem_write_read_pairs(RISCVMachine *m, uint64_t guest_addr,
                                     int64_t n) {
    uint64_t val = 0xDEADBEEFCAFEBABEULL;
    uint64_t out = 0;
    int64_t t0 = mono_ns();
    for (int64_t i = 0; i < n; i++) {
        libriscv_copy_to_guest(m,   guest_addr, &val, sizeof(val));
        libriscv_copy_from_guest(m, &out, guest_addr, sizeof(out));
        val ^= out;
    }
    return mono_ns() - t0;
}

// ── oracle helpers for fuzz comparison ────────────────────────────────────

// step1 runs exactly one instruction. Returns libriscv error code.
static int step1(RISCVMachine *m) {
    return libriscv_run(m, 1);
}

// snapshot_regs fills dst[0..31]=integer regs, dst[32]=PC.
static void snapshot_regs(RISCVMachine *m, uint64_t *dst) {
    RISCVRegisters *r = libriscv_get_registers(m);
    memcpy(dst, r->r, 32 * sizeof(uint64_t));
    dst[32] = r->pc;
}

// snapshot_mem copies len bytes of guest memory at gva into dst.
static int snapshot_mem(RISCVMachine *m, uint64_t gva, void *dst, unsigned len) {
    return libriscv_copy_from_guest(m, dst, gva, len);
}

// set_regs_and_pc sets x1..x31 and PC. x0 is always 0.
static void set_regs_and_pc(RISCVMachine *m, const uint64_t *xregs, uint64_t pc) {
    RISCVRegisters *r = libriscv_get_registers(m);
    memcpy(&r->r[1], xregs+1, 31 * sizeof(uint64_t));
    libriscv_jump(m, pc);
}

// write_guest copies src into guest memory at gva.
static int write_guest(RISCVMachine *m, uint64_t gva, const void *src, unsigned len) {
    return libriscv_copy_to_guest(m, gva, src, len);
}
*/
import "C"
import "unsafe"

// ── Machine (benchmark use) ────────────────────────────────────────────────

// Machine wraps a libriscv RISCVMachine with a Go-friendly lifecycle.
type Machine struct {
	m *C.RISCVMachine
}

// NewMachine creates a libriscv machine from an ELF image in memory.
// memBytes is the guest address space size. Returns nil on failure.
func NewMachine(elf []byte, memBytes uint64) *Machine {
	m := C.new_bench_machine(
		unsafe.Pointer(&elf[0]),
		C.size_t(len(elf)),
		C.uint64_t(memBytes),
	)
	if m == nil {
		return nil
	}
	return &Machine{m: m}
}

// NewMachineNoJIT creates a libriscv machine with binary translation disabled.
func NewMachineNoJIT(elf []byte, memBytes uint64) *Machine {
	m := C.new_bench_machine_no_jit(
		unsafe.Pointer(&elf[0]),
		C.size_t(len(elf)),
		C.uint64_t(memBytes),
	)
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

// RunToCompletion runs the guest until it calls exit or insnLimit is reached.
func (m *Machine) RunToCompletion(insnLimit uint64) uint64 {
	return uint64(C.run_to_completion(m.m, C.uint64_t(insnLimit)))
}

// MemWriteReadPairs benchmarks copy_to_guest+copy_from_guest pairs in C.
func (m *Machine) MemWriteReadPairs(guestAddr uint64, n int64) int64 {
	return int64(C.mem_write_read_pairs(m.m, C.uint64_t(guestAddr), C.int64_t(n)))
}

// ── Oracle helpers (fuzz comparison) ──────────────────────────────────────

// Step1 runs exactly one instruction. Returns the libriscv error code.
func (m *Machine) Step1() int {
	return int(C.step1(m.m))
}

// SnapshotRegs returns all 32 integer registers and PC as [33]uint64.
// Index 0..31 = x0..x31, index 32 = PC.
func (m *Machine) SnapshotRegs() [33]uint64 {
	var dst [33]uint64
	C.snapshot_regs(m.m, (*C.uint64_t)(unsafe.Pointer(&dst[0])))
	return dst
}

// SnapshotMem reads length bytes of guest memory at gva.
// Returns nil on failure.
func (m *Machine) SnapshotMem(gva uint64, length uint) []byte {
	buf := make([]byte, length)
	if C.snapshot_mem(m.m, C.uint64_t(gva), unsafe.Pointer(&buf[0]), C.uint(length)) != 0 {
		return nil
	}
	return buf
}

// SetRegsAndPC sets x1..x31 and PC. x0 is always zero and is ignored.
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
