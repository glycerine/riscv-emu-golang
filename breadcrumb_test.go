//go:build breadcrumb

package riscv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBreadcrumbTestTracer(t *testing.T, cfg BreadcrumbConfig) (*BreadcrumbTracer, string) {
	t.Helper()
	path := cfg.Path
	if path == "" {
		path = filepath.Join(t.TempDir(), "breadcrumbs.log")
		cfg.Path = path
	}
	b, err := NewBreadcrumbTracer(cfg)
	if err != nil {
		t.Fatalf("NewBreadcrumbTracer: %v", err)
	}
	return b, path
}

func readClosedBreadcrumbLog(t *testing.T, b *BreadcrumbTracer, path string) string {
	t.Helper()
	if err := b.Close(); err != nil {
		t.Fatalf("Close breadcrumb tracer: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}

func TestBreadcrumbUserModeOnly(t *testing.T) {
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{Interval: 1})

	if err := b.RecordPC(1, 1, 0x1000, PrivUser); err != nil {
		t.Fatalf("RecordPC user 1: %v", err)
	}
	if err := b.RecordPC(2, 2, 0x2000, PrivSupervisor); err != nil {
		t.Fatalf("RecordPC supervisor: %v", err)
	}
	if err := b.RecordPC(3, 3, 0x1004, PrivUser); err != nil {
		t.Fatalf("RecordPC user 2: %v", err)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	if !strings.Contains(log, "seq=1 attempt=1") || !strings.Contains(log, "pc=0x0000000000001000") {
		t.Fatalf("first user PC missing from log:\n%s", log)
	}
	if !strings.Contains(log, "seq=2 attempt=3") || !strings.Contains(log, "pc=0x0000000000001004") {
		t.Fatalf("second user PC missing from log:\n%s", log)
	}
	if strings.Contains(log, "pc=0x0000000000002000") {
		t.Fatalf("supervisor PC was hashed despite default user-only scope:\n%s", log)
	}
}

func TestBreadcrumbIncludePrivileged(t *testing.T) {
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{
		Interval:          1,
		IncludePrivileged: true,
	})

	if err := b.RecordPC(1, 1, 0x1000, PrivUser); err != nil {
		t.Fatalf("RecordPC user: %v", err)
	}
	if err := b.RecordPC(2, 2, 0x2000, PrivSupervisor); err != nil {
		t.Fatalf("RecordPC supervisor: %v", err)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	if !strings.Contains(log, "priv=supervisor") || !strings.Contains(log, "pc=0x0000000000002000") {
		t.Fatalf("privileged PC missing with IncludePrivileged=true:\n%s", log)
	}
}

func TestBreadcrumbRunWithChain(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	defer mem.Free()
	code := uint64(0x1000)
	for i, insn := range []uint32{
		0x00000013, // nop
		0x00000013, // nop
		0x00100073, // ebreak
	} {
		if fault := mem.Store32(code+uint64(i)*4, insn); fault != nil {
			t.Fatalf("Store32 insn %d: %v", i, fault)
		}
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{Interval: 1})
	cpu.SetBreadcrumbTracer(b)
	defer cpu.SetBreadcrumbTracer(nil)

	err = RunWithChain(cpu, &cpu.Notes)
	if !errors.Is(err, ErrEbreak) {
		t.Fatalf("RunWithChain err = %v, want ErrEbreak", err)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	for _, pc := range []string{
		"pc=0x0000000000001000",
		"pc=0x0000000000001004",
		"pc=0x0000000000001008",
	} {
		if !strings.Contains(log, pc) {
			t.Fatalf("missing %s in breadcrumb log:\n%s", pc, log)
		}
	}
}

func TestBreadcrumbRunCached(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	defer mem.Free()
	code := uint64(0x2000)
	for i, insn := range []uint32{
		0x00000013, // nop
		0x00000013, // nop
		0x00100073, // ebreak
	} {
		if fault := mem.Store32(code+uint64(i)*4, insn); fault != nil {
			t.Fatalf("Store32 insn %d: %v", i, fault)
		}
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(code)
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{Interval: 1})
	cpu.SetBreadcrumbTracer(b)
	defer cpu.SetBreadcrumbTracer(nil)

	res, _, err := RunDefaultDualBudget(cpu, &cpu.Notes, 10, ^uint64(0))
	if res != RunBudgetContinue || !errors.Is(err, ErrEbreak) {
		t.Fatalf("RunDefaultDualBudget = (%v, %v), want RunBudgetContinue, ErrEbreak", res, err)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	for _, pc := range []string{
		"pc=0x0000000000002000",
		"pc=0x0000000000002004",
		"pc=0x0000000000002008",
	} {
		if !strings.Contains(log, pc) {
			t.Fatalf("missing %s in breadcrumb log:\n%s", pc, log)
		}
	}
}

func TestBreadcrumbRunEmuRunModeStartsImmediately(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "run-mode-breadcrumbs.log")

	code, err := RunEmu(&EmuConfig{
		RunPath:           "testvectors/jea9linux/elf/write_stdout.elf",
		MemorySize:        Size64MB,
		InstructionBudget: 1 << 20,
		Breadcrumb: BreadcrumbConfig{
			Path:     path,
			Interval: 1,
		},
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("RunEmu -run with breadcrumb: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "jea9linux stdout\n"; got != want {
		t.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	log := string(data)
	if !strings.Contains(log, "guest_control=false") {
		t.Fatalf("-run breadcrumb header did not mark guest_control=false:\n%s", log)
	}
	if strings.Contains(log, "# activation") || strings.Contains(log, "mode=armed-next-user") {
		t.Fatalf("-run breadcrumb unexpectedly waited for guest activation:\n%s", log)
	}
	if !strings.Contains(log, "seq=1 ") || !strings.Contains(log, "priv=user") {
		t.Fatalf("-run breadcrumb did not record user PCs immediately:\n%s", log)
	}
}

func TestBreadcrumbArmNextUserRequiresPrivilegedInterlude(t *testing.T) {
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{
		Interval:     1,
		StartPaused:  true,
		GuestControl: true,
	})
	m := newBiosMMIO(nil, nil, nil)
	m.enableBreadcrumb(b)

	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegControl, 4, uint64(breadcrumbControlArmNextUserReset)); !ok || fault != nil {
		t.Fatalf("Store arm-next-user control = (%v, %v)", ok, fault)
	}
	if err := b.afterAttempt(10, 9, 0x1111, PrivUser, 0, PrivUser, 0); err != nil {
		t.Fatalf("afterAttempt arm write: %v", err)
	}
	if err := b.afterAttempt(11, 10, 0x1115, PrivUser, 0, PrivUser, 0); err != nil {
		t.Fatalf("afterAttempt user before privilege interlude: %v", err)
	}
	if b.active {
		t.Fatal("arm-next-user activated before seeing a privileged interlude")
	}
	if err := b.afterAttempt(12, 11, 0x2222, PrivSupervisor, 0, PrivSupervisor, 0); err != nil {
		t.Fatalf("afterAttempt supervisor work: %v", err)
	}
	if err := b.afterAttempt(13, 12, 0x3333, PrivSupervisor, 0, PrivUser, 0x12345000); err != nil {
		t.Fatalf("afterAttempt sret: %v", err)
	}
	if !b.active {
		t.Fatal("arm-next-user did not activate on return to user mode")
	}
	if err := b.afterAttempt(14, 13, 0x4444, PrivUser, 0x12345000, PrivUser, 0x12345000); err != nil {
		t.Fatalf("afterAttempt first active user PC: %v", err)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	for _, absent := range []string{
		"priv=user pc=0x0000000000001111",
		"priv=user pc=0x0000000000001115",
		"priv=supervisor pc=0x0000000000002222",
		"priv=supervisor pc=0x0000000000003333",
	} {
		if strings.Contains(log, absent) {
			t.Fatalf("pre-activation PC %s was hashed:\n%s", absent, log)
		}
	}
	if !strings.Contains(log, "mode=armed-next-user") || !strings.Contains(log, "pc=0x0000000000004444") {
		t.Fatalf("armed activation or first user PC missing:\n%s", log)
	}
}

func TestBreadcrumbGuestControlStartAfterMMIOStore(t *testing.T) {
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{
		Interval:     1,
		StartPaused:  true,
		GuestControl: true,
	})
	m := newBiosMMIO(nil, nil, nil)
	m.enableBreadcrumb(b)

	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegControl, 4, uint64(breadcrumbControlStart)); !ok || fault != nil {
		t.Fatalf("Store start control = (%v, %v)", ok, fault)
	}
	if err := b.afterAttempt(1, 1, 0x6000, PrivUser, 0xfeed0000, PrivUser, 0xfeed0000); err != nil {
		t.Fatalf("afterAttempt MMIO store: %v", err)
	}
	if !b.active {
		t.Fatal("guest control start did not activate after MMIO store instruction")
	}
	if err := b.afterAttempt(2, 2, 0x6004, PrivUser, 0xfeed0000, PrivUser, 0xfeed0000); err != nil {
		t.Fatalf("afterAttempt first instruction after start: %v", err)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	if strings.Contains(log, "priv=user pc=0x0000000000006000") {
		t.Fatalf("MMIO start instruction was hashed:\n%s", log)
	}
	if !strings.Contains(log, "priv=user pc=0x0000000000006004") {
		t.Fatalf("instruction after MMIO start was not hashed:\n%s", log)
	}
}

func TestBreadcrumbGuestControlCapturesTargetAddressSpace(t *testing.T) {
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{
		Interval:     1,
		StartPaused:  true,
		GuestControl: true,
	})
	m := newBiosMMIO(nil, nil, nil)
	m.enableBreadcrumb(b)

	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegTargetPID, 4, 4321); !ok || fault != nil {
		t.Fatalf("Store target pid = (%v, %v)", ok, fault)
	}
	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegControl, 4, uint64(breadcrumbControlArmNextASReset)); !ok || fault != nil {
		t.Fatalf("Store arm-next-address-space control = (%v, %v)", ok, fault)
	}

	if err := b.afterAttempt(10, 9, 0x1111, PrivUser, 0xaaa000, PrivUser, 0xaaa000); err != nil {
		t.Fatalf("afterAttempt arm write: %v", err)
	}
	if err := b.afterAttempt(11, 10, 0x2222, PrivSupervisor, 0xaaa000, PrivSupervisor, 0xbbb000); err != nil {
		t.Fatalf("afterAttempt supervisor work: %v", err)
	}
	if err := b.afterAttempt(12, 11, 0x2ff0, PrivSupervisor, 0xbbb000, PrivUser, 0xaaa000); err != nil {
		t.Fatalf("afterAttempt return to arming address space: %v", err)
	}
	if b.active {
		t.Fatal("arm-next-address-space activated after returning to the arming address space")
	}
	if err := b.afterAttempt(13, 12, 0x2ff4, PrivUser, 0xaaa000, PrivUser, 0xaaa000); err != nil {
		t.Fatalf("afterAttempt arming address space user work: %v", err)
	}
	if err := b.afterAttempt(14, 13, 0x3333, PrivSupervisor, 0xbbb000, PrivUser, 0x12345000); err != nil {
		t.Fatalf("afterAttempt return to target user address space: %v", err)
	}
	if !b.active {
		t.Fatal("arm-next-address-space did not activate")
	}
	if err := b.afterAttempt(15, 14, 0x4444, PrivUser, 0x99999000, PrivUser, 0x99999000); err != nil {
		t.Fatalf("afterAttempt other user address space: %v", err)
	}
	if err := b.afterAttempt(16, 15, 0x5555, PrivUser, 0x12345000, PrivUser, 0x12345000); err != nil {
		t.Fatalf("afterAttempt target user address space: %v", err)
	}

	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegTargetPID, 4); fault != nil || got != 4321 {
		t.Fatalf("target pid = (%d, %v), want 4321, nil", got, fault)
	}
	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegTargetSATP, 8); fault != nil || got != 0x12345000 {
		t.Fatalf("target satp = (%#x, %v), want 0x12345000, nil", got, fault)
	}
	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegTargetStatus, 4); fault != nil || got != 3 {
		t.Fatalf("target status = (%#x, %v), want filter-enabled|target-valid, nil", got, fault)
	}

	log := readClosedBreadcrumbLog(t, b, path)
	if !strings.Contains(log, "target_pid=4321") || !strings.Contains(log, "target_satp=0x0000000012345000") {
		t.Fatalf("activation did not record target identity:\n%s", log)
	}
	if strings.Contains(log, "priv=user pc=0x0000000000004444") {
		t.Fatalf("other user address space polluted breadcrumb trace:\n%s", log)
	}
	if strings.Contains(log, "priv=user pc=0x0000000000002ff4") {
		t.Fatalf("arming address space polluted breadcrumb trace:\n%s", log)
	}
	if !strings.Contains(log, "seq=1 ") || !strings.Contains(log, "priv=user pc=0x0000000000005555") {
		t.Fatalf("target user address space was not hashed:\n%s", log)
	}
}

