package keysmith

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/u-root/u-root/pkg/cpio"
	"golang.org/x/crypto/ssh"
)

func TestPrepareWritesFreshKeysAndArchive(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "rootfs")
	mustMkdir(t, filepath.Join(root, "etc", "ssh"), 0o755)
	mustMkdir(t, filepath.Join(root, "root", ".ssh"), 0o700)
	mustWriteFile(t, filepath.Join(root, "etc", "ssh", hostKeyFile), []byte("stale host key\n"), 0o600)
	mustWriteFile(t, filepath.Join(root, "root", ".ssh", authorizedKeyFile), []byte("stale login key\n"), 0o600)
	mustWriteFile(t, filepath.Join(root, "binfile"), []byte("hello\n"), 0o755)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("binfile", filepath.Join(root, "link-to-binfile")); err != nil {
			t.Fatalf("Symlink failed: %v", err)
		}
	}

	out := filepath.Join(base, "out", "initramfs.cpio.gz")
	res, err := Prepare(Config{
		RootDir:       root,
		InitramfsPath: out,
		KeyName:       "emunet-test",
		UserKeyDir:    filepath.Join(base, "home", ".ssh"),
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	if res.KeyName != "emunet-test" {
		t.Fatalf("Result KeyName = %q; want emunet-test", res.KeyName)
	}
	assertPrivatePublicPair(t, res.HostPrivateKeyPath, res.HostPublicKeyPath)
	assertPrivatePublicPair(t, res.UserPrivateKeyPath, res.UserPublicKeyPath)

	userPublic, err := os.ReadFile(res.UserPublicKeyPath)
	if err != nil {
		t.Fatalf("ReadFile(user public) failed: %v", err)
	}
	authorized, err := os.ReadFile(res.AuthorizedKeysPath)
	if err != nil {
		t.Fatalf("ReadFile(authorized_keys) failed: %v", err)
	}
	if !bytes.Equal(authorized, userPublic) {
		t.Fatalf("authorized_keys was not replaced with the fresh user public key")
	}
	assertMode(t, res.HostPrivateKeyPath, 0o600)
	assertMode(t, res.HostPublicKeyPath, 0o644)
	assertMode(t, res.UserPrivateKeyPath, 0o600)
	assertMode(t, res.UserPublicKeyPath, 0o644)
	assertMode(t, res.AuthorizedKeysPath, 0o600)

	records := readArchive(t, out)
	hostRec := requireRecord(t, records, "etc/ssh/"+hostKeyFile)
	if hostRec.UID != 0 || hostRec.GID != 0 {
		t.Fatalf("host key archive owner = %d:%d; want 0:0", hostRec.UID, hostRec.GID)
	}
	if got := hostRec.Mode & 0o777; got != 0o600 {
		t.Fatalf("host key archive mode = %o; want 600", got)
	}

	authRec := requireRecord(t, records, "root/.ssh/"+authorizedKeyFile)
	if got := recordBytes(t, authRec); !bytes.Equal(got, userPublic) {
		t.Fatalf("authorized_keys archive content mismatch")
	}

	if runtime.GOOS != "windows" {
		linkRec := requireRecord(t, records, "link-to-binfile")
		if got := linkRec.Mode & cpio.S_IFMT; got != cpio.S_IFLNK {
			t.Fatalf("symlink archive mode type = %#o; want symlink", got)
		}
		if got := string(recordBytes(t, linkRec)); got != "binfile" {
			t.Fatalf("symlink target = %q; want binfile", got)
		}
	}
}

func TestRepackInitramfsIsDeterministicForUnchangedTree(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "rootfs")
	mustMkdir(t, filepath.Join(root, "etc"), 0o755)
	mustWriteFile(t, filepath.Join(root, "etc", "issue"), []byte("hello\n"), 0o644)

	first := filepath.Join(base, "first.cpio.gz")
	second := filepath.Join(base, "second.cpio.gz")
	if err := RepackInitramfs(root, first); err != nil {
		t.Fatalf("first RepackInitramfs failed: %v", err)
	}
	if err := RepackInitramfs(root, second); err != nil {
		t.Fatalf("second RepackInitramfs failed: %v", err)
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("ReadFile(first) failed: %v", err)
	}
	secondBytes, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("ReadFile(second) failed: %v", err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("archives differ for unchanged tree")
	}
}

func TestPrepareDefaultsUserKeyPath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "rootfs")
	mustMkdir(t, filepath.Join(root, "etc", "ssh"), 0o755)

	res, err := Prepare(Config{
		RootDir:       root,
		InitramfsPath: filepath.Join(base, "initramfs.cpio.gz"),
		HomeDir:       filepath.Join(base, "home"),
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	wantPrivate := filepath.Join(base, "home", ".ssh", userKeyFilePrefix+DefaultKeyName)
	if res.UserPrivateKeyPath != wantPrivate {
		t.Fatalf("UserPrivateKeyPath = %q; want %q", res.UserPrivateKeyPath, wantPrivate)
	}
}

func TestRepackRejectsOutputInsideRoot(t *testing.T) {
	root := t.TempDir()
	err := RepackInitramfs(root, filepath.Join(root, "initramfs.cpio.gz"))
	if err == nil {
		t.Fatalf("expected error for output inside root")
	}
}

func assertPrivatePublicPair(t *testing.T, privatePath, publicPath string) {
	t.Helper()
	privateBytes, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", privatePath, err)
	}
	signer, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		t.Fatalf("ParsePrivateKey(%q) failed: %v", privatePath, err)
	}
	publicBytes, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", publicPath, err)
	}
	publicKey, _, _, rest, err := ssh.ParseAuthorizedKey(publicBytes)
	if err != nil {
		t.Fatalf("ParseAuthorizedKey(%q) failed: %v", publicPath, err)
	}
	if len(rest) != 0 {
		t.Fatalf("%q has trailing authorized key data: %q", publicPath, rest)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), publicKey.Marshal()) {
		t.Fatalf("%q public key does not match %q", publicPath, privatePath)
	}
}

func readArchive(t *testing.T, path string) map[string]cpio.Record {
	t.Helper()
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", path, err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("NewReader(%q) failed: %v", path, err)
	}
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("ReadAll gzip %q failed: %v", path, err)
	}

	records := make(map[string]cpio.Record)
	reader := cpio.Newc.Reader(bytes.NewReader(raw))
	if err := cpio.ForEachRecord(reader, func(rec cpio.Record) error {
		records[rec.Name] = rec
		return nil
	}); err != nil {
		t.Fatalf("read cpio records from %q failed: %v", path, err)
	}
	return records
}

func requireRecord(t *testing.T, records map[string]cpio.Record, name string) cpio.Record {
	t.Helper()
	rec, ok := records[name]
	if !ok {
		t.Fatalf("archive missing record %q", name)
	}
	return rec
}

func recordBytes(t *testing.T, rec cpio.Record) []byte {
	t.Helper()
	if rec.ReaderAt == nil {
		return nil
	}
	buf := make([]byte, rec.FileSize)
	n, err := rec.ReaderAt.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt(%q) failed: %v", rec.Name, err)
	}
	return buf[:n]
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) failed: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%q mode = %o; want %o", path, got, want)
	}
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatalf("MkdirAll(%q) failed: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(%q) failed: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) failed: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("WriteFile(%q) failed: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(%q) failed: %v", path, err)
	}
}
