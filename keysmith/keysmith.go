// Package keysmith prepares a guest initramfs tree for SSH-based emulator tests.
package keysmith

import (
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/glycerine/rpc25519/selfcert"
)

const (
	// DefaultKeyName is used for the host-side login key when Config.KeyName is empty.
	DefaultKeyName = "emunet"

	hostKeyFile        = "ssh_host_ed25519_key"
	userKeyFilePrefix  = "id_ed25519_"
	authorizedKeyFile  = "authorized_keys"
	sshDirMode         = 0o700
	authorizedKeysMode = 0o600
)

// Config describes the initramfs tree and host key location to prepare.
type Config struct {
	// RootDir is the unpacked initramfs root, such as xendor/alpine-minirootfs-... .
	RootDir string

	// InitramfsPath is the cpio.gz archive to write after keys are refreshed.
	InitramfsPath string

	// KeyName names the host-side login key. Empty defaults to DefaultKeyName.
	KeyName string

	// UserKeyDir is where id_ed25519_${KeyName} is written. Empty defaults to $HOME/.ssh.
	UserKeyDir string

	// HomeDir overrides $HOME for default UserKeyDir selection. It is mostly useful in tests.
	HomeDir string

	// RepackOnly true means we change no keys
	RepackOnly bool
}

// Result reports the files written by Prepare.
type Result struct {
	KeyName string

	HostPrivateKeyPath string
	HostPublicKeyPath  string

	UserPrivateKeyPath string
	UserPublicKeyPath  string

	AuthorizedKeysPath string
	InitramfsPath      string
}

// Prepare generates fresh Ed25519 SSH keys, installs the public login key in the
// guest root account, and repacks the initramfs as a deterministic newc cpio.gz.
func Prepare(cfg Config) (r Result, err error) {
	cfg, err = cfg.withDefaults()
	if err != nil {
		return
	}
	r.KeyName = cfg.KeyName
	r.InitramfsPath = cfg.InitramfsPath

	if !cfg.RepackOnly {
		hostPriv, err := freshPrivateKey()
		if err != nil {
			return Result{}, fmt.Errorf("generate guest ssh host key: %w", err)
		}
		hostDir := filepath.Join(cfg.RootDir, "etc", "ssh")
		if err := selfcert.PrivateToSSHKeyPair(hostPriv, cfg.KeyName+"-host", hostKeyFile, hostDir); err != nil {
			return Result{}, fmt.Errorf("write guest ssh host key: %w", err)
		}
		r.HostPrivateKeyPath = filepath.Join(hostDir, hostKeyFile)
		r.HostPublicKeyPath = filepath.Join(hostDir, hostKeyFile+".pub")

		userPriv, err := freshPrivateKey()
		if err != nil {
			return Result{}, fmt.Errorf("generate host user ssh key: %w", err)
		}
		userKeyFile := userPrivateKeyFile(cfg.KeyName)
		if err := selfcert.PrivateToSSHKeyPair(userPriv, cfg.KeyName, userKeyFile, cfg.UserKeyDir); err != nil {
			return Result{}, fmt.Errorf("write host user ssh key: %w", err)
		}

		userPublicPath := filepath.Join(cfg.UserKeyDir, userKeyFile+".pub")
		userPublicKey, err := os.ReadFile(userPublicPath)
		if err != nil {
			return Result{}, fmt.Errorf("read host user ssh public key %q: %w", userPublicPath, err)
		}

		r.UserPrivateKeyPath = filepath.Join(cfg.UserKeyDir, userKeyFile)
		r.UserPublicKeyPath = userPublicPath

		authorizedKeysPath, err := writeAuthorizedKeys(cfg.RootDir, userPublicKey)
		if err != nil {
			return Result{}, err
		}
		r.AuthorizedKeysPath = authorizedKeysPath

	} // end if !cfg.RepackOnly

	if err := RepackInitramfs(cfg.RootDir, cfg.InitramfsPath); err != nil {
		return Result{}, err
	}
	return
}