func TestBreadcrumbMMIOTripwireInterrupt(t *testing.T) {
	b, path := newBreadcrumbTestTracer(t, BreadcrumbConfig{Interval: 10})
	defer b.Close()
	m := newBiosMMIO(nil, nil, nil)
	m.enableBreadcrumb(b)

	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegTripSeq, 8, 2); !ok || fault != nil {
		t.Fatalf("Store trip seq = (%v, %v)", ok, fault)
	}
	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegTripSignal, 4, 3); !ok || fault != nil {
		t.Fatalf("Store trip signal = (%v, %v)", ok, fault)
	}
	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegTripPID, 4, 1234); !ok || fault != nil {
		t.Fatalf("Store trip pid = (%v, %v)", ok, fault)
	}
	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegTripMode, 4, 1); !ok || fault != nil {
		t.Fatalf("Store trip mode = (%v, %v)", ok, fault)
	}

	if err := b.RecordPC(1, 1, 0x5000, PrivUser); err != nil {
		t.Fatalf("RecordPC 1: %v", err)
	}
	if m.breadcrumb.InterruptPending() {
		t.Fatal("trip interrupt pending before target seq")
	}
	if err := b.RecordPC(2, 2, 0x5004, PrivUser); err != nil {
		t.Fatalf("RecordPC 2: %v", err)
	}
	if !m.breadcrumb.InterruptPending() {
		t.Fatal("trip interrupt did not become pending at target seq")
	}
	if got := m.plicPendingBits(); got&(uint32(1)<<biosBreadcrumbIRQ) == 0 {
		t.Fatalf("PLIC pending bits %#x missing breadcrumb IRQ %#x", got, biosBreadcrumbIRQ)
	}
	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegIRQStatus, 4); fault != nil || got != 1 {
		t.Fatalf("IRQ status = (%d, %v), want 1, nil", got, fault)
	}
	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegTripHitSeq, 8); fault != nil || got != 2 {
		t.Fatalf("trip hit seq = (%d, %v), want 2, nil", got, fault)
	}
	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegTripHitAttempt, 8); fault != nil || got != 2 {
		t.Fatalf("trip hit attempt = (%d, %v), want 2, nil", got, fault)
	}
	if got, _, fault := m.Load(biosBreadcrumbBase+breadcrumbRegTripHitPC, 8); fault != nil || got != 0x5004 {
		t.Fatalf("trip hit PC = (%#x, %v), want 0x5004, nil", got, fault)
	}

	if ok, fault := m.Store(biosBreadcrumbBase+breadcrumbRegIRQStatus, 4, 1); !ok || fault != nil {
		t.Fatalf("ack IRQ status = (%v, %v)", ok, fault)
	}
	if m.breadcrumb.InterruptPending() {
		t.Fatal("trip interrupt still pending after ack")
	}

	log := readClosedBreadcrumbLog(t, b, path)
	if !strings.Contains(log, "# trip") || !strings.Contains(log, "signal=3 pid=1234") {
		t.Fatalf("trip event missing from log:\n%s", log)
	}
}

