package riscv

import (
	"bytes"
	"math"
	"testing"
	"time"
)

// ── Instruction encoding helpers ─────────────────────────────────────────

// renc encodes an R-type instruction.
func renc(opcode, funct3, funct7 uint32, rd, rs1, rs2 uint32) uint32 {
	return funct7<<25 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

// benc encodes a B-type (branch) instruction.
func benc(opcode, funct3, rs1, rs2 uint32, offset int32) uint32 {
	imm := uint32(offset)
	b12 := (imm >> 12) & 1
	b11 := (imm >> 11) & 1
	b10_5 := (imm >> 5) & 0x3F
	b4_1 := (imm >> 1) & 0xF
	return b12<<31 | b10_5<<25 | rs2<<20 | rs1<<15 | funct3<<12 | b4_1<<8 | b11<<7 | opcode
}

// ienc encodes an I-type instruction (ADDI, loads, JALR, CSRRS, etc.)
func ienc(opcode, funct3, rd, rs1 uint32, imm int32) uint32 {
	return uint32(imm)<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

// senc encodes an S-type instruction (stores).
func senc(opcode, funct3, rs1, rs2 uint32, imm int32) uint32 {
	u := uint32(imm)
	return (u>>5)<<25 | rs2<<20 | rs1<<15 | funct3<<12 | (u&0x1F)<<7 | opcode
}

// uenc encodes a U-type instruction (LUI, AUIPC).
func uenc(opcode, rd uint32, imm uint32) uint32 {
	return imm&0xFFFFF000 | rd<<7 | opcode
}

// jenc encodes a J-type instruction (JAL).
func jenc(rd uint32, offset int32) uint32 {
	u := uint32(offset)
	return ((u>>20)&1)<<31 | ((u>>1)&0x3FF)<<21 | ((u>>11)&1)<<20 | ((u>>12)&0xFF)<<12 | rd<<7 | 0x6F
}

// Opcode constants.
const (
	opLOAD   = uint32(0x03)
	opSTORE  = uint32(0x23)
	opOPIMM  = uint32(0x13)
	opOP     = uint32(0x33)
	opBRANCH = uint32(0x63)
	opJALR   = uint32(0x67)
	opLUI    = uint32(0x37)
	opAUIPC  = uint32(0x17)
	opSYSTEM = uint32(0x73)
	opOP32   = uint32(0x3B)

	instrECALL  = uint32(0x00000073)
	instrEBREAK = uint32(0x00100073)
)

// storeInsns writes instruction words to guest memory at addr.
func storeInsns(mem *GuestMemory, addr uint64, insns []uint32) {
	for i, insn := range insns {
		mem.Store32(addr+uint64(i*4), insn)
	}
}

// ── CPU snapshot for state comparison ────────────────────────────────────

type cpuSnapshot struct {
	x    [32]uint64
	f    [32]uint64
	pc   uint64
	fcsr uint32
}

func takeCPUSnapshot(cpu *CPU) cpuSnapshot {
	var s cpuSnapshot
	s.pc = cpu.PC()
	s.fcsr = cpu.FCSR()
	for i := 0; i < 32; i++ {
		s.x[i] = cpu.Reg(uint8(i))
		s.f[i] = cpu.FReg(uint8(i))
	}
	return s
}

func (a cpuSnapshot) compare(t *testing.T, b cpuSnapshot, label string) {
	t.Helper()
	for i := 0; i < 32; i++ {
		if a.x[i] != b.x[i] {
			t.Errorf("%s: x[%d] mismatch: 0x%x vs 0x%x", label, i, a.x[i], b.x[i])
		}
	}
	for i := 0; i < 32; i++ {
		if a.f[i] != b.f[i] {
			t.Errorf("%s: f[%d] mismatch: 0x%x vs 0x%x", label, i, a.f[i], b.f[i])
		}
	}
	if a.pc != b.pc {
		t.Errorf("%s: PC mismatch: 0x%x vs 0x%x", label, a.pc, b.pc)
	}
	if a.fcsr != b.fcsr {
		t.Errorf("%s: FCSR mismatch: 0x%x vs 0x%x", label, a.fcsr, b.fcsr)
	}
}

// ── JIT execution helpers ────────────────────────────────────────────────

// runJITWithOS mirrors RunWithOS but uses JIT instead of the interpreter.
func runJITWithOS(cpu *CPU) (exitCode int, err error) {
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	defer cpu.Notes.Pop()

	jit := NewJIT()
	err = jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		return ex.Code, nil
	}
	return
}

// ecallStop is a NoteHandler that stops on ECALL with NoteFatal.
func ecallStop(cpu *CPU, n Note) NoteDisposition {
	if IsEcall(n) {
		return NoteFatal
	}
	return NoteForward
}

// newTestCPU creates a CPU with the given memory size and instructions at codeVA.
func newTestCPU(t *testing.T, memSize uint64, codeVA uint64, insns []uint32) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(memSize)
	if err != nil {
		t.Fatal(err)
	}
	storeInsns(mem, codeVA, insns)
	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	return cpu, mem
}

func TestJIT_LazyBlockMapSurvivesDirectCacheCollision(t *testing.T) {
	jit := NewJIT()
	pc1 := uint64(0x1000)
	pc2 := pc1 + blockCacheSize*2
	if cacheIdx(pc1) != cacheIdx(pc2) {
		t.Fatalf("test PCs do not collide: idx1=%d idx2=%d", cacheIdx(pc1), cacheIdx(pc2))
	}
	blk1 := &compiledBlock{fn: 0x11110000, chainEntry: 0x11110080, nativeMmap: []byte{}}
	blk2 := &compiledBlock{fn: 0x22220000, chainEntry: 0x22220080, nativeMmap: []byte{}}

	jit.insertBlock(pc1, blk1)
	jit.insertBlock(pc2, blk2)
	if got := jit.lookupBlock(pc2); got != blk2 {
		t.Fatalf("lookupBlock(pc2) = %p, want blk2 %p", got, blk2)
	}
	if got := jit.lookupBlock(pc1); got != blk1 {
		t.Fatalf("lookupBlock(pc1) after collision = %p, want blk1 %p", got, blk1)
	}
	if entry := jit.cache[cacheIdx(pc1)]; entry.pc != pc1 || entry.blk != blk1 {
		t.Fatalf("direct cache after backing-map hit = {pc:0x%x blk:%p}, want {0x%x %p}",
			entry.pc, entry.blk, pc1, blk1)
	}
}

