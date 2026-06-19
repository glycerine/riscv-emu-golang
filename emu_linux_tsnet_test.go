package riscv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	emunetLinuxPeerHelperEnv = "RISCV_EMUNET_LINUX_PEER_HELPER"
	emunetLinuxPeerRoleEnv   = "RISCV_EMUNET_LINUX_PEER_ROLE"
	emunetLinuxPeerAddrEnv   = "RISCV_EMUNET_LINUX_PEER_ADDR"
	emunetLinuxPeerIPEnv     = "RISCV_EMUNET_LINUX_PEER_IP"
	emunetLinuxPeerTargetEnv = "RISCV_EMUNET_LINUX_PEER_TARGET"
)

func TestRunEmuBiosFWDynamicHandBuiltLinuxEmunetNetupGatewaySmoke(t *testing.T) {
	const bootWallBudget = 20 * time.Second
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	setTestEmunetHome(t, t.TempDir())
	emunetAddr := reserveTestEmunetAddr(t)
	installFakeEmunetLeaderHook(t, 20*time.Millisecond)

	const doneMarker = "EMUNET-SMOKE-42"
	script := strings.Join([]string{
		"set -e",
		"i=0",
		"while [ $i -lt 10 ]; do ifconfig eth0 | grep -q 10.77.0.2 && break; i=$((i + 1)); sleep 1; done",
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
		EmunetAddr: emunetAddr,
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

func TestRunEmuBiosFWDynamicHandBuiltLinuxTwoEmuProcessesPingEachOther(t *testing.T) {
	const timeout = 120 * time.Second
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	home := t.TempDir()
	rpcDir := t.TempDir()
	emunetAddr := reserveTestEmunetAddr(t)
	writeTestRPC25519HostCID(t, home)

	peerA := startEmunetLinuxPeerHelper(t, ctx, home, rpcDir, emunetAddr, "A", "10.77.0.2", "")
	defer peerA.killAndWait()
	waitForPeerOutput(t, peerA, "EMUPEER-A-READY", 50*time.Second)

	peerB := startEmunetLinuxPeerHelper(t, ctx, home, rpcDir, emunetAddr, "B", "10.77.0.3", "10.77.0.2")
	defer peerB.killAndWait()
	waitForPeerOutput(t, peerB, "EMUPEER-B-READY", 50*time.Second)

	waitForPeerDone(t, peerB)
	out := peerB.stdout.String()
	if !strings.Contains(out, "EMUPEER-B-SUCCESS") {
		t.Fatalf("peer B did not report success\nstdout tail:\n%s\nstderr:\n%s",
			tailString(out, 8192), peerB.stderr.String())
	}
}

func TestEmunetLinuxPeerGuestHelper(t *testing.T) {
	if os.Getenv(emunetLinuxPeerHelperEnv) != "1" {
		return
	}
	const bootWallBudget = 90 * time.Second
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	role := os.Getenv(emunetLinuxPeerRoleEnv)
	emunetAddr := os.Getenv(emunetLinuxPeerAddrEnv)
	expectIP := os.Getenv(emunetLinuxPeerIPEnv)
	targetIP := os.Getenv(emunetLinuxPeerTargetEnv)
	if role == "" || emunetAddr == "" || expectIP == "" {
		t.Fatalf("missing helper env: role=%q addr=%q ip=%q target=%q", role, emunetAddr, expectIP, targetIP)
	}
	installFakeEmunetLeaderHook(t, 20*time.Millisecond)

	doneMarker := fmt.Sprintf("EMUPEER-%s-FINISHED", role)
	successMarker := fmt.Sprintf("EMUPEER-%s-SUCCESS", role)
	script := strings.Join([]string{
		"set -u",
		"i=0",
		fmt.Sprintf("while [ $i -lt 20 ]; do if ifconfig eth0 | grep -q 'inet addr:%s'; then break; fi; i=$((i + 1)); sleep 1; done", expectIP),
		"ifconfig eth0",
		"cat /proc/net/route",
		fmt.Sprintf("if ! ifconfig eth0 | grep -q 'inet addr:%s'; then echo EMUPEER-%s-WRONG-IP; echo %s; exit 0; fi", expectIP, role, doneMarker),
		fmt.Sprintf("echo EMUPEER-%s-READY", role),
	}, "\n") + "\n"
	if targetIP == "" {
		script += "while true; do sleep 1; done\n"
	} else {
		script += strings.Join([]string{
			"success=0",
			"i=0",
			fmt.Sprintf("while [ $i -lt 30 ]; do if ping -c 1 -W 1 %s; then echo %s; success=1; break; fi; i=$((i + 1)); sleep 1; done", targetIP, successMarker),
			fmt.Sprintf("if [ \"$success\" != 1 ]; then echo EMUPEER-%s-FAIL; fi", role),
			"cat /proc/net/arp 2>/dev/null || true",
			"ifconfig eth0",
			fmt.Sprintf("echo %s", doneMarker),
		}, "\n") + "\n"
	}

	stdout := &teeSafeStringWriter{out: os.Stdout}
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

	ok, err := runBiosUntilOutputWithin(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxMakeBootArgs,
		Memory:     "256MB",
		HostIO:     true,
		Net:        true,
		EmunetAddr: emunetAddr,
		Stdin:      stdinR,
		Stdout:     stdout,
		Stderr:     &stderr,
	}, doneMarker, 30_000_000_000, bootWallBudget)
	out := stdout.String()
	if stderr.Len() != 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}
	if err != nil {
		t.Fatalf("peer %s Linux helper err = %v\nstdout tail:\n%s\nstderr:\n%s",
			role, err, tailString(out, 8192), stderr.String())
	}
	if !ok {
		t.Fatalf("peer %s Linux helper marker missing\nstdout tail:\n%s\nstderr:\n%s",
			role, tailString(out, 8192), stderr.String())
	}
	if targetIP != "" && !strings.Contains(out, successMarker) {
		t.Fatalf("peer %s did not ping target %s\nstdout tail:\n%s\nstderr:\n%s",
			role, targetIP, tailString(out, 8192), stderr.String())
	}
}

type emunetLinuxPeerProcess struct {
	role   string
	cmd    *exec.Cmd
	stdout safeStringWriter
	stderr safeStringWriter
	done   chan error
}

type teeSafeStringWriter struct {
	safeStringWriter
	out io.Writer
}

func (w *teeSafeStringWriter) Write(p []byte) (int, error) {
	n, err := w.safeStringWriter.Write(p)
	if w.out != nil {
		_, _ = w.out.Write(p)
	}
	return n, err
}

func startEmunetLinuxPeerHelper(t *testing.T, ctx context.Context, home, rpcDir, emunetAddr, role, ip, target string) *emunetLinuxPeerProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestEmunetLinuxPeerGuestHelper$", "-test.v")
	cmd.Env = envWith(os.Environ(), map[string]string{
		"HOME":                     home,
		"XDG_CONFIG_HOME":          "",
		"RPC25519_SERVER_DATA_DIR": rpcDir,
		emunetLinuxPeerHelperEnv:   "1",
		emunetLinuxPeerRoleEnv:     role,
		emunetLinuxPeerAddrEnv:     emunetAddr,
		emunetLinuxPeerIPEnv:       ip,
		emunetLinuxPeerTargetEnv:   target,
		"GOCPU_VIZJIT_OFF":         "1",
	})
	p := &emunetLinuxPeerProcess{
		role: role,
		cmd:  cmd,
		done: make(chan error, 1),
	}
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start peer %s helper: %v", role, err)
	}
	go func() {
		p.done <- cmd.Wait()
	}()
	return p
}

