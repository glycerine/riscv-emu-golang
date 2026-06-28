package keysmith

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	cpioNewcMagic = "070701"
	cpioTrailer   = "TRAILER!!!"

	cpioSIFIFO  = 0x1000
	cpioSIFCHR  = 0x2000
	cpioSIFDIR  = 0x4000
	cpioSIFBLK  = 0x6000
	cpioSIFREG  = 0x8000
	cpioSIFLNK  = 0xa000
	cpioSIFSOCK = 0xc000
	cpioSIFMT   = 0xf000

	cpioSISUID = 0x800
	cpioSISGID = 0x400
	cpioSISVTX = 0x200
)

type cpioInfo struct {
	Ino      uint64
	Mode     uint64
	UID      uint64
	GID      uint64
	NLink    uint64
	MTime    uint64
	FileSize uint64
	Dev      uint64
	Major    uint64
	Minor    uint64
	Rmajor   uint64
	Rminor   uint64
	Name     string
}

type cpioRecord struct {
	cpioInfo
	sourcePath string
	data       []byte
}

func cpioRecords(rootDir string) ([]cpioRecord, error) {
	recorder := newCPIORecorder()
	var records []cpioRecord
	err := filepath.WalkDir(rootDir, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name, err := archiveName(rootDir, path)
		if err != nil {
			return err
		}
		rec, err := recorder.record(path, name)
		if err != nil {
			return fmt.Errorf("record %q: %w", path, err)
		}
		rec.UID = 0
		rec.GID = 0
		rec.Dev = 0
		rec.Major = 0
		rec.Minor = 0
		records = append(records, rec)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk initramfs root %q: %w", rootDir, err)
	}
	return records, nil
}

func writeNewcTrailer(w io.Writer) error {
	return writeNewcRecord(w, cpioRecord{cpioInfo: cpioInfo{Name: cpioTrailer}})
}

func writeNewcRecord(w io.Writer, rec cpioRecord) error {
	info := rec.cpioInfo
	if rec.sourcePath == "" && rec.data == nil {
		info.FileSize = 0
	}
	if rec.data != nil {
		info.FileSize = uint64(len(rec.data))
	}

	if _, err := io.WriteString(w, cpioNewcMagic); err != nil {
		return err
	}
	fields := [...]uint64{
		info.Ino,
		info.Mode,
		info.UID,
		info.GID,
		info.NLink,
		info.MTime,
		info.FileSize,
		info.Major,
		info.Minor,
		info.Rmajor,
		info.Rminor,
		uint64(len(info.Name) + 1),
		0,
	}
	for _, field := range fields {
		if err := writeNewcHex(w, field); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, info.Name); err != nil {
		return err
	}
	if _, err := w.Write([]byte{0}); err != nil {
		return err
	}
	if err := writeNewcPad(w, 110+len(info.Name)+1); err != nil {
		return err
	}
	if rec.data != nil {
		if _, err := w.Write(rec.data); err != nil {
			return err
		}
		return writeNewcPad(w, len(rec.data))
	}
	if rec.sourcePath == "" {
		return nil
	}

	f, err := os.Open(rec.sourcePath)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if uint64(n) != info.FileSize {
		return fmt.Errorf("wrote %d bytes of file instead of %d bytes", n, info.FileSize)
	}
	return writeNewcPad(w, int(n))
}

func writeNewcHex(w io.Writer, v uint64) error {
	if v > 0xffffffff {
		return fmt.Errorf("newc field value %#x overflows 32 bits", v)
	}
	_, err := fmt.Fprintf(w, "%08X", v)
	return err
}

func writeNewcPad(w io.Writer, n int) error {
	pad := (4 - (n & 3)) & 3
	if pad == 0 {
		return nil
	}
	_, err := w.Write(make([]byte, pad))
	return err
}

func cpioModeFromFileMode(mode os.FileMode) uint64 {
	out := uint64(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		out |= cpioSISUID
	}
	if mode&os.ModeSetgid != 0 {
		out |= cpioSISGID
	}
	if mode&os.ModeSticky != 0 {
		out |= cpioSISVTX
	}
	switch {
	case mode&os.ModeSymlink != 0:
		out |= cpioSIFLNK
	case mode.IsDir():
		out |= cpioSIFDIR
	case mode.IsRegular():
		out |= cpioSIFREG
	case mode&os.ModeNamedPipe != 0:
		out |= cpioSIFIFO
	case mode&os.ModeSocket != 0:
		out |= cpioSIFSOCK
	case mode&os.ModeDevice != 0 && mode&os.ModeCharDevice != 0:
		out |= cpioSIFCHR
	case mode&os.ModeDevice != 0:
		out |= cpioSIFBLK
	}
	return out
}