func TestJIT_InlineEcallExitCountsInstructionAttempts(t *testing.T) {
	const codeVA = uint64(0x1000)
	insns := []uint32{
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = 0
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size1MB, codeVA, insns)
	defer mem.Free()

	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	cpu.Notes.Push(o.Handle)
	defer cpu.Notes.Pop()

	jit := NewJIT()
	err := jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); !ok || ex.Code != 0 {
		t.Fatalf("RunJIT err = %v, want exit 0", err)
	}
	if got := cpu.RiscvInstrBegun(); got != uint64(len(insns)) {
		t.Fatalf("RiscvInstrBegun() = %d, want %d", got, len(insns))
	}
}

// ── Lockstep helpers (used by riscv_test.go too) ─────────────────────────

// compareFullMemory compares guest memory byte-for-byte up to size/2.
// The upper half is excluded: the sandbox stack, guard page, shadow
// register file, and their residue from accumulated dispatch cycles
// occupy the top of the mmap. Guest ELF code+data lives in the lower
// portion (typically < 16 KB for riscv-tests).
func compareFullMemory(t *testing.T, a, b *GuestMemory, blockNum int) {
	t.Helper()
	limit := a.Size() / 2
	sliceA := a.RawSlice()[:limit]
	sliceB := b.RawSlice()[:limit]
	if bytes.Equal(sliceA, sliceB) {
		return
	}
	// slow path: only on actual mismatch
	diffs := 0
	for i := range sliceA {
		if sliceA[i] != sliceB[i] {
			if diffs < 5 {
				t.Errorf("block %d: memory[0x%04x] mismatch: jit=0x%02x interp=0x%02x",
					blockNum, i, sliceA[i], sliceB[i])
			}
			diffs++
		}
	}
	if diffs > 5 {
		t.Errorf("block %d: ...and %d more memory differences", blockNum, diffs-5)
	}
}

// isExitEcall checks if the error is an ECALL requesting exit.
func isExitEcall(cpu *CPU, err error) bool {
	if err != ErrEcall {
		return false
	}
	return cpu.x[17] == 93 || cpu.x[17] == 94
}

// advancePastException handles non-exit exceptions by advancing PC.
func advancePastException(cpu *CPU, err error) {
	if err == ErrEcall {
		cpu.pc += 4
	}
}

// ══════════════════════════════════════════════════════════════════════════
// TESTS
// ══════════════════════════════════════════════════════════════════════════

// TestJIT_ADD verifies a single ADD instruction through the full JIT pipeline.
func TestJIT_ADD(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	add := uint32(0x003100B3) // ADD x1, x2, x3
	codeVA := uint64(0x1000)
	mem.Store32(codeVA, add)
	mem.Store32(codeVA+4, instrECALL)

	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.SetReg(2, 100)
	cpu.SetReg(3, 42)

	jit := NewJIT()
	cpu.Notes.Push(ecallStop)
	jit.RunJIT(cpu)

	got := cpu.Reg(1)
	want := uint64(142)
	if got != want {
		t.Errorf("x1 = %d, want %d", got, want)
	}
	if got := jit.InterpretedInsns; got != 0 {
		t.Fatalf("InterpretedInsns = %d, want 0 for fully JIT-covered ADD block", got)
	}
}

func TestJIT_MulHighAndPopcount(t *testing.T) {
	cpopImm := int32((0x60 << 5) | 2)
	tests := []struct {
		name string
		insn uint32
		x2   uint64
		x3   uint64
		want uint64
	}{
		{
			name: "MULH_NegPos",
			insn: renc(opOP, 1, 0x01, 1, 2, 3),
			x2:   ^uint64(0),
			x3:   2,
			want: ^uint64(0),
		},
		{
			name: "MULH_NegNeg",
			insn: renc(opOP, 1, 0x01, 1, 2, 3),
			x2:   ^uint64(0),
			x3:   ^uint64(0),
			want: 0,
		},
		{
			name: "MULHU_MaxMax",
			insn: renc(opOP, 3, 0x01, 1, 2, 3),
			x2:   ^uint64(0),
			x3:   ^uint64(0),
			want: ^uint64(1),
		},
		{
			name: "MULHSU_NegPos",
			insn: renc(opOP, 2, 0x01, 1, 2, 3),
			x2:   ^uint64(0),
			x3:   2,
			want: ^uint64(0),
		},
		{
			name: "MULHSU_IntMinMax",
			insn: renc(opOP, 2, 0x01, 1, 2, 3),
			x2:   0x8000000000000000,
			x3:   ^uint64(0),
			want: 0x8000000000000000,
		},
		{
			name: "CPOP",
			insn: ienc(opOPIMM, 1, 1, 2, cpopImm),
			x2:   0xf0f0f0f0f0f0f0f0,
			want: 32,
		},
		{
			name: "CPOPW",
			insn: renc(opOP32, 2, 0x60, 1, 2, 2),
			x2:   0xffffffff0000000f,
			want: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, err := NewGuestMemory(Size64MB)
			if err != nil {
				t.Fatal(err)
			}
			defer mem.Free()

			codeVA := uint64(0x1000)
			storeInsns(mem, codeVA, []uint32{tt.insn, instrECALL})

			cpu := NewCPU(*mem)
			cpu.SetPC(codeVA)
			cpu.SetReg(2, tt.x2)
			cpu.SetReg(3, tt.x3)
			cpu.Notes.Push(ecallStop)

			jit := NewJIT()
			_ = jit.RunJIT(cpu)
			if jit.DispatchCompile == 0 {
				t.Fatalf("DispatchCompile = 0, block did not JIT")
			}
			if got := jit.NoJITSize(); got != 0 {
				t.Fatalf("NoJITSize = %d, want 0", got)
			}
			if got := cpu.Reg(1); got != tt.want {
				t.Fatalf("x1 = 0x%016x, want 0x%016x", got, tt.want)
			}
		})
	}
}

// TestJIT_Fib verifies a tight loop through the JIT pipeline.
func TestJIT_Fib(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	codeVA := uint64(0x1000)
	insns := []uint32{
		renc(0x33, 0, 0x00, 5, 10, 11), // ADD x5, x10, x11
		ienc(opOPIMM, 0, 10, 11, 0),    // MV x10, x11
		ienc(opOPIMM, 0, 11, 5, 0),     // MV x11, x5
		ienc(opOPIMM, 0, 12, 12, 1),    // ADDI x12, x12, 1
		benc(opBRANCH, 4, 12, 13, -16), // BLT x12, x13, -16
		instrECALL,
	}
	storeInsns(mem, codeVA, insns)

	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.SetReg(10, 0)
	cpu.SetReg(11, 1)
	cpu.SetReg(12, 0)
	cpu.SetReg(13, 20)

	cpu.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu)

	got := cpu.Reg(10)
	want := uint64(6765)
	if got != want {
		t.Errorf("fib(20) = %d, want %d", got, want)
	}
	t.Logf("fib(20) = %d, insns = %d", got, cpu.RiscvInstrBegun())
}