func TestBreadcrumbBiosFDTAdvertisedWhenEnabled(t *testing.T) {
	without, err := buildVirtFDT(Size4GB, virtFDTOptions{})
	if err != nil {
		t.Fatalf("buildVirtFDT without breadcrumb: %v", err)
	}
	if strings.Contains(string(without), "glycerine,riscv-breadcrumb-v1") {
		t.Fatal("breadcrumb compatible appeared without Breadcrumb option")
	}

	with, err := buildVirtFDT(Size4GB, virtFDTOptions{Breadcrumb: true})
	if err != nil {
		t.Fatalf("buildVirtFDT with breadcrumb: %v", err)
	}
	if !strings.Contains(string(with), "glycerine,riscv-breadcrumb-v1") {
		t.Fatal("breadcrumb compatible missing from FDT")
	}
	if !strings.Contains(string(with), "breadcrumb@10002000") {
		t.Fatal("breadcrumb FDT node missing")
	}
}

func TestBreadcrumbBiosDefaultsWaitForGuestMMIO(t *testing.T) {
	cfg := BreadcrumbConfig{Path: filepath.Join(t.TempDir(), "breadcrumbs.log")}
	cfg = cfg.normalizedForEmu(true)
	b, err := NewBreadcrumbTracer(cfg)
	if err != nil {
		t.Fatalf("NewBreadcrumbTracer: %v", err)
	}
	defer b.Close()
	if !b.guestControl {
		t.Fatal("BIOS breadcrumb config did not enable guest control")
	}
	if b.active {
		t.Fatal("BIOS breadcrumb tracer started active before guest MMIO control")
	}
}

