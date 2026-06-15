//go:build libriscv

// hellobench — per-ECALL timing comparison for the "Hello, …" guest.
//
// For each configuration we
//
//	(1) run once with full output capture and verify the bytes
//	    match "Hello, <tag>!\n" × 10000 — any divergence dies loudly;
//	(2) run N times with a cheap sink (null_stdout / io.Discard /
//	    /dev/null) and report mean ± stddev ns/call.
//
// The verify pass is the regression guardrail: a dispatcher bug that
// silently drops or corrupts bytes can't hide behind a "slightly
// faster" timing. The timed pass uses a cheap sink so capture
// overhead doesn't confound the numbers.
//
//	$ cd ~/ris && make hello
//	  libriscv             ??? ns/call   Hello, libriscv!
//	  GoCPU interpreter    ??? ns/call   Hello, Go CPU!
//	  GoCPU JIT            ??? ns/call   Hello, Go CPU!
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	riscv "github.com/glycerine/riscv-emu-golang"
	libriscv "github.com/glycerine/riscv-emu-golang/bench/libriscv"
)

const (
	defaultITERS = 10000
	guestMem     = riscv.Size64MB
	guestSP      = 0x03F00000
)

func main() {
	var (
		flagRepeat = flag.Int("repeat", 5, "number of timed runs per configuration")
		flagElfDir = flag.String("elfdir", "bench/hello_guest", "dir containing hello_*.elf")
		flagOnly   = flag.String("only", "", "run only one configuration: libriscv|libriscv-real|gocpu-interp|gocpu-jit. Intended for CPU profiling — attach `sample $PID 10` or Xcode Instruments to the running process.")
	)
	flag.Parse()

	// If the user asked for GoCPU VizJit dumps, mirror the setup on the
	// libriscv side so every block gets a companion file in
	// debug_libriscv_dir/ with the same <tag>.asm.pc_0x... prefix.
	// Must run before any NewMachine/InstallAOT call (libriscv reads
	// LIBRISCV_DUMP_DIR at translation time).
	setupLibriscvDump()
	// After all libriscv runs finish, enrich the Guest RISC-V section
	// of each per-block file with disassembly. The C++ dumper emits
	// hex-only on purpose — duplicating GoCPU's RISC-V disassembler in
	// C++ would drift.
	defer augmentLibriscvDumpsIfEnabled()

	libriscvELF := mustRead(filepath.Join(*flagElfDir, "hello_libriscv.elf"))
	gocpuELF := mustRead(filepath.Join(*flagElfDir, "hello_gocpu.elf"))

	// ── profiling mode: single configuration, large repeat ─────────────
	//
	// Example macOS profiling session:
	//   ./hellobench -only=libriscv       -repeat=100000 &   # libriscv
	//   sample $! 10 -file /tmp/libriscv.sampled
	//   ./hellobench -only=gocpu-jit      -repeat=100000 &   # GoCPU
	//   sample $! 10 -file /tmp/gocpu.sampled
	//   diff <(awk '/Call graph/,0' /tmp/libriscv.sampled) \
	//        <(awk '/Call graph/,0' /tmp/gocpu.sampled)
	//
	// PID printing is done before the run so external samplers can
	// attach before the hot loop starts.
	if *flagOnly != "" {
		runOnly(*flagOnly, *flagRepeat, libriscvELF, gocpuELF)
		return
	}

	// ── correctness first. Any failure dies loudly. ────────────────────
	verifyLibriscv(libriscvELF, "Hello, libriscv!\n")
	verifyLibriscvRealWrite(libriscvELF, "Hello, libriscv!\n")
	verifyGoCPUInterp(gocpuELF, "Hello, Go CPU!\n")
	verifyGoCPUJIT(gocpuELF, "Hello, Go CPU!\n")

	// ── timed runs with cheap sinks ────────────────────────────────────
	//
	// Each emulator runs through its normal OS personality. GoCPU stdout
	// is routed to io.Discard so ECALL handling is included without
	// measuring terminal or filesystem writes.

	libriscvNs, stddev := meanVar(*flagRepeat, func() int64 {
		return timeLibriscv(libriscvELF)
	})
	printLine("libriscv", libriscvNs, stddev, defaultITERS, "Hello, libriscv!")

	libriscvRWNs, stddev := meanVar(*flagRepeat, func() int64 {
		return timeLibriscvRealWrite(libriscvELF)
	})
	printLine("libriscv real write", libriscvRWNs, stddev, defaultITERS, "Hello, libriscv!")

	gocpuInterpNs, stddev := meanVar(*flagRepeat, func() int64 {
		return timeGoCPU(gocpuELF, false)
	})
	printLine("GoCPU interpreter", gocpuInterpNs, stddev, defaultITERS, "Hello, Go CPU!")

	gocpuJITNs, stddev := meanVar(*flagRepeat, func() int64 {
		return timeGoCPU(gocpuELF, true)
	})
	printLine("GoCPU JIT", gocpuJITNs, stddev, defaultITERS, "Hello, Go CPU!")
}

