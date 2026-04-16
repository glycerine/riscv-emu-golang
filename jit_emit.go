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

// ── Region pre-scan (Phase 2: block chaining) ─────────────────────────

type flowClass int

const (
	flowSeq    flowClass = iota // sequential: next = pc + insnSize
	flowBranch                  // conditional: next = {pc + insnSize, target}
	flowJump                    // unconditional J/C.J rd==0: next = target only
	flowTerm                    // no successors (JALR, JAL rd!=0, ECALL, CSR)
)

// classifyFlow determines the control-flow type of the instruction at pc.
// Returns the flow class, branch/jump target (if any), and instruction size.
// Only decodes enough to identify control flow — NOT full semantics.
func classifyFlow(mem *GuestMemory, pc uint64) (flowClass, uint64, uint64) {
	half, f := mem.Fetch16(pc)
	if f != nil {
		return flowTerm, 0, 0 // can't fetch — treat as terminator
	}

	if half&0x3 != 0x3 {
		// 16-bit RVC
		insn := uint16(half)
		quad := insn & 0x3
		funct3 := insn >> 13

		switch quad {
		case 0x1: // Quadrant 1
			switch funct3 {
			case 0b101: // C.J
				return flowJump, pc + uint64(rvcJOffset(insn)), 2
			case 0b110: // C.BEQZ
				return flowBranch, pc + uint64(rvcBOffset(insn)), 2
			case 0b111: // C.BNEZ
				return flowBranch, pc + uint64(rvcBOffset(insn)), 2
			}
		case 0x2: // Quadrant 2
			if funct3 == 0b100 {
				rd := (insn >> 7) & 0x1F
				rs2 := (insn >> 2) & 0x1F
				bit12 := (insn >> 12) & 1
				if bit12 == 0 && rs2 == 0 { // C.JR
					return flowTerm, 0, 2
				}
				if bit12 == 1 && rs2 == 0 { // C.JALR or C.EBREAK
					_ = rd
					return flowTerm, 0, 2
				}
			}
		}
		return flowSeq, 0, 2
	}

	// 32-bit instruction
	insn, f2 := mem.Fetch32(pc)
	if f2 != nil {
		insn, f2 = mem.Fetch32U(pc)
		if f2 != nil {
			return flowTerm, 0, 0
		}
	}

	opcode := insn & 0x7F
	rd := (insn >> 7) & 0x1F

	switch opcode {
	case 0x63: // BRANCH
		return flowBranch, pc + uint64(bImm(insn)), 4
	case 0x6F: // JAL
		if rd == 0 {
			return flowJump, pc + uint64(jImm(insn)), 4
		}
		return flowTerm, 0, 4 // function call
	case 0x67: // JALR
		return flowTerm, 0, 4
	case 0x73: // SYSTEM (ECALL, EBREAK, CSR)
		return flowTerm, 0, 4
	default:
		return flowSeq, 0, 4
	}
}

// regionInfo describes the extent of a JIT block region.
type regionInfo struct {
	endPC   uint64 // exclusive: first PC past the region
	pcCount int    // number of distinct PCs in the region
}

// scanRegion does a BFS over the control flow graph starting at entryPC
// to determine the region of code that should be emitted as a single block.
func scanRegion(mem *GuestMemory, entryPC uint64) regionInfo {
	const maxInsns = 2048
	const maxRange = 16384

	visited := make(map[uint64]bool)
	worklist := []uint64{entryPC}
	maxEnd := entryPC

	for len(worklist) > 0 && len(visited) < maxInsns {
		pc := worklist[0]
		worklist = worklist[1:]

		if visited[pc] {
			continue
		}
		if pc < entryPC || pc > entryPC+maxRange {
			continue
		}

		fc, target, insnSize := classifyFlow(mem, pc)
		if insnSize == 0 {
			continue // fetch failed
		}

		visited[pc] = true
		if end := pc + insnSize; end > maxEnd {
			maxEnd = end
		}

		switch fc {
		case flowSeq:
			worklist = append(worklist, pc+insnSize)
		case flowBranch:
			worklist = append(worklist, pc+insnSize)
			if target >= entryPC && target <= entryPC+maxRange {
				worklist = append(worklist, target)
			}
		case flowJump:
			if target >= entryPC && target <= entryPC+maxRange {
				worklist = append(worklist, target)
			}
		case flowTerm:
			// no successors
		}
	}

	return regionInfo{endPC: maxEnd, pcCount: len(visited)}
}