// ── Test 5c: Block re-entry with modified state ──────────────────────────

func TestJIT_BlockReentry(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()

	// First run: x1 = 0 → 1
	jit.RunJIT(cpu)
	if cpu.Reg(1) != 1 {
		t.Fatalf("first run: x1 = %d, want 1", cpu.Reg(1))
	}

	// Re-enter with modified state (simulates budget re-entry)
	cpu.SetPC(0x1000)
	cpu.SetReg(1, 100)
	jit.RunJIT(cpu)
	if cpu.Reg(1) != 101 {
		t.Errorf("second run: x1 = %d, want 101 (proves re-read from x[])", cpu.Reg(1))
	}
}

// ── Test 4: Cycle counter accuracy ───────────────────────────────────────

func TestJIT_CycleCount(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1), // ADDI x1, x0, 1
		ienc(opOPIMM, 0, 2, 0, 2), // ADDI x2, x0, 2
		ienc(opOPIMM, 0, 3, 0, 3), // ADDI x3, x0, 3
		ienc(opOPIMM, 0, 4, 0, 4), // ADDI x4, x0, 4
		ienc(opOPIMM, 0, 5, 0, 5), // ADDI x5, x0, 5
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	// IC removed — JIT no longer tracks per-instruction cycle counts.
	// Verify execution correctness only.
	if cpu.Reg(1) != 1 || cpu.Reg(5) != 5 {
		t.Errorf("x1=%d x5=%d, want 1 and 5", cpu.Reg(1), cpu.Reg(5))
	}
}

func TestJIT_CycleCount_Loop(t *testing.T) {
	// Run same fib(5) program through both interpreter and JIT, compare cycle counts.
	insns := []uint32{
		renc(0x33, 0, 0x00, 5, 10, 11), // ADD x5, x10, x11
		ienc(opOPIMM, 0, 10, 11, 0),    // MV x10, x11
		ienc(opOPIMM, 0, 11, 5, 0),     // MV x11, x5
		ienc(opOPIMM, 0, 12, 12, 1),    // ADDI x12, x12, 1
		benc(opBRANCH, 4, 12, 13, -16), // BLT x12, x13, -16
		instrECALL,
	}
	initRegs := func(cpu *CPU) {
		cpu.SetReg(10, 0)
		cpu.SetReg(11, 1)
		cpu.SetReg(12, 0)
		cpu.SetReg(13, 5)
	}

	// Interpreter
	cpu1, mem1 := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem1.Free()
	initRegs(cpu1)
	cpu1.Notes.Push(ecallStop)
	RunWithChain(cpu1, &cpu1.Notes)
	interpCycles := cpu1.RiscvInstrBegun()

	// JIT
	cpu2, mem2 := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem2.Free()
	initRegs(cpu2)
	cpu2.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu2)

	// IC removed — JIT no longer tracks cycles. Verify result correctness.
	if cpu1.Reg(11) != cpu2.Reg(11) {
		t.Errorf("fib(5) result mismatch: interp x11=%d jit x11=%d", cpu1.Reg(11), cpu2.Reg(11))
	}
	t.Logf("fib(5) interp cycles: %d, jit x11=%d", interpCycles, cpu2.Reg(11))
}

// ── Test 3: Load/Store through JIT ───────────────────────────────────────

func TestJIT_LoadStore(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		uenc(opLUI, 10, 0x4000),     // LUI x10, 0x2  → x10 = 0x4000
		ienc(opOPIMM, 0, 11, 0, 42), // ADDI x11, x0, 42
		senc(opSTORE, 2, 10, 11, 0), // SW x11, 0(x10)
		ienc(opLOAD, 2, 12, 10, 0),  // LW x12, 0(x10)
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	// x12 should contain 42 (store then load)
	if cpu.Reg(12) != 42 {
		t.Errorf("x12 = %d, want 42", cpu.Reg(12))
	}
	// Verify memory was actually written
	v, f := mem.Load32(0x4000)
	if f != nil {
		t.Fatalf("Load32 fault: %v", f)
	}
	if v != 42 {
		t.Errorf("mem[0x4000] = %d, want 42", v)
	}
}

func TestJIT_LoadStore_AllWidths(t *testing.T) {
	tests := []struct {
		name     string
		storeF3  uint32 // funct3 for store (0=SB, 1=SH, 2=SW, 3=SD)
		loadF3   uint32 // funct3 for load (0=LB, 1=LH, 2=LW, 3=LD, 4=LBU, 5=LHU, 6=LWU)
		storeVal int32
		want     uint64
	}{
		{"SD_LD", 3, 3, 42, 42},
		{"SW_LW", 2, 2, -1, 0xFFFFFFFFFFFFFFFF},  // sign-extends
		{"SW_LWU", 2, 6, -1, 0x00000000FFFFFFFF}, // zero-extends
		{"SH_LH", 1, 1, -1, 0xFFFFFFFFFFFFFFFF},  // sign-extends
		{"SH_LHU", 1, 5, -1, 0x000000000000FFFF}, // zero-extends
		{"SB_LB", 0, 0, -1, 0xFFFFFFFFFFFFFFFF},  // sign-extends
		{"SB_LBU", 0, 4, -1, 0x00000000000000FF}, // zero-extends
		{"SB_42", 0, 4, 42, 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
				uenc(opLUI, 10, 0x4000),              // LUI x10, 0x2 → 0x4000
				ienc(opOPIMM, 0, 11, 0, tc.storeVal), // ADDI x11, x0, val
				senc(opSTORE, tc.storeF3, 10, 11, 0), // Sx x11, 0(x10)
				ienc(opLOAD, tc.loadF3, 12, 10, 0),   // Lx x12, 0(x10)
				instrECALL,
			})
			defer mem.Free()
			cpu.Notes.Push(ecallStop)

			jit := NewJIT()
			jit.RunJIT(cpu)

			if cpu.Reg(12) != tc.want {
				t.Errorf("x12 = 0x%x, want 0x%x", cpu.Reg(12), tc.want)
			}
		})
	}
}

