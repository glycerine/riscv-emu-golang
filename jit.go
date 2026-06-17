package riscv

// jit.go — JIT manager: block cache, RunJIT dispatch loop.

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/abjit"
	"github.com/glycerine/riscv-emu-golang/internal/jitcall"
)

// debugJIT enables diagnostic logging in emitBlock.
var debugJIT = os.Getenv("GOCPU_DEBUG_JIT") != ""

// SetDebugJIT enables/disables emitBlock diagnostic logging.
func SetDebugJIT(on bool) {
	debugJIT = on
}

// chainPatchInfo describes a chain exit that can be patched by Go.
type chainPatchInfo struct {
	targetPC        uint64 // guest PC this exit targets
	patchOffset     int    // byte offset of backend safe-chain patch data
	livePatchOffset int    // optional byte offset of backend live-chain patch data
	liveChain       liveChainMeta
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
	siteIdx    int
	pcPatchOff [2]int // byte offsets of cache_pc[0], cache_pc[1]
	fnPatchOff [2]int // byte offsets of cache_fn[0], cache_fn[1]
	missStreak uint32 // total patch attempts for this site
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
// memory. It is intentionally mutable, like libriscv's DecoderData
// table: lazy blocks discovered after AOT install are published back
// into this table so native JALR dispatch can hit without returning
// to Go. Guest ld/st use the main guest-memory base (R14) and cannot
// reach the decoder_cache.
type DecodedExecuteSegment struct {
	vaddrBegin       uint64                    // guest VA start
	vaddrEnd         uint64                    // guest VA end (exclusive)
	vaddrSize        uint64                    // = vaddrEnd - vaddrBegin; pre-computed for hot-path reads
	nativeCodeBase   uintptr                   // first byte of unified code mmap
	nativeCodeSize   int                       // total bytes in code mmap
	nativeCodeMmap   []byte                    // same slab as nativeCodeBase; held for Munmap
	decoderCacheMmap []byte                    // DecoderData[] mmap; mutable by the owning JIT
	decoderCacheBase uintptr                   // = &decoderCacheMmap[0]
	decoderCacheMask uint64                    // power-of-two - 1
	blocks           map[uint64]*compiledBlock // PC → block (AOT + any lazy additions)

	// isLikelyJIT is true when this segment backs guest pages that are
	// writable+executable — i.e., the guest might overwrite code within
	// it (LuaJIT-style). Mirrors libriscv's m_is_likely_jit. Consumed by
	// future Phase 2c features (FENCE.I opt-in invalidation, stale
	// detection on mprotect -X). Ignored in Phase 2b dispatch.
	isLikelyJIT bool
}

// free releases the native code and decoder_cache backing stores.
// Segments are single-JIT owned; after free, neither the segment nor
// any block that points into it may be dispatched.
func (seg *DecodedExecuteSegment) free() {
	if seg == nil {
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
	fn             uintptr           // native function pointer
	chainEntry     uintptr           // entry point for chaining
	liveChainEntry uintptr           // entry after prologue/fixed ABI setup, before register-file loads
	liveChain      liveChainMeta     // conservative metadata for no-store ARM64 chaining
	chainExits     []chainPatchInfo  // chain exits for patching
	jalrICs        []jalrICPatchInfo // JALR IC sites for patching
	hasFP          bool              // block-local FP metadata; dispatch must still preserve FP across chains
	numInsns       int               // static instruction count from emission

	// segment is the DecodedExecuteSegment whose decoder_cache should be
	// used while running this block, or nil for pure lazy blocks outside
	// AOT regions. AOT blocks live in segment.nativeCodeMmap; lazy
	// additions inside an AOT region are backed by JIT.lazyCodeArena.
	segment *DecodedExecuteSegment

	// nativeMmap is non-nil only for legacy/debug blocks that own their own
	// code mmap. Normal lazy blocks are backed by JIT.lazyCodeArena; AOT
	// blocks live in segment.nativeCodeMmap.
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
	// Must agree with JitOKJalrMiss.
	jitOKJalrMiss = JitOKJalrMiss
	// jitMisalign: JIT block hit a misaligned memory access it can't handle
	// inline. sret.PC = faulting instruction's PC. Dispatcher re-executes
	// via the interpreter (which does byte-by-byte), then continues.
	jitMisalign = JitMisalign
	// jitBudget: native countdown budget gate fired. sret.PC is the
	// unexecuted guest PC and State.IC/result IC holds the remaining budget.
	jitBudget = 8
)

// Block cache: direct-mapped array replaces map[uint64]*compiledBlock.
// Eliminates Go map hash+probe overhead (~20-30ns) per dispatch cycle.
const (
	blockCacheShift = 12                   // 4096 entries
	blockCacheSize  = 1 << blockCacheShift // must be power of 2
	blockCacheMask  = blockCacheSize - 1
	jitMaxBudget    = ^uint64(0)
)

type blockCacheEntry struct {
	pc  uint64
	blk *compiledBlock
}

type JITInstructionCounterMode uint8

const (
	// JITICNone is retained for older callers. The native JIT no longer has
	// a no-counter emission mode: all emitted code uses R15 as a decreasing
	// guest-instruction budget, and Go derives instruction attempts at the
	// dispatch boundary.
	JITICNone JITInstructionCounterMode = iota

	// JITICPrecise is the only effective native JIT mode.
	JITICPrecise
)

func (m JITInstructionCounterMode) String() string {
	switch m {
	case JITICNone:
		return "none"
	case JITICPrecise:
		return "precise"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(m))
	}
}

// JITFallbackTraceEntry records one PC that the lazy JIT could not dispatch
// as native code and therefore had to hand to the interpreter.
type JITFallbackTraceEntry struct {
	PC          uint64
	Count       uint64
	Reason      string
	InExec      bool
	RegionBegin uint64
	RegionEnd   uint64
	IsRVC       bool
	Half        uint16
	Word        uint32
	Disasm      string
	FetchFault  string
}

// JIT holds the cache of compiled basic blocks.
type JIT struct {

	// unique jump labels (at least for the lifetime
	// of this JIT instance, to allow alot of
	// cross assembly jumping.
	lastLabelSerial int64

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

	// lazySegments are decoder-cache-only execute segments used by the
	// pure lazy JIT. They do not own a unified native-code slab; each
	// lazy block still owns its per-block nativeMmap. The segment exists
	// so native JALR/return dispatch can look up any already-compiled
	// lazy target through the same flat decoder_cache that AOT uses.
	lazySegments    []*DecodedExecuteSegment
	hotLazySegment  *DecodedExecuteSegment
	soleLazySegment *DecodedExecuteSegment

	// lazyBlocks holds every lazy-compiled block. Grown via insertBlock;
	// drained by Close(), which clears block entry pointers and frees the
	// shared lazyCodeArena. Bounded in practice by the number of distinct
	// PCs ever lazily compiled in this JIT's lifetime; blocks remain live
	// for chain-exit pin safety (patches in other blocks may still target
	// their native code).
	lazyBlocks []*compiledBlock

	// lazyBlockMap is the collision-safe backing store for lazy blocks.
	// lookupBlock hits the direct-mapped cache first, then this map on
	// misses. This keeps the common dispatch path cheap without losing
	// already-compiled blocks when larger guests collide in the 4096-slot
	// direct cache.
	lazyBlockMap map[uint64]*compiledBlock

	// lazyCodeArena is the single executable slab used by lazy block
	// compilation. It is sized once from the loaded ELF length, then every
	// lazy block is carved out linearly. This avoids one executable mmap per
	// compiled block.
	lazyCodeArena []byte
	lazyCodeOff   int

	cache      [blockCacheSize]blockCacheEntry
	noJIT      map[uint64]bool // PCs where translation failed — don't retry
	InterpOnly bool            // debug: force all-interpreter mode
	trace      bool            // debug: log block executions to stderr

	fallbackTrace map[uint64]*JITFallbackTraceEntry

	// AutoAOT opts into RunJIT's first-entry auto-install of
	// AOT segments based on cpu.mem.ExecRegions(). Leave false
	// use the lazy compile path — used by benchmarks that measure the
	// lazy-vs-AOT gap and by tests that want to drive the fallback path.
	AutoAOT bool

	// HotRegionThreshold promotes a registered executable region from
	// lazy block compilation to a full AOT segment after this many lazy
	// compiles inside the region. Zero disables promotion. This is an
	// explicit opt-in and still works when AutoAOT is false, so
	// callers can measure a lazy warm-up followed by segment execution.
	HotRegionThreshold uint32
	HotRegionsCompiled uint64
	hotRegionCounts    map[uint64]uint32

	irAlloc    RegAllocator
	regPolicy  RegPolicy
	useABJIT   bool
	abjitState *abjit.State

	faultPageZero bool

	stopperPage uintptr // InfiniteLoopStopperPage: mmap'd guard page for preemption
	watchAddr   uint64  // tohost address; JIT blocks exit when a store hits this address

	UseR15InstructionCounter  bool  // compatibility knob; R15 budget codegen is always enabled
	DebugOneBlockLockstepMode bool  // StepBlock uses LockstepModeBudget as its native dispatch budget
	LockstepModeBudget        int64 // max IC before forced exit (default 65536)

	// Dispatch counters (for diagnostics).
	DispatchOK         uint64 // jitOK returns to Go dispatch
	DispatchOther      uint64 // non-OK returns (ecall, fault, etc.)
	DispatchInterp     uint64 // no-block interpreter fallback dispatches
	DispatchCompile    uint64 // new block compilations
	DispatchBudget     uint64 // jitBudget returns to Go dispatch
	DispatchEcall      uint64 // jitEcall returns to Go dispatch
	DispatchEcallReal  uint64 // jitEcall returns whose trap PC is an ECALL instruction
	DispatchEcallStale uint64 // jitEcall returns whose trap PC is not an ECALL instruction
	DispatchFault      uint64 // native fault/misalignment/illegal returns to Go dispatch
	InterpretedInsns   uint64 // guest instruction attempts by JIT-owned interpreter fallback
	ICDeltaOK          uint64 // instruction-attempt delta reported by jitOK returns
	ICDeltaBudget      uint64 // instruction-attempt delta reported by jitBudget returns
	ICDeltaJalrMiss    uint64 // instruction-attempt delta reported by jitOKJalrMiss returns
	ICDeltaEcall       uint64 // instruction-attempt delta reported by jitEcall returns
	ICDeltaFault       uint64 // instruction-attempt delta reported by native fault returns
	ICDeltaOther       uint64 // instruction-attempt delta reported by other native returns
	ICDeltaMax         uint64 // largest instruction-attempt delta from one native dispatch
	ChainPatched       uint64 // chain exits successfully patched
	ChainPatchTry      uint64 // chain exits considered for patching
	ChainPatchNoTarget uint64 // chain patch attempts whose target block was absent
	ChainPatchNoMatch  uint64 // chain patch attempts with no matching static exit
	ChainPatchedJalr   uint64 // JALR IC sites successfully patched
	JalrICMisses       uint64 // JALR IC returns to Go (site not warm or polymorphic)
	JalrICDeopts       uint64 // JALR IC sites that crossed the deopt threshold

	AOTSegmentsInstalled uint64 // AOT segments successfully installed
	AOTBlocksInstalled   uint64 // AOT blocks in successfully installed segments
	AOTCompileFailures   uint64 // AOT region compile attempts that failed/skipped

	// AOT decoder-cache probes visible to the Go dispatcher. Native
	// decoder-cache jumps do not return to Go, so these are not total
	// native hit counts. They classify Go-observed dispatch PCs that
	// fall inside an installed AOT segment.
	AOTDecoderCacheLookups uint64
	AOTDecoderCacheHits    uint64
	AOTDecoderCacheMisses  uint64
	AOTDecoderCacheOutside uint64
}

// NewJIT creates a new JIT translation cache using the Fixed
// Static Mapping allocator. The current default register
// allocation policy is PolicyABJIT (compare PolicyRV8); see lower_amd64.go
func NewJIT() *JIT {
	j := &JIT{
		noJIT:                    make(map[uint64]bool),
		lazyBlockMap:             make(map[uint64]*compiledBlock),
		irAlloc:                  NewFixedStaticAllocator(),
		UseR15InstructionCounter: true,
		LockstepModeBudget:       65536,

		// faster to disable AOT? massively.
		// at: 18fcb35 (origin/oslayer, oslayer) atg on linux and darwin
		// GOCPU_VIZJIT_OFF=1 make bench-jit-coremark
		// AutoAOT: false =>
		// BenchmarkJIT_CoreMark_ABJIT-8 1  575_636_119 ns/op 676.8 MIPS  27835656 B/op     42_213 allocs/op (10x fewer allocations!)
		//  with AutoAOT: true =>
		// BenchmarkJIT_CoreMark_ABJIT-8 1 1_092_111_163 ns/op 356.8 MIPS  817350016 B/op    579_873 allocs/op

		AutoAOT: false,
	}
	j.SetRegPolicy(PolicyABJIT)
	if err := j.initStopperPage(); err != nil {
		panic("NewJIT: " + err.Error())
	}
	return j
}

// SetAllocStrategy reinstalls the Fixed Static Mapping allocator and clears
// cached blocks, so existing callers continue to work.
func (j *JIT) SetAllocStrategy(name string) {
	j.irAlloc = NewFixedStaticAllocator()
	// Clear block cache — compiled blocks used the old allocator.
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.lazyBlockMap = make(map[uint64]*compiledBlock)
	j.noJIT = make(map[uint64]bool)
}

// SetRegPolicy switches the register allocation policy and clears
// cached blocks (they were compiled with the old policy).
func (j *JIT) SetRegPolicy(p RegPolicy) {
	j.regPolicy = p
	j.useABJIT = p.Name == "abjit"
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.lazyBlockMap = make(map[uint64]*compiledBlock)
	j.noJIT = make(map[uint64]bool)
}

// NoJITSize returns the number of PCs in the noJIT set (translation failures).
func (j *JIT) NoJITSize() int { return len(j.noJIT) }

// SetInstructionCounterMode validates the legacy instruction-counter mode API.
// Native code generation no longer switches modes: every emitted block uses
// R15 as a decreasing budget and Go computes instruction attempts from the
// remaining value returned by the trampoline.
func (j *JIT) SetInstructionCounterMode(mode JITInstructionCounterMode) {
	switch mode {
	case JITICNone, JITICPrecise:
		j.UseR15InstructionCounter = true
	default:
		panic(fmt.Sprintf("unknown JIT instruction counter mode: %d", uint8(mode)))
	}
}

// InstructionCounterMode reports the effective mode. It always reports
// precise because the JIT has one countdown-budget codegen contract.
func (j *JIT) InstructionCounterMode() JITInstructionCounterMode {
	return JITICPrecise
}

// EnableFallbackTrace enables a low-volume histogram of lazy-JIT PCs that
// execute through the interpreter because no native block is available.
func (j *JIT) EnableFallbackTrace() {
	if j.fallbackTrace == nil {
		j.fallbackTrace = make(map[uint64]*JITFallbackTraceEntry)
	}
}

// FallbackTraceTop returns the fallback histogram sorted by descending count.
func (j *JIT) FallbackTraceTop(limit int) []JITFallbackTraceEntry {
	if len(j.fallbackTrace) == 0 || limit == 0 {
		return nil
	}
	out := make([]JITFallbackTraceEntry, 0, len(j.fallbackTrace))
	for _, ent := range j.fallbackTrace {
		out = append(out, *ent)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Count == out[k].Count {
			return out[i].PC < out[k].PC
		}
		return out[i].Count > out[k].Count
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (j *JIT) markNoJIT(mem *GuestMemory, pc uint64, reason string) {
	j.noJIT[pc] = true
	if j.fallbackTrace != nil {
		ent := j.fallbackTraceEntry(mem, pc)
		if ent.Reason == "" || ent.Reason == "nojit-hit" {
			ent.Reason = reason
		}
	}
}

func (j *JIT) recordInterpreterFallback(mem *GuestMemory, pc uint64) {
	if j.fallbackTrace == nil {
		return
	}
	ent := j.fallbackTraceEntry(mem, pc)
	ent.Count++
	if ent.Reason == "" {
		ent.Reason = "nojit-hit"
	}
}

func (j *JIT) fallbackTraceEntry(mem *GuestMemory, pc uint64) *JITFallbackTraceEntry {
	if ent := j.fallbackTrace[pc]; ent != nil {
		return ent
	}
	ent := newJITFallbackTraceEntry(mem, pc)
	j.fallbackTrace[pc] = ent
	return ent
}

func newJITFallbackTraceEntry(mem *GuestMemory, pc uint64) *JITFallbackTraceEntry {
	ent := &JITFallbackTraceEntry{PC: pc}
	if mem != nil {
		if r := mem.FindExecRegion(pc); r != nil {
			ent.InExec = true
			ent.RegionBegin = r.VAddrBegin
			ent.RegionEnd = r.VAddrEnd
		}
		half, fh := mem.Fetch16(pc)
		if fh != nil {
			ent.FetchFault = fh.Error()
		} else {
			ent.Half = half
			if half&0x3 != 0x3 {
				ent.IsRVC = true
				ent.Disasm = DisasmRVC(half)
			} else {
				word, fw := mem.Fetch32(pc)
				if fw != nil && fw.Kind == FaultMisalign {
					word, fw = mem.Fetch32U(pc)
				}
				if fw != nil {
					ent.FetchFault = fw.Error()
				} else {
					ent.Word = word
					ent.Disasm = DisasmRV32(pc, word)
				}
			}
		}
	}
	return ent
}

func lazyCompilePanicMessage(mem *GuestMemory, pc uint64, res *emitResult, err error) string {
	msg := fmt.Sprintf("lazy JIT native compile failed: pc=0x%x", pc)
	if res != nil {
		irCount := 0
		if res.block != nil {
			irCount = len(res.block.Instrs)
		}
		msg += fmt.Sprintf(" numInsns=%d ir=%d", res.numInsns, irCount)
	}
	if mem != nil {
		ent := newJITFallbackTraceEntry(mem, pc)
		if ent.InExec {
			msg += fmt.Sprintf(" exec=[0x%x,0x%x)", ent.RegionBegin, ent.RegionEnd)
		} else {
			msg += " exec=<none>"
		}
		if ent.FetchFault != "" {
			msg += " fetch=" + ent.FetchFault
		} else if ent.IsRVC {
			msg += fmt.Sprintf(" half=0x%04x insn=%s", ent.Half, ent.Disasm)
		} else {
			msg += fmt.Sprintf(" word=0x%08x insn=%s", ent.Word, ent.Disasm)
		}
	}
	if err != nil {
		msg += ": " + err.Error()
	}
	return msg
}

func (j *JIT) preciseInstructionCounterEnabled() bool {
	return true
}

func blockTouchesInstructionCounterReg(b *Block) bool {
	if b == nil {
		return false
	}
	for i := range b.Instrs {
		switch b.Instrs[i].Op {
		case IRZeroIC, IRLoadIC, IRIncIC, IRDecIC, IRSpillIC, IRRegBudget,
			IRBudgetZero, IRBudgetReserve, IRCall, IRSyscall, IRJalrIC:
			return true
		}
	}
	return false
}

func (j *JIT) reserveInstructionCounterReg(b *Block) bool {
	if j.regPolicy.InstructionCounterReg == 0 {
		return false
	}
	return j.preciseInstructionCounterEnabled() || blockTouchesInstructionCounterReg(b)
}

// SetHotRegionThreshold enables lazy-to-AOT promotion for executable
// regions. A value of zero disables promotion.
func (j *JIT) SetHotRegionThreshold(n uint32) {
	j.HotRegionThreshold = n
	if n == 0 {
		j.hotRegionCounts = nil
	}
}

// stepInterpreted executes exactly one guest instruction through the
// interpreter while the JIT dispatcher is active.
func (j *JIT) stepInterpreted(cpu *CPU) error {
	err := cpu.step()
	cpu.riscvInstrBegun++
	j.InterpretedInsns++
	return err
}

func (j *JIT) stepBlockDispatchBudget() uint64 {
	if j.DebugOneBlockLockstepMode && j.LockstepModeBudget > 0 {
		return uint64(j.LockstepModeBudget)
	}
	return jitMaxBudget
}

// StepBlockBudget executes JIT work against a caller-provided countdown budget.
func (j *JIT) StepBlockBudget(cpu *CPU, budget uint64) (RunBudgetResult, error) {
	if budget == 0 {
		return RunBudgetExpired, nil
	}
	before := cpu.RiscvInstrBegun()
	_, err := j.stepBlockWithBudget(cpu, budget)
	if err != nil {
		return RunBudgetContinue, err
	}
	if cpu.RiscvInstrBegun()-before >= budget {
		return RunBudgetExpired, nil
	}
	return RunBudgetContinue, nil
}

func (j *JIT) StepBlockRetiredBudget(cpu *CPU, retiredBudget uint64) (RunBudgetResult, error) {
	if retiredBudget == 0 {
		return RunBudgetExpired, nil
	}
	res, _, err := j.StepBlockDualBudget(cpu, ^uint64(0), retiredBudget)
	return res, err
}

func (j *JIT) StepBlockDualBudget(cpu *CPU, attemptBudget, retiredBudget uint64) (RunBudgetResult, RunBudgetLimit, error) {
	if retiredBudget == 0 {
		return RunBudgetExpired, RunBudgetLimitRetired, nil
	}
	if attemptBudget == 0 {
		return RunBudgetExpired, RunBudgetLimitAttempt, nil
	}
	attemptBase := cpu.RiscvInstrBegun()
	retiredBase := cpu.RiscvInstrRetired()
	for {
		attemptsUsed := cpu.RiscvInstrBegun() - attemptBase
		retiredUsed := cpu.RiscvInstrRetired() - retiredBase
		if retiredBudget != 0 && retiredUsed >= retiredBudget {
			return RunBudgetExpired, RunBudgetLimitRetired, nil
		}
		if attemptBudget != 0 && attemptsUsed >= attemptBudget {
			return RunBudgetExpired, RunBudgetLimitAttempt, nil
		}
		dispatchBudget := jitMaxBudget
		if attemptBudget != 0 {
			remaining := attemptBudget - attemptsUsed
			if remaining < dispatchBudget {
				dispatchBudget = remaining
			}
		}
		if retiredBudget != 0 {
			remaining := retiredBudget - retiredUsed
			if remaining < dispatchBudget {
				dispatchBudget = remaining
			}
		}
		if dispatchBudget == 0 {
			if retiredBudget != 0 && retiredUsed >= retiredBudget {
				return RunBudgetExpired, RunBudgetLimitRetired, nil
			}
			return RunBudgetExpired, RunBudgetLimitAttempt, nil
		}
		beforeAttempts := cpu.RiscvInstrBegun()
		_, err := j.stepBlockWithBudget(cpu, dispatchBudget)
		if err != nil {
			return RunBudgetContinue, RunBudgetLimitNone, err
		}
		if cpu.RiscvInstrBegun() == beforeAttempts {
			return RunBudgetContinue, RunBudgetLimitNone, nil
		}
	}
}

// InstallAOT runs the whole-program AOT translator on the ELF bytes.
// For every PT_LOAD segment with PF_X set, it registers an ExecRegion
// on the guest memory, linearly scans the range to enumerate basic-
// block ranges, batch-compiles every translatable block into one
// unified native-code mmap per PT_LOAD, pre-resolves every static
// chain exit whose target is in the AOT set, and builds a mask-
// bounded mutable decoder_cache. The resulting segments are
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
		j.AOTCompileFailures++
		return
	}
	seg.isLikelyJIT = writable
	j.aotSegments = append(j.aotSegments, seg)
	j.AOTSegmentsInstalled++
	j.AOTBlocksInstalled += uint64(len(seg.blocks))
}

// CloneConfig returns a fresh JIT with the same policy/configuration as j,
// but no compiled code. AOT segments are mutable and JIT-owned, so Machine
// clones deliberately do not share them; the child lazily compiles or
// AutoAOT-installs against its own guest memory.
func (j *JIT) CloneConfig() *JIT {
	child := NewJIT()
	child.irAlloc = j.irAlloc
	child.regPolicy = j.regPolicy
	child.useABJIT = j.useABJIT
	child.InterpOnly = j.InterpOnly
	child.AutoAOT = j.AutoAOT
	child.HotRegionThreshold = j.HotRegionThreshold
	child.UseR15InstructionCounter = j.UseR15InstructionCounter
	child.DebugOneBlockLockstepMode = j.DebugOneBlockLockstepMode
	child.LockstepModeBudget = j.LockstepModeBudget
	child.faultPageZero = j.faultPageZero
	return child
}

// Close releases every AOT segment owned by this JIT and munmaps the
// lazy-code arena. Safe to call multiple times; subsequent calls are
// no-ops. After Close, the JIT must not dispatch — native code is gone.
func (j *JIT) Close() {
	for _, s := range j.aotSegments {
		s.free()
	}
	j.aotSegments = nil
	j.hotSegment = nil
	j.soleSegment = nil

	for _, s := range j.lazySegments {
		s.free()
	}
	j.lazySegments = nil
	j.hotLazySegment = nil
	j.soleLazySegment = nil

	for _, blk := range j.lazyBlocks {
		if len(blk.nativeMmap) > 0 {
			_ = syscall.Munmap(blk.nativeMmap)
		}
		blk.nativeMmap = nil
		blk.fn = 0
		blk.chainEntry = 0
		blk.liveChainEntry = 0
	}
	if len(j.lazyCodeArena) > 0 {
		_ = syscall.Munmap(j.lazyCodeArena)
	}
	j.lazyCodeArena = nil
	j.lazyCodeOff = 0
	j.lazyBlocks = nil
	j.lazyBlockMap = nil
	j.abjitState = nil
	j.freeStopperPage()
}

// InvalidateSegment removes the segment containing pc from the
// dispatch set and frees it. On the next JALR/dispatch into the
// same region, nextExecuteSegment will re-create a fresh segment from
// the current guest memory contents (mirrors libriscv's
// evict_execute_segment + next_execute_segment flow).
//
// Returns true if a segment was invalidated, false if pc was not in
// any segment.
//
// Caveat: existing lazy blocks or other AOT segments may hold chain-
// exit pointers or JALR IC entries referencing the invalidated
// segment's native code. free() munmaps it, so those pointers are
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
			s.free()
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
		s.free()
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
	j.lazyBlockMap = make(map[uint64]*compiledBlock)
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

func (j *JIT) refreshSoleLazySegment() {
	if len(j.lazySegments) == 1 {
		j.soleLazySegment = j.lazySegments[0]
	} else {
		j.soleLazySegment = nil
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

func (j *JIT) findLazySegment(pc uint64) *DecodedExecuteSegment {
	if s := j.hotLazySegment; s != nil && pc >= s.vaddrBegin && pc < s.vaddrEnd {
		return s
	}
	for _, s := range j.lazySegments {
		if pc >= s.vaddrBegin && pc < s.vaddrEnd {
			j.hotLazySegment = s
			return s
		}
	}
	return nil
}

func allocLazyDecoderCache(vaddrBegin, vaddrEnd uint64) ([]byte, uint64, error) {
	if vaddrBegin >= vaddrEnd {
		return nil, 0, fmt.Errorf("allocLazyDecoderCache: empty range")
	}
	minSize := uint64((vaddrEnd - vaddrBegin) / 2 * 8)
	cacheSize := uint64(1)
	for cacheSize < minSize {
		cacheSize *= 2
	}
	if cacheSize < 8 {
		cacheSize = 8
	}
	cacheMmap, err := allocRWAnon(int(cacheSize))
	if err != nil {
		return nil, 0, err
	}
	return cacheMmap, cacheSize - 1, nil
}

func (j *JIT) ensureLazyDecoderSegment(mem *GuestMemory, pc uint64) *DecodedExecuteSegment {
	if mem == nil {
		return nil
	}
	if s := j.soleLazySegment; s != nil && pc >= s.vaddrBegin && pc < s.vaddrEnd {
		return s
	}
	if s := j.findLazySegment(pc); s != nil {
		return s
	}
	region := mem.FindExecRegion(pc)
	if region == nil || region.VAddrEnd <= region.VAddrBegin {
		return nil
	}
	begin := region.VAddrBegin
	end := region.VAddrEnd
	cacheMmap, cacheMask, err := allocLazyDecoderCache(begin, end)
	if err != nil {
		return nil
	}
	seg := &DecodedExecuteSegment{
		vaddrBegin:       begin,
		vaddrEnd:         end,
		vaddrSize:        end - begin,
		decoderCacheMmap: cacheMmap,
		decoderCacheBase: uintptr(unsafe.Pointer(&cacheMmap[0])),
		decoderCacheMask: cacheMask,
		blocks:           make(map[uint64]*compiledBlock),
		isLikelyJIT:      region.IsLikelyJIT,
	}
	j.lazySegments = append(j.lazySegments, seg)
	j.hotLazySegment = seg
	j.refreshSoleLazySegment()
	return seg
}

func (j *JIT) decoderParamsForBlock(blk *compiledBlock) (uintptr, uint64, uint64, uint64) {
	if blk != nil {
		if seg := blk.segment; seg != nil {
			return seg.decoderCacheBase, seg.decoderCacheMask, seg.vaddrBegin, seg.vaddrSize
		}
	}
	if seg := j.soleSegment; seg != nil {
		return seg.decoderCacheBase, seg.decoderCacheMask, seg.vaddrBegin, seg.vaddrSize
	}
	return 0, 0, 0, 0
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
			j.countAOTDecoderCacheProbe(s, pc)
			if blk, ok := s.blocks[pc]; ok {
				return blk
			}
		} else {
			j.AOTDecoderCacheOutside++
		}
	} else if len(j.aotSegments) > 0 {
		if s := j.findSegment(pc); s != nil {
			j.countAOTDecoderCacheProbe(s, pc)
			if blk, ok := s.blocks[pc]; ok {
				return blk
			}
		} else {
			j.AOTDecoderCacheOutside++
		}
	}
	idx := cacheIdx(pc)
	if j.cache[idx].pc == pc {
		return j.cache[idx].blk
	}
	if blk := j.lazyBlockMap[pc]; blk != nil {
		j.cache[idx] = blockCacheEntry{pc, blk}
		return blk
	}
	return nil
}

func (j *JIT) countAOTDecoderCacheProbe(seg *DecodedExecuteSegment, pc uint64) {
	j.AOTDecoderCacheLookups++
	if decoderCacheEntry(seg, pc) != 0 {
		j.AOTDecoderCacheHits++
	} else {
		j.AOTDecoderCacheMisses++
	}
}

func (j *JIT) recordNativeICDelta(status uint64, delta uint64) {
	if delta > j.ICDeltaMax {
		j.ICDeltaMax = delta
	}
	switch int(status) {
	case jitOK:
		j.ICDeltaOK += delta
	case jitBudget:
		j.DispatchBudget++
		j.ICDeltaBudget += delta
	case jitOKJalrMiss:
		j.ICDeltaJalrMiss += delta
	case jitEcall:
		j.DispatchEcall++
		j.ICDeltaEcall += delta
	case jitLoadFault, jitStoreFault, jitMisalign, jitIllegal, jitEbreak:
		j.DispatchFault++
		j.ICDeltaFault += delta
	default:
		j.ICDeltaOther += delta
	}
}

func nativeRetiredDelta(status uint64, attempts uint64) uint64 {
	if attempts == 0 {
		return 0
	}
	switch int(status) {
	case jitEcall, jitEbreak, jitLoadFault, jitStoreFault, jitMisalign, jitIllegal:
		return attempts - 1
	default:
		return attempts
	}
}

func (j *JIT) recordEcallTrapPC(cpu *CPU, trapPC uint64) {
	if cpu == nil {
		j.DispatchEcallStale++
		return
	}
	insn, fault := (&cpu.mem).Fetch32(trapPC)
	if fault == nil && insn == 0x00000073 {
		j.DispatchEcallReal++
	} else {
		j.DispatchEcallStale++
	}
}

func decoderCacheEntry(seg *DecodedExecuteSegment, pc uint64) uintptr {
	slot := decoderCacheSlot(seg, pc)
	if slot == nil {
		return 0
	}
	return atomic.LoadUintptr(slot)
}

func decoderCacheSlot(seg *DecodedExecuteSegment, pc uint64) *uintptr {
	if seg == nil || len(seg.decoderCacheMmap) < 8 {
		return nil
	}
	if pc < seg.vaddrBegin || pc >= seg.vaddrEnd {
		return nil
	}
	byteOff := ((pc - seg.vaddrBegin) << 2) & seg.decoderCacheMask
	if byteOff+8 > uint64(len(seg.decoderCacheMmap)) {
		return nil
	}
	return (*uintptr)(unsafe.Pointer(&seg.decoderCacheMmap[int(byteOff)]))
}

func storeDecoderCacheEntry(seg *DecodedExecuteSegment, pc uint64, chainEntry uintptr) bool {
	slot := decoderCacheSlot(seg, pc)
	if slot == nil {
		return false
	}
	atomic.StoreUintptr(slot, chainEntry)
	return true
}

// insertBlock stores a compiled block in the cache. Lazy blocks are also
// registered in j.lazyBlocks so JIT.Close can clear their entry pointers
// before freeing the shared lazy-code arena. Lazy blocks that land inside
// an AOT segment are also published into that
// segment's mutable decoder_cache so native JALR dispatch can jump to
// them without returning to Go on the next hit.
func (j *JIT) insertBlock(pc uint64, blk *compiledBlock) {
	idx := cacheIdx(pc)
	j.cache[idx] = blockCacheEntry{pc, blk}
	if blk != nil && blk.fn != 0 {
		j.lazyBlocks = append(j.lazyBlocks, blk)
		if j.lazyBlockMap == nil {
			j.lazyBlockMap = make(map[uint64]*compiledBlock)
		}
		j.lazyBlockMap[pc] = blk
	}
	if blk == nil || blk.chainEntry == 0 {
		return
	}
	if seg := blk.segment; seg != nil {
		seg.blocks[pc] = blk
		storeDecoderCacheEntry(seg, pc, blk.chainEntry)
		return
	}
	if len(j.aotSegments) == 0 {
		return
	}
	if seg := j.findSegment(pc); seg != nil {
		seg.blocks[pc] = blk
		blk.segment = seg
		storeDecoderCacheEntry(seg, pc, blk.chainEntry)
	}
}

func (j *JIT) maybeCompileHotRegion(mem *GuestMemory, pc uint64) bool {
	threshold := j.HotRegionThreshold
	if threshold == 0 || j.InterpOnly || mem == nil {
		return false
	}
	if j.findSegment(pc) != nil {
		return false
	}
	region := mem.FindExecRegion(pc)
	if region == nil {
		return false
	}
	begin := region.VAddrBegin
	if j.hotRegionCounts == nil {
		j.hotRegionCounts = make(map[uint64]uint32)
	}
	count := j.hotRegionCounts[begin] + 1
	j.hotRegionCounts[begin] = count
	if count < threshold {
		return false
	}
	if seg := j.nextExecuteSegment(mem, pc); seg != nil {
		j.HotRegionsCompiled++
		delete(j.hotRegionCounts, begin)
		_, hasPC := seg.blocks[pc]
		return hasPC
	}
	return false
}

// StepBlock executes one dispatch cycle and returns.
func (j *JIT) StepBlock(cpu *CPU) (ic uint64, err error) {
	return j.stepBlockWithBudget(cpu, j.stepBlockDispatchBudget())
}

func (j *JIT) stepBlockWithBudget(cpu *CPU, budget uint64) (ic uint64, err error) {
	if j.watchAddr == 0 && cpu.watchAddr != 0 {
		j.watchAddr = cpu.watchAddr
	}
	if cpu.mem.TohostAddr != 0 && cpu.watchAddr == 0 {
		panic("JIT: ELF has tohost symbol but cpu.SetWatchAddr was never called — JIT blocks with tohost writes will hang")
	}
	pc := cpu.pc

	blk := j.lookupBlock(pc)
	if blk != nil {
		var res jitcall.Result
		if j.useABJIT {
			dcBase, dcMask, vBegin, segSz := j.decoderParamsForBlock(blk)
			res = abjitDispatch(blk, cpu, j, dcBase, dcMask, vBegin, segSz, budget)
		} else {
			dcBase, dcMask, vBegin, segSz := j.decoderParamsForBlock(blk)
			res = sandboxRv8Call(blk.fn, cpu,
				cpu.mem.RegFileBase(), cpu.mem.StackTop(),
				dcBase, dcMask, vBegin, segSz, budget)
			cpu.riscvInstrBegun += res.ICdelta
			cpu.riscvInstrRetired += nativeRetiredDelta(res.Status, res.ICdelta)
		}
		if j.trace {
			fmt.Fprintf(os.Stderr, "JIT pc=0x%x -> PC=0x%x status=%d\n",
				pc, res.PC, res.Status)
		}
		cpu.pc = res.PC
		j.recordNativeICDelta(res.Status, res.ICdelta)

		switch int(res.Status) {
		case jitOK:
			j.DispatchOK++
			sourceBlk := blk
			if j.useABJIT && res.SourceBlock != 0 {
				sourceBlk = (*compiledBlock)(unsafe.Pointer(res.SourceBlock))
			}
			if len(sourceBlk.chainExits) > 0 {
				if exitIdx, ok := chainExitIndexFromResult(res); ok {
					j.tryPatchChainExit(sourceBlk, exitIdx, cpu.pc)
				} else {
					j.tryPatchChain(sourceBlk, cpu.pc)
				}
			}
			return cpu.riscvInstrBegun, nil
		case jitOKJalrMiss:
			j.JalrICMisses++
			j.tryPatchJalrIC(blk, int(res.FaultAddr), cpu.pc)
			return cpu.riscvInstrBegun, nil
		case jitBudget:
			if res.ICdelta == 0 && budget > 0 {
				if err := j.stepInterpreted(cpu); err != nil {
					return cpu.riscvInstrBegun, err
				}
			}
			return cpu.riscvInstrBegun, nil
		case jitMisalign:
			if err := j.stepInterpreted(cpu); err != nil {
				return cpu.riscvInstrBegun, err
			}
			return cpu.riscvInstrBegun, nil
		case jitEcall:
			j.recordEcallTrapPC(cpu, cpu.pc)
			cpu.setTrap(CauseEcallU, 4)
			if cpu.mtvec != 0 {
				cpu.mepc = cpu.pc
				cpu.mcause = 8
				cpu.mtval = 0
				cpu.pc = cpu.mtvec
				return cpu.riscvInstrBegun, nil
			}
			return cpu.riscvInstrBegun, ErrEcall
		case jitEbreak:
			cpu.setEbreakTrapAtPC()
			return cpu.riscvInstrBegun, ErrEbreak
		case jitLoadFault:
			return cpu.riscvInstrBegun, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
		case jitStoreFault:
			return cpu.riscvInstrBegun, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
		default:
			err = j.stepInterpreted(cpu)
			return cpu.riscvInstrBegun, err
		}
	}

	// Try to translate
	if !j.InterpOnly && !j.noJIT[pc] {
		res := j.emitBlock(&cpu.mem, pc)
		if res != nil && res.numInsns > 0 {
			compiled, cerr := j.jitCompile(res, &cpu.mem)
			if cerr == nil {
				j.DispatchCompile++
				j.insertBlock(pc, compiled)
				j.maybeCompileHotRegion(&cpu.mem, pc)
				return j.stepBlockWithBudget(cpu, budget) // retry — returns cpu.riscvInstrBegun
			}
			panic(lazyCompilePanicMessage(&cpu.mem, pc, res, cerr))
		}
		if res == nil {
			j.markNoJIT(&cpu.mem, pc, "emit-nil")
		} else {
			j.markNoJIT(&cpu.mem, pc, "emit-zero")
		}
	}

	// Interpreter fallback
	j.DispatchInterp++
	j.recordInterpreterFallback(&cpu.mem, pc)
	err = j.stepInterpreted(cpu)
	return cpu.riscvInstrBegun, err
}

// stepBlockDebugV1V2 runs a block through both V1 and V2, compares all
// register outputs, and panics with full diagnostics on first mismatch.
// The V1 result is used to update cpu state (it's the production path).
func (j *JIT) stepBlockResult(cpu *CPU, res jitcall.Result) (uint64, error) {
	switch int(res.Status) {
	case jitOK:
		return 0, nil
	case jitEcall:
		return 0, ErrEcall
	case jitEbreak:
		return 0, ErrEbreak
	case jitLoadFault:
		return 0, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
	case jitStoreFault:
		return 0, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
	default:
		return 0, nil
	}
}

// RunJIT executes the CPU using JIT-compiled blocks where possible,
// falling back to the interpreter for untranslatable instructions.
func (j *JIT) RunJIT(cpu *CPU) (err0 error) {
	old := debug.SetPanicOnFault(true)
	defer debug.SetPanicOnFault(old)

	/*
		defer func() {
			r := recover()
			if r != nil {
				switch x := r.(type) {
				case *ExitError:
					if x.Code == 0 {
						return
					}
					err0 = x
					return
				}
				err0 = fmt.Errorf("RunJIT panic recovered = '%v'", r)
				vv("err0 = %v", err0)
			}
		}()
	*/

	// Propagate the tohost watch address so JIT blocks emit exit checks
	// on stores to this address. Must happen before AOT compilation.
	j.watchAddr = cpu.watchAddr
	if cpu.mem.TohostAddr != 0 && cpu.watchAddr == 0 {
		panic("JIT: ELF has tohost symbol but cpu.SetWatchAddr was never called — JIT blocks with tohost writes will hang")
	}

	//vv("RunJIT(): j.AutoAOT = %v", j.AutoAOT)

	// lazy JIT is the default now.
	// AOT JIT means that on the first RunJIT call for a JIT that has
	// no segments yet, transparently translate every executable region
	// the loader already registered on cpu.mem. Only PCs outside those
	// regions (self-modifying code, guest-generated blocks, tests that
	// built a raw mem) fall back to the lazy compile path below.
	// Leave AutoAOT false on the JIT to get the lazy path end-to-end.
	if j.AutoAOT && len(j.aotSegments) == 0 && len(cpu.mem.ExecRegions()) > 0 {
		err := j.InstallAOTFromMem(&cpu.mem)
		//vv("j.InstallAOTFromMem err = '%v'", err)
		panicOn(err)
	}

	for {
		// Tohost polling — once per dispatch cycle (block granularity).
		// This is how the standard RISCV test ELFs communicate they
		// are done. The write into 'tohost' and then infinite loop.
		// Hence they will enver exit if we don't check.
		if cpu.watchAddr != 0 {
			if v, _ := cpu.mem.Load64(cpu.watchAddr); v != 0 {
				return &ExitError{Code: tohostExitCode(v)}
			}
		}

		pc := cpu.pc

		blk := j.lookupBlock(pc)
		if blk != nil {
			//vv("lookupBlock cache hit. pc = 0x%x", pc)

			var res jitcall.Result
			if j.useABJIT {
				dcBase, dcMask, vBegin, segSz := j.decoderParamsForBlock(blk)
				res = abjitDispatch(blk, cpu, j, dcBase, dcMask, vBegin, segSz, jitMaxBudget)
			} else {
				regFile := cpu.mem.RegFileBase()
				stackTop := cpu.mem.StackTop()
				dcBase, dcMask, vBegin, segSz := j.decoderParamsForBlock(blk)
				res = sandboxRv8Call(blk.fn, cpu, regFile, stackTop,
					dcBase, dcMask, vBegin, segSz, jitMaxBudget)
				cpu.riscvInstrBegun += res.ICdelta
				cpu.riscvInstrRetired += nativeRetiredDelta(res.Status, res.ICdelta)
			}
			cpu.pc = res.PC
			j.recordNativeICDelta(res.Status, res.ICdelta)

			switch int(res.Status) {
			case jitOK:
				j.DispatchOK++
				// Patch this block's chain exit to jump directly to the target.
				// When a chain exit isn't patched, the slow stub returns here.
				// After patching, future executions jump directly — bypassing Go.
				sourceBlk := blk
				if j.useABJIT && res.SourceBlock != 0 {
					sourceBlk = (*compiledBlock)(unsafe.Pointer(res.SourceBlock))
				}
				if len(sourceBlk.chainExits) > 0 {
					if exitIdx, ok := chainExitIndexFromResult(res); ok {
						j.tryPatchChainExit(sourceBlk, exitIdx, cpu.pc)
					} else {
						j.tryPatchChain(sourceBlk, cpu.pc)
					}
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

			case jitBudget:
				if res.ICdelta == 0 {
					if err := j.stepInterpreted(cpu); err != nil {
						return err
					}
				}
				continue

			case jitMisalign:
				// Misaligned access: re-execute the faulting instruction via
				// the interpreter (which handles misalignment with byte-by-byte
				// reads/writes), then continue JIT dispatch.
				if err := j.stepInterpreted(cpu); err != nil {
					return err
				}
				continue

			case jitEcall:
				j.recordEcallTrapPC(cpu, cpu.pc)
				cpu.setTrap(CauseEcallU, 4)
				if cpu.mtvec != 0 {
					cpu.mepc = cpu.pc
					cpu.mcause = 8
					cpu.mtval = 0
					cpu.pc = cpu.mtvec
					continue
				}
				n := noteFromCPUError(cpu, ErrEcall)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				case NoteExit:
					return &ExitError{Code: cpu.ExitCode}
				default:
					return ErrEcall
				}

			case jitEbreak:
				cpu.setEbreakTrapAtPC()
				n := noteFromCPUError(cpu, ErrEbreak)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				case NoteExit:
					return &ExitError{Code: cpu.ExitCode}
				default:
					return ErrEbreak
				}

			case jitLoadFault:
				f := &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
				n := noteFromStepErr(f, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				case NoteExit:
					return &ExitError{Code: cpu.ExitCode}
				default:
					return f
				}

			case jitStoreFault:
				f := &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
				n := noteFromStepErr(f, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				case NoteExit:
					return &ExitError{Code: cpu.ExitCode}
				default:
					return f
				}

			default:
				err := j.stepInterpreted(cpu)
				if err == nil {
					continue
				}
				n := noteFromCPUError(cpu, err)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return err
				}
			}
		}

		//vv("lookupBlock cache miss. No compiled block. pc = 0x%x", pc)

		// No compiled block. If pc falls inside a registered ExecRegion
		// that isn't yet covered by any AOT segment (e.g., the guest
		// mmapped a new R-X region and jumped to it — LuaJIT pattern),
		// build a segment for it now. Re-try dispatch on success.
		// AutoAOT=false opts out — benchmarks and tests that measure
		// the lazy path need to prevent on-demand AOT too.
		if !j.InterpOnly && j.AutoAOT && len(cpu.mem.execRegions) > 0 {
			if seg := j.nextExecuteSegment(&cpu.mem, pc); seg != nil {
				if _, ok := seg.blocks[pc]; ok {
					continue // retry — next lookupBlock hits the new AOT block
				}
			}
		}

		// No compiled block — try to translate.
		if !j.InterpOnly && !j.noJIT[pc] {
			res := j.emitBlock(&cpu.mem, pc)
			if res != nil && res.numInsns > 0 {
				blk, err := j.jitCompile(res, &cpu.mem)
				if err == nil {
					j.DispatchCompile++
					j.insertBlock(pc, blk)
					j.maybeCompileHotRegion(&cpu.mem, pc)
					continue
				}
				panic(lazyCompilePanicMessage(&cpu.mem, pc, res, err))
			} else if debugJIT {
				if res == nil {
					fmt.Fprintf(os.Stderr, "EMIT_NIL pc=0x%x\n", pc)
				} else {
					fmt.Fprintf(os.Stderr, "EMIT_ZERO pc=0x%x numInsns=%d\n", pc, res.numInsns)
				}
			}
			if res == nil {
				j.markNoJIT(&cpu.mem, pc, "emit-nil")
			} else {
				j.markNoJIT(&cpu.mem, pc, "emit-zero")
			}
		}

		// Interpret one instruction.
		j.DispatchInterp++
		j.recordInterpreterFallback(&cpu.mem, pc)
		err := j.stepInterpreted(cpu)
		if err == nil {
			continue
		}
		n := noteFromCPUError(cpu, err)
		switch cpu.Notes.Deliver(cpu, n) {
		case NoteHandled:
			continue
		default:
			return err
		}
	}
}

