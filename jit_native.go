package riscv

// jit_native.go — Native IR→machine-code compilation pipeline.
// emitBlock produces ir.Block, which is lowered and assembled to native
// code via the goasm package (no cgo).

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/goasm"
)

// Reusable assembler context — Prog slabs are recycled across compilations.
var jitCtx *goasm.Ctx
var jitCtxArch goasm.Arch

const (
	lazyCodeArenaMultiplier = 5
	lazyCodeArenaMinSize    = 500 << 20
)

func getJITCtx(arch goasm.Arch) *goasm.Ctx {
	if jitCtx == nil || jitCtxArch != arch {
		jitCtx = goasm.New(arch)
		jitCtxArch = arch
	} else {
		jitCtx.Reset()
	}
	return jitCtx
}

func (j *JIT) checkNativeBackend() error {
	if j.regPolicy.Arch != goasm.HostArch() {
		return fmt.Errorf("jit: policy %q targets arch %d, host arch is %d",
			j.regPolicy.Name, j.regPolicy.Arch, goasm.HostArch())
	}
	if j.regPolicy.PatchImm64 == nil {
		return fmt.Errorf("jit: policy %q has no patcher", j.regPolicy.Name)
	}
	return nil
}

func alignUpInt(n, align int) int {
	if align <= 1 {
		return n
	}
	return (n + align - 1) &^ (align - 1)
}

func lazyCodeAlignmentForArch(arch goasm.Arch) int {
	if arch == goasm.ARM64 {
		return 4
	}
	return 1
}

func lazyCodeArenaSize(mem *GuestMemory, firstCodeSize, alignment int) (int, error) {
	var target uint64
	if mem != nil && (mem.loadedELFSize != 0 || mem.loadedELFImageSize != 0) {
		binarySize := mem.loadedELFSize
		if mem.loadedELFImageSize > binarySize {
			binarySize = mem.loadedELFImageSize
		}
		if binarySize > ^uint64(0)/lazyCodeArenaMultiplier {
			return 0, fmt.Errorf("jit lazy code arena size overflow: loaded_elf=%d loaded_image=%d",
				mem.loadedELFSize, mem.loadedELFImageSize)
		}
		target = binarySize * lazyCodeArenaMultiplier
	} else if mem != nil {
		for _, r := range mem.execRegions {
			if r.VAddrEnd > r.VAddrBegin {
				if target > ^uint64(0)-(r.VAddrEnd-r.VAddrBegin) {
					return 0, fmt.Errorf("jit lazy code arena size overflow while summing exec regions")
				}
				target += r.VAddrEnd - r.VAddrBegin
			}
		}
		if target > ^uint64(0)/lazyCodeArenaMultiplier {
			return 0, fmt.Errorf("jit lazy code arena size overflow: exec_region_bytes=%d", target)
		}
		target *= lazyCodeArenaMultiplier
	}
	if target < lazyCodeArenaMinSize {
		target = lazyCodeArenaMinSize
	}
	if first := uint64(alignUpInt(firstCodeSize, alignment)); target < first {
		target = first
	}
	maxInt := uint64(^uint(0) >> 1)
	if target > maxInt {
		return 0, fmt.Errorf("jit lazy code arena too large: %d bytes", target)
	}
	return int(target), nil
}

func (j *JIT) allocLazyCode(size int, mem *GuestMemory) ([]byte, uintptr, error) {
	if size <= 0 {
		return nil, 0, fmt.Errorf("jit lazy code allocation: invalid size %d", size)
	}
	alignment := lazyCodeAlignmentForArch(j.regPolicy.Arch)
	if j.lazyCodeArena == nil {
		arenaSize, err := lazyCodeArenaSize(mem, size, alignment)
		if err != nil {
			return nil, 0, err
		}
		arena, err := allocExec(arenaSize)
		if err != nil {
			return nil, 0, err
		}
		j.lazyCodeArena = arena
		j.lazyCodeOff = 0
	}
	off := alignUpInt(j.lazyCodeOff, alignment)
	end := off + size
	if off < 0 || end < off || end > len(j.lazyCodeArena) {
		var loadedELF uint64
		var loadedImage uint64
		if mem != nil {
			loadedELF = mem.loadedELFSize
			loadedImage = mem.loadedELFImageSize
		}
		return nil, 0, fmt.Errorf("jit lazy code arena exhausted: need=%d used=%d cap=%d loaded_elf=%d loaded_image=%d",
			size, j.lazyCodeOff, len(j.lazyCodeArena), loadedELF, loadedImage)
	}
	j.lazyCodeOff = end
	codeMem := j.lazyCodeArena[off:end]
	return codeMem, uintptr(unsafe.Pointer(&codeMem[0])), nil
}

