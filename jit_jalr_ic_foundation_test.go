package riscv

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"riscv/ir"
)

// Foundation tests for JALR inline-cache at the jitCompile boundary
// (Phase 1, Step 3). Parallel to jit_chain_foundation_test.go.
//
// Byte-level IR-side tests live in ir/lower_amd64_jalric_test.go. The
// tests here drive jitCompileWith(useV2=false) — the Fixed Static
// Mapping production path — and inspect the compiledBlock's jalrICs
// and the bytes at the two imm64 patch offsets.

// buildJalrICOnlyBlock builds an IR block with a single JalrIC site:
// populate vreg x10 with a constant, then JalrIC targeting it.
func buildJalrICOnlyBlock(siteIdx int) *ir.Block {
	e := ir.NewEmitter()
	e.Const(ir.VReg(10), 0x1000)
	e.WriteBackAll()
	e.JalrIC(ir.VReg(10), siteIdx)
	return e.Block
}

// JF1 — After jitCompileWith, compiledBlock.jalrICs has the site and
// both patch offsets point into the executable page. Before any patch,
// cache_pc == 0xFFFFFFFFFFFFFFFF (unmatchable sentinel) and cache_fn
// == address of the miss stub (inside the same code page).
func TestJITJalrIC_Foundation_InitialSlotsBackpatched(t *testing.T) {
	blk := buildJalrICOnlyBlock(3)
	j := NewJIT()
	compiled, err := j.jitCompileWith(&emitResult{block: blk, numInsns: 0}, false)
	if err != nil {
		t.Fatalf("jitCompileWith: %v", err)
	}
	if len(compiled.jalrICs) != 1 {
		t.Fatalf("len(jalrICs) = %d, want 1", len(compiled.jalrICs))
	}
	ic := compiled.jalrICs[0]
	if ic.siteIdx != 3 {
		t.Errorf("siteIdx = %d, want 3", ic.siteIdx)
	}
	if ic.pcPatchOffset <= 0 || ic.fnPatchOffset <= 0 {
		t.Errorf("patch offsets non-positive: pc=%d fn=%d",
			ic.pcPatchOffset, ic.fnPatchOffset)
	}
	if ic.pcPatchOffset == ic.fnPatchOffset {
		t.Errorf("patch offsets equal (%d) — should be distinct slots",
			ic.pcPatchOffset)
	}

	// Read 8 bytes at each patch offset.
	//nolint:gosec // test-only JIT-code inspection
	pcSlot := (*[8]byte)(unsafe.Pointer(compiled.fn + uintptr(ic.pcPatchOffset)))
	//nolint:gosec // test-only JIT-code inspection
	fnSlot := (*[8]byte)(unsafe.Pointer(compiled.fn + uintptr(ic.fnPatchOffset)))

	pcVal := binary.LittleEndian.Uint64(pcSlot[:])
	fnVal := binary.LittleEndian.Uint64(fnSlot[:])

	if pcVal != ^uint64(0) {
		t.Errorf("cache_pc slot = 0x%016x, want 0xFFFFFFFFFFFFFFFF (unmatchable)",
			pcVal)
	}
	if fnVal == 0x7BADC0DE7BADC0DE {
		t.Errorf("cache_fn slot still holds sentinel — backpatchJalrICs never ran")
	}
	if fnVal == 0 {
		t.Errorf("cache_fn slot is zero")
	}

	// fnVal must point into the same code page: within [codeBase, codeBase+8KB).
	// (8KB is enough slack — our blocks are small, miss stub is after main code.)
	if fnVal < uint64(compiled.fn) || fnVal > uint64(compiled.fn)+8192 {
		t.Errorf("cache_fn slot = 0x%x, not within code page [0x%x, 0x%x+8192)",
			fnVal, compiled.fn, compiled.fn)
	}

	// MOVABS prefix sanity at each slot-2: must be 49 BA.
	for name, off := range map[string]int{"pc": ic.pcPatchOffset, "fn": ic.fnPatchOffset} {
		//nolint:gosec // test-only inspection
		prefix := (*[2]byte)(unsafe.Pointer(compiled.fn + uintptr(off-2)))
		if prefix[0] != 0x49 || prefix[1] != 0xBA {
			t.Errorf("%s slot prefix = %02x %02x, want 49 BA (MOVABS R10)",
				name, prefix[0], prefix[1])
		}
	}
}

// JF2 — patchChainTarget (generic 8-byte writer) can update both slots.
// After patching, readback matches. Proves the patching pipeline is
// byte-addressable and the two slots are independently writable.
func TestJITJalrIC_Foundation_PatchRoundtrip(t *testing.T) {
	const newPc = uintptr(0xCAFEBABE12345678)
	const newFn = uintptr(0xDEADBEEFDEADBEEF)

	blk := buildJalrICOnlyBlock(0)
	j := NewJIT()
	compiled, err := j.jitCompileWith(&emitResult{block: blk, numInsns: 0}, false)
	if err != nil {
		t.Fatalf("jitCompileWith: %v", err)
	}
	if len(compiled.jalrICs) != 1 {
		t.Fatalf("len(jalrICs) = %d, want 1", len(compiled.jalrICs))
	}
	ic := compiled.jalrICs[0]

	patchChainTarget(compiled.fn, ic.pcPatchOffset, newPc)
	patchChainTarget(compiled.fn, ic.fnPatchOffset, newFn)

	//nolint:gosec // test-only inspection
	gotPc := uintptr(binary.LittleEndian.Uint64(
		(*[8]byte)(unsafe.Pointer(compiled.fn + uintptr(ic.pcPatchOffset)))[:]))
	//nolint:gosec // test-only inspection
	gotFn := uintptr(binary.LittleEndian.Uint64(
		(*[8]byte)(unsafe.Pointer(compiled.fn + uintptr(ic.fnPatchOffset)))[:]))

	if gotPc != newPc {
		t.Errorf("cache_pc readback: got 0x%x, want 0x%x", gotPc, newPc)
	}
	if gotFn != newFn {
		t.Errorf("cache_fn readback: got 0x%x, want 0x%x", gotFn, newFn)
	}
}