func writeTestRPC25519HostCID(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, ".config", "rpc25519", "host.cid")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create rpc25519 config dir: %v", err)
	}
	const cid = "hostCID-0123456789abcdefghijklmnopqr\n"
	if len(cid) != 37 {
		t.Fatalf("test rpc25519 host.cid length = %d, want 37", len(cid))
	}
	if err := os.WriteFile(path, []byte(cid), 0600); err != nil {
		t.Fatalf("write rpc25519 host.cid: %v", err)
	}
}

func waitForPeerOutput(t *testing.T, p *emunetLinuxPeerProcess, marker string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.stdout.String(), marker) {
			return
		}
		select {
		case err := <-p.done:
			t.Fatalf("peer %s exited before %q: %v\nstdout tail:\n%s\nstderr:\n%s",
				p.role, marker, err, tailString(p.stdout.String(), 8192), p.stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for peer %s marker %q\nstdout tail:\n%s\nstderr:\n%s",
		p.role, marker, tailString(p.stdout.String(), 8192), p.stderr.String())
}

func waitForPeerDone(t *testing.T, p *emunetLinuxPeerProcess) {
	t.Helper()
	err := <-p.done
	if p.cmd != nil {
		p.cmd.Process = nil
	}
	if err != nil {
		t.Fatalf("peer %s helper failed: %v\nstdout tail:\n%s\nstderr:\n%s",
			p.role, err, tailString(p.stdout.String(), 8192), p.stderr.String())
	}
}

func (p *emunetLinuxPeerProcess) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
}

func (p *emunetLinuxPeerProcess) killAndWait() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(time.Second):
	}
}

func envWith(base []string, set map[string]string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, replace := set[key]; replace {
				continue
			}
		}
		out = append(out, kv)
	}
	for key, value := range set {
		out = append(out, key+"="+value)
	}
	return out
}