// runOnly executes a single configuration repeat times, printing the
// process PID first (for external sampler attach) and the mean
// ns/call at the end. No verify pass, no correctness check — this is
// a profiling harness, not a correctness harness.
func runOnly(mode string, repeat int, libriscvELF, gocpuELF []byte) {
	var fn func() int64
	var tag string
	switch mode {
	case "libriscv":
		fn = func() int64 { return timeLibriscv(libriscvELF) }
		tag = "Hello, libriscv!"
	case "libriscv-real":
		fn = func() int64 { return timeLibriscvRealWrite(libriscvELF) }
		tag = "Hello, libriscv!"
	case "gocpu-interp":
		fn = func() int64 { return timeGoCPU(gocpuELF, false) }
		tag = "Hello, Go CPU!"
	case "gocpu-jit":
		fn = func() int64 { return timeGoCPU(gocpuELF, true) }
		tag = "Hello, Go CPU!"
	default:
		die("unknown -only mode: %q", mode)
	}

	fmt.Fprintf(os.Stderr, "# hellobench -only=%s pid=%d repeat=%d iters_per_run=%d\n",
		mode, os.Getpid(), repeat, defaultITERS)
	fmt.Fprintln(os.Stderr, "# sleeping 1s before hot loop — attach sampler now.")
	time.Sleep(1 * time.Second)

	mean, stddev := meanVar(repeat, fn)
	printLine(mode, mean, stddev, defaultITERS, tag)
}

// ── correctness (verify) runs — one-shot per config ──────────────────

func verifyLibriscv(elf []byte, expectedLine string) {
	m := libriscv.NewMachineCapturing(elf, guestMem)
	if m == nil {
		die("libriscv.NewMachineCapturing returned nil")
	}
	defer m.Close()
	m.RunToCompletion(0)
	verifyOutput("libriscv", m.CapturedOutput(), expectedLine)
}

func verifyLibriscvRealWrite(elf []byte, expectedLine string) {
	// Real-write mode goes through kernel write(1, …). Redirect fd=1
	// to a tempfile so we can actually read what libriscv emitted.
	captured := withStdoutToTempFile(func() {
		m := libriscv.NewMachineRealWrite(elf, guestMem)
		if m == nil {
			die("libriscv.NewMachineRealWrite returned nil")
		}
		defer m.Close()
		m.RunToCompletion(0)
	})
	verifyOutput("libriscv real write", captured, expectedLine)
}

func verifyGoCPUInterp(elf []byte, expectedLine string) {
	mem, err := riscv.NewGuestMemory(guestMem)
	if err != nil {
		die("NewGuestMemory: %v", err)
	}
	defer mem.Free()
	elfInfo, err := riscv.LoadELFBytes(mem, elf)
	if err != nil {
		die("LoadELFBytes: %v", err)
	}
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(elfInfo.Entry)
	cpu.SetReg(2, guestSP)

	var buf bytes.Buffer
	_, err = riscv.RunWithLinuxOS(cpu, &buf)
	if err != nil {
		die("RunWithLinuxOS: %v", err)
	}
	verifyOutput("GoCPU interpreter", buf.Bytes(), expectedLine)
}

func verifyGoCPUJIT(elf []byte, expectedLine string) {
	mem, err := riscv.NewGuestMemory(guestMem)
	if err != nil {
		die("NewGuestMemory: %v", err)
	}
	defer mem.Free()
	elfInfo, err := riscv.LoadELFBytes(mem, elf)
	if err != nil {
		die("LoadELFBytes: %v", err)
	}
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(elfInfo.Entry)
	cpu.SetReg(2, guestSP)

	var buf bytes.Buffer
	cleanup := riscv.InstallLinuxOS(cpu, &buf)
	defer cleanup()

	j := riscv.NewJIT()

	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(*riscv.ExitError); ok {
					return
				}
				panic(r)
			}
		}()
		if err := j.RunJIT(cpu); err != nil {
			if _, ok := err.(*riscv.ExitError); !ok {
				die("RunJIT: %v", err)
			}
		}
	}()
	verifyOutput("GoCPU JIT", buf.Bytes(), expectedLine)
}

