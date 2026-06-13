package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const exitMarker = "GOCPU_QEMU_TEST_EXIT"

const (
	linuxRebootMagic1      = 0xfee1dead
	linuxRebootMagic2      = 672274793
	linuxRebootCmdRestart  = 0x01234567
	linuxRebootCmdPowerOff = 0x4321fedc
)

func main() {
	status := run()
	fmt.Printf("\n%s=%d\n", exitMarker, status)
	sync()
	reboot(linuxRebootCmdPowerOff)
	reboot(linuxRebootCmdRestart)
	select {}
}

func run() int {
	_ = os.MkdirAll("/tmp", 0o1777)
	_ = os.MkdirAll("/dev", 0o755)
	_ = syscall.Mknod("/dev/null", syscall.S_IFCHR|0o666, int((1<<8)|3))
	_ = syscall.Mknod("/dev/console", syscall.S_IFCHR|0o600, int((5<<8)|1))

	args := []string{"/riscv-arm64.test"}
	args = append(args, readArgs("/test-argv")...)
	if len(args) == 1 {
		args = append(args, "-test.v")
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	extraEnv := readArgs("/test-env")
	cmd.Env = []string{
		"TMPDIR=/tmp",
		"HOME=/",
		"PATH=/",
	}
	if !hasEnvKey(extraEnv, "GOCPU_VIZJIT") && !hasEnvKey(extraEnv, "GOCPU_VIZJIT_OFF") {
		cmd.Env = append(cmd.Env, "GOCPU_VIZJIT_OFF=1")
	}
	cmd.Env = append(cmd.Env, extraEnv...)

	var status int
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					status = 128 + int(ws.Signal())
				} else {
					status = ws.ExitStatus()
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "qemu init: running test binary: %v\n", err)
			status = 125
		}
	}
	if hasEnvKey(extraEnv, "GOCPU_QEMU_DUMP_VIZJIT") {
		dumpVizJIT(envValue(extraEnv, "GOCPU_VIZJIT"))
	}
	return status
}

func sync() {
	_, _, _ = syscall.Syscall(syscall.SYS_SYNC, 0, 0, 0)
}

func reboot(cmd uintptr) {
	_, _, _ = syscall.Syscall(
		syscall.SYS_REBOOT,
		uintptr(linuxRebootMagic1),
		uintptr(linuxRebootMagic2),
		cmd,
	)
}

func readArgs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var args []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		arg := sc.Text()
		if arg == "" {
			continue
		}
		args = append(args, arg)
	}
	return args
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func dumpVizJIT(dir string) {
	if dir == "" {
		return
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.asm"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "qemu init: glob VizJIT dumps: %v\n", err)
		return
	}
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qemu init: read VizJIT dump %s: %v\n", path, err)
			continue
		}
		fmt.Printf("\n==== %s ====\n%s\n", path, data)
	}
}
