package riscv

// jit.go — JIT manager: block cache, RunJIT dispatch loop.

import (
	"encoding/binary"
	"fmt"
	"os"
	"riscv/abjit"
	"riscv/internal/jitcall"
	"riscv/ir"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// debugJIT enables diagnostic logging in emitBlock.
var debugJIT bool

// SetDebugJIT enables/disables emitBlock diagnostic logging.
func SetDebugJIT(on bool) { debugJIT = on }

// chainPatchInfo describes a chain exit that can be patched by Go.
type chainPatchInfo struct {
	targetPC    uint64 // guest PC this exit targets
	patchOffset int    // byte offset of imm64 in MOVABS within the code page
}

// jalrICPatchInfo describes a 2-way JALR inline-cache site. Slot [k]
// holds a (cache_pc, cache_fn) pair; cache_pc is the cached target PC
// and cache_fn is the corresponding block's chainEntry. Shift-policy
// tryPatchJalrIC fills slot 0 with the most recent miss target,
// demoting the old slot-0 contents to slot 1.
//
// A miss counter per site drives thrash deopt: after
// jalrICDeoptThreshold patches, the site is considered polymorphic
// (rotating across >2 targets) and further patching stops. The inline
// check still runs — sites cache whatever they last held — but the
// self-modifying-code (SMC) stalls caused by repeated patching stop.
type jalrICPatchInfo struct {
	siteIdx     int
	pcPatchOff  [2]int // byte offsets of cache_pc[0], cache_pc[1]
	fnPatchOff  [2]int // byte offsets of cache_fn[0], cache_fn[1]
	missStreak  uint32 // total patch attempts for this site
}

// jalrICDeoptThreshold is the number of miss-patches a JALR IC site
// will accept before it gives up and stops patching. Rationale: a
// monomorphic site settles with 1 patch; a bi-modal site with 2.
// Anything that keeps missing past this threshold is almost certainly
// 3+ target rotating — patching just thrashes the I-cache without
// buying hit rate.
const jalrICDeoptThreshold uint32 = 16

// DecodedExecuteSegment is a contiguous guest-VA executable region
// with its own AOT-compiled native code and decoder_cache. Mirrors
// libriscv's DecodedExecuteSegment<W> (xendor/libriscv/lib/libriscv/
// decoded_exec_segment.hpp:14-150). In Phase 2a, exactly one segment
// exists (covering the ELF .text); Phase 2b will add dynamic
// segments for guest-generated code (LuaJIT, V8).
//
// The decoder_cache is a flat array indexed by
// (pc - vaddrBegin) >> 1 holding uintptr chainEntry values (our
// equivalent of libriscv's DecoderData.m_bytecode handler). Slot = 0
// means no translation exists for that PC (either an untranslatable
// block inside the segment, or a mid-block PC that is not a BB entry).
//
// The decoder_cache lives in its own mmap, separate from main guest
// memory. It is mprotect'd PROT_READ after population — neither a
// JIT bug nor a hostile guest can corrupt it. Guest ld/st use the
// main guest-memory base (R14) and cannot reach the decoder_cache.
type DecodedExecuteSegment struct {
	vaddrBegin       uint64                    // guest VA start
	vaddrEnd         uint64                    // guest VA end (exclusive)
	vaddrSize        uint64                    // = vaddrEnd - vaddrBegin; pre-computed for hot-path reads
	nativeCodeBase   uintptr                   // first byte of unified code mmap
	nativeCodeSize   int                       // total bytes in code mmap
	nativeCodeMmap   []byte                    // same slab as nativeCodeBase; held for Munmap
	decoderCacheMmap []byte                    // DecoderData[] mmap (RO post-init)
	decoderCacheBase uintptr                   // = &decoderCacheMmap[0]
	decoderCacheMask uint64                    // power-of-two - 1
	blocks           map[uint64]*compiledBlock // PC → block (AOT + any lazy additions)

	// isLikelyJIT is true when this segment backs guest pages that are
	// writable+executable — i.e., the guest might overwrite code within
	// it (LuaJIT-style). Mirrors libriscv's m_is_likely_jit. Consumed by
	// future Phase 2c features (FENCE.I opt-in invalidation, stale
	// detection on mprotect -X). Ignored in Phase 2b dispatch.
	isLikelyJIT bool

	// refcount gates native-code + decoder_cache mmap release. Starts at
	// 1 on install; every (future) sharer Retain()s; destroying a holder
	// Release()s. When it reaches 0, both mmaps are Munmap'd. Segments
	// are immutable after install (blocks map read-only, decoder_cache
	// mprotect RO, native code already patched), so sharing is safe.
	refcount atomic.Int32
}

// Retain increments the segment's refcount. Matches libriscv's
// shared_ptr semantics for segments shared across forked Machines.
// No-op if seg is nil.
func (seg *DecodedExecuteSegment) Retain() {
	if seg == nil {
		return
	}
	seg.refcount.Add(1)
}

// Release decrements the segment's refcount and, on reaching zero,
// munmaps the native code and decoder_cache backing stores. The
// segment must not be used after the final Release.
// No-op if seg is nil.
func (seg *DecodedExecuteSegment) Release() {
	if seg == nil {
		return
	}
	if seg.refcount.Add(-1) != 0 {
		return
	}
	if len(seg.nativeCodeMmap) > 0 {
		_ = syscall.Munmap(seg.nativeCodeMmap)
		seg.nativeCodeMmap = nil
		seg.nativeCodeBase = 0
		seg.nativeCodeSize = 0
	}
	if len(seg.decoderCacheMmap) > 0 {
		_ = syscall.Munmap(seg.decoderCacheMmap)
		seg.decoderCacheMmap = nil
		seg.decoderCacheBase = 0
	}
}

// compiledBlock holds a compiled function pointer produced by the native
// IR pipeline.
type compiledBlock struct {
	fn         uintptr           // native function pointer
	chainEntry uintptr           // entry point for chaining
	chainExits []chainPatchInfo  // chain exits for patching
	jalrICs    []jalrICPatchInfo // JALR IC sites for patching
	hasFP      bool              // block uses FP registers (skip f[] copy when false)

	// segment is the DecodedExecuteSegment that owns this block's native
	// code, or nil for lazy-compiled blocks. Set at AOT install time;
	// RunJIT reads it to publish the right decoder_cache parameters to
	// CallAOT's sret extension.
	segment *DecodedExecuteSegment

	// nativeMmap is the per-block code slab for lazy-compiled blocks.
	// nil for AOT blocks (their code lives in segment.nativeCodeMmap,
	// reclaimed by segment.Release). Held here so JIT.Close can munmap.
	nativeMmap []byte
}

// JIT status codes returned by compiled blocks.
const (
	jitOK         = 0
	jitEcall      = 1
	jitEbreak     = 2
	jitLoadFault  = 3
	jitStoreFault = 4
	jitIllegal    = 5
	// jitOKJalrMiss is emitted by a JALR-IC miss stub. sret.PC holds the
	// target PC; sret.FaultAddr (repurposed) holds the site index. Go
	// dispatcher patches the site's IC slots, then dispatch continues.
	// Must agree with ir.JitOKJalrMiss.
	jitOKJalrMiss = ir.JitOKJalrMiss
	// jitMisalign: JIT block hit a misaligned memory access it can't handle
	// inline. sret.PC = faulting instruction's PC. Dispatcher re-executes
	// via the interpreter (which does byte-by-byte), then continues.
	jitMisalign = ir.JitMisalign
)

// Block cache: direct-mapped array replaces map[uint64]*compiledBlock.
// Eliminates Go map hash+probe overhead (~20-30ns) per dispatch cycle.
const (
	blockCacheShift = 12                   // 4096 entries
	blockCacheSize  = 1 << blockCacheShift // must be power of 2
	blockCacheMask  = blockCacheSize - 1
)

type blockCacheEntry struct {
	pc  uint64
	blk *compiledBlock
}

// JIT holds the cache of compiled basic blocks.
type JIT struct {
	// aotSegments holds all AOT-compiled segments installed so far
	// (one per PT_LOAD R-X in the ELF, plus any dynamically-created
	// segments from guest JIT-style code). Empty in pure lazy mode.
	// Dispatch consults findSegment(pc) first; on miss, falls through
	// to the direct-mapped lazy cache below.
	aotSegments []*DecodedExecuteSegment

	// hotSegment caches the segment most recently matched by
	// findSegment. Most dispatches stay within one segment (tight loops,
	// calls within the same function), so a single-pointer hot-cache
	// short-circuits the linear scan of aotSegments.
	hotSegment *DecodedExecuteSegment

	// soleSegment is aotSegments[0] when exactly one segment is installed,
	// else nil. Maintained as an invariant by refreshSoleSegment at every
	// mutation site. Enables RunJIT/lookupBlock fast paths that skip
	// findSegment and the blk.segment null-chain — the common case for
	// single-PT_LOAD ELFs (coremark, dhrystone, bench_guest).
	soleSegment *DecodedExecuteSegment

	// lazyBlocks holds every lazy-compiled block whose nativeMmap is
	// non-nil. Grown via insertBlock; drained by Close(), which munmaps
	// each block's nativeMmap. Bounded in practice by the number of
	// distinct PCs ever lazily compiled in this JIT's lifetime; blocks
	// remain live for chain-exit pin safety (patches in other blocks
	// may still target their native code).
	lazyBlocks []*compiledBlock

	cache      [blockCacheSize]blockCacheEntry
	noJIT      map[uint64]bool // PCs where translation failed — don't retry
	InterpOnly bool            // debug: force all-interpreter mode
	trace      bool            // debug: log block executions to stderr

	// DisableAutoAOT opts out of RunJIT's first-entry auto-install of
	// AOT segments based on cpu.mem.ExecRegions(). Set to true to force
	// the lazy compile path — used by benchmarks that measure the
	// lazy-vs-AOT gap and by tests that want to drive the fallback path.
	DisableAutoAOT bool

	irAlloc   ir.RegAllocator
	regPolicy  ir.RegPolicy
	useABJIT   bool
	abjitState *abjit.State

	// Dispatch counters (for diagnostics).
	DispatchOK       uint64 // jitOK returns to Go dispatch
	DispatchOther    uint64 // non-OK returns (ecall, fault, etc.)
	DispatchInterp   uint64 // interpreter fallback
	DispatchCompile  uint64 // new block compilations
	ChainPatched     uint64 // chain exits successfully patched
	ChainPatchedJalr uint64 // JALR IC sites successfully patched
	JalrICMisses     uint64 // JALR IC returns to Go (site not warm or polymorphic)
	JalrICDeopts     uint64 // JALR IC sites that crossed the deopt threshold
}

// NewJIT creates a new JIT translation cache using the Fixed Static Mapping allocator.
func NewJIT() *JIT {
	return &JIT{
		noJIT:     make(map[uint64]bool),
		irAlloc:   ir.NewFixedStaticAllocator(),
		regPolicy: ir.PolicyRV8,
	}
}

// SetAllocStrategy reinstalls the Fixed Static Mapping allocator and clears
// cached blocks, so existing callers continue to work.
func (j *JIT) SetAllocStrategy(name string) {
	j.irAlloc = ir.NewFixedStaticAllocator()
	// Clear block cache — compiled blocks used the old allocator.
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.noJIT = make(map[uint64]bool)
}

// SetRegPolicy switches the register allocation policy and clears
// cached blocks (they were compiled with the old policy).
func (j *JIT) SetRegPolicy(p ir.RegPolicy) {
	j.regPolicy = p
	j.useABJIT = p.Name == "abjit"
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.noJIT = make(map[uint64]bool)
}

// NoJITSize returns the number of PCs in the noJIT set (translation failures).
func (j *JIT) NoJITSize() int { return len(j.noJIT) }

// InstallAOT runs the whole-program AOT translator on the ELF bytes.
// For every PT_LOAD segment with PF_X set, it registers an ExecRegion
// on the guest memory, linearly scans the range to enumerate basic-
// block ranges, batch-compiles every translatable block into one
// unified native-code mmap per PT_LOAD, pre-resolves every static
// chain exit whose target is in the AOT set, and builds a mask-
// bounded read-only decoder_cache. The resulting segments are
// appended to j.aotSegments.
//
// Safe to call on a fresh JIT; installing twice appends additional
// segments (the old mmaps are retained).
//
// Returns nil if the ELF has no PT_LOAD R-X entries (callers can still
// use the lazy path in that case). Individual segments that fail to
// compile are skipped; other segments still install.
func (j *JIT) InstallAOT(mem *GuestMemory, elfBytes []byte) error {
	loads, ok := FindExecLoads(elfBytes)
	if !ok || len(loads) == 0 {
		return nil
	}
	for _, load := range loads {
		begin := load.VAddr
		end := load.VAddr + load.MemSz
		mem.AddExecRegion(begin, end, load.Writable)
		j.compileAOTRegion(mem, begin, end, load.MemSz, load.Writable)
	}
	j.refreshSoleSegment()
	return nil
}

// InstallAOTFromMem runs the AOT translator on every executable region
// already registered on mem (e.g., set up by LoadELFBytes). This is the
// path RunJIT calls automatically on first entry, so callers who load
// an ELF through LoadELFBytes get whole-program AOT without having to
// invoke InstallAOT explicitly. Does nothing if mem has no ExecRegions.
//
// Individual segments that fail to compile are skipped silently — the
// lazy compile path covers them at runtime.
func (j *JIT) InstallAOTFromMem(mem *GuestMemory) error {
	regions := mem.ExecRegions()
	if len(regions) == 0 {
		return nil
	}
	for _, r := range regions {
		if r.VAddrEnd <= r.VAddrBegin {
			continue
		}
		size := r.VAddrEnd - r.VAddrBegin
		j.compileAOTRegion(mem, r.VAddrBegin, r.VAddrEnd, size, r.IsLikelyJIT)
	}
	j.refreshSoleSegment()
	return nil
}

// compileAOTRegion is the shared body of InstallAOT and
// InstallAOTFromMem: enumerate blocks in [begin, end), compile them as
// one segment, and record it. Silently skips on failure so other
// regions still install.
func (j *JIT) compileAOTRegion(mem *GuestMemory, begin, end, size uint64, writable bool) {
	ranges := enumerateBlockRanges(mem, begin, size)
	seg, err := j.jitCompileAOTSegment(mem, ranges, begin, end)
	if err != nil {
		return
	}
	seg.isLikelyJIT = writable
	j.aotSegments = append(j.aotSegments, seg)
}

// CloneShared returns a new JIT that shares j's AOT segments (each
// Retain'd) but has its own lazy block cache. Safe to install more
// AOT segments or lazy-compile blocks in the clone without affecting
// j, because Phase 2b segments are structurally immutable after
// install (blocks map read-only, decoder_cache mprotect RO, native
// code already patched).
//
// The clone's debug flags (InterpOnly, UseV2, DebugV1V2, trace) start
// at zero values; set them on the returned JIT if desired. Counters
// also start at zero so the clone gets its own measurement baseline.
//
// The noJIT failure set starts empty in the clone; it may re-discover
// untranslatable PCs the parent already found, at tiny cost. The
// child's lazyBlocks registry is also empty — lazy mmaps are per-JIT
// and stay with the JIT that owns them until its Close.
func (j *JIT) CloneShared() *JIT {
	child := &JIT{
		aotSegments: append([]*DecodedExecuteSegment(nil), j.aotSegments...),
		noJIT:       make(map[uint64]bool),
		irAlloc:     j.irAlloc,   // stateless; sharing is safe
		regPolicy:   j.regPolicy, // struct copy; function pointers are safe to share
		useABJIT:    j.useABJIT,
	}
	for _, s := range child.aotSegments {
		s.Retain()
	}
	child.refreshSoleSegment()
	return child
}

// Close releases every AOT segment owned by this JIT and munmaps
// every lazy-compiled block's native code. Safe to call multiple
// times; subsequent calls are no-ops. After Close, the JIT must not
// dispatch — the native code mmaps are gone.
func (j *JIT) Close() {
	for _, s := range j.aotSegments {
		s.Release()
	}
	j.aotSegments = nil
	j.hotSegment = nil
	j.soleSegment = nil

	for _, blk := range j.lazyBlocks {
		if len(blk.nativeMmap) > 0 {
			_ = syscall.Munmap(blk.nativeMmap)
			blk.nativeMmap = nil
			blk.fn = 0
		}
	}
	j.lazyBlocks = nil
	j.abjitState = nil
}

// InvalidateSegment removes the segment containing pc from the
// dispatch set and Release()s it. On the next JALR/dispatch into the
// same region, nextExecuteSegment will re-create a fresh segment from
// the current guest memory contents (mirrors libriscv's
// evict_execute_segment + next_execute_segment flow).
//
// Returns true if a segment was invalidated, false if pc was not in
// any segment.
//
// Caveat: existing lazy blocks or other AOT segments may hold chain-
// exit pointers or JALR IC entries referencing the invalidated
// segment's native code. Release() munmaps it, so those pointers are
// dangling. Phase 2b uses this API only in controlled test scenarios
// where no such references exist. Phase 2c will add cross-segment
// reference tracking for safe runtime invalidation.
func (j *JIT) InvalidateSegment(pc uint64) bool {
	for i, s := range j.aotSegments {
		if pc >= s.vaddrBegin && pc < s.vaddrEnd {
			j.aotSegments = append(j.aotSegments[:i], j.aotSegments[i+1:]...)
			if j.hotSegment == s {
				j.hotSegment = nil
			}
			j.refreshSoleSegment()
			j.clearBlockCache()
			s.Release()
			return true
		}
	}
	return false
}

// InvalidateExecRegion invalidates every AOT segment whose range
// intersects [begin, end). Useful when the guest (or an OS personality
// hook) un-maps or downgrades permissions on a range. See
// InvalidateSegment for the same lazy-reference caveat.
//
// Returns the number of segments invalidated.
func (j *JIT) InvalidateExecRegion(begin, end uint64) int {
	if begin >= end {
		return 0
	}
	kept := j.aotSegments[:0]
	freed := 0
	for _, s := range j.aotSegments {
		if s.vaddrEnd <= begin || s.vaddrBegin >= end {
			kept = append(kept, s)
			continue
		}
		if j.hotSegment == s {
			j.hotSegment = nil
		}
		s.Release()
		freed++
	}
	j.aotSegments = kept
	if freed > 0 {
		j.refreshSoleSegment()
		j.clearBlockCache()
	}
	return freed
}

// clearBlockCache zeros the direct-mapped lazy cache. Called on
// segment invalidation so stale lookups don't return a block whose
// chain-exits point into freed mmaps.
func (j *JIT) clearBlockCache() {
	j.cache = [blockCacheSize]blockCacheEntry{}
}

// refreshSoleSegment maintains the soleSegment invariant: points at
// aotSegments[0] iff exactly one segment is installed, else nil. Call
// after every mutation of aotSegments (InstallAOT, Close,
// InvalidateSegment, InvalidateExecRegion, nextExecuteSegment).
func (j *JIT) refreshSoleSegment() {
	if len(j.aotSegments) == 1 {
		j.soleSegment = j.aotSegments[0]
	} else {
		j.soleSegment = nil
	}
}

// findSegment returns the AOT segment containing pc, or nil if pc
// falls outside every installed segment. Uses a one-slot hot cache
// since consecutive dispatches almost always stay within one segment.
func (j *JIT) findSegment(pc uint64) *DecodedExecuteSegment {
	if s := j.hotSegment; s != nil && pc >= s.vaddrBegin && pc < s.vaddrEnd {
		return s
	}
	for _, s := range j.aotSegments {
		if pc >= s.vaddrBegin && pc < s.vaddrEnd {
			j.hotSegment = s
			return s
		}
	}
	return nil
}

// SetTrace enables/disables trace logging to stderr.
func (j *JIT) SetTrace(on bool) { j.trace = on }

// cacheIdx computes the direct-mapped cache index for a PC.
// Shift right by 1 (not 2) because RVC instructions are 2-byte aligned.
func cacheIdx(pc uint64) uint64 {
	return (pc >> 1) & blockCacheMask
}

// lookupBlock returns the compiled block for pc, or nil.
//
// Dispatch order:
//  1. If pc falls inside an installed AOT segment, look up the block
//     in that segment's blocks map.
//  2. Otherwise (or AOT miss), consult the direct-mapped lazy cache.
//
// This preserves correctness for JALRs landing outside every segment
// (e.g., LuaJIT-style dynamic code before its segment is created —
// reached via the lazy path or, in Step 6, via nextExecuteSegment).
func (j *JIT) lookupBlock(pc uint64) *compiledBlock {
	if s := j.soleSegment; s != nil {
		// Fast path: exactly one segment installed. Inline bounds check
		// avoids the findSegment call + hotSegment indirection.
		if pc >= s.vaddrBegin && pc < s.vaddrEnd {
			if blk, ok := s.blocks[pc]; ok {
				return blk
			}
		}
	} else if len(j.aotSegments) > 0 {
		if s := j.findSegment(pc); s != nil {
			if blk, ok := s.blocks[pc]; ok {
				return blk
			}
		}
	}
	idx := cacheIdx(pc)
	if j.cache[idx].pc == pc {
		return j.cache[idx].blk
	}
	return nil
}

// insertBlock stores a compiled block in the cache. If blk owns its
// own native-code mmap (set by the lazy-compile path), the block is
// also registered in j.lazyBlocks so JIT.Close can munmap it. TCC and
// AOT blocks don't set nativeMmap and are not registered here.
func (j *JIT) insertBlock(pc uint64, blk *compiledBlock) {
	idx := cacheIdx(pc)
	j.cache[idx] = blockCacheEntry{pc, blk}
	if blk != nil && blk.nativeMmap != nil {
		j.lazyBlocks = append(j.lazyBlocks, blk)
	}
}

// StepBlock executes one dispatch cycle and returns.
func (j *JIT) StepBlock(cpu *CPU) (ic uint64, err error) {
	pc := cpu.pc

	blk := j.lookupBlock(pc)
	if blk != nil {
		res := sandboxCall(blk.fn, cpu,
			cpu.mem.RegFileBase(), cpu.mem.StackTop(),
			0, 0, 0, 0)
		if j.trace {
			fmt.Fprintf(os.Stderr, "JIT pc=0x%x -> PC=0x%x IC=%d status=%d\n",
				pc, res.PC, res.IC, res.Status)
		}
		cpu.pc = res.PC
		cpu.cycle += res.IC

		switch int(res.Status) {
		case jitOK:
			return res.IC, nil
		case jitOKJalrMiss:
			return res.IC, nil
		case jitMisalign:
			if err := cpu.step(); err != nil {
				return res.IC + 1, err
			}
			cpu.cycle++
			return res.IC + 1, nil
		case jitEcall:
			if cpu.mtvec != 0 {
				cpu.mepc = cpu.pc
				cpu.mcause = 8
				cpu.mtval = 0
				cpu.pc = cpu.mtvec
				return res.IC, nil
			}
			return res.IC, ErrEcall
		case jitEbreak:
			return res.IC, ErrEbreak
		case jitLoadFault:
			return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
		case jitStoreFault:
			return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
		default:
			err = cpu.step()
			cpu.cycle++
			return res.IC + 1, err
		}
	}

	// Try to translate
	if !j.InterpOnly && !j.noJIT[pc] {
		res := emitBlock(&cpu.mem, pc)
		if res != nil && res.numInsns > 0 {
			compiled, cerr := j.jitCompile(res)
			if cerr == nil {
				j.insertBlock(pc, compiled)
				return j.StepBlock(cpu) // retry with compiled block
			}
		}
		j.noJIT[pc] = true
	}

	// Interpreter fallback
	err = cpu.step()
	cpu.cycle++
	return 1, err
}

// stepBlockDebugV1V2 runs a block through both V1 and V2, compares all
// register outputs, and panics with full diagnostics on first mismatch.
// The V1 result is used to update cpu state (it's the production path).
func (j *JIT) stepBlockResult(_ *CPU, res jitcall.Result) (uint64, error) {
	switch int(res.Status) {
	case jitOK:
		return res.IC, nil
	case jitEcall:
		return res.IC, ErrEcall
	case jitEbreak:
		return res.IC, ErrEbreak
	case jitLoadFault:
		return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
	case jitStoreFault:
		return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
	default:
		return res.IC, nil
	}
}

// RunJIT executes the CPU using JIT-compiled blocks where possible,
// falling back to the interpreter for untranslatable instructions.
func (j *JIT) RunJIT(cpu *CPU) error {
	// AOT is the default: on the first RunJIT call for a JIT that has
	// no segments yet, transparently translate every executable region
	// the loader already registered on cpu.mem. Only PCs outside those
	// regions (self-modifying code, guest-generated blocks, tests that
	// built a raw mem) fall back to the lazy compile path below.
	// Set DisableAutoAOT on the JIT to force the lazy path end-to-end.
	if !j.DisableAutoAOT && len(j.aotSegments) == 0 && len(cpu.mem.ExecRegions()) > 0 {
		_ = j.InstallAOTFromMem(&cpu.mem)
	}

	for {
		// Tohost polling — once per dispatch cycle (block granularity).
		if cpu.watchAddr != 0 {
			if v, _ := cpu.mem.Load64(cpu.watchAddr); v != 0 {
				panic(&ExitError{Code: tohostExitCode(v)})
			}
		}

		pc := cpu.pc

		blk := j.lookupBlock(pc)
		if blk != nil {
			var res jitcall.Result
			if j.useABJIT {
				var dcBase uintptr
				var dcMask, vBegin, segSz uint64
				if seg := j.soleSegment; seg != nil {
					dcBase = seg.decoderCacheBase
					dcMask = seg.decoderCacheMask
					vBegin = seg.vaddrBegin
					segSz = seg.vaddrSize
				} else if len(j.aotSegments) > 0 {
					seg := blk.segment
					if seg == nil {
						seg = j.hotSegment
					}
					if seg == nil {
						seg = j.aotSegments[0]
					}
					dcBase = seg.decoderCacheBase
					dcMask = seg.decoderCacheMask
					vBegin = seg.vaddrBegin
					segSz = seg.vaddrSize
				}
				res = abjitDispatch(blk, cpu, j, dcBase, dcMask, vBegin, segSz)
			} else {
				regFile := cpu.mem.RegFileBase()
				stackTop := cpu.mem.StackTop()
				if seg := j.soleSegment; seg != nil {
					res = sandboxCall(blk.fn, cpu, regFile, stackTop,
						seg.decoderCacheBase, seg.decoderCacheMask,
						seg.vaddrBegin, seg.vaddrSize)
				} else if len(j.aotSegments) > 0 {
					seg := blk.segment
					if seg == nil {
						seg = j.hotSegment
						if seg == nil {
							seg = j.aotSegments[0]
						}
					}
					res = sandboxCall(blk.fn, cpu, regFile, stackTop,
						seg.decoderCacheBase, seg.decoderCacheMask,
						seg.vaddrBegin, seg.vaddrSize)
				} else {
					res = sandboxCall(blk.fn, cpu, regFile, stackTop,
						0, 0, 0, 0)
				}
			}
			cpu.pc = res.PC
			cpu.cycle += res.IC

			switch int(res.Status) {
			case jitOK:
				j.DispatchOK++
				// Patch this block's chain exit to jump directly to the target.
				// When a chain exit isn't patched, the slow stub returns here.
				// After patching, future executions jump directly — bypassing Go.
				if len(blk.chainExits) > 0 {
					j.tryPatchChain(blk, cpu.pc)
				}
				continue

			case jitOKJalrMiss:
				// JALR inline-cache miss: sret.PC = computed target PC (already
				// written to cpu.pc above); sret.FaultAddr = site index.
				// Patch the site so the next JALR to the same target takes the
				// direct-jump path, then continue dispatch.
				j.JalrICMisses++
				j.tryPatchJalrIC(blk, int(res.FaultAddr), cpu.pc)
				continue

			case jitMisalign:
				// Misaligned access: re-execute the faulting instruction via
				// the interpreter (which handles misalignment with byte-by-byte
				// reads/writes), then continue JIT dispatch.
				if err := cpu.step(); err != nil {
					return err
				}
				cpu.cycle++
				continue

			case jitEcall:
				if cpu.mtvec != 0 {
					cpu.mepc = cpu.pc
					cpu.mcause = 8
					cpu.mtval = 0
					cpu.pc = cpu.mtvec
					continue
				}
				n := noteFromStepErr(ErrEcall, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return ErrEcall
				}

			case jitEbreak:
				n := noteFromStepErr(ErrEbreak, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return ErrEbreak
				}

			case jitLoadFault:
				f := &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
				n := noteFromStepErr(f, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return f
				}

			case jitStoreFault:
				f := &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
				n := noteFromStepErr(f, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return f
				}

			default:
				err := cpu.step()
				cpu.cycle++
				if err == nil {
					continue
				}
				n := noteFromStepErr(err, cpu.PC())
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return err
				}
			}
		}

		// No compiled block. If pc falls inside a registered ExecRegion
		// that isn't yet covered by any AOT segment (e.g., the guest
		// mmapped a new R-X region and jumped to it — LuaJIT pattern),
		// build a segment for it now. Re-try dispatch on success.
		// DisableAutoAOT opts out — benchmarks and tests that measure
		// the lazy path need to prevent on-demand AOT too.
		if !j.InterpOnly && !j.DisableAutoAOT && len(cpu.mem.execRegions) > 0 {
			if seg := j.nextExecuteSegment(&cpu.mem, pc); seg != nil {
				if _, ok := seg.blocks[pc]; ok {
					continue // retry — next lookupBlock hits the new AOT block
				}
			}
		}

		// No compiled block — try to translate.
		if !j.InterpOnly && !j.noJIT[pc] {
			res := emitBlock(&cpu.mem, pc)
			if res != nil && res.numInsns > 0 {
				blk, err := j.jitCompile(res)
				if err == nil {
					j.DispatchCompile++
					j.insertBlock(pc, blk)
					continue
				}
				if debugJIT {
					fmt.Fprintf(os.Stderr, "COMPILE_FAIL pc=0x%x numInsns=%d err=%v\n", pc, res.numInsns, err)
				}
			} else if debugJIT {
				if res == nil {
					fmt.Fprintf(os.Stderr, "EMIT_NIL pc=0x%x\n", pc)
				} else {
					fmt.Fprintf(os.Stderr, "EMIT_ZERO pc=0x%x numInsns=%d\n", pc, res.numInsns)
				}
			}
			j.noJIT[pc] = true
		}

		// Interpret one instruction.
		j.DispatchInterp++
		err := cpu.step()
		cpu.cycle++
		if err == nil {
			continue
		}
		n := noteFromStepErr(err, cpu.PC())
		switch cpu.Notes.Deliver(cpu, n) {
		case NoteHandled:
			continue
		default:
			return err
		}
	}
}