// ── timed (sink) runs — called N times for mean/stddev ───────────────

func timeLibriscv(elf []byte) int64 {
	m := libriscv.NewMachine(elf, guestMem)
	if m == nil {
		die("libriscv.NewMachine returned nil")
	}
	defer m.Close()
	t0 := time.Now()
	m.RunToCompletion(0)
	return time.Since(t0).Nanoseconds()
}

// timeLibriscvRealWrite times libriscv with its output callback routed
// through kernel write(2). Guest fd=1 is redirected to /dev/null.
func timeLibriscvRealWrite(elf []byte) int64 {
	m := libriscv.NewMachineRealWrite(elf, guestMem)
	if m == nil {
		die("libriscv.NewMachineRealWrite returned nil")
	}
	defer m.Close()

	var elapsed int64
	withStdoutToDevNull(func() {
		t0 := time.Now()
		m.RunToCompletion(0)
		elapsed = time.Since(t0).Nanoseconds()
	})
	return elapsed
}

func timeGoCPU(elf []byte, jit bool) int64 {
	mem, err := riscv.NewGuestMemory(guestMem)
	if err != nil {
		die("NewGuestMemory: %v", err)
	}
	defer mem.Free()
	elfInfo, err := riscv.LoadELFBytes(mem, elf)
	if err != nil {
		die("LoadELFBytes: %v", err)
	}
	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(elfInfo.Entry)
	cpu.SetReg(2, guestSP)

	cleanup := riscv.InstallLinuxOS(cpu, io.Discard)
	defer cleanup()

	t0 := time.Now()
	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(*riscv.ExitError); ok {
					return
				}
				panic(r)
			}
		}()
		if jit {
			j := riscv.NewJIT()
			runErr = j.RunJIT(cpu)
		} else {
			runErr = riscv.RunWithChain(cpu, &cpu.Notes)
		}
	}()
	elapsed := time.Since(t0).Nanoseconds()
	if runErr != nil {
		if _, ok := runErr.(*riscv.ExitError); !ok {
			die("timeGoCPU (jit=%v): %v", jit, runErr)
		}
	}
	return elapsed
}

// ── correctness checking ─────────────────────────────────────────────

// verifyOutput compares got to expectedLine × defaultITERS. On
// mismatch, die() with a concise locator: length, first-differing
// byte position, and context on both sides.
func verifyOutput(label string, got []byte, expectedLine string) {
	want := strings.Repeat(expectedLine, defaultITERS)
	if len(got) != len(want) {
		die("%s: captured %d bytes, want %d\n  got [first 32]:  %q\n  want[first 32]:  %q",
			label, len(got), len(want),
			first32(got), first32([]byte(want)))
	}
	if bytes.Equal(got, []byte(want)) {
		return
	}
	for i := range got {
		if got[i] != want[i] {
			a := max(0, i-8)
			b := min(len(got), i+24)
			die("%s: byte %d mismatch\n  got : %q\n  want: %q",
				label, i, got[a:b], want[a:b])
		}
	}
}

func first32(b []byte) []byte {
	if len(b) <= 32 {
		return b
	}
	return b[:32]
}

// ── host fd=1 redirection helpers ────────────────────────────────────

// withStdoutToTempFile runs fn with fd=1 redirected to a tempfile and
// returns the captured bytes. Used by the verify path for the
// direct-SYSCALL runner where writes target kernel fd=1 directly.
func withStdoutToTempFile(fn func()) []byte {
	tmpf, err := os.CreateTemp("", "hellocap-*")
	if err != nil {
		die("CreateTemp: %v", err)
	}
	defer os.Remove(tmpf.Name())
	defer tmpf.Close()

	saved, err := syscall.Dup(1)
	if err != nil {
		die("dup(1): %v", err)
	}
	defer syscall.Close(saved)

	if err := syscall.Dup2(int(tmpf.Fd()), 1); err != nil {
		die("dup2(tmp, 1): %v", err)
	}
	restored := false
	defer func() {
		if !restored {
			syscall.Dup2(saved, 1)
		}
	}()

	fn()

	if err := syscall.Dup2(saved, 1); err != nil {
		die("dup2(restore): %v", err)
	}
	restored = true

	if _, err := tmpf.Seek(0, 0); err != nil {
		die("seek: %v", err)
	}
	data, err := io.ReadAll(tmpf)
	if err != nil {
		die("read: %v", err)
	}
	return data
}