func TestBreadcrumbHandBuiltLinuxSleepTraceDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("hand-built Linux breadcrumb trace test is slow")
	}
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	const targetSource = "testvectors/jea9linux/src/breadcrumb_sleep.c"
	for _, path := range []string{biosPath, kernelPath, initrdPath, targetSource} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	target := buildBreadcrumbSleepGuest(t, targetSource)
	const doneMarker = "BREADCRUMB-SLEEP-DONE"
	traces := make([]string, 2)
	for i := range traces {
		tracePath := filepath.Join(t.TempDir(), fmt.Sprintf("breadcrumbs-%d.log", i))
		stdout, stderr := runLinuxBreadcrumbSleepTrace(t, target, tracePath, doneMarker)
		if !strings.Contains(stdout, "BREADCRUMB-SLEEP-RC:0") {
			t.Fatalf("guest breadcrumb sleep exited non-zero\nstdout tail:\n%s\nstderr:\n%s",
				tailString(stdout, 8192), stderr)
		}
		raw, err := os.ReadFile(tracePath)
		if err != nil {
			t.Fatalf("read breadcrumb trace %d: %v\nstdout tail:\n%s\nstderr:\n%s",
				i+1, err, tailString(stdout, 8192), stderr)
		}
		traces[i] = canonicalBreadcrumbHashTrace(string(raw))
		if traces[i] == "" {
			t.Fatalf("breadcrumb trace %d had no checkpoints\nstdout tail:\n%s\nstderr:\n%s\nraw trace:\n%s",
				i+1, tailString(stdout, 8192), stderr, string(raw))
		}
	}
	if traces[0] != traces[1] {
		t.Fatalf("same guest nanosleep process trace differed across runs\n%s", firstBreadcrumbTraceDiff(traces[0], traces[1]))
	}
}

