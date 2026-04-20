package riscv

// DecodedInsn is one slot in the decoder cache.
//
// For RVC instructions, pre-decoded fields (rd/rs1/rs2/imm) and the
// synthetic RVC opcode class (op >= 0x80) drive execRVCSlot directly,
// eliminating the re-extraction work stepRVC would otherwise do per visit.
// For 32-bit instructions the raw bits are cached for stepFromInsn.
type DecodedInsn struct {
	// len is 2 for RVC, 4 for 32-bit, 0 for "not yet decoded".
	len uint8
	// op is the dispatch class:
	//   0x03..0x7F — RV32 opcode (bits[6:0]); executor is stepFromInsn.
	//   0x80+      — RVC synthetic class (see opC_* in decode.go);
	//                executor is execRVCSlot.
	op uint8
	// Pre-decoded register fields (5-bit, with RVC 3-bit fields already
	// translated to x8..x15).
	rd, rs1, rs2, rs3 uint8
	funct3, funct7    uint8
	// flags bitfield:
	//   bit 0 — blockEnd: this insn ends a basic block (branch/jump/trap).
	//           RunCached uses it to amortize cycle-counter and watchAddr
	//           updates across a whole block.
	flags uint8
	_pad  uint8
	// imm is the pre-decoded signed immediate (type depends on op).
	imm int32
	// insn is the raw 32-bit bits (or 16-bit RVC bits in the low word).
	insn uint32
}

// flagBlockEnd is set on instructions that terminate a basic block:
// branches, JAL/JALR, ECALL/EBREAK, FENCE.I, MRET, and any RVC equivalent.
const flagBlockEnd = 1 << 0

//go:nosplit
func (d *DecodedInsn) blockEnd() bool { return d.flags&flagBlockEnd != 0 }

// DecoderCache is a flat slab of DecodedInsn, indexed by (pc - base) >> 1.
// Instructions are always ≥ 2-byte aligned, so half-word indexing is tight.
type DecoderCache struct {
	base uint64
	size uint64 // same as len(slots)*2 — cached to avoid len() in hot path
	slots []DecodedInsn
}

// NewDecoderCache allocates a cache covering [base, base+size) bytes.
func NewDecoderCache(base, size uint64) *DecoderCache {
	if size&1 != 0 {
		size++
	}
	return &DecoderCache{
		base:  base,
		size:  size,
		slots: make([]DecodedInsn, size/2),
	}
}

// lookup returns the slot pointer for pc, or nil if pc is out of cache range.
// Single-compare bounds check via unsigned subtract — when pc < base,
// the subtraction underflows to a very large uint64 that still fails the
// check against size.
//
//go:nosplit
func (dc *DecoderCache) lookup(pc uint64) *DecodedInsn {
	off := pc - dc.base
	if off >= dc.size {
		return nil
	}
	return &dc.slots[off>>1]
}

// InvalidateAll clears every slot. Call before re-using the cache.
func (dc *DecoderCache) InvalidateAll() {
	for i := range dc.slots {
		dc.slots[i] = DecodedInsn{}
	}
}
