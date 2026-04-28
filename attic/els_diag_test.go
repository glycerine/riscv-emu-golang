package riscv

import (
	"fmt"
	"riscv"
	"testing"
	"time"
)

// TestELS_vs_Fixed_Long runs the bench guest through both ELS and Fixed
// allocators in lockstep, comparing IC and next-PC after every block.
// This catches register allocation bugs that produce wrong branch outcomes.
//
// Known issue: ELS diverges at block ~612061 (PC=0x1050), producing a wrong
// branch that loops the guest forever instead of progressing to the exit.
func TestELS_vs_Fixed_Long(t *testing.T) {
	elfData := loadCPUELF(t)

	cpuE, memE := newBenchCPU(t, elfData)
	defer memE.Free()
	cpuF, memF := newBenchCPU(t, elfData)
	defer memF.Free()

	jitE := riscv.NewJIT()
	jitE.SetAllocStrategy("els")
	jitF := riscv.NewJIT()
	jitF.SetAllocStrategy("fixed")

	start := time.Now()
	for block := 0; block < 700000; block++ {
		pcE, pcF := cpuE.PC(), cpuF.PC()
		if pcE != pcF {
			t.Fatalf("block %d: PC divergence: els=0x%x fixed=0x%x", block, pcE, pcF)
		}
		icE, errE := jitE.StepBlock(cpuE)
		icF, errF := jitF.StepBlock(cpuF)
		if icE != icF || cpuE.PC() != cpuF.PC() {
			t.Fatalf("block %d (pc=0x%x): IC els=%d fixed=%d, nextPC els=0x%x fixed=0x%x",
				block, pcE, icE, icF, cpuE.PC(), cpuF.PC())
		}
		if errE != nil || errF != nil {
			t.Logf("block %d: exit err=%v after %v", block, errE, time.Since(start))
			return
		}
	}
	t.Logf("700k blocks match in %v", time.Since(start))
}

// TestELS_DumpFailingBlock runs to the divergence point and dumps
// the IR, allocations, and compiled Progs for both allocators.
func TestELS_DumpFailingBlock(t *testing.T) {
	elfData := loadCPUELF(t)

	// Use Fixed to run to the failing block's PC (it produces correct results).
	cpu, mem := newBenchCPU(t, elfData)
	defer mem.Free()

	jitF := riscv.NewJIT()
	jitF.SetAllocStrategy("fixed")

	// Run until we reach a block at PC=0x1050 (or close to divergence).
	// The divergence is at block ~612061, but the block at 0x1050 is compiled
	// on first encounter and cached. We just need to emit the IR for 0x1050.
	for block := 0; block < 612062; block++ {
		_, err := jitF.StepBlock(cpu)
		if err != nil {
			t.Logf("stopped at block %d: %v", block, err)
			break
		}
	}

	// Emit the IR block at PC=0x1050.
	res := riscv.EmitBlockForBench(mem, 0x1050)
	if res == nil {
		t.Fatal("emitBlock returned nil for PC=0x1050")
	}
	b := res.Block

	t.Logf("Block at PC=0x1050: %d IR instructions, %d RISC-V insns",
		len(b.Instrs), res.NumInsns)

	// Dump IR.
	for i, ins := range b.Instrs {
		t.Logf("  [%3d] %s", i, ins.String())
	}

	// Run both allocators.
	poolE := ir.AMD64Pool(b)
	poolF := ir.AMD64Pool(b)
	pinned := ir.AMD64Pinned()

	allocELS := ir.NewAllocator().Allocate(b, poolE, pinned, nil)
	allocFixed := ir.NewFixedStaticAllocator().Allocate(b, poolF, pinned, nil)

	t.Logf("\nELS: %d stack slots", allocELS.StackSlots)
	t.Logf("Fixed: %d stack slots", allocFixed.StackSlots)

	// Dump allocation differences.
	maxVR := len(allocELS.Kind)
	if len(allocFixed.Kind) > maxVR {
		maxVR = len(allocFixed.Kind)
	}
	diffs := 0
	for vr := 0; vr < maxVR; vr++ {
		var kE, kF ir.AllocKind
		if vr < len(allocELS.Kind) {
			kE = allocELS.Kind[vr]
		}
		if vr < len(allocFixed.Kind) {
			kF = allocFixed.Kind[vr]
		}
		if kE == ir.AllocUnused && kF == ir.AllocUnused {
			continue
		}
		eHost := hostRegName(allocELS, ir.VReg(vr))
		fHost := hostRegName(allocFixed, ir.VReg(vr))
		marker := " "
		if kE != kF || eHost != fHost {
			marker = "*"
			diffs++
		}
		t.Logf("%s VReg %3d: ELS=%-12s Fixed=%-12s", marker, vr, eHost, fHost)
	}
	t.Logf("\n%d allocation differences", diffs)

	// Dump ELS interval details for VRegs that differ.
	t.Logf("\nELS intervals for differing VRegs:")
	for vr := 0; vr < maxVR; vr++ {
		var kE, kF ir.AllocKind
		if vr < len(allocELS.Kind) {
			kE = allocELS.Kind[vr]
		}
		if vr < len(allocFixed.Kind) {
			kF = allocFixed.Kind[vr]
		}
		eHost := hostRegName(allocELS, ir.VReg(vr))
		fHost := hostRegName(allocFixed, ir.VReg(vr))
		if kE != kF || eHost != fHost {
			for _, ia := range allocELS.IntervalMap {
				if ia.Interval.VReg == ir.VReg(vr) {
					t.Logf("  VReg %d: [%d, %d] -> %s",
						vr, ia.Interval.Start, ia.Interval.End, regName(ia.Host))
				}
			}
		}
	}
}

func hostRegName(alloc *ir.Allocation, vr ir.VReg) string {
	if int(vr) >= len(alloc.Kind) {
		return "unused"
	}
	switch alloc.Kind[vr] {
	case ir.AllocUnused:
		return "unused"
	case ir.AllocStack:
		slot := int16(-1)
		if int(vr) < len(alloc.SpillSlot) {
			slot = alloc.SpillSlot[vr]
		}
		return fmt.Sprintf("stack[%d]", slot)
	case ir.AllocReg:
		for _, ia := range alloc.IntervalMap {
			if ia.Interval.VReg == vr {
				return regName(ia.Host)
			}
		}
		return "reg(?)"
	}
	return "?"
}

func regName(host int16) string {
	names := map[int16]string{
		0: "AX", 1: "CX", 2: "DX", 3: "BX", 4: "SP", 5: "BP",
		6: "SI", 7: "DI", 8: "R8", 9: "R9", 10: "R10", 11: "R11",
		12: "R12", 13: "R13", 14: "R14", 15: "R15",
	}
	if n, ok := names[host]; ok {
		return n
	}
	if host >= 16 && host < 32 {
		return fmt.Sprintf("X%d", host-16)
	}
	return fmt.Sprintf("reg%d", host)
}
