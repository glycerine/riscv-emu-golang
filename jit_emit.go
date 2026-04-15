package riscv

// jit_emit.go — Translates RISC-V basic blocks to C source code.
// The generated C is compiled by TCC into native x86-64 code.

import (
	"fmt"
	"strings"
)

// emitResult holds the generated C source and metadata about the block.
type emitResult struct {
	csrc     string   // complete C source for the block
	startPC  uint64   // first instruction PC
	endPC    uint64   // PC past the last instruction
	numInsns int      // number of RISC-V instructions translated
	regsUsed [32]bool // which registers are read or written
}

// emitBlock translates a basic block starting at pc into C source.
// Reads instructions from guest memory until a block-terminating instruction.
func emitBlock(mem *GuestMemory, pc uint64) *emitResult {
	e := &emitter{
		mem:     mem,
		startPC: pc,
		pc:      pc,
		body:    strings.Builder{},
	}

	for e.numInsns < 512 && !e.terminated {
		// Fetch instruction (handle RVC)
		half, fh := mem.Fetch16(e.pc)
		if fh != nil {
			break // can't fetch — end block
		}

		if half&0x3 != 0x3 {
			// 16-bit compressed instruction
			e.emitRVC(uint16(half))
		} else {
			// 32-bit instruction
			insn, f := mem.Fetch32(e.pc)
			if f != nil {
				break
			}
			e.emit32(insn)
		}
	}

	if e.numInsns == 0 {
		return nil // empty block (first instruction untranslatable)
	}

	return e.finalize()
}

// emitter accumulates C code for a basic block.
type emitter struct {
	mem       *GuestMemory
	startPC   uint64
	pc        uint64
	body      strings.Builder
	numInsns  int
	regsUsed  [32]bool
	terminated bool
	// Branch targets within this block (for internal gotos)
	labels map[uint64]bool
}

// ── C source generation helpers ─────────────────────────────────────────

func (e *emitter) rd(r uint32) string {
	if r == 0 {
		return "" // writes to x0 are discarded
	}
	e.regsUsed[r] = true
	return fmt.Sprintf("r%d", r)
}

func (e *emitter) rs(r uint32) string {
	if r == 0 {
		return "0"
	}
	e.regsUsed[r] = true
	return fmt.Sprintf("r%d", r)
}

func (e *emitter) emit(format string, args ...any) {
	fmt.Fprintf(&e.body, format, args...)
}

func (e *emitter) emitLabel() {
	e.emit("b_%x:\n", e.pc)
}

func (e *emitter) emitIC() {
	e.emit("    ic++;\n")
}

// emitWriteBack emits code to write back all cached registers and return.
func (e *emitter) emitReturn(pc uint64, status int) {
	e.emitWriteBackAll()
	e.emit("    return (JITResult){0x%xULL, ic, %d, 0};\n", pc, status)
}

func (e *emitter) emitReturnFault(pcExpr string, status int, addrExpr string) {
	e.emitWriteBackAll()
	e.emit("    return (JITResult){%s, ic, %d, %s};\n", pcExpr, status, addrExpr)
}

func (e *emitter) emitWriteBackAll() {
	for i := 1; i < 32; i++ {
		if e.regsUsed[i] {
			e.emit("    x[%d] = r%d;\n", i, i)
		}
	}
}

// advancePC moves to the next instruction.
func (e *emitter) advancePC(size uint64) {
	e.numInsns++
	e.pc += size
}

// ── 32-bit instruction emitter ──────────────────────────────────────────

