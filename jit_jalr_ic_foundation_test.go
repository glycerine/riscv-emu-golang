package riscv

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"riscv/ir"
)

// Foundation tests for the 2-way JALR inline-cache at the jitCompile
// boundary (Phase 1.5, Step 3). Byte-level IR-side tests live in
// ir/lower_amd64_jalric_test.go. These drive jitCompileWith
// (useV2=false) — the Fixed Static Mapping production path — and
// inspect the compiledBlock's jalrICs and the bytes at the four
// imm64 patch offsets.

// buildJalrICOnlyBlock builds an IR block with a single 2-way IC
// site: populate vreg x10 with a constant, then JalrIC targeting it.
func buildJalrICOnlyBlock(siteIdx int) *ir.Block {
	e := ir.NewEmitter()
	e.Const(ir.VReg(10), 0x1000)
	e.WriteBackAll()
	e.JalrIC(ir.VReg(10), siteIdx)
	return e.Block
}

// readSlot returns the 8-byte value at the given offset inside a
// compiled block's executable memory.
func readSlot(fn uintptr, off int) uint64 {
	//nolint:gosec // test-only JIT-code inspection
	p := (*[8]byte)(unsafe.Pointer(fn + uintptr(off)))
	return binary.LittleEndian.Uint64(p[:])
}

// JF1 — After jitCompileWith, compiledBlock.jalrICs has the site and
// four patch offsets (pc[0..1], fn[0..1]). Before any patch:
//   - cache_pc[0] == cache_pc[1] == 0xFFFFFFFFFFFFFFFF (unmatchable)
//   - cache_fn[0] == cache_fn[1] == miss stub address (same, inside
//     the code page)
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

	// All four offsets must be distinct and positive.
	offs := []int{
		ic.pcPatchOff[0], ic.pcPatchOff[1],
		ic.fnPatchOff[0], ic.fnPatchOff[1],
	}
	for i, off := range offs {
		if off <= 0 {
			t.Errorf("off[%d] = %d, want > 0", i, off)
		}
		for j := i + 1; j < len(offs); j++ {
			if offs[j] == off {
				t.Errorf("off[%d] and off[%d] both = %d (should be distinct)",
					i, j, off)
			}
		}
	}

	// pc slots: both should be ^uint64(0).
	for k := 0; k < 2; k++ {
		v := readSlot(compiled.fn, ic.pcPatchOff[k])
		if v != ^uint64(0) {
			t.Errorf("cache_pc[%d] = 0x%016x, want 0xFFFFFFFFFFFFFFFF", k, v)
		}
	}

	// fn slots: both should equal the miss stub address (inside code page).
	fn0 := readSlot(compiled.fn, ic.fnPatchOff[0])
	fn1 := readSlot(compiled.fn, ic.fnPatchOff[1])
	if fn0 != fn1 {
		t.Errorf("cache_fn[0] (0x%x) != cache_fn[1] (0x%x) at init",
			fn0, fn1)
	}
	if fn0 < uint64(compiled.fn) || fn0 > uint64(compiled.fn)+8192 {
		t.Errorf("cache_fn[0] = 0x%x not within code page [0x%x, +8192)",
			fn0, compiled.fn)
	}

	// MOVABS R10 prefix (49 BA) sanity at each slot-2.
	for i, off := range offs {
		//nolint:gosec // test-only inspection
		prefix := (*[2]byte)(unsafe.Pointer(compiled.fn + uintptr(off-2)))
		if prefix[0] != 0x49 || prefix[1] != 0xBA {
			t.Errorf("off[%d] prefix = %02x %02x, want 49 BA (MOVABS R10)",
				i, prefix[0], prefix[1])
		}
	}
}