// patchChainTarget overwrites the backend's 8-byte patch data slot at
// codeBase+patchOffset to redirect to targetAddr.
func patchChainTarget(codeBase uintptr, patchOffset int, targetAddr uintptr) {
	patchAddr := codeBase + uintptr(patchOffset)
	withExecWrite(func() {
		//nolint:gosec // JIT code patching requires direct memory writes.
		p := (*[8]byte)(unsafe.Pointer(patchAddr)) //nolint:govet
		binary.LittleEndian.PutUint64(p[:], uint64(targetAddr))
	})
	flushIcache(patchAddr, 8)
}

func liveChainCompatible(src liveChainMeta, target *compiledBlock) bool {
	if target == nil || !src.Enabled || !target.liveChain.Enabled {
		return false
	}
	if target.liveChainEntry == 0 {
		return false
	}
	if src.HasDirtyArch {
		return false
	}
	for vr := 1; vr < 32; vr++ {
		if !target.liveChain.EntryLiveArch[vr] {
			continue
		}
		if !src.ValidExitArch[vr] {
			return false
		}
		if !src.ArchHostValid[vr] || !target.liveChain.ArchHostValid[vr] {
			return false
		}
		if src.ArchHost[vr] != target.liveChain.ArchHost[vr] {
			return false
		}
	}
	return true
}

