//go:build libriscv

// Package libriscv_bench provides benchmark calibration and fuzz oracle
// against libriscv.
package libriscv_bench

/*
#cgo CFLAGS: -I${SRCDIR}/../../xendor/libriscv/c -I${SRCDIR}/../../xendor/libriscv/build_capi/libriscv
#cgo LDFLAGS: -L${SRCDIR}/../../xendor/libriscv/build_capi -L${SRCDIR}/../../xendor/libriscv/build_capi/libriscv -lriscv_capi -lriscv -lstdc++ -lm
#cgo darwin LDFLAGS: -framework Security

#include <libriscv.h>
#include <libriscv_settings.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <sys/time.h>

#if defined(__APPLE__)
#include <mach/mach_time.h>
#endif

// ── silent callbacks ───────────────────────────────────────────────────────

static void null_error(void *o, int t, const char *m, long d) {
    (void)o; (void)t; (void)m; (void)d;
}
static void null_stdout(void *o, const char *m, unsigned n) {
    (void)o; (void)m; (void)n;
}

// real_write_stdout forwards bytes to the host kernel via write(2).
// Used by the apples-to-apples benchmark where we want libriscv's
// per-ECALL cost to include actual kernel-entry overhead (matching
// what the GoCPU direct-syscall runner pays). Callers are expected
// to redirect fd=1 appropriately (e.g., to /dev/null) before use.
static void real_write_stdout(void *o, const char *m, unsigned n) {
    (void)o;
    ssize_t w = write(1, m, n);
    (void)w;
}

// ── portable Linux clock_gettime shim ─────────────────────────────────────

typedef struct {
    int64_t tv_sec;
    int64_t tv_nsec;
} LinuxTimespec64;

static int portable_linux_clock_gettime(int clkid, LinuxTimespec64 *out) {
    // Linux clock IDs, as seen by a riscv64 Linux guest.
    enum {
        LINUX_CLOCK_REALTIME = 0,
        LINUX_CLOCK_MONOTONIC = 1,
        LINUX_CLOCK_PROCESS_CPUTIME_ID = 2,
        LINUX_CLOCK_THREAD_CPUTIME_ID = 3,
        LINUX_CLOCK_MONOTONIC_RAW = 4,
        LINUX_CLOCK_REALTIME_COARSE = 5,
        LINUX_CLOCK_MONOTONIC_COARSE = 6,
        LINUX_CLOCK_BOOTTIME = 7
    };

    switch (clkid) {
    case LINUX_CLOCK_REALTIME:
    case LINUX_CLOCK_REALTIME_COARSE: {
        struct timeval tv;
        if (gettimeofday(&tv, NULL) != 0) return -1;
        out->tv_sec = (int64_t)tv.tv_sec;
        out->tv_nsec = (int64_t)tv.tv_usec * 1000;
        return 0;
    }
    case LINUX_CLOCK_MONOTONIC:
    case LINUX_CLOCK_MONOTONIC_RAW:
    case LINUX_CLOCK_MONOTONIC_COARSE:
    case LINUX_CLOCK_BOOTTIME:
#if defined(__APPLE__)
    {
        uint64_t now = mach_absolute_time();
        mach_timebase_info_data_t timebase;
        mach_timebase_info(&timebase);
        uint64_t ns = (uint64_t)((long double)now * (long double)timebase.numer / (long double)timebase.denom);
        if (ns == 0) ns = 1;
        out->tv_sec = (int64_t)(ns / 1000000000ULL);
        out->tv_nsec = (int64_t)(ns % 1000000000ULL);
        return 0;
    }
#else
    {
        struct timespec ts;
        clockid_t host_clock = CLOCK_MONOTONIC;
#ifdef CLOCK_MONOTONIC_RAW
        if (clkid == LINUX_CLOCK_MONOTONIC_RAW) host_clock = CLOCK_MONOTONIC_RAW;
#endif
#ifdef CLOCK_BOOTTIME
        if (clkid == LINUX_CLOCK_BOOTTIME) host_clock = CLOCK_BOOTTIME;
#endif
        if (clock_gettime(host_clock, &ts) != 0) return -1;
        out->tv_sec = (int64_t)ts.tv_sec;
        out->tv_nsec = (int64_t)ts.tv_nsec;
        return 0;
    }
#endif
    case LINUX_CLOCK_PROCESS_CPUTIME_ID:
    case LINUX_CLOCK_THREAD_CPUTIME_ID:
    default:
        return -1;
    }
}

static void portable_clock_gettime_syscall(RISCVMachine *m) {
    RISCVRegisters *regs = libriscv_get_registers(m);
    int clkid = (int)regs->r[10];
    uint64_t buffer = regs->r[11];
    LinuxTimespec64 ts;
    if (portable_linux_clock_gettime(clkid, &ts) != 0) {
        regs->r[10] = (uint64_t)-22; // EINVAL
        return;
    }
    if (libriscv_copy_to_guest(m, buffer, &ts, sizeof(ts)) != 0) {
        regs->r[10] = (uint64_t)-14; // EFAULT
        return;
    }
    regs->r[10] = 0;
}

static void install_portable_clock_handlers(void) {
    libriscv_set_syscall_handler(113, portable_clock_gettime_syscall);
    libriscv_set_syscall_handler(403, portable_clock_gettime_syscall);
}

static int bridge_has_libtcc_jit(void) {
#if defined(RISCV_BINARY_TRANSLATION) && defined(RISCV_LIBTCC)
    return 1;
#else
    return 0;
#endif
}

// ── capturing stdout callback ──────────────────────────────────────────────
// CaptureBuffer accumulates bytes written by the guest to fd=1/2 via
// libriscv's opts.output path. Backed by realloc-by-doubling so amortized
// cost per byte is O(1). Used by hellobench for regression verification.

typedef struct {
    char   *data;
    size_t  len;
    size_t  cap;
} CaptureBuffer;

static CaptureBuffer *new_capture_buffer(void) {
    return (CaptureBuffer*)calloc(1, sizeof(CaptureBuffer));
}
static void free_capture_buffer(CaptureBuffer *cb) {
    if (cb == NULL) return;
    free(cb->data);
    free(cb);
}
static size_t capture_buffer_len(CaptureBuffer *cb) {
    return cb->len;
}
static const char *capture_buffer_data(CaptureBuffer *cb) {
    return cb->data;
}
static void capture_stdout(void *o, const char *m, unsigned n) {
    CaptureBuffer *cb = (CaptureBuffer*)o;
    size_t need = cb->len + n;
    if (need > cb->cap) {
        size_t ncap = cb->cap ? cb->cap : 1024;
        while (ncap < need) ncap *= 2;
        cb->data = (char*)realloc(cb->data, ncap);
        cb->cap = ncap;
    }
    memcpy(cb->data + cb->len, m, n);
    cb->len = need;
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

// new_capturing_machine routes guest stdout into a caller-supplied
// CaptureBuffer. Used by the hellobench harness to verify output
// bytes match the expected "Hello, libriscv!\n" × 10000 on every run.
static RISCVMachine *new_capturing_machine(const void *elf, size_t elf_size,
                                             uint64_t memory_bytes,
                                             CaptureBuffer *cb) {
    RISCVOptions opts;
    libriscv_set_defaults(&opts);
    opts.max_memory     = memory_bytes;
    opts.strict_sandbox = 1;
    opts.error          = null_error;
    opts.output         = capture_stdout;
    opts.opaque         = cb;
    return libriscv_new(elf, (unsigned)elf_size, &opts);
}

// new_real_write_machine routes guest stdout through the host kernel
// via write(2). Used for the apples-to-apples comparison — libriscv's
// per-ECALL cost then includes a real kernel-entry cost, matching
// the GoCPU direct-syscall runner's path.
static RISCVMachine *new_real_write_machine(const void *elf, size_t elf_size,
                                              uint64_t memory_bytes) {
    RISCVOptions opts;
    libriscv_set_defaults(&opts);
    opts.max_memory     = memory_bytes;
    opts.strict_sandbox = 1;
    opts.error          = null_error;
    opts.output         = real_write_stdout;
    return libriscv_new(elf, (unsigned)elf_size, &opts);
}

static RISCVMachine *new_real_write_machine_with_args(const void *elf, size_t elf_size,
                                                       uint64_t memory_bytes,
                                                       unsigned argc,
                                                       const char **argv,
                                                       int allow_host_files) {
    RISCVOptions opts;
    libriscv_set_defaults(&opts);
    opts.max_memory     = memory_bytes;
    opts.strict_sandbox = allow_host_files ? 0 : 1;
    opts.argc           = argc;
    opts.argv           = argv;
    opts.error          = null_error;
    opts.output         = real_write_stdout;
    RISCVMachine *m = libriscv_new(elf, (unsigned)elf_size, &opts);
    if (m != NULL) install_portable_clock_handlers();
    return m;
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

// HasTCCJIT reports whether the linked libriscv build has binary translation
// with libtcc enabled.
func HasTCCJIT() bool {
	return C.bridge_has_libtcc_jit() != 0
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

// NewMachineRealWrite creates a libriscv machine whose guest fd=1/2
// writes go through the host kernel via write(2). Used by the
// apples-to-apples benchmark — libriscv's measured cost then includes
// kernel entry, matching what the GoCPU direct-syscall runner pays.
func NewMachineRealWrite(elf []byte, memBytes uint64) *Machine {
	m := C.new_real_write_machine(
		unsafe.Pointer(&elf[0]),
		C.size_t(len(elf)),
		C.uint64_t(memBytes),
	)
	if m == nil {
		return nil
	}
	return &Machine{m: m}
}

// NewMachineRealWriteWithArgs creates a libriscv machine with explicit Linux
// argv, with guest fd=1/2 forwarded through the host write(2) syscall.
func NewMachineRealWriteWithArgs(elf []byte, memBytes uint64, args []string, allowHostFiles bool) *Machine {
	if len(elf) == 0 {
		return nil
	}
	cargs := make([]*C.char, len(args))
	for i, arg := range args {
		cargs[i] = C.CString(arg)
	}
	defer func() {
		for _, arg := range cargs {
			C.free(unsafe.Pointer(arg))
		}
	}()

	var argv **C.char
	if len(cargs) > 0 {
		argv = (**C.char)(unsafe.Pointer(&cargs[0]))
	}
	allow := C.int(0)
	if allowHostFiles {
		allow = 1
	}
	m := C.new_real_write_machine_with_args(
		unsafe.Pointer(&elf[0]),
		C.size_t(len(elf)),
		C.uint64_t(memBytes),
		C.unsigned(len(cargs)),
		(**C.char)(argv),
		allow,
	)
	if m == nil {
		return nil
	}
	return &Machine{m: m}
}

// CapturingMachine is a libriscv machine that accumulates guest
// stdout/stderr writes into a C-side buffer. Use CapturedOutput to
// retrieve the bytes and Close to free everything.
//
// Use this instead of NewMachine when the caller wants to verify
// guest output on every benchmark run — see bench/hellobench.
type CapturingMachine struct {
	m  *C.RISCVMachine
	cb *C.CaptureBuffer
}

// NewMachineCapturing creates a machine whose guest stdout (fd 1/2)
// is captured into a Go-accessible buffer. Returns nil on failure.
func NewMachineCapturing(elf []byte, memBytes uint64) *CapturingMachine {
	cb := C.new_capture_buffer()
	if cb == nil {
		return nil
	}
	m := C.new_capturing_machine(
		unsafe.Pointer(&elf[0]),
		C.size_t(len(elf)),
		C.uint64_t(memBytes),
		cb,
	)
	if m == nil {
		C.free_capture_buffer(cb)
		return nil
	}
	return &CapturingMachine{m: m, cb: cb}
}

// Close frees the machine and the capture buffer.
func (m *CapturingMachine) Close() {
	if m.m != nil {
		C.delete_machine(m.m)
		m.m = nil
	}
	if m.cb != nil {
		C.free_capture_buffer(m.cb)
		m.cb = nil
	}
}

// RunToCompletion runs the guest until exit or insnLimit is reached.
func (m *CapturingMachine) RunToCompletion(insnLimit uint64) uint64 {
	return uint64(C.run_to_completion(m.m, C.uint64_t(insnLimit)))
}

// CapturedOutput returns a copy of the bytes the guest has written
// via fd=1/2 since the machine was created. Safe to call after
// RunToCompletion.
func (m *CapturingMachine) CapturedOutput() []byte {
	n := int(C.capture_buffer_len(m.cb))
	if n == 0 {
		return nil
	}
	return C.GoBytes(unsafe.Pointer(C.capture_buffer_data(m.cb)), C.int(n))
}

// Close frees the machine.
func (m *Machine) Close() {
	if m.m != nil {
		C.delete_machine(m.m)
		m.m = nil
	}
}

// AllowFile permits a single exact host path to be opened by the guest when
// the machine was created with host filesystem access enabled.
func (m *Machine) AllowFile(path string) {
	if m == nil || m.m == nil || path == "" {
		return
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	C.libriscv_allow_file(m.m, cpath)
}

// RunToCompletion runs the guest until it calls exit or insnLimit is reached.
func (m *Machine) RunToCompletion(insnLimit uint64) uint64 {
	return uint64(C.run_to_completion(m.m, C.uint64_t(insnLimit)))
}

// ReturnValue returns the current value in the guest A0 register.
func (m *Machine) ReturnValue() int64 {
	return int64(C.libriscv_return_value(m.m))
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