// JF3 — tryPatchJalrIC uses shift semantics: first miss installs
// target in slot 0 (and sentinel-cum-stubAddr move to slot 1);
// second miss with different target shifts slot 0 to slot 1 and
// installs new target in slot 0; a third miss repeating the first
// target finds slot 1 still holding it (no patch needed in hardware
// — but our Go dispatcher patches unconditionally, so slot 1 → slot 0).
// This test verifies the readback after each patch.
func TestJITJalrIC_Foundation_ShiftSemantics(t *testing.T) {
	blk := buildJalrICOnlyBlock(0)
	j := NewJIT()
	compiled, err := j.jitCompileWith(&emitResult{block: blk, numInsns: 0}, false)
	if err != nil {
		t.Fatalf("jitCompileWith: %v", err)
	}
	// Install the compiled block in the JIT's block cache so that the
	// dispatcher's lookupBlock can find "targets" we patch toward. We do
	// this by registering synthetic "target blocks" at chosen PCs that
	// all point to the same compiled fn (chainEntry).
	target1 := &compiledBlock{fn: compiled.fn, chainEntry: compiled.chainEntry}
	target2 := &compiledBlock{fn: compiled.fn, chainEntry: compiled.chainEntry + 1}

	// Choose target PCs whose cacheIdx hashes differ so both survive
	// in the direct-mapped block cache. cacheIdx = (pc >> 1) & 0xFFF.
	const targetPC1 = uint64(0x1000) // hashes to slot 0x800
	const targetPC2 = uint64(0x4000) // hashes to slot 0x000
	j.insertBlock(targetPC1, target1)
	j.insertBlock(targetPC2, target2)

	ic := compiled.jalrICs[0]

	// Initial: pc[0] = pc[1] = sentinel; fn[0] = fn[1] = stubAddr.
	initialPc := readSlot(compiled.fn, ic.pcPatchOff[0])
	initialFn := readSlot(compiled.fn, ic.fnPatchOff[0])
	if initialPc != ^uint64(0) {
		t.Fatalf("initial pc[0] = 0x%x, want sentinel", initialPc)
	}

	// Miss 1: target1. Shift: slot 1 ← slot 0 (sentinel/stubAddr);
	// slot 0 ← (target1PC, target1.chainEntry).
	j.tryPatchJalrIC(compiled, 0, targetPC1)
	if got := readSlot(compiled.fn, ic.pcPatchOff[0]); got != targetPC1 {
		t.Errorf("after miss1 pc[0] = 0x%x, want 0x%x", got, targetPC1)
	}
	if got := readSlot(compiled.fn, ic.fnPatchOff[0]); got != uint64(target1.chainEntry) {
		t.Errorf("after miss1 fn[0] = 0x%x, want 0x%x",
			got, target1.chainEntry)
	}
	if got := readSlot(compiled.fn, ic.pcPatchOff[1]); got != initialPc {
		t.Errorf("after miss1 pc[1] = 0x%x, want original sentinel 0x%x",
			got, initialPc)
	}
	if got := readSlot(compiled.fn, ic.fnPatchOff[1]); got != initialFn {
		t.Errorf("after miss1 fn[1] = 0x%x, want original stubAddr 0x%x",
			got, initialFn)
	}

	// Miss 2: target2. Shift: slot 1 ← slot 0 (target1); slot 0 ← target2.
	j.tryPatchJalrIC(compiled, 0, targetPC2)
	if got := readSlot(compiled.fn, ic.pcPatchOff[0]); got != targetPC2 {
		t.Errorf("after miss2 pc[0] = 0x%x, want 0x%x", got, targetPC2)
	}
	if got := readSlot(compiled.fn, ic.pcPatchOff[1]); got != targetPC1 {
		t.Errorf("after miss2 pc[1] = 0x%x, want 0x%x (shifted from slot 0)",
			got, targetPC1)
	}
	if got := readSlot(compiled.fn, ic.fnPatchOff[0]); got != uint64(target2.chainEntry) {
		t.Errorf("after miss2 fn[0] = 0x%x, want 0x%x",
			got, target2.chainEntry)
	}
	if got := readSlot(compiled.fn, ic.fnPatchOff[1]); got != uint64(target1.chainEntry) {
		t.Errorf("after miss2 fn[1] = 0x%x, want 0x%x (shifted from slot 0)",
			got, target1.chainEntry)
	}

	// Counter should have incremented twice.
	if j.ChainPatchedJalr != 2 {
		t.Errorf("ChainPatchedJalr = %d, want 2", j.ChainPatchedJalr)
	}
}

// JF2 — patchChainTarget (generic 8-byte writer) works on all four
// slots independently; readback matches.
func TestJITJalrIC_Foundation_PatchRoundtrip(t *testing.T) {
	blk := buildJalrICOnlyBlock(0)
	j := NewJIT()
	compiled, err := j.jitCompileWith(&emitResult{block: blk, numInsns: 0}, false)
	if err != nil {
		t.Fatalf("jitCompileWith: %v", err)
	}
	ic := compiled.jalrICs[0]

	vals := [4]uintptr{
		0x1111111111111111,
		0x2222222222222222,
		0x3333333333333333,
		0x4444444444444444,
	}
	offs := [4]int{
		ic.pcPatchOff[0], ic.pcPatchOff[1],
		ic.fnPatchOff[0], ic.fnPatchOff[1],
	}
	for i := range offs {
		patchChainTarget(compiled.fn, offs[i], vals[i])
	}
	for i := range offs {
		got := uintptr(readSlot(compiled.fn, offs[i]))
		if got != vals[i] {
			t.Errorf("slot %d readback: got 0x%x, want 0x%x", i, got, vals[i])
		}
	}
}
