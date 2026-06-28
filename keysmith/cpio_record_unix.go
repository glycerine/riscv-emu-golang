//go:build !windows

package keysmith

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type cpioRecorder struct {
	inodeMap map[devInode]cpioInfo
	inumber  uint64
}

type devInode struct {
	dev uint64
	ino uint64
}

func newCPIORecorder() *cpioRecorder {
	return &cpioRecorder{
		inodeMap: make(map[devInode]cpioInfo),
		inumber:  2,
	}
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
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return cpioRecord{}, fmt.Errorf("file info has no Stat_t")
	}
	info := cpioInfo{
		Ino:      uint64(sys.Ino),
		Mode:     uint64(sys.Mode),
		UID:      uint64(sys.Uid),
		GID:      uint64(sys.Gid),
		NLink:    uint64(sys.Nlink),
		MTime:    uint64(mtime),
		FileSize: uint64(fi.Size()),
		Dev:      uint64(sys.Dev),
		Major:    uint64(unix.Major(uint64(sys.Dev))),
		Minor:    uint64(unix.Minor(uint64(sys.Dev))),
		Rmajor:   uint64(unix.Major(uint64(sys.Rdev))),
		Rminor:   uint64(unix.Minor(uint64(sys.Rdev))),
		Name:     name,
	}
	info, seenHardlink := r.inode(info)

	rec := cpioRecord{cpioInfo: info}
	switch info.Mode & cpioSIFMT {
	case cpioSIFREG:
		if !seenHardlink {
			rec.sourcePath = path
		}
	case cpioSIFLNK:
		target, err := os.Readlink(path)
		if err != nil {
			return cpioRecord{}, err
		}
		rec.data = []byte(target)
	case cpioSIFDIR, cpioSIFCHR, cpioSIFBLK, cpioSIFIFO, cpioSIFSOCK:
		rec.FileSize = 0
	default:
		return cpioRecord{}, fmt.Errorf("unsupported file type mode %#o", info.Mode&cpioSIFMT)
	}
	if rec.sourcePath == "" && rec.data == nil {
		rec.FileSize = 0
	}
	return rec, nil
}

func (r *cpioRecorder) inode(info cpioInfo) (cpioInfo, bool) {
	key := devInode{dev: info.Dev, ino: info.Ino}
	info.Dev = 0
	if prev, ok := r.inodeMap[key]; ok {
		info.Ino = prev.Ino
		return info, prev.Name != info.Name
	}

	info.Ino = r.inumber
	r.inumber++
	r.inodeMap[key] = info
	return info, false
}