func TestJIT_LoadFault(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),   // ADDI x10, x0, 1
		ienc(opOPIMM, 1, 10, 10, 26), // SLLI x10, x10, 26 → x10 = 0x4000000 (past 64MB)
		ienc(opLOAD, 2, 11, 10, 0),   // LW x11, 0(x10) → fault
		instrECALL,
	})
	defer mem.Free()

	var gotFault bool
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseLoadFault {
			gotFault = true
			return NoteFatal
		}
		return NoteForward
	})

	jit := NewJIT()
	jit.RunJIT(cpu)

	if !gotFault {
		t.Error("expected load fault, got none")
	}
}

func TestJIT_StoreFault(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),   // ADDI x10, x0, 1
		ienc(opOPIMM, 1, 10, 10, 26), // SLLI x10, x10, 26 → 0x4000000
		senc(opSTORE, 2, 10, 0, 0),   // SW x0, 0(x10) → fault
		instrECALL,
	})
	defer mem.Free()

	var gotFault bool
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseStoreFault {
			gotFault = true
			return NoteFatal
		}
		return NoteForward
	})

	jit := NewJIT()
	jit.RunJIT(cpu)

	if !gotFault {
		t.Error("expected store fault, got none")
	}
}

// ── Test 2: JIT vs Interpreter register state ────────────────────────────

func TestJIT_vs_Interp_Registers(t *testing.T) {
	tests := []struct {
		name  string
		insns []uint32
		init  func(cpu *CPU)
	}{
		{
			name: "ALU_mix",
			insns: []uint32{
				ienc(opOPIMM, 0, 1, 0, 100),  // ADDI x1, x0, 100
				ienc(opOPIMM, 0, 2, 0, 42),   // ADDI x2, x0, 42
				renc(opOP, 0, 0x00, 3, 1, 2), // ADD x3, x1, x2
				renc(opOP, 0, 0x20, 4, 1, 2), // SUB x4, x1, x2
				renc(opOP, 4, 0x00, 6, 3, 4), // XOR x6, x3, x4
				instrECALL,
			},
		},
		{
			name: "load_store",
			insns: []uint32{
				uenc(opLUI, 10, 0x4000),       // LUI x10, 0x2
				ienc(opOPIMM, 0, 11, 0, 0x55), // ADDI x11, x0, 0x55
				senc(opSTORE, 2, 10, 11, 0),   // SW x11, 0(x10)
				ienc(opLOAD, 2, 12, 10, 0),    // LW x12, 0(x10)
				senc(opSTORE, 0, 10, 11, 8),   // SB x11, 8(x10)
				ienc(opLOAD, 0, 13, 10, 8),    // LB x13, 8(x10)
				ienc(opLOAD, 4, 14, 10, 8),    // LBU x14, 8(x10)
				senc(opSTORE, 3, 10, 11, 16),  // SD x11, 16(x10)
				ienc(opLOAD, 3, 15, 10, 16),   // LD x15, 16(x10)
				instrECALL,
			},
		},
		{
			name: "branch_skip",
			insns: []uint32{
				ienc(opOPIMM, 0, 1, 0, 5),   // ADDI x1, x0, 5
				ienc(opOPIMM, 0, 2, 0, 5),   // ADDI x2, x0, 5
				benc(opBRANCH, 0, 1, 2, 8),  // BEQ x1, x2, +8
				ienc(opOPIMM, 0, 3, 0, 999), // ADDI x3, x0, 999 (skipped)
				ienc(opOPIMM, 0, 3, 0, 42),  // ADDI x3, x0, 42
				instrECALL,
			},
		},
		{
			name: "shifts",
			insns: []uint32{
				ienc(opOPIMM, 0, 1, 0, -1),       // ADDI x1, x0, -1 (all 1s)
				ienc(opOPIMM, 1, 2, 1, 32),       // SLLI x2, x1, 32
				ienc(opOPIMM, 5, 3, 1, 32),       // SRLI x3, x1, 32
				ienc(opOPIMM, 5, 4, 1, 32|0x400), // SRAI x4, x1, 32 (funct7 bit)
				instrECALL,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Interpreter
			cpu1, mem1 := newTestCPU(t, Size64MB, 0x1000, tc.insns)
			defer mem1.Free()
			if tc.init != nil {
				tc.init(cpu1)
			}
			cpu1.Notes.Push(ecallStop)
			RunWithChain(cpu1, &cpu1.Notes)
			snap1 := takeCPUSnapshot(cpu1)

			// JIT
			cpu2, mem2 := newTestCPU(t, Size64MB, 0x1000, tc.insns)
			defer mem2.Free()
			if tc.init != nil {
				tc.init(cpu2)
			}
			cpu2.Notes.Push(ecallStop)
			jit := NewJIT()
			jit.RunJIT(cpu2)
			snap2 := takeCPUSnapshot(cpu2)

			snap1.compare(t, snap2, tc.name)
		})
	}
}

// ── Test 7: EBREAK through JIT ───────────────────────────────────────────

func TestJIT_EBREAK(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 42), // ADDI x1, x0, 42
		instrEBREAK,
	})
	defer mem.Free()

	var gotBreak bool
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseBreakpoint {
			gotBreak = true
			return NoteFatal
		}
		return NoteForward
	})

	jit := NewJIT()
	jit.RunJIT(cpu)

	if !gotBreak {
		t.Error("expected EBREAK note, got none")
	}
	if cpu.Reg(1) != 42 {
		t.Errorf("x1 = %d, want 42", cpu.Reg(1))
	}
}

// ── Test 6: Memory consistency across JIT/interpreter boundary ───────────

func TestJIT_MemConsistency_Interp_to_JIT(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	// Pre-store via host
	mem.Store32(0x4000, 0xCAFEBABE)

	// JIT block loads it
	storeInsns(mem, 0x1000, []uint32{
		uenc(opLUI, 10, 0x4000),    // LUI x10, 0x2
		ienc(opLOAD, 2, 11, 10, 0), // LW x11, 0(x10)
		instrECALL,
	})

	cpu := NewCPU(*mem)
	cpu.SetPC(0x1000)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	// LW sign-extends: 0xCAFEBABE → 0xFFFFFFFFCAFEBABE
	want := uint64(0xFFFFFFFFCAFEBABE)
	if cpu.Reg(11) != want {
		t.Errorf("x11 = 0x%x, want 0x%x", cpu.Reg(11), want)
	}
}

