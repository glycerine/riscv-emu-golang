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
	elfDataLE    = 1 // little-endian
	elfTypeExec  = 2 // ET_EXEC
	elfTypeDyn   = 3 // ET_DYN (also static-pie)
	elfMachRISCV = 0xF3
	ptLoad       = 1 // PT_LOAD
	shtSymtab    = 2 // SHT_SYMTAB
	shtStrtab    = 3 // SHT_STRTAB
	shtProgbits  = 1 // SHT_PROGBITS

	// PT_LOAD permission flags (p_flags).
	pfX = 0x1 // PF_X (executable)
	pfW = 0x2 // PF_W (writable)
	pfR = 0x4 // PF_R (readable)
)

// staticPieBase is the load address applied to ET_DYN (static-pie) ELFs
// whose segment vaddrs are zero-based. Matches the standard RISC-V virt
// machine RAM start and OpenSBI's expected runtime address.
const staticPieBase = uint64(0x80000000)

// Elf64Header is the 64-byte ELF file header.
type Elf64Header struct {
	Ident     [16]byte
	Type      uint16
	Machine   uint16
	Version   uint32
	Entry     uint64
	PhOff     uint64 // program header table file offset
	ShOff     uint64 // section header table file offset
	Flags     uint32
	EhSize    uint16
	PhEntSize uint16
	PhNum     uint16
	ShEntSize uint16
	ShNum     uint16
	ShStrNdx  uint16
}

