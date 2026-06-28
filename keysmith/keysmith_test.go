package keysmith

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"unicode/utf16"

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
		if err := os.Symlink("/guest/absolute-target", filepath.Join(root, "absolute-link")); err != nil {
			t.Fatalf("absolute Symlink failed: %v", err)
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
		if got := linkRec.Mode & cpioSIFMT; got != cpioSIFLNK {
			t.Fatalf("symlink archive mode type = %#o; want symlink", got)
		}
		if got := string(recordBytes(t, linkRec)); got != "binfile" {
			t.Fatalf("symlink target = %q; want binfile", got)
		}
		absoluteLinkRec := requireRecord(t, records, "absolute-link")
		if got := absoluteLinkRec.Mode & cpioSIFMT; got != cpioSIFLNK {
			t.Fatalf("absolute symlink archive mode type = %#o; want symlink", got)
		}
		if got := string(recordBytes(t, absoluteLinkRec)); got != "/guest/absolute-target" {
			t.Fatalf("absolute symlink target = %q; want /guest/absolute-target", got)
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

func TestCygwinSymlinkTarget(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "utf8",
			raw:  append(append([]byte(nil), cygwinSymlinkMagic...), []byte("/bin/busybox\x00")...),
			want: "/bin/busybox",
		},
		{
			name: "utf16le",
			raw:  append(append([]byte(nil), cygwinSymlinkMagic...), append([]byte{0xff, 0xfe}, utf16LEBytes("/bin/busybox")...)...),
			want: "/bin/busybox",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cygwinSymlinkTarget(tt.raw)
			if !ok {
				t.Fatalf("cygwinSymlinkTarget returned ok=false")
			}
			if got != tt.want {
				t.Fatalf("cygwinSymlinkTarget = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestReparseDataLinkTargetSkipsBinaryPrefix(t *testing.T) {
	raw := append([]byte{0, 0, 0, 0}, []byte("/bin/busybox")...)
	got, ok := reparseDataLinkTarget(raw)
	if !ok {
		t.Fatalf("reparseDataLinkTarget returned ok=false")
	}
	if got != "/bin/busybox" {
		t.Fatalf("reparseDataLinkTarget = %q; want /bin/busybox", got)
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

func readArchive(t *testing.T, path string) map[string]cpioRecord {
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

	records := make(map[string]cpioRecord)
	for off := 0; ; {
		rec, next, ok := readNewcRecord(t, raw, off)
		if !ok {
			break
		}
		if rec.Name == cpioTrailer {
			break
		}
		records[rec.Name] = rec
		off = next
	}
	return records
}

func readNewcRecord(t *testing.T, raw []byte, off int) (cpioRecord, int, bool) {
	t.Helper()
	if off == len(raw) {
		return cpioRecord{}, off, false
	}
	if off+110 > len(raw) {
		t.Fatalf("truncated cpio header at offset %d", off)
	}
	if magic := string(raw[off : off+6]); magic != cpioNewcMagic {
		t.Fatalf("cpio magic at offset %d = %q; want %q", off, magic, cpioNewcMagic)
	}
	var fields [13]uint64
	pos := off + 6
	for i := range fields {
		v, err := strconv.ParseUint(string(raw[pos:pos+8]), 16, 64)
		if err != nil {
			t.Fatalf("parse cpio field %d at offset %d: %v", i, pos, err)
		}
		fields[i] = v
		pos += 8
	}
	nameLen := int(fields[11])
	if nameLen <= 0 {
		t.Fatalf("cpio record at offset %d has invalid name length %d", off, nameLen)
	}
	nameStart := off + 110
	nameEnd := nameStart + nameLen
	if nameEnd > len(raw) {
		t.Fatalf("cpio record at offset %d has truncated name", off)
	}
	if raw[nameEnd-1] != 0 {
		t.Fatalf("cpio record at offset %d name is not NUL-terminated", off)
	}
	dataStart := roundUp4(nameEnd)
	fileSize := int(fields[6])
	dataEnd := dataStart + fileSize
	if dataEnd > len(raw) {
		t.Fatalf("cpio record at offset %d has truncated data", off)
	}
	rec := cpioRecord{
		cpioInfo: cpioInfo{
			Ino:      fields[0],
			Mode:     fields[1],
			UID:      fields[2],
			GID:      fields[3],
			NLink:    fields[4],
			MTime:    fields[5],
			FileSize: fields[6],
			Major:    fields[7],
			Minor:    fields[8],
			Rmajor:   fields[9],
			Rminor:   fields[10],
			Name:     string(raw[nameStart : nameEnd-1]),
		},
		data: append([]byte(nil), raw[dataStart:dataEnd]...),
	}
	return rec, roundUp4(dataEnd), true
}

func roundUp4(n int) int {
	return (n + 3) &^ 3
}

func requireRecord(t *testing.T, records map[string]cpioRecord, name string) cpioRecord {
	t.Helper()
	rec, ok := records[name]
	if !ok {
		t.Fatalf("archive missing record %q", name)
	}
	return rec
}

func recordBytes(t *testing.T, rec cpioRecord) []byte {
	t.Helper()
	return rec.data
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

func utf16LEBytes(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(u16)*2+2)
	for _, v := range u16 {
		out = append(out, byte(v), byte(v>>8))
	}
	return append(out, 0, 0)
}
