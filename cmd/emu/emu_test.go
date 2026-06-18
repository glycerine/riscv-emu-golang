package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	riscv "github.com/glycerine/riscv-emu-golang"
)

func TestRunEmuDefaultRunsGoHelloFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmu(EmuConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/hello.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmu: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "hello jea9linux go\n"; got != want {
		t.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
	}
}

func TestRunEmuReturnsGuestExitCodeAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmu(EmuConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/nilpanic.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmu: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "panic: runtime error") {
		t.Fatalf("stderr = %q, want Go panic text", stderr.String())
	}
}

func TestRunEmuSeedControlsGetrandom(t *testing.T) {
	first := runEmuFixtureOutput(t, 1234)
	second := runEmuFixtureOutput(t, 1234)
	third := runEmuFixtureOutput(t, 5678)

	if first != second {
		t.Fatalf("same seed output differs: %q != %q", first, second)
	}
	if first == third {
		t.Fatalf("different seeds produced matching output: %q", first)
	}
}

func TestEmuEnvDefaultsFollowHermitMode(t *testing.T) {
	t.Setenv("JEA9_EMU_ENV_TEST_MARKER", "inherited")

	nonHermit := EmuConfig{}.withDefaults()
	if !envHas(nonHermit.Env, "JEA9_EMU_ENV_TEST_MARKER=inherited") {
		t.Fatalf("non-hermit Env did not inherit marker: %q", nonHermit.Env)
	}

	hermit := EmuConfig{Hermit: true}.withDefaults()
	if len(hermit.Env) != 0 {
		t.Fatalf("hermit Env len = %d, want 0; Env=%q", len(hermit.Env), hermit.Env)
	}

	explicitEmpty := EmuConfig{Env: []string{}}.withDefaults()
	if len(explicitEmpty.Env) != 0 {
		t.Fatalf("explicit empty Env len = %d, want 0; Env=%q", len(explicitEmpty.Env), explicitEmpty.Env)
	}
}

func TestEmuTimeModeFollowsHermitFlag(t *testing.T) {
	if got := (EmuConfig{}).timeMode(); got != riscv.RealTime {
		t.Fatalf("default emu time mode = %v, want RealTime", got)
	}
	if got := (EmuConfig{Hermit: true}).timeMode(); got != riscv.HermitTime {
		t.Fatalf("hermit emu time mode = %v, want HermitTime", got)
	}
}

func TestEmuBiosFlagIsSeparateFromRun(t *testing.T) {
	const path = "../../testvectors/jea9linux/elf/write_stdout.elf"
	cfg, _, _ := parseEmuConfigForTest(t,
		"-bios", path,
	)
	if cfg.RunPath != "" {
		t.Fatalf("-bios populated RunPath = %q", cfg.RunPath)
	}
	if got, want := cfg.BiosPath, path; got != want {
		t.Fatalf("BiosPath = %q, want %q", got, want)
	}

	empty := EmuConfig{}
	if err := empty.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted missing -run/-bios path")
	}
	both := EmuConfig{
		RunPath:  "../../testvectors/jea9linux/elf/write_stdout.elf",
		BiosPath: "../../xendor/opensbi/build/platform/generic/firmware/fw_jump.elf",
	}
	if err := both.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted both -run and -bios")
	}
}

func TestEmuBiosBootFlagsParse(t *testing.T) {
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	initrdPath := filepath.Join(dir, "rootfs.cpio")
	dumpDTBPath := filepath.Join(dir, "virt.dtb")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrdPath, []byte("initrd"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, _, _ := parseEmuConfigForTest(t,
		"-bios", "../../testvectors/jea9linux/elf/write_stdout.elf",
		"-kernel", kernelPath,
		"-kernel-addr", "0x80400000",
		"-initrd", initrdPath,
		"-append", "console=hvc0 root=/dev/ram0",
		"-dump-dtb", dumpDTBPath,
		"-machine", "virt",
		"-mem", "512mb",
	)
	if cfg.BiosPath == "" || cfg.RunPath != "" {
		t.Fatalf("parsed BiosPath=%q RunPath=%q", cfg.BiosPath, cfg.RunPath)
	}
	if cfg.KernelPath != kernelPath || cfg.KernelAddr != 0x80400000 {
		t.Fatalf("parsed kernel path/addr = %q/%#x", cfg.KernelPath, cfg.KernelAddr)
	}
	if cfg.InitrdPath != initrdPath {
		t.Fatalf("InitrdPath = %q, want %q", cfg.InitrdPath, initrdPath)
	}
	if cfg.Append != "console=hvc0 root=/dev/ram0" {
		t.Fatalf("Append = %q", cfg.Append)
	}
	if cfg.DumpDTBPath != dumpDTBPath {
		t.Fatalf("DumpDTBPath = %q, want %q", cfg.DumpDTBPath, dumpDTBPath)
	}
	if cfg.machine() != "virt" {
		t.Fatalf("machine = %q, want virt", cfg.machine())
	}
	if cfg.BiosRAMSize != riscv.Size512MB {
		t.Fatalf("BiosRAMSize = %d, want %d", cfg.BiosRAMSize, riscv.Size512MB)
	}
	if cfg.MemorySize != riscv.Size4GB {
		t.Fatalf("MemorySize slab = %d, want %d", cfg.MemorySize, riscv.Size4GB)
	}
	budget, err := cfg.schedulerBudget()
	if err != nil {
		t.Fatalf("schedulerBudget: %v", err)
	}
	if budget != ^uint64(0) {
		t.Fatalf("BIOS default schedulerBudget = %d, want max", budget)
	}

	runWithKernel := EmuConfig{
		RunPath:    "../../testvectors/jea9linux/elf/write_stdout.elf",
		KernelPath: kernelPath,
	}
	if err := runWithKernel.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted -kernel with -run")
	}
}

