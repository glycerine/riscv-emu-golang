package riscv

// jit_vizjit.go — per-block debug dump of guest RISC-V → IR → host x86
// assembly, gated on ir.VIZJIT_DIR. One file per compiled block,
// named <run-tag>.gocpu.asm.pc_<hex>.asm so a sorted ls groups all
// outputs from one emulator run together.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"riscv/ir"
)

// vizJitTag is the 16-hex-char session tag — generated once per
// emulator run and prepended to every dump filename.
var (
	vizJitTagOnce sync.Once
	vizJitTag     string
	vizJitDirOnce sync.Once
	vizJitDirOK   bool
	vizJitDirErr  error
)

func getVizJitTag() string {
	vizJitTagOnce.Do(func() {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			// crypto/rand failing is basically impossible; fall back to
			// a fixed tag rather than panicking in a debug facility.
			vizJitTag = "0000000000000000"
			return
		}
		vizJitTag = hex.EncodeToString(b[:])
	})
	return vizJitTag
}

// GetVizJitTag returns the 16-hex-char run tag used in VizJit dump
// filenames. Intended for callers outside this package (e.g. hellobench)
// that want to align companion dumps with GoCPU's by sharing the tag.
func GetVizJitTag() string { return getVizJitTag() }

// DisasmRV32 renders one 32-bit RISC-V instruction as a mnemonic.
// Exported so external tooling (hellobench's libriscv-dump augmenter)
// can format guest-code sections the same way VizJit does.
func DisasmRV32(pc uint64, insn uint32) string { return disasmRV32(pc, insn) }

// DisasmRVC renders one 16-bit compressed RISC-V instruction as a
// mnemonic. See DisasmRV32.
func DisasmRVC(insn uint16) string { return disasmRVC(insn) }

// AugmentLibriscvDumps walks `dir` for files matching
// `*.libriscv.asm.pc_*.asm` (written by libriscv's tr_dump.cpp) and
// appends a mnemonic column to every hex line inside the
// `== Guest RISC-V ==` section. The C++ dumper produces hex-only
// entries on purpose, leaving disassembly to Go — the C++ side has no
// RISC-V disassembler, and duplicating GoCPU's would drift.
//
// Idempotent: lines that already have a mnemonic are left alone.
// Only the Guest RISC-V section is touched; other sections pass through.
func AugmentLibriscvDumps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !strings.Contains(name, ".libriscv.asm.pc_") ||
			!strings.HasSuffix(name, ".asm") {
			continue
		}
		path := filepath.Join(dir, name)
		if err := augmentOneLibriscvDump(path); err != nil {
			fmt.Fprintf(os.Stderr, "AugmentLibriscvDumps: %s: %v\n", path, err)
		}
	}
	return nil
}