// Elf64Shdr is an ELF64 section header (64 bytes).
type Elf64Shdr struct {
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

// Elf64Sym is an ELF64 symbol table entry (24 bytes).
type Elf64Sym struct {
	Name  uint32
	Info  uint8
	Other uint8
	Shndx uint16
	Value uint64
	Size  uint64
}

// Elf64Phdr is an ELF64 program header (56 bytes).
type Elf64Phdr struct {
	Type   uint32
	Flags  uint32
	Offset uint64 // file offset of segment data
	VAddr  uint64 // virtual address
	PAddr  uint64 // physical address (we use VAddr)
	FileSz uint64 // bytes in file
	MemSz  uint64 // bytes in memory (>= FileSz)
	Align  uint64
}

// ELF holds a parsed ELF64 RISC-V executable.
type ELF struct {
	Entry      uint64       // copy of Header.Entry (always valid, 0 if no header)
	LoadBias   uint64       // runtime address added to ELF p_vaddr/symbol values
	TohostAddr uint64       // address of "tohost" symbol (0 if not found)
	Header     *Elf64Header // parsed file header
	Shdrs      []*Elf64Shdr // parsed section headers
	Data       []byte       // raw ELF bytes for symbol/string table access
}

// LoadELF reads an ELF64 RISC-V executable from path, loads all PT_LOAD
// segments into mem, and returns the parsed ELF.
func LoadELF(mem *GuestMemory, path string) (*ELF, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadELFBytes(mem, data)
}

// LoadELFBytes loads an ELF64 RISC-V executable from an in-memory byte slice.
func LoadELFBytes(mem *GuestMemory, data []byte) (*ELF, error) {
	return loadELFReader(mem, &byteReader{data: data}, data)
}

func loadELFReader(mem *GuestMemory, r io.ReadSeeker, data []byte) (*ELF, error) {
	le := binary.LittleEndian

	// ── read and validate ELF header ─────────────────────────────────────
	var hdr Elf64Header
	if err := binary.Read(r, le, &hdr); err != nil {
		return nil, fmt.Errorf("elf: read header: %w", err)
	}
	if string(hdr.Ident[:4]) != elfMagic {
		return nil, errors.New("elf: not an ELF file")
	}
	if hdr.Ident[4] != elfClass64 {
		return nil, errors.New("elf: not a 64-bit ELF")
	}
	if hdr.Ident[5] != elfDataLE {
		return nil, errors.New("elf: not little-endian")
	}
	if hdr.Type != elfTypeExec && hdr.Type != elfTypeDyn {
		return nil, fmt.Errorf("elf: not an executable (type 0x%x)", hdr.Type)
	}

	// Static-pie ELFs (ET_DYN) have segment vaddrs relative to zero.
	// Slide them to the standard RISC-V virt RAM base so that OpenSBI and
	// other static-pie firmware land at the address they expect to run from.
	// True shared libraries are also ET_DYN but have no fixed runtime address;
	// we treat everything as static-pie here since the emulator only runs
	// bare-metal firmware and kernels, not userspace dynamic linking.
	loadBias := uint64(0)
	if hdr.Type == elfTypeDyn {
		loadBias = staticPieBase
	}
	if hdr.Machine != elfMachRISCV {
		return nil, fmt.Errorf("elf: not RISC-V (machine 0x%x)", hdr.Machine)
	}
	if hdr.PhEntSize != 56 {
		return nil, fmt.Errorf("elf: unexpected phentsize %d", hdr.PhEntSize)
	}
	if hdr.PhNum == 0 {
		return nil, errors.New("elf: no program headers")
	}

	// ── iterate program headers ───────────────────────────────────────────
	loaded := 0
	var loadedImageSize uint64
	for i := range int(hdr.PhNum) {
		offset := int64(hdr.PhOff) + int64(i)*int64(hdr.PhEntSize)
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("elf: seek to phdr %d: %w", i, err)
		}
		var ph Elf64Phdr
		if err := binary.Read(r, le, &ph); err != nil {
			return nil, fmt.Errorf("elf: read phdr %d: %w", i, err)
		}
		if ph.Type != ptLoad {
			continue // skip NOTE, GNU_STACK, RISCV_ATTRIBUTES, etc.
		}
		if ph.MemSz == 0 {
			continue
		}
		if loadedImageSize > ^uint64(0)-ph.MemSz {
			return nil, fmt.Errorf("elf: PT_LOAD image size overflow")
		}
		loadedImageSize += ph.MemSz
		if ph.VAddr > ^uint64(0)-loadBias {
			return nil, fmt.Errorf("elf: segment %d address overflow", i)
		}
		segAddr := ph.VAddr + loadBias

		// Copy file bytes into guest memory.
		if ph.FileSz > 0 {
			if _, err := r.Seek(int64(ph.Offset), io.SeekStart); err != nil {
				return nil, fmt.Errorf("elf: seek to segment %d data: %w", i, err)
			}
			buf := make([]byte, ph.FileSz)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, fmt.Errorf("elf: read segment %d: %w", i, err)
			}
			if f := mem.WriteBytes(segAddr, buf); f != nil {
				return nil, fmt.Errorf("elf: write segment %d to 0x%x: %v",
					i, segAddr, f)
			}
		}

		// Zero-fill BSS (MemSz > FileSz)
		if ph.MemSz > ph.FileSz {
			if segAddr > ^uint64(0)-ph.FileSz {
				return nil, fmt.Errorf("elf: segment %d BSS address overflow", i)
			}
			bssStart := segAddr + ph.FileSz
			bssSize := ph.MemSz - ph.FileSz
			if f := mem.ZeroRange(bssStart, bssSize); f != nil {
				return nil, fmt.Errorf("elf: zero BSS for segment %d: %v", i, f)
			}
		}

		// Record executable extents.
		if ph.Flags&pfX != 0 {
			if segAddr > ^uint64(0)-ph.MemSz {
				return nil, fmt.Errorf("elf: segment %d executable range overflow", i)
			}
			mem.AddExecRegion(segAddr, segAddr+ph.MemSz, ph.Flags&pfW != 0)
		}

		loaded++
	}

	if loaded == 0 {
		return nil, errors.New("elf: no PT_LOAD segments found")
	}
	if data != nil {
		mem.loadedELFSize = uint64(len(data))
	}
	mem.loadedELFImageSize = loadedImageSize

	// ── apply RELA relocations for static-pie ELFs ────────────────────────
	// Static-pie ELFs (ET_DYN) contain a .rela.dyn section with
	// R_RISCV_RELATIVE entries that patch internal pointers at load time.
	// A normal dynamic linker would do this; we do it here since the emulator
	// has no dynamic linker. Each entry is 24 bytes: r_offset (8), r_info (8),
	// r_addend (8). For R_RISCV_RELATIVE (type 3):
	//   *( loadBias + r_offset ) = loadBias + r_addend
	if hdr.Type == elfTypeDyn && data != nil {
		if err := applyRelaRelocations(mem, data, loadBias); err != nil {
			return nil, fmt.Errorf("elf: RELA relocations: %w", err)
		}
	}

	// ── parse section headers ─────────────────────────────────────────────
	var shdrs []*Elf64Shdr
	if hdr.ShOff != 0 && hdr.ShNum != 0 && hdr.ShEntSize >= 64 && data != nil {
		for i := 0; i < int(hdr.ShNum); i++ {
			off := int(hdr.ShOff) + i*int(hdr.ShEntSize)
			if off+64 > len(data) {
				break
			}
			sh := new(Elf64Shdr)
			if err := binary.Read(&byteReader{data: data[off:]}, le, sh); err != nil {
				break
			}
			shdrs = append(shdrs, sh)
		}
	}

	ef := &ELF{
		Entry:    hdr.Entry + loadBias,
		LoadBias: loadBias,
		Header:   &hdr,
		Shdrs:    shdrs,
		Data:     data,
	}

	// Auto-detect tohost symbol.
	if addr, ok := ef.FindSymbolAddr("tohost"); ok {
		ef.TohostAddr = addr
		mem.TohostAddr = addr
	}

	return ef, nil
}

