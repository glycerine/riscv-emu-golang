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
	"riscv/goasm"
	"riscv/ir"
	"syscall"
	"unsafe"
)

// aotBlockCompile is the per-block working state carried between
// the lower+assemble pass and the copy+backpatch pass.
type aotBlockCompile struct {
	startPC     uint64
	bytes       []byte
	lowerResult *ir.LowerResult
	baseOffset  int // offset of this block within the unified mmap
	blk         *compiledBlock
}

// jitCompileAOTSegment batch-compiles every block range into one
// contiguous native-code mmap, builds a DecodedExecuteSegment with
// a mask-bounded read-only decoder_cache, and pre-resolves static
// chain exits whose target PC is inside the segment.
func (j *JIT) jitCompileAOTSegment(
	mem *GuestMemory,
	ranges []blockRange,
	vaddrBegin, vaddrEnd uint64,
) (*DecodedExecuteSegment, error) {
	if vaddrBegin >= vaddrEnd {
		return nil, fmt.Errorf("jitCompileAOTSegment: empty range")
	}

	// ── Pass 1: lower + assemble each range; accumulate byte lengths ──
	compiles := make([]*aotBlockCompile, 0, len(ranges))
	totalSize := 0
	for _, r := range ranges {
		res := emitBlockLinear(mem, r.startPC, r.endPC)
		if res == nil || res.numInsns == 0 {
			continue // untranslatable; decoder_cache slot stays 0
		}
		pool := ir.AMD64Pool(res.block)
		pinned := ir.AMD64Pinned()
		alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

		// Fresh Ctx per block — assembled bytes accumulate independently.
		ctx := goasm.New(goasm.AMD64)
		ctx.Append(ctx.NewATEXT())
		lowerResult, lowerErr := ir.LowerAMD64(ctx, res.block, alloc)
		if lowerErr != nil {
			continue // lowering failed — skip
		}
		code, err := ctx.Assemble()
		if err != nil || len(code) == 0 {
			continue
		}

		compiles = append(compiles, &aotBlockCompile{
			startPC:     r.startPC,
			bytes:       code,
			lowerResult: lowerResult,
			baseOffset:  totalSize,
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
	for _, bc := range compiles {
		copy(execMem[bc.baseOffset:bc.baseOffset+len(bc.bytes)], bc.bytes)

		blockBase := codeBase + uintptr(bc.baseOffset)
		bc.blk = &compiledBlock{fn: blockBase}

		if bc.lowerResult.ChainEntryProg == nil {
			// V2 or debug variants don't produce ChainEntryProg; skip.
			blocks[bc.startPC] = bc.blk
			continue
		}
		bc.blk.chainEntry = blockBase + uintptr(bc.lowerResult.ChainEntryProg.Pc)

		// Backpatch chain-exit sentinels → slow-exit stub addresses.
		// Same as jitCompileWith but offsets are into the big mmap (add
		// bc.baseOffset); addresses land inside the same unified mmap.
		for _, ce := range bc.lowerResult.ChainExits {
			patchOff := bc.baseOffset + int(ce.MovProg.Pc) + 2
			stubAddr := blockBase + uintptr(ce.StubProg.Pc)
			binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(stubAddr))
			bc.blk.chainExits = append(bc.blk.chainExits, chainPatchInfo{
				targetPC:    ce.TargetPC,
				patchOffset: patchOff,
			})
		}

		// JALR IC sentinel init (same shape as existing backpatchJalrICs
		// but offsets rebased into the big mmap).
		aotBackpatchJalrICs(execMem, blockBase, bc.baseOffset, bc.lowerResult, bc.blk)

		blocks[bc.startPC] = bc.blk
	}

	// ── Pass 3: pre-resolve static chain exits whose target is in the segment ──
	// Writes the target's absolute chainEntry into each MOVABS imm64,
	// overwriting the slow-exit-stub address written in Pass 2. At run
	// time the chain JMP goes directly to the target block's chainEntry
	// with no Go round-trip or runtime patching.
	prePatches := 0
	for _, bc := range compiles {
		for _, ce := range bc.blk.chainExits {
			target, ok := blocks[ce.targetPC]
			if !ok || target.chainEntry == 0 {
				continue
			}
			binary.LittleEndian.PutUint64(execMem[ce.patchOffset:], uint64(target.chainEntry))
			prePatches++
		}
	}

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

	// mprotect the decoder_cache read-only. Guest cannot reach it via
	// its own ld/st anyway (different base pointer), but RO is
	// defense-in-depth against host-side bugs.
	if err := syscall.Mprotect(cacheMmap, syscall.PROT_READ); err != nil {
		return nil, fmt.Errorf("mprotect decoder_cache: %w", err)
	}

	seg := &DecodedExecuteSegment{
		vaddrBegin:       vaddrBegin,
		vaddrEnd:         vaddrEnd,
		nativeCodeBase:   codeBase,
		nativeCodeSize:   totalSize,
		decoderCacheMmap: cacheMmap,
		decoderCacheBase: uintptr(unsafe.Pointer(&cacheMmap[0])),
		decoderCacheMask: cacheSize - 1,
		blocks:           blocks,
	}

	if debugJIT {
		fmt.Printf("AOT: %d blocks compiled, %d bytes native code, "+
			"%d pre-patched chain exits, decoder_cache=%d bytes\n",
			len(compiles), totalSize, prePatches, cacheSize)
	}

	return seg, nil
}

// aotBackpatchJalrICs initializes JALR IC sentinel slots for one
// AOT-compiled block, writing patch offsets relative to the big
// mmap (not the block's own buffer). Parallel to backpatchJalrICs
// in jit_native.go but accepts a baseOffset for rebasing.
func aotBackpatchJalrICs(
	execMem []byte,
	blockBase uintptr,
	baseOffset int,
	lowerResult *ir.LowerResult,
	blk *compiledBlock,
) {
	for _, ic := range lowerResult.JalrICs {
		var info jalrICPatchInfo
		info.siteIdx = ic.SiteIdx
		stubAddr := blockBase + uintptr(ic.StubProg.Pc)
		for k := 0; k < 2; k++ {
			if ic.PcMov[k] == nil || ic.FnMov[k] == nil {
				continue
			}
			pcOff := baseOffset + int(ic.PcMov[k].Pc) + 2
			fnOff := baseOffset + int(ic.FnMov[k].Pc) + 2
			info.pcPatchOff[k] = pcOff
			info.fnPatchOff[k] = fnOff
			binary.LittleEndian.PutUint64(execMem[pcOff:], ^uint64(0))
			binary.LittleEndian.PutUint64(execMem[fnOff:], uint64(stubAddr))
		}
		blk.jalrICs = append(blk.jalrICs, info)
	}
}
