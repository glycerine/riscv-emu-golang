//go:build tsnet

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRunEmuBiosFWDynamicHandBuiltLinuxEmunetNetupGatewaySmoke(t *testing.T) {
	const bootWallBudget = 20 * time.Second
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux-6.17-hand-built/Image"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RISCV_EMU_EMUNET_ADDR", reserveTestEmunetAddr(t))
	installFakeEmunetLeaderHook(t, 20*time.Millisecond)

	const doneMarker = "EMUNET-SMOKE-42"
	script := strings.Join([]string{
		"set -e",
		"netup",
		"ifconfig eth0",
		"route -n",
		"cat /etc/resolv.conf",
		"ping -c 1 10.77.0.1",
		"echo EMUNET-SMOKE-4''2",
	}, "\n") + "\n"

	var stdout safeStringWriter
	var stderr bytes.Buffer
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	go func() {
		defer stdinW.Close()
		deadline := time.Now().Add(bootWallBudget)
		for time.Now().Before(deadline) {
			if strings.Contains(stdout.String(), "=== RISC-V initramfs booted ===") {
				_, _ = io.WriteString(stdinW, script)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	ok, err := runBiosUntilOutputWithin(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Net:        true,
		Stdin:      stdinR,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, doneMarker, 2_500_000_000, bootWallBudget)
	elapsed := time.Since(start)
	out := stdout.String()
	if err != nil {
		t.Fatalf("hand-built Linux emunet smoke err after %s = %v\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, err, tailString(out, 8192), stderr.String())
	}
	if !ok {
		t.Fatalf("hand-built Linux emunet smoke marker missing after %s\nstdout tail:\n%s\nstderr:\n%s",
			elapsed, tailString(out, 8192), stderr.String())
	}
	for _, want := range []string{"10.77.0.2", "10.77.0.1", "100.100.100.100"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hand-built Linux emunet smoke output missing %q\nstdout tail:\n%s", want, tailString(out, 8192))
		}
	}
	t.Logf("hand-built Linux configured emunet and pinged gateway in %s", elapsed)
}