// jitCompile compiles an IR block to native code and returns a compiledBlock.
func (j *JIT) jitCompile(res *emitResult, mem ...*GuestMemory) (*compiledBlock, error) {
	if res.block == nil {
		return nil, fmt.Errorf("jit: nil block")
	}
	if err := j.checkNativeBackend(); err != nil {
		return nil, err
	}

	pool := j.regPolicy.Pool(res.block)
	pinned := j.regPolicy.Pinned()
	if j.reserveInstructionCounterReg(res.block) {
		pool.IntRegs = removeReg(pool.IntRegs, j.regPolicy.InstructionCounterReg)
	}
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := getJITCtx(j.regPolicy.Arch)
	ctx.Append(ctx.NewATEXT())

	lowerResult, lowerErr := j.regPolicy.Lower(ctx, res.block, alloc)
	if lowerErr != nil {
		return nil, fmt.Errorf("jit lower: %w", lowerErr)
	}

	// Step 4: Assemble to native bytes.
	code, err := ctx.Assemble()
	if err != nil {
		return nil, fmt.Errorf("jit assemble: %w", err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("jit: assembler produced empty output")
	}

	// Step 5: Allocate executable memory and copy code.
	var guestMem *GuestMemory
	if len(mem) > 0 {
		guestMem = mem[0]
	}
	execMem, codeBase, err := j.allocLazyCode(len(code), guestMem)
	if err != nil {
		return nil, err
	}
	blk := &compiledBlock{
		fn:       codeBase,
		hasFP:    allocHasFP(alloc),
		numInsns: res.numInsns,
	}

	var writeErr error
	withExecWrite(func() {
		copy(execMem, code)

		// Step 6: Block chaining setup — backpatch MOVABS sentinels and record metadata.
		if lowerResult != nil && lowerResult.ChainEntryProg != nil {
			blk.chainEntry = codeBase + uintptr(lowerResult.ChainEntryProg.Pc)
			if lowerResult.LiveChainEntryProg != nil {
				blk.liveChainEntry = codeBase + uintptr(lowerResult.LiveChainEntryProg.Pc)
				blk.liveChain = lowerResult.LiveChain
			}
			for _, ce := range lowerResult.ChainExits {
				// If a slow exit stub exists, backpatch the sentinel to
				// point to it. Otherwise the sentinel remains until chain
				// linking patches in the real target address.
				patchValue := nativePatchSentinel
				if ce.StubProg != nil {
					stubAddr := codeBase + uintptr(ce.StubProg.Pc)
					patchValue = uint64(stubAddr)
				}
				patchOff, patchErr := j.regPolicy.PatchImm64(execMem, ce.MovProg, patchValue)
				if patchErr != nil {
					writeErr = fmt.Errorf("jit patch chain exit: %w", patchErr)
					return
				}
				if ce.SourceMovProg != nil {
					if _, patchErr := j.regPolicy.PatchImm64(execMem, ce.SourceMovProg, uint64(uintptr(unsafe.Pointer(blk)))); patchErr != nil {
						writeErr = fmt.Errorf("jit patch chain source: %w", patchErr)
						return
					}
				}
				livePatchOff := -1
				if ce.LiveMovProg != nil {
					liveOff, patchErr := j.regPolicy.PatchImm64(execMem, ce.LiveMovProg, 0)
					if patchErr != nil {
						writeErr = fmt.Errorf("jit patch live chain exit: %w", patchErr)
						return
					}
					livePatchOff = liveOff
				}

				blk.chainExits = append(blk.chainExits, chainPatchInfo{
					targetPC:        ce.TargetPC,
					patchOffset:     patchOff,
					livePatchOffset: livePatchOff,
					liveChain:       ce.LiveChain,
				})
			}
			if err := backpatchJalrICs(execMem, codeBase, lowerResult, blk, j.regPolicy.PatchImm64); err != nil {
				writeErr = err
				return
			}
			if err := backpatchGocallResumes(execMem, codeBase, lowerResult, j.regPolicy.PatchImm64); err != nil {
				writeErr = err
				return
			}
		}
	})
	if writeErr != nil {
		return nil, writeErr
	}
	if debugJIT {
		fmt.Fprintf(os.Stderr, "COMPILE_OK pc=0x%x numInsns=%d bytes=%d\n%s\n",
			res.startPC, res.numInsns, len(code), ctx.DumpProgs())
	}

	// VizJit dump — DumpProgs after Assemble so branch targets show
	// resolved byte offsets.
	if _, on := vizJitEnabled(); on {
		vizProgs := ctx.DumpProgs()
		var vizMem *GuestMemory
		if len(mem) > 0 {
			vizMem = mem[0]
		}
		vizJitDump(res.startPC, res.endPC, vizMem, res.block, vizProgs,
			code, codeBase)
	}

	flushIcache(codeBase, len(code))

	if len(mem) > 0 && blk.chainEntry != 0 {
		if seg := j.ensureLazyDecoderSegment(mem[0], res.startPC); seg != nil {
			blk.segment = seg
			seg.blocks[res.startPC] = blk
			storeDecoderCacheEntry(seg, res.startPC, blk.chainEntry)
		}
	}

	return blk, nil
}

// backpatchJalrICs initializes each JALR IC site's patchable slots and
// records site metadata on the block. Note: the name "IC" is historical;
// both rv8 and abjit lowerers now use a decoder-cache lookup for the
// fast path, with these slots as the miss/fallback mechanism.
// Before backpatch both MOVABS slots hold the sentinel 0x7BADC0DE7BADC0DE. After:
//   - cache_pc slot = 0xFFFFFFFFFFFFFFFF (unmatchable → first CMPQ misses)
//   - cache_fn slot = address of the per-site miss stub
//
// On first miss the Go dispatcher calls patchJalrIC to swap in a real
// target.
func backpatchJalrICs(execMem []byte, codeBase uintptr, lowerResult *LowerResult, blk *compiledBlock, patch PatchImm64) error {
	for _, ic := range lowerResult.JalrICs {
		var info jalrICPatchInfo
		info.siteIdx = ic.SiteIdx
		stubAddr := codeBase + uintptr(ic.StubProg.Pc)

		// Initialize both IC slots. pc[k] = unmatchable sentinel so first
		// CMPQ misses; fn[k] = miss stub so the initial JMP goes somewhere
		// valid. Shift-policy tryPatchJalrIC will fill slot 0 on first
		// miss, slot 1 on second, etc.
		for k := 0; k < 2; k++ {
			if ic.PcMov[k] == nil || ic.FnMov[k] == nil {
				continue
			}
			pcOff, err := patch(execMem, ic.PcMov[k], ^uint64(0))
			if err != nil {
				return fmt.Errorf("jit patch jalr pc slot: %w", err)
			}
			fnOff, err := patch(execMem, ic.FnMov[k], uint64(stubAddr))
			if err != nil {
				return fmt.Errorf("jit patch jalr fn slot: %w", err)
			}
			info.pcPatchOff[k] = pcOff
			info.fnPatchOff[k] = fnOff
		}

		blk.jalrICs = append(blk.jalrICs, info)
	}
	return nil
}

func backpatchGocallResumes(execMem []byte, codeBase uintptr, lr *LowerResult, patch PatchImm64) error {
	for _, gr := range lr.GocallResumes {
		resumeAddr := codeBase + uintptr(gr.ResumeProg.Pc)
		if _, err := patch(execMem, gr.AddrMov, uint64(resumeAddr)); err != nil {
			return fmt.Errorf("jit patch gocall resume: %w", err)
		}
	}
	return nil
}

// compileDebugInfo holds debug artifacts from compilation.
type compileDebugInfo struct {
	code  []byte // assembled native bytes
	progs string // symbolic Prog listing (Go asm syntax)
}

// jitCompileDebug compiles an IR block and returns debug info (Prog listing + assembled bytes).
func (j *JIT) jitCompileDebug(res *emitResult) (*compiledBlock, *compileDebugInfo, error) {
	if res.block == nil {
		return nil, nil, fmt.Errorf("jit: nil block")
	}
	if err := j.checkNativeBackend(); err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("jit lower: %w", lowerErr)
	}

	code, err := ctx.Assemble()
	if err != nil {
		return nil, nil, fmt.Errorf("jit assemble: %w", err)
	}
	if len(code) == 0 {
		return nil, nil, fmt.Errorf("jit: assembler produced empty output")
	}

	// Post-assembly DumpProgs so branch targets show resolved byte offsets.
	progDump := ctx.DumpProgs()

	execMem, codeBase, err := j.allocLazyCode(len(code), nil)
	if err != nil {
		return nil, nil, err
	}
	blk := &compiledBlock{fn: codeBase, hasFP: allocHasFP(alloc)}

	// Backpatch chain-exit sentinels to the slow-exit stubs and record
	// metadata, same as jitCompile. Without this, any chain exit in
	// the block would JMP to the literal sentinel 0x7BADC0DE7BADC0DE and
	// segfault when executed.
	var writeErr error
	withExecWrite(func() {
		copy(execMem, code)
		if lowerResult != nil && lowerResult.ChainEntryProg != nil {
			blk.chainEntry = codeBase + uintptr(lowerResult.ChainEntryProg.Pc)
			if lowerResult.LiveChainEntryProg != nil {
				blk.liveChainEntry = codeBase + uintptr(lowerResult.LiveChainEntryProg.Pc)
				blk.liveChain = lowerResult.LiveChain
			}
			for _, ce := range lowerResult.ChainExits {
				patchValue := nativePatchSentinel
				if ce.StubProg != nil {
					stubAddr := codeBase + uintptr(ce.StubProg.Pc)
					patchValue = uint64(stubAddr)
				}
				patchOff, patchErr := j.regPolicy.PatchImm64(execMem, ce.MovProg, patchValue)
				if patchErr != nil {
					writeErr = fmt.Errorf("jit patch chain exit: %w", patchErr)
					return
				}
				if ce.SourceMovProg != nil {
					if _, patchErr := j.regPolicy.PatchImm64(execMem, ce.SourceMovProg, uint64(uintptr(unsafe.Pointer(blk)))); patchErr != nil {
						writeErr = fmt.Errorf("jit patch chain source: %w", patchErr)
						return
					}
				}
				livePatchOff := -1
				if ce.LiveMovProg != nil {
					liveOff, patchErr := j.regPolicy.PatchImm64(execMem, ce.LiveMovProg, 0)
					if patchErr != nil {
						writeErr = fmt.Errorf("jit patch live chain exit: %w", patchErr)
						return
					}
					livePatchOff = liveOff
				}
				blk.chainExits = append(blk.chainExits, chainPatchInfo{
					targetPC:        ce.TargetPC,
					patchOffset:     patchOff,
					livePatchOffset: livePatchOff,
					liveChain:       ce.LiveChain,
				})
			}
			if err := backpatchJalrICs(execMem, codeBase, lowerResult, blk, j.regPolicy.PatchImm64); err != nil {
				writeErr = err
				return
			}
		}
	})
	if writeErr != nil {
		return nil, nil, writeErr
	}
	flushIcache(codeBase, len(code))

	dbg := &compileDebugInfo{code: code, progs: progDump}
	return blk, dbg, nil
}

