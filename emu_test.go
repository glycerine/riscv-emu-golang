package riscv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunEmuDefaultRunsGoHelloFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := RunEmu(&EmuConfig{
		RunPath:           "testvectors/jea9linux/go/elf/hello.elf",
		MemorySize:        Size16GB,
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

func TestTerminalStatusTextUsesCRLF(t *testing.T) {
	got := terminalStatusText("console%d: %s\nnext", 1, "/tmp/console1.sock")
	want := "console1: /tmp/console1.sock\r\nnext\r\n"
	if got != want {
		t.Fatalf("terminalStatusText = %q, want %q", got, want)
	}
}

func TestRunEmuReturnsGuestExitCodeAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := RunEmu(&EmuConfig{
		RunPath:           "testvectors/jea9linux/go/elf/nilpanic.elf",
		MemorySize:        Size16GB,
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

	nonHermit := &EmuConfig{}
	nonHermit.setDefaults()
	if !envHas(nonHermit.Env, "JEA9_EMU_ENV_TEST_MARKER=inherited") {
		t.Fatalf("non-hermit Env did not inherit marker: %q", nonHermit.Env)
	}

	hermit := &EmuConfig{Hermit: true}
	hermit.setDefaults()

	if len(hermit.Env) != 0 {
		t.Fatalf("hermit Env len = %d, want 0; Env=%q", len(hermit.Env), hermit.Env)
	}

	explicitEmpty := &EmuConfig{Env: []string{}}
	explicitEmpty.setDefaults()
	if len(explicitEmpty.Env) != 0 {
		t.Fatalf("explicit empty Env len = %d, want 0; Env=%q", len(explicitEmpty.Env), explicitEmpty.Env)
	}
}

func TestEmuTimeModeFollowsHermitFlag(t *testing.T) {
	if got := (&EmuConfig{}).timeMode(); got != RealTime {
		t.Fatalf("default emu time mode = %v, want RealTime", got)
	}
	if got := (&EmuConfig{Hermit: true}).timeMode(); got != HermitTime {
		t.Fatalf("hermit emu time mode = %v, want HermitTime", got)
	}
}

func TestEmuBiosFlagIsSeparateFromRun(t *testing.T) {
	const path = "testvectors/jea9linux/elf/write_stdout.elf"
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
		RunPath:  "testvectors/jea9linux/elf/write_stdout.elf",
		BiosPath: "xendor/opensbi/build/platform/generic/firmware/fw_jump.elf",
	}
	if err := both.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted both -run and -bios")
	}
}

func TestParseEmuIdleFlag(t *testing.T) {
	cfg, _, _ := parseEmuConfigForTest(t,
		"-run", "testvectors/jea9linux/elf/write_stdout.elf",
		"-idle", "25ms",
	)
	d, ok, err := cfg.idleSleepCap()
	if err != nil {
		t.Fatalf("idleSleepCap: %v", err)
	}
	if !ok {
		t.Fatal("-idle did not register an override")
	}
	if d != 25*time.Millisecond {
		t.Fatalf("-idle duration = %s, want 25ms", d)
	}
}

func TestParseEmuIdleFlagAcceptsZeroToDisableSleep(t *testing.T) {
	for _, raw := range []string{"0ms", "0"} {
		t.Run(raw, func(t *testing.T) {
			cfg, _, _ := parseEmuConfigForTest(t,
				"-run", "testvectors/jea9linux/elf/write_stdout.elf",
				"-idle", raw,
			)
			d, ok, err := cfg.idleSleepCap()
			if err != nil {
				t.Fatalf("idleSleepCap: %v", err)
			}
			if !ok {
				t.Fatalf("-idle %s did not register an override", raw)
			}
			if d != 0 {
				t.Fatalf("-idle duration = %s, want 0", d)
			}
		})
	}
}

func TestEmuIdleFlagRejectsNegativeDuration(t *testing.T) {
	cfg := EmuConfig{
		RunPath: "testvectors/jea9linux/elf/write_stdout.elf",
		Idle:    "-1ms",
	}
	if err := cfg.ValidateConfig(); err == nil || !strings.Contains(err.Error(), "-idle") {
		t.Fatalf("ValidateConfig err = %v, want -idle error", err)
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
		"-bios", "testvectors/jea9linux/elf/write_stdout.elf",
		"-kernel", kernelPath,
		"-kernel-addr", "0x80400000",
		"-initrd", initrdPath,
		"-append", "console=hvc0 root=/dev/ram0",
		"-dump-dtb", dumpDTBPath,
		"-machine", "virt",
		"-mem", "512mb",
		"-hostio",
		"-net",
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
	if cfg.BiosRAMSize != Size512MB {
		t.Fatalf("BiosRAMSize = %d, want %d", cfg.BiosRAMSize, Size512MB)
	}
	if cfg.MemorySize != Size4GB {
		t.Fatalf("MemorySize slab = %d, want %d", cfg.MemorySize, Size4GB)
	}
	if !cfg.HostIO {
		t.Fatal("HostIO = false, want true")
	}
	if !cfg.Net {
		t.Fatal("Net = false, want true")
	}
	budget, err := cfg.schedulerBudget()
	if err != nil {
		t.Fatalf("schedulerBudget: %v", err)
	}
	if budget != ^uint64(0) {
		t.Fatalf("BIOS default schedulerBudget = %d, want max", budget)
	}

	runWithKernel := EmuConfig{
		RunPath:    "testvectors/jea9linux/elf/write_stdout.elf",
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
		{"512mb", Size512MB},
		{"512MB", Size512MB},
		{"512 MiB", Size512MB},
		{"2GB", Size2GB},
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

	cfg := &EmuConfig{
		BiosPath:    "testvectors/jea9linux/elf/write_stdout.elf",
		KernelPath:  kernelPath,
		KernelAddr:  0x80400000,
		InitrdPath:  initrdPath,
		Append:      "console=hvc0 root=/dev/ram0",
		DumpDTBPath: dumpDTBPath,
		Memory:      "512MB",
		Stdin:       strings.NewReader(""),
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
	}
	cfg.setDefaults()
	guest, err := prepareBiosGuest(cfg)
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()
	if got := guest.mem.Size(); got != Size4GB {
		t.Fatalf("guest memory slab = %d, want %d", got, Size4GB)
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
	if !bytes.Contains(guest.fdt, fdtU64(Size512MB)) {
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
	fdt, err := buildVirtFDT(Size16GB, virtFDTOptions{BootArgs: "from-external-dtb"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dtbPath, fdt, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &EmuConfig{
		BiosPath:    "testvectors/jea9linux/elf/write_stdout.elf",
		DTBPath:     dtbPath,
		DumpDTBPath: dumpDTBPath,
		MemorySize:  Size16GB,
		Stdin:       strings.NewReader(""),
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
	}
	cfg.setDefaults()
	guest, err := prepareBiosGuest(cfg)
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
	m.uarts[0].rx = append(m.uarts[0].rx, "ls\n"...)

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
	for len(m.uarts[0].rx) < 2 && time.Now().Before(deadline) {
		m.drainUARTInput(0)
	}
	if len(m.uarts[0].rx) < 2 {
		t.Fatalf("UART RX len = %d, want stdin bytes", len(m.uarts[0].rx))
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

func TestBiosUART1ConsoleSocketRoundTrip(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	m := newBiosMMIOWithConsoleSockets(nil, io.Discard, nil, true)
	defer m.closeUARTOutput()

	path := emuConsoleSocketPath(os.Getpid(), 1)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial console socket: %v", err)
	}
	defer conn.Close()
	console := m.uarts[1].out.(*emuConsoleSocket)
	deadline := time.Now().Add(time.Second)
	for console.activeConn() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if console.activeConn() == nil {
		t.Fatal("console socket did not accept connection")
	}

	m.storeUARTPort(1, 0, 1, 'Z')
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read console socket output: %v", err)
	}
	if buf[0] != 'Z' {
		t.Fatalf("console socket output = %q, want Z", buf[0])
	}

	if _, err := conn.Write([]byte("q")); err != nil {
		t.Fatalf("write console socket input: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for len(m.uarts[1].rx) < 1 && time.Now().Before(deadline) {
		m.drainUARTInput(1)
	}
	if got := m.loadUARTPort(1, 0, 1); byte(got) != 'q' {
		t.Fatalf("UART1 RBR = %q, want q", byte(got))
	}
}

func TestBiosUART1DTRDropClosesActiveConsoleSocket(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	m := newBiosMMIOWithConsoleSockets(nil, io.Discard, nil, true)
	defer m.closeUARTOutput()

	path := emuConsoleSocketPath(os.Getpid(), 1)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial console socket: %v", err)
	}
	defer conn.Close()
	console := m.uarts[1].out.(*emuConsoleSocket)
	deadline := time.Now().Add(time.Second)
	for console.activeConn() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if console.activeConn() == nil {
		t.Fatal("console socket did not accept connection")
	}

	m.storeUARTPort(1, 4, 1, uint64(uartMCRDTR))
	m.storeUARTPort(1, 4, 1, 0)
	if console.activeConn() != nil {
		t.Fatal("console active connection still present after UART1 DTR drop")
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var buf [1]byte
	n, err := conn.Read(buf[:])
	if err == nil {
		t.Fatalf("console socket read after DTR drop returned n=%d nil error, want close", n)
	}
}

func TestBiosUART1DTRDropAllowsThreeSequentialConsoleSessions(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	m := newBiosMMIOWithConsoleSockets(nil, io.Discard, nil, true)
	defer m.closeUARTOutput()

	path := emuConsoleSocketPath(os.Getpid(), 1)
	console, ok := m.uarts[1].out.(*emuConsoleSocket)
	if !ok {
		t.Fatalf("UART1 output = %T, want console socket", m.uarts[1].out)
	}
	for session := 0; session < 3; session++ {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("session %d dial console socket: %v", session+1, err)
		}

		deadline := time.Now().Add(time.Second)
		for console.activeConn() == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if console.activeConn() == nil {
			_ = conn.Close()
			t.Fatalf("session %d console socket did not accept connection", session+1)
		}

		m.storeUARTPort(1, 4, 1, uint64(uartMCRDTR))
		out := byte('A' + session)
		m.storeUARTPort(1, 0, 1, uint64(out))
		buf := make([]byte, 1)
		if _, err := io.ReadFull(conn, buf); err != nil {
			_ = conn.Close()
			t.Fatalf("session %d read console socket output: %v", session+1, err)
		}
		if buf[0] != out {
			_ = conn.Close()
			t.Fatalf("session %d console socket output = %q, want %q", session+1, buf[0], out)
		}

		in := byte('0' + session)
		if _, err := conn.Write([]byte{in}); err != nil {
			_ = conn.Close()
			t.Fatalf("session %d write console socket input: %v", session+1, err)
		}
		deadline = time.Now().Add(time.Second)
		for len(m.uarts[1].rx) < 1 && time.Now().Before(deadline) {
			m.drainUARTInput(1)
		}
		if got := m.loadUARTPort(1, 0, 1); byte(got) != in {
			_ = conn.Close()
			t.Fatalf("session %d UART1 RBR = %q, want %q", session+1, byte(got), in)
		}

		m.storeUARTPort(1, 4, 1, 0)
		if console.activeConn() != nil {
			_ = conn.Close()
			t.Fatalf("session %d console active connection still present after UART1 DTR drop", session+1)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			_ = conn.Close()
			t.Fatalf("session %d set read deadline: %v", session+1, err)
		}
		var closed [1]byte
		n, err := conn.Read(closed[:])
		_ = conn.Close()
		if err == nil {
			t.Fatalf("session %d console socket read after DTR drop returned n=%d nil error, want close", session+1, n)
		}
	}
}

func TestInitramfsRespawnsTTY1DebugShell(t *testing.T) {
	initScript, err := os.ReadFile("xendor/linux/initramfs/init")
	if err != nil {
		t.Fatalf("read initramfs init: %v", err)
	}
	text := string(initScript)
	for _, want := range []string{
		"if [ -e /dev/ttyS1 ]; then",
		"while [ -e /dev/ttyS1 ]; do",
		"exec /bin/sh -i </dev/ttyS1 >/dev/ttyS1 2>&1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("initramfs init missing %q", want)
		}
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

func TestBiosHostIOFDTAdvertisedWhenEnabled(t *testing.T) {
	without, err := buildVirtFDT(Size4GB, virtFDTOptions{})
	if err != nil {
		t.Fatalf("build FDT without hostio: %v", err)
	}
	if bytes.Contains(without, []byte("glycerine,riscv-hostio-v1")) {
		t.Fatal("hostio compatible appeared without HostIO")
	}

	with, err := buildVirtFDT(Size4GB, virtFDTOptions{HostIO: true})
	if err != nil {
		t.Fatalf("build FDT with hostio: %v", err)
	}
	if !bytes.Contains(with, []byte("hostio@10001000")) {
		t.Fatal("hostio node missing from FDT")
	}
	if !bytes.Contains(with, []byte("glycerine,riscv-hostio-v1")) {
		t.Fatal("hostio compatible missing from FDT")
	}
}

func TestBiosHostIOOpenWriteSeekReadClose(t *testing.T) {
	mem, mmio := newHostIOTestDevice(t)
	path := filepath.Join(t.TempDir(), "host.txt")

	handle := runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpOpen,
		Flags:   hostIOOpenReadWrite | hostIOOpenCreate | hostIOOpenTrunc,
		Path:    writeHostIOTestBytes(t, mem, 0x2000, []byte(path)),
		PathLen: uint64(len(path)),
		Mode:    0644,
	}).Result
	if handle <= 0 {
		t.Fatalf("open handle = %d, want positive", handle)
	}

	payload := []byte("hello hostio")
	got := runHostIOTestCmd(t, mem, hostIOCommand{
		Op:     hostIOOpWrite,
		Handle: uint64(handle),
		Buf:    writeHostIOTestBytes(t, mem, 0x3000, payload),
		Len:    uint64(len(payload)),
	})
	if got.Result != int64(len(payload)) {
		t.Fatalf("write result = %d, want %d", got.Result, len(payload))
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{
		Op:     hostIOOpSeek,
		Flags:  uint32(io.SeekStart),
		Handle: uint64(handle),
	})
	if got.Result != 0 {
		t.Fatalf("seek result = %d, want 0", got.Result)
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{
		Op:     hostIOOpRead,
		Handle: uint64(handle),
		Buf:    0x4000,
		Len:    uint64(len(payload)),
	})
	if got.Result != int64(len(payload)) {
		t.Fatalf("read result = %d, want %d", got.Result, len(payload))
	}
	readBack := make([]byte, len(payload))
	if fault := mem.ReadBytes(0x4000, readBack); fault != nil {
		t.Fatalf("read guest buffer: %v", fault)
	}
	if !bytes.Equal(readBack, payload) {
		t.Fatalf("read payload = %q, want %q", readBack, payload)
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{Op: hostIOOpClose, Handle: uint64(handle)})
	if got.Result != 0 {
		t.Fatalf("close result = %d, want 0", got.Result)
	}
	if mmio.hostio.files[uint64(handle)] != nil {
		t.Fatal("closed handle still present")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("host file = %q, want %q", onDisk, payload)
	}
}

func TestBiosHostIOPathCommands(t *testing.T) {
	mem, _ := newHostIOTestDevice(t)
	dir := filepath.Join(t.TempDir(), "a", "b")
	file := filepath.Join(dir, "payload.txt")
	payload := []byte("from guest ram")

	got := runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpMkdirAll,
		Path:    writeHostIOTestBytes(t, mem, 0x2000, []byte(dir)),
		PathLen: uint64(len(dir)),
		Mode:    0755,
	})
	if got.Result != 0 {
		t.Fatalf("mkdirall result = %d errno=%d, want success", got.Result, got.Errno)
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpWriteFile,
		Path:    writeHostIOTestBytes(t, mem, 0x3000, []byte(file)),
		PathLen: uint64(len(file)),
		Buf:     writeHostIOTestBytes(t, mem, 0x4000, payload),
		Len:     uint64(len(payload)),
		Mode:    0644,
	})
	if got.Result != int64(len(payload)) {
		t.Fatalf("writefile result = %d errno=%d, want %d", got.Result, got.Errno, len(payload))
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpReadFile,
		Path:    writeHostIOTestBytes(t, mem, 0x5000, []byte(file)),
		PathLen: uint64(len(file)),
		Buf:     0x6000,
		Len:     uint64(len(payload)),
	})
	if got.Result != int64(len(payload)) {
		t.Fatalf("readfile result = %d errno=%d, want %d", got.Result, got.Errno, len(payload))
	}
	readBack := make([]byte, len(payload))
	if fault := mem.ReadBytes(0x6000, readBack); fault != nil {
		t.Fatalf("read guest buffer: %v", fault)
	}
	if !bytes.Equal(readBack, payload) {
		t.Fatalf("readfile payload = %q, want %q", readBack, payload)
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpStat,
		Path:    writeHostIOTestBytes(t, mem, 0x7000, []byte(file)),
		PathLen: uint64(len(file)),
		Buf:     0x8000,
		Len:     hostIOStatSize,
	})
	if got.Result != hostIOStatSize {
		t.Fatalf("stat result = %d errno=%d, want stat size", got.Result, got.Errno)
	}
	var stat [hostIOStatSize]byte
	if fault := mem.ReadBytes(0x8000, stat[:]); fault != nil {
		t.Fatalf("read stat: %v", fault)
	}
	if size := binary.LittleEndian.Uint64(stat[0:]); size != uint64(len(payload)) {
		t.Fatalf("stat size = %d, want %d", size, len(payload))
	}

	got = runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpReadDir,
		Path:    writeHostIOTestBytes(t, mem, 0x9000, []byte(dir)),
		PathLen: uint64(len(dir)),
		Buf:     0xa000,
		Len:     4096,
	})
	if got.Result <= hostIODirentHeaderSize {
		t.Fatalf("readdir result = %d errno=%d, want entry bytes", got.Result, got.Errno)
	}
	var direntHeader [hostIODirentHeaderSize]byte
	if fault := mem.ReadBytes(0xa000, direntHeader[:]); fault != nil {
		t.Fatalf("read dirent header: %v", fault)
	}
	nameLen := binary.LittleEndian.Uint32(direntHeader[24:])
	name := make([]byte, nameLen)
	if fault := mem.ReadBytes(0xa000+hostIODirentHeaderSize, name); fault != nil {
		t.Fatalf("read dirent name: %v", fault)
	}
	if string(name) != "payload.txt" {
		t.Fatalf("readdir first name = %q, want payload.txt", name)
	}
}

