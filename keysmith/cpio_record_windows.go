//go:build windows

package keysmith

import (
	"fmt"
	"os"
	"strings"
)

type cpioRecorder struct {
	inumber uint64
}

func newCPIORecorder() *cpioRecorder {
	return &cpioRecorder{inumber: 2}
}

func (r *cpioRecorder) record(path, name string) (cpioRecord, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return cpioRecord{}, err
	}
	mtime := fi.ModTime().Unix()
	if mtime < 0 {
		return cpioRecord{}, fmt.Errorf("mtime before Unix epoch: %v", fi.ModTime())
	}
	info := cpioInfo{
		Ino:      r.inumber,
		Mode:     windowsCPIOMode(name, fi.Mode()),
		NLink:    1,
		MTime:    uint64(mtime),
		FileSize: uint64(fi.Size()),
		Name:     name,
	}
	r.inumber++

	if target, ok := windowsLinkTarget(path); ok {
		info.Mode = cpioSIFLNK | 0o777
		return cpioRecord{
			cpioInfo: info,
			data:     []byte(target),
		}, nil
	}

	rec := cpioRecord{cpioInfo: info}
	switch info.Mode & cpioSIFMT {
	case cpioSIFREG:
		rec.sourcePath = path
	case cpioSIFDIR:
		rec.FileSize = 0
	case cpioSIFCHR, cpioSIFBLK, cpioSIFIFO, cpioSIFSOCK:
		rec.FileSize = 0
	default:
		return cpioRecord{}, fmt.Errorf("unsupported file type mode %#o from host mode %v size %d", info.Mode&cpioSIFMT, fi.Mode(), fi.Size())
	}
	return rec, nil
}

func windowsLinkTarget(path string) (string, bool) {
	if target, err := os.Readlink(path); err == nil {
		return target, true
	}
	if target, ok := windowsReparseLinkTarget(path); ok {
		return target, true
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return cygwinSymlinkTarget(raw)
}

func windowsCPIOMode(name string, mode os.FileMode) uint64 {
	typ := cpioModeFromFileMode(mode) & cpioSIFMT
	switch typ {
	case cpioSIFDIR:
		perm := uint64(0o755)
		if name == "root/.ssh" {
			perm = 0o700
		}
		return typ | perm
	case cpioSIFREG:
		perm := uint64(0o755)
		switch {
		case name == "root/.ssh/authorized_keys":
			perm = 0o600
		case strings.HasPrefix(name, "etc/ssh/ssh_host_") && !strings.HasSuffix(name, ".pub"):
			perm = 0o600
		case strings.HasPrefix(name, "etc/ssh/ssh_host_") && strings.HasSuffix(name, ".pub"):
			perm = 0o644
		}
		return typ | perm
	default:
		return cpioModeFromFileMode(mode)
	}
}
