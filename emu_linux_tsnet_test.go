package riscv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	emunetLinuxPeerHelperEnv = "RISCV_EMUNET_LINUX_PEER_HELPER"
	emunetLinuxPeerRoleEnv   = "RISCV_EMUNET_LINUX_PEER_ROLE"
	emunetLinuxPeerAddrEnv   = "RISCV_EMUNET_LINUX_PEER_ADDR"
	emunetLinuxPeerTsnetEnv  = "RISCV_EMUNET_LINUX_PEER_TSNET_DIR"
	emunetLinuxPeerModeEnv   = "RISCV_EMUNET_LINUX_PEER_MODE"
)

func TestRunEmuBiosFWDynamicHandBuiltLinuxEmunetNetupGatewaySmoke(t *testing.T) {
	const bootWallBudget = linuxAlpineSmokeWallBudget
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
			if linuxInitramfsReady(stdout.String()) {
				_, _ = io.WriteString(stdinW, script)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	cfg := &EmuConfig{
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
	}
	ok, err := runBiosUntilOutputWithin(cfg, doneMarker, 2_500_000_000, bootWallBudget)
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
	const timeout = 180 * time.Second
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("hand-built Linux BIOS fixture not present: %s", path)
		}
	}
	tsnetDir := requireExistingTsnetAuthState(t)

	cases := []struct {
		name       string
		firstMode  string
		secondMode string
	}{
		{name: "follower_pings_leader", firstMode: "hold", secondMode: "ping"},
		{name: "leader_pings_follower", firstMode: "ping", secondMode: "hold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			configHome := t.TempDir()
			rpcDir := t.TempDir()
			emunetAddr := reserveTestEmunetAddr(t)
			writeTestRPC25519HostCIDConfigHome(t, configHome)

			first := startRealEmunetLinuxPeerHelper(t, ctx, configHome, rpcDir, emunetAddr, tsnetDir, "A", tc.firstMode)
			defer first.killAndWait()
			second := startRealEmunetLinuxPeerHelper(t, ctx, configHome, rpcDir, emunetAddr, tsnetDir, "B", tc.secondMode)
			defer second.killAndWait()

			pinger := first
			holder := second
			if tc.secondMode == "ping" {
				pinger = second
				holder = first
			}
			waitForPingerExit(t, pinger)
			holder.killAndWait()
		})
	}
}