func TestBiosHostIOReadlinkNullTerminatesWhenSpaceAllows(t *testing.T) {
	mem, _ := newHostIOTestDevice(t)
	dir := t.TempDir()
	target := "go/src/github.com/glycerine/riscv-emu-golang"
	link := filepath.Join(dir, "ris")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	const bufAddr = uint64(0x4000)
	fill := bytes.Repeat([]byte{0xaa}, 128)
	if fault := mem.WriteBytes(bufAddr, fill); fault != nil {
		t.Fatalf("fill guest buffer: %v", fault)
	}

	got := runHostIOTestCmd(t, mem, hostIOCommand{
		Op:      hostIOOpReadlink,
		Path:    writeHostIOTestBytes(t, mem, 0x2000, []byte(link)),
		PathLen: uint64(len(link)),
		Buf:     bufAddr,
		Len:     uint64(len(fill)),
	})
	if got.Result != int64(len(target)) {
		t.Fatalf("readlink result = %d errno=%d, want %d", got.Result, got.Errno, len(target))
	}

	readBack := make([]byte, len(target)+2)
	if fault := mem.ReadBytes(bufAddr, readBack); fault != nil {
		t.Fatalf("read guest buffer: %v", fault)
	}
	if string(readBack[:len(target)]) != target {
		t.Fatalf("readlink target = %q, want %q", readBack[:len(target)], target)
	}
	if readBack[len(target)] != 0 {
		t.Fatalf("byte after readlink target = %#x, want NUL", readBack[len(target)])
	}
	if readBack[len(target)+1] != 0xaa {
		t.Fatalf("byte after terminator = %#x, want untouched marker", readBack[len(target)+1])
	}
}

