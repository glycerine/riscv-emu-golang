//go:build !tcc

package riscv

// jit_native.go — Native IR→machine-code compilation pipeline.
// Replaces TCC: emitBlock produces ir.Block, which is lowered and assembled
// to native code via the goasm package (no cgo).

import "unsafe"

// compiledBlock holds a natively-compiled function pointer.
type compiledBlock struct {
	fn      uintptr // native function pointer (mmap'd executable memory)
	backing []byte  // keeps mmap'd memory alive for GC
}

// jitCompile compiles an IR block to native code and returns a compiledBlock.
func jitCompile(res *emitResult) (*compiledBlock, error) {
	// TODO: Phase 5 Step 8 — wire up ir.Allocate → ir.LowerAMD64 → goasm.Assemble
	// For now, return nil to fall back to interpreter.
	_ = res
	return nil, nil
}

// allocExec allocates executable memory via mmap.
func allocExec(size int) ([]byte, error) {
	_ = size
	_ = unsafe.Pointer(nil) // keep import
	// TODO: implement via syscall.Mmap with PROT_READ|PROT_WRITE|PROT_EXEC
	return nil, nil
}