// RepackInitramfs writes rootDir as a gzip-compressed newc cpio archive. It
// preserves modes, symlinks, device metadata, and mtimes, while forcing uid/gid
// to root and using deterministic archive ordering and gzip metadata.
func RepackInitramfs(rootDir, initramfsPath string) error {
	if rootDir == "" {
		return fmt.Errorf("RootDir cannot be empty")
	}
	if initramfsPath == "" {
		return fmt.Errorf("InitramfsPath cannot be empty")
	}

	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("resolve RootDir %q: %w", rootDir, err)
	}
	outAbs, err := filepath.Abs(initramfsPath)
	if err != nil {
		return fmt.Errorf("resolve InitramfsPath %q: %w", initramfsPath, err)
	}
	if insideTree(rootAbs, outAbs) {
		return fmt.Errorf("InitramfsPath %q must be outside RootDir %q", initramfsPath, rootDir)
	}

	rootInfo, err := os.Stat(rootAbs)
	if err != nil {
		return fmt.Errorf("stat RootDir %q: %w", rootDir, err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("RootDir %q is not a directory", rootDir)
	}

	records, err := cpioRecords(rootAbs)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return fmt.Errorf("create initramfs output directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(outAbs), "."+filepath.Base(outAbs)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary initramfs: %w", err)
	}
	tmpName := tmp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tmpName)
		}
	}()

	gw, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("create gzip writer: %w", err)
	}
	gw.ModTime = time.Unix(0, 0).UTC()
	for _, rec := range records {
		if err := writeNewcRecord(gw, rec); err != nil {
			_ = gw.Close()
			_ = tmp.Close()
			return fmt.Errorf("write cpio record %q: %w", rec.Name, err)
		}
	}
	if err := writeNewcTrailer(gw); err != nil {
		_ = gw.Close()
		_ = tmp.Close()
		return fmt.Errorf("write cpio trailer: %w", err)
	}
	if err := gw.Close(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("finish gzip stream: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary initramfs: %w", err)
	}

	if runtime.GOOS == "windows" {
		_ = os.Remove(outAbs)
	}
	if err := os.Rename(tmpName, outAbs); err != nil {
		return fmt.Errorf("replace initramfs %q: %w", initramfsPath, err)
	}
	keepTemp = true
	return nil
}

func (cfg Config) withDefaults() (Config, error) {
	if cfg.RootDir == "" {
		return Config{}, fmt.Errorf("RootDir cannot be empty")
	}
	if cfg.InitramfsPath == "" {
		return Config{}, fmt.Errorf("InitramfsPath cannot be empty")
	}
	if cfg.KeyName == "" {
		cfg.KeyName = DefaultKeyName
	}
	if strings.ContainsAny(cfg.KeyName, `/\`) {
		return Config{}, fmt.Errorf("KeyName cannot contain path separators: %q", cfg.KeyName)
	}
	if cfg.UserKeyDir == "" {
		home := cfg.HomeDir
		if home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return Config{}, fmt.Errorf("find user home directory: %w", err)
			}
		}
		if home == "" {
			return Config{}, fmt.Errorf("HomeDir cannot be empty when UserKeyDir is empty")
		}
		cfg.UserKeyDir = filepath.Join(home, ".ssh")
	}
	return cfg, nil
}

func freshPrivateKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

func userPrivateKeyFile(keyName string) string {
	return userKeyFilePrefix + keyName
}

func writeAuthorizedKeys(rootDir string, publicKey []byte) (string, error) {
	sshDir := filepath.Join(rootDir, "root", ".ssh")
	if err := os.MkdirAll(sshDir, sshDirMode); err != nil {
		return "", fmt.Errorf("create guest root ssh directory: %w", err)
	}
	if err := os.Chmod(sshDir, sshDirMode); err != nil {
		return "", fmt.Errorf("chmod guest root ssh directory: %w", err)
	}

	path := filepath.Join(sshDir, authorizedKeyFile)
	if err := os.WriteFile(path, publicKey, authorizedKeysMode); err != nil {
		return "", fmt.Errorf("write guest authorized_keys: %w", err)
	}
	if err := os.Chmod(path, authorizedKeysMode); err != nil {
		return "", fmt.Errorf("chmod guest authorized_keys: %w", err)
	}
	return path, nil
}

func archiveName(rootDir, path string) (string, error) {
	rel, err := filepath.Rel(rootDir, path)
	if err != nil {
		return "", fmt.Errorf("make archive path for %q: %w", path, err)
	}
	return filepath.ToSlash(rel), nil
}

func insideTree(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
