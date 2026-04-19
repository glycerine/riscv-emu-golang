//go:build !tcc

package riscv

// jit_native.go — Native IR→machine-code compilation pipeline.
// Replaces TCC: emitBlock produces ir.Block, which is lowered and assembled
// to native code via the goasm package (no cgo).

import (
	"fmt"
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

// compiledBlock holds a natively-compiled function pointer.
type compiledBlock struct {
	fn     uintptr        // native function pointer (mmap'd executable memory)
	shadow *compiledBlock // V2 shadow block for DebugV1V2 comparison
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
	var lowerErr error
	if useV2 {
		lowerErr = ir.LowerAMD64_V2(ctx, res.block, alloc)
	} else {
		lowerErr = ir.LowerAMD64(ctx, res.block, alloc)
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

	return &compiledBlock{
		fn: uintptr(unsafe.Pointer(&execMem[0])),
	}, nil
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

	var lowerErr error
	if useV2 {
		lowerErr = ir.LowerAMD64_V2(ctx, res.block, alloc)
	} else {
		lowerErr = ir.LowerAMD64(ctx, res.block, alloc)
	}
	if lowerErr != nil {
		return nil, nil, fmt.Errorf("jit lower: %w", lowerErr)
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
