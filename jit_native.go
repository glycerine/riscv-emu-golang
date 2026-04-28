package riscv

// jit_native.go — Native IR→machine-code compilation pipeline.
// emitBlock produces ir.Block, which is lowered and assembled to native
// code via the goasm package (no cgo).

import (
	"encoding/binary"
	"fmt"
	"riscv/goasm"
	"syscall"
	"unsafe"
)

// Reusable assembler context — Prog slabs are recycled across compilations.
var jitCtx *goasm.Ctx

func getJITCtx() *goasm.Ctx {
	if jitCtx == nil {
		jitCtx = goasm.New(goasm.AMD64)
	} else {
		jitCtx.Reset()
	}
	return jitCtx
}

// jitCompile compiles an IR block to native code and returns a compiledBlock.
func (j *JIT) jitCompile(res *emitResult, mem ...*GuestMemory) (*compiledBlock, error) {
	if res.block == nil {
		return nil, fmt.Errorf("jit: nil block")
	}

	pool := j.regPolicy.Pool(res.block)
	pinned := j.regPolicy.Pinned()
	if j.UseR15InstructionCounter || j.DebugOneBlockLockstepMode {
		pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
	}
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := getJITCtx()
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
	execMem, err := allocExec(len(code))
	if err != nil {
		return nil, fmt.Errorf("jit mmap: %w", err)
	}
	copy(execMem, code)

	// VizJit dump — DumpProgs after Assemble so branch targets show
	// resolved byte offsets.
	if _, on := vizJitEnabled(); on {
		vizProgs := ctx.DumpProgs()
		var vizMem *GuestMemory
		if len(mem) > 0 {
			vizMem = mem[0]
		}
		vizJitDump(res.startPC, res.endPC, vizMem, res.block, vizProgs,
			code, uintptr(unsafe.Pointer(&execMem[0])))
	}

	codeBase := uintptr(unsafe.Pointer(&execMem[0]))
	blk := &compiledBlock{
		fn:         codeBase,
		nativeMmap: execMem,
		hasFP:      allocHasFP(alloc),
		numInsns:   res.numInsns,
	}

	// Step 6: Block chaining setup — backpatch MOVABS sentinels and record metadata.
	if lowerResult != nil && lowerResult.ChainEntryProg != nil {
		blk.chainEntry = codeBase + uintptr(lowerResult.ChainEntryProg.Pc)
		for _, ce := range lowerResult.ChainExits {
			patchOff := int(ce.MovProg.Pc) + 2

			// If a slow exit stub exists, backpatch the sentinel to
			// point to it. Otherwise the sentinel remains until chain
			// linking patches in the real target address.
			if ce.StubProg != nil {
				stubAddr := codeBase + uintptr(ce.StubProg.Pc)
				binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(stubAddr))
			}

			blk.chainExits = append(blk.chainExits, chainPatchInfo{
				targetPC:    ce.TargetPC,
				patchOffset: patchOff,
			})
		}
		backpatchJalrICs(execMem, codeBase, lowerResult, blk)
		backpatchGocallResumes(execMem, codeBase, lowerResult)
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
func backpatchJalrICs(execMem []byte, codeBase uintptr, lowerResult *LowerResult, blk *compiledBlock) {
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
			pcOff := int(ic.PcMov[k].Pc) + 2
			fnOff := int(ic.FnMov[k].Pc) + 2
			info.pcPatchOff[k] = pcOff
			info.fnPatchOff[k] = fnOff
			binary.LittleEndian.PutUint64(execMem[pcOff:], ^uint64(0))
			binary.LittleEndian.PutUint64(execMem[fnOff:], uint64(stubAddr))
		}

		blk.jalrICs = append(blk.jalrICs, info)
	}
}

func backpatchGocallResumes(execMem []byte, codeBase uintptr, lr *LowerResult) {
	for _, gr := range lr.GocallResumes {
		patchOff := int(gr.AddrMov.Pc) + 2
		resumeAddr := codeBase + uintptr(gr.ResumeProg.Pc)
		binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(resumeAddr))
	}
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

	pool := j.regPolicy.Pool(res.block)
	pinned := j.regPolicy.Pinned()
	if j.UseR15InstructionCounter || j.DebugOneBlockLockstepMode {
		pool.IntRegs = removeReg(pool.IntRegs, goasm.REG_AMD64_R15)
	}
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
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

	execMem, err := allocExec(len(code))
	if err != nil {
		return nil, nil, fmt.Errorf("jit mmap: %w", err)
	}
	copy(execMem, code)

	codeBase := uintptr(unsafe.Pointer(&execMem[0]))
	blk := &compiledBlock{fn: codeBase, nativeMmap: execMem, hasFP: allocHasFP(alloc)}

	// Backpatch chain-exit sentinels to the slow-exit stubs and record
	// metadata, same as jitCompile. Without this, any chain exit in
	// the block would JMP to the literal sentinel 0x7BADC0DE7BADC0DE and
	// segfault when executed.
	if lowerResult != nil && lowerResult.ChainEntryProg != nil {
		blk.chainEntry = codeBase + uintptr(lowerResult.ChainEntryProg.Pc)
		for _, ce := range lowerResult.ChainExits {
			patchOff := int(ce.MovProg.Pc) + 2
			if ce.StubProg != nil {
				stubAddr := codeBase + uintptr(ce.StubProg.Pc)
				binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(stubAddr))
			}
			blk.chainExits = append(blk.chainExits, chainPatchInfo{
				targetPC:    ce.TargetPC,
				patchOffset: patchOff,
			})
		}
		backpatchJalrICs(execMem, codeBase, lowerResult, blk)
	}

	dbg := &compileDebugInfo{code: code, progs: progDump}
	return blk, dbg, nil
}

// allocExec allocates executable memory via mmap.
func allocExec(size int) ([]byte, error) {
	pageSize := syscall.Getpagesize()
	mapSize := ((size + pageSize - 1) / pageSize) * pageSize
	mem, err := syscall.Mmap(
		-1, 0, mapSize,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE,
	)
	if err != nil {
		return nil, err
	}
	return mem, nil
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
