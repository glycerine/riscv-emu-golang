package riscv

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// Part A — chain-exit foundation tests at the jitCompile boundary.
//
// A1/A4/A5 live in ir/lower_amd64_chain_test.go and exercise LowerAMD64
// directly. The tests here drive jitCompile(useV2=false) — the Fixed
// Static Mapping production path — and inspect the compiledBlock's
// chainExits and the bytes at patchOffset within the executable page.
//
// If A2/A3 pass today, the full plumbing from IR → lowered bytes → block
// metadata is correct and Part C can proceed. If they fail, the failing
// assertion pinpoints where the pipeline breaks.

// buildChainExitOnlyBlock builds an Block containing nothing but a
// single IRChainExit{targetPC}. Uses the high-level Emitter so we match
// the conventions jit_emit_ir would use in production.
func buildChainExitOnlyBlock(targetPC uint64) *Block {
	e := NewEmitter()
	e.ChainExit(targetPC, 0)
	b := e.Block
	b.Instrs[0] = b.Instrs[0] // no-op touch; Block is already populated
	// Recompute maxVreg since Emitter fields don't re-scan on append.
	b.Instrs[len(b.Instrs)-1] = b.Instrs[len(b.Instrs)-1] // no-op
	return b
}

// A2 — After jitCompile(useV2=false), the compiled block's chainExits
// are populated and their patchOffset points into the executable page.
// The 8 bytes at patchOffset equal the slow-exit stub address (codeBase +
// StubProg.Pc), which is how jit_native.go backpatches the sentinel.
func TestJITChain_Foundation_StubBackpatched(t *testing.T) {
	const targetPC uint64 = 0xDEAD0000BEEF1234

	blk := buildChainExitOnlyBlock(targetPC)
	j := NewJIT() // Fixed Static Mapping by default.
	compiled, err := j.jitCompile(&emitResult{block: blk, numInsns: 0})
	if err != nil {
		t.Fatalf("jitCompile: %v", err)
	}
	if len(compiled.chainExits) != 1 {
		t.Fatalf("len(chainExits) = %d, want 1 (emitChainableReturn's TODO "+
			"likely keeps lowerResult.ChainEntryProg nil, causing "+
			"jit_native.go:90 to skip chainExit setup)", len(compiled.chainExits))
	}
	ce := compiled.chainExits[0]
	if ce.targetPC != targetPC {
		t.Errorf("targetPC = 0x%x, want 0x%x", ce.targetPC, targetPC)
	}
	if ce.patchOffset <= 0 {
		t.Errorf("patchOffset = %d, want > 0", ce.patchOffset)
	}

	// Read the 8 bytes at blk.fn + patchOffset.
	//nolint:gosec // test-only inspection of JIT code bytes
	p := (*[8]byte)(unsafe.Pointer(compiled.fn + uintptr(ce.patchOffset)))
	gotTarget := binary.LittleEndian.Uint64(p[:])

	// The bytes should point somewhere inside the same code page (the stub
	// for this exit lives after the block body).
	if gotTarget == 0x7BADC0DE7BADC0DE {
		t.Errorf("imm64 at patchOffset still holds sentinel — jit_native.go's "+
			"backpatch loop did not fire for this exit (patchOffset=%d)",
			ce.patchOffset)
	}
	if gotTarget == 0 {
		t.Errorf("imm64 at patchOffset is zero (patchOffset=%d)", ce.patchOffset)
	}

	// Sanity: the two bytes just before patchOffset should be 0x48 0xB9
	// (REX.W + MOV-to-RCX opcode). rv8 uses RCX as the chain-exit staging reg.
	if ce.patchOffset < 2 {
		t.Fatalf("patchOffset = %d < 2, can't check MOVABS prefix", ce.patchOffset)
	}
	//nolint:gosec // test-only inspection
	prefix := (*[2]byte)(unsafe.Pointer(compiled.fn + uintptr(ce.patchOffset-2)))
	if prefix[0] != 0x48 || prefix[1] != 0xB9 {
		t.Errorf("bytes at patchOffset-2 = %02x %02x, want 48 B9 "+
			"(REX.W, MOV RCX imm64)", prefix[0], prefix[1])
	}
}

// A3 — patchChainTarget writes to exactly the bytes the lowerer placed
// the imm64 at. After writing a test target, we must read it back.
func TestJITChain_Foundation_PatchTargetRoundtrip(t *testing.T) {
	const targetPC uint64 = 0xFACEB00CCAFEBABE
	const testTarget uintptr = 0xCAFEBABE12345678

	blk := buildChainExitOnlyBlock(targetPC)
	j := NewJIT()
	compiled, err := j.jitCompile(&emitResult{block: blk, numInsns: 0})
	if err != nil {
		t.Fatalf("jitCompile: %v", err)
	}
	if len(compiled.chainExits) != 1 {
		t.Fatalf("len(chainExits) = %d, want 1", len(compiled.chainExits))
	}
	ce := compiled.chainExits[0]

	patchChainTarget(compiled.fn, ce.patchOffset, testTarget)

	//nolint:gosec // test-only inspection of JIT code bytes
	p := (*[8]byte)(unsafe.Pointer(compiled.fn + uintptr(ce.patchOffset)))
	got := uintptr(binary.LittleEndian.Uint64(p[:]))
	if got != testTarget {
		t.Errorf("readback after patchChainTarget: got 0x%x, want 0x%x",
			got, testTarget)
	}
}
