//go:build breadcrumb

package riscv

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := b.afterAttempt(10, 9, 0x1111, PrivUser, PrivUser); err != nil {
		t.Fatalf("afterAttempt arm write: %v", err)
	}
	if err := b.afterAttempt(11, 10, 0x1115, PrivUser, PrivUser); err != nil {
		t.Fatalf("afterAttempt user before privilege interlude: %v", err)
	}
	if b.active {
		t.Fatal("arm-next-user activated before seeing a privileged interlude")
	}
	if err := b.afterAttempt(12, 11, 0x2222, PrivSupervisor, PrivSupervisor); err != nil {
		t.Fatalf("afterAttempt supervisor work: %v", err)
	}
	if err := b.afterAttempt(13, 12, 0x3333, PrivSupervisor, PrivUser); err != nil {
		t.Fatalf("afterAttempt sret: %v", err)
	}
	if !b.active {
		t.Fatal("arm-next-user did not activate on return to user mode")
	}
	if err := b.afterAttempt(14, 13, 0x4444, PrivUser, PrivUser); err != nil {
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
	if err := b.afterAttempt(1, 1, 0x6000, PrivUser, PrivUser); err != nil {
		t.Fatalf("afterAttempt MMIO store: %v", err)
	}
	if !b.active {
		t.Fatal("guest control start did not activate after MMIO store instruction")
	}
	if err := b.afterAttempt(2, 2, 0x6004, PrivUser, PrivUser); err != nil {
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