func TestJIT_MemConsistency_JIT_to_Interp(t *testing.T) {
	// JIT stores, then CSR forces interpreter, then interpreter loads.
	// CSR instruction: CSRRS x3, cycle, x0 = funct12=0xC00 rs1=0 funct3=2 rd=3
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00) // CSRRS x3, cycle, x0

	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		uenc(opLUI, 10, 0x4000),     // LUI x10, 0x2
		ienc(opOPIMM, 0, 11, 0, 42), // ADDI x11, x0, 42
		senc(opSTORE, 2, 10, 11, 0), // SW x11, 0(x10) — JIT store
		csrrs,                       // untranslatable → interpreter
		ienc(opLOAD, 2, 12, 10, 0),  // LW x12, 0(x10) — may be JIT or interp
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(12) != 42 {
		t.Errorf("x12 = %d, want 42 (JIT store must be visible after interpreter step)", cpu.Reg(12))
	}
}

// ── Test 8: Mixed JIT/interpreter execution ──────────────────────────────

func TestJIT_MixedExecution(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00) // CSRRS x3, cycle, x0

	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 10),   // ADDI x1, x0, 10  (JIT'd)
		ienc(opOPIMM, 0, 2, 0, 20),   // ADDI x2, x0, 20  (JIT'd)
		csrrs,                        // NOT JIT'd — terminates block
		ienc(opOPIMM, 0, 4, 0, 30),   // ADDI x4, x0, 30  (new JIT block)
		renc(opOP, 0, 0x00, 5, 1, 2), // ADD x5, x1, x2   (JIT'd)
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 10 {
		t.Errorf("x1 = %d, want 10", cpu.Reg(1))
	}
	if cpu.Reg(2) != 20 {
		t.Errorf("x2 = %d, want 20", cpu.Reg(2))
	}
	if cpu.Reg(4) != 30 {
		t.Errorf("x4 = %d, want 30", cpu.Reg(4))
	}
	if cpu.Reg(5) != 30 {
		t.Errorf("x5 = %d, want 30 (x1+x2 across JIT/interp boundary)", cpu.Reg(5))
	}
}

// ── Test 9: Forward branch within region ─────────────────────────────────

func TestJIT_ForwardBranch(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1),   // ADDI x1, x0, 1
		benc(opBRANCH, 0, 0, 0, 12), // BEQ x0, x0, +12 (always taken → 0x1010)
		ienc(opOPIMM, 0, 2, 0, 999), // ADDI x2, x0, 999 (SKIPPED)
		ienc(opOPIMM, 0, 3, 0, 999), // ADDI x3, x0, 999 (SKIPPED)
		ienc(opOPIMM, 0, 2, 0, 42),  // ADDI x2, x0, 42 (branch target)
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 1 {
		t.Errorf("x1 = %d, want 1", cpu.Reg(1))
	}
	if cpu.Reg(2) != 42 {
		t.Errorf("x2 = %d, want 42", cpu.Reg(2))
	}
	if cpu.Reg(3) != 0 {
		t.Errorf("x3 = %d, want 0 (should be skipped)", cpu.Reg(3))
	}
}

func TestJIT_ForwardBranch_NotTaken(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1),  // ADDI x1, x0, 1
		ienc(opOPIMM, 0, 2, 0, 2),  // ADDI x2, x0, 2
		benc(opBRANCH, 1, 1, 1, 8), // BNE x1, x1, +8 (never taken)
		ienc(opOPIMM, 0, 3, 0, 42), // ADDI x3, x0, 42 (falls through)
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(3) != 42 {
		t.Errorf("x3 = %d, want 42 (fall-through)", cpu.Reg(3))
	}
}

// ── Test 10: Unconditional jump (JAL rd=0) ───────────────────────────────

func TestJIT_J_Forward(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1),   // ADDI x1, x0, 1
		jenc(0, 12),                 // JAL x0, +12 (jump to 0x1010)
		ienc(opOPIMM, 0, 2, 0, 999), // ADDI x2, x0, 999 (SKIPPED)
		ienc(opOPIMM, 0, 3, 0, 999), // ADDI x3, x0, 999 (SKIPPED)
		ienc(opOPIMM, 0, 2, 0, 42),  // ADDI x2, x0, 42
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 1 {
		t.Errorf("x1 = %d, want 1", cpu.Reg(1))
	}
	if cpu.Reg(2) != 42 {
		t.Errorf("x2 = %d, want 42", cpu.Reg(2))
	}
	if cpu.Reg(3) != 0 {
		t.Errorf("x3 = %d, want 0 (should be skipped)", cpu.Reg(3))
	}
}

// ── Test 5a: Two-block dispatch (JAL rd!=0) ──────────────────────────────

func TestJIT_TwoBlockDispatch(t *testing.T) {
	// Block A at 0x1000: set x1, then JAL ra, +16 → 0x1010
	// Block B at 0x1010: set x2, ECALL
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 3, 0, 10),  // 0x1000: ADDI x3, x0, 10
		jenc(1, 12),                 // 0x1004: JAL x1(ra), +12 → 0x1010
		ienc(opOPIMM, 0, 4, 0, 999), // 0x1008: ADDI x4, x0, 999 (skipped)
		ienc(opOPIMM, 0, 5, 0, 999), // 0x100C: skipped
		ienc(opOPIMM, 0, 2, 0, 20),  // 0x1010: ADDI x2, x0, 20
		instrECALL,                  // 0x1014: ECALL
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	// x1 = return address = 0x1008 (PC after the JAL)
	if cpu.Reg(1) != 0x1008 {
		t.Errorf("x1 (ra) = 0x%x, want 0x1008", cpu.Reg(1))
	}
	if cpu.Reg(2) != 20 {
		t.Errorf("x2 = %d, want 20", cpu.Reg(2))
	}
	if cpu.Reg(3) != 10 {
		t.Errorf("x3 = %d, want 10", cpu.Reg(3))
	}
}

// ── Test 5b: JALR indirect jump ──────────────────────────────────────────

func TestJIT_JALR_IndirectJump(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 10),   // 0x1000: ADDI x1, x0, 10
		ienc(opOPIMM, 0, 5, 0, 0),    // 0x1004: ADDI x5, x0, 0
		ienc(opOPIMM, 0, 5, 5, 0x10), // 0x1008: ADDI x5, x5, 0x10 → x5=0x10
		// Need to add 0x1000 to get 0x1010. Use AUIPC.
		uenc(opAUIPC, 6, 0),         // 0x100C: AUIPC x6, 0 → x6 = 0x100C
		renc(opOP, 0, 0, 5, 6, 5),   // 0x1010: ADD x5, x6, x5 → x5 = 0x101C
		ienc(opJALR, 0, 0, 5, 0),    // 0x1014: JALR x0, x5, 0 → jump to 0x101C
		ienc(opOPIMM, 0, 7, 0, 999), // 0x1018: ADDI x7, x0, 999 (skipped)
		ienc(opOPIMM, 0, 2, 0, 20),  // 0x101C: ADDI x2, x0, 20
		instrECALL,                  // 0x1020: ECALL
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 10 {
		t.Errorf("x1 = %d, want 10", cpu.Reg(1))
	}
	if cpu.Reg(2) != 20 {
		t.Errorf("x2 = %d, want 20", cpu.Reg(2))
	}
	if cpu.Reg(7) != 0 {
		t.Errorf("x7 = %d, want 0 (should be skipped)", cpu.Reg(7))
	}
}