func TestBiosHostIOReadlinkRequiresSpaceForTerminator(t *testing.T) {
	mem, _ := newHostIOTestDevice(t)
	dir := t.TempDir()
	target := "go/src/github.com/glycerine/riscv-emu-golang"
	link := filepath.Join(dir, "ris")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	const bufAddr = uint64(0x4000)
	fill := bytes.Repeat([]byte{0xaa}, len(target))
	if fault := mem.WriteBytes(bufAddr, fill); fault != nil {
		t.Fatalf("fill guest buffer: %v", fault)
	}

	got := runHostIOTestCmdAnyStatus(t, mem, hostIOCommand{
		Op:      hostIOOpReadlink,
		Path:    writeHostIOTestBytes(t, mem, 0x2000, []byte(link)),
		PathLen: uint64(len(link)),
		Buf:     bufAddr,
		Len:     uint64(len(target)),
	})
	if got.Status != hostIOStatusErr {
		t.Fatalf("readlink status = %d errno=%d result=%d, want error", got.Status, got.Errno, got.Result)
	}
	if got.Errno != hostIOErrENOBUFS {
		t.Fatalf("readlink errno = %d, want ENOBUFS", got.Errno)
	}
	if got.Result != int64(len(target)+1) {
		t.Fatalf("readlink required size = %d, want %d", got.Result, len(target)+1)
	}

	readBack := make([]byte, len(fill))
	if fault := mem.ReadBytes(bufAddr, readBack); fault != nil {
		t.Fatalf("read guest buffer: %v", fault)
	}
	if !bytes.Equal(readBack, fill) {
		t.Fatalf("failed readlink modified buffer = %x, want %x", readBack, fill)
	}
}

func TestBiosVirtioNetFDTAdvertisedWhenEnabled(t *testing.T) {
	without, err := buildVirtFDT(Size4GB, virtFDTOptions{})
	if err != nil {
		t.Fatalf("build FDT without net: %v", err)
	}
	if bytes.Contains(without, []byte("virtio_net@10008000")) {
		t.Fatal("virtio-net node appeared without Net")
	}

	with, err := buildVirtFDT(Size4GB, virtFDTOptions{Net: true})
	if err != nil {
		t.Fatalf("build FDT with net: %v", err)
	}
	if !bytes.Contains(with, []byte("virtio_net@10008000")) {
		t.Fatal("virtio-net node missing from FDT")
	}
	if !bytes.Contains(with, []byte("virtio,mmio")) {
		t.Fatal("virtio-mmio compatible missing from FDT")
	}
	if !bytes.Contains(with, fdtU64(biosVirtioNetBase)) {
		t.Fatal("virtio-net base missing from FDT")
	}
}

func TestBiosSecondUARTFDTAdvertised(t *testing.T) {
	fdt, err := buildVirtFDT(Size4GB, virtFDTOptions{})
	if err != nil {
		t.Fatalf("build FDT: %v", err)
	}
	for _, want := range [][]byte{
		[]byte("serial1"),
		[]byte("uart@10000100"),
		fdtU64(biosUART1Base),
	} {
		if !bytes.Contains(fdt, want) {
			t.Fatalf("FDT missing %q", want)
		}
	}
}

func TestBiosVirtioNetMMIOProbeAndConfig(t *testing.T) {
	mem, _, _ := newVirtioNetTestDevice(t)

	if got := mustLoad32Emu(t, mem, biosVirtioNetBase+virtioMMIOMagicValue); got != virtioMMIOMagic {
		t.Fatalf("virtio magic = %#x, want %#x", got, virtioMMIOMagic)
	}
	if got := mustLoad32Emu(t, mem, biosVirtioNetBase+virtioMMIOVersion); got != 2 {
		t.Fatalf("virtio version = %d, want 2", got)
	}
	if got := mustLoad32Emu(t, mem, biosVirtioNetBase+virtioMMIODeviceID); got != virtioDeviceIDNet {
		t.Fatalf("virtio device id = %d, want net", got)
	}
	if got := mustLoad32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueNumMax); got != uint32(virtioQueueSize) {
		t.Fatalf("default queue max = %d, want %d", got, virtioQueueSize)
	}
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIODeviceFeaturesSel, 1)
	if got := mustLoad32Emu(t, mem, biosVirtioNetBase+virtioMMIODeviceFeatures); got&1 == 0 {
		t.Fatalf("high feature word %#x does not advertise VIRTIO_F_VERSION_1", got)
	}
	if status := mustLoad16Emu(t, mem, biosVirtioNetBase+virtioMMIOConfig+6); status != virtioNetStatusLinkUp {
		t.Fatalf("virtio-net status = %#x, want link up", status)
	}
	if mtu := mustLoad16Emu(t, mem, biosVirtioNetBase+virtioMMIOConfig+10); mtu != virtioNetMTU {
		t.Fatalf("virtio-net MTU = %d, want %d", mtu, virtioNetMTU)
	}
}

func TestVirtioNetGeneratedMACEmbedsPIDAndIsLocalUnicast(t *testing.T) {
	mac, err := generateVirtioNetMAC(bytes.NewReader([]byte{0xff, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}), 0x01020304)
	if err != nil {
		t.Fatal(err)
	}
	if mac[0]&0x02 == 0 {
		t.Fatalf("local bit not set in MAC %x", mac)
	}
	if mac[0]&0x01 != 0 {
		t.Fatalf("multicast bit set in MAC %x", mac)
	}
	if got, want := binary.BigEndian.Uint32(mac[1:5]), uint32(0x01020304); got != want {
		t.Fatalf("PID bytes = %#x, want %#x", got, want)
	}
	if mac[5] != 0xee {
		t.Fatalf("tail random byte = %#x, want 0xee", mac[5])
	}
}

func TestVirtioNetGeneratedMACVariesByPIDAndRandomTail(t *testing.T) {
	mac1, err := generateVirtioNetMAC(bytes.NewReader([]byte{0, 0, 0, 0, 0, 0x11}), 100)
	if err != nil {
		t.Fatal(err)
	}
	mac2, err := generateVirtioNetMAC(bytes.NewReader([]byte{0, 0, 0, 0, 0, 0x11}), 101)
	if err != nil {
		t.Fatal(err)
	}
	if mac1 == mac2 {
		t.Fatalf("different PIDs produced same MAC %x", mac1)
	}
	mac3, err := generateVirtioNetMAC(bytes.NewReader([]byte{0, 0, 0, 0, 0, 0x12}), 100)
	if err != nil {
		t.Fatal(err)
	}
	if mac1 == mac3 {
		t.Fatalf("different random tail produced same MAC %x", mac1)
	}
}

