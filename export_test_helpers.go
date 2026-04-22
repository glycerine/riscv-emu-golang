package riscv

import "riscv/ir"

type ExportedEmitResult struct {
	Block    *ir.Block
	StartPC  uint64
	EndPC    uint64
	NumInsns int
}

func ExportEmitBlock(mem *GuestMemory, pc uint64) *ExportedEmitResult {
	r := emitBlock(mem, pc)
	if r == nil {
		return nil
	}
	return &ExportedEmitResult{Block: r.block, StartPC: r.startPC, EndPC: r.endPC, NumInsns: r.numInsns}
}

func ExportEmitBlockLinear(mem *GuestMemory, startPC, endPC uint64) *ExportedEmitResult {
	r := emitBlockLinear(mem, startPC, endPC)
	if r == nil {
		return nil
	}
	return &ExportedEmitResult{Block: r.block, StartPC: r.startPC, EndPC: r.endPC, NumInsns: r.numInsns}
}

func ExportInstallManualBlock(j *JIT, mem *GuestMemory, startPC, endPC uint64) error {
	res := emitBlockLinear(mem, startPC, endPC)
	if res == nil {
		return nil
	}
	blk, err := j.jitCompileWith(res, false)
	if err != nil {
		return err
	}
	j.insertBlock(startPC, blk)
	return nil
}

type BlockRange struct {
	Start, End uint64
}

func ExportGetSoleSegment(j *JIT) *DecodedExecuteSegment { return j.soleSegment }

func ExportSegmentInfo(seg *DecodedExecuteSegment) (base uintptr, mask, vBegin, vSize uint64) {
	return seg.decoderCacheBase, seg.decoderCacheMask, seg.vaddrBegin, seg.vaddrSize
}

func ExportInstallAOTSegment(j *JIT, mem *GuestMemory, vaddrBegin, vaddrEnd uint64, ranges []BlockRange) error {
	var br []blockRange
	for _, r := range ranges {
		br = append(br, blockRange{startPC: r.Start, endPC: r.End})
	}
	seg, err := j.jitCompileAOTSegment(mem, br, vaddrBegin, vaddrEnd)
	if err != nil {
		return err
	}
	j.aotSegments = append(j.aotSegments, seg)
	j.refreshSoleSegment()
	return nil
}
