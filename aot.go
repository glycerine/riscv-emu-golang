package riscv

// aot.go — Whole-program AOT translation driver (Phase 2a).
//
// Terminology mirrors libriscv (xendor/libriscv/lib/libriscv):
//
//   DecodedExecuteSegment — a contiguous guest-VA executable region
//                           with its own decoder_cache and native
//                           code mmap.
//   decoder_cache         — flat DecoderData[] array indexed by
//                           (pc - vaddrBegin) >> 1 (our guests always
//                           support compressed, so SHIFT = 1).
//   DecoderData           — one entry; in our JIT-only model holds
//                           the native chainEntry (uintptr) for that
//                           PC, or 0 if no translation exists.
//
// This file implements the Phase 2a flow:
//   1. collectBranchTargets: one linear pass over .text collects every
//      static branch/jump destination plus every post-terminator PC.
//   2. enumerateBlockRanges: sorts those points to produce a
//      contiguous list of basic-block ranges.
//
// Steps 3 (emitBlockLinear), 4 (jitCompileAOTSegment), and 5 (dispatch
// swap) live in other files; this one is the static analyzer.

import "sort"

// blockRange is one basic-block extent identified by the linear scan.
// startPC is where the block begins; endPC is one past the last byte
// of the block's last instruction.
type blockRange struct {
	startPC uint64
	endPC   uint64
}

// collectBranchTargets does one linear walk of [textBase, textBase+textSize)
// and returns:
//
//   - targets:  every static branch/jump destination reachable from an
//     instruction in the range (BRANCH, JAL direct, C.J, C.BEQZ/BNEZ).
//     Targets outside the range are dropped.
//   - termFT:   every PC that immediately follows a terminator
//     instruction (JALR, ECALL/EBREAK/CSR, JAL with rd != 0).
//     These are potential block starts — the fallthrough after a
//     call, etc. Not every such PC is actually reachable, but we
//     include them conservatively so the enumerated ranges don't span
//     an already-known block boundary.
//
// Uses classifyFlow (jit_decode.go:28) for instruction size + CF kind.
// No BFS, no worklist — one sequential pass over memory.
func collectBranchTargets(mem *GuestMemory, textBase, textSize uint64) (
	targets map[uint64]struct{},
	termFT map[uint64]struct{},
) {
	targets = make(map[uint64]struct{})
	termFT = make(map[uint64]struct{})

	textEnd := textBase + textSize
	pc := textBase
	for pc < textEnd {
		fc, target, insnSize := classifyFlow(mem, pc)
		if insnSize == 0 {
			// Fetch fault — can't decode further from here. Advance by
			// 2 to retry at the next aligned PC (allows recovery in case
			// of a short gap; worst case the next insn is also bad).
			pc += 2
			continue
		}
		switch fc {
		case flowBranch, flowJump:
			if target >= textBase && target < textEnd {
				targets[target] = struct{}{}
			}
		case flowTerm:
			// Fallthrough after a terminator is a potential block start.
			if next := pc + insnSize; next < textEnd {
				termFT[next] = struct{}{}
			}
		}
		pc += insnSize
	}
	return targets, termFT
}

// enumerateBlockRanges returns a sorted list of block ranges covering
// [textBase, textEnd). Block starts are:
//
//	textBase ∪ collectBranchTargets.targets ∪ collectBranchTargets.termFT
//
// Each adjacent pair in the sorted list defines a block's [startPC,
// endPC). The final range extends to textEnd.
func enumerateBlockRanges(mem *GuestMemory, textBase, textSize uint64) []blockRange {
	targets, termFT := collectBranchTargets(mem, textBase, textSize)

	textEnd := textBase + textSize

	// Union the three sources of block-start PCs.
	starts := make(map[uint64]struct{}, len(targets)+len(termFT)+1)
	starts[textBase] = struct{}{}
	for t := range targets {
		starts[t] = struct{}{}
	}
	for t := range termFT {
		starts[t] = struct{}{}
	}

	sorted := make([]uint64, 0, len(starts))
	for pc := range starts {
		if pc < textBase || pc >= textEnd {
			continue
		}
		sorted = append(sorted, pc)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	ranges := make([]blockRange, 0, len(sorted))
	for i := range sorted {
		startPC := sorted[i]
		endPC := textEnd
		if i+1 < len(sorted) {
			endPC = sorted[i+1]
		}
		ranges = append(ranges, blockRange{startPC: startPC, endPC: endPC})
	}
	return ranges
}