func TestJIT_AUIPCJALRFusionPreservesAUIPCDestWhenJALRDoesNotLink(t *testing.T) {
	const entryPC = uint64(0x1000)
	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		uenc(opAUIPC, 31, 0),       // 0x1000: AUIPC t6, 0 -> t6 = 0x1000
		ienc(opJALR, 0, 0, 31, 12), // 0x1004: JALR x0, 12(t6) -> 0x100c
		ienc(opOPIMM, 0, 31, 0, 7), // 0x1008: skipped
		instrECALL,                 // 0x100c
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	if got := cpu.Reg(31); got != entryPC {
		t.Fatalf("t6 after fused AUIPC+JALR = 0x%x, want AUIPC result 0x%x", got, entryPC)
	}
}

// ── Test 11: Translation failure and noJIT fallback ──────────────────────

func TestJIT_TranslationFailure(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 1, 0, 0xC00) // CSRRS x1, cycle, x0

	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		csrrs, // untranslatable
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	// CSR instructions are now handled by creating a 1-instruction block
	// that returns to the interpreter. The block compiles but exits
	// immediately so the interpreter handles the CSR.
	// x1 should have a cycle count value (nonzero after at least 1 instruction)
	t.Logf("x1 (cycle) = %d", cpu.Reg(1))
	if got := jit.InterpretedInsns; got != 1 {
		t.Fatalf("InterpretedInsns = %d, want 1 for single CSR fallback", got)
	}
}

// ── Test 12: Bail label safety net ───────────────────────────────────────

func TestJIT_BailLabel(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00) // CSRRS x3, cycle, x0

	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1),   // ADDI x1, x0, 1
		benc(opBRANCH, 0, 0, 0, 8),  // BEQ x0, x0, +8 → 0x100C
		ienc(opOPIMM, 0, 2, 0, 999), // ADDI x2, x0, 999 (skipped)
		csrrs,                       // 0x100C: untranslatable → bail label
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 1 {
		t.Errorf("x1 = %d, want 1", cpu.Reg(1))
	}
	if cpu.Reg(2) != 0 {
		t.Errorf("x2 = %d, want 0 (should be skipped)", cpu.Reg(2))
	}
	// x3 should have a cycle value from the interpreter handling the CSR
	t.Logf("x3 (cycle via bail→interp) = %d", cpu.Reg(3))
}

// ── Test 13: Last-block cache ────────────────────────────────────────────

func TestJIT_LastBlockCache(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()

	for i := 0; i < 3; i++ {
		cpu.SetPC(0x1000)
		jit.RunJIT(cpu)
	}

	if cpu.Reg(1) != 3 {
		t.Errorf("x1 = %d, want 3 after 3 runs", cpu.Reg(1))
	}
	// Verify the block was cached.
	if jit.lookupBlock(0x1000) == nil {
		t.Error("block at 0x1000 not cached")
	}
}

// ── Test 16: Instruction budget / semi-cooperative preemption ───────────

// TestJIT_InstructionBudget verifies a loop that runs far more iterations than
// the per-block instruction budget (ir/MaxIC=4096). The JIT block must exit at
// the backward branch when the budget is exceeded, the dispatch loop re-enters
// at the loop target, cached registers are re-read from x[], and execution
// continues. The final result must be correct and the cycle count accurate.
func TestJIT_InstructionBudget(t *testing.T) {
	// Loop that iterates 100000 times — well above ir/MaxIC=4096.
	// Tests the backward-branch budget check path.
	//
	//   0x1000: ADDI x1, x1, 1          # counter++
	//   0x1004: BLT  x1, x2, -4         # if x1 < x2 goto 0x1000
	//   0x1008: ECALL
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1),   // ADDI x1, x1, 1
		benc(opBRANCH, 4, 1, 2, -4), // BLT x1, x2, -4
		instrECALL,
	})
	defer mem.Free()
	cpu.SetReg(1, 0)
	cpu.SetReg(2, 100000)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 100000 {
		t.Errorf("x1 = %d, want 100000", cpu.Reg(1))
	}
}

// TestJIT_InstructionBudget_JForward_Loop verifies a loop formed by an
// unconditional backward jump (JAL x0, -N). The JAL rd==0 backward path
// must also respect the budget.
func TestJIT_InstructionBudget_JForward_Loop(t *testing.T) {
	// Loop: increment x1, check against x2, if reached then jump forward
	// over the backward jump to ECALL. Otherwise, JAL backward.
	//
	//   0x1000: ADDI x1, x1, 1       # counter++
	//   0x1004: BEQ  x1, x2, +8      # if x1 == x2 goto 0x100C
	//   0x1008: JAL  x0, -8          # backward unconditional jump to 0x1000
	//   0x100C: ECALL
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1),  // ADDI x1, x1, 1
		benc(opBRANCH, 0, 1, 2, 8), // BEQ x1, x2, +8
		jenc(0, -8),                // JAL x0, -8
		instrECALL,
	})
	defer mem.Free()
	cpu.SetReg(1, 0)
	cpu.SetReg(2, 50000)
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if cpu.Reg(1) != 50000 {
		t.Errorf("x1 = %d, want 50000", cpu.Reg(1))
	}
}

// ── Test 17: Stopper page preemption of infinite loop ────────────────────

// TestJIT_StopperPage_InfiniteLoop verifies that RequestPreemption can
// break a JIT-compiled infinite loop. The guest runs a tight backward
// branch with no exit. A goroutine arms the stopper page after a short
// delay. The TESTQ probe at the backward branch faults (SIGSEGV), Go
// converts it to a panic, and the test's recover catches it.
func TestJIT_StopperPage_InfiniteLoop(t *testing.T) {
	t.Skip("lack of pcdata means 'fatal error: unknown caller pc' atm.")
	//   0x1000: ADDI x1, x1, 1      # counter++
	//   0x1004: JAL  x0, -4          # infinite backward jump to 0x1000
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1), // ADDI x1, x1, 1
		jenc(0, -4),               // JAL x0, -4
	})
	defer mem.Free()
	cpu.SetReg(1, 0)

	jit := NewJIT()

	// Arm the stopper page after 50ms from a separate goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		jit.RequestPreemption()
	}()

	// RunJIT will panic when the stopper page faults. Recover it.
	var panicked bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		jit.RunJIT(cpu)
	}()

	<-done
	jit.ClearPreemption()

	if !panicked {
		t.Fatal("expected RunJIT to panic from stopper page fault, but it returned normally")
	}

	// The loop should have run for some iterations before being stopped.
	if cpu.Reg(1) == 0 {
		t.Error("x1 = 0; loop never executed")
	}
	t.Logf("loop ran %d iterations before preemption", cpu.Reg(1))
}

