//go:build !tcc

package riscv

// jit_native.go — Native IR→machine-code compilation pipeline.
// Replaces TCC: emitBlock produces ir.Block, which is lowered and assembled
// to native code via the goasm package (no cgo).

import (
	"encoding/binary"
	"fmt"
	"os"
	"riscv/goasm"
	"riscv/ir"
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

// chainPatchInfo describes a chain exit that can be patched by Go.
type chainPatchInfo struct {
	targetPC    uint64 // guest PC this exit targets
	patchOffset int    // byte offset of imm64 in MOVABS within the code page
}

// compiledBlock holds a natively-compiled function pointer.
type compiledBlock struct {
	fn         uintptr          // native function pointer (mmap'd executable memory)
	chainEntry uintptr          // entry point that skips pinned reg setup (for chaining)
	chainExits []chainPatchInfo // chain exits that Go can patch
	shadow     *compiledBlock   // V2 shadow block for DebugV1V2 comparison
}

// jitCompile compiles an IR block to native code and returns a compiledBlock.
func (j *JIT) jitCompile(res *emitResult) (*compiledBlock, error) {
	return j.jitCompileWith(res, false)
}

func (j *JIT) jitCompileV2(res *emitResult) (*compiledBlock, error) {
	return j.jitCompileWith(res, true)
}

func (j *JIT) jitCompileWith(res *emitResult, useV2 bool) (*compiledBlock, error) {
	if res.block == nil {
		return nil, fmt.Errorf("jit: nil block")
	}

	// Step 1: Register allocation.
	var pool ir.RegPool
	if useV2 {
		pool = ir.AMD64Pool_V2(res.block)
	} else {
		pool = ir.AMD64Pool(res.block)
	}
	pinned := ir.AMD64Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	// Step 2: Reuse assembler context and emit ATEXT prologue.
	ctx := getJITCtx()
	ctx.Append(ctx.NewATEXT())

	// Step 3: Lower IR to x86-64 Progs.
	var lowerResult *ir.LowerResult
	var lowerErr error
	if useV2 {
		lowerResult, lowerErr = ir.LowerAMD64_V2(ctx, res.block, alloc)
	} else {
		lowerResult, lowerErr = ir.LowerAMD64(ctx, res.block, alloc)
	}
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

	codeBase := uintptr(unsafe.Pointer(&execMem[0]))
	blk := &compiledBlock{
		fn: codeBase,
	}

	// Step 6: Block chaining setup — backpatch MOVABS sentinels and record metadata.
	if lowerResult != nil && lowerResult.ChainEntryProg != nil {
		blk.chainEntry = codeBase + uintptr(lowerResult.ChainEntryProg.Pc)
		fmt.Fprintf(os.Stderr, "CHAIN: block pc=0x%x codeLen=%d has %d chain exits, chainEntry offset=%d\n",
			res.startPC, len(code), len(lowerResult.ChainExits), lowerResult.ChainEntryProg.Pc)

		for i, ce := range lowerResult.ChainExits {
			// The MOVABS R10, imm64 encoding is: 49 BA <8 bytes imm64>.
			// The imm64 starts at byte offset +2 from the instruction start.
			patchOff := int(ce.MovProg.Pc) + 2
			stubAddr := codeBase + uintptr(ce.StubProg.Pc)

			// Backpatch the sentinel to point to the slow exit stub.
			fmt.Fprintf(os.Stderr, "  exit[%d]: targetPC=0x%x movProg.Pc=%d stubProg.Pc=%d patchOff=%d bytes@mov=%02x%02x\n",
				i, ce.TargetPC, ce.MovProg.Pc, ce.StubProg.Pc, patchOff,
				execMem[int(ce.MovProg.Pc)], execMem[int(ce.MovProg.Pc)+1])
			binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(stubAddr))

			blk.chainExits = append(blk.chainExits, chainPatchInfo{
				targetPC:    ce.TargetPC,
				patchOffset: patchOff,
			})
		}
	}

	return blk, nil
}

// compileDebugInfo holds debug artifacts from compilation.
type compileDebugInfo struct {
	code  []byte // assembled native bytes
	progs string // symbolic Prog listing (Go asm syntax)
}

// jitCompileDebug compiles an IR block and returns debug info (Prog listing + assembled bytes).
func (j *JIT) jitCompileDebug(res *emitResult, useV2 bool) (*compiledBlock, *compileDebugInfo, error) {
	if res.block == nil {
		return nil, nil, fmt.Errorf("jit: nil block")
	}

	var pool ir.RegPool
	if useV2 {
		pool = ir.AMD64Pool_V2(res.block)
	} else {
		pool = ir.AMD64Pool(res.block)
	}
	pinned := ir.AMD64Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	if useV2 {
		_, lowerErr := ir.LowerAMD64_V2(ctx, res.block, alloc)
		if lowerErr != nil {
			return nil, nil, fmt.Errorf("jit lower: %w", lowerErr)
		}
	} else {
		_, lowerErr := ir.LowerAMD64(ctx, res.block, alloc)
		if lowerErr != nil {
			return nil, nil, fmt.Errorf("jit lower: %w", lowerErr)
		}
	}

	// Capture Prog listing before assembly.
	progDump := ctx.DumpProgs()

	code, err := ctx.Assemble()
	if err != nil {
		return nil, nil, fmt.Errorf("jit assemble: %w", err)
	}
	if len(code) == 0 {
		return nil, nil, fmt.Errorf("jit: assembler produced empty output")
	}

	execMem, err := allocExec(len(code))
	if err != nil {
		return nil, nil, fmt.Errorf("jit mmap: %w", err)
	}
	copy(execMem, code)

	blk := &compiledBlock{fn: uintptr(unsafe.Pointer(&execMem[0]))}
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