func allocHasFP(alloc *Allocation) bool {
	// Architectural FP VRegs 32-63 directly allocated.
	for vr := VReg(32); vr < 64; vr++ {
		if int(vr) < len(alloc.Kind) && (alloc.Kind[vr] == AllocReg || alloc.Kind[vr] == AllocStack) {
			return true
		}
	}
	// FP base pointer (VRFBase) used — the block accesses f[] via memory
	// loads/stores through the FP register file base, even though no
	// architectural FP VReg is directly allocated.
	vr := VRFBase
	if int(vr) < len(alloc.Kind) && (alloc.Kind[vr] == AllocReg || alloc.Kind[vr] == AllocStack) {
		return true
	}
	return false
}

func removeReg(regs []int16, target int16) []int16 {
	out := make([]int16, 0, len(regs))
	for _, r := range regs {
		if r != target {
			out = append(out, r)
		}
	}
	return out
}

// allocRWAnon allocates anonymous mmap with PROT_READ|PROT_WRITE
// (no PROT_EXEC). Used by the AOT decoder_cache which holds plain
// uintptr payloads and is later mprotected to PROT_READ.
func allocRWAnon(size int) ([]byte, error) {
	pageSize := syscall.Getpagesize()
	mapSize := ((size + pageSize - 1) / pageSize) * pageSize
	mem, err := syscall.Mmap(
		-1, 0, mapSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE,
	)
	if err != nil {
		return nil, err
	}
	return mem, nil
}