func augmentOneLibriscvDump(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")

	// Find the Guest RISC-V section bounds. Section ends at the next
	// `== ` header or EOF.
	var startIdx, endIdx = -1, len(lines)
	for i, ln := range lines {
		if ln == "== Guest RISC-V ==" {
			startIdx = i + 1
			continue
		}
		if startIdx >= 0 && strings.HasPrefix(ln, "== ") {
			endIdx = i
			break
		}
	}
	if startIdx < 0 {
		return nil // no section; nothing to do
	}

	changed := false
	for i := startIdx; i < endIdx; i++ {
		ln := lines[i]
		// Match "0xPPPPPPPP  HHHH" (RVC, 4-hex) or
		//       "0xPPPPPPPP  HHHHHHHH" (RV32, 8-hex). Skip lines that
		// already have something past the hex.
		pc, hex, ok := parseHexLine(ln)
		if !ok {
			continue
		}
		var mnem string
		switch len(hex) {
		case 4:
			v, err := strconv.ParseUint(hex, 16, 16)
			if err != nil {
				continue
			}
			mnem = disasmRVC(uint16(v))
			lines[i] = fmt.Sprintf("0x%08x  %04x      %s", pc, v, mnem)
		case 8:
			v, err := strconv.ParseUint(hex, 16, 32)
			if err != nil {
				continue
			}
			mnem = disasmRV32(pc, uint32(v))
			lines[i] = fmt.Sprintf("0x%08x  %08x  %s", pc, v, mnem)
		default:
			continue
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// parseHexLine extracts `pc` and the `hex` token from a line of form
// "0xPPPPPPPP  HHHH" or "0xPPPPPPPP  HHHHHHHH". Returns ok=false if the
// line isn't a plain hex-only entry (already has a mnemonic, unreadable
// marker, blank, etc.).
func parseHexLine(ln string) (pc uint64, hex string, ok bool) {
	s := strings.TrimRight(ln, " \t")
	if !strings.HasPrefix(s, "0x") {
		return 0, "", false
	}
	parts := strings.Fields(s)
	if len(parts) != 2 { // strictly two columns
		return 0, "", false
	}
	pcStr := strings.TrimPrefix(parts[0], "0x")
	v, err := strconv.ParseUint(pcStr, 16, 64)
	if err != nil {
		return 0, "", false
	}
	if ln := len(parts[1]); ln != 4 && ln != 8 {
		return 0, "", false
	}
	return v, parts[1], true
}

// vizJitEnabled returns the dump directory if VizJit is active, or
// ("", false) if disabled. Creates the directory on first active
// call.
func vizJitEnabled() (string, bool) {
	dir := ir.VIZJIT_DIR
	if dir == "" {
		return "", false
	}
	vizJitDirOnce.Do(func() {
		vizJitDirErr = os.MkdirAll(dir, 0o755)
		vizJitDirOK = vizJitDirErr == nil
		if vizJitDirErr != nil {
			fmt.Fprintf(os.Stderr, "VizJit: could not create dir %q: %v — dumps disabled\n",
				dir, vizJitDirErr)
			panic(fmt.Sprintf("requested but not possible VIZJIT_DIR path '%v': '%v' --fix your directories?", dir, vizJitDirErr))
		}
	})
	return dir, vizJitDirOK
}

// vizJitDump writes a single block's dump to disk. Returns without
// error on any failure — this is a debug facility, never the critical
// path.
//
// startPC..endPC: guest RISC-V PC range for the block.
// mem:            guest memory to fetch instruction bytes.
// block:          IR block (for the IR listing).
// progs:          goasm Ctx.DumpProgs() output (host assembly).
// codeLen:        size of assembled host bytes.
// codeBase:       base address of host code (0 if not yet placed).
func vizJitDump(
	startPC, endPC uint64,
	mem *GuestMemory,
	block *ir.Block,
	progs string,
	codeLen int,
	codeBase uintptr,
) {
	dir, ok := vizJitEnabled()
	if !ok {
		return
	}
	tag := getVizJitTag()
	fname := fmt.Sprintf("%s.gocpu.asm.pc_0x%08x.asm", tag, startPC)
	path := filepath.Join(dir, fname)

	var sb strings.Builder
	fmt.Fprintf(&sb, "# gocpu VizJit dump\n")
	fmt.Fprintf(&sb, "# run tag:    %s\n", tag)
	fmt.Fprintf(&sb, "# entry PC:   0x%08x\n", startPC)
	fmt.Fprintf(&sb, "# byte range: 0x%08x..0x%08x (%d bytes)\n",
		startPC, endPC, endPC-startPC)
	if codeBase != 0 {
		fmt.Fprintf(&sb, "# host code:  0x%x, %d bytes\n", codeBase, codeLen)
	} else {
		fmt.Fprintf(&sb, "# host code:  %d bytes\n", codeLen)
	}
	sb.WriteString("\n")

	sb.WriteString("== Guest RISC-V ==\n")
	if mem != nil {
		vizJitDisasmGuest(&sb, mem, startPC, endPC)
	} else {
		sb.WriteString("(guest memory not available for this block)\n")
	}
	sb.WriteString("\n")

	sb.WriteString("== IR ==\n")
	if block != nil {
		for _, ins := range block.Instrs {
			sb.WriteString(ins.String())
			sb.WriteByte('\n')
		}
	}
	sb.WriteString("\n")

	sb.WriteString("== Host (goasm Progs) ==\n")
	sb.WriteString(progs)
	if !strings.HasSuffix(progs, "\n") {
		sb.WriteByte('\n')
	}

	_ = os.WriteFile(path, []byte(sb.String()), 0o644)
}

// vizJitDumpIndex appends one line to the per-run index file mapping
// entry PC to dump filename. Called opportunistically from
// InstallAOT.
func vizJitDumpIndex(lines []string) {
	dir, ok := vizJitEnabled()
	if !ok {
		return
	}
	tag := getVizJitTag()
	path := filepath.Join(dir, fmt.Sprintf("%s.gocpu.asm.index.txt", tag))
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// ── RISC-V disassembly ─────────────────────────────────────────────────
//
// Not exhaustive — covers the common RV64IMAFDC subset we see in
// practice. Unknown encodings render as "??? raw=0x...." with the
// opcode shown so the reader can correlate.

var abiRegNames = [32]string{
	"zero", "ra", "sp", "gp", "tp", "t0", "t1", "t2",
	"s0", "s1", "a0", "a1", "a2", "a3", "a4", "a5",
	"a6", "a7", "s2", "s3", "s4", "s5", "s6", "s7",
	"s8", "s9", "s10", "s11", "t3", "t4", "t5", "t6",
}

var abiFRegNames = [32]string{
	"ft0", "ft1", "ft2", "ft3", "ft4", "ft5", "ft6", "ft7",
	"fs0", "fs1", "fa0", "fa1", "fa2", "fa3", "fa4", "fa5",
	"fa6", "fa7", "fs2", "fs3", "fs4", "fs5", "fs6", "fs7",
	"fs8", "fs9", "fs10", "fs11", "ft8", "ft9", "ft10", "ft11",
}

func rn(r uint8) string {
	if r > 31 {
		return fmt.Sprintf("x%d", r)
	}
	return abiRegNames[r]
}

func frn(r uint8) string {
	if r > 31 {
		return fmt.Sprintf("f%d", r)
	}
	return abiFRegNames[r]
}

// vizJitDisasmGuest walks the byte range and emits one disassembly
// line per instruction.
func vizJitDisasmGuest(sb *strings.Builder, mem *GuestMemory, startPC, endPC uint64) {
	pc := startPC
	for pc < endPC {
		half, fh := mem.Fetch16(pc)
		if fh != nil {
			fmt.Fprintf(sb, "0x%08x  <fetch16 fault: %v>\n", pc, fh)
			break
		}
		if half&0x3 != 0x3 {
			// 16-bit RVC instruction
			fmt.Fprintf(sb, "0x%08x  %04x      %s\n", pc, half, disasmRVC(uint16(half)))
			pc += 2
			continue
		}
		insn, f := mem.Fetch32(pc)
		if f != nil {
			if f.Kind == FaultMisalign {
				insn, f = mem.Fetch32U(pc)
			}
			if f != nil {
				fmt.Fprintf(sb, "0x%08x  <fetch32 fault: %v>\n", pc, f)
				break
			}
		}
		fmt.Fprintf(sb, "0x%08x  %08x  %s\n", pc, insn, disasmRV32(pc, insn))
		pc += 4
	}
}

// disasmRV32 renders one 32-bit instruction as a mnemonic string.
func disasmRV32(pc uint64, insn uint32) string {
	var d DecodedInsn
	decodeInsn32(&d, insn)

	rd := d.rd
	rs1 := d.rs1
	rs2 := d.rs2
	f3 := d.funct3
	f7 := d.funct7
	imm := int64(d.imm)

	switch d.op {
	case 0x37: // LUI
		return fmt.Sprintf("lui     %s, 0x%x", rn(rd), uint32(imm)>>12)
	case 0x17: // AUIPC
		return fmt.Sprintf("auipc   %s, 0x%x", rn(rd), uint32(imm)>>12)
	case 0x6F: // JAL
		target := uint64(int64(pc) + imm)
		if rd == 0 {
			return fmt.Sprintf("j       0x%x", target)
		}
		return fmt.Sprintf("jal     %s, 0x%x", rn(rd), target)
	case 0x67: // JALR
		if rd == 0 && rs1 == 1 && imm == 0 {
			return "ret"
		}
		return fmt.Sprintf("jalr    %s, %d(%s)", rn(rd), imm, rn(rs1))
	case 0x63: // BRANCH
		target := uint64(int64(pc) + imm)
		name := []string{"beq", "bne", "???", "???", "blt", "bge", "bltu", "bgeu"}[f3]
		return fmt.Sprintf("%-7s %s, %s, 0x%x", name, rn(rs1), rn(rs2), target)
	case 0x03: // LOAD
		name := []string{"lb", "lh", "lw", "ld", "lbu", "lhu", "lwu", "???"}[f3]
		return fmt.Sprintf("%-7s %s, %d(%s)", name, rn(rd), imm, rn(rs1))
	case 0x23: // STORE
		name := []string{"sb", "sh", "sw", "sd", "???", "???", "???", "???"}[f3]
		return fmt.Sprintf("%-7s %s, %d(%s)", name, rn(rs2), imm, rn(rs1))
	case 0x13: // OP-IMM
		switch f3 {
		case 0:
			if rd == 0 && rs1 == 0 && imm == 0 {
				return "nop"
			}
			if imm == 0 {
				return fmt.Sprintf("mv      %s, %s", rn(rd), rn(rs1))
			}
			if rs1 == 0 {
				return fmt.Sprintf("li      %s, %d", rn(rd), imm)
			}
			return fmt.Sprintf("addi    %s, %s, %d", rn(rd), rn(rs1), imm)
		case 2:
			return fmt.Sprintf("slti    %s, %s, %d", rn(rd), rn(rs1), imm)
		case 3:
			return fmt.Sprintf("sltiu   %s, %s, %d", rn(rd), rn(rs1), imm)
		case 4:
			return fmt.Sprintf("xori    %s, %s, %d", rn(rd), rn(rs1), imm)
		case 6:
			return fmt.Sprintf("ori     %s, %s, %d", rn(rd), rn(rs1), imm)
		case 7:
			return fmt.Sprintf("andi    %s, %s, %d", rn(rd), rn(rs1), imm)
		case 1:
			return fmt.Sprintf("slli    %s, %s, %d", rn(rd), rn(rs1), imm&0x3F)
		case 5:
			if f7&0x40 != 0 {
				return fmt.Sprintf("srai    %s, %s, %d", rn(rd), rn(rs1), imm&0x3F)
			}
			return fmt.Sprintf("srli    %s, %s, %d", rn(rd), rn(rs1), imm&0x3F)
		}
	case 0x1B: // OP-IMM-32
		switch f3 {
		case 0:
			return fmt.Sprintf("addiw   %s, %s, %d", rn(rd), rn(rs1), imm)
		case 1:
			return fmt.Sprintf("slliw   %s, %s, %d", rn(rd), rn(rs1), imm&0x1F)
		case 5:
			if f7&0x40 != 0 {
				return fmt.Sprintf("sraiw   %s, %s, %d", rn(rd), rn(rs1), imm&0x1F)
			}
			return fmt.Sprintf("srliw   %s, %s, %d", rn(rd), rn(rs1), imm&0x1F)
		}
	case 0x33: // OP
		return disasmRV32_OP(rd, rs1, rs2, f3, f7)
	case 0x3B: // OP-32
		return disasmRV32_OP32(rd, rs1, rs2, f3, f7)
	case 0x0F: // MISC-MEM
		if f3 == 0 {
			return "fence"
		}
		if f3 == 1 {
			return "fence.i"
		}
	case 0x73: // SYSTEM
		switch insn {
		case 0x00000073:
			return "ecall"
		case 0x00100073:
			return "ebreak"
		case 0x30200073:
			return "mret"
		case 0x10200073:
			return "sret"
		}
		// CSR ops
		name := []string{"???", "csrrw", "csrrs", "csrrc", "???", "csrrwi", "csrrsi", "csrrci"}[f3]
		return fmt.Sprintf("%-7s %s, 0x%x, %s", name, rn(rd), uint32(insn)>>20, rn(rs1))
	case 0x2F: // AMO
		return disasmRV32_AMO(rd, rs1, rs2, f3, f7)
	case 0x07: // F-load
		if f3 == 2 {
			return fmt.Sprintf("flw     %s, %d(%s)", frn(rd), imm, rn(rs1))
		}
		if f3 == 3 {
			return fmt.Sprintf("fld     %s, %d(%s)", frn(rd), imm, rn(rs1))
		}
	case 0x27: // F-store
		if f3 == 2 {
			return fmt.Sprintf("fsw     %s, %d(%s)", frn(rs2), imm, rn(rs1))
		}
		if f3 == 3 {
			return fmt.Sprintf("fsd     %s, %d(%s)", frn(rs2), imm, rn(rs1))
		}
	case 0x43, 0x47, 0x4B, 0x4F: // FMADD/FMSUB/FNMADD/FNMSUB
		return fmt.Sprintf("fmadd/fmsub/fnmadd/fnmsub op=0x%x rd=%s rs1=%s rs2=%s rs3=%s",
			d.op, frn(rd), frn(rs1), frn(rs2), frn(d.rs3))
	case 0x53: // OP-FP
		return fmt.Sprintf("op-fp   funct7=0x%02x rd=%s rs1=%s rs2=%s", f7, frn(rd), frn(rs1), frn(rs2))
	}
	return fmt.Sprintf("??? op=0x%02x raw=0x%08x", d.op, insn)
}

func disasmRV32_OP(rd, rs1, rs2, f3, f7 uint8) string {
	switch f7 {
	case 0x00:
		name := []string{"add", "sll", "slt", "sltu", "xor", "srl", "or", "and"}[f3]
		return fmt.Sprintf("%-7s %s, %s, %s", name, rn(rd), rn(rs1), rn(rs2))
	case 0x20:
		if f3 == 0 {
			return fmt.Sprintf("sub     %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		}
		if f3 == 5 {
			return fmt.Sprintf("sra     %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		}
	case 0x01:
		name := []string{"mul", "mulh", "mulhsu", "mulhu", "div", "divu", "rem", "remu"}[f3]
		return fmt.Sprintf("%-7s %s, %s, %s", name, rn(rd), rn(rs1), rn(rs2))
	}
	return fmt.Sprintf("op      f7=0x%02x f3=%d %s, %s, %s", f7, f3, rn(rd), rn(rs1), rn(rs2))
}

func disasmRV32_OP32(rd, rs1, rs2, f3, f7 uint8) string {
	switch f7 {
	case 0x00:
		switch f3 {
		case 0:
			return fmt.Sprintf("addw    %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		case 1:
			return fmt.Sprintf("sllw    %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		case 5:
			return fmt.Sprintf("srlw    %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		}
	case 0x20:
		if f3 == 0 {
			return fmt.Sprintf("subw    %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		}
		if f3 == 5 {
			return fmt.Sprintf("sraw    %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
		}
	case 0x01:
		names := []string{"mulw", "???", "???", "???", "divw", "divuw", "remw", "remuw"}
		return fmt.Sprintf("%-7s %s, %s, %s", names[f3], rn(rd), rn(rs1), rn(rs2))
	}
	return fmt.Sprintf("op32    f7=0x%02x f3=%d", f7, f3)
}

func disasmRV32_AMO(rd, rs1, rs2, f3, f7 uint8) string {
	suffix := ".w"
	if f3 == 3 {
		suffix = ".d"
	}
	op := f7 >> 2
	names := map[uint8]string{
		0x02: "amoadd", 0x00: "amoadd", 0x01: "amoswap", 0x03: "lr",
		0x04: "sc", 0x08: "amoor", 0x0C: "amoand", 0x10: "amomin",
		0x14: "amomax", 0x18: "amominu", 0x1C: "amomaxu", 0x05: "amoxor",
	}
	name, ok := names[op]
	if !ok {
		name = fmt.Sprintf("amo_0x%02x", op)
	}
	if op == 0x03 { // LR
		return fmt.Sprintf("%s%s    %s, (%s)", name, suffix, rn(rd), rn(rs1))
	}
	return fmt.Sprintf("%s%s  %s, %s, (%s)", name, suffix, rn(rd), rn(rs2), rn(rs1))
}

// disasmRVC renders a 16-bit RVC instruction. Mirrors decodeRVC's dispatch.
func disasmRVC(insn uint16) string {
	var d DecodedInsn
	decodeRVC(&d, insn)
	imm := int64(d.imm)
	switch d.op {
	case opC_ADDI4SPN:
		return fmt.Sprintf("c.addi4spn %s, sp, %d", rn(d.rd), imm)
	case opC_FLD:
		return fmt.Sprintf("c.fld   %s, %d(%s)", frn(d.rd), imm, rn(d.rs1))
	case opC_LW:
		return fmt.Sprintf("c.lw    %s, %d(%s)", rn(d.rd), imm, rn(d.rs1))
	case opC_LD:
		return fmt.Sprintf("c.ld    %s, %d(%s)", rn(d.rd), imm, rn(d.rs1))
	case opC_FSD:
		return fmt.Sprintf("c.fsd   %s, %d(%s)", frn(d.rs2), imm, rn(d.rs1))
	case opC_SW:
		return fmt.Sprintf("c.sw    %s, %d(%s)", rn(d.rs2), imm, rn(d.rs1))
	case opC_SD:
		return fmt.Sprintf("c.sd    %s, %d(%s)", rn(d.rs2), imm, rn(d.rs1))
	case opC_ADDI:
		if d.rd == 0 && imm == 0 {
			return "c.nop"
		}
		return fmt.Sprintf("c.addi  %s, %d", rn(d.rd), imm)
	case opC_ADDIW:
		return fmt.Sprintf("c.addiw %s, %d", rn(d.rd), imm)
	case opC_LI:
		return fmt.Sprintf("c.li    %s, %d", rn(d.rd), imm)
	case opC_LUI_OR_ADDI16SP:
		if d.rd == 2 {
			return fmt.Sprintf("c.addi16sp sp, %d", imm)
		}
		return fmt.Sprintf("c.lui   %s, 0x%x", rn(d.rd), uint32(imm)>>12)
	case opC_MISC_ALU:
		return fmt.Sprintf("c.misc-alu raw=0x%04x", insn)
	case opC_J:
		return fmt.Sprintf("c.j     %+d", imm)
	case opC_BEQZ:
		return fmt.Sprintf("c.beqz  %s, %+d", rn(d.rs1), imm)
	case opC_BNEZ:
		return fmt.Sprintf("c.bnez  %s, %+d", rn(d.rs1), imm)
	case opC_SLLI:
		return fmt.Sprintf("c.slli  %s, %d", rn(d.rd), imm)
	case opC_FLDSP:
		return fmt.Sprintf("c.fldsp %s, %d(sp)", frn(d.rd), imm)
	case opC_LWSP:
		return fmt.Sprintf("c.lwsp  %s, %d(sp)", rn(d.rd), imm)
	case opC_LDSP:
		return fmt.Sprintf("c.ldsp  %s, %d(sp)", rn(d.rd), imm)
	case opC_JR:
		return fmt.Sprintf("c.jr    %s", rn(d.rd))
	case opC_MV:
		return fmt.Sprintf("c.mv    %s, %s", rn(d.rd), rn(d.rs2))
	case opC_EBREAK:
		return "c.ebreak"
	case opC_JALR:
		return fmt.Sprintf("c.jalr  %s", rn(d.rd))
	case opC_ADD:
		return fmt.Sprintf("c.add   %s, %s", rn(d.rd), rn(d.rs2))
	case opC_FSDSP:
		return fmt.Sprintf("c.fsdsp %s, %d(sp)", frn(d.rs2), imm)
	case opC_SWSP:
		return fmt.Sprintf("c.swsp  %s, %d(sp)", rn(d.rs2), imm)
	case opC_SDSP:
		return fmt.Sprintf("c.sdsp  %s, %d(sp)", rn(d.rs2), imm)
	}
	return fmt.Sprintf("??? c raw=0x%04x", insn)
}