// patchChainTarget overwrites the 8-byte imm64 in a MOVABS instruction
// at codeBase+patchOffset to redirect to targetAddr.
//
//go:nosplit
func patchChainTarget(codeBase uintptr, patchOffset int, targetAddr uintptr) {
	//nolint:gosec // JIT code patching requires direct memory writes to RWX pages.
	p := (*[8]byte)(unsafe.Pointer(codeBase + uintptr(patchOffset))) //nolint:govet
	binary.LittleEndian.PutUint64(p[:], uint64(targetAddr))
}

// tryPatchChain patches a previous block's chain exit to jump directly
// to the target block, bypassing the Go dispatch loop on future executions.
func (j *JIT) tryPatchChain(blk *compiledBlock, targetPC uint64) {
	target := j.lookupBlock(targetPC)
	if target == nil || target.chainEntry == 0 {
		return
	}
	for _, ce := range blk.chainExits {
		if ce.targetPC == targetPC {
			patchChainTarget(blk.fn, ce.patchOffset, target.chainEntry)
			j.ChainPatched++
			break
		}
	}
}

// tryPatchJalrIC updates a JALR IC site with shift semantics: the
// previous slot-0 content is demoted to slot 1, and the new target is
// installed in slot 0. For a bi-modal call site `{A, B, A, B, ...}`,
// this converges to `{A, B}` after two misses and then hits 100%.
// For 3+ rotating targets the site still thrashes (expected; see
// plan "Priority 1.5").
//
// Called from the dispatch loop on jitOKJalrMiss returns.
func (j *JIT) tryPatchJalrIC(blk *compiledBlock, siteIdx int, targetPC uint64) {
	if siteIdx < 0 || siteIdx >= len(blk.jalrICs) {
		return
	}
	ic := &blk.jalrICs[siteIdx]
	if ic.missStreak >= jalrICDeoptThreshold {
		return // deopted site: stop patching; inline check still runs.
	}
	target := j.lookupBlock(targetPC)
	if target == nil || target.chainEntry == 0 {
		return
	}

	// Read current slot 0 so we can demote it to slot 1.
	pc0Old := readJalrICSlot(blk.fn, ic.pcPatchOff[0])
	fn0Old := readJalrICSlot(blk.fn, ic.fnPatchOff[0])

	// Shift: slot 1 ← slot 0 (demote), slot 0 ← new target (promote).
	patchChainTarget(blk.fn, ic.pcPatchOff[1], uintptr(pc0Old))
	patchChainTarget(blk.fn, ic.fnPatchOff[1], uintptr(fn0Old))
	patchChainTarget(blk.fn, ic.pcPatchOff[0], uintptr(targetPC))
	patchChainTarget(blk.fn, ic.fnPatchOff[0], target.chainEntry)
	j.ChainPatchedJalr++
	ic.missStreak++
	if ic.missStreak == jalrICDeoptThreshold {
		j.JalrICDeopts++
	}
}

// readJalrICSlot reads 8 bytes from JIT code memory at the given
// offset. Mirrors patchChainTarget's access pattern.
//
//go:nosplit
func readJalrICSlot(codeBase uintptr, patchOffset int) uint64 {
	//nolint:gosec // JIT code inspection requires direct memory reads.
	p := (*[8]byte)(unsafe.Pointer(codeBase + uintptr(patchOffset))) //nolint:govet
	return binary.LittleEndian.Uint64(p[:])
}
