package riscv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ELF64 constants we care about.
const (
	elfMagic     = "\x7fELF"
	elfClass64   = 2
	elfDataLE    = 1  // little-endian
	elfTypeExec  = 2  // ET_EXEC
	elfMachRISCV = 0xF3
	ptLoad       = 1  // PT_LOAD
)

// ELF64Header is the 64-byte ELF file header.
type elf64Header struct {
	Ident     [16]byte
	Type      uint16
	Machine   uint16
	Version   uint32
	Entry     uint64
	PhOff     uint64 // program header table file offset
	ShOff     uint64 // section header table file offset (unused)
	Flags     uint32
	EhSize    uint16
	PhEntSize uint16
	PhNum     uint16
	ShEntSize uint16
	ShNum     uint16
	ShStrNdx  uint16
}

// ELF64 program header (56 bytes).
type elf64Phdr struct {
	Type   uint32
	Flags  uint32
	Offset uint64 // file offset of segment data
	VAddr  uint64 // virtual address
	PAddr  uint64 // physical address (we use VAddr)
	FileSz uint64 // bytes in file
	MemSz  uint64 // bytes in memory (>= FileSz)
	Align  uint64
}

// LoadELF reads an ELF64 RISC-V executable from path, loads all PT_LOAD
// segments into mem, and returns the entry point address.
//
// Segments whose MemSz > FileSz have the extra bytes zero-filled (BSS).
// The memory image must fit within mem; no dynamic linking is performed.
func LoadELF(mem *GuestMemory, path string) (entry uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return loadELFReader(mem, f)
}

// LoadELFBytes loads an ELF64 RISC-V executable from an in-memory byte slice.
func LoadELFBytes(mem *GuestMemory, data []byte) (entry uint64, err error) {
	return loadELFReader(mem, &byteReader{data: data})
}

func loadELFReader(mem *GuestMemory, r io.ReadSeeker) (uint64, error) {
	le := binary.LittleEndian

	// ── read and validate ELF header ─────────────────────────────────────
	var hdr elf64Header
	if err := binary.Read(r, le, &hdr); err != nil {
		return 0, fmt.Errorf("elf: read header: %w", err)
	}
	if string(hdr.Ident[:4]) != elfMagic {
		return 0, errors.New("elf: not an ELF file")
	}
	if hdr.Ident[4] != elfClass64 {
		return 0, errors.New("elf: not a 64-bit ELF")
	}
	if hdr.Ident[5] != elfDataLE {
		return 0, errors.New("elf: not little-endian")
	}
	if hdr.Type != elfTypeExec {
		return 0, fmt.Errorf("elf: not an executable (type 0x%x)", hdr.Type)
	}
	if hdr.Machine != elfMachRISCV {
		return 0, fmt.Errorf("elf: not RISC-V (machine 0x%x)", hdr.Machine)
	}
	if hdr.PhEntSize != 56 {
		return 0, fmt.Errorf("elf: unexpected phentsize %d", hdr.PhEntSize)
	}
	if hdr.PhNum == 0 {
		return 0, errors.New("elf: no program headers")
	}

	// ── iterate program headers ───────────────────────────────────────────
	loaded := 0
	for i := range int(hdr.PhNum) {
		offset := int64(hdr.PhOff) + int64(i)*int64(hdr.PhEntSize)
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("elf: seek to phdr %d: %w", i, err)
		}
		var ph elf64Phdr
		if err := binary.Read(r, le, &ph); err != nil {
			return 0, fmt.Errorf("elf: read phdr %d: %w", i, err)
		}
		if ph.Type != ptLoad {
			continue // skip NOTE, GNU_STACK, RISCV_ATTRIBUTES, etc.
		}
		if ph.MemSz == 0 {
			continue
		}

		// Copy file bytes into guest memory
		if ph.FileSz > 0 {
			if _, err := r.Seek(int64(ph.Offset), io.SeekStart); err != nil {
				return 0, fmt.Errorf("elf: seek to segment %d data: %w", i, err)
			}
			buf := make([]byte, ph.FileSz)
			if _, err := io.ReadFull(r, buf); err != nil {
				return 0, fmt.Errorf("elf: read segment %d: %w", i, err)
			}
			if f := mem.WriteBytes(ph.VAddr, buf); f != nil {
				return 0, fmt.Errorf("elf: write segment %d to 0x%x: %v", i, ph.VAddr, f)
			}
		}

		// Zero-fill BSS (MemSz > FileSz)
		if ph.MemSz > ph.FileSz {
			bssStart := ph.VAddr + ph.FileSz
			bssSize  := ph.MemSz - ph.FileSz
			if f := mem.ZeroRange(bssStart, bssSize); f != nil {
				return 0, fmt.Errorf("elf: zero BSS for segment %d: %v", i, f)
			}
		}
		loaded++
	}

	if loaded == 0 {
		return 0, errors.New("elf: no PT_LOAD segments found")
	}
	return hdr.Entry, nil
}

// byteReader wraps a byte slice as an io.ReadSeeker.
type byteReader struct {
	data []byte
	pos  int64
}

func (b *byteReader) Read(p []byte) (int, error) {
	if b.pos >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += int64(n)
	return n, nil
}

func (b *byteReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:   abs = offset
	case io.SeekCurrent: abs = b.pos + offset
	case io.SeekEnd:     abs = int64(len(b.data)) + offset
	default:
		return 0, errors.New("byteReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("byteReader: negative position")
	}
	b.pos = abs
	return abs, nil
}