// applyRelaRelocations processes the RELA relocation table for a static-pie
// ELF. It scans the program headers for PT_DYNAMIC, reads the RELA/RELASZ
// entries to locate the relocation table in the file, then applies each
// R_RISCV_RELATIVE entry into guest memory.
//
// Only R_RISCV_RELATIVE (type 3) is handled. Any other relocation type causes
// an error — static-pie firmware should never have symbol-dependent relocations.
func applyRelaRelocations(mem *GuestMemory, data []byte, loadBias uint64) error {
	le := binary.LittleEndian

	if len(data) < 64 {
		return nil
	}

	phOff := le.Uint64(data[32:])
	phEntSize := le.Uint16(data[54:])
	phNum := le.Uint16(data[56:])

	if phOff == 0 || phNum == 0 || phEntSize < 56 {
		return nil
	}

	// Walk program headers to find PT_DYNAMIC (type 2).
	const ptDynamic = 2
	var dynOffset, dynFileSz uint64
	for i := 0; i < int(phNum); i++ {
		off := int(phOff) + i*int(phEntSize)
		if off+56 > len(data) {
			break
		}
		phType := le.Uint32(data[off:])
		if phType != ptDynamic {
			continue
		}
		dynOffset = le.Uint64(data[off+8:])  // p_offset (file offset)
		dynFileSz = le.Uint64(data[off+32:]) // p_filesz
		break
	}
	if dynOffset == 0 {
		return nil // no DYNAMIC segment, nothing to do
	}

	// Parse DYNAMIC entries to find DT_RELA, DT_RELASZ.
	// Each dynamic entry is 16 bytes: d_tag (8) + d_val (8).
	const (
		dtRela      = 7
		dtRelasz    = 8
		dtRelaent   = 9
		dtRelacount = 0x6ffffff9
		dtNull      = 0
	)
	var relaVAddr, relaSz uint64
	relaEnt := uint64(24) // default RELA entry size
	for pos := dynOffset; pos+16 <= dynOffset+dynFileSz && pos+16 <= uint64(len(data)); pos += 16 {
		tag := le.Uint64(data[pos:])
		val := le.Uint64(data[pos+8:])
		switch tag {
		case dtRela:
			relaVAddr = val
		case dtRelasz:
			relaSz = val
		case dtRelaent:
			relaEnt = val
		case dtNull:
			goto doneDyn
		}
	}
doneDyn:
	if relaVAddr == 0 || relaSz == 0 || relaEnt == 0 {
		return nil // no RELA table
	}

	// relaVAddr is a vaddr relative to zero (pre-slide). We need to find its
	// file offset by scanning PT_LOAD segments for the one that contains it.
	relaFileOffset := uint64(0)
	for i := 0; i < int(phNum); i++ {
		off := int(phOff) + i*int(phEntSize)
		if off+56 > len(data) {
			break
		}
		if le.Uint32(data[off:]) != 1 { // PT_LOAD
			continue
		}
		segVAddr := le.Uint64(data[off+16:])
		segFileSz := le.Uint64(data[off+32:])
		segOffset := le.Uint64(data[off+8:])
		if relaVAddr >= segVAddr && relaVAddr < segVAddr+segFileSz {
			relaFileOffset = segOffset + (relaVAddr - segVAddr)
			break
		}
	}
	if relaFileOffset == 0 {
		return fmt.Errorf("cannot find file offset for RELA vaddr 0x%x", relaVAddr)
	}

	// Apply relocations. Each RELA entry: r_offset(8) r_info(8) r_addend(8).
	const rRISCVRelative = 3
	nRela := relaSz / relaEnt
	for i := uint64(0); i < nRela; i++ {
		pos := relaFileOffset + i*relaEnt
		if pos+24 > uint64(len(data)) {
			return fmt.Errorf("RELA entry %d out of bounds", i)
		}
		rOffset := le.Uint64(data[pos:])
		rInfo := le.Uint64(data[pos+8:])
		rAddend := le.Uint64(data[pos+16:]) // treated as int64 for signed addends

		relocType := rInfo & 0xffffffff
		if relocType != rRISCVRelative {
			return fmt.Errorf("unsupported relocation type %d at entry %d", relocType, i)
		}

		// *( loadBias + r_offset ) = loadBias + r_addend
		target := loadBias + rOffset
		value := loadBias + rAddend // int64 addend; loadBias dominates for firmware

		var buf [8]byte
		le.PutUint64(buf[:], value)
		if f := mem.WriteBytes(target, buf[:]); f != nil {
			return fmt.Errorf("write reloc %d to 0x%x: %v", i, target, f)
		}
	}
	return nil
}

