package riscv

// jit_decode.go — Decode-only helpers used by the IR JIT emission path.

// ── Region pre-scan ─────────────────────────────────────────────────────

type flowClass int

const (
	flowSeq    flowClass = iota // sequential: next = pc + insnSize
	flowBranch                  // conditional: next = {pc + insnSize, target}
	flowJump                    // unconditional J/C.J rd==0: next = target only
	flowCall                    // JAL ra: next = {pc + insnSize, target}
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
		if rd == 1 {
			return flowCall, pc + uint64(jImm(insn)), 4
		}
		return flowTerm, 0, 4
	case 0x67: // JALR
		return flowTerm, 0, 4
	case 0x73: // SYSTEM (ECALL, EBREAK, CSR) — all terminate the block.
		// ECALL always terminates: the AOT enumerator registers pc+4 as a
		// new block start via termFT in aot.go, which lowerSyscall targets
		// with a chain exit when InlineEcallEnabled is on.
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
	visited := newU64setSized(256)
	worklist := []uint64{entryPC}
	maxEnd := entryPC

	for len(worklist) > 0 {
		pc := worklist[0]
		worklist = worklist[1:]

		if visited.has(pc) {
			continue
		}
		if pc < entryPC {
			continue
		}

		fc, target, insnSize := classifyFlow(mem, pc)
		if insnSize == 0 {
			continue // fetch failed
		}

		visited.add(pc)
		if end := pc + insnSize; end > maxEnd {
			maxEnd = end
		}

		switch fc {
		case flowSeq:
			worklist = append(worklist, pc+insnSize)
		case flowBranch:
			worklist = append(worklist, pc+insnSize)
			if target >= entryPC {
				worklist = append(worklist, target)
			}
		case flowJump:
			if target >= entryPC {
				worklist = append(worklist, target)
			}
		case flowCall:
			worklist = append(worklist, pc+insnSize)
		case flowTerm:
			// no successors
		}
	}

	return regionInfo{endPC: maxEnd, pcCount: visited.len()}
}

// ── RVC immediate extraction ────────────────────────────────────────────

func rvcSignedImm6(insn uint16) int64 {
	imm := int64(insn>>2) & 0x1F
	if (insn>>12)&1 != 0 {
		imm |= -32
	}
	return imm
}

// rvcJOffset extracts the CJ-type signed offset for C.J/C.JAL.
func rvcJOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o>>12)&1)<<11 | ((o>>11)&1)<<4 | ((o>>9)&3)<<8 |
		((o>>8)&1)<<10 | ((o>>7)&1)<<6 | ((o>>6)&1)<<7 |
		((o>>3)&7)<<1 | ((o>>2)&1)<<5
	if off&(1<<11) != 0 {
		off |= -1 << 12
	}
	return off
}

// rvcBOffset extracts the CB-type signed offset for C.BEQZ/C.BNEZ.
func rvcBOffset(insn uint16) int64 {
	o := int64(insn)
	off := ((o>>12)&1)<<8 | ((o>>10)&3)<<3 | ((o>>5)&3)<<6 |
		((o>>3)&3)<<1 | ((o>>2)&1)<<5
	if off&(1<<8) != 0 {
		off |= -1 << 9
	}
	return off
}

// ── 32-bit immediate extraction ─────────────────────────────────────────

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