func (e *emitter) emit32(insn uint32) {
	opcode := insn & 0x7F
	rd := (insn >> 7) & 0x1F
	funct3 := (insn >> 12) & 0x7
	rs1 := (insn >> 15) & 0x1F
	rs2 := (insn >> 20) & 0x1F
	funct7 := insn >> 25

	// I-type immediate (sign-extended)
	iimm := int64(int32(insn)) >> 20

	e.emitLabel()
	e.emitIC()

	switch opcode {
	// ── LUI ──────────────────────────────────────────────────────────
	case 0x37:
		uimm := int64(int32(insn & 0xFFFFF000))
		if rd != 0 {
			e.emit("    %s = %dLL;\n", e.rd(rd), uimm)
		}
		e.advancePC(4)

	// ── AUIPC ────────────────────────────────────────────────────────
	case 0x17:
		uimm := int64(int32(insn & 0xFFFFF000))
		if rd != 0 {
			e.emit("    %s = 0x%xULL + %dLL;\n", e.rd(rd), e.pc, uimm)
		}
		e.advancePC(4)

	// ── JAL ──────────────────────────────────────────────────────────
	case 0x6F:
		jimm := jImm(insn)
		target := e.pc + uint64(jimm)
		if rd != 0 {
			e.emit("    %s = 0x%xULL;\n", e.rd(rd), e.pc+4) // link
		}
		e.advancePC(4)
		e.emitReturn(target, jitOK)
		e.terminated = true

	// ── JALR ─────────────────────────────────────────────────────────
	case 0x67:
		// Compute target BEFORE writing link (rd may alias rs1).
		e.emit("    { uint64_t t = (%s + %dLL) & ~(uint64_t)1;\n", e.rs(rs1), iimm)
		if rd != 0 {
			e.emit("    %s = 0x%xULL;\n", e.rd(rd), e.pc+4) // link
		}
		e.advancePC(4)
		e.emitWriteBackAll()
		e.emit("      return (JITResult){t, ic, 0, 0}; }\n")
		e.terminated = true

	// ── BRANCH ───────────────────────────────────────────────────────
	case 0x63:
		bimm := bImm(insn)
		target := e.pc + uint64(bimm)
		_ = e.pc + 4 // nextPC (used implicitly by fall-through)
		cmp := branchCmp(funct3)
		if cmp == "" {
			e.terminated = true
			e.advancePC(4)
			break
		}

		// Check if target is within this block (backward branch = loop)
		if target >= e.startPC && target < e.pc {
			// Internal backward branch — emit goto
			e.emit("    if (%s %s %s) goto b_%x;\n",
				e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3), target)
			e.advancePC(4)
		} else {
			// External branch — exit block
			e.emit("    if (%s %s %s) {\n",
				e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3))
			e.advancePC(4)
			e.emitWriteBackAll()
			e.emit("      return (JITResult){0x%xULL, ic, 0, 0};\n    }\n", target)
			// Fall through continues to next instruction
		}

	// ── LOAD ─────────────────────────────────────────────────────────
	case 0x03:
		e.emitLoad(rd, rs1, iimm, funct3)
		e.advancePC(4)

	// ── STORE ────────────────────────────────────────────────────────
	case 0x23:
		simm := sImm(insn)
		e.emitStore(rs1, rs2, simm, funct3)
		e.advancePC(4)

	// ── OP-IMM ───────────────────────────────────────────────────────
	case 0x13:
		e.emitOpImm(rd, rs1, iimm, funct3, funct7)
		e.advancePC(4)

	// ── OP-IMM-32 ────────────────────────────────────────────────────
	case 0x1B:
		e.emitOpImm32(rd, rs1, iimm, funct3, funct7)
		e.advancePC(4)

	// ── OP (R-type) ──────────────────────────────────────────────────
	case 0x33:
		e.emitOp(rd, rs1, rs2, funct3, funct7)
		e.advancePC(4)

	// ── OP-32 (R-type, word) ─────────────────────────────────────────
	case 0x3B:
		e.emitOp32(rd, rs1, rs2, funct3, funct7)
		e.advancePC(4)

	// ── FENCE (no-op) ────────────────────────────────────────────────
	case 0x0F:
		e.advancePC(4)

	// ── SYSTEM ───────────────────────────────────────────────────────
	case 0x73:
		switch insn {
		case 0x00000073: // ECALL
			e.advancePC(4)
			e.emitReturn(e.pc, jitEcall)
			e.terminated = true
		case 0x00100073: // EBREAK
			e.advancePC(4)
			e.emitReturn(e.pc, jitEbreak)
			e.terminated = true
		default:
			// CSR or unknown — end block before this instruction.
			// Don't advancePC: numInsns stays at previous count.
			// RunJIT will fall back to interpreter for this instruction.
			e.terminated = true
		}

	default:
		// Unknown opcode — end block before this instruction.
		e.terminated = true
	}
}