func chainExitIndexFromResult(res jitcall.Result) (int, bool) {
	if res.FaultAddr == 0 {
		return 0, false
	}
	idx := res.FaultAddr - 1
	if idx > uint64(int(^uint(0)>>1)) {
		return 0, false
	}
	return int(idx), true
}

func (j *JIT) tryPatchChainExit(blk *compiledBlock, exitIdx int, targetPC uint64) {
	j.ChainPatchTry++
	if blk == nil || exitIdx < 0 || exitIdx >= len(blk.chainExits) {
		j.ChainPatchNoMatch++
		return
	}
	ce := &blk.chainExits[exitIdx]
	if ce.targetPC != targetPC {
		j.ChainPatchNoMatch++
		return
	}
	target := j.lookupBlock(targetPC)
	if target == nil || target.chainEntry == 0 {
		j.ChainPatchNoTarget++
		return
	}
	if ce.livePatchOffset >= 0 && liveChainCompatible(ce.liveChain, target) {
		patchChainTarget(blk.fn, ce.livePatchOffset, target.liveChainEntry)
	} else {
		patchChainTarget(blk.fn, ce.patchOffset, target.chainEntry)
	}
	j.ChainPatched++
}

// tryPatchChain patches a previous block's chain exit to jump directly
// to the target block, bypassing the Go dispatch loop on future executions.
func (j *JIT) tryPatchChain(blk *compiledBlock, targetPC uint64) {
	j.ChainPatchTry++
	target := j.lookupBlock(targetPC)
	if target == nil || target.chainEntry == 0 {
		j.ChainPatchNoTarget++
		return
	}
	for _, ce := range blk.chainExits {
		if ce.targetPC == targetPC {
			if ce.livePatchOffset >= 0 && liveChainCompatible(ce.liveChain, target) {
				patchChainTarget(blk.fn, ce.livePatchOffset, target.liveChainEntry)
			} else {
				patchChainTarget(blk.fn, ce.patchOffset, target.chainEntry)
			}
			j.ChainPatched++
			return
		}
	}
	j.ChainPatchNoMatch++
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