// withStdoutToDevNull runs fn with fd=1 redirected to /dev/null.
// Used by the timed runs of the direct-SYSCALL runner — the sink
// must be a real kernel fd (otherwise SYSCALL's write would go to
// the terminal), and /dev/null is the cheapest real sink available.
func withStdoutToDevNull(fn func()) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		die("open /dev/null: %v", err)
	}
	defer devnull.Close()

	saved, err := syscall.Dup(1)
	if err != nil {
		die("dup(1): %v", err)
	}
	defer syscall.Close(saved)

	if err := syscall.Dup2(int(devnull.Fd()), 1); err != nil {
		die("dup2(devnull, 1): %v", err)
	}
	defer syscall.Dup2(saved, 1)

	fn()
}

// ── stats + utilities ────────────────────────────────────────────────

func meanVar(n int, run func() int64) (mean, stddev float64) {
	if n < 1 {
		n = 1
	}
	var tms []float64
	var sum float64
	for range n {
		t := float64(run())
		tms = append(tms, t)
		sum += t
	}
	mean = sum / float64(n)
	var v float64
	for _, t := range tms {
		d := (t - mean)
		v += d * d
	}
	v = v / float64(n)
	stddev = math.Sqrt(v)

	return
}

func printLine(label string, perCall, stddev float64, iters int, tag string) {
	perCall /= float64(iters)
	stddev /= float64(iters)
	fmt.Printf("  %-20s %0.1f +/- %0.2f ns/call   %s\n", label, perCall, stddev, tag)
}

// setupLibriscvDump bridges GoCPU's VizJit dump feature with the new
// libriscv-side dumper in xendor/libriscv/lib/libriscv/tr_dump.cpp. The
// two sides read different env vars; this helper makes "enable dumps"
// a single environmental switch (GOCPU_VIZJIT) that lights up both.
//
// Behavior:
//   - If neither GOCPU_VIZJIT nor LIBRISCV_DUMP_DIR is set, noop.
//   - If GOCPU_VIZJIT is set but LIBRISCV_DUMP_DIR is not, default
//     LIBRISCV_DUMP_DIR to <parent>/debug_libriscv_dir.
//   - Propagate GoCPU's 16-hex run tag into LIBRISCV_DUMP_TAG so both
//     dump sets share the same <tag>.asm.pc_0x... prefix — keys `diff`
//     alignment.
func setupLibriscvDump() {
	gocpuDir := os.Getenv("GOCPU_VIZJIT")
	lrDir := os.Getenv("LIBRISCV_DUMP_DIR")
	if gocpuDir == "" && lrDir == "" {
		return
	}
	if lrDir == "" {
		lrDir = filepath.Join(filepath.Dir(gocpuDir), "debug_libriscv_dir")
		_ = os.Setenv("LIBRISCV_DUMP_DIR", lrDir)
	}
	if err := os.MkdirAll(lrDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "hellobench: could not create %s: %v\n", lrDir, err)
		return
	}
	if os.Getenv("LIBRISCV_DUMP_TAG") == "" {
		_ = os.Setenv("LIBRISCV_DUMP_TAG", riscv.GetVizJitTag())
	}
	fmt.Fprintf(os.Stderr, "# libriscv dumps -> %s (tag=%s)\n",
		lrDir, os.Getenv("LIBRISCV_DUMP_TAG"))
}

func augmentLibriscvDumpsIfEnabled() {
	dir := os.Getenv("LIBRISCV_DUMP_DIR")
	if dir == "" {
		return
	}
	if err := riscv.AugmentLibriscvDumps(dir); err != nil {
		fmt.Fprintf(os.Stderr, "augment libriscv dumps in %s: %v\n", dir, err)
	}
}

func mustRead(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		die("read %s: %v", path, err)
	}
	return data
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hellobench: ")
	fmt.Fprintf(os.Stderr, format, args...)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  (goos=%s goarch=%s)\n", runtime.GOOS, runtime.GOARCH)
	os.Exit(1)
}