func buildBreadcrumbSleepGuest(t *testing.T, source string) string {
	t.Helper()
	zig, err := osexec.LookPath("zig")
	if err != nil {
		t.Skip("zig is required to build the guest breadcrumb sleep fixture")
	}
	cacheDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "breadcrumb_sleep.elf")
	cmd := osexec.Command(zig,
		"cc",
		"-target", "riscv64-linux-musl",
		"-static",
		"-nostdlib",
		"-fno-builtin",
		"-fno-stack-protector",
		"-fno-sanitize=all",
		"-Wl,-e,_start",
		source,
		"-o", out,
	)
	cmd.Env = append(os.Environ(),
		"ZIG_LOCAL_CACHE_DIR="+filepath.Join(cacheDir, "local"),
		"ZIG_GLOBAL_CACHE_DIR="+filepath.Join(cacheDir, "global"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build guest breadcrumb sleep fixture: %v\n%s", err, output)
	}
	resolved, err := filepath.EvalSymlinks(out)
	if err != nil {
		t.Fatalf("resolve guest breadcrumb sleep fixture path: %v", err)
	}
	return resolved
}

func runLinuxBreadcrumbSleepTrace(t *testing.T, target, tracePath, doneMarker string) (string, string) {
	t.Helper()
	var stdout safeStringWriter
	var stderr bytes.Buffer
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

	guestTarget := "/host" + target
	script := strings.Join([]string{
		"set -e",
		"echo 0 > /proc/sys/kernel/randomize_va_space",
		"(while :; do :; done) &",
		"bg=$!",
		"set +e",
		guestTarget,
		"rc=$?",
		"kill $bg 2>/dev/null",
		"wait $bg 2>/dev/null",
		"set -e",
		"echo BREADCRUMB-SLEEP-R''C:$rc",
		"echo BREADCRUMB-SLEEP-DON''E",
	}, "\n") + "\n"
	go func() {
		defer stdinW.Close()
		deadline := time.Now().Add(linuxAlpineSmokeWallBudget)
		for time.Now().Before(deadline) {
			if linuxInitramfsReady(stdout.String()) {
				_, _ = io.WriteString(stdinW, script)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	ok, err := runBiosUntilOutputWithin(&EmuConfig{
		BiosPath:   "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf",
		KernelPath: "xendor/linux-6.17-hand-built/Image",
		InitrdPath: "xendor/linux/initramfs.cpio.gz",
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Stdin:      stdinR,
		Stdout:     &stdout,
		Stderr:     &stderr,
		Breadcrumb: BreadcrumbConfig{
			Path:     tracePath,
			Interval: 1,
		},
	}, doneMarker, 2_500_000_000, linuxAlpineSmokeWallBudget)
	out := stdout.String()
	if err != nil {
		t.Fatalf("hand-built Linux breadcrumb sleep err: %v\nstdout tail:\n%s\nstderr:\n%s",
			err, tailString(out, 8192), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux breadcrumb sleep marker missing\nstdout tail:\n%s\nstderr:\n%s",
			tailString(out, 8192), stderr.String())
	}
	return out, stderr.String()
}

func canonicalBreadcrumbHashTrace(log string) string {
	var out strings.Builder
	for _, line := range strings.Split(log, "\n") {
		if !strings.HasPrefix(line, "seq=") {
			continue
		}
		seq := breadcrumbLogField(line, "seq")
		priv := breadcrumbLogField(line, "priv")
		pc := breadcrumbLogField(line, "pc")
		hash := breadcrumbLogField(line, "hash")
		if seq == "" || priv == "" || pc == "" || hash == "" {
			continue
		}
		fmt.Fprintf(&out, "seq=%s priv=%s pc=%s hash=%s\n", seq, priv, pc, hash)
	}
	return out.String()
}

func firstBreadcrumbTraceDiff(a, b string) string {
	alines := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
	blines := strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	n := len(alines)
	if len(blines) < n {
		n = len(blines)
	}
	for i := 0; i < n; i++ {
		if alines[i] != blines[i] {
			return fmt.Sprintf("first mismatch at checkpoint line %d\nfirst:  %s\nsecond: %s", i+1, alines[i], blines[i])
		}
	}
	if len(alines) != len(blines) {
		return fmt.Sprintf("checkpoint line count differs: first=%d second=%d", len(alines), len(blines))
	}
	return "traces differ"
}

func breadcrumbLogField(line, key string) string {
	prefix := key + "="
	for _, field := range strings.Fields(line) {
		if strings.HasPrefix(field, prefix) {
			return strings.TrimPrefix(field, prefix)
		}
	}
	return ""
}
