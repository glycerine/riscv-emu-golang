package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/glycerine/riscv-emu-golang/keysmith"
)

const (
	defaultRootRel      = "xendor/alpine-minirootfs-3.24.1-riscv64"
	defaultInitramfsRel = "xendor/linux/initramfs.cpio.gz"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "rekey: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("rekey", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var repoRoot string
	var rootDir string
	var initramfsPath string
	var keyName string
	var userKeyDir string

	fs.StringVar(&repoRoot, "repo-root", "", "riscv-emu-golang checkout root; empty searches from the current directory")
	fs.StringVar(&rootDir, "root", "", "unpacked initramfs root; empty uses xendor/alpine-minirootfs-3.24.1-riscv64 under repo-root")
	fs.StringVar(&initramfsPath, "initramfs", "", "initramfs cpio.gz to write; empty uses xendor/linux/initramfs.cpio.gz under repo-root")
	fs.StringVar(&keyName, "key", keysmith.DefaultKeyName, "ssh key name for $HOME/.ssh/id_ed25519_${key}")
	fs.StringVar(&userKeyDir, "user-key-dir", "", "directory for host login key; empty uses $HOME/.ssh")

	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := resolveRepoRoot(repoRoot)
	if err != nil {
		return err
	}
	if rootDir == "" {
		rootDir = filepath.Join(root, defaultRootRel)
	}
	if initramfsPath == "" {
		initramfsPath = filepath.Join(root, defaultInitramfsRel)
	}

	res, err := keysmith.Prepare(keysmith.Config{
		RootDir:       rootDir,
		InitramfsPath: initramfsPath,
		KeyName:       keyName,
		UserKeyDir:    userKeyDir,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rekey: wrote %s\n", res.InitramfsPath)
	fmt.Fprintf(os.Stderr, "rekey: wrote host login key %s\n", res.UserPrivateKeyPath)

	fmt.Fprintf(os.Stderr, "rekey: go install ./cmd/emul\n")
	if err := goInstallEmul(root); err != nil {
		return err
	}

	emulPath, err := installedEmulPath(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rekey: exec %s\n", emulPath)
	return execEmul(emulPath, append([]string{emulPath}, fs.Args()...))
}

func resolveRepoRoot(flagValue string) (string, error) {
	if flagValue != "" {
		root, err := filepath.Abs(flagValue)
		if err != nil {
			return "", fmt.Errorf("resolve -repo-root %q: %w", flagValue, err)
		}
		if err := validateRepoRoot(root); err != nil {
			return "", err
		}
		return root, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	for {
		if validateRepoRoot(wd) == nil {
			return wd, nil
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", fmt.Errorf("could not find riscv-emu-golang repo root from %q; pass -repo-root", wd)
		}
		wd = parent
	}
}

func validateRepoRoot(root string) error {
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return fmt.Errorf("%q is not a repo root: %w", root, err)
	}
	if !bytes.Contains(goMod, []byte("module github.com/glycerine/riscv-emu-golang")) {
		return fmt.Errorf("%q is not github.com/glycerine/riscv-emu-golang", root)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "emul", "emul.go")); err != nil {
		return fmt.Errorf("%q is missing cmd/emul/emul.go: %w", root, err)
	}
	return nil
}

func goInstallEmul(repoRoot string) error {
	cmd := exec.Command("go", "install", "./cmd/emul")
	cmd.Dir = repoRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go install ./cmd/emul: %w", err)
	}
	return nil
}

func installedEmulPath(repoRoot string) (string, error) {
	gobin, gopath, goexe, err := goEnv(repoRoot)
	if err != nil {
		return "", err
	}
	if gobin == "" {
		if gopath == "" {
			return "", fmt.Errorf("go env GOPATH is empty")
		}
		parts := filepath.SplitList(gopath)
		if len(parts) == 0 || parts[0] == "" {
			return "", fmt.Errorf("go env GOPATH has no usable entries: %q", gopath)
		}
		gobin = filepath.Join(parts[0], "bin")
	}
	return filepath.Join(gobin, "emul"+goexe), nil
}

func goEnv(repoRoot string) (gobin, gopath, goexe string, err error) {
	cmd := exec.Command("go", "env", "GOBIN", "GOPATH", "GOEXE")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", "", "", fmt.Errorf("go env failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	if len(lines) != 3 {
		return "", "", "", fmt.Errorf("go env returned %d lines; want 3", len(lines))
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2]), nil
}
