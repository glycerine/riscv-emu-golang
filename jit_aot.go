package riscv

// jit_aot.go — AOT batch-compiler: takes a list of basic-block
// ranges (from aot.go:enumerateBlockRanges), emits+assembles each,
// concatenates the native code into one mmap, pre-resolves static
// chain exits, and builds the segment's decoder_cache.
//
// Mirrors libriscv's DecodedExecuteSegment initialization
// (xendor/libriscv/lib/libriscv/decoder_cache.cpp) but retains
// per-block goasm.Ctx lowering (no global regalloc refactor).

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

// aotBlockCompile is the per-block working state carried between
// the lower+assemble pass and the copy+backpatch pass.
type aotBlockCompile struct {
	startPC     uint64
	endPC       uint64
	bytes       []byte
	lowerResult *LowerResult
	baseOffset  int // offset of this block within the unified mmap
	blk         *compiledBlock
	block       *Block // retained for VizJit dump (nil when VizJit disabled)
	progs       string // goasm Prog listing (empty when VizJit disabled)
	hasFP       bool
}

// jitCompileAOTSegment batch-compiles every block range into one
// contiguous native-code mmap, builds a DecodedExecuteSegment with
// a mask-bounded mutable decoder_cache, and pre-resolves static
// chain exits whose target PC is inside the segment.
func (j *JIT) jitCompileAOTSegment(
	mem *GuestMemory,
	ranges []blockRange,
	vaddrBegin, vaddrEnd uint64,
) (*DecodedExecuteSegment, error) {
	if vaddrBegin >= vaddrEnd {
		return nil, fmt.Errorf("jitCompileAOTSegment: empty range")
	}
	if err := j.checkNativeBackend(); err != nil {
		return nil, err
	}

	// ── Pass 1: lower + assemble each range; accumulate byte lengths ──
	compiles := make([]*aotBlockCompile, 0, len(ranges))
	totalSize := 0
	for _, r := range ranges {
		res := j.emitBlockLinear(mem, r.startPC, r.endPC)
		if res == nil || res.numInsns == 0 {
			continue // untranslatable; decoder_cache slot stays 0
		}
		pool := j.regPolicy.Pool(res.block)
		pinned := j.regPolicy.Pinned()
		if j.reserveInstructionCounterReg(res.block) {
			pool.IntRegs = removeReg(pool.IntRegs, j.regPolicy.InstructionCounterReg)
		}
		alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

		ctx := goasm.New(j.regPolicy.Arch)
		ctx.Append(ctx.NewATEXT())
		lowerResult, lowerErr := j.regPolicy.Lower(ctx, res.block, alloc)
		if lowerErr != nil {
			continue // lowering failed — skip
		}

		code, err := ctx.Assemble()
		if err != nil || len(code) == 0 {
			continue
		}

		// Capture Prog listing after Assemble so branch targets show
		// resolved byte offsets instead of 0.
		var progs string
		var vizBlock *Block
		if _, on := vizJitEnabled(); on {
			progs = ctx.DumpProgs()
			vizBlock = res.block
		}

		compiles = append(compiles, &aotBlockCompile{
			startPC:     r.startPC,
			endPC:       r.endPC,
			bytes:       code,
			lowerResult: lowerResult,
			baseOffset:  totalSize,
			block:       vizBlock,
			progs:       progs,
			hasFP:       allocHasFP(alloc),
		})
		totalSize += len(code)
	}

	if totalSize == 0 {
		return nil, fmt.Errorf("jitCompileAOTSegment: no blocks translated")
	}

	// ── Pass 2: allocate one big exec mmap and copy each block ──
	execMem, err := allocExec(totalSize)
	if err != nil {
		return nil, fmt.Errorf("allocExec: %w", err)
	}
	codeBase := uintptr(unsafe.Pointer(&execMem[0]))

	blocks := make(map[uint64]*compiledBlock, len(compiles))
	prePatches := 0
	var writeErr error
	withExecWrite(func() {
		for _, bc := range compiles {
			copy(execMem[bc.baseOffset:bc.baseOffset+len(bc.bytes)], bc.bytes)

			blockBase := codeBase + uintptr(bc.baseOffset)
			bc.blk = &compiledBlock{fn: blockBase, hasFP: bc.hasFP}

			if bc.lowerResult.ChainEntryProg == nil {
				// V2 or debug variants don't produce ChainEntryProg; skip.
				blocks[bc.startPC] = bc.blk
				continue
			}
			bc.blk.chainEntry = blockBase + uintptr(bc.lowerResult.ChainEntryProg.Pc)
			if bc.lowerResult.LiveChainEntryProg != nil {
				bc.blk.liveChainEntry = blockBase + uintptr(bc.lowerResult.LiveChainEntryProg.Pc)
				bc.blk.liveChain = bc.lowerResult.LiveChain
			}

			// Backpatch chain-exit sentinels → slow-exit stub addresses.
			//
			// Offsets are stored RELATIVE TO blk.fn (i.e., just
			// ce.MovProg.Pc + 2), not absolute into the big mmap. This
			// matches the existing patchChainTarget invariant:
			// `patchChainTarget(blk.fn, offset, ...)` computes blk.fn +
			// offset. In the big-mmap AOT layout, blk.fn already includes
			// baseOffset, so the offset field must not include it again.
			//
			// For the initial sentinel write below, we need the absolute
			// position into execMem — that's bc.baseOffset + patchOff.
			for _, ce := range bc.lowerResult.ChainExits {
				patchValue := nativePatchSentinel
				if ce.StubProg != nil {
					stubAddr := blockBase + uintptr(ce.StubProg.Pc)
					patchValue = uint64(stubAddr)
				}
				patchOff, patchErr := j.regPolicy.PatchImm64(execMem[bc.baseOffset:], ce.MovProg, patchValue)
				if patchErr != nil {
					writeErr = fmt.Errorf("jitCompileAOTSegment: patch chain exit: %w", patchErr)
					return
				}
				if ce.SourceMovProg != nil {
					if _, patchErr := j.regPolicy.PatchImm64(execMem[bc.baseOffset:], ce.SourceMovProg, uint64(uintptr(unsafe.Pointer(bc.blk)))); patchErr != nil {
						writeErr = fmt.Errorf("jitCompileAOTSegment: patch chain source: %w", patchErr)
						return
					}
				}
				livePatchOff := -1
				if ce.LiveMovProg != nil {
					liveOff, patchErr := j.regPolicy.PatchImm64(execMem[bc.baseOffset:], ce.LiveMovProg, 0)
					if patchErr != nil {
						writeErr = fmt.Errorf("jitCompileAOTSegment: patch live chain exit: %w", patchErr)
						return
					}
					livePatchOff = liveOff
				}
				bc.blk.chainExits = append(bc.blk.chainExits, chainPatchInfo{
					targetPC:        ce.TargetPC,
					patchOffset:     patchOff,
					livePatchOffset: livePatchOff,
					liveChain:       ce.LiveChain,
				})
			}

			// JALR IC sentinel init — same offset convention.
			if err := aotBackpatchJalrICs(execMem, blockBase, bc.baseOffset, bc.lowerResult, bc.blk, j.regPolicy.PatchImm64); err != nil {
				writeErr = err
				return
			}

			if err := aotBackpatchGocallResumes(execMem, blockBase, bc.baseOffset, bc.lowerResult, j.regPolicy.PatchImm64); err != nil {
				writeErr = err
				return
			}

			blocks[bc.startPC] = bc.blk
		}

		// ── Pass 3: pre-resolve static chain exits whose target is in the segment ──
		// Writes the target's absolute chainEntry into each backend patch
		// slot, overwriting the slow-exit-stub address written in Pass 2.
		// At run time the chain jump goes directly to the target block's
		// chainEntry with no Go round-trip or runtime patching.
		for _, bc := range compiles {
			for i, ce := range bc.blk.chainExits {
				target, ok := blocks[ce.targetPC]
				if !ok || target.chainEntry == 0 {
					continue
				}
				if i >= len(bc.lowerResult.ChainExits) || bc.lowerResult.ChainExits[i].TargetPC != ce.targetPC {
					writeErr = fmt.Errorf("jitCompileAOTSegment: chain exit metadata mismatch at block 0x%x exit %d", bc.startPC, i)
					return
				}
				if ce.livePatchOffset >= 0 && liveChainCompatible(ce.liveChain, target) {
					if _, patchErr := j.regPolicy.PatchImm64(execMem[bc.baseOffset:], bc.lowerResult.ChainExits[i].LiveMovProg, uint64(target.liveChainEntry)); patchErr != nil {
						writeErr = fmt.Errorf("jitCompileAOTSegment: pre-patch live chain exit: %w", patchErr)
						return
					}
				} else {
					if _, patchErr := j.regPolicy.PatchImm64(execMem[bc.baseOffset:], bc.lowerResult.ChainExits[i].MovProg, uint64(target.chainEntry)); patchErr != nil {
						writeErr = fmt.Errorf("jitCompileAOTSegment: pre-patch chain exit: %w", patchErr)
						return
					}
				}
				prePatches++
			}
		}
	})
	if writeErr != nil {
		return nil, writeErr
	}
	flushIcache(codeBase, totalSize)

	// ── Pass 4: build the decoder_cache mmap ──
	// Slot layout: 8 bytes per 2-byte-aligned PC slot.
	minSize := uint64((vaddrEnd - vaddrBegin) / 2 * 8)
	cacheSize := uint64(1)
	for cacheSize < minSize {
		cacheSize *= 2
	}
	if cacheSize < 8 {
		cacheSize = 8 // minimum so the mask + load are valid
	}
	cacheMmap, err := allocRWAnon(int(cacheSize))
	if err != nil {
		return nil, fmt.Errorf("allocRWAnon (decoder_cache): %w", err)
	}
	for _, bc := range compiles {
		if bc.startPC < vaddrBegin || bc.startPC >= vaddrEnd {
			continue
		}
		idx := (bc.startPC - vaddrBegin) / 2
		byteOff := idx * 8
		if byteOff+8 > uint64(len(cacheMmap)) {
			continue
		}
		binary.LittleEndian.PutUint64(cacheMmap[byteOff:], uint64(bc.blk.chainEntry))
	}

	seg := &DecodedExecuteSegment{
		vaddrBegin:       vaddrBegin,
		vaddrEnd:         vaddrEnd,
		vaddrSize:        vaddrEnd - vaddrBegin,
		nativeCodeBase:   codeBase,
		nativeCodeSize:   totalSize,
		nativeCodeMmap:   execMem,
		decoderCacheMmap: cacheMmap,
		decoderCacheBase: uintptr(unsafe.Pointer(&cacheMmap[0])),
		decoderCacheMask: cacheSize - 1,
		blocks:           blocks,
	}

	// Back-link each block to its owning segment so RunJIT can publish
	// per-block decoder_cache params without another findSegment lookup.
	for _, bc := range compiles {
		if bc.blk != nil {
			bc.blk.segment = seg
		}
	}

	if debugJIT {
		fmt.Printf("AOT: %d blocks compiled, %d bytes native code, "+
			"%d pre-patched chain exits, decoder_cache=%d bytes\n",
			len(compiles), totalSize, prePatches, cacheSize)
	}

	// VizJit: dump per-block assembly/IR after codeBase is known.
	// No-op when VizJit is disabled (field block is nil, progs empty).
	if _, on := vizJitEnabled(); on {
		var indexLines []string
		indexLines = append(indexLines, fmt.Sprintf("# AOT segment 0x%x..0x%x, %d blocks",
			vaddrBegin, vaddrEnd, len(compiles)))
		for _, bc := range compiles {
			if bc.block == nil {
				continue
			}
			blockBase := codeBase + uintptr(bc.baseOffset)
			vizJitDump(bc.startPC, bc.endPC, mem, bc.block, bc.progs, bc.bytes, blockBase)
			indexLines = append(indexLines,
				fmt.Sprintf("0x%08x  %s.gocpu.asm.pc_0x%08x.asm", bc.startPC, getVizJitTag(), bc.startPC))
		}
		vizJitDumpIndex(indexLines)
	}

	return seg, nil
}

