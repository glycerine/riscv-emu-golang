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

// compiledBlock holds a natively-compiled function pointer.
type compiledBlock struct {
	fn      uintptr // native function pointer (mmap'd executable memory)
	backing []byte  // prevents GC of mmap'd memory
}

// jitCompile compiles an IR block to native code and returns a compiledBlock.
func jitCompile(res *emitResult) (*compiledBlock, error) {
	if res.block == nil {
		return nil, fmt.Errorf("jit: nil block")
	}

	// Step 1: Register allocation.
	pool := ir.AMD64Pool(res.block)
	pinned := ir.AMD64Pinned()
	alloc := ir.Allocate(res.block, pool, pinned, nil)



	// Step 2: Create assembler context and emit ATEXT prologue.
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	// Step 3: Lower IR to x86-64 Progs.
	if err := ir.LowerAMD64(ctx, res.block, alloc); err != nil {
		return nil, fmt.Errorf("jit lower: %w", err)
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
		fn:      uintptr(unsafe.Pointer(&execMem[0])),
		backing: execMem,
	}, nil
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