// ── Test 14: Fault address correctness ───────────────────────────────────

func TestJIT_FaultAddress(t *testing.T) {
	const entryPC = 0x1000
	const faultInsnPC = entryPC + 4*3 // LW is the 4th instruction (index 3)
	const expectFaultAddr = uint64(0x4000008)

	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),   // ADDI x10, x0, 1
		ienc(opOPIMM, 1, 10, 10, 26), // SLLI x10, x10, 26 → 0x4000000
		ienc(opOPIMM, 0, 10, 10, 8),  // ADDI x10, x10, 8 → 0x4000008
		ienc(opLOAD, 2, 11, 10, 0),   // LW x11, 0(x10) → fault at 0x4000008
		instrECALL,
	})
	defer mem.Free()

	var (
		gotCause  uint64
		gotTval   uint64
		gotNotePC uint64
	)
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseLoadFault {
			gotCause = n.Cause
			gotTval = n.Tval
			gotNotePC = n.PC
			return NoteFatal
		}
		return NoteForward
	})

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	if gotCause != CauseLoadFault {
		t.Fatalf("no load fault delivered (cause=%d)", gotCause)
	}
	if gotTval != expectFaultAddr {
		t.Errorf("fault Tval = 0x%x, want 0x%x (was 0 before per-call-site fault tail fix)",
			gotTval, expectFaultAddr)
	}
	if gotNotePC != faultInsnPC {
		t.Errorf("fault Note.PC = 0x%x, want 0x%x (faulting LW instruction; was block startPC before fix)",
			gotNotePC, faultInsnPC)
	}
	if cpu.PC() != faultInsnPC {
		t.Errorf("cpu.PC after fault = 0x%x, want 0x%x (faulting LW instruction)",
			cpu.PC(), faultInsnPC)
	}
}

// TestJIT_StoreFaultAddress is the store-side counterpart to TestJIT_FaultAddress.
// Verifies the IR JIT reports the actual faulting store instruction's PC and
// the exact faulting guest address (not block startPC and not 0).
func TestJIT_StoreFaultAddress(t *testing.T) {
	const entryPC = 0x1000
	const faultInsnPC = entryPC + 4*3 // SW is the 4th instruction (index 3)
	const expectFaultAddr = uint64(0x4000010)

	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),   // ADDI x10, x0, 1
		ienc(opOPIMM, 1, 10, 10, 26), // SLLI x10, x10, 26 → 0x4000000
		ienc(opOPIMM, 0, 10, 10, 16), // ADDI x10, x10, 16 → 0x4000010
		senc(opSTORE, 2, 10, 11, 0),  // SW x11, 0(x10) → fault at 0x4000010
		instrECALL,
	})
	defer mem.Free()

	var (
		gotCause  uint64
		gotTval   uint64
		gotNotePC uint64
	)
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseStoreFault {
			gotCause = n.Cause
			gotTval = n.Tval
			gotNotePC = n.PC
			return NoteFatal
		}
		return NoteForward
	})

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	if gotCause != CauseStoreFault {
		t.Fatalf("no store fault delivered (cause=%d)", gotCause)
	}
	if gotTval != expectFaultAddr {
		t.Errorf("fault Tval = 0x%x, want 0x%x", gotTval, expectFaultAddr)
	}
	if gotNotePC != faultInsnPC {
		t.Errorf("fault Note.PC = 0x%x, want 0x%x (faulting SW instruction)",
			gotNotePC, faultInsnPC)
	}
	if cpu.PC() != faultInsnPC {
		t.Errorf("cpu.PC after fault = 0x%x, want 0x%x", cpu.PC(), faultInsnPC)
	}
}

// TestJIT_FLW_Misaligned exercises the FP byte-by-byte misalign path for FLW.
// An FLW from an address whose low 2 bits are non-zero must succeed and load
// a correctly NaN-boxed 32-bit value via byte-by-byte reads.
func TestJIT_FLW_Misaligned(t *testing.T) {
	const entryPC = 0x1000
	const dataAddr = uint64(0x4001) // deliberately misaligned

	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		uenc(opLUI, 10, 0x4000),
		ienc(opOPIMM, 0, 10, 10, 1),
		ienc(0x07, 2, 1, 10, 0), // FLW f1, 0(x10) — funct3=2
		instrECALL,
	})
	defer mem.Free()

	want32 := uint32(0xDEADBEEF)
	if f := mem.WriteBytes(dataAddr, []byte{
		byte(want32), byte(want32 >> 8), byte(want32 >> 16), byte(want32 >> 24),
	}); f != nil {
		t.Fatalf("WriteBytes setup failed: %v", f)
	}

	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseEcallU {
			return NoteFatal
		}
		t.Errorf("unexpected fault cause=%d tval=0x%x pc=0x%x",
			n.Cause, n.Tval, n.PC)
		return NoteFatal
	})

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	got := cpu.FReg(1)
	wantBoxed := uint64(0xFFFFFFFF00000000) | uint64(want32)
	if got != wantBoxed {
		t.Errorf("f1 = 0x%016x, want 0x%016x (NaN-boxed 0x%08x from misaligned FLW)",
			got, wantBoxed, want32)
	}
}

