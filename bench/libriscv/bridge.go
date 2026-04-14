//go:build libriscv

// Package libriscv_bench provides benchmark calibration numbers for libriscv.
//
// Build requirements (managed by `make bench-setup`):
//
//	vendor/libriscv/c/libriscv.h               — C API header
//	vendor/libriscv/build_capi/libriscv_capi.a — C API static lib
//	vendor/libriscv/build_capi/libriscv/libriscv.a — core static lib
//	bench/libriscv_guest/bench_guest.elf       — RISC-V guest binary
//
// The #cgo LDFLAGS line is intentionally absent: Go's security policy
// forbids ${SRCDIR}-relative paths in linker flags. Library paths are
// supplied by the Makefile via CGO_LDFLAGS env var instead.
//
// Usage:
//
//	make bench              # full comparison
//	make bench-libriscv     # this package only
package libriscv_bench

/*
#cgo CFLAGS: -I${SRCDIR}/../../vendor/libriscv/c

#include <libriscv.h>
#include <stdlib.h>
#include <stdint.h>
#include <time.h>

// ── silent callbacks ───────────────────────────────────────────────────────

static void null_error(void *o, int t, const char *m, long d) {
    (void)o; (void)t; (void)m; (void)d;
}
static void null_stdout(void *o, const char *m, unsigned n) {
    (void)o; (void)m; (void)n;
}

// ── timing ────────────────────────────────────────────────────────────────

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
    opts.stdout         = null_stdout;
    return libriscv_new(elf, (unsigned)elf_size, &opts);
}

static void delete_machine(RISCVMachine *m) {
    libriscv_delete(m);
}

// run_to_completion runs m until the guest calls exit or hits insn_limit.
// Returns instructions retired, or 0 on error.
static uint64_t run_to_completion(RISCVMachine *m, uint64_t insn_limit) {
    int r = libriscv_run(m, insn_limit);
    if (r < 0) return 0;
    return libriscv_instruction_counter(m);
}

// ── memory benchmark ───────────────────────────────────────────────────────

// mem_write_read_pairs performs n copy_to_guest+copy_from_guest uint64
// pairs at guest_addr. Timing is done entirely in C so no cgo boundary
// is crossed in the hot loop. Returns total elapsed nanoseconds.
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
*/
import "C"
import "unsafe"

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

// Close frees the machine. Safe to call multiple times.
func (m *Machine) Close() {
	if m.m != nil {
		C.delete_machine(m.m)
		m.m = nil
	}
}

// RunToCompletion runs the guest until it calls exit or insnLimit is reached.
// Returns instructions retired, or 0 on error.
func (m *Machine) RunToCompletion(insnLimit uint64) uint64 {
	return uint64(C.run_to_completion(m.m, C.uint64_t(insnLimit)))
}

// MemWriteReadPairs performs n copy_to_guest+copy_from_guest uint64 pairs
// at guestAddr. Timing is measured inside C (no cgo overhead in the loop).
// Returns total nanoseconds elapsed.
func (m *Machine) MemWriteReadPairs(guestAddr uint64, n int64) int64 {
	return int64(C.mem_write_read_pairs(m.m, C.uint64_t(guestAddr), C.int64_t(n)))
}