func TestEmunetLinuxPeerGuestHelper(t *testing.T) {
	if os.Getenv(emunetLinuxPeerHelperEnv) != "1" {
		return
	}
	const bootWallBudget = 120 * time.Second
	const biosPath = "xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "xendor/linux-6.17-hand-built/Image"
	const initrdPath = "xendor/linux/initramfs.cpio.gz"
	role := os.Getenv(emunetLinuxPeerRoleEnv)
	emunetAddr := os.Getenv(emunetLinuxPeerAddrEnv)
	tsnetDir := os.Getenv(emunetLinuxPeerTsnetEnv)
	mode := os.Getenv(emunetLinuxPeerModeEnv)
	if role == "" || emunetAddr == "" || tsnetDir == "" || mode == "" {
		t.Fatalf("missing helper env: role=%q addr=%q tsnet=%q mode=%q", role, emunetAddr, tsnetDir, mode)
	}

	script := realEmunetLinuxPeerScript(mode)
	var stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(bootWallBudget):
			fmt.Fprintf(os.Stderr, "peer %s still running after %s in mode %s\n", role, bootWallBudget, mode)
		case <-done:
		}
	}()
	cfg := &EmuConfig{
		BiosPath:    biosPath,
		KernelPath:  kernelPath,
		InitrdPath:  initrdPath,
		Append:      linuxMakeBootArgs,
		Memory:      "256MB",
		HostIO:      true,
		Net:         true,
		EmunetAddr:  emunetAddr,
		EmunetTrace: true,
		TsnetDir:    tsnetDir,
		Stdin:       strings.NewReader(script),
		Stdout:      os.Stdout,
		Stderr:      &stderr,
	}
	code, err := RunEmu(cfg)
	close(done)
	if stderr.Len() != 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}
	if err != nil {
		t.Fatalf("peer %s Linux helper err = %v\nstderr:\n%s", role, err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("peer %s Linux helper exit code = %d\nstderr:\n%s", role, code, stderr.String())
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

func requireExistingTsnetAuthState(t *testing.T) string {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("skipping real emunet integration: HOME is unset, cannot locate existing tsnet auth state")
	}
	dir := filepath.Join(home, defaultEmunetSubdir, defaultTsnetStateSubdir)
	state := filepath.Join(dir, "tailscaled.state")
	info, err := os.Stat(state)
	if err != nil {
		t.Logf("skipping real emunet integration: no existing tsnet auth state at %s: %v", state, err)
		t.Skip("real emunet integration requires existing tsnet authentication")
	}
	if info.Size() == 0 {
		t.Logf("skipping real emunet integration: existing tsnet auth state is empty at %s", state)
		t.Skip("real emunet integration requires non-empty tsnet authentication")
	}
	t.Logf("real emunet integration using existing tsnet auth state %s (%d bytes)", state, info.Size())
	return dir
}

func startRealEmunetLinuxPeerHelper(t *testing.T, ctx context.Context, configHome, rpcDir, emunetAddr, tsnetDir, role, mode string) *emunetLinuxPeerProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestEmunetLinuxPeerGuestHelper$", "-test.v")
	cmd.Env = envWith(os.Environ(), map[string]string{
		"XDG_CONFIG_HOME":          configHome,
		"RPC25519_SERVER_DATA_DIR": rpcDir,
		emunetLinuxPeerHelperEnv:   "1",
		emunetLinuxPeerRoleEnv:     role,
		emunetLinuxPeerAddrEnv:     emunetAddr,
		emunetLinuxPeerTsnetEnv:    tsnetDir,
		emunetLinuxPeerModeEnv:     mode,
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

func writeTestRPC25519HostCIDConfigHome(t *testing.T, configHome string) {
	t.Helper()
	path := filepath.Join(configHome, ".config", "rpc25519", "host.cid")
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

func realEmunetLinuxPeerScript(mode string) string {
	lines := []string{
		"set +e",
		"i=0",
		"own=",
		"while [ $i -lt 90 ]; do",
		"  own=$(ifconfig eth0 2>/dev/null | sed -n 's/.*inet addr:\\([0-9.]*\\).*/\\1/p')",
		"  case \"$own\" in 10.77.0.*) break ;; esac",
		"  netup 2>/dev/null || true",
		"  sleep 1",
		"  i=$((i + 1))",
		"done",
		"ifconfig eth0",
		"cat /proc/net/route",
		"cat /proc/net/arp 2>/dev/null || true",
		"case \"$own\" in 10.77.0.*) ;; *) while true; do sleep 60; done ;; esac",
	}
	if mode == "hold" {
		lines = append(lines, "while true; do sleep 60; done")
		return strings.Join(lines, "\n") + "\n"
	}
	lines = append(lines,
		"i=0",
		"while [ $i -lt 120 ]; do",
		"  for target in 10.77.0.2 10.77.0.3 10.77.0.4 10.77.0.5 10.77.0.6 10.77.0.7 10.77.0.8 10.77.0.9 10.77.0.10; do",
		"    [ \"$target\" = \"$own\" ] && continue",
		"    if ping -c 1 -W 1 \"$target\"; then",
		"      reboot -f",
		"      while true; do sleep 60; done",
		"    fi",
		"  done",
		"  sleep 1",
		"  i=$((i + 1))",
		"done",
		"cat /proc/net/arp 2>/dev/null || true",
		"ifconfig eth0",
		"while true; do sleep 60; done",
	)
	return strings.Join(lines, "\n") + "\n"
}

func waitForPingerExit(t *testing.T, p *emunetLinuxPeerProcess) {
	t.Helper()
	err := <-p.done
	if p.cmd != nil {
		p.cmd.Process = nil
	}
	if err != nil {
		home := os.Getenv("HOME")
		t.Fatalf("pinger %s failed before guest reboot: %v\nstdout tail:\n%s\nstderr:\n%s\nemunet logs:\n%s",
			p.role, err, tailString(p.stdout.String(), 32768), p.stderr.String(), emunetTestLogDump(home))
	}
}

func emunetTestLogDump(home string) string {
	dir := filepath.Join(home, ".local", "state", "emunet")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "oplog.") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Sprintf("no oplogs in %s", dir)
	}
	var b strings.Builder
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(&b, "== %s ==\nread error: %v\n", path, err)
			continue
		}
		fmt.Fprintf(&b, "== %s ==\n%s\n", path, tailString(string(data), 32768))
	}
	return b.String()
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