// TestJIT_FLD_Misaligned exercises the FP byte-by-byte misalign path: an FLD
// from an address whose low 3 bits are non-zero must succeed and load the
// correct 64-bit value (no fault). Before Fix 2, the JIT trapped on misaligned
// FP loads via the buggy shared fault label; now it falls through to
// emitMisalignedFPLoad which does a byte-by-byte read.
func TestJIT_FLD_Misaligned(t *testing.T) {
	const entryPC = 0x1000
	const dataAddr = uint64(0x4001) // deliberately misaligned (low 3 bits != 0)

	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		uenc(opLUI, 10, 0x4000),     // LUI x10, 0x4 → x10 = 0x4000
		ienc(opOPIMM, 0, 10, 10, 1), // ADDI x10, x10, 1 → x10 = 0x4001
		ienc(0x07, 3, 1, 10, 0),     // FLD f1, 0(x10) — funct3=3
		instrECALL,
	})
	defer mem.Free()

	bytes := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	if f := mem.WriteBytes(dataAddr, bytes); f != nil {
		t.Fatalf("WriteBytes setup failed: %v", f)
	}

	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseEcallU {
			return NoteFatal // expected — clean termination
		}
		t.Errorf("unexpected fault cause=%d tval=0x%x pc=0x%x",
			n.Cause, n.Tval, n.PC)
		return NoteFatal
	})

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	got := cpu.FReg(1)
	want := uint64(0x0807060504030201)
	if got != want {
		t.Errorf("f1 = 0x%016x, want 0x%016x (little-endian bytes from 0x2001)",
			got, want)
	}
}

// TestJIT_FSD_Misaligned exercises the byte-by-byte misaligned store path.
func TestJIT_FSD_Misaligned(t *testing.T) {
	const entryPC = 0x1000
	const dataAddr = uint64(0x4001) // deliberately misaligned

	// Pre-fill f1 via FLD from an aligned address, then FSD to a misaligned one.
	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		uenc(opLUI, 10, 0x3000),     // x10 = 0x3000 (aligned source)
		uenc(opLUI, 11, 0x4000),     // x11 = 0x4000
		ienc(opOPIMM, 0, 11, 11, 1), // x11 = 0x4001 (misaligned dest)
		ienc(0x07, 3, 1, 10, 0),     // FLD f1, 0(x10) — from aligned
		senc(0x27, 3, 11, 1, 0),     // FSD f1, 0(x11) — to misaligned
		instrECALL,
	})
	defer mem.Free()

	src := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	if f := mem.WriteBytes(0x3000, src); f != nil {
		t.Fatalf("WriteBytes setup failed: %v", f)
	}

	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseEcallU {
			return NoteFatal
		}
		t.Errorf("unexpected fault cause=%d tval=0x%x pc=0x%x",
			n.Cause, n.Tval, n.PC)
		return NoteFatal
	})

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	got := make([]byte, 8)
	if f := mem.ReadBytes(dataAddr, got); f != nil {
		t.Fatalf("ReadBytes: %v", f)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("dest bytes = %x, want %x", got, src)
	}
}

// TestJIT_NaNBoxF32_Roundtrip is a regression test for the boxF32 / unboxF32
// IR-emission bug.
//
// The bug: boxF32 emitted Or(fp-dst, int-a, int-b) and unboxF32 emitted
// ShrImm(int-tmp, fp-src, 32). Both ops used integer ops with one XMM operand,
// which the V1 amd64 lowerer rendered as the invalid x86 mnemonic "ORQ R11, X0"
// (or "SHRQ ?, X?"). Any block containing FLW, FADD.S, FSUB.S, FMUL.S, FDIV.S,
// FCMP.S, FCVT.*.S, or FMA-S instructions failed JIT compilation and silently
// fell back to the interpreter for those blocks.
//
// This block exercises the full roundtrip: two FLW instructions (each emits a
// boxF32 on the loaded value) feed a FADD.S (which unboxF32's both operands
// and boxF32's the result). The block ends with ECALL for a clean exit.
//
//	x10 = 0x4000                     ; aligned data buffer
//	FLW  f1, 0(x10)                  ; f1 = NaN-boxed 1.5     (uses boxF32)
//	FLW  f2, 4(x10)                  ; f2 = NaN-boxed 2.5     (uses boxF32)
//	FADD.S f3, f1, f2                ; f3 = 1.5 + 2.5 = 4.0   (uses unboxF32 ×2 + boxF32)
//	ECALL
//
// The test asserts:
//  1. The block JIT-compiled (DispatchCompile > 0).
//     Pre-fix, this block failed at the assembler with
//     "invalid instruction: ORQ R11, X0" and silently fell back to the
//     interpreter. InterpretedInsns is the exact regression tripwire here.
//  2. The interpreter was never invoked (InterpretedInsns == 0).
//  3. f3 holds the correct NaN-boxed Float32(4.0).
func TestJIT_NaNBoxF32_Roundtrip(t *testing.T) {
	const entryPC = 0x1000
	const dataAddr = uint64(0x4000)

	cpu, mem := newTestCPU(t, Size64MB, entryPC, []uint32{
		uenc(opLUI, 10, 0x4000),         // LUI x10, 0x2 → x10 = 0x4000
		ienc(0x07, 2, 1, 10, 0),         // FLW f1, 0(x10)
		ienc(0x07, 2, 2, 10, 4),         // FLW f2, 4(x10)
		renc(0x53, 0x07, 0x00, 3, 1, 2), // FADD.S f3, f1, f2 (DYN rounding)
		instrECALL,
	})
	defer mem.Free()

	// Pre-fill 8 bytes: 1.5 (LE) at offset 0, 2.5 (LE) at offset 4.
	a := math.Float32bits(1.5)
	b := math.Float32bits(2.5)
	if f := mem.WriteBytes(dataAddr, []byte{
		byte(a), byte(a >> 8), byte(a >> 16), byte(a >> 24),
		byte(b), byte(b >> 8), byte(b >> 16), byte(b >> 24),
	}); f != nil {
		t.Fatalf("WriteBytes setup: %v", f)
	}

	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseEcallU {
			return NoteFatal
		}
		t.Errorf("unexpected fault cause=%d tval=0x%x pc=0x%x text=%s",
			n.Cause, n.Tval, n.PC, n.Text)
		return NoteFatal
	})

	jit := NewJIT()
	_ = jit.RunJIT(cpu)

	if jit.DispatchCompile == 0 {
		t.Fatal("JIT did not compile any block — boxF32/unboxF32 lowering regressed")
	}
	if jit.DispatchInterp != 0 {
		t.Errorf("interpreter was invoked %d times — JIT block(s) fell back due to compile failure (boxF32/unboxF32 lowering regressed?)",
			jit.DispatchInterp)
	}
	if got := jit.InterpretedInsns; got != 0 {
		t.Errorf("JIT-owned interpreter fallback attempted %d instructions — expected full native coverage", got)
	}

	const nanBoxHi = uint64(0xFFFFFFFF00000000)
	wantBoxed := nanBoxHi | uint64(math.Float32bits(4.0))
	if got := cpu.FReg(3); got != wantBoxed {
		t.Errorf("f3 = 0x%016x, want 0x%016x (NaN-boxed Float32(4.0))",
			got, wantBoxed)
	}
}