// aotBackpatchJalrICs initializes JALR IC sentinel slots for one
// AOT-compiled block.
//
// Patch offsets stored on blk are RELATIVE to blk.fn (matching the
// existing readJalrICSlot / patchChainTarget conventions). For the
// initial sentinel write, we need absolute position into execMem,
// which is baseOffset + patchOff.
func aotBackpatchJalrICs(
	execMem []byte,
	blockBase uintptr,
	baseOffset int,
	lowerResult *LowerResult,
	blk *compiledBlock,
	patch PatchImm64,
) error {
	for _, ic := range lowerResult.JalrICs {
		var info jalrICPatchInfo
		info.siteIdx = ic.SiteIdx
		stubAddr := blockBase + uintptr(ic.StubProg.Pc)
		for k := 0; k < 2; k++ {
			if ic.PcMov[k] == nil || ic.FnMov[k] == nil {
				continue
			}
			// Relative to blk.fn:
			pcOff, err := patch(execMem[baseOffset:], ic.PcMov[k], ^uint64(0))
			if err != nil {
				return fmt.Errorf("jitCompileAOTSegment: patch jalr pc slot: %w", err)
			}
			fnOff, err := patch(execMem[baseOffset:], ic.FnMov[k], uint64(stubAddr))
			if err != nil {
				return fmt.Errorf("jitCompileAOTSegment: patch jalr fn slot: %w", err)
			}
			info.pcPatchOff[k] = pcOff
			info.fnPatchOff[k] = fnOff
		}
		blk.jalrICs = append(blk.jalrICs, info)
	}
	return nil
}

func aotBackpatchGocallResumes(execMem []byte, blockBase uintptr, baseOffset int, lr *LowerResult, patch PatchImm64) error {
	for _, gr := range lr.GocallResumes {
		resumeAddr := blockBase + uintptr(gr.ResumeProg.Pc)
		if _, err := patch(execMem[baseOffset:], gr.AddrMov, uint64(resumeAddr)); err != nil {
			return fmt.Errorf("jitCompileAOTSegment: patch gocall resume: %w", err)
		}
	}
	return nil
}