// ── OP-IMM (opcode 0x13) ────────────────────────────────────────────────

func (e *emitter) emitOpImm(rd, rs1 uint32, imm int64, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	s := e.rs(rs1)
	shamt := imm & 63

	switch funct3 {
	case 0: // ADDI
		e.emit("    %s = %s + %dLL;\n", d, s, imm)
	case 1: // SLLI
		e.emit("    %s = %s << %d;\n", d, s, shamt)
	case 2: // SLTI
		e.emit("    %s = ((int64_t)%s < %dLL) ? 1 : 0;\n", d, s, imm)
	case 3: // SLTIU
		e.emit("    %s = ((uint64_t)%s < (uint64_t)%dLL) ? 1 : 0;\n", d, s, imm)
	case 4: // XORI
		e.emit("    %s = %s ^ %dLL;\n", d, s, imm)
	case 5: // SRLI / SRAI
		if funct7&0x20 != 0 {
			e.emit("    %s = (uint64_t)((int64_t)%s >> %d);\n", d, s, shamt) // SRAI
		} else {
			e.emit("    %s = (uint64_t)%s >> %d;\n", d, s, shamt) // SRLI
		}
	case 6: // ORI
		e.emit("    %s = %s | %dLL;\n", d, s, imm)
	case 7: // ANDI
		e.emit("    %s = %s & %dLL;\n", d, s, imm)
	}
}

// ── OP-IMM-32 (opcode 0x1B) ─────────────────────────────────────────────

func (e *emitter) emitOpImm32(rd, rs1 uint32, imm int64, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	s := e.rs(rs1)
	shamt := imm & 31

	switch funct3 {
	case 0: // ADDIW
		e.emit("    %s = (int64_t)(int32_t)((int32_t)%s + %d);\n", d, s, int32(imm))
	case 1: // SLLIW
		e.emit("    %s = (int64_t)(int32_t)((uint32_t)%s << %d);\n", d, s, shamt)
	case 5: // SRLIW / SRAIW
		if funct7&0x20 != 0 {
			e.emit("    %s = (int64_t)((int32_t)%s >> %d);\n", d, s, shamt) // SRAIW
		} else {
			e.emit("    %s = (int64_t)(int32_t)((uint32_t)%s >> %d);\n", d, s, shamt) // SRLIW
		}
	}
}

// ── OP (opcode 0x33) ────────────────────────────────────────────────────