func TestVirtioNetGeneratedMACRetriesRouterCollision(t *testing.T) {
	input := []byte{
		0x02, 0, 0, 0, 0, 0x01,
		0x00, 0, 0, 0, 0, 0x02,
	}
	mac, err := generateVirtioNetMAC(bytes.NewReader(input), 0x726973ff)
	if err != nil {
		t.Fatal(err)
	}
	if mac == emunetRouterMAC {
		t.Fatalf("MAC generator returned router MAC %x", mac)
	}
	if got, want := mac[5], byte(0x02); got != want {
		t.Fatalf("MAC generator did not retry to second random tail: got %#x want %#x", got, want)
	}
}

func TestBiosVirtioNetTXQueueNotifyInjectsFrame(t *testing.T) {
	mem, stack, _ := newVirtioNetTestDevice(t)
	const (
		desc  = uint64(0x20000)
		avail = uint64(0x21000)
		used  = uint64(0x22000)
		buf   = uint64(0x30000)
	)
	setupVirtioQueueTest(t, mem, virtioNetQueueTX, 8, desc, avail, used)

	frame := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0x02, 0x72, 0x69, 0x73, 0x00, 0x02,
		0x08, 0x00,
		0x45, 0x00, 0x00, 0x14,
	}
	packet := append(make([]byte, virtioNetHeaderSize), frame...)
	if fault := mem.WriteBytes(buf, packet); fault != nil {
		t.Fatalf("write TX packet: %v", fault)
	}
	writeVirtqDescTest(t, mem, desc, 0, buf, uint32(len(packet)), 0, 0)
	addVirtqAvailTest(t, mem, avail, 8, 0, 0)

	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueNotify, uint32(virtioNetQueueTX))

	frames := stack.Frames()
	if len(frames) != 1 {
		t.Fatalf("injected frames len = %d, want 1", len(frames))
	}
	if !bytes.Equal(frames[0], frame) {
		t.Fatalf("injected frame = %x, want %x", frames[0], frame)
	}
	if got := mustLoad16Emu(t, mem, used+2); got != 1 {
		t.Fatalf("used idx = %d, want 1", got)
	}
	if got := mustLoad32Emu(t, mem, used+4); got != 0 {
		t.Fatalf("used id = %d, want 0", got)
	}
	if got := mustLoad32Emu(t, mem, biosVirtioNetBase+virtioMMIOInterruptStatus); got&virtioMMIOIntVring == 0 {
		t.Fatalf("interrupt status = %#x, want vring bit", got)
	}
}

func TestBiosVirtioNetRXInjectsGuestFrameAndInterrupts(t *testing.T) {
	mem, _, mmio := newVirtioNetTestDevice(t)
	const (
		desc  = uint64(0x20000)
		avail = uint64(0x21000)
		used  = uint64(0x22000)
		buf   = uint64(0x30000)
	)
	setupVirtioQueueTest(t, mem, virtioNetQueueRX, 8, desc, avail, used)
	writeVirtqDescTest(t, mem, desc, 0, buf, 2048, virtqDescFWrite, 0)
	addVirtqAvailTest(t, mem, avail, 8, 0, 0)

	mmio.storePLIC(4*uint64(biosVirtioNetIRQ), 4, 1)
	mmio.storePLIC(0x2000+0x80*uint64(plicSContext), 4, uint64(1)<<biosVirtioNetIRQ)
	mmio.storePLIC(0x200000+0x1000*uint64(plicSContext), 4, 0)

	frame := []byte{
		0x02, 0x72, 0x69, 0x73, 0x00, 0x02,
		0x02, 0x72, 0x69, 0x73, 0x00, 0x01,
		0x08, 0x00,
		0x45, 0x00, 0x00, 0x14,
	}
	if ok := mmio.virtioNet.InjectGuestFrame(frame); !ok {
		t.Fatal("InjectGuestFrame returned false")
	}

	got := make([]byte, virtioNetHeaderSize+len(frame))
	if fault := mem.ReadBytes(buf, got); fault != nil {
		t.Fatalf("read RX packet: %v", fault)
	}
	if !bytes.Equal(got[:virtioNetHeaderSize], make([]byte, virtioNetHeaderSize)) {
		t.Fatalf("virtio-net RX header = %x, want zero", got[:virtioNetHeaderSize])
	}
	if !bytes.Equal(got[virtioNetHeaderSize:], frame) {
		t.Fatalf("RX frame = %x, want %x", got[virtioNetHeaderSize:], frame)
	}
	if got := mustLoad16Emu(t, mem, used+2); got != 1 {
		t.Fatalf("used idx = %d, want 1", got)
	}
	if got := mustLoad32Emu(t, mem, used+4); got != 0 {
		t.Fatalf("used id = %d, want 0", got)
	}
	if got := mustLoad32Emu(t, mem, used+8); got != uint32(virtioNetHeaderSize+len(frame)) {
		t.Fatalf("used len = %d, want %d", got, virtioNetHeaderSize+len(frame))
	}
	if !mmio.SupervisorExternalInterruptPending() {
		t.Fatal("PLIC did not report virtio-net interrupt")
	}
	if got := mmio.loadPLIC(0x200000+0x1000*uint64(plicSContext)+4, 4); got != uint64(biosVirtioNetIRQ) {
		t.Fatalf("PLIC claim = %d, want virtio-net IRQ %d", got, biosVirtioNetIRQ)
	}
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOInterruptACK, virtioMMIOIntVring)
	mmio.storePLIC(0x200000+0x1000*uint64(plicSContext)+4, 4, uint64(biosVirtioNetIRQ))
	if mmio.SupervisorExternalInterruptPending() {
		t.Fatal("virtio-net interrupt still pending after ack and PLIC complete")
	}
}

func newVirtioNetTestDevice(t *testing.T) (*GuestMemory, *virtioNetMemoryStack, *biosMMIO) {
	t.Helper()
	mem, err := NewGuestMemory(Size512MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	stack := newVirtioNetMemoryStack()
	m := newBiosMMIO(nil, io.Discard, nil)
	m.enableVirtioNet(mem, stack)
	mem.SetMMIO(m)
	t.Cleanup(func() {
		m.closeVirtioNet()
		mem.Free()
	})
	return mem, stack, m
}

func setupVirtioQueueTest(t *testing.T, mem *GuestMemory, queue uint16, num uint16, desc, avail, used uint64) {
	t.Helper()
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueSel, uint32(queue))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueNum, uint32(num))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueDescLow, uint32(desc))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueDescHigh, uint32(desc>>32))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueAvailLow, uint32(avail))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueAvailHigh, uint32(avail>>32))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueUsedLow, uint32(used))
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueUsedHigh, uint32(used>>32))
	mustStore16Emu(t, mem, avail, 0)
	mustStore16Emu(t, mem, avail+2, 0)
	mustStore16Emu(t, mem, used, 0)
	mustStore16Emu(t, mem, used+2, 0)
	mustStore32Emu(t, mem, biosVirtioNetBase+virtioMMIOQueueReady, 1)
}

func writeVirtqDescTest(t *testing.T, mem *GuestMemory, table uint64, id uint16, addr uint64, length uint32, flags uint16, next uint16) {
	t.Helper()
	var raw [16]byte
	binary.LittleEndian.PutUint64(raw[0:8], addr)
	binary.LittleEndian.PutUint32(raw[8:12], length)
	binary.LittleEndian.PutUint16(raw[12:14], flags)
	binary.LittleEndian.PutUint16(raw[14:16], next)
	if fault := mem.WriteBytes(table+uint64(id)*16, raw[:]); fault != nil {
		t.Fatalf("write virtq desc %d: %v", id, fault)
	}
}

func addVirtqAvailTest(t *testing.T, mem *GuestMemory, avail uint64, num uint16, idx uint16, head uint16) {
	t.Helper()
	mustStore16Emu(t, mem, avail+4+2*uint64(idx%num), head)
	mustStore16Emu(t, mem, avail+2, idx+1)
}

func mustLoad16Emu(t *testing.T, mem *GuestMemory, addr uint64) uint16 {
	t.Helper()
	got, fault := mem.Load16(addr)
	if fault != nil {
		t.Fatalf("Load16(%#x): %v", addr, fault)
	}
	return got
}

func mustLoad32Emu(t *testing.T, mem *GuestMemory, addr uint64) uint32 {
	t.Helper()
	got, fault := mem.Load32(addr)
	if fault != nil {
		t.Fatalf("Load32(%#x): %v", addr, fault)
	}
	return got
}