// FindSymbolAddr looks up a symbol by name in the ELF symbol table.
// Uses pre-parsed Shdrs to locate SHT_SYMTAB; reads symbol entries
// and string table from Data. Returns (0, false) if not found.
func (ef *ELF) FindSymbolAddr(name string) (uint64, bool) {
	if ef == nil || ef.Data == nil || len(ef.Shdrs) == 0 {
		return 0, false
	}
	le := binary.LittleEndian
	data := ef.Data

	// Find SHT_SYMTAB section.
	var symtab *Elf64Shdr
	for _, sh := range ef.Shdrs {
		if sh.Type == shtSymtab {
			symtab = sh
			break
		}
	}
	if symtab == nil {
		return 0, false
	}

	// Read the associated string table (sh_link points to STRTAB index).
	if int(symtab.Link) >= len(ef.Shdrs) {
		return 0, false
	}
	strtab := ef.Shdrs[symtab.Link]
	if uint64(len(data)) < strtab.Offset+strtab.Size {
		return 0, false
	}
	strData := data[strtab.Offset : strtab.Offset+strtab.Size]

	// Iterate symbol entries.
	entSize := int(symtab.EntSize)
	if entSize < 24 {
		entSize = 24
	}
	nSyms := int(symtab.Size) / entSize
	for i := 0; i < nSyms; i++ {
		off := int(symtab.Offset) + i*entSize
		if off+24 > len(data) {
			break
		}
		var sym Elf64Sym
		if err := binary.Read(&byteReader{data: data[off:]}, le, &sym); err != nil {
			break
		}
		nameOff := int(sym.Name)
		if nameOff >= len(strData) {
			continue
		}
		end := nameOff
		for end < len(strData) && strData[end] != 0 {
			end++
		}
		if string(strData[nameOff:end]) == name {
			return ef.LoadBias + sym.Value, true
		}
	}
	return 0, false
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
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.pos + offset
	case io.SeekEnd:
		abs = int64(len(b.data)) + offset
	default:
		return 0, errors.New("byteReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("byteReader: negative position")
	}
	b.pos = abs
	return abs, nil
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
	var shstr Elf64Shdr
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
		var sh Elf64Shdr
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
		var ph Elf64Phdr
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
	shOff := strtabOff + strtabPad
	total := int(shOff) + shdrCount*shdrSize

	buf := make([]byte, total)
	le := binary.LittleEndian

	// ── ELF header ───────────────────────────────────────────────────────
	copy(buf[0:], "\x7fELF")
	buf[4], buf[5], buf[6] = 2, 1, 1  // ELFCLASS64, ELFDATA2LSB, EV_CURRENT
	le.PutUint16(buf[16:], 2)         // ET_EXEC
	le.PutUint16(buf[18:], 0xF3)      // EM_RISCV
	le.PutUint32(buf[20:], 1)         // e_version
	le.PutUint64(buf[24:], codeVA)    // e_entry
	le.PutUint64(buf[32:], 64)        // e_phoff
	le.PutUint64(buf[40:], shOff)     // e_shoff
	le.PutUint16(buf[52:], 64)        // e_ehsize
	le.PutUint16(buf[54:], 56)        // e_phentsize
	le.PutUint16(buf[56:], 1)         // e_phnum
	le.PutUint16(buf[58:], shdrSize)  // e_shentsize
	le.PutUint16(buf[60:], shdrCount) // e_shnum
	le.PutUint16(buf[62:], 1)         // e_shstrndx = section [1]

	// ── Program header ────────────────────────────────────────────────────
	ph := buf[64:]
	le.PutUint32(ph[0:], 1)                       // PT_LOAD
	le.PutUint32(ph[4:], 5)                       // PF_R|PF_X
	le.PutUint64(ph[8:], uint64(codeOff))         // p_offset
	le.PutUint64(ph[16:], codeVA)                 // p_vaddr
	le.PutUint64(ph[24:], codeVA)                 // p_paddr
	le.PutUint64(ph[32:], uint64(len(codeBytes))) // p_filesz
	le.PutUint64(ph[40:], uint64(len(codeBytes))) // p_memsz
	le.PutUint64(ph[48:], 0x10)                   // p_align

	// ── Code ──────────────────────────────────────────────────────────────
	copy(buf[codeOff:], codeBytes)

	// ── Strtab: just a single null byte (all names are "") ────────────────
	buf[strtabOff] = 0x00

	// ── Section header [0]: SHT_NULL ─────────────────────────────────────
	// All zeros — required first entry in every ELF section table.

	// ── Section header [1]: SHT_STRTAB (.shstrtab) ───────────────────────
	sh1 := buf[int(shOff)+shdrSize:]
	// sh_name=0 (points to "\0" in strtab — empty string, valid)
	le.PutUint32(sh1[4:], 3)          // sh_type = SHT_STRTAB
	le.PutUint64(sh1[24:], strtabOff) // sh_offset
	le.PutUint64(sh1[32:], 1)         // sh_size = 1 byte ("\0")
	le.PutUint64(sh1[48:], 1)         // sh_addralign

	return buf
}