func (e *emitter) emitOp(rd, rs1, rs2, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	a := e.rs(rs1)
	b := e.rs(rs2)

	switch funct7 {
	case 0x00: // base RV64I
		switch funct3 {
		case 0:
			e.emit("    %s = %s + %s;\n", d, a, b)
		case 1:
			e.emit("    %s = %s << (%s & 63);\n", d, a, b)
		case 2:
			e.emit("    %s = ((int64_t)%s < (int64_t)%s) ? 1 : 0;\n", d, a, b)
		case 3:
			e.emit("    %s = ((uint64_t)%s < (uint64_t)%s) ? 1 : 0;\n", d, a, b)
		case 4:
			e.emit("    %s = %s ^ %s;\n", d, a, b)
		case 5:
			e.emit("    %s = (uint64_t)%s >> (%s & 63);\n", d, a, b)
		case 6:
			e.emit("    %s = %s | %s;\n", d, a, b)
		case 7:
			e.emit("    %s = %s & %s;\n", d, a, b)
		}
	case 0x20: // SUB / SRA / Zbb
		switch funct3 {
		case 0:
			e.emit("    %s = %s - %s;\n", d, a, b) // SUB
		case 5:
			e.emit("    %s = (uint64_t)((int64_t)%s >> (%s & 63));\n", d, a, b) // SRA
		case 4:
			e.emit("    %s = ~(%s ^ %s);\n", d, a, b) // XNOR
		case 6:
			e.emit("    %s = %s | ~%s;\n", d, a, b) // ORN
		case 7:
			e.emit("    %s = %s & ~%s;\n", d, a, b) // ANDN
		}
	case 0x01: // M extension
		switch funct3 {
		case 0:
			e.emit("    %s = (uint64_t)((int64_t)%s * (int64_t)%s);\n", d, a, b) // MUL
		case 1: // MULH
			e.emit("    %s = (uint64_t)((__int128)(int64_t)%s * (__int128)(int64_t)%s >> 64);\n", d, a, b)
		case 2: // MULHSU
			e.emit("    %s = (uint64_t)(((__int128)(int64_t)%s * (__int128)(uint64_t)%s) >> 64);\n", d, a, b)
		case 3: // MULHU
			e.emit("    %s = (uint64_t)((unsigned __int128)(uint64_t)%s * (unsigned __int128)(uint64_t)%s >> 64);\n", d, a, b)
		case 4: // DIV
			e.emit("    %s = (%s == 0) ? (uint64_t)-1 : ((int64_t)%s == -9223372036854775807LL-1 && (int64_t)%s == -1) ? %s : (uint64_t)((int64_t)%s / (int64_t)%s);\n",
				d, b, a, b, a, a, b)
		case 5: // DIVU
			e.emit("    %s = (%s == 0) ? (uint64_t)-1 : %s / %s;\n", d, b, a, b)
		case 6: // REM
			e.emit("    %s = (%s == 0) ? %s : ((int64_t)%s == -9223372036854775807LL-1 && (int64_t)%s == -1) ? 0 : (uint64_t)((int64_t)%s %% (int64_t)%s);\n",
				d, b, a, a, b, a, b)
		case 7: // REMU
			e.emit("    %s = (%s == 0) ? %s : %s %% %s;\n", d, b, a, a, b)
		}
	case 0x05: // MIN/MAX (Zbb) + CLMUL (Zbc)
		switch funct3 {
		case 4:
			e.emit("    %s = ((int64_t)%s < (int64_t)%s) ? %s : %s;\n", d, a, b, a, b) // MIN
		case 5:
			e.emit("    %s = (%s < %s) ? %s : %s;\n", d, a, b, a, b) // MINU
		case 6:
			e.emit("    %s = ((int64_t)%s > (int64_t)%s) ? %s : %s;\n", d, a, b, a, b) // MAX
		case 7:
			e.emit("    %s = (%s > %s) ? %s : %s;\n", d, a, b, a, b) // MAXU
		default:
			// CLMUL etc. — bail to interpreter
			e.emitReturn(e.pc, jitOK)
			e.terminated = true
		}
	case 0x07: // Zicond
		switch funct3 {
		case 5:
			e.emit("    %s = (%s == 0) ? 0 : %s;\n", d, b, a) // CZERO.EQZ
		case 7:
			e.emit("    %s = (%s != 0) ? 0 : %s;\n", d, b, a) // CZERO.NEZ
		}
	default:
		// Unknown funct7 — bail
		e.terminated = true
	}
}

// ── OP-32 (opcode 0x3B) ─────────────────────────────────────────────────

func (e *emitter) emitOp32(rd, rs1, rs2, funct3, funct7 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	a := e.rs(rs1)
	b := e.rs(rs2)

	switch funct7 {
	case 0x00:
		switch funct3 {
		case 0:
			e.emit("    %s = (int64_t)(int32_t)((int32_t)%s + (int32_t)%s);\n", d, a, b) // ADDW
		case 1:
			e.emit("    %s = (int64_t)(int32_t)((uint32_t)%s << (%s & 31));\n", d, a, b) // SLLW
		case 5:
			e.emit("    %s = (int64_t)(int32_t)((uint32_t)%s >> (%s & 31));\n", d, a, b) // SRLW
		}
	case 0x20:
		switch funct3 {
		case 0:
			e.emit("    %s = (int64_t)(int32_t)((int32_t)%s - (int32_t)%s);\n", d, a, b) // SUBW
		case 5:
			e.emit("    %s = (int64_t)((int32_t)%s >> (%s & 31));\n", d, a, b) // SRAW
		}
	case 0x01: // M extension (word)
		switch funct3 {
		case 0:
			e.emit("    %s = (int64_t)(int32_t)((int32_t)%s * (int32_t)%s);\n", d, a, b) // MULW
		case 4: // DIVW
			e.emit("    %s = ((int32_t)%s == 0) ? (uint64_t)-1 : ((int32_t)%s == -2147483647-1 && (int32_t)%s == -1) ? (int64_t)(int32_t)%s : (int64_t)((int32_t)%s / (int32_t)%s);\n",
				d, b, a, b, a, a, b)
		case 5: // DIVUW
			e.emit("    %s = ((uint32_t)%s == 0) ? (uint64_t)-1 : (int64_t)(int32_t)((uint32_t)%s / (uint32_t)%s);\n", d, b, a, b)
		case 6: // REMW
			e.emit("    %s = ((int32_t)%s == 0) ? (int64_t)(int32_t)%s : ((int32_t)%s == -2147483647-1 && (int32_t)%s == -1) ? 0 : (int64_t)((int32_t)%s %% (int32_t)%s);\n",
				d, b, a, a, b, a, b)
		case 7: // REMUW
			e.emit("    %s = ((uint32_t)%s == 0) ? (int64_t)(int32_t)%s : (int64_t)(int32_t)((uint32_t)%s %% (uint32_t)%s);\n", d, b, a, a, b)
		}
	default:
		e.terminated = true
	}
}

