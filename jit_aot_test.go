package riscv

import (
	"encoding/binary"
	"fmt"
	"os"
	"riscv/goasm"
	"riscv/internal/jitcall"
	"riscv/ir"
	"syscall"
	"testing"
	"unsafe"
)

func jitcallCallAOT(fn uintptr, x *[32]uint64, f *[32]uint64, fcsr *uint32,
	memBase uintptr, memMask uint64, pc uint64) jitcall.Result {
	return jitcall.CallAOT(fn, x, f, fcsr,
		memBase, memMask,
		0, 0, // decoderCacheBase, decoderCacheMask (no JALR in test)
		0, 0, // vaddrBegin, segSize (unused without JALR)
		pc)
}

// compileAOTBlock emits IR, lowers via LowerAMD64AOT, assembles, and
// copies to executable memory. It also produces a VizJit dump so the
// generated assembly can be inspected under debug_vizjit_dir/.
func compileAOTBlock(t *testing.T, mem *GuestMemory, startPC, endPC uint64) (fn uintptr, cleanup func()) {
	t.Helper()
	res := emitBlockLinear(mem, startPC, endPC)
	if res == nil {
		t.Fatal("emitBlockLinear returned nil")
	}
	if res.block == nil {
		t.Fatal("emitBlockLinear: block is nil")
	}

	t.Logf("IR block: %d instructions, DispatchPCs=%v",
		len(res.block.Instrs), formatDispatchPCs(res.block))

	j := NewJIT()
	pool := ir.AMD64Pool(res.block)
	pinned := ir.AMD64Pinned()
	alloc := j.irAlloc.Allocate(res.block, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	lowerResult, err := ir.LowerAMD64AOT(ctx, res.block, alloc)
	if err != nil {
		t.Fatalf("LowerAMD64AOT: %v", err)
	}

	// Capture goasm Prog listing BEFORE Assemble (which consumes state).
	progs := ctx.DumpProgs()

	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("Assemble: zero bytes")
	}

	execMem, err := allocExec(len(code))
	if err != nil {
		t.Fatalf("allocExec: %v", err)
	}
	copy(execMem, code)

	fn = uintptr(unsafe.Pointer(&execMem[0]))

	// Backpatch chain-exit sentinels → slow-exit stub addresses.
	// Same logic as jitCompileAOTSegment Pass 2.
	for _, ce := range lowerResult.ChainExits {
		patchOff := int(ce.MovProg.Pc) + 2
		stubAddr := fn + uintptr(ce.StubProg.Pc)
		binary.LittleEndian.PutUint64(execMem[patchOff:], uint64(stubAddr))
	}

	// VizJit dump — writes Guest RISC-V + IR + Host asm to disk.
	vizJitDump(startPC, endPC, mem, res.block, progs, len(code), fn)
	if dir, on := vizJitEnabled(); on {
		tag := getVizJitTag()
		t.Logf("VizJit dump: %s/%s.gocpu.asm.pc_0x%08x.asm", dir, tag, startPC)
	}

	// Also log the progs to test output for immediate visibility.
	t.Logf("Host asm (%d bytes):\n%s", len(code), progs)

	// Write raw bytes for offline disassembly:
	//   objdump -b binary -m i386:x86-64 -D debug_vizjit_dir/*.bin
	if dir, on := vizJitEnabled(); on {
		binPath := fmt.Sprintf("%s/%s.gocpu.pc_0x%08x.bin", dir, getVizJitTag(), startPC)
		_ = os.WriteFile(binPath, code, 0o644)
		t.Logf("Raw bytes: %s", binPath)
	}

	cleanup = func() { syscall.Munmap(execMem) }
	return
}

func formatDispatchPCs(block *ir.Block) string {
	if len(block.DispatchPCs) == 0 {
		return "(none)"
	}
	s := "{"
	for pc, label := range block.DispatchPCs {
		s += fmt.Sprintf("0x%x→L%d ", pc, label)
	}
	return s + "}"
}

// TestAOT_DispatchTable verifies the PC dispatch table routes correctly
// when re-entering a function at a mid-function PC.
//
// Guest program [0x1000, 0x1010):
//
//	0x1000  ADDI x1, x0, 42      marker: "entered at function start"
//	0x1004  ADDI x2, x0, 1       another marker
//	0x1008  ADDI x3, x0, 77      code after dispatch target
//	0x100c  BNE x10, x11, -4     backward branch to 0x1008 (never taken: x10==x11==0)
//
// The backward branch creates 0x1008 as a dispatch target in DispatchPCs.
// BNE is never taken at runtime (x10 and x11 are both zero).
func TestAOT_DispatchTable(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	mem.Store32(0x1000, ienc(opOPIMM, 0, 1, 0, 42))    // ADDI x1, x0, 42
	mem.Store32(0x1004, ienc(opOPIMM, 0, 2, 0, 1))     // ADDI x2, x0, 1
	mem.Store32(0x1008, ienc(opOPIMM, 0, 3, 0, 77))    // ADDI x3, x0, 77
	mem.Store32(0x100c, benc(opBRANCH, 1, 10, 11, -4)) // BNE x10, x11, -4 → 0x1008

	fn, cleanup := compileAOTBlock(t, mem, 0x1000, 0x1010)
	defer cleanup()

	t.Run("entry_at_startPC", func(t *testing.T) {
		var x [32]uint64
		var f [32]uint64
		var fcsr uint32

		result := jitcallCallAOT(fn, &x, &f, &fcsr,
			mem.Base(), mem.Mask(), 0x1000)

		if x[1] != 42 {
			t.Errorf("x[1] = %d, want 42 (ADDI x1, x0, 42 should execute)", x[1])
		}
		if x[2] != 1 {
			t.Errorf("x[2] = %d, want 1 (ADDI x2, x0, 1 should execute)", x[2])
		}
		if x[3] != 77 {
			t.Errorf("x[3] = %d, want 77 (ADDI x3, x0, 77 should execute)", x[3])
		}
		if result.PC != 0x1010 {
			t.Errorf("result.PC = 0x%x, want 0x1010", result.PC)
		}
		t.Logf("entry_at_startPC: x[1]=%d x[2]=%d x[3]=%d PC=0x%x IC=%d Status=%d",
			x[1], x[2], x[3], result.PC, result.IC, result.Status)
	})

	t.Run("entry_at_dispatch_target", func(t *testing.T) {
		var x [32]uint64
		var f [32]uint64
		var fcsr uint32

		result := jitcallCallAOT(fn, &x, &f, &fcsr,
			mem.Base(), mem.Mask(), 0x1008)

		if x[1] != 0 {
			t.Errorf("x[1] = %d, want 0 (ADDI x1 should be SKIPPED by dispatch)", x[1])
		}
		if x[2] != 0 {
			t.Errorf("x[2] = %d, want 0 (ADDI x2 should be SKIPPED by dispatch)", x[2])
		}
		if x[3] != 77 {
			t.Errorf("x[3] = %d, want 77 (ADDI x3 should execute after dispatch)", x[3])
		}
		if result.PC != 0x1010 {
			t.Errorf("result.PC = 0x%x, want 0x1010", result.PC)
		}
		t.Logf("entry_at_dispatch_target: x[1]=%d x[2]=%d x[3]=%d PC=0x%x IC=%d Status=%d",
			x[1], x[2], x[3], result.PC, result.IC, result.Status)
	})
}
