//go:build libriscv

// hellobench — per-ECALL timing comparison for the "Hello, …" guest.
//
// Runs three RV64 emulator configurations on functionally-identical
// guest ELFs (differing only in the literal message string), reports
// wall-clock per-ECALL cost averaged over ITERS iterations.
//
//	$ cd ~/ris && make hello
//	  libriscv             ???  ns/call   Hello, libriscv!
//	  GoCPU interpreter    ???  ns/call   Hello, Go CPU!
//	  GoCPU direct syscall ???  ns/call   Hello, Go CPU!   (Phase 2)
//
// The third line is only emitted once Phase 2 (native SYSCALL fast
// path) lands. Until then the driver prints two lines.
//
// Guest output is discarded (io.Discard / null_stdout) to keep the
// terminal readable when running millions of iterations.
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
	"syscall"
	"time"

	riscv "riscv"
	libriscv "riscv/bench/libriscv"
)

const (
	defaultITERS = 10000
	guestMem     = riscv.Size64MB
	guestSP      = 0x03F00000
)

func main() {
	var (
		flagRepeat = flag.Int("repeat", 5, "take best of N runs per configuration")
		flagVerify = flag.Bool("verify", false, "verify output string on first run")
		flagElfDir = flag.String("elfdir", "bench/hello_guest", "dir containing hello_*.elf")
	)
	flag.Parse()

	libriscvELF := mustRead(filepath.Join(*flagElfDir, "hello_libriscv.elf"))
	gocpuELF := mustRead(filepath.Join(*flagElfDir, "hello_gocpu.elf"))

	// We redirect the host process's fd=1 to /dev/null around each
	// guest run so the direct-SYSCALL path (which writes straight to
	// the kernel's fd=1) doesn't spam the terminal with ~30000 lines.
	// libriscv's null_stdout callback and the GoCPU interpreter's
	// io.Discard WriteFunc don't use fd=1, so the redirect is a no-op
	// for them — but applying it uniformly keeps the methodology
	// identical across runners.

	// ── libriscv ──────────────────────────────────────────────────────
	libriscvNs, stddev := meanVar(*flagRepeat, func() int64 {
		var ns int64
		withStdoutToDevNull(func() {
			m := libriscv.NewMachine(libriscvELF, guestMem)
			if m == nil {
				die("libriscv.NewMachine returned nil")
			}
			defer m.Close()
			t0 := time.Now()
			m.RunToCompletion(0)
			ns = time.Since(t0).Nanoseconds()
		})
		return ns
	})
	printLine("libriscv", libriscvNs, stddev, defaultITERS, "Hello, libriscv!")

	// ── GoCPU interpreter ─────────────────────────────────────────────
	gocpuInterpNs, stddev := meanVar(*flagRepeat, func() int64 {
		return runGoCPU(gocpuELF /*jit=*/, false, *flagVerify)
	})
	printLine("GoCPU interpreter", gocpuInterpNs, stddev, defaultITERS, "Hello, Go CPU!")

	// ── GoCPU direct syscall (Phase 2) ────────────────────────────────
	// Present when the ECALL fast path is compiled in (see
	// riscv/internal/syscalls). Runs on a fresh JIT that uses
	// IRSyscall emission; the guest's ECALL becomes a direct
	// Go-asm dispatcher call + kernel SYSCALL, no Go dispatch loop.
	if hasDirectSyscall() {
		gocpuDirectNs, stddev := meanVar(*flagRepeat, func() int64 {
			return runGoCPUDirect(gocpuELF)
		})
		printLine("GoCPU direct syscall", gocpuDirectNs, stddev, defaultITERS, "Hello, Go CPU!")
	}
}

// runGoCPU executes the gocpuELF once and returns wall-clock ns for
// the full run. If jit is true, runs through RunJIT; otherwise runs
// through the uncached interpreter.
func runGoCPU(elf []byte, jit bool, verify bool) int64 {
	mem, err := riscv.NewGuestMemory(guestMem)
	if err != nil {
		die("NewGuestMemory: %v", err)
	}
	defer mem.Free()
	entry, err := riscv.LoadELFBytes(mem, elf)
	if err != nil {
		die("LoadELFBytes: %v", err)
	}

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetReg(2, guestSP)

	var out io.Writer = io.Discard
	var buf *bytes.Buffer
	if verify {
		buf = &bytes.Buffer{}
		out = buf
	}

	cleanup := riscv.InstallLinuxOS(cpu, out)
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
		die("run (jit=%v): %v", jit, runErr)
	}
	if verify && buf.Len() == 0 {
		die("verify: no output captured (jit=%v)", jit)
	}
	return elapsed
}

// hasDirectSyscall reports whether the JIT's Phase-2 fast path is
// available. Today there is no such hook on JIT; this is a stub
// that Phase 2 will flip.
func hasDirectSyscall() bool {
	// TODO(phase2): return j.HasSyscallTable() or equivalent.
	return false
}

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
	fmt.Printf("  %-20s %0.1f +/- %0.2f ns/call   %s\n", label, perCall, stddev, tag)
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
