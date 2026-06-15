package riscv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJea9LinuxTimeZoneFilesFollowsGoRuntimePaths(t *testing.T) {
	hostZoneRoot := filepath.Join(t.TempDir(), "zoneinfo")
	writeTimeZoneTestFile(t, filepath.Join(hostZoneRoot, "America", "Los_Angeles"), []byte("la-zone"))
	writeTimeZoneTestFile(t, filepath.Join(hostZoneRoot, "Europe", "London"), []byte("london-zone"))

	hostEtcRoot := filepath.Join(t.TempDir(), "etc-zoneinfo")
	writeTimeZoneTestFile(t, filepath.Join(hostEtcRoot, "America", "New_York"), []byte("ny-zone"))

	hostGoRoot := t.TempDir()
	hostZip := filepath.Join(hostGoRoot, "lib", "time", "zoneinfo.zip")
	writeTimeZoneTestFile(t, hostZip, []byte("zip-zoneinfo"))

	files := loadJea9LinuxTimeZoneFiles([]jea9LinuxZoneInfoSource{
		{guestPath: "/usr/share/zoneinfo/", hostPath: hostZoneRoot},
		{guestPath: "/etc/zoneinfo", hostPath: hostEtcRoot},
		{guestPath: "/usr/local/go/lib/time/zoneinfo.zip", hostPath: hostZip},
	})

	if got := string(files["/usr/share/zoneinfo//America/Los_Angeles"]); got != "la-zone" {
		t.Fatalf("Los_Angeles zone = %q, want la-zone", got)
	}
	if got := string(files["/usr/share/zoneinfo//Europe/London"]); got != "london-zone" {
		t.Fatalf("London zone = %q, want london-zone", got)
	}
	if got := string(files["/etc/zoneinfo/America/New_York"]); got != "ny-zone" {
		t.Fatalf("New_York zone = %q, want ny-zone", got)
	}
	if got := string(files["/usr/local/go/lib/time/zoneinfo.zip"]); got != "zip-zoneinfo" {
		t.Fatalf("zoneinfo.zip = %q, want zip-zoneinfo", got)
	}
}

func TestLoadJea9LinuxTimeZoneFilesFollowsSymlinkedSourceRoot(t *testing.T) {
	hostZoneRoot := filepath.Join(t.TempDir(), "real-zoneinfo")
	writeTimeZoneTestFile(t, filepath.Join(hostZoneRoot, "America", "Los_Angeles"), []byte("la-zone"))
	linkRoot := filepath.Join(t.TempDir(), "zoneinfo-link")
	if err := os.Symlink(hostZoneRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	files := loadJea9LinuxTimeZoneFiles([]jea9LinuxZoneInfoSource{
		{guestPath: "/usr/share/zoneinfo/", hostPath: linkRoot},
	})
	if got := string(files["/usr/share/zoneinfo//America/Los_Angeles"]); got != "la-zone" {
		t.Fatalf("symlinked Los_Angeles zone = %q, want la-zone", got)
	}
}

func TestJea9Linux_DefaultTimeZoneFileOpenable(t *testing.T) {
	files := jea9LinuxTimeZoneFiles()
	if len(files) == 0 {
		t.Skip("host has no discoverable zoneinfo files")
	}
	path := ""
	for candidate := range files {
		path = candidate
		if candidate == "/usr/share/zoneinfo//America/Los_Angeles" ||
			candidate == "/usr/local/go/lib/time/zoneinfo.zip" {
			break
		}
	}

	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	writeGuestCString(t, mem, 0x5000, path)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysOpenat, jea9TestATFDCWD, 0x5000, 0, 0); d != NoteHandled {
		t.Fatalf("openat(%q) disposition = %v", path, d)
	}
	if fd := cpu.Reg(10); fd < 3 {
		t.Fatalf("openat(%q) fd = %d, want >= 3", path, fd)
	}
}

func writeTimeZoneTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