func mustStore16Emu(t *testing.T, mem *GuestMemory, addr uint64, value uint16) {
	t.Helper()
	if fault := mem.Store16(addr, value); fault != nil {
		t.Fatalf("Store16(%#x): %v", addr, fault)
	}
}

func mustStore32Emu(t *testing.T, mem *GuestMemory, addr uint64, value uint32) {
	t.Helper()
	if fault := mem.Store32(addr, value); fault != nil {
		t.Fatalf("Store32(%#x): %v", addr, fault)
	}
}

func newHostIOTestDevice(t *testing.T) (*GuestMemory, *biosMMIO) {
	t.Helper()
	mem, err := NewGuestMemory(Size512MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	m := newBiosMMIO(nil, io.Discard, nil)
	m.enableHostIO(mem)
	mem.SetMMIO(m)
	t.Cleanup(func() {
		m.closeHostIO()
		mem.Free()
	})
	return mem, m
}

func runHostIOTestCmd(t *testing.T, mem *GuestMemory, cmd hostIOCommand) hostIOCommand {
	t.Helper()
	got := runHostIOTestCmdAnyStatus(t, mem, cmd)
	if got.Status != hostIOStatusOK {
		t.Fatalf("hostio op %d status=%d errno=%d result=%d", got.Op, got.Status, got.Errno, got.Result)
	}
	status, fault := mem.Load32(biosHostIOBase + hostIORegStatus)
	if fault != nil {
		t.Fatalf("load hostio status: %v", fault)
	}
	if status != hostIOStatusOK {
		t.Fatalf("hostio status register = %d, want OK", status)
	}
	return got
}

func runHostIOTestCmdAnyStatus(t *testing.T, mem *GuestMemory, cmd hostIOCommand) hostIOCommand {
	t.Helper()
	const cmdAddr = uint64(0x1000)
	writeHostIOTestCmd(t, mem, cmdAddr, cmd)
	if fault := mem.Store64(biosHostIOBase+hostIORegCmdAddr, cmdAddr); fault != nil {
		t.Fatalf("store hostio cmd addr: %v", fault)
	}
	if fault := mem.Store64(biosHostIOBase+hostIORegCmdSize, hostIOCmdSize); fault != nil {
		t.Fatalf("store hostio cmd size: %v", fault)
	}
	if fault := mem.Store32(biosHostIOBase+hostIORegSubmit, 1); fault != nil {
		t.Fatalf("submit hostio cmd: %v", fault)
	}
	return readHostIOTestCmd(t, mem, cmdAddr)
}

func writeHostIOTestBytes(t *testing.T, mem *GuestMemory, addr uint64, data []byte) uint64 {
	t.Helper()
	if fault := mem.WriteBytes(addr, data); fault != nil {
		t.Fatalf("write guest bytes at %#x: %v", addr, fault)
	}
	return addr
}

func writeHostIOTestCmd(t *testing.T, mem *GuestMemory, addr uint64, cmd hostIOCommand) {
	t.Helper()
	var raw [hostIOCmdSize]byte
	binary.LittleEndian.PutUint32(raw[0:], cmd.Op)
	binary.LittleEndian.PutUint32(raw[4:], cmd.Flags)
	binary.LittleEndian.PutUint64(raw[8:], cmd.Path)
	binary.LittleEndian.PutUint64(raw[16:], cmd.PathLen)
	binary.LittleEndian.PutUint64(raw[24:], cmd.Path2)
	binary.LittleEndian.PutUint64(raw[32:], cmd.Path2Len)
	binary.LittleEndian.PutUint64(raw[40:], cmd.Buf)
	binary.LittleEndian.PutUint64(raw[48:], cmd.Len)
	binary.LittleEndian.PutUint64(raw[56:], cmd.Offset)
	binary.LittleEndian.PutUint64(raw[64:], cmd.Mode)
	binary.LittleEndian.PutUint64(raw[72:], cmd.Handle)
	binary.LittleEndian.PutUint64(raw[80:], uint64(cmd.Result))
	binary.LittleEndian.PutUint32(raw[88:], cmd.Errno)
	binary.LittleEndian.PutUint32(raw[92:], cmd.Status)
	if fault := mem.WriteBytes(addr, raw[:]); fault != nil {
		t.Fatalf("write hostio command: %v", fault)
	}
}

func readHostIOTestCmd(t *testing.T, mem *GuestMemory, addr uint64) hostIOCommand {
	t.Helper()
	var raw [hostIOCmdSize]byte
	if fault := mem.ReadBytes(addr, raw[:]); fault != nil {
		t.Fatalf("read hostio command: %v", fault)
	}
	return hostIOCommand{
		Op:       binary.LittleEndian.Uint32(raw[0:]),
		Flags:    binary.LittleEndian.Uint32(raw[4:]),
		Path:     binary.LittleEndian.Uint64(raw[8:]),
		PathLen:  binary.LittleEndian.Uint64(raw[16:]),
		Path2:    binary.LittleEndian.Uint64(raw[24:]),
		Path2Len: binary.LittleEndian.Uint64(raw[32:]),
		Buf:      binary.LittleEndian.Uint64(raw[40:]),
		Len:      binary.LittleEndian.Uint64(raw[48:]),
		Offset:   binary.LittleEndian.Uint64(raw[56:]),
		Mode:     binary.LittleEndian.Uint64(raw[64:]),
		Handle:   binary.LittleEndian.Uint64(raw[72:]),
		Result:   int64(binary.LittleEndian.Uint64(raw[80:])),
		Errno:    binary.LittleEndian.Uint32(raw[88:]),
		Status:   binary.LittleEndian.Uint32(raw[92:]),
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

func TestRunEmuListShowsLiveConsoleSocket(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	dir := emuInstanceDir(os.Getpid())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emuConsoleSocketPath(os.Getpid(), 1), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	code, err := RunEmu(&EmuConfig{List: true, Stdout: &stdout})
	if err != nil {
		t.Fatalf("runEmu list: %v", err)
	}
	if code != 0 {
		t.Fatalf("runEmu list exit = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, strconv.Itoa(os.Getpid())) || !strings.Contains(out, "\t1\t") {
		t.Fatalf("list output = %q, want current pid with console 1", out)
	}
}

func TestRunEmuDebugAttachesSingleOtherConsole1(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	const selfPID = 111
	const targetPID = 222
	installFakeEmuProcessTable(t, selfPID, map[int]bool{selfPID: true, targetPID: true})

	path := emuConsoleSocketPath(targetPID, 1)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(path)

	done := make(chan string, 1)
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			done <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		got, err := io.ReadAll(conn)
		if err != nil {
			done <- "read: " + err.Error()
			return
		}
		if _, err := conn.Write([]byte("ack:" + string(got))); err != nil {
			done <- "write: " + err.Error()
			return
		}
		done <- string(got)
	}()

	var stdout bytes.Buffer
	code, err := RunEmu(&EmuConfig{
		Debug:  true,
		Stdin:  strings.NewReader("hello"),
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("runEmu debug: %v", err)
	}
	if code != 0 {
		t.Fatalf("runEmu debug exit = %d, want 0", code)
	}
	if got := <-done; got != "\rhello" {
		t.Fatalf("server read = %q, want enter then hello", got)
	}
	if got := stdout.String(); got != "ack:\rhello" {
		t.Fatalf("debug stdout = %q, want ack then enter then hello", got)
	}
}

func TestRunEmuDebugAttachesSingleOtherConsole1ThreeTimes(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	const selfPID = 111
	const targetPID = 222
	installFakeEmuProcessTable(t, selfPID, map[int]bool{selfPID: true, targetPID: true})

	path := emuConsoleSocketPath(targetPID, 1)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(path)

	done := make(chan string, 3)
	go func() {
		for session := 0; session < 3; session++ {
			conn, err := ln.AcceptUnix()
			if err != nil {
				done <- "accept: " + err.Error()
				return
			}
			got, err := io.ReadAll(conn)
			if err != nil {
				_ = conn.Close()
				done <- "read: " + err.Error()
				return
			}
			if _, err := conn.Write([]byte("ack:" + string(got))); err != nil {
				_ = conn.Close()
				done <- "write: " + err.Error()
				return
			}
			_ = conn.Close()
			done <- string(got)
		}
	}()

	for session := 0; session < 3; session++ {
		msg := fmt.Sprintf("hello%d", session+1)
		var stdout bytes.Buffer
		code, err := RunEmu(&EmuConfig{
			Debug:  true,
			Stdin:  strings.NewReader(msg),
			Stdout: &stdout,
		})
		if err != nil {
			t.Fatalf("session %d runEmu debug: %v", session+1, err)
		}
		if code != 0 {
			t.Fatalf("session %d runEmu debug exit = %d, want 0", session+1, code)
		}
		want := "\r" + msg
		if got := <-done; got != want {
			t.Fatalf("session %d server read = %q, want %q", session+1, got, want)
		}
		if got, want := stdout.String(), "ack:"+want; got != want {
			t.Fatalf("session %d debug stdout = %q, want %q", session+1, got, want)
		}
	}
}

func TestResolveDebugAttachRejectsMultipleOtherEmus(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	const selfPID = 111
	installFakeEmuProcessTable(t, selfPID, map[int]bool{222: true, 333: true})
	for _, pid := range []int{222, 333} {
		dir := emuInstanceDir(pid)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(emuConsoleSocketPath(pid, 1), []byte{}, 0600); err != nil {
			t.Fatal(err)
		}
	}

	err := resolveDebugAttach(&EmuConfig{Debug: true})
	if err == nil || !strings.Contains(err.Error(), "multiple other running emu instances") {
		t.Fatalf("resolveDebugAttach err = %v, want multiple emus error", err)
	}
}

func TestResolveDebugAttachRejectsNoOtherEmus(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	const selfPID = 111
	installFakeEmuProcessTable(t, selfPID, map[int]bool{selfPID: true})
	dir := emuInstanceDir(selfPID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emuConsoleSocketPath(selfPID, 1), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	err := resolveDebugAttach(&EmuConfig{Debug: true})
	if err == nil || !strings.Contains(err.Error(), "no other running emu instances") {
		t.Fatalf("resolveDebugAttach err = %v, want no other emu error", err)
	}
}

func TestResolveDebugAttachRejectsMissingConsole1(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	const selfPID = 111
	const targetPID = 222
	installFakeEmuProcessTable(t, selfPID, map[int]bool{targetPID: true})
	dir := emuInstanceDir(targetPID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emuConsoleSocketPath(targetPID, 2), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	err := resolveDebugAttach(&EmuConfig{Debug: true})
	if err == nil || !strings.Contains(err.Error(), "console 1 is not available") {
		t.Fatalf("resolveDebugAttach err = %v, want missing console 1 error", err)
	}
}

func TestRunEmuAttachConsoleCopiesBytes(t *testing.T) {
	setTestEmunetHome(t, shortTempHome(t))
	path := emuConsoleSocketPath(os.Getpid(), 1)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(path)

	done := make(chan string, 1)
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			done <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		got, err := io.ReadAll(conn)
		if err != nil {
			done <- "read: " + err.Error()
			return
		}
		if _, err := conn.Write([]byte("ack:" + string(got))); err != nil {
			done <- "write: " + err.Error()
			return
		}
		done <- string(got)
	}()

	var stdout bytes.Buffer
	code, err := RunEmu(&EmuConfig{
		AttachPID:     os.Getpid(),
		AttachConsole: 1,
		Stdin:         strings.NewReader("hello"),
		Stdout:        &stdout,
	})
	if err != nil {
		t.Fatalf("runEmu attach: %v", err)
	}
	if code != 0 {
		t.Fatalf("runEmu attach exit = %d, want 0", code)
	}
	if got := <-done; got != "hello" {
		t.Fatalf("server read = %q, want hello", got)
	}
	if got := stdout.String(); got != "ack:hello" {
		t.Fatalf("attach stdout = %q, want ack:hello", got)
	}
}

func installFakeEmuProcessTable(t *testing.T, self int, alive map[int]bool) {
	t.Helper()
	oldCurrentPID := emuCurrentPID
	oldProcessAlive := emuProcessAlive
	emuCurrentPID = func() int { return self }
	emuProcessAlive = func(pid int) bool { return alive[pid] }
	t.Cleanup(func() {
		emuCurrentPID = oldCurrentPID
		emuProcessAlive = oldProcessAlive
	})
}

func shortTempHome(t *testing.T) string {
	t.Helper()
	base := "/private/tmp"
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "ris-emu-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestPrepareBiosGuestRejectsFwJumpFDTKernelOverlap(t *testing.T) {
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_jump.elf"
	if !fileExists(biosPath) {
		t.Skipf("OpenSBI fw_jump fixture not present: %s", biosPath)
	}
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	if err := os.WriteFile(kernelPath, bytes.Repeat([]byte{0x6f}, 8192), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		KernelAddr: fwJumpGenericFDTAddr - 4096,
		MemorySize: Size16GB,
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}
	cfg.setDefaults()
	_, err := prepareBiosGuest(cfg)
	if err == nil {
		t.Fatal("prepareBiosGuest accepted a fw_jump FDT/kernel overlap")
	}
	if !strings.Contains(err.Error(), "fw_dynamic.elf") {
		t.Fatalf("overlap error = %v, want fw_dynamic guidance", err)
	}
}

func TestPrepareBiosGuestFWDynamicSetsInfoBlock(t *testing.T) {
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	if !fileExists(biosPath) {
		t.Skipf("OpenSBI fw_dynamic fixture not present: %s", biosPath)
	}
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	kernel := []byte{0x4d, 0x5a, 0x6f, 0x10}
	if err := os.WriteFile(kernelPath, kernel, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		MemorySize: Size16GB,
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}
	cfg.setDefaults()
	guest, err := prepareBiosGuest(cfg)

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
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
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
	cfg := &EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}
	cfg.setDefaults()
	guest, err := prepareBiosGuest(cfg)
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
	if got := guest.cpu.PrivilegeMode(); got != PrivSupervisor {
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
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	cfg := &EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}
	ok, err := runBiosUntilOutput(cfg, "Linux version 6.17.0", 10_000_000)
	if err != nil {
		t.Fatalf("hand-built Linux banner err = %v\nstdout tail:\n%s\nstderr:\n%s",
			err, tailString(stdout.String(), 4096), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux banner missing\nstdout tail:\n%s\nstderr:\n%s",
			tailString(stdout.String(), 4096), stderr.String())
	}
}

func TestRunEmuBiosFWDynamicHandBuiltLinuxBootsToInitUnder90s(t *testing.T) {
	const bootWallBudget = linuxAlpineSmokeWallBudget
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}
	emunetAddr := installFakeEmunetForLinuxSmoke(t)

	var stdout safeStringWriter
	var stderr bytes.Buffer
	start := time.Now()
	ok, err := runBiosUntilOutputWithin(&EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Net:        true,
		EmunetAddr: emunetAddr,
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, "Run /init as init process", 2_000_000_000, bootWallBudget)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("hand-built Linux /init boot err after %s = %v\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, err, tailString(stdout.String(), 4096), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux /init marker missing after %s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(stdout.String(), 4096), stderr.String())
	}
	bootSeconds, ok := linuxLogSecondsAtMarker(stdout.String(), "Run /init as init process")
	if elapsed > bootWallBudget {
		t.Fatalf("hand-built Linux boot to /init took %s host time, want <= %s; kernel timestamp %.6fs\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, bootWallBudget, bootSeconds, tailString(stdout.String(), 4096), stderr.String())
	}
	if ok {
		t.Logf("hand-built Linux booted to /init in %s host time; kernel timestamp %.6fs", elapsed, bootSeconds)
	} else {
		t.Logf("hand-built Linux booted to /init in %s host time", elapsed)
	}
}

func TestRunEmuBiosFWDynamicHandBuiltLinuxHostFSMountReadWrite(t *testing.T) {
	const bootWallBudget = linuxAlpineSmokeWallBudget
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}
	hostDir := t.TempDir()
	hostDir, err := filepath.EvalSymlinks(hostDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", hostDir, err)
	}
	fromHost := filepath.Join(hostDir, "from-host.txt")
	fromGuest := filepath.Join(hostDir, "from-guest.txt")
	if err := os.WriteFile(fromHost, []byte("hello-from-hostfs-host\n"), 0644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}

	const doneMarker = "HOSTFS-SMOKE-42"
	script := strings.Join([]string{
		"set -e",
		"mkdir -p /host",
		"mount -t hostfs none /host -o " + hostDir,
		"cat /host/from-host.txt",
		"echo hello-from-hostfs-guest > /host/from-guest.txt",
		"cat /host/from-guest.txt",
		"echo HOSTFS-SMOKE-4''2",
	}, "\n") + "\n"

	var stdout safeStringWriter
	var stderr bytes.Buffer
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	go func() {
		defer stdinW.Close()
		deadline := time.Now().Add(bootWallBudget)
		for time.Now().Before(deadline) {
			if linuxInitramfsReady(stdout.String()) {
				_, _ = io.WriteString(stdinW, script)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	ok, err := runBiosUntilOutputWithin(&EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Stdin:      stdinR,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, doneMarker, 2_500_000_000, bootWallBudget)
	elapsed := time.Since(start)
	out := stdout.String()
	if err != nil {
		t.Fatalf("hand-built Linux hostfs smoke err after %s = %v\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, err, tailString(out, 8192), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux hostfs smoke marker missing after %s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(out, 8192), stderr.String())
	}
	if !strings.Contains(out, "hello-from-hostfs-host") {
		t.Fatalf("guest did not cat host-created file\nstdout tail:\n%s", tailString(out, 8192))
	}
	if !strings.Contains(out, "hello-from-hostfs-guest") {
		t.Fatalf("guest did not cat guest-created file\nstdout tail:\n%s", tailString(out, 8192))
	}
	got, err := os.ReadFile(fromGuest)
	if err != nil {
		t.Fatalf("host cannot read guest-created file: %v", err)
	}
	if string(got) != "hello-from-hostfs-guest\n" {
		t.Fatalf("guest-created file = %q, want %q", got, "hello-from-hostfs-guest\n")
	}
	t.Logf("hand-built Linux mounted hostfs and round-tripped host files in %s", elapsed)
}

func TestRunEmuBiosFWDynamicHandBuiltLinuxVirtioNetRegistersEth0(t *testing.T) {
	const bootWallBudget = linuxAlpineSmokeWallBudget
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}
	emunetAddr := installFakeEmunetForLinuxSmoke(t)

	const doneMarker = "NET-SMOKE-42"
	script := strings.Join([]string{
		"test -x /bin/netup",
		"test -x /usr/share/udhcpc/default.script",
		"cat /proc/net/dev",
		"echo NET-SMOKE-4''2",
	}, "\n") + "\n"

	var stdout safeStringWriter
	var stderr bytes.Buffer
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	go func() {
		defer stdinW.Close()
		deadline := time.Now().Add(bootWallBudget)
		for time.Now().Before(deadline) {
			if linuxInitramfsReady(stdout.String()) {
				_, _ = io.WriteString(stdinW, script)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	ok, err := runBiosUntilOutputWithin(&EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Net:        true,
		EmunetAddr: emunetAddr,
		Stdin:      stdinR,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, doneMarker, 2_500_000_000, bootWallBudget)
	elapsed := time.Since(start)
	out := stdout.String()
	if err != nil {
		t.Fatalf("hand-built Linux virtio-net smoke err after %s = %v\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, err, tailString(out, 8192), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux virtio-net smoke marker missing after %s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(out, 8192), stderr.String())
	}
	if !strings.Contains(out, "eth0:") {
		t.Fatalf("guest /proc/net/dev did not list eth0\nstdout tail:\n%s", tailString(out, 8192))
	}
	t.Logf("hand-built Linux registered virtio-net eth0 in %s", elapsed)
}

func TestRunEmuBiosFWDynamicHandBuiltLinuxCtrlCInterruptsPing(t *testing.T) {
	const bootWallBudget = linuxAlpineSmokeWallBudget
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	const doneMarker = "CTRLC-SMOKE-42"
	var stdout safeStringWriter
	var stderr bytes.Buffer
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	go func() {
		defer stdinW.Close()
		deadline := time.Now().Add(bootWallBudget)
		for time.Now().Before(deadline) {
			if linuxInitramfsReady(stdout.String()) {
				_, _ = io.WriteString(stdinW, "ifconfig lo up\nping 127.0.0.1\n")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		for time.Now().Before(deadline) {
			if strings.Contains(stdout.String(), "PING 127.0.0.1") {
				time.Sleep(250 * time.Millisecond)
				_, _ = stdinW.Write([]byte{0x03})
				_, _ = io.WriteString(stdinW, "echo CTRLC-SMOKE-4''2\n")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	ok, err := runBiosUntilOutputWithin(&EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Stdin:      stdinR,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, doneMarker, 2_500_000_000, bootWallBudget)
	elapsed := time.Since(start)
	out := stdout.String()
	if err != nil {
		t.Fatalf("hand-built Linux ctrl-c smoke err after %s = %v\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, err, tailString(out, 8192), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux ctrl-c smoke marker missing after %s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(out, 8192), stderr.String())
	}
	t.Logf("hand-built Linux interrupted foreground ping with ctrl-c in %s", elapsed)
}

func TestDiagRunEmuBiosFWDynamicHandBuiltLinuxInitcallDebug(t *testing.T) {
	if !runLinuxInitcallDiag {
		t.Skip("runLinuxInitcallDiag is false")
	}
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(&EmuConfig{
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
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS smoke fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	code, err := RunEmu(&EmuConfig{
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
const linuxMakeBootArgs = linuxUARTBootArgs + " panic=1 reboot=t init_on_alloc=0 init_on_free=0 audit=0 lsm=capability cma=0 numa=off slub_debug=- lpj=XXXXX"
const linuxAlpineSmokeWallBudget = 90 * time.Second

var (
	runLinuxInitcallDiag            bool
	runLegacyGenericLinuxTimerProbe bool
)

func TestRunEmuBiosFWDynamicLinuxBootsWith512MBRAM(t *testing.T) {
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS boot fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(&EmuConfig{
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
	if !runLegacyGenericLinuxTimerProbe {
		t.Skip("runLegacyGenericLinuxTimerProbe is false")
	}
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS boot fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	ok, err := runBiosUntilOutput(&EmuConfig{
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

func runBiosUntilOutput(cfg *EmuConfig, marker string, maxInstructions uint64) (bool, error) {
	return runBiosUntilOutputWithin(cfg, marker, maxInstructions, 0)
}

func runBiosUntilOutputWithin(cfg *EmuConfig, marker string, maxInstructions uint64, maxElapsed time.Duration) (bool, error) {
	guest, err := prepareBiosGuest(cfg.setDefaults())
	if err != nil {
		return false, err
	}
	defer guest.mem.Free()
	defer guest.mmio.closeUARTOutput()
	defer guest.mmio.closeHostIO()
	defer guest.mmio.closeVirtioNet()

	const chunk = uint64(100_000)
	start := time.Now()
	var used uint64
	for used < maxInstructions {
		step := chunk
		if rem := maxInstructions - used; rem < step {
			step = rem
		}
		res, err := RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, step)
		used += step
		if strings.Contains(writerString(cfg.Stdout), marker) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("%w at pc=%#x insn=%#x state=%+v", err, guest.cpu.PC(), guestInsnForTest(guest.mem, guest.cpu.PC()), guest.cpu.DebugSnapshot())
		}
		if res == RunBudgetExit {
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

type safeStringWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *safeStringWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *safeStringWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func writerString(w interface{}) string {
	if s, ok := w.(interface{ String() string }); ok {
		return s.String()
	}
	return ""
}

func linuxInitramfsReady(output string) bool {
	return strings.Contains(output, "initramfs booted ===")
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func linuxLogSecondsAtMarker(output, marker string) (float64, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		open := strings.IndexByte(line, '[')
		close := strings.IndexByte(line, ']')
		if open < 0 || close <= open {
			continue
		}
		seconds, err := strconv.ParseFloat(strings.TrimSpace(line[open+1:close]), 64)
		if err != nil {
			continue
		}
		return seconds, true
	}
	return 0, false
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

func guestInsnForTest(mem *GuestMemory, pc uint64) uint32 {
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
		res, err := RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, step)
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
		if res == RunBudgetExit {
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
	const path = "xendor/opensbi/build/platform/generic/firmware/fw_jump.elf"
	if !fileExists(path) {
		t.Skipf("OpenSBI firmware fixture not present: %s", path)
	}

	cfg := &EmuConfig{
		BiosPath:   path,
		MemorySize: Size16GB,
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}
	cfg.setDefaults()
	guest, err := prepareBiosGuest(cfg)

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

	_, err = RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, 256)
	if isNullFDTFault(err) {
		t.Fatalf("OpenSBI still dereferenced a null FDT pointer: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code, err := RunEmu(&EmuConfig{
		BiosPath:   path,
		MemorySize: Size16GB,
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

func loadBigEndianU32(mem *GuestMemory, addr uint64) (uint32, *MemFault) {
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

func guestMemoryBytes(t *testing.T, mem *GuestMemory, addr uint64, n int) []byte {
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

func loadLittleEndianU64(t *testing.T, mem *GuestMemory, addr uint64) uint64 {
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
	var fault *MemFault
	return errors.As(err, &fault) && fault.Kind == FaultLoad && fault.Addr < 8
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
			code, err := RunEmu(&EmuConfig{
				RunPath:           "testvectors/jea9linux/elf/write_stdout.elf",
				MemorySize:        Size64MB,
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
	cfg := &EmuConfig{
		RunPath: "testvectors/jea9linux/elf/write_stdout.elf",
		JITLazy: true,
		JITAOT:  true,
	}
	if err := cfg.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted both -jitlazy and -jitaot")
	}
}

func TestEmuDefaultFlagsRunGoTimeNowFixtureCompletes(t *testing.T) {
	cfg, stdout, stderr := parseEmuConfigForTest(t,
		"-run", "testvectors/jea9linux/go/elf/timenow.elf",
	)

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := RunEmu(cfg)
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
	cfg := &EmuConfig{}
	cfg.setDefaults()
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

	biosCfg := &EmuConfig{BiosPath: "testvectors/jea9linux/elf/write_stdout.elf"}
	biosCfg.setDefaults()
	if biosCfg.Budget != defaultEmuBiosBudget {
		t.Fatalf("BIOS Budget = %q, want %q", biosCfg.Budget, defaultEmuBiosBudget)
	}
	biosBudget, err := biosCfg.schedulerBudget()
	if err != nil {
		t.Fatalf("BIOS schedulerBudget default: %v", err)
	}
	if biosBudget != ^uint64(0) {
		t.Fatalf("BIOS schedulerBudget = %d, want max", biosBudget)
	}
}

func TestParseEmuJITModeFlags(t *testing.T) {
	sandbox, _, _ := parseEmuConfigForTest(t,
		"-run", "testvectors/jea9linux/elf/write_stdout.elf",
		"-sandbox",
	)
	if sandbox.MemoryModel != MemoryModelSandbox {
		t.Fatalf("-sandbox parsed as MemoryModel=%s, want %s", sandbox.MemoryModel, MemoryModelSandbox)
	}

	lazy, _, _ := parseEmuConfigForTest(t,
		"-run", "testvectors/jea9linux/elf/write_stdout.elf",
		"-jitlazy",
	)
	if !lazy.JITLazy || lazy.JITAOT {
		t.Fatalf("-jitlazy parsed as JITLazy=%v JITAOT=%v", lazy.JITLazy, lazy.JITAOT)
	}

	aot, _, _ := parseEmuConfigForTest(t,
		"-run", "testvectors/jea9linux/elf/write_stdout.elf",
		"-jitaot",
	)
	if !aot.JITAOT || aot.JITLazy {
		t.Fatalf("-jitaot parsed as JITLazy=%v JITAOT=%v", aot.JITLazy, aot.JITAOT)
	}

	interp, _, _ := parseEmuConfigForTest(t,
		"-run", "testvectors/jea9linux/elf/write_stdout.elf",
	)
	if interp.MemoryModel != MemoryModelLinear {
		t.Fatalf("default parsed as MemoryModel=%s, want %s", interp.MemoryModel, MemoryModelLinear)
	}
	if interp.JITLazy || interp.JITAOT {
		t.Fatalf("default parsed as JITLazy=%v JITAOT=%v", interp.JITLazy, interp.JITAOT)
	}
}

func BenchmarkRunEmuGoHelloInterpreter(b *testing.B) {
	benchmarkRunEmuGoHello(b, &EmuConfig{})
}

func BenchmarkRunEmuGoHelloLazyJIT(b *testing.B) {
	benchmarkRunEmuGoHello(b, &EmuConfig{JITLazy: true})
}

func BenchmarkRunEmuGoHelloAOTJIT(b *testing.B) {
	benchmarkRunEmuGoHello(b, &EmuConfig{JITAOT: true})
}

func benchmarkRunEmuGoHello(b *testing.B, mode *EmuConfig) {
	b.Helper()
	b.ReportAllocs()
	var totalStats EmuJITStats
	for i := 0; i < b.N; i++ {
		var stdout, stderr bytes.Buffer
		var stats EmuJITStats
		cfg := mode
		cfg.RunPath = "testvectors/jea9linux/go/elf/hello.elf"
		cfg.MemorySize = Size16GB
		cfg.InstructionBudget = 1 << 20
		cfg.Stdin = strings.NewReader("")
		cfg.Stdout = &stdout
		cfg.Stderr = &stderr
		cfg.JITStats = &stats

		code, err := RunEmu(cfg)
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

func parseEmuConfigForTest(t *testing.T, args ...string) (*EmuConfig, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	fs := flag.NewFlagSet("emu-test", flag.ContinueOnError)
	var flagErrors bytes.Buffer
	fs.SetOutput(&flagErrors)

	cfg := &EmuConfig{}
	defineEmuFlagsForTest(fs, cfg)
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
	return cfg, &stdout, &stderr
}

func defineEmuFlagsForTest(fs *flag.FlagSet, c *EmuConfig) {
	if c.MemorySize == 0 {
		c.MemorySize = defaultEmuMemorySize
	}
	fs.StringVar(&c.RunPath, "run", "", "path to RISCV ELF binary to run")
	fs.StringVar(&c.BiosPath, "bios", "", "path to RISCV machine-mode BIOS/firmware ELF to boot")
	fs.StringVar(&c.KernelPath, "kernel", "", "path to kernel or next-stage payload to load with -bios")
	fs.Uint64Var(&c.KernelAddr, "kernel-addr", 0, "guest physical address for raw -kernel payloads; default 0x80200000")
	fs.StringVar(&c.InitrdPath, "initrd", "", "path to initrd image to load and advertise in the BIOS FDT")
	fs.StringVar(&c.Append, "append", "", "kernel command line for generated BIOS FDT bootargs")
	fs.StringVar(&c.DTBPath, "dtb", "", "path to external flattened device tree blob for -bios")
	fs.StringVar(&c.DumpDTBPath, "dump-dtb", "", "write the BIOS FDT blob to this path before boot")
	fs.BoolVar(&c.HostIO, "hostio", false, "enable non-hermetic custom MMIO host filesystem passthrough for -bios")
	fs.BoolVar(&c.Net, "net", false, "enable non-hermetic virtio-net MMIO device for -bios")
	fs.BoolVar(&c.NetDirectTailnet, "net-direct", false, "connect -net directly to one tsnet stack instead of the shared emunet leader")
	fs.StringVar(&c.EmunetAddr, "emunet-addr", "", "local emunet rendezvous address; empty uses 127.0.0.1:7557")
	fs.BoolVar(&c.EmunetTrace, "emunet-trace", false, "write emunet packet drop trace lines to the emunet oplog")
	fs.StringVar(&c.TsnetDir, "tsnet-dir", "", "tsnet state directory; empty uses $HOME/.emunet/riscv-emu")
	fs.StringVar(&c.TsnetHostname, "tsnet-hostname", "", "tsnet hostname; empty uses riscv-emu")
	fs.StringVar(&c.TsnetAuthKey, "tsnet-authkey", "", "tsnet auth key for unattended first authorization")
	fs.BoolVar(&c.TsnetEphemeral, "tsnet-ephemeral", false, "request ephemeral tsnet node state")
	fs.StringVar(&c.TsnetGuestIPv4, "tsnet-guest-ipv4", "", "override guest DHCP IPv4 address")
	fs.StringVar(&c.TsnetDHCPServerIPv4, "tsnet-dhcp-server-ipv4", "", "override DHCP server IPv4 advertised to the guest")
	fs.StringVar(&c.TsnetDNSIPv4, "tsnet-dns-ipv4", "", "override DNS IPv4 advertised to the guest")
	fs.StringVar(&c.Machine, "machine", "virt", "machine model for -bios; currently only virt")
	fs.Uint64Var(&c.Seed, "seed", 0, "pseudo random number generator seed")
	fs.StringVar(&c.Memory, "mem", "", "guest memory size as bytes or KB/MB/GB/TB; with -bios this is RAM advertised to Linux")
	fs.StringVar(&c.Budget, "budget", "", "scheduler/run budget as an instruction count, duration, or max")
	fs.Var(SandboxMemoryModelFlag{Model: &c.MemoryModel}, "sandbox", "use sandboxed guest memory for -run; default linear JIT skips the sandbox mask and jea9linux access checks")
	fs.BoolVar(&c.JITLazy, "jitlazy", false, "run with the native lazy JIT instead of the interpreter")
	fs.BoolVar(&c.JITAOT, "jitaot", false, "run with explicit AOT JIT instead of the interpreter")
	fs.BoolVar(&c.Hermit, "hermit", false, "disable host filesystem passthrough")
	fs.BoolVar(&c.Deadlock, "deadlock", false, "run each thread until it blocks before scheduling another thread")
	fs.BoolVar(&c.PRNG, "prng", false, "use deterministic PRNG scheduling quantum and clock advancement")
	fs.BoolVar(&c.Chaos, "chaos", false, "use deterministic chaos scheduling")
	fs.Int64Var(&c.RealtimeOffsetNS, "init", defaultEmuRealtimeStartNS, "initial realtime clock value in nanoseconds since Unix epoch")
	fs.StringVar(&c.Idle, "idle", "", "BIOS/Linux WFI host sleep cap as a duration")
	fs.BoolVar(&c.List, "list", false, "list running emu instances with attachable consoles")
	fs.BoolVar(&c.Debug, "debug", false, "attach to console 1 of the single other running emu instance")
	fs.IntVar(&c.AttachPID, "pid", 0, "attach mode: host PID of an existing emu process")
	fs.IntVar(&c.AttachConsole, "console", -1, "attach mode: console index to attach to with -pid")
}

func runEmuFixtureOutput(t *testing.T, seed uint64) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := RunEmu(&EmuConfig{
		RunPath:           "testvectors/jea9linux/go/elf/cryptorand.elf",
		MemorySize:        Size16GB,
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
