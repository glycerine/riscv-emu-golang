package riscv

// jit_segment.go — dynamic segment creation for JALR targets inside a
// registered exec region that isn't yet covered by an AOT segment.
//
// Mirrors libriscv's next_execute_segment(pc) in cpu.cpp:87–200.
// Whereas libriscv scans guest pages for a contiguous exec run, we
// consult the GuestMemory ExecRegion table (populated from PT_LOAD R-X
// at ELF load or by future os.go syscall hooks) — the permissions are
// already known to the emulator, so no page scan is required.

// nextExecuteSegment builds an AOT segment covering the ExecRegion
// that contains pc. Called from dispatch when lookupBlock(pc) misses
// *and* findSegment(pc) misses — i.e., pc is in guest code memory but
// no segment has been compiled for it yet.
//
// Returns the newly-installed segment on success, nil if pc is outside
// every registered ExecRegion or the compile failed. A nil return hands
// control to the existing lazy-compile path, which still produces a
// correct (if slower) translation for guests whose exec regions we
// haven't yet been told about.
func (j *JIT) nextExecuteSegment(mem *GuestMemory, pc uint64) *DecodedExecuteSegment {
	region := mem.FindExecRegion(pc)
	if region == nil {
		return nil
	}
	// Defensive: if a segment already covers this region (stale
	// findSegment call? concurrent install?), return it.
	if seg := j.findSegment(pc); seg != nil {
		return seg
	}
	// Snapshot the region (the returned pointer may be invalidated by
	// AddExecRegion churn later in this call; copy the fields we need).
	begin := region.VAddrBegin
	end := region.VAddrEnd
	isJIT := region.IsLikelyJIT
	size := end - begin

	ranges := enumerateBlockRanges(mem, begin, size)
	if len(ranges) == 0 {
		return nil
	}
	seg, err := j.jitCompileAOTSegment(mem, ranges, begin, end)
	if err != nil {
		return nil
	}
	seg.isLikelyJIT = isJIT
	j.aotSegments = append(j.aotSegments, seg)
	return seg
}