// emitBlock translates a basic block starting at pc into C source.
// Uses a two-phase approach: scan the region, then emit all instructions.
func emitBlock(mem *GuestMemory, pc uint64) *emitResult {
	// Phase 1: pre-scan to determine region extent.
	region := scanRegion(mem, pc)

	e := &emitter{
		mem:         mem,
		startPC:     pc,
		pc:          pc,
		body:        strings.Builder{},
		visited:     make(map[uint64]bool),
		regionEnd:   region.endPC,
		gotoTargets: make(map[uint64]bool),
	}

	// Phase 2: emit all instructions from startPC to regionEnd.
	for e.numInsns < 2048 && !e.terminated && e.pc < e.regionEnd {
		// Cycle detection: if we've already emitted code at this PC
		// (e.g., a forward J landed on a PC we passed through), emit
		// a goto to the existing label and stop.
		if e.visited[e.pc] {
			e.emit("    goto b_%x;\n", e.pc)
			e.gotoTargets[e.pc] = true
			e.terminated = true
			break
		}
		e.visited[e.pc] = true

		// Fetch instruction (handle RVC)
		half, fh := mem.Fetch16(e.pc)
		if fh != nil {
			break // can't fetch — end block
		}

		if half&0x3 != 0x3 {
			// 16-bit compressed instruction
			e.emitRVC(uint16(half))
		} else {
			// 32-bit instruction — fall back to unaligned fetch if needed
			insn, f := mem.Fetch32(e.pc)
			if f != nil {
				if f.Kind == FaultMisalign {
					insn, f = mem.Fetch32U(e.pc)
				}
				if f != nil {
					break
				}
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
	// Visited PCs — cycle detection for jump following (Phase 1 chaining)
	visited map[uint64]bool
	// Region pre-scan results (Phase 2 chaining)
	regionEnd   uint64          // exclusive end PC from scanRegion
	gotoTargets map[uint64]bool // PCs referenced by goto (for bail labels in finalize)
	// FP / extension flags — control conditional header emission
	usesFP         bool // any FP instruction emitted
	usesZbbHelpers bool // CLZ/CTZ/CPOP emitted (need inline helpers)
	// icEmitted is set by emitBranch to indicate it already emitted ic++.
	// advancePC checks this to avoid double-counting.
	icEmitted bool
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

// advancePC marks an instruction as consumed and moves to the next PC.
// Emits ic++ in the generated C so the instruction count stays accurate,
// unless emitBranch already emitted ic++ (indicated by icEmitted flag).
func (e *emitter) advancePC(size uint64) {
	e.numInsns++
	e.pc += size
	if e.icEmitted {
		e.icEmitted = false // consumed
	} else {
		e.emit("    ic++;\n")
	}
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
		e.emitJAL(rd, jimm, 4)

	// ── JALR ─────────────────────────────────────────────────────────
	case 0x67:
		e.emitJALR(rd, rs1, iimm, 4)

	// ── BRANCH ───────────────────────────────────────────────────────
	case 0x63:
		bimm := bImm(insn)
		cmp := branchCmp(funct3)
		if cmp == "" {
			e.terminated = true
			e.advancePC(4)
			break
		}
		e.emitBranch(rs1, rs2, funct3, bimm)
		e.advancePC(4) // icEmitted flag prevents double ic++

	// ── LOAD ─────────────────────────────────────────────────────────
	case 0x03:
		e.emitLoad(rd, rs1, iimm, funct3)
		if !e.terminated { e.advancePC(4) }

	// ── STORE ────────────────────────────────────────────────────────
	case 0x23:
		simm := sImm(insn)
		e.emitStore(rs1, rs2, simm, funct3)
		if !e.terminated { e.advancePC(4) }

	// ── OP-IMM ───────────────────────────────────────────────────────
	case 0x13:
		e.emitOpImm(rd, rs1, iimm, funct3, funct7)
		if !e.terminated { e.advancePC(4) }

	// ── OP-IMM-32 ────────────────────────────────────────────────────
	case 0x1B:
		e.emitOpImm32(rd, rs1, iimm, funct3, funct7)
		if !e.terminated { e.advancePC(4) }

	// ── OP (R-type) ──────────────────────────────────────────────────
	case 0x33:
		e.emitOp(rd, rs1, rs2, funct3, funct7)
		if !e.terminated { e.advancePC(4) }

	// ── OP-32 (R-type, word) ─────────────────────────────────────────
	case 0x3B:
		e.emitOp32(rd, rs1, rs2, funct3, funct7)
		if !e.terminated { e.advancePC(4) }

	// ── FP LOAD ──────────────────────────────────────────────────────
	case 0x07:
		e.emitFPLoad(rd, rs1, iimm, funct3)
		if !e.terminated {
			e.advancePC(4)
		}

	// ── FP STORE ─────────────────────────────────────────────────────
	case 0x27:
		simm := sImm(insn)
		e.emitFPStore(rs1, rs2, simm, funct3)
		if !e.terminated {
			e.advancePC(4)
		}

	// ── FMADD / FMSUB / FNMSUB / FNMADD ─────────────────────────────
	case 0x43, 0x47, 0x4B, 0x4F:
		rs3 := insn >> 27
		fpfmt := (insn >> 25) & 0x3
		e.emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt)
		if !e.terminated {
			e.advancePC(4)
		}

	// ── FP-OP ────────────────────────────────────────────────────────
	case 0x53:
		funct5 := insn >> 27
		fpfmt := (insn >> 25) & 0x3
		e.emitFPOp(rd, rs1, rs2, funct3, funct5, fpfmt)
		if !e.terminated {
			e.advancePC(4)
		}

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
		if imm == 0 {
			if rs1 == 0 {
				e.emit("    %s = 0;\n", d) // LI rd, 0
			} else {
				e.emit("    %s = %s;\n", d, s) // MV rd, rs1
			}
		} else if rs1 == 0 {
			e.emit("    %s = %dLL;\n", d, imm) // LI rd, imm
		} else {
			e.emit("    %s = %s + %dLL;\n", d, s, imm) // ADDI
		}
	case 1: // SLLI / BSETI / BCLRI / BINVI / CLZ/CTZ/CPOP/SEXT.B/SEXT.H
		funct6 := funct7 >> 1 // bits[31:26], mask out shamt[5]
		switch funct6 {
		case 0x00: // SLLI
			e.emit("    %s = %s << %d;\n", d, s, shamt)
		case 0x0A: // BSETI
			e.emit("    %s = %s | (1ULL << %d);\n", d, s, shamt)
		case 0x12: // BCLRI
			e.emit("    %s = %s & ~(1ULL << %d);\n", d, s, shamt)
		case 0x1A: // BINVI
			e.emit("    %s = %s ^ (1ULL << %d);\n", d, s, shamt)
		case 0x30: // CLZ/CTZ/CPOP/SEXT.B/SEXT.H
			e.usesZbbHelpers = true
			switch shamt {
			case 0:
				e.emit("    %s = jit_clz64(%s);\n", d, s)
			case 1:
				e.emit("    %s = jit_ctz64(%s);\n", d, s)
			case 2:
				e.emit("    %s = jit_cpop64(%s);\n", d, s)
			case 0x22: // SEXT.B (rs2 field = 0x22)
				e.emit("    %s = (int64_t)(int8_t)%s;\n", d, s)
			case 0x23: // SEXT.H (rs2 field = 0x23)
				e.emit("    %s = (int64_t)(int16_t)%s;\n", d, s)
			default:
				e.terminated = true
			}
		default:
			e.terminated = true
		}
	case 2: // SLTI
		e.emit("    %s = ((int64_t)%s < %dLL) ? 1 : 0;\n", d, s, imm)
	case 3: // SLTIU
		e.emit("    %s = ((uint64_t)%s < (uint64_t)%dLL) ? 1 : 0;\n", d, s, imm)
	case 4: // XORI
		e.emit("    %s = %s ^ %dLL;\n", d, s, imm)
	case 5: // SRLI/SRAI / BEXTI / RORI / ORC.B / REV8 / ZEXT.H
		funct6 := funct7 >> 1
		switch funct6 {
		case 0x00: // SRLI
			e.emit("    %s = (uint64_t)%s >> %d;\n", d, s, shamt)
		case 0x10: // SRAI
			e.emit("    %s = (uint64_t)((int64_t)%s >> %d);\n", d, s, shamt)
		case 0x12: // BEXTI
			e.emit("    %s = (%s >> %d) & 1;\n", d, s, shamt)
		case 0x18: // RORI
			e.emit("    %s = (%s >> %d) | (%s << %d);\n", d, s, shamt, s, 64-shamt)
		case 0x0A: // ORC.B
			e.emit("    { uint64_t v_ = %s;\n", s)
			e.emit("      v_ |= v_ << 1; v_ |= v_ << 2; v_ |= v_ << 4;\n")
			e.emit("      %s = v_ & 0x0101010101010101ULL; %s *= 0xFF; }\n", d, d)
		case 0x1A: // REV8
			e.emit("    { uint64_t v_ = %s;\n", s)
			e.emit("      v_ = ((v_ & 0x00FF00FF00FF00FFULL) << 8) | ((v_ & 0xFF00FF00FF00FF00ULL) >> 8);\n")
			e.emit("      v_ = ((v_ & 0x0000FFFF0000FFFFULL) << 16) | ((v_ & 0xFFFF0000FFFF0000ULL) >> 16);\n")
			e.emit("      %s = (v_ << 32) | (v_ >> 32); }\n", d)
		case 0x02: // ZEXT.H
			e.emit("    %s = %s & 0xFFFFULL;\n", d, s)
		default:
			e.terminated = true
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
		if imm == 0 {
			if rs1 == 0 {
				e.emit("    %s = 0;\n", d)
			} else {
				e.emit("    %s = (int64_t)(int32_t)%s;\n", d, s) // SEXT.W
			}
		} else {
			e.emit("    %s = (int64_t)(int32_t)((int32_t)%s + %d);\n", d, s, int32(imm))
		}
	case 1: // SLLIW / SLLI.UW
		if funct7 == 0x04 { // SLLI.UW
			e.emit("    %s = (uint64_t)(uint32_t)%s << %d;\n", d, s, shamt)
		} else {
			e.emit("    %s = (int64_t)(int32_t)((uint32_t)%s << %d);\n", d, s, shamt) // SLLIW
		}
	case 5: // SRLIW / SRAIW / RORIW
		switch funct7 >> 1 {
		case 0x00: // SRLIW
			e.emit("    %s = (int64_t)(int32_t)((uint32_t)%s >> %d);\n", d, s, shamt)
		case 0x10: // SRAIW
			e.emit("    %s = (int64_t)((int32_t)%s >> %d);\n", d, s, shamt)
		case 0x30: // RORIW
			e.emit("    { uint32_t a_ = (uint32_t)%s; %s = (int64_t)(int32_t)((a_ >> %d) | (a_ << %d)); }\n",
				s, d, shamt, 32-shamt)
		default:
			e.terminated = true
		}
	default:
		e.terminated = true
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
		case 1: // MULH — bail: TCC has no __int128
			e.terminated = true
			return
		case 2: // MULHSU — bail: TCC has no __int128
			e.terminated = true
			return
		case 3: // MULHU — bail: TCC has no __int128
			e.terminated = true
			return
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
	case 0x04: // Zbb: ZEXT.H
		e.emit("    %s = %s & 0xFFFFULL;\n", d, a)
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
			e.terminated = true
		}
	case 0x07: // Zicond
		switch funct3 {
		case 5:
			e.emit("    %s = (%s == 0) ? 0 : %s;\n", d, b, a) // CZERO.EQZ
		case 7:
			e.emit("    %s = (%s != 0) ? 0 : %s;\n", d, b, a) // CZERO.NEZ
		default:
			e.terminated = true
		}
	case 0x10: // Zba: SH1ADD/SH2ADD/SH3ADD
		switch funct3 {
		case 2:
			e.emit("    %s = %s + (%s << 1);\n", d, b, a) // SH1ADD
		case 4:
			e.emit("    %s = %s + (%s << 2);\n", d, b, a) // SH2ADD
		case 6:
			e.emit("    %s = %s + (%s << 3);\n", d, b, a) // SH3ADD
		default:
			e.terminated = true
		}
	case 0x14: // Zbs: BSET / Zbb: ORC.B
		switch funct3 {
		case 1: // BSET
			e.emit("    %s = %s | (1ULL << (%s & 63));\n", d, a, b)
		case 5: // ORC.B
			e.emit("    { uint64_t v_ = %s;\n", a)
			e.emit("      v_ |= v_ << 1; v_ |= v_ << 2; v_ |= v_ << 4;\n")
			e.emit("      %s = v_ & 0x0101010101010101ULL; %s *= 0xFF; }\n", d, d)
		default:
			e.terminated = true
		}
	case 0x24: // Zbs: BCLR/BEXT
		switch funct3 {
		case 1:
			e.emit("    %s = %s & ~(1ULL << (%s & 63));\n", d, a, b) // BCLR
		case 5:
			e.emit("    %s = (%s >> (%s & 63)) & 1;\n", d, a, b) // BEXT
		default:
			e.terminated = true
		}
	case 0x30: // Zbb: ROL/ROR
		switch funct3 {
		case 1: // ROL
			e.emit("    { uint64_t s_ = %s & 63; %s = (%s << s_) | (%s >> (64-s_)); }\n", b, d, a, a)
		case 5: // ROR
			e.emit("    { uint64_t s_ = %s & 63; %s = (%s >> s_) | (%s << (64-s_)); }\n", b, d, a, a)
		default:
			e.terminated = true
		}
	case 0x34: // Zbs: BINV
		e.emit("    %s = %s ^ (1ULL << (%s & 63));\n", d, a, b)
	case 0x35: // Zbb: REV8
		e.emit("    { uint64_t v_ = %s;\n", a)
		e.emit("      v_ = ((v_ & 0x00FF00FF00FF00FFULL) << 8) | ((v_ & 0xFF00FF00FF00FF00ULL) >> 8);\n")
		e.emit("      v_ = ((v_ & 0x0000FFFF0000FFFFULL) << 16) | ((v_ & 0xFFFF0000FFFF0000ULL) >> 16);\n")
		e.emit("      %s = (v_ << 32) | (v_ >> 32); }\n", d)
	case 0x60: // Zbb: CLZ/CTZ/CPOP
		e.usesZbbHelpers = true
		switch rs2 {
		case 0:
			e.emit("    %s = jit_clz64(%s);\n", d, a) // CLZ
		case 1:
			e.emit("    %s = jit_ctz64(%s);\n", d, a) // CTZ
		case 2:
			e.emit("    %s = jit_cpop64(%s);\n", d, a) // CPOP
		default:
			e.terminated = true
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
	case 0x04: // Zba: ADD.UW
		e.emit("    %s = %s + (uint64_t)(uint32_t)%s;\n", d, b, a)
	case 0x10: // Zba: SH1ADD.UW / SH2ADD.UW / SH3ADD.UW
		switch funct3 {
		case 2:
			e.emit("    %s = %s + ((uint64_t)(uint32_t)%s << 1);\n", d, b, a)
		case 4:
			e.emit("    %s = %s + ((uint64_t)(uint32_t)%s << 2);\n", d, b, a)
		case 6:
			e.emit("    %s = %s + ((uint64_t)(uint32_t)%s << 3);\n", d, b, a)
		default:
			e.terminated = true
		}
	case 0x30: // Zbb: ROLW/RORW
		switch funct3 {
		case 1: // ROLW
			e.emit("    { uint32_t s_ = (uint32_t)%s & 31; %s = (int64_t)(int32_t)(((uint32_t)%s << s_) | ((uint32_t)%s >> (32-s_))); }\n",
				b, d, a, a)
		case 5: // RORW
			e.emit("    { uint32_t s_ = (uint32_t)%s & 31; %s = (int64_t)(int32_t)(((uint32_t)%s >> s_) | ((uint32_t)%s << (32-s_))); }\n",
				b, d, a, a)
		default:
			e.terminated = true
		}
	case 0x60: // Zbb: CLZW/CTZW/CPOPW
		e.usesZbbHelpers = true
		switch rs2 {
		case 0:
			e.emit("    %s = jit_clz32((uint32_t)%s);\n", d, a)
		case 1:
			e.emit("    %s = jit_ctz32((uint32_t)%s);\n", d, a)
		case 2:
			e.emit("    %s = jit_cpop32((uint32_t)%s);\n", d, a)
		default:
			e.terminated = true
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
	// OOB check only (NOT alignment) — misaligned loads are handled below.
	e.emit("      if (__builtin_expect((addr | (addr+%d)) & ~mem_mask, 0)) {\n", width-1)
	e.emitWriteBackAll()
	e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitLoadFault)

	if rd != 0 {
		if width == 1 {
			// Byte loads never misalign.
			if signed_ {
				e.emit("      %s = (int64_t)(*(%s*)(mem_base + (addr & mem_mask)));\n", e.rd(rd), ctype)
			} else {
				e.emit("      %s = (uint64_t)(*(%s*)(mem_base + (addr & mem_mask)));\n", e.rd(rd), ctype)
			}
		} else {
			d := e.rd(rd)
			// Fast path: aligned access.
			e.emit("      if (__builtin_expect(addr & %d, 0)) {\n", width-1)
			// Slow path: misaligned — byte-by-byte little-endian load.
			e.emit("        uint64_t v_ = 0;\n")
			e.emit("        for (int i_ = 0; i_ < %d; i_++) v_ |= (uint64_t)*(uint8_t*)(mem_base + ((addr+i_) & mem_mask)) << (i_*8);\n", width)
			if signed_ {
				// Sign-extend from width to 64 bits.
				shift := 64 - width*8
				e.emit("        %s = (int64_t)(v_ << %d) >> %d;\n", d, shift, shift)
			} else {
				e.emit("        %s = v_;\n", d)
			}
			e.emit("      } else {\n")
			if signed_ {
				e.emit("        %s = (int64_t)(*(%s*)(mem_base + (addr & mem_mask)));\n", d, ctype)
			} else {
				e.emit("        %s = (uint64_t)(*(%s*)(mem_base + (addr & mem_mask)));\n", d, ctype)
			}
			e.emit("      }\n")
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

	s := e.rs(rs2)
	e.emit("    { uint64_t addr = %s + %dLL;\n", e.rs(rs1), imm)
	// OOB check only — misaligned stores handled below.
	e.emit("      if (__builtin_expect((addr | (addr+%d)) & ~mem_mask, 0)) {\n", width-1)
	e.emitWriteBackAll()
	e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitStoreFault)

	if width == 1 {
		e.emit("      *(uint8_t*)(mem_base + (addr & mem_mask)) = (uint8_t)%s; }\n", s)
	} else {
		e.emit("      if (__builtin_expect(addr & %d, 0)) {\n", width-1)
		// Misaligned: byte-by-byte little-endian store.
		e.emit("        uint64_t v_ = (uint64_t)(%s)%s;\n", ctype, s)
		e.emit("        for (int i_ = 0; i_ < %d; i_++) *(uint8_t*)(mem_base + ((addr+i_) & mem_mask)) = (uint8_t)(v_ >> (i_*8));\n", width)
		e.emit("      } else {\n")
		e.emit("        *(%s*)(mem_base + (addr & mem_mask)) = (%s)%s;\n", ctype, ctype, s)
		e.emit("      } }\n")
	}
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

// ── FP LOAD (opcode 0x07) ───────────────────────────────────────────────

func (e *emitter) emitFPLoad(rd, rs1 uint32, imm int64, funct3 uint32) {
	e.usesFP = true
	var width uint64
	switch funct3 {
	case 2: // FLW
		width = 4
	case 3: // FLD
		width = 8
	default:
		e.terminated = true
		return
	}
	e.emit("    { uint64_t addr = %s + %dLL;\n", e.rs(rs1), imm)
	e.emit("      if (__builtin_expect((addr & %d) | ((addr | (addr+%d)) & ~mem_mask), 0)) {\n",
		width-1, width-1)
	e.emitWriteBackAll()
	e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitLoadFault)
	if funct3 == 2 { // FLW: NaN-box
		e.emit("      f[%d] = box_f32(*(uint32_t*)(mem_base + (addr & mem_mask))); }\n", rd)
	} else { // FLD
		e.emit("      f[%d] = *(uint64_t*)(mem_base + (addr & mem_mask)); }\n", rd)
	}
}

// ── FP STORE (opcode 0x27) ──────────────────────────────────────────────

func (e *emitter) emitFPStore(rs1, rs2 uint32, imm int64, funct3 uint32) {
	e.usesFP = true
	var width uint64
	switch funct3 {
	case 2: // FSW
		width = 4
	case 3: // FSD
		width = 8
	default:
		e.terminated = true
		return
	}
	e.emit("    { uint64_t addr = %s + %dLL;\n", e.rs(rs1), imm)
	e.emit("      if (__builtin_expect((addr & %d) | ((addr | (addr+%d)) & ~mem_mask), 0)) {\n",
		width-1, width-1)
	e.emitWriteBackAll()
	e.emit("        return (JITResult){0x%xULL, ic, %d, addr}; }\n", e.pc, jitStoreFault)
	if funct3 == 2 { // FSW
		e.emit("      *(uint32_t*)(mem_base + (addr & mem_mask)) = (uint32_t)f[%d]; }\n", rs2)
	} else { // FSD
		e.emit("      *(uint64_t*)(mem_base + (addr & mem_mask)) = f[%d]; }\n", rs2)
	}
}

// ── FMADD family (opcodes 0x43, 0x47, 0x4B, 0x4F) ─────────────────────

func (e *emitter) emitFMA(opcode, rd, rs1, rs2, rs3, fpfmt uint32) {
	e.usesFP = true
	if fpfmt > 1 {
		e.terminated = true
		return
	}

	// Build the expression: ±(a * b) ± c
	var neg, sub string
	switch opcode {
	case 0x43: // FMADD:  a*b + c
	case 0x47: // FMSUB:  a*b - c
		sub = "-"
	case 0x4B: // FNMSUB: -(a*b) + c
		neg = "-"
	case 0x4F: // FNMADD: -(a*b) - c
		neg = "-"
		sub = "-"
	}
	if sub == "" {
		sub = "+"
	}

	if fpfmt == 0 { // single
		e.emit("    wr_f32(f, %d, %s(rd_f32(f,%d) * rd_f32(f,%d)) %s rd_f32(f,%d));\n",
			rd, neg, rs1, rs2, sub, rs3)
	} else { // double
		e.emit("    wr_f64(f, %d, %s(rd_f64(f,%d) * rd_f64(f,%d)) %s rd_f64(f,%d));\n",
			rd, neg, rs1, rs2, sub, rs3)
	}
}

// ── FP-OP (opcode 0x53) ────────────────────────────────────────────────

func (e *emitter) emitFPOp(rd, rs1, rs2, funct3, funct5, fpfmt uint32) {
	e.usesFP = true
	if fpfmt == 0 {
		e.emitFPOpS(rd, rs1, rs2, funct3, funct5)
	} else if fpfmt == 1 {
		e.emitFPOpD(rd, rs1, rs2, funct3, funct5)
	} else {
		e.terminated = true
	}
}

func (e *emitter) emitFPOpS(rd, rs1, rs2, funct3, funct5 uint32) {
	switch funct5 {
	case 0x00: // FADD.S
		e.emit("    wr_f32(f, %d, rd_f32(f,%d) + rd_f32(f,%d));\n", rd, rs1, rs2)
	case 0x01: // FSUB.S
		e.emit("    wr_f32(f, %d, rd_f32(f,%d) - rd_f32(f,%d));\n", rd, rs1, rs2)
	case 0x02: // FMUL.S
		e.emit("    wr_f32(f, %d, rd_f32(f,%d) * rd_f32(f,%d));\n", rd, rs1, rs2)
	case 0x03: // FDIV.S
		e.emit("    wr_f32(f, %d, rd_f32(f,%d) / rd_f32(f,%d));\n", rd, rs1, rs2)
	case 0x0B: // FSQRT.S
		e.emit("    wr_f32(f, %d, jit_sqrtf(rd_f32(f,%d)));\n", rd, rs1)
	case 0x04: // FSGNJ.S / FSGNJN.S / FSGNJX.S
		e.emitFsgnjS(rd, rs1, rs2, funct3)
	case 0x05: // FMIN.S / FMAX.S
		switch funct3 {
		case 0:
			e.emit("    wr_f32(f, %d, jit_fminf(rd_f32(f,%d), rd_f32(f,%d)));\n", rd, rs1, rs2)
		case 1:
			e.emit("    wr_f32(f, %d, jit_fmaxf(rd_f32(f,%d), rd_f32(f,%d)));\n", rd, rs1, rs2)
		default:
			e.terminated = true
		}
	case 0x08: // FCVT.S.D (from double to single, fpfmt=0, rs2=1)
		e.emit("    wr_f32(f, %d, (float)rd_f64(f,%d));\n", rd, rs1)
	case 0x14: // FEQ.S / FLT.S / FLE.S — writes to integer rd
		e.emitFcmpS(rd, rs1, rs2, funct3)
	case 0x18: // FCVT.{W,WU,L,LU}.S — float→int
		e.emitFcvtToIntS(rd, rs1, rs2)
	case 0x1A: // FCVT.S.{W,WU,L,LU} — int→float
		e.emitFcvtFromIntS(rd, rs1, rs2)
	case 0x1C: // FMV.X.W / FCLASS.S
		switch funct3 {
		case 0: // FMV.X.W
			if rd != 0 {
				e.emit("    %s = (int64_t)(int32_t)(uint32_t)f[%d];\n", e.rd(rd), rs1)
			}
		default: // FCLASS.S — bail
			e.terminated = true
		}
	case 0x1E: // FMV.W.X
		e.emit("    f[%d] = box_f32((uint32_t)%s);\n", rd, e.rs(rs1))
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFPOpD(rd, rs1, rs2, funct3, funct5 uint32) {
	switch funct5 {
	case 0x00: // FADD.D
		e.emit("    wr_f64(f, %d, rd_f64(f,%d) + rd_f64(f,%d));\n", rd, rs1, rs2)
	case 0x01: // FSUB.D
		e.emit("    wr_f64(f, %d, rd_f64(f,%d) - rd_f64(f,%d));\n", rd, rs1, rs2)
	case 0x02: // FMUL.D
		e.emit("    wr_f64(f, %d, rd_f64(f,%d) * rd_f64(f,%d));\n", rd, rs1, rs2)
	case 0x03: // FDIV.D
		e.emit("    wr_f64(f, %d, rd_f64(f,%d) / rd_f64(f,%d));\n", rd, rs1, rs2)
	case 0x0B: // FSQRT.D
		e.emit("    wr_f64(f, %d, jit_sqrt(rd_f64(f,%d)));\n", rd, rs1)
	case 0x04: // FSGNJ.D / FSGNJN.D / FSGNJX.D
		e.emitFsgnjD(rd, rs1, rs2, funct3)
	case 0x05: // FMIN.D / FMAX.D
		switch funct3 {
		case 0:
			e.emit("    wr_f64(f, %d, jit_fmin(rd_f64(f,%d), rd_f64(f,%d)));\n", rd, rs1, rs2)
		case 1:
			e.emit("    wr_f64(f, %d, jit_fmax(rd_f64(f,%d), rd_f64(f,%d)));\n", rd, rs1, rs2)
		default:
			e.terminated = true
		}
	case 0x08: // FCVT.D.S (from single to double, fpfmt=1, rs2=0)
		e.emit("    wr_f64(f, %d, (double)rd_f32(f,%d));\n", rd, rs1)
	case 0x14: // FEQ.D / FLT.D / FLE.D
		e.emitFcmpD(rd, rs1, rs2, funct3)
	case 0x18: // FCVT.{W,WU,L,LU}.D — double→int
		e.emitFcvtToIntD(rd, rs1, rs2)
	case 0x1A: // FCVT.D.{W,WU,L,LU} — int→double
		e.emitFcvtFromIntD(rd, rs1, rs2)
	case 0x1C: // FMV.X.D / FCLASS.D
		switch funct3 {
		case 0: // FMV.X.D
			if rd != 0 {
				e.emit("    %s = f[%d];\n", e.rd(rd), rs1)
			}
		default: // FCLASS.D — bail
			e.terminated = true
		}
	case 0x1E: // FMV.D.X
		e.emit("    f[%d] = %s;\n", rd, e.rs(rs1))
	default:
		e.terminated = true
	}
}

// ── FP sign injection helpers ───────────────────────────────────────────

func (e *emitter) emitFsgnjS(rd, rs1, rs2, funct3 uint32) {
	s1 := fmt.Sprintf("unbox_f32(f[%d])", rs1)
	s2 := fmt.Sprintf("unbox_f32(f[%d])", rs2)
	switch funct3 {
	case 0: // FSGNJ.S — copy sign of rs2
		e.emit("    f[%d] = box_f32((%s & 0x7FFFFFFFu) | (%s & 0x80000000u));\n", rd, s1, s2)
	case 1: // FSGNJN.S — negate sign of rs2
		e.emit("    f[%d] = box_f32((%s & 0x7FFFFFFFu) | (~%s & 0x80000000u));\n", rd, s1, s2)
	case 2: // FSGNJX.S — XOR signs
		e.emit("    f[%d] = box_f32(%s ^ (%s & 0x80000000u));\n", rd, s1, s2)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFsgnjD(rd, rs1, rs2, funct3 uint32) {
	switch funct3 {
	case 0: // FSGNJ.D
		e.emit("    f[%d] = (f[%d] & 0x7FFFFFFFFFFFFFFFULL) | (f[%d] & 0x8000000000000000ULL);\n", rd, rs1, rs2)
	case 1: // FSGNJN.D
		e.emit("    f[%d] = (f[%d] & 0x7FFFFFFFFFFFFFFFULL) | (~f[%d] & 0x8000000000000000ULL);\n", rd, rs1, rs2)
	case 2: // FSGNJX.D
		e.emit("    f[%d] = f[%d] ^ (f[%d] & 0x8000000000000000ULL);\n", rd, rs1, rs2)
	default:
		e.terminated = true
	}
}

// ── FP comparison helpers ───────────────────────────────────────────────

func (e *emitter) emitFcmpS(rd, rs1, rs2, funct3 uint32) {
	if rd == 0 {
		return // write to x0 discarded
	}
	d := e.rd(rd) // mark integer register as used
	switch funct3 {
	case 0: // FLE.S
		e.emit("    %s = (rd_f32(f,%d) <= rd_f32(f,%d)) ? 1 : 0;\n", d, rs1, rs2)
	case 1: // FLT.S
		e.emit("    %s = (rd_f32(f,%d) < rd_f32(f,%d)) ? 1 : 0;\n", d, rs1, rs2)
	case 2: // FEQ.S
		e.emit("    %s = (rd_f32(f,%d) == rd_f32(f,%d)) ? 1 : 0;\n", d, rs1, rs2)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcmpD(rd, rs1, rs2, funct3 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	switch funct3 {
	case 0: // FLE.D
		e.emit("    %s = (rd_f64(f,%d) <= rd_f64(f,%d)) ? 1 : 0;\n", d, rs1, rs2)
	case 1: // FLT.D
		e.emit("    %s = (rd_f64(f,%d) < rd_f64(f,%d)) ? 1 : 0;\n", d, rs1, rs2)
	case 2: // FEQ.D
		e.emit("    %s = (rd_f64(f,%d) == rd_f64(f,%d)) ? 1 : 0;\n", d, rs1, rs2)
	default:
		e.terminated = true
	}
}

// ── FP conversion helpers ───────────────────────────────────────────────

func (e *emitter) emitFcvtToIntS(rd, rs1, rs2 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	switch rs2 {
	case 0: // FCVT.W.S
		e.emit("    %s = (int64_t)(int32_t)rd_f32(f,%d);\n", d, rs1)
	case 1: // FCVT.WU.S
		e.emit("    %s = (int64_t)(int32_t)(uint32_t)rd_f32(f,%d);\n", d, rs1)
	case 2: // FCVT.L.S
		e.emit("    %s = (int64_t)rd_f32(f,%d);\n", d, rs1)
	case 3: // FCVT.LU.S
		e.emit("    %s = (uint64_t)rd_f32(f,%d);\n", d, rs1)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtToIntD(rd, rs1, rs2 uint32) {
	if rd == 0 {
		return
	}
	d := e.rd(rd)
	switch rs2 {
	case 0: // FCVT.W.D
		e.emit("    %s = (int64_t)(int32_t)rd_f64(f,%d);\n", d, rs1)
	case 1: // FCVT.WU.D
		e.emit("    %s = (int64_t)(int32_t)(uint32_t)rd_f64(f,%d);\n", d, rs1)
	case 2: // FCVT.L.D
		e.emit("    %s = (int64_t)rd_f64(f,%d);\n", d, rs1)
	case 3: // FCVT.LU.D
		e.emit("    %s = (uint64_t)rd_f64(f,%d);\n", d, rs1)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtFromIntS(rd, rs1, rs2 uint32) {
	s := e.rs(rs1) // integer register
	switch rs2 {
	case 0: // FCVT.S.W
		e.emit("    wr_f32(f, %d, (float)(int32_t)%s);\n", rd, s)
	case 1: // FCVT.S.WU
		e.emit("    wr_f32(f, %d, (float)(uint32_t)%s);\n", rd, s)
	case 2: // FCVT.S.L
		e.emit("    wr_f32(f, %d, (float)(int64_t)%s);\n", rd, s)
	case 3: // FCVT.S.LU
		e.emit("    wr_f32(f, %d, (float)%s);\n", rd, s)
	default:
		e.terminated = true
	}
}

func (e *emitter) emitFcvtFromIntD(rd, rs1, rs2 uint32) {
	s := e.rs(rs1)
	switch rs2 {
	case 0: // FCVT.D.W
		e.emit("    wr_f64(f, %d, (double)(int32_t)%s);\n", rd, s)
	case 1: // FCVT.D.WU
		e.emit("    wr_f64(f, %d, (double)(uint32_t)%s);\n", rd, s)
	case 2: // FCVT.D.L
		e.emit("    wr_f64(f, %d, (double)(int64_t)%s);\n", rd, s)
	case 3: // FCVT.D.LU
		e.emit("    wr_f64(f, %d, (double)%s);\n", rd, s)
	default:
		e.terminated = true
	}
}

// ── Shared helpers for JAL, JALR, BRANCH ────────────────────────────────
// These are used by both 32-bit and RVC emitters.

// emitJAL emits a JAL (rd = link, exit block to target).
// insnSize is 4 for 32-bit or 2 for RVC.
func (e *emitter) emitJAL(rd uint32, offset int64, insnSize uint64) {
	target := e.pc + uint64(offset)
	if rd != 0 {
		e.emit("    %s = 0x%xULL;\n", e.rd(rd), e.pc+insnSize) // link
	}
	e.advancePC(insnSize)

	// Phase 1 chaining: for pure jumps (rd==0), follow the target
	// instead of exiting to the Go dispatch loop.
	if rd == 0 {
		e.emit("    goto b_%x;\n", target)
		e.gotoTargets[target] = true
		e.pc = target // continue emitting from target
		// Don't set terminated — the main loop will continue from target.
		// The visited check at the top of the loop handles cycles.
		return
	}

	// rd != 0: function call — must exit block.
	e.emitReturn(target, jitOK)
	e.terminated = true
}

// emitJALR emits a JALR (rd = link, exit block to rs1+imm).
// insnSize is 4 for 32-bit or 2 for RVC.
func (e *emitter) emitJALR(rd, rs1 uint32, imm int64, insnSize uint64) {
	// Compute target BEFORE writing link (rd may alias rs1).
	e.emit("    { uint64_t t = (%s + %dLL) & ~(uint64_t)1;\n", e.rs(rs1), imm)
	if rd != 0 {
		e.emit("    %s = 0x%xULL;\n", e.rd(rd), e.pc+insnSize) // link
	}
	e.advancePC(insnSize)
	e.emitWriteBackAll()
	e.emit("      return (JITResult){t, ic, 0, 0}; }\n")
	e.terminated = true
}

// emitBranch emits a conditional branch (internal goto or external exit).
// Does NOT call advancePC — the caller must do that for the fall-through path.
// Emits its own ic++ before the conditional because the caller's advancePC
// (which also emits ic++) is unreachable on the taken path.
func (e *emitter) emitBranch(rs1, rs2, funct3 uint32, offset int64) {
	target := e.pc + uint64(offset)
	cmp := branchCmp(funct3)

	// ic++ for the branch instruction itself — must happen before the
	// conditional so both taken and not-taken paths count it.
	// Set icEmitted so the caller's advancePC doesn't double-count.
	e.emitIC()
	e.icEmitted = true

	// Internal branch: target already emitted OR will be emitted (within region).
	internal := e.visited[target] ||
		(e.regionEnd > 0 && target >= e.startPC && target < e.regionEnd)
	if internal {
		e.emit("    if (%s %s %s) goto b_%x;\n",
			e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3), target)
		e.gotoTargets[target] = true
	} else {
		// External branch — exit block on taken path.
		e.emit("    if (%s %s %s) {\n",
			e.rsC(rs1, funct3), cmp, e.rsC(rs2, funct3))
		e.emitWriteBackAll()
		e.emit("      return (JITResult){0x%xULL, ic, 0, 0};\n    }\n", target)
		// Fall through continues to next instruction
	}
}

// ── RVC (compressed instructions) ───────────────────────────────────────
// Strategy: decode 16-bit instruction, expand to equivalent 32-bit fields,
// then call existing emitters. Same approach as libriscv's tr_emit_rvc.cpp.

func (e *emitter) emitRVC(insn uint16) {
	e.emitLabel()

	quad := insn & 0x3
	funct3 := insn >> 13

	switch quad {
	case 0x0:
		e.emitRVC_Q0(insn, funct3)
	case 0x1:
		e.emitRVC_Q1(insn, funct3)
	case 0x2:
		e.emitRVC_Q2(insn, funct3)
	default:
		e.terminated = true
	}

	if !e.terminated {
		e.advancePC(2)
	}
}

// emitRVC_Q0 handles Quadrant 0: loads/stores with compressed registers.
func (e *emitter) emitRVC_Q0(insn uint16, funct3 uint16) {
	rd := uint32(8 + ((insn >> 2) & 7))
	rs1 := uint32(8 + ((insn >> 7) & 7))

	switch funct3 {
	case 0b000: // C.ADDI4SPN: rd' = sp + nzuimm
		nzuimm := int64(((insn>>11)&3)<<4 | ((insn>>7)&0xF)<<6 |
			((insn>>6)&1)<<2 | ((insn>>5)&1)<<3)
		if nzuimm == 0 {
			e.terminated = true
			return
		}
		e.emitOpImm(rd, 2, nzuimm, 0, 0) // ADDI rd, sp, nzuimm
	case 0b001: // C.FLD
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitFPLoad(rd, rs1, uimm, 3) // FLD rd', uimm(rs1')
	case 0b010: // C.LW
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
		e.emitLoad(rd, rs1, uimm, 2) // LW rd, uimm(rs1)
	case 0b011: // C.LD
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitLoad(rd, rs1, uimm, 3) // LD rd, uimm(rs1)
	case 0b101: // C.FSD
		rs2 := uint32(8 + ((insn >> 2) & 7))
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitFPStore(rs1, rs2, uimm, 3) // FSD rs2', uimm(rs1')
	case 0b110: // C.SW
		rs2 := uint32(8 + ((insn >> 2) & 7))
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>6)&1)<<2 | ((insn>>5)&1)<<6)
		e.emitStore(rs1, rs2, uimm, 2) // SW rs2, uimm(rs1)
	case 0b111: // C.SD
		rs2 := uint32(8 + ((insn >> 2) & 7))
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>5)&3)<<6)
		e.emitStore(rs1, rs2, uimm, 3) // SD rs2, uimm(rs1)
	default:
		e.terminated = true
	}
}

// emitRVC_Q1 handles Quadrant 1: arithmetic, branches, jumps.
func (e *emitter) emitRVC_Q1(insn uint16, funct3 uint16) {
	switch funct3 {
	case 0b000: // C.NOP / C.ADDI
		rd := uint32((insn >> 7) & 0x1F)
		imm := rvcSignedImm6(insn)
		e.emitOpImm(rd, rd, imm, 0, 0) // ADDI rd, rd, imm

	case 0b001: // C.ADDIW
		rd := uint32((insn >> 7) & 0x1F)
		imm := rvcSignedImm6(insn)
		e.emitOpImm32(rd, rd, imm, 0, 0) // ADDIW rd, rd, imm

	case 0b010: // C.LI
		rd := uint32((insn >> 7) & 0x1F)
		imm := rvcSignedImm6(insn)
		e.emitOpImm(rd, 0, imm, 0, 0) // ADDI rd, x0, imm

	case 0b011: // C.ADDI16SP / C.LUI
		rd := uint32((insn >> 7) & 0x1F)
		if rd == 2 { // C.ADDI16SP
			nzimm := int64(((insn>>12)&1)<<9 | ((insn>>6)&1)<<4 |
				((insn>>5)&1)<<6 | ((insn>>3)&3)<<7 | ((insn>>2)&1)<<5)
			if (insn>>12)&1 != 0 {
				nzimm |= -512
			}
			if nzimm == 0 {
				e.terminated = true
				return
			}
			e.emitOpImm(2, 2, nzimm, 0, 0) // ADDI sp, sp, nzimm
		} else if rd != 0 { // C.LUI
			nzimm := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			if (insn>>12)&1 != 0 {
				nzimm |= -32
			}
			if nzimm == 0 {
				e.terminated = true
				return
			}
			uimm := nzimm << 12
			e.emit("    %s = %dLL;\n", e.rd(rd), uimm) // LUI rd, nzimm
		}

	case 0b100: // C.MISC-ALU
		rs1 := uint32(8 + ((insn >> 7) & 7))
		rs2 := uint32(8 + ((insn >> 2) & 7))
		funct2 := (insn >> 10) & 3
		switch funct2 {
		case 0b00: // C.SRLI
			shamt := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			e.emitOpImm(rs1, rs1, shamt, 5, 0) // SRLI
		case 0b01: // C.SRAI
			shamt := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
			e.emitOpImm(rs1, rs1, shamt, 5, 0x20) // SRAI
		case 0b10: // C.ANDI
			imm := rvcSignedImm6(insn)
			e.emitOpImm(rs1, rs1, imm, 7, 0) // ANDI
		case 0b11: // C.SUB/XOR/OR/AND/SUBW/ADDW
			bit12 := (insn >> 12) & 1
			op := (insn >> 5) & 3
			if bit12 == 0 {
				switch op {
				case 0b00:
					e.emitOp(rs1, rs1, rs2, 0, 0x20) // SUB
				case 0b01:
					e.emitOp(rs1, rs1, rs2, 4, 0) // XOR
				case 0b10:
					e.emitOp(rs1, rs1, rs2, 6, 0) // OR
				case 0b11:
					e.emitOp(rs1, rs1, rs2, 7, 0) // AND
				}
			} else {
				switch op {
				case 0b00:
					e.emitOp32(rs1, rs1, rs2, 0, 0x20) // SUBW
				case 0b01:
					e.emitOp32(rs1, rs1, rs2, 0, 0) // ADDW
				default:
					e.terminated = true
				}
			}
		}

	case 0b101: // C.J — unconditional jump
		off := rvcJOffset(insn)
		e.emitJAL(0, off, 2) // JAL x0, offset

	case 0b110: // C.BEQZ
		rs1 := uint32(8 + ((insn >> 7) & 7))
		off := rvcBOffset(insn)
		e.emitBranch(rs1, 0, 0, off) // BEQ rs1, x0, offset

	case 0b111: // C.BNEZ
		rs1 := uint32(8 + ((insn >> 7) & 7))
		off := rvcBOffset(insn)
		e.emitBranch(rs1, 0, 1, off) // BNE rs1, x0, offset

	default:
		e.terminated = true
	}
}

// emitRVC_Q2 handles Quadrant 2: stack-pointer relative, register ops.
func (e *emitter) emitRVC_Q2(insn uint16, funct3 uint16) {
	rd := uint32((insn >> 7) & 0x1F)
	rs2 := uint32((insn >> 2) & 0x1F)

	switch funct3 {
	case 0b000: // C.SLLI
		shamt := int64(((insn>>12)&1)<<5 | (insn>>2)&0x1F)
		e.emitOpImm(rd, rd, shamt, 1, 0) // SLLI rd, rd, shamt

	case 0b001: // C.FLDSP
		uimm := int64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
		e.emitFPLoad(rd, 2, uimm, 3) // FLD rd, uimm(sp)

	case 0b010: // C.LWSP
		uimm := int64(((insn>>12)&1)<<5 | ((insn>>4)&7)<<2 | ((insn>>2)&3)<<6)
		e.emitLoad(rd, 2, uimm, 2) // LW rd, uimm(sp)

	case 0b011: // C.LDSP
		uimm := int64(((insn>>12)&1)<<5 | ((insn>>5)&3)<<3 | ((insn>>2)&7)<<6)
		e.emitLoad(rd, 2, uimm, 3) // LD rd, uimm(sp)

	case 0b100:
		bit12 := (insn >> 12) & 1
		if bit12 == 0 {
			if rs2 == 0 { // C.JR
				if rd == 0 {
					e.terminated = true
					return
				}
				e.emitJALR(0, rd, 0, 2) // JALR x0, rd, 0
			} else { // C.MV
				e.emitOpImm(rd, rs2, 0, 0, 0) // ADDI rd, rs2, 0
			}
		} else {
			if rd == 0 && rs2 == 0 { // C.EBREAK
				e.advancePC(2)
				e.emitReturn(e.pc, jitEbreak)
				e.terminated = true
			} else if rs2 == 0 { // C.JALR
				e.emitJALR(1, rd, 0, 2) // JALR ra, rd, 0
			} else { // C.ADD
				e.emitOp(rd, rd, rs2, 0, 0) // ADD rd, rd, rs2
			}
		}

	case 0b101: // C.FSDSP
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
		e.emitFPStore(2, rs2, uimm, 3) // FSD rs2, uimm(sp)

	case 0b110: // C.SWSP
		uimm := int64(((insn>>9)&0xF)<<2 | ((insn>>7)&3)<<6)
		e.emitStore(2, rs2, uimm, 2) // SW rs2, uimm(sp)

	case 0b111: // C.SDSP
		uimm := int64(((insn>>10)&7)<<3 | ((insn>>7)&7)<<6)
		e.emitStore(2, rs2, uimm, 3) // SD rs2, uimm(sp)

	default:
		e.terminated = true
	}
}

// ── RVC immediate extraction helpers ────────────────────────────────────

// rvcSignedImm6 extracts 6-bit sign-extended immediate from CI-type.
// imm[5] = insn[12], imm[4:0] = insn[6:2]
func rvcSignedImm6(insn uint16) int64 {
	imm := int64(insn>>2) & 0x1F
	if (insn>>12)&1 != 0 {
		imm |= -32
	}
	return imm
}

// rvcJOffset extracts the CJ-type signed offset for C.J/C.JAL.
// Same as cjOffset in cpu.go.
func rvcJOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o >> 12) & 1) << 11 | ((o >> 11) & 1) << 4 | ((o >> 9) & 3) << 8 |
		((o >> 8) & 1) << 10 | ((o >> 7) & 1) << 6 | ((o >> 6) & 1) << 7 |
		((o >> 3) & 7) << 1 | ((o >> 2) & 1) << 5
	if off&(1<<11) != 0 {
		off |= -1 << 12
	}
	return off
}

// rvcBOffset extracts the CB-type signed offset for C.BEQZ/C.BNEZ.
// Same as cbOffset in cpu.go.
func rvcBOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o >> 12) & 1) << 8 | ((o >> 10) & 3) << 3 | ((o >> 5) & 3) << 6 |
		((o >> 3) & 3) << 1 | ((o >> 2) & 1) << 5
	if off&(1<<8) != 0 {
		off |= -1 << 9
	}
	return off
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
	out.WriteString("extern void jit_trace(const char *label, uint64_t addr, uint64_t val);\n")
	out.WriteString("typedef struct { uint64_t pc; uint64_t ic; uint64_t status; uint64_t fault_addr; } JITResult;\n\n")

	// Conditional FP header
	if e.usesFP {
		out.WriteString("typedef union { int32_t i32[2]; float f32[2]; int64_t i64; double f64; uint64_t u64; } fp64reg;\n")
		out.WriteString("static uint64_t box_f32(uint32_t bits) { return 0xFFFFFFFF00000000ULL | (uint64_t)bits; }\n")
		out.WriteString("static uint32_t unbox_f32(uint64_t r) { return (r>>32)==0xFFFFFFFF ? (uint32_t)r : 0x7FC00000u; }\n")
		out.WriteString("static float rd_f32(uint64_t *f, int r) { fp64reg t; t.i32[0]=(int32_t)unbox_f32(f[r]); return t.f32[0]; }\n")
		out.WriteString("static double rd_f64(uint64_t *f, int r) { fp64reg t; t.u64=f[r]; return t.f64; }\n")
		out.WriteString("static void wr_f32(uint64_t *f, int r, float v) { fp64reg t; t.f32[0]=v; f[r]=box_f32((uint32_t)t.i32[0]); }\n")
		out.WriteString("static void wr_f64(uint64_t *f, int r, double v) { fp64reg t; t.f64=v; f[r]=t.u64; }\n")
		out.WriteString("static float jit_fminf(float a,float b) { if(a!=a)return b; if(b!=b)return a; if(a<b)return a; if(b<a)return b; fp64reg u,v; u.f32[0]=a; v.f32[0]=b; return (u.i32[0]&(int32_t)0x80000000)?a:b; }\n")
		out.WriteString("static float jit_fmaxf(float a,float b) { if(a!=a)return b; if(b!=b)return a; if(a>b)return a; if(b>a)return b; fp64reg u,v; u.f32[0]=a; v.f32[0]=b; return (v.i32[0]&(int32_t)0x80000000)?a:b; }\n")
		out.WriteString("static double jit_fmin(double a,double b) { if(a!=a)return b; if(b!=b)return a; if(a<b)return a; if(b<a)return b; fp64reg u,v; u.f64=a; v.f64=b; return (u.u64>>63)?a:b; }\n")
		out.WriteString("static double jit_fmax(double a,double b) { if(a!=a)return b; if(b!=b)return a; if(a>b)return a; if(b>a)return b; fp64reg u,v; u.f64=a; v.f64=b; return (v.u64>>63)?a:b; }\n")
		out.WriteString("extern float jit_sqrtf(float);\n")
		out.WriteString("extern double jit_sqrt(double);\n\n")
	}

	// Conditional Zbb helper functions
	if e.usesZbbHelpers {
		out.WriteString("static int jit_clz64(uint64_t x){if(!x)return 64;int n=0;if(!(x&0xFFFFFFFF00000000ULL)){n+=32;x<<=32;}if(!(x&0xFFFF000000000000ULL)){n+=16;x<<=16;}if(!(x&0xFF00000000000000ULL)){n+=8;x<<=8;}if(!(x&0xF000000000000000ULL)){n+=4;x<<=4;}if(!(x&0xC000000000000000ULL)){n+=2;x<<=2;}if(!(x&0x8000000000000000ULL))n+=1;return n;}\n")
		out.WriteString("static int jit_ctz64(uint64_t x){if(!x)return 64;int n=0;if(!(x&0xFFFFFFFF)){n+=32;x>>=32;}if(!(x&0xFFFF)){n+=16;x>>=16;}if(!(x&0xFF)){n+=8;x>>=8;}if(!(x&0xF)){n+=4;x>>=4;}if(!(x&0x3)){n+=2;x>>=2;}if(!(x&0x1))n+=1;return n;}\n")
		out.WriteString("static int jit_cpop64(uint64_t x){x=x-((x>>1)&0x5555555555555555ULL);x=(x&0x3333333333333333ULL)+((x>>2)&0x3333333333333333ULL);x=(x+(x>>4))&0x0F0F0F0F0F0F0F0FULL;return(int)((x*0x0101010101010101ULL)>>56);}\n")
		out.WriteString("static int jit_clz32(uint32_t x){if(!x)return 32;int n=0;if(!(x&0xFFFF0000u)){n+=16;x<<=16;}if(!(x&0xFF000000u)){n+=8;x<<=8;}if(!(x&0xF0000000u)){n+=4;x<<=4;}if(!(x&0xC0000000u)){n+=2;x<<=2;}if(!(x&0x80000000u))n+=1;return n;}\n")
		out.WriteString("static int jit_ctz32(uint32_t x){if(!x)return 32;int n=0;if(!(x&0xFFFF)){n+=16;x>>=16;}if(!(x&0xFF)){n+=8;x>>=8;}if(!(x&0xF)){n+=4;x>>=4;}if(!(x&0x3)){n+=2;x>>=2;}if(!(x&0x1))n+=1;return n;}\n")
		out.WriteString("static int jit_cpop32(uint32_t x){x=x-((x>>1)&0x55555555u);x=(x&0x33333333u)+((x>>2)&0x33333333u);x=(x+(x>>4))&0x0F0F0F0Fu;return(int)((x*0x01010101u)>>24);}\n\n")
	}

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

	// Bail labels: safety net for gotos to PCs that were not emitted
	// (e.g., block terminated early due to untranslatable instruction mid-region).
	// Each bail label writes back registers and returns to Go dispatch at that PC.
	for target := range e.gotoTargets {
		if !e.visited[target] {
			fmt.Fprintf(&out, "b_%x:\n", target)
			for i := 1; i < 32; i++ {
				if e.regsUsed[i] {
					fmt.Fprintf(&out, "    x[%d] = r%d;\n", i, i)
				}
			}
			fmt.Fprintf(&out, "    return (JITResult){0x%xULL, ic, 0, 0};\n", target)
		}
	}

	out.WriteString("}\n")

	return &emitResult{
		csrc:     out.String(),
		startPC:  e.startPC,
		endPC:    e.pc,
		numInsns: e.numInsns,
		regsUsed: e.regsUsed,
	}
}