func TestParseEmuMemorySize(t *testing.T) {
	tests := []struct {
		raw  string
		want uint64
	}{
		{"1024", 1024},
		{"0x400", 1024},
		{"512mb", riscv.Size512MB},
		{"512MB", riscv.Size512MB},
		{"512 MiB", riscv.Size512MB},
		{"2GB", riscv.Size2GB},
	}
	for _, tt := range tests {
		got, err := parseEmuMemorySize(tt.raw)
		if err != nil {
			t.Fatalf("parseEmuMemorySize(%q): %v", tt.raw, err)
		}
		if got != tt.want {
			t.Fatalf("parseEmuMemorySize(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestPrepareBiosGuestLoadsKernelInitrdAndBootArgs(t *testing.T) {
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	initrdPath := filepath.Join(dir, "rootfs.cpio")
	dumpDTBPath := filepath.Join(dir, "virt.dtb")
	kernel := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	initrd := []byte("initrd-data")
	if err := os.WriteFile(kernelPath, kernel, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrdPath, initrd, 0644); err != nil {
		t.Fatal(err)
	}

	guest, err := prepareBiosGuest(EmuConfig{
		BiosPath:    "../../testvectors/jea9linux/elf/write_stdout.elf",
		KernelPath:  kernelPath,
		KernelAddr:  0x80400000,
		InitrdPath:  initrdPath,
		Append:      "console=hvc0 root=/dev/ram0",
		DumpDTBPath: dumpDTBPath,
		Memory:      "512MB",
		Stdin:       strings.NewReader(""),
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
	}.withDefaults())
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()
	if got := guest.mem.Size(); got != riscv.Size4GB {
		t.Fatalf("guest memory slab = %d, want %d", got, riscv.Size4GB)
	}

	if got := guestMemoryBytes(t, guest.mem, guest.kernel.addr, len(kernel)); !bytes.Equal(got, kernel) {
		t.Fatalf("kernel bytes at %#x = %x, want %x", guest.kernel.addr, got, kernel)
	}
	if got := guestMemoryBytes(t, guest.mem, guest.initrd.addr, len(initrd)); !bytes.Equal(got, initrd) {
		t.Fatalf("initrd bytes at %#x = %x, want %x", guest.initrd.addr, got, initrd)
	}
	if !bytes.Contains(guest.fdt, []byte("console=hvc0 root=/dev/ram0\x00")) {
		t.Fatalf("generated FDT does not contain bootargs: %x", guest.fdt)
	}
	if !bytes.Contains(guest.fdt, fdtU64(riscv.Size512MB)) {
		t.Fatalf("generated FDT does not contain 512MB RAM size")
	}
	if !bytes.Contains(guest.fdt, fdtU64(guest.initrd.addr)) || !bytes.Contains(guest.fdt, fdtU64(guest.initrd.end)) {
		t.Fatalf("generated FDT does not contain initrd range %#x..%#x", guest.initrd.addr, guest.initrd.end)
	}
	if !bytes.Contains(guest.fdt, []byte("riscv,isa-base\x00")) ||
		!bytes.Contains(guest.fdt, []byte("riscv,isa-extensions\x00")) ||
		!bytes.Contains(guest.fdt, []byte("zba\x00")) ||
		!bytes.Contains(guest.fdt, []byte("zbb\x00")) ||
		!bytes.Contains(guest.fdt, []byte("zbc\x00")) ||
		!bytes.Contains(guest.fdt, []byte("zicond\x00")) ||
		!bytes.Contains(guest.fdt, []byte("sstc\x00")) ||
		!bytes.Contains(guest.fdt, []byte("rv64imafdcsu_zba_zbb_zbc_zicond_zicsr_zifencei_sstc\x00")) {
		t.Fatalf("generated FDT does not advertise expected ISA extensions")
	}
	if !bytes.Contains(guest.fdt, []byte("syscon-reboot\x00")) ||
		!bytes.Contains(guest.fdt, []byte("syscon\x00")) ||
		!bytes.Contains(guest.fdt, fdtU64(biosSysconBase)) {
		t.Fatalf("generated FDT does not advertise BIOS syscon reset")
	}
	dumped, err := os.ReadFile(dumpDTBPath)
	if err != nil {
		t.Fatalf("read dumped dtb: %v", err)
	}
	if !bytes.Equal(dumped, guest.fdt) {
		t.Fatalf("dumped DTB differs from guest FDT")
	}
}

func TestPrepareBiosGuestUsesExternalDTB(t *testing.T) {
	dir := t.TempDir()
	dtbPath := filepath.Join(dir, "external.dtb")
	dumpDTBPath := filepath.Join(dir, "dumped.dtb")
	fdt, err := buildVirtFDT(riscv.Size16GB, virtFDTOptions{BootArgs: "from-external-dtb"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dtbPath, fdt, 0644); err != nil {
		t.Fatal(err)
	}

	guest, err := prepareBiosGuest(EmuConfig{
		BiosPath:    "../../testvectors/jea9linux/elf/write_stdout.elf",
		DTBPath:     dtbPath,
		DumpDTBPath: dumpDTBPath,
		MemorySize:  riscv.Size16GB,
		Stdin:       strings.NewReader(""),
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
	}.withDefaults())
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()

	if !guest.externalDTB {
		t.Fatal("prepareBiosGuest did not mark external DTB")
	}
	if !bytes.Equal(guest.fdt, fdt) {
		t.Fatalf("guest FDT differs from external DTB")
	}
	dumped, err := os.ReadFile(dumpDTBPath)
	if err != nil {
		t.Fatalf("read dumped dtb: %v", err)
	}
	if !bytes.Equal(dumped, fdt) {
		t.Fatalf("dumped external DTB differs from input")
	}
}

func TestBiosUARTTransmitInterruptThroughPLIC(t *testing.T) {
	var stdout bytes.Buffer
	m := newBiosMMIO(strings.NewReader(""), &stdout, nil)

	m.storePLIC(4*uint64(biosUARTIRQ), 4, 1)
	m.storePLIC(0x2000+0x80*uint64(plicSContext), 4, uint64(1)<<biosUARTIRQ)
	m.storePLIC(0x200000+0x1000*uint64(plicSContext), 4, 0)

	if m.SupervisorExternalInterruptPending() {
		t.Fatal("PLIC reported UART interrupt before THRI was enabled")
	}

	m.storeUART(1, 1, uint64(uartIERTHRI))
	if !m.SupervisorExternalInterruptPending() {
		t.Fatal("PLIC did not report UART interrupt after THRI enable")
	}
	if got := m.loadPLIC(0x200000+0x1000*uint64(plicSContext)+4, 4); got != uint64(biosUARTIRQ) {
		t.Fatalf("PLIC claim = %d, want UART IRQ %d", got, biosUARTIRQ)
	}
	if got := m.loadUART(2, 1); byte(got) != uartIIRTHRI {
		t.Fatalf("UART IIR = 0x%x, want THRI 0x%x", got, uartIIRTHRI)
	}
	if m.SupervisorExternalInterruptPending() {
		t.Fatal("UART interrupt still pending after IIR read while claimed")
	}

	m.storeUART(0, 1, 'A')
	m.storePLIC(0x200000+0x1000*uint64(plicSContext)+4, 4, uint64(biosUARTIRQ))
	if stdout.String() != "A" {
		t.Fatalf("stdout = %q, want A", stdout.String())
	}
	if !m.SupervisorExternalInterruptPending() {
		t.Fatal("UART THR write did not reassert transmit interrupt")
	}
}

func TestBiosUARTAsyncOutputFlushesPromptWithoutNewline(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	m := newBiosMMIO(nil, pw, nil)
	defer func() {
		m.closeUARTOutput()
		_ = pw.Close()
	}()

	gotCh := make(chan string, 1)
	go func() {
		buf := make([]byte, len("prompt> "))
		n, err := io.ReadFull(pr, buf)
		if err != nil {
			gotCh <- fmt.Sprintf("read error after %d bytes: %v", n, err)
			return
		}
		gotCh <- string(buf)
	}()

	for _, b := range []byte("prompt> ") {
		m.storeUART(0, 1, uint64(b))
	}
	select {
	case got := <-gotCh:
		if got != "prompt> " {
			t.Fatalf("async UART output = %q, want prompt", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("async UART output did not flush prompt without newline")
	}
}

func TestBiosUARTReceiveInterruptThroughPLIC(t *testing.T) {
	m := newBiosMMIO(nil, io.Discard, nil)
	m.uartRX = append(m.uartRX, "ls\n"...)

	m.storePLIC(4*uint64(biosUARTIRQ), 4, 1)
	m.storePLIC(0x2000+0x80*uint64(plicSContext), 4, uint64(1)<<biosUARTIRQ)
	m.storePLIC(0x200000+0x1000*uint64(plicSContext), 4, 0)

	if m.SupervisorExternalInterruptPending() {
		t.Fatal("PLIC reported UART receive interrupt before RDI was enabled")
	}

	m.storeUART(1, 1, uint64(uartIERRDI))
	if !m.SupervisorExternalInterruptPending() {
		t.Fatal("PLIC did not report UART receive interrupt after RDI enable")
	}
	if got := m.loadUART(5, 1); byte(got)&uartLSRDR == 0 {
		t.Fatalf("UART LSR = 0x%x, want data-ready", got)
	}
	if got := m.loadPLIC(0x200000+0x1000*uint64(plicSContext)+4, 4); got != uint64(biosUARTIRQ) {
		t.Fatalf("PLIC claim = %d, want UART IRQ %d", got, biosUARTIRQ)
	}
	if got := m.loadUART(2, 1); byte(got) != uartIIRRDI {
		t.Fatalf("UART IIR = 0x%x, want RDI 0x%x", got, uartIIRRDI)
	}
	for _, want := range []byte("ls\n") {
		if got := m.loadUART(0, 1); byte(got) != want {
			t.Fatalf("UART RBR = %q, want %q", byte(got), want)
		}
	}
	if got := m.loadUART(5, 1); byte(got)&uartLSRDR != 0 {
		t.Fatalf("UART LSR = 0x%x, want data-ready clear", got)
	}
	m.storePLIC(0x200000+0x1000*uint64(plicSContext)+4, 4, uint64(biosUARTIRQ))
	if m.SupervisorExternalInterruptPending() {
		t.Fatal("UART receive interrupt still pending after draining input")
	}
}

func TestBiosUARTInputReaderFeedsReceiveFIFO(t *testing.T) {
	m := newBiosMMIO(strings.NewReader("x\n"), io.Discard, nil)
	deadline := time.Now().Add(time.Second)
	for len(m.uartRX) < 2 && time.Now().Before(deadline) {
		m.drainUARTInput()
	}
	if len(m.uartRX) < 2 {
		t.Fatalf("UART RX len = %d, want stdin bytes", len(m.uartRX))
	}
	if got := m.loadUART(5, 1); byte(got)&uartLSRDR == 0 {
		t.Fatalf("UART LSR = 0x%x, want data-ready", got)
	}
	if got := m.loadUART(0, 1); byte(got) != 'x' {
		t.Fatalf("UART RBR first byte = %q, want x", byte(got))
	}
	if got := m.loadUART(0, 1); byte(got) != '\n' {
		t.Fatalf("UART RBR second byte = %q, want newline", byte(got))
	}
}

func TestBiosSysconResetInvokesCallback(t *testing.T) {
	calls := 0
	m := newBiosMMIO(nil, io.Discard, func() {
		calls++
	})

	if ok, fault := m.Store(biosSysconBase+uint64(biosSysconResetOffset), 4, uint64(biosSysconResetValue)); !ok || fault != nil {
		t.Fatalf("syscon reset store ok=%v fault=%v, want handled without fault", ok, fault)
	}
	if calls != 1 {
		t.Fatalf("reset callback calls = %d, want 1", calls)
	}
	if ok, fault := m.Store(biosSysconBase+8, 4, 0); !ok || fault != nil {
		t.Fatalf("syscon non-reset store ok=%v fault=%v, want handled without fault", ok, fault)
	}
	if calls != 1 {
		t.Fatalf("reset callback calls after non-reset store = %d, want 1", calls)
	}
}

func TestEnableRawTerminalIgnoresNonFileStdin(t *testing.T) {
	restore, raw, err := enableRawTerminal(strings.NewReader(""))
	if err != nil {
		t.Fatalf("enableRawTerminal: %v", err)
	}
	if raw {
		t.Fatal("enableRawTerminal entered raw mode for non-file stdin")
	}
	if restore != nil {
		t.Fatal("enableRawTerminal returned restore callback for non-file stdin")
	}
}

func TestPrepareBiosGuestRejectsFwJumpFDTKernelOverlap(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_jump.elf"
	if !fileExists(biosPath) {
		t.Skipf("OpenSBI fw_jump fixture not present: %s", biosPath)
	}
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	if err := os.WriteFile(kernelPath, bytes.Repeat([]byte{0x6f}, 8192), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := prepareBiosGuest(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		KernelAddr: fwJumpGenericFDTAddr - 4096,
		MemorySize: riscv.Size16GB,
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}.withDefaults())
	if err == nil {
		t.Fatal("prepareBiosGuest accepted a fw_jump FDT/kernel overlap")
	}
	if !strings.Contains(err.Error(), "fw_dynamic.elf") {
		t.Fatalf("overlap error = %v, want fw_dynamic guidance", err)
	}
}

func TestPrepareBiosGuestFWDynamicSetsInfoBlock(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	if !fileExists(biosPath) {
		t.Skipf("OpenSBI fw_dynamic fixture not present: %s", biosPath)
	}
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	kernel := []byte{0x4d, 0x5a, 0x6f, 0x10}
	if err := os.WriteFile(kernelPath, kernel, 0644); err != nil {
		t.Fatal(err)
	}

	guest, err := prepareBiosGuest(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		MemorySize: riscv.Size16GB,
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}.withDefaults())
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()

	if guest.dynamicAddr == 0 {
		t.Fatal("fw_dynamic info block address is zero")
	}
	if got := guest.cpu.Reg(12); got != guest.dynamicAddr {
		t.Fatalf("a2 = %#x, want dynamic info addr %#x", got, guest.dynamicAddr)
	}
	if got := loadLittleEndianU64(t, guest.mem, guest.dynamicAddr); got != fwDynamicInfoMagic {
		t.Fatalf("dynamic magic = %#x, want %#x", got, fwDynamicInfoMagic)
	}
	if got := loadLittleEndianU64(t, guest.mem, guest.dynamicAddr+16); got != guest.nextAddr {
		t.Fatalf("dynamic next_addr = %#x, want %#x", got, guest.nextAddr)
	}
	if got := loadLittleEndianU64(t, guest.mem, guest.dynamicAddr+24); got != fwDynamicNextModeS {
		t.Fatalf("dynamic next_mode = %#x, want S-mode %#x", got, fwDynamicNextModeS)
	}
}

func TestRunEmuBiosFWDynamicHandsOffToTinySModeImage(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	if !fileExists(biosPath) {
		t.Skipf("OpenSBI fw_dynamic fixture not present: %s", biosPath)
	}

	const (
		sentinelOffset = uint64(64)
		sentinelMagic  = uint64(0x12345678)
	)
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	if err := os.WriteFile(kernelPath, tinySModeSentinelImage(sentinelOffset), 0644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	guest, err := prepareBiosGuest(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}.withDefaults())
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()

	sentinelAddr := defaultBiosKernelAddr + sentinelOffset
	if got := loadLittleEndianU64(t, guest.mem, sentinelAddr); got == sentinelMagic {
		t.Fatalf("sentinel at %#x already equals magic before OpenSBI runs", sentinelAddr)
	}
	if err := runBiosUntilMagic(guest, sentinelAddr, sentinelMagic, 25_000_000); err != nil {
		t.Fatalf("OpenSBI did not hand off to tiny S-mode Image: %v\nstdout tail:\n%s\nstderr:\n%s",
			err, tailString(stdout.String(), 4096), stderr.String())
	}
	if got := guest.cpu.PrivilegeMode(); got != riscv.PrivSupervisor {
		t.Fatalf("privilege after sentinel = %v, want supervisor; state=%+v", got, guest.cpu.DebugSnapshot())
	}
	if got := guest.cpu.Reg(10); got != 0 {
		t.Fatalf("a0/hartid after handoff = %#x, want 0", got)
	}
	if got := guest.cpu.Reg(11); got != guest.fdtAddr {
		t.Fatalf("a1/FDT after handoff = %#x, want %#x", got, guest.fdtAddr)
	}
	if got := guest.cpu.PC(); got < defaultBiosKernelAddr || got >= defaultBiosKernelAddr+uint64(len(guest.kernel.data)) {
		t.Fatalf("PC after sentinel = %#x, want inside tiny Image [%#x,%#x); state=%+v",
			got, defaultBiosKernelAddr, defaultBiosKernelAddr+uint64(len(guest.kernel.data)), guest.cpu.DebugSnapshot())
	}
	if !strings.Contains(stdout.String(), "Domain0 Next Mode           : S-mode") {
		t.Fatalf("OpenSBI did not report S-mode handoff\nstdout tail:\n%s", tailString(stdout.String(), 4096))
	}
}

func TestRunEmuBiosFWDynamicHandBuiltLinuxPrintsBanner(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux-6.17-hand-built/Image"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, "Linux version 6.17.0", 10_000_000)
	if err != nil {
		t.Fatalf("hand-built Linux banner err = %v\nstdout tail:\n%s\nstderr:\n%s",
			err, tailString(stdout.String(), 4096), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux banner missing\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
}

func TestRunEmuBiosFWDynamicHandBuiltLinuxBootsToInitUnder5s(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux-6.17-hand-built/Image"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	ok, err := runBiosUntilOutputWithin(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, "Run /init as init process", 2_000_000_000, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("hand-built Linux /init boot err after %s = %v\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, err, tailString(stdout.String(), 4096), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux /init marker missing after %s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(stdout.String(), 4096), stderr.String())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hand-built Linux boot to /init took %s, want <= 5s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(stdout.String(), 4096), stderr.String())
	}
	t.Logf("hand-built Linux booted to /init in %s", elapsed)
}

func TestDiagRunEmuBiosFWDynamicHandBuiltLinuxInitcallDebug(t *testing.T) {
	if os.Getenv("RISCV_EMU_LINUX_INITCALL_DIAG") == "" {
		t.Skip("set RISCV_EMU_LINUX_INITCALL_DIAG=1 to profile hand-built Linux initcalls")
	}
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux-6.17-hand-built/Image"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs + " initcall_debug ignore_loglevel loglevel=8",
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, "Run /init as init process", 2_000_000_000)
	t.Logf("slowest initcalls:\n%s", slowInitcallReport(stdout.String(), 30))
	t.Logf("stdout tail:\n%s", tailString(stdout.String(), 8192))
	if err != nil {
		t.Fatalf("hand-built Linux initcall diag err = %v\nstderr:\n%s", err, stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux initcall diag marker missing\nstderr:\n%s", stderr.String())
	}
}

func TestRunEmuBiosFWDynamicLinuxSmoke(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS smoke fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	code, err := runEmu(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Budget:     "100000",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if !errors.Is(err, errBiosBudgetExpired) {
		t.Fatalf("runEmu fw_dynamic linux smoke err = %v, want errBiosBudgetExpired; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("runEmu fw_dynamic linux smoke exit code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

const linuxUARTBootArgs = "console=ttyS0,115200 earlycon=uart8250,mmio,0x10000000 rdinit=/init"

func TestRunEmuBiosFWDynamicLinuxBootsWith512MBRAM(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS boot fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, "Total pages: 131072", 400_000_000)
	if err != nil {
		t.Fatalf("Linux boot milestone err = %v\nstdout tail:\n%s\nstderr:\n%s",
			err, tailString(stdout.String(), 4096), stderr.String())
	}
	if !ok {
		t.Fatalf("Linux boot milestone not reached\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Domain0 Next Mode           : S-mode") {
		t.Fatalf("OpenSBI did not report S-mode handoff\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Boot HART ISA Extensions    : sstc") {
		t.Fatalf("OpenSBI did not report Sstc support\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Platform Reboot Device      : syscon-reboot") {
		t.Fatalf("OpenSBI did not register syscon reboot\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Standard SBI Extensions     :") ||
		!strings.Contains(stdout.String(), "srst") {
		t.Fatalf("OpenSBI did not advertise SRST\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Booting Linux on hartid 0") {
		t.Fatalf("Linux kernel banner missing\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
	if !strings.Contains(stdout.String(), "node   0: [mem 0x0000000080060000-0x000000009fffffff]") {
		t.Fatalf("Linux did not see the expected 512MB RAM range\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
}

func TestRunEmuBiosFWDynamicLinuxPassesTimerProbe(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS boot fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, "Freeing unused kernel image", 1_600_000_000)
	if err != nil {
		t.Fatalf("Linux timer milestone err = %v\nstdout tail:\n%s\nstderr:\n%s",
			err, tailString(stdout.String(), 4096), stderr.String())
	}
	if !ok {
		t.Fatalf("Linux timer milestone not reached\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
}

func runBiosUntilOutput(cfg EmuConfig, marker string, maxInstructions uint64) (bool, error) {
	return runBiosUntilOutputWithin(cfg, marker, maxInstructions, 0)
}

func runBiosUntilOutputWithin(cfg EmuConfig, marker string, maxInstructions uint64, maxElapsed time.Duration) (bool, error) {
	guest, err := prepareBiosGuest(cfg.withDefaults())
	if err != nil {
		return false, err
	}
	defer guest.mem.Free()

	const chunk = uint64(100_000)
	start := time.Now()
	var used uint64
	for used < maxInstructions {
		step := chunk
		if rem := maxInstructions - used; rem < step {
			step = rem
		}
		res, err := riscv.RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, step)
		used += step
		if strings.Contains(writerString(cfg.Stdout), marker) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("%w at pc=%#x insn=%#x state=%+v", err, guest.cpu.PC(), guestInsnForTest(guest.mem, guest.cpu.PC()), guest.cpu.DebugSnapshot())
		}
		if res == riscv.RunBudgetExit {
			return strings.Contains(writerString(cfg.Stdout), marker), nil
		}
		if maxElapsed > 0 && time.Since(start) > maxElapsed {
			return false, fmt.Errorf("%w after %d instructions and %s at pc=%#x insn=%#x state=%+v",
				errBiosBudgetExpired, used, time.Since(start), guest.cpu.PC(),
				guestInsnForTest(guest.mem, guest.cpu.PC()), guest.cpu.DebugSnapshot())
		}
	}
	return false, fmt.Errorf("%w after %d instructions at pc=%#x insn=%#x state=%+v", errBiosBudgetExpired, maxInstructions, guest.cpu.PC(), guestInsnForTest(guest.mem, guest.cpu.PC()), guest.cpu.DebugSnapshot())
}

func writerString(w interface{}) string {
	if s, ok := w.(interface{ String() string }); ok {
		return s.String()
	}
	return ""
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func slowInitcallReport(output string, limit int) string {
	type entry struct {
		name  string
		usecs int64
	}
	re := regexp.MustCompile(`initcall (.+?) returned -?\d+ after ([0-9]+) usecs`)
	matches := re.FindAllStringSubmatch(output, -1)
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		usecs, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, entry{name: m[1], usecs: usecs})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].usecs > entries[j].usecs
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	if len(entries) == 0 {
		return "(no initcall_debug timings found)"
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%8d usecs  %s\n", e.usecs, e.name)
	}
	return strings.TrimRight(b.String(), "\n")
}

func guestInsnForTest(mem *riscv.GuestMemory, pc uint64) uint32 {
	insn, fault := mem.Load32(pc)
	if fault != nil {
		return 0
	}
	return insn
}

func runBiosUntilMagic(guest *biosGuest, addr, magic, maxInstructions uint64) error {
	const chunk = uint64(100_000)
	var used uint64
	for used < maxInstructions {
		step := chunk
		if rem := maxInstructions - used; rem < step {
			step = rem
		}
		res, err := riscv.RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, step)
		used += step
		got, fault := guest.mem.Load64(addr)
		if fault != nil {
			return fmt.Errorf("loading sentinel at %#x: %w", addr, fault)
		}
		if got == magic {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w after %d instructions at pc=%#x insn=%#x state=%+v sentinel=%#x",
				err, used, guest.cpu.PC(), guestInsnForTest(guest.mem, guest.cpu.PC()), guest.cpu.DebugSnapshot(), got)
		}
		if res == riscv.RunBudgetExit {
			return fmt.Errorf("BIOS exited after %d instructions before sentinel %#x became %#x; pc=%#x state=%+v sentinel=%#x",
				used, addr, magic, guest.cpu.PC(), guest.cpu.DebugSnapshot(), got)
		}
	}
	got, fault := guest.mem.Load64(addr)
	if fault != nil {
		return fmt.Errorf("loading sentinel at %#x: %w", addr, fault)
	}
	return fmt.Errorf("%w after %d instructions at pc=%#x insn=%#x state=%+v sentinel=%#x want=%#x",
		errBiosBudgetExpired, maxInstructions, guest.cpu.PC(), guestInsnForTest(guest.mem, guest.cpu.PC()), guest.cpu.DebugSnapshot(), got, magic)
}

func tinySModeSentinelImage(sentinelOffset uint64) []byte {
	var image [72]byte
	putRV32(image[0:4], rvAUIPC(5, 0))                       // auipc t0, 0
	putRV32(image[4:8], rvADDI(5, 5, int32(sentinelOffset))) // addi  t0, t0, sentinelOffset
	putRV32(image[8:12], rvLUI(6, 0x12345))                  // lui   t1, 0x12345
	putRV32(image[12:16], rvADDI(6, 6, 0x678))               // addi  t1, t1, 0x678
	putRV32(image[16:20], rvSD(5, 6, 0))                     // sd    t1, 0(t0)
	putRV32(image[20:24], rvJAL(0, 0))                       // jal   x0, 0
	return image[:]
}

func putRV32(dst []byte, insn uint32) {
	binary.LittleEndian.PutUint32(dst, insn)
}

func rvAUIPC(rd uint8, imm20 uint32) uint32 {
	return (imm20 << 12) | (uint32(rd) << 7) | 0x17
}

func rvLUI(rd uint8, imm20 uint32) uint32 {
	return (imm20 << 12) | (uint32(rd) << 7) | 0x37
}

func rvADDI(rd, rs1 uint8, imm int32) uint32 {
	return ((uint32(imm) & 0xfff) << 20) | (uint32(rs1) << 15) | (uint32(rd) << 7) | 0x13
}

func rvSD(rs1, rs2 uint8, imm int32) uint32 {
	imm12 := uint32(imm) & 0xfff
	return ((imm12 >> 5) << 25) | (uint32(rs2) << 20) | (uint32(rs1) << 15) | (3 << 12) | ((imm12 & 0x1f) << 7) | 0x23
}

func rvJAL(rd uint8, imm int32) uint32 {
	uimm := uint32(imm)
	return (((uimm >> 20) & 0x1) << 31) |
		(((uimm >> 1) & 0x3ff) << 21) |
		(((uimm >> 11) & 0x1) << 20) |
		(((uimm >> 12) & 0xff) << 12) |
		(uint32(rd) << 7) |
		0x6f
}

func TestRunEmuBiosOpenSBIFwJumpGetsFDT(t *testing.T) {
	const path = "../../xendor/opensbi/build/platform/generic/firmware/fw_jump.elf"
	if !fileExists(path) {
		t.Skipf("OpenSBI firmware fixture not present: %s", path)
	}

	guest, err := prepareBiosGuest(EmuConfig{
		BiosPath:   path,
		MemorySize: riscv.Size16GB,
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}.withDefaults())
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()

	if got := guest.cpu.Reg(10); got != 0 {
		t.Fatalf("a0/hartid = %d, want 0", got)
	}
	fdtAddr := guest.cpu.Reg(11)
	if fdtAddr == 0 {
		t.Fatal("a1/FDT pointer is zero")
	}
	if got := guest.cpu.PC(); got != guest.elf.Entry {
		t.Fatalf("PC = 0x%x, want ELF entry 0x%x", got, guest.elf.Entry)
	}
	magic, fault := loadBigEndianU32(guest.mem, fdtAddr)
	if fault != nil {
		t.Fatalf("loading FDT magic at 0x%x: %v", fdtAddr, fault)
	}
	if magic != 0xd00dfeed {
		t.Fatalf("FDT magic at 0x%x = 0x%08x, want 0xd00dfeed", fdtAddr, magic)
	}

	_, err = riscv.RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, 256)
	if isNullFDTFault(err) {
		t.Fatalf("OpenSBI still dereferenced a null FDT pointer: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code, err := runEmu(EmuConfig{
		BiosPath:   path,
		MemorySize: riscv.Size16GB,
		Budget:     "256",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if !errors.Is(err, errBiosBudgetExpired) {
		t.Fatalf("runEmu -bios err = %v, want errBiosBudgetExpired; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("runEmu -bios exit code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func loadBigEndianU32(mem *riscv.GuestMemory, addr uint64) (uint32, *riscv.MemFault) {
	var raw [4]byte
	for i := range raw {
		v, fault := mem.Load8(addr + uint64(i))
		if fault != nil {
			return 0, fault
		}
		raw[i] = v
	}
	return binary.BigEndian.Uint32(raw[:]), nil
}

func guestMemoryBytes(t *testing.T, mem *riscv.GuestMemory, addr uint64, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	for i := range out {
		v, fault := mem.Load8(addr + uint64(i))
		if fault != nil {
			t.Fatalf("Load8 at %#x: %v", addr+uint64(i), fault)
		}
		out[i] = v
	}
	return out
}

func loadLittleEndianU64(t *testing.T, mem *riscv.GuestMemory, addr uint64) uint64 {
	t.Helper()
	var raw [8]byte
	for i := range raw {
		v, fault := mem.Load8(addr + uint64(i))
		if fault != nil {
			t.Fatalf("Load8 at %#x: %v", addr+uint64(i), fault)
		}
		raw[i] = v
	}
	return binary.LittleEndian.Uint64(raw[:])
}

func fdtU64(v uint64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint32(out[0:4], uint32(v>>32))
	binary.BigEndian.PutUint32(out[4:8], uint32(v))
	return out[:]
}

func isNullFDTFault(err error) bool {
	if err == nil {
		return false
	}
	var fault *riscv.MemFault
	return errors.As(err, &fault) && fault.Kind == riscv.FaultLoad && fault.Addr < 8
}

func envHas(env []string, want string) bool {
	for _, got := range env {
		if got == want {
			return true
		}
	}
	return false
}

func TestRunEmuJea9LinuxFixtureModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		jitlazy bool
		jitaot  bool
	}{
		{name: "interpreter"},
		{name: "lazy-jit", jitlazy: true},
		{name: "aot-jit", jitaot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := runEmu(EmuConfig{
				RunPath:           "../../testvectors/jea9linux/elf/write_stdout.elf",
				MemorySize:        riscv.Size64MB,
				InstructionBudget: 1 << 20,
				JITLazy:           tc.jitlazy,
				JITAOT:            tc.jitaot,
				Stdin:             strings.NewReader(""),
				Stdout:            &stdout,
				Stderr:            &stderr,
			})
			if err != nil {
				t.Fatalf("runEmu: %v; stderr=%q", err, stderr.String())
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got, want := stdout.String(), "jea9linux stdout\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

func TestEmuJITFlagsAreMutuallyExclusive(t *testing.T) {
	cfg := EmuConfig{
		RunPath: "../../testvectors/jea9linux/elf/write_stdout.elf",
		JITLazy: true,
		JITAOT:  true,
	}
	if err := cfg.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted both -jitlazy and -jitaot")
	}
}

func TestEmuDefaultFlagsRunGoTimeNowFixtureCompletes(t *testing.T) {
	cfg, stdout, stderr := parseEmuConfigForTest(t,
		"-run", "../../testvectors/jea9linux/go/elf/timenow.elf",
	)

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := runEmu(cfg)
		done <- result{code: code, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("runEmu: %v; stdout=%q stderr=%q", got.err, stdout.String(), stderr.String())
		}
		if got.code != 0 {
			t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", got.code, stdout.String(), stderr.String())
		}
		if strings.TrimSpace(stdout.String()) == "" {
			t.Fatalf("stdout is empty; stderr=%q", stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("default emu timenow run did not complete; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestEmuConfigDefaultsPreserveExplicitZeroClock(t *testing.T) {
	cfg := EmuConfig{}.withDefaults()
	if cfg.MemorySize != defaultEmuMemorySize {
		t.Fatalf("MemorySize = %d, want %d", cfg.MemorySize, defaultEmuMemorySize)
	}
	if cfg.Budget != defaultEmuBudget {
		t.Fatalf("Budget = %q, want %q", cfg.Budget, defaultEmuBudget)
	}
	budget, err := cfg.schedulerBudget()
	if err != nil {
		t.Fatalf("schedulerBudget default: %v", err)
	}
	if budget != defaultEmuInstructionBudget {
		t.Fatalf("schedulerBudget = %d, want %d", budget, defaultEmuInstructionBudget)
	}

	bios := EmuConfig{BiosPath: "../../testvectors/jea9linux/elf/write_stdout.elf"}.withDefaults()
	if bios.Budget != defaultEmuBiosBudget {
		t.Fatalf("BIOS Budget = %q, want %q", bios.Budget, defaultEmuBiosBudget)
	}
	biosBudget, err := bios.schedulerBudget()
	if err != nil {
		t.Fatalf("BIOS schedulerBudget default: %v", err)
	}
	if biosBudget != ^uint64(0) {
		t.Fatalf("BIOS schedulerBudget = %d, want max", biosBudget)
	}
}

func TestParseEmuJITModeFlags(t *testing.T) {
	lazy, _, _ := parseEmuConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
		"-jitlazy",
	)
	if !lazy.JITLazy || lazy.JITAOT {
		t.Fatalf("-jitlazy parsed as JITLazy=%v JITAOT=%v", lazy.JITLazy, lazy.JITAOT)
	}

	aot, _, _ := parseEmuConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
		"-jitaot",
	)
	if !aot.JITAOT || aot.JITLazy {
		t.Fatalf("-jitaot parsed as JITLazy=%v JITAOT=%v", aot.JITLazy, aot.JITAOT)
	}

	interp, _, _ := parseEmuConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
	)
	if interp.JITLazy || interp.JITAOT {
		t.Fatalf("default parsed as JITLazy=%v JITAOT=%v", interp.JITLazy, interp.JITAOT)
	}
}

func BenchmarkRunEmuGoHelloInterpreter(b *testing.B) {
	benchmarkRunEmuGoHello(b, EmuConfig{})
}

func BenchmarkRunEmuGoHelloLazyJIT(b *testing.B) {
	benchmarkRunEmuGoHello(b, EmuConfig{JITLazy: true})
}

func BenchmarkRunEmuGoHelloAOTJIT(b *testing.B) {
	benchmarkRunEmuGoHello(b, EmuConfig{JITAOT: true})
}

func benchmarkRunEmuGoHello(b *testing.B, mode EmuConfig) {
	b.Helper()
	b.ReportAllocs()
	var totalStats EmuJITStats
	for i := 0; i < b.N; i++ {
		var stdout, stderr bytes.Buffer
		var stats EmuJITStats
		cfg := mode
		cfg.RunPath = "../../testvectors/jea9linux/go/elf/hello.elf"
		cfg.MemorySize = riscv.Size16GB
		cfg.InstructionBudget = 1 << 20
		cfg.Stdin = strings.NewReader("")
		cfg.Stdout = &stdout
		cfg.Stderr = &stderr
		cfg.JITStats = &stats

		code, err := runEmu(cfg)
		if err != nil {
			b.Fatalf("runEmu: %v; stderr=%q", err, stderr.String())
		}
		if code != 0 {
			b.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
		}
		if got, want := stdout.String(), "hello jea9linux go\n"; got != want {
			b.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
		}

		totalStats.DispatchOK += stats.DispatchOK
		totalStats.DispatchCompile += stats.DispatchCompile
		totalStats.DispatchInterp += stats.DispatchInterp
		totalStats.ChainPatchedJalr += stats.ChainPatchedJalr
		totalStats.JalrICMisses += stats.JalrICMisses
		totalStats.JalrICDeopts += stats.JalrICDeopts
		totalStats.AOTSegmentsInstalled += stats.AOTSegmentsInstalled
		totalStats.AOTBlocksInstalled += stats.AOTBlocksInstalled
		totalStats.AOTCompileFailures += stats.AOTCompileFailures
		totalStats.AOTDecoderCacheLookups += stats.AOTDecoderCacheLookups
		totalStats.AOTDecoderCacheHits += stats.AOTDecoderCacheHits
		totalStats.AOTDecoderCacheMisses += stats.AOTDecoderCacheMisses
		totalStats.AOTDecoderCacheOutside += stats.AOTDecoderCacheOutside
	}
	if totalStats.DispatchOK != 0 || totalStats.DispatchCompile != 0 || totalStats.DispatchInterp != 0 {
		b.ReportMetric(float64(totalStats.DispatchOK)/float64(b.N), "dispatch_ok/op")
		b.ReportMetric(float64(totalStats.DispatchCompile)/float64(b.N), "compile/op")
		b.ReportMetric(float64(totalStats.DispatchInterp)/float64(b.N), "interp_fallback/op")
		b.ReportMetric(float64(totalStats.ChainPatchedJalr)/float64(b.N), "jalr_patch/op")
		b.ReportMetric(float64(totalStats.JalrICMisses)/float64(b.N), "jalr_miss/op")
		b.ReportMetric(float64(totalStats.JalrICDeopts)/float64(b.N), "jalr_deopt/op")
	}
	if totalStats.AOTSegmentsInstalled != 0 || totalStats.AOTCompileFailures != 0 {
		b.ReportMetric(float64(totalStats.AOTSegmentsInstalled)/float64(b.N), "aotseg/op")
		b.ReportMetric(float64(totalStats.AOTBlocksInstalled)/float64(b.N), "aotblock/op")
		b.ReportMetric(float64(totalStats.AOTCompileFailures)/float64(b.N), "aotfail/op")
	}
	if totalStats.AOTDecoderCacheLookups != 0 {
		b.ReportMetric(float64(totalStats.AOTDecoderCacheLookups)/float64(b.N), "aotdc_lookup/op")
		b.ReportMetric(float64(totalStats.AOTDecoderCacheHits)/float64(b.N), "aotdc_hit/op")
		b.ReportMetric(float64(totalStats.AOTDecoderCacheMisses)/float64(b.N), "aotdc_miss/op")
	}
	if totalStats.AOTDecoderCacheOutside != 0 {
		b.ReportMetric(float64(totalStats.AOTDecoderCacheOutside)/float64(b.N), "aotdc_outside/op")
	}
}

func TestParseEmuBudgetAndSeedBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want uint64
	}{
		{name: "1", want: 1},
		{name: "1ns", want: 1},
		{name: "1us", want: 1_000},
		{name: "1ms", want: 1_000_000},
		{name: "1s", want: 1_000_000_000},
		{name: "1e6", want: 1_000_000},
		{name: "1_000_000", want: 1_000_000},
		{name: "1000000", want: 1_000_000},
		{name: "max", want: ^uint64(0)},
		{name: "^uint64(0)", want: ^uint64(0)},
	} {
		got, err := parseEmuBudget(tc.name)
		if err != nil {
			t.Fatalf("parseEmuBudget(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("parseEmuBudget(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "1.5", "nope"} {
		if _, err := parseEmuBudget(bad); err == nil {
			t.Fatalf("parseEmuBudget(%q) returned nil error", bad)
		}
	}

	const seed = uint64(0x0102030405060708)
	if got := binary.LittleEndian.Uint64(seedBytes(seed)); got != seed {
		t.Fatalf("seedBytes round trip = %#x, want %#x", got, seed)
	}
}

func parseEmuConfigForTest(t *testing.T, args ...string) (EmuConfig, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	fs := flag.NewFlagSet("emu-test", flag.ContinueOnError)
	var flagErrors bytes.Buffer
	fs.SetOutput(&flagErrors)

	cfg := &EmuConfig{}
	cfg.DefineFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v; output=%q", err, flagErrors.String())
	}
	if err := cfg.ValidateConfig(); err != nil {
		t.Fatalf("validate flags: %v", err)
	}
	cfg.Args = append([]string{cfg.programPath()}, fs.Args()...)
	cfg.Env = []string{}

	var stdout, stderr bytes.Buffer
	cfg.Stdin = strings.NewReader("")
	cfg.Stdout = &stdout
	cfg.Stderr = &stderr
	return *cfg, &stdout, &stderr
}

func runEmuFixtureOutput(t *testing.T, seed uint64) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := runEmu(EmuConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/cryptorand.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Seed:              seed,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmu: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	return stdout.String()
}
