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
	elfDataLE    = 1    // little-endian
	elfTypeExec  = 2    // ET_EXEC
	elfMachRISCV = 0xF3
	ptLoad       = 1    // PT_LOAD
	shtSymtab    = 2    // SHT_SYMTAB
	shtStrtab    = 3    // SHT_STRTAB
	shtProgbits  = 1    // SHT_PROGBITS

	// PT_LOAD permission flags (p_flags).
	pfX = 0x1 // PF_X (executable)
	pfW = 0x2 // PF_W (writable)
	pfR = 0x4 // PF_R (readable)
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

// ELF64 section header (64 bytes).
type elf64Shdr struct {
	Name      uint32
	Type      uint32
	Flags     uint64
	Addr      uint64
	Offset    uint64
	Size      uint64
	Link      uint32
	Info      uint32
	AddrAlign uint64
	EntSize   uint64
}

// ELF64 symbol table entry (24 bytes).
type elf64Sym struct {
	Name  uint32
	Info  uint8
	Other uint8
	Shndx uint16
	Value uint64
	Size  uint64
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

		// Record executable extents so the JIT can auto-install AOT on
		// first RunJIT. isJIT=true for RW+X (self-modifying code), so
		// the segment-level flag propagates correctly — matches what
		// InstallAOT(mem, elfBytes) does for itself.
		if ph.Flags&pfX != 0 {
			mem.AddExecRegion(ph.VAddr, ph.VAddr+ph.MemSz, ph.Flags&pfW != 0)
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

// FindSymbolAddr parses the ELF symbol table in data and returns the
// address (st_value) of the named symbol. Returns (0, false) if the
// symbol is not found or the binary has no symbol table.
func FindSymbolAddr(data []byte, name string) (uint64, bool) {
	le := binary.LittleEndian

	// Validate ELF header minimally.
	if len(data) < 64 {
		return 0, false
	}
	if string(data[:4]) != elfMagic || data[4] != elfClass64 {
		return 0, false
	}

	shOff := le.Uint64(data[40:])     // e_shoff
	shEntSize := le.Uint16(data[58:]) // e_shentsize
	shNum := le.Uint16(data[60:])     // e_shnum

	if shOff == 0 || shNum == 0 || shEntSize < 64 {
		return 0, false
	}

	// Find SHT_SYMTAB section.
	var symtab elf64Shdr
	found := false
	for i := 0; i < int(shNum); i++ {
		off := int(shOff) + i*int(shEntSize)
		if off+64 > len(data) {
			return 0, false
		}
		if err := binary.Read(&byteReader{data: data[off:]}, le, &symtab); err != nil {
			return 0, false
		}
		if symtab.Type == shtSymtab {
			found = true
			break
		}
	}
	if !found {
		return 0, false
	}

	// Read the associated string table (sh_link points to STRTAB index).
	strtabOff := int(shOff) + int(symtab.Link)*int(shEntSize)
	if strtabOff+64 > len(data) {
		return 0, false
	}
	var strtab elf64Shdr
	if err := binary.Read(&byteReader{data: data[strtabOff:]}, le, &strtab); err != nil {
		return 0, false
	}
	strData := data[strtab.Offset:]
	if uint64(len(data)) < strtab.Offset+strtab.Size {
		return 0, false
	}
	strData = strData[:strtab.Size]

	// Iterate symbol entries.
	entSize := int(symtab.EntSize)
	if entSize < 24 {
		entSize = 24 // Elf64_Sym is 24 bytes
	}
	nSyms := int(symtab.Size) / entSize
	for i := 0; i < nSyms; i++ {
		off := int(symtab.Offset) + i*entSize
		if off+24 > len(data) {
			break
		}
		var sym elf64Sym
		if err := binary.Read(&byteReader{data: data[off:]}, le, &sym); err != nil {
			break
		}
		// Look up name in string table.
		nameOff := int(sym.Name)
		if nameOff >= len(strData) {
			continue
		}
		// Find null terminator.
		end := nameOff
		for end < len(strData) && strData[end] != 0 {
			end++
		}
		symName := string(strData[nameOff:end])
		if symName == name {
			return sym.Value, true
		}
	}
	return 0, false
}

// FindTextSection parses the ELF section headers in data and returns
// the (virtual address, size) of the `.text` section. Returns false if
// the ELF lacks a section header table or no `.text` section is found.
//
// Used by the AOT translator to establish the initial
// DecodedExecuteSegment's guest-VA range. Does not depend on any state
// established by LoadELFBytes — caller can invoke this independently.
func FindTextSection(data []byte) (vaddr, size uint64, ok bool) {
	le := binary.LittleEndian

	if len(data) < 64 {
		return 0, 0, false
	}
	if string(data[:4]) != elfMagic || data[4] != elfClass64 {
		return 0, 0, false
	}

	shOff := le.Uint64(data[40:])     // e_shoff
	shEntSize := le.Uint16(data[58:]) // e_shentsize
	shNum := le.Uint16(data[60:])     // e_shnum
	shStrNdx := le.Uint16(data[62:])  // e_shstrndx (section name string table)

	if shOff == 0 || shNum == 0 || shEntSize < 64 || shStrNdx >= shNum {
		return 0, 0, false
	}

	// Load the section name string table header.
	shstrOff := int(shOff) + int(shStrNdx)*int(shEntSize)
	if shstrOff+64 > len(data) {
		return 0, 0, false
	}
	var shstr elf64Shdr
	if err := binary.Read(&byteReader{data: data[shstrOff:]}, le, &shstr); err != nil {
		return 0, 0, false
	}
	if uint64(len(data)) < shstr.Offset+shstr.Size {
		return 0, 0, false
	}
	names := data[shstr.Offset : shstr.Offset+shstr.Size]

	// Walk section headers looking for a SHT_PROGBITS named ".text".
	for i := 0; i < int(shNum); i++ {
		off := int(shOff) + i*int(shEntSize)
		if off+64 > len(data) {
			return 0, 0, false
		}
		var sh elf64Shdr
		if err := binary.Read(&byteReader{data: data[off:]}, le, &sh); err != nil {
			return 0, 0, false
		}
		if sh.Type != shtProgbits {
			continue
		}
		// Resolve section name from the name table.
		nameOff := int(sh.Name)
		if nameOff >= len(names) {
			continue
		}
		end := nameOff
		for end < len(names) && names[end] != 0 {
			end++
		}
		if string(names[nameOff:end]) == ".text" {
			return sh.Addr, sh.Size, true
		}
	}
	return 0, 0, false
}

// ExecLoad describes one PT_LOAD segment whose PF_X flag is set — i.e.,
// a guest-VA range that the loader expects to be executable.
//
// Phase 2b of the JIT uses this to decide how many DecodedExecuteSegments
// to build at InstallAOT time: one per ExecLoad. Writable==true denotes
// a rare RW-X PT_LOAD (treated as isLikelyJIT from the start).
type ExecLoad struct {
	VAddr    uint64
	MemSz    uint64
	Writable bool
}

// FindExecLoads parses the ELF program-header table in data and returns
// every PT_LOAD entry with PF_X set. Returns (nil, false) if data is not
// a recognizable ELF64 or has no program headers. Returns ([], true) if
// the ELF is valid but has no executable loads (unusual but well-formed).
//
// Does not depend on state established by LoadELFBytes.
func FindExecLoads(data []byte) ([]ExecLoad, bool) {
	le := binary.LittleEndian

	if len(data) < 64 {
		return nil, false
	}
	if string(data[:4]) != elfMagic || data[4] != elfClass64 {
		return nil, false
	}

	phOff := le.Uint64(data[32:])     // e_phoff
	phEntSize := le.Uint16(data[54:]) // e_phentsize
	phNum := le.Uint16(data[56:])     // e_phnum

	if phOff == 0 || phNum == 0 || phEntSize < 56 {
		return nil, false
	}

	var out []ExecLoad
	for i := 0; i < int(phNum); i++ {
		off := int(phOff) + i*int(phEntSize)
		if off+56 > len(data) {
			return nil, false
		}
		var ph elf64Phdr
		if err := binary.Read(&byteReader{data: data[off:]}, le, &ph); err != nil {
			return nil, false
		}
		if ph.Type != ptLoad {
			continue
		}
		if ph.Flags&pfX == 0 {
			continue
		}
		if ph.MemSz == 0 {
			continue
		}
		out = append(out, ExecLoad{
			VAddr:    ph.VAddr,
			MemSz:    ph.MemSz,
			Writable: ph.Flags&pfW != 0,
		})
	}
	return out, true
}

// BuildELF constructs a minimal RV64 executable ELF with one PT_LOAD
// segment at codeVA containing the given 32-bit instruction words.
// Useful for constructing synthetic test programs without an assembler.
//
// The ELF includes a minimal section header table (SHT_NULL + SHT_STRTAB)
// so that consumers such as libriscv that call section_by_name() during
// ELF loading do not reject the binary for having e_shnum == 0.
func BuildELF(codeVA uint64, code []uint32) []byte {
	// File layout:
	//   [  0..63] ELF header (64 bytes)
	//   [ 64..119] Program header — PT_LOAD (56 bytes)
	//   [120..120+codeLen-1] Instruction words
	//   [120+codeLen] strtab: "\0" padded to 8 bytes
	//   [128+codeLen] Section header [0]: SHT_NULL (64 bytes)
	//   [192+codeLen] Section header [1]: SHT_STRTAB (64 bytes)
	const (
		codeOff    = 64 + 56 // 120
		strtabData = "\x00"  // one null byte — all section names are empty string
		strtabPad  = 8       // padded to 8-byte alignment
		shdrSize   = 64      // ELF64 section header size
		shdrCount  = 2       // SHT_NULL + SHT_STRTAB
	)

	codeBytes := make([]byte, len(code)*4)
	for i, insn := range code {
		binary.LittleEndian.PutUint32(codeBytes[i*4:], insn)
	}

	strtabOff := uint64(codeOff + len(codeBytes))
	shOff      := strtabOff + strtabPad
	total      := int(shOff) + shdrCount*shdrSize

	buf := make([]byte, total)
	le  := binary.LittleEndian

	// ── ELF header ───────────────────────────────────────────────────────
	copy(buf[0:], "\x7fELF")
	buf[4], buf[5], buf[6] = 2, 1, 1              // ELFCLASS64, ELFDATA2LSB, EV_CURRENT
	le.PutUint16(buf[16:], 2)                      // ET_EXEC
	le.PutUint16(buf[18:], 0xF3)                   // EM_RISCV
	le.PutUint32(buf[20:], 1)                      // e_version
	le.PutUint64(buf[24:], codeVA)                 // e_entry
	le.PutUint64(buf[32:], 64)                     // e_phoff
	le.PutUint64(buf[40:], shOff)                  // e_shoff
	le.PutUint16(buf[52:], 64)                     // e_ehsize
	le.PutUint16(buf[54:], 56)                     // e_phentsize
	le.PutUint16(buf[56:], 1)                      // e_phnum
	le.PutUint16(buf[58:], shdrSize)               // e_shentsize
	le.PutUint16(buf[60:], shdrCount)              // e_shnum
	le.PutUint16(buf[62:], 1)                      // e_shstrndx = section [1]

	// ── Program header ────────────────────────────────────────────────────
	ph := buf[64:]
	le.PutUint32(ph[0:], 1)                        // PT_LOAD
	le.PutUint32(ph[4:], 5)                        // PF_R|PF_X
	le.PutUint64(ph[8:], uint64(codeOff))          // p_offset
	le.PutUint64(ph[16:], codeVA)                  // p_vaddr
	le.PutUint64(ph[24:], codeVA)                  // p_paddr
	le.PutUint64(ph[32:], uint64(len(codeBytes)))  // p_filesz
	le.PutUint64(ph[40:], uint64(len(codeBytes)))  // p_memsz
	le.PutUint64(ph[48:], 0x10)                    // p_align

	// ── Code ──────────────────────────────────────────────────────────────
	copy(buf[codeOff:], codeBytes)

	// ── Strtab: just a single null byte (all names are "") ────────────────
	buf[strtabOff] = 0x00

	// ── Section header [0]: SHT_NULL ─────────────────────────────────────
	// All zeros — required first entry in every ELF section table.

	// ── Section header [1]: SHT_STRTAB (.shstrtab) ───────────────────────
	sh1 := buf[int(shOff)+shdrSize:]
	// sh_name=0 (points to "\0" in strtab — empty string, valid)
	le.PutUint32(sh1[4:], 3)                       // sh_type = SHT_STRTAB
	le.PutUint64(sh1[24:], strtabOff)              // sh_offset
	le.PutUint64(sh1[32:], 1)                      // sh_size = 1 byte ("\0")
	le.PutUint64(sh1[48:], 1)                      // sh_addralign

	return buf
}