// ── LOAD ─────────────────────────────────────────────────────────────────

func (e *emitter) emitLoad(rd, rs1 uint32, imm int64, funct3 uint32) {
	width, ctype, signed_ := loadInfo(funct3)
	if ctype == "" {
		e.terminated = true
		return
	}

	e.emit("    { uint64_t addr = %s + %dLL;\n", e.rs(rs1), imm)
	e.emit("      if (__builtin_expect((addr & %d) | ((addr | (addr+%d)) & ~mem_mask), 0)) {\n",
		width-1, width-1)
	e.emitWriteBackAll()
	e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitLoadFault)

	if rd != 0 {
		if signed_ {
			e.emit("      %s = (int64_t)(*(%s*)(mem_base + (addr & mem_mask)));\n",
				e.rd(rd), ctype)
		} else {
			e.emit("      %s = (uint64_t)(*(%s*)(mem_base + (addr & mem_mask)));\n",
				e.rd(rd), ctype)
		}
	}
	e.emit("    }\n")
}

func loadInfo(funct3 uint32) (width uint64, ctype string, signed_ bool) {
	switch funct3 {
	case 0:
		return 1, "int8_t", true // LB
	case 1:
		return 2, "int16_t", true // LH
	case 2:
		return 4, "int32_t", true // LW
	case 3:
		return 8, "uint64_t", false // LD
	case 4:
		return 1, "uint8_t", false // LBU
	case 5:
		return 2, "uint16_t", false // LHU
	case 6:
		return 4, "uint32_t", false // LWU
	}
	return 0, "", false
}

// ── STORE ────────────────────────────────────────────────────────────────

func (e *emitter) emitStore(rs1, rs2 uint32, imm int64, funct3 uint32) {
	width, ctype := storeInfo(funct3)
	if ctype == "" {
		e.terminated = true
		return
	}

	e.emit("    { uint64_t addr = %s + %dLL;\n", e.rs(rs1), imm)
	e.emit("      if (__builtin_expect((addr & %d) | ((addr | (addr+%d)) & ~mem_mask), 0)) {\n",
		width-1, width-1)
	e.emitWriteBackAll()
	e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitStoreFault)
	e.emit("      *(%s*)(mem_base + (addr & mem_mask)) = (%s)%s; }\n",
		ctype, ctype, e.rs(rs2))
}

func storeInfo(funct3 uint32) (width uint64, ctype string) {
	switch funct3 {
	case 0:
		return 1, "uint8_t"
	case 1:
		return 2, "uint16_t"
	case 2:
		return 4, "uint32_t"
	case 3:
		return 8, "uint64_t"
	}
	return 0, ""
}

// ── RVC (compressed instructions) ───────────────────────────────────────

func (e *emitter) emitRVC(insn uint16) {
	// For now, bail on all compressed instructions — let interpreter handle.
	// Phase 2 will add RVC emission.
	e.emitLabel()
	e.emitReturn(e.pc, jitOK)
	e.terminated = true
}

// ── Immediate extraction helpers ────────────────────────────────────────

func jImm(insn uint32) int64 {
	// J-type: imm[20|10:1|11|19:12]
	b20 := (insn >> 31) & 1
	b10_1 := (insn >> 21) & 0x3FF
	b11 := (insn >> 20) & 1
	b19_12 := (insn >> 12) & 0xFF
	raw := b20<<20 | b19_12<<12 | b11<<11 | b10_1<<1
	// Sign-extend from bit 20
	if b20 != 0 {
		raw |= 0xFFF00000
	}
	return int64(int32(raw))
}

func bImm(insn uint32) int64 {
	// B-type: imm[12|10:5|4:1|11]
	b12 := (insn >> 31) & 1
	b10_5 := (insn >> 25) & 0x3F
	b4_1 := (insn >> 8) & 0xF
	b11 := (insn >> 7) & 1
	raw := b12<<12 | b11<<11 | b10_5<<5 | b4_1<<1
	if b12 != 0 {
		raw |= 0xFFFFE000
	}
	return int64(int32(raw))
}

func sImm(insn uint32) int64 {
	// S-type: imm[11:5] in bits[31:25], imm[4:0] in bits[11:7]
	hi := int32(insn) >> 25
	lo := int32((insn >> 7) & 0x1F)
	return int64(hi<<5 | lo)
}

func branchCmp(funct3 uint32) string {
	switch funct3 {
	case 0:
		return "=="
	case 1:
		return "!="
	case 4:
		return "<"
	case 5:
		return ">="
	case 6:
		return "<"
	case 7:
		return ">="
	}
	return ""
}

// rsC returns the register expression with appropriate cast for branch comparison.
func (e *emitter) rsC(r, funct3 uint32) string {
	s := e.rs(r)
	if funct3 >= 4 && funct3 <= 5 {
		return "(int64_t)" + s // signed comparison
	}
	return s // unsigned or equality
}

// ── Finalize: wrap body in a complete C function ────────────────────────

func (e *emitter) finalize() *emitResult {
	var out strings.Builder

	// Header — define types directly to avoid needing system headers in TCC.
	// Also provide memset (TCC needs it for struct initialization on macOS).
	out.WriteString("typedef unsigned long long uint64_t;\n")
	out.WriteString("typedef long long int64_t;\n")
	out.WriteString("typedef unsigned int uint32_t;\n")
	out.WriteString("typedef int int32_t;\n")
	out.WriteString("typedef unsigned short uint16_t;\n")
	out.WriteString("typedef short int16_t;\n")
	out.WriteString("typedef unsigned char uint8_t;\n")
	out.WriteString("typedef signed char int8_t;\n")
	out.WriteString("void *memset(void *s, int c, unsigned long n) { char *p=s; while(n--) *p++=c; return s; }\n")
	out.WriteString("typedef struct { uint64_t pc; uint64_t ic; uint64_t status; uint64_t fault_addr; } JITResult;\n\n")
	out.WriteString("JITResult block_entry(uint64_t *x, uint64_t *f, uint32_t *fcsr,\n")
	out.WriteString("                      char *mem_base, uint64_t mem_mask) {\n")

	// Declare cached register variables
	for i := 1; i < 32; i++ {
		if e.regsUsed[i] {
			fmt.Fprintf(&out, "    uint64_t r%d = x[%d];\n", i, i)
		}
	}
	out.WriteString("    uint64_t ic = 0;\n\n")

	// Body
	out.WriteString(e.body.String())

	// Fall-through return: always present so bail-out blocks don't fall off the end.
	// For blocks that already have an explicit return (JAL, ECALL), this is dead code.
	for i := 1; i < 32; i++ {
		if e.regsUsed[i] {
			fmt.Fprintf(&out, "    x[%d] = r%d;\n", i, i)
		}
	}
	fmt.Fprintf(&out, "    return (JITResult){0x%xULL, ic, 0, 0};\n", e.pc)

	out.WriteString("}\n")

	return &emitResult{
		csrc:     out.String(),
		startPC:  e.startPC,
		endPC:    e.pc,
		numInsns: e.numInsns,
		regsUsed: e.regsUsed,
	}
}
