package riscv

import "testing"

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

	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*ExitError); ok {
				exitCode = ex.Code
				err = nil
				return
			}
			panic(r)
		}
	}()

	jit := NewJIT()
	err = jit.RunJIT(cpu)
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

// ── Lockstep helpers (used by riscv_test.go too) ─────────────────────────

// compareFullMemory reads both sandboxes and compares byte-for-byte.
func compareFullMemory(t *testing.T, a, b *GuestMemory, blockNum int) {
	t.Helper()
	size := a.Size()
	bufA := make([]byte, size)
	bufB := make([]byte, size)
	a.ReadBytes(0, bufA)
	b.ReadBytes(0, bufB)

	diffs := 0
	for i := range bufA {
		if bufA[i] != bufB[i] {
			if diffs < 5 {
				t.Errorf("block %d: memory[0x%04x] mismatch: jit=0x%02x interp=0x%02x",
					blockNum, i, bufA[i], bufB[i])
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

	res := emitBlock(mem, codeVA)
	if res == nil {
		t.Fatal("emitBlock returned nil")
	}
	t.Logf("Generated C (%d insns):\n%s", res.numInsns, res.csrc)

	blk, err := tccCompile(res.csrc)
	if err != nil {
		t.Fatalf("tccCompile: %v", err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.SetReg(2, 100)
	cpu.SetReg(3, 42)

	jit := NewJIT()
	jit.blocks[codeVA] = blk
	cpu.Notes.Push(ecallStop)
	jit.RunJIT(cpu)

	got := cpu.Reg(1)
	want := uint64(142)
	if got != want {
		t.Errorf("x1 = %d, want %d", got, want)
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
		renc(0x33, 0, 0x00, 5, 10, 11),              // ADD x5, x10, x11
		ienc(opOPIMM, 0, 10, 11, 0),                  // MV x10, x11
		ienc(opOPIMM, 0, 11, 5, 0),                   // MV x11, x5
		ienc(opOPIMM, 0, 12, 12, 1),                  // ADDI x12, x12, 1
		benc(opBRANCH, 4, 12, 13, -16),               // BLT x12, x13, -16
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
	t.Logf("fib(20) = %d, insns = %d", got, cpu.Cycle())
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

	// 5 ADDIs + 1 ECALL = 6
	if cpu.Cycle() != 6 {
		t.Errorf("cycle count = %d, want 6", cpu.Cycle())
	}
}

func TestJIT_CycleCount_Loop(t *testing.T) {
	// Run same fib(5) program through both interpreter and JIT, compare cycle counts.
	insns := []uint32{
		renc(0x33, 0, 0x00, 5, 10, 11),  // ADD x5, x10, x11
		ienc(opOPIMM, 0, 10, 11, 0),     // MV x10, x11
		ienc(opOPIMM, 0, 11, 5, 0),      // MV x11, x5
		ienc(opOPIMM, 0, 12, 12, 1),     // ADDI x12, x12, 1
		benc(opBRANCH, 4, 12, 13, -16),  // BLT x12, x13, -16
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
	interpCycles := cpu1.Cycle()

	// JIT
	cpu2, mem2 := newTestCPU(t, Size64MB, 0x1000, insns)
	defer mem2.Free()
	initRegs(cpu2)
	cpu2.Notes.Push(ecallStop)
	jit := NewJIT()
	jit.RunJIT(cpu2)
	jitCycles := cpu2.Cycle()

	if interpCycles != jitCycles {
		t.Errorf("cycle mismatch: interp=%d jit=%d", interpCycles, jitCycles)
	}
	t.Logf("fib(5) cycles: %d", jitCycles)
}

// ── Test 3: Load/Store through JIT ───────────────────────────────────────

func TestJIT_LoadStore(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		uenc(opLUI, 10, 0x2000),           // LUI x10, 0x2  → x10 = 0x2000
		ienc(opOPIMM, 0, 11, 0, 42),       // ADDI x11, x0, 42
		senc(opSTORE, 2, 10, 11, 0),       // SW x11, 0(x10)
		ienc(opLOAD, 2, 12, 10, 0),        // LW x12, 0(x10)
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
	v, f := mem.Load32(0x2000)
	if f != nil {
		t.Fatalf("Load32 fault: %v", f)
	}
	if v != 42 {
		t.Errorf("mem[0x2000] = %d, want 42", v)
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
		{"SW_LW", 2, 2, -1, 0xFFFFFFFFFFFFFFFF},   // sign-extends
		{"SW_LWU", 2, 6, -1, 0x00000000FFFFFFFF},   // zero-extends
		{"SH_LH", 1, 1, -1, 0xFFFFFFFFFFFFFFFF},    // sign-extends
		{"SH_LHU", 1, 5, -1, 0x000000000000FFFF},   // zero-extends
		{"SB_LB", 0, 0, -1, 0xFFFFFFFFFFFFFFFF},    // sign-extends
		{"SB_LBU", 0, 4, -1, 0x00000000000000FF},   // zero-extends
		{"SB_42", 0, 4, 42, 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
				uenc(opLUI, 10, 0x2000),                     // LUI x10, 0x2 → 0x2000
				ienc(opOPIMM, 0, 11, 0, tc.storeVal),        // ADDI x11, x0, val
				senc(opSTORE, tc.storeF3, 10, 11, 0),        // Sx x11, 0(x10)
				ienc(opLOAD, tc.loadF3, 12, 10, 0),          // Lx x12, 0(x10)
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
				ienc(opOPIMM, 0, 1, 0, 100),        // ADDI x1, x0, 100
				ienc(opOPIMM, 0, 2, 0, 42),          // ADDI x2, x0, 42
				renc(opOP, 0, 0x00, 3, 1, 2),        // ADD x3, x1, x2
				renc(opOP, 0, 0x20, 4, 1, 2),        // SUB x4, x1, x2
				renc(opOP, 4, 0x00, 6, 3, 4),        // XOR x6, x3, x4
				instrECALL,
			},
		},
		{
			name: "load_store",
			insns: []uint32{
				uenc(opLUI, 10, 0x2000),             // LUI x10, 0x2
				ienc(opOPIMM, 0, 11, 0, 0x55),       // ADDI x11, x0, 0x55
				senc(opSTORE, 2, 10, 11, 0),         // SW x11, 0(x10)
				ienc(opLOAD, 2, 12, 10, 0),          // LW x12, 0(x10)
				senc(opSTORE, 0, 10, 11, 8),         // SB x11, 8(x10)
				ienc(opLOAD, 0, 13, 10, 8),          // LB x13, 8(x10)
				ienc(opLOAD, 4, 14, 10, 8),          // LBU x14, 8(x10)
				senc(opSTORE, 3, 10, 11, 16),        // SD x11, 16(x10)
				ienc(opLOAD, 3, 15, 10, 16),         // LD x15, 16(x10)
				instrECALL,
			},
		},
		{
			name: "branch_skip",
			insns: []uint32{
				ienc(opOPIMM, 0, 1, 0, 5),           // ADDI x1, x0, 5
				ienc(opOPIMM, 0, 2, 0, 5),           // ADDI x2, x0, 5
				benc(opBRANCH, 0, 1, 2, 8),          // BEQ x1, x2, +8
				ienc(opOPIMM, 0, 3, 0, 999),         // ADDI x3, x0, 999 (skipped)
				ienc(opOPIMM, 0, 3, 0, 42),          // ADDI x3, x0, 42
				instrECALL,
			},
		},
		{
			name: "shifts",
			insns: []uint32{
				ienc(opOPIMM, 0, 1, 0, -1),          // ADDI x1, x0, -1 (all 1s)
				ienc(opOPIMM, 1, 2, 1, 32),          // SLLI x2, x1, 32
				ienc(opOPIMM, 5, 3, 1, 32),          // SRLI x3, x1, 32
				ienc(opOPIMM, 5, 4, 1, 32|0x400),    // SRAI x4, x1, 32 (funct7 bit)
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
	mem.Store32(0x2000, 0xCAFEBABE)

	// JIT block loads it
	storeInsns(mem, 0x1000, []uint32{
		uenc(opLUI, 10, 0x2000),       // LUI x10, 0x2
		ienc(opLOAD, 2, 11, 10, 0),   // LW x11, 0(x10)
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
		uenc(opLUI, 10, 0x2000),           // LUI x10, 0x2
		ienc(opOPIMM, 0, 11, 0, 42),      // ADDI x11, x0, 42
		senc(opSTORE, 2, 10, 11, 0),      // SW x11, 0(x10) — JIT store
		csrrs,                              // untranslatable → interpreter
		ienc(opLOAD, 2, 12, 10, 0),       // LW x12, 0(x10) — may be JIT or interp
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
		ienc(opOPIMM, 0, 1, 0, 10),       // ADDI x1, x0, 10  (JIT'd)
		ienc(opOPIMM, 0, 2, 0, 20),       // ADDI x2, x0, 20  (JIT'd)
		csrrs,                              // NOT JIT'd — terminates block
		ienc(opOPIMM, 0, 4, 0, 30),       // ADDI x4, x0, 30  (new JIT block)
		renc(opOP, 0, 0x00, 5, 1, 2),     // ADD x5, x1, x2   (JIT'd)
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
		ienc(opOPIMM, 0, 1, 0, 1),        // ADDI x1, x0, 1
		benc(opBRANCH, 0, 0, 0, 12),      // BEQ x0, x0, +12 (always taken → 0x1010)
		ienc(opOPIMM, 0, 2, 0, 999),      // ADDI x2, x0, 999 (SKIPPED)
		ienc(opOPIMM, 0, 3, 0, 999),      // ADDI x3, x0, 999 (SKIPPED)
		ienc(opOPIMM, 0, 2, 0, 42),       // ADDI x2, x0, 42 (branch target)
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
		ienc(opOPIMM, 0, 1, 0, 1),        // ADDI x1, x0, 1
		ienc(opOPIMM, 0, 2, 0, 2),        // ADDI x2, x0, 2
		benc(opBRANCH, 1, 1, 1, 8),       // BNE x1, x1, +8 (never taken)
		ienc(opOPIMM, 0, 3, 0, 42),       // ADDI x3, x0, 42 (falls through)
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
		ienc(opOPIMM, 0, 1, 0, 1),  // ADDI x1, x0, 1
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
		ienc(opOPIMM, 0, 3, 0, 10), // 0x1000: ADDI x3, x0, 10
		jenc(1, 12),                 // 0x1004: JAL x1(ra), +12 → 0x1010
		ienc(opOPIMM, 0, 4, 0, 999), // 0x1008: ADDI x4, x0, 999 (skipped)
		ienc(opOPIMM, 0, 5, 0, 999), // 0x100C: skipped
		ienc(opOPIMM, 0, 2, 0, 20), // 0x1010: ADDI x2, x0, 20
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
		ienc(opOPIMM, 0, 1, 0, 10),    // 0x1000: ADDI x1, x0, 10
		ienc(opOPIMM, 0, 5, 0, 0),     // 0x1004: ADDI x5, x0, 0
		ienc(opOPIMM, 0, 5, 5, 0x10),  // 0x1008: ADDI x5, x5, 0x10 → x5=0x10
		// Need to add 0x1000 to get 0x1010. Use AUIPC.
		uenc(opAUIPC, 6, 0),            // 0x100C: AUIPC x6, 0 → x6 = 0x100C
		renc(opOP, 0, 0, 5, 6, 5),     // 0x1010: ADD x5, x6, x5 → x5 = 0x101C
		ienc(opJALR, 0, 0, 5, 0),      // 0x1014: JALR x0, x5, 0 → jump to 0x101C
		ienc(opOPIMM, 0, 7, 0, 999),   // 0x1018: ADDI x7, x0, 999 (skipped)
		ienc(opOPIMM, 0, 2, 0, 20),    // 0x101C: ADDI x2, x0, 20
		instrECALL,                     // 0x1020: ECALL
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

// ── Test 11: Translation failure and noJIT fallback ──────────────────────

func TestJIT_TranslationFailure(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 1, 0, 0xC00) // CSRRS x1, cycle, x0

	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		csrrs,       // untranslatable
		instrECALL,
	})
	defer mem.Free()
	cpu.Notes.Push(ecallStop)

	jit := NewJIT()
	jit.RunJIT(cpu)

	if !jit.noJIT[0x1000] {
		t.Error("expected noJIT[0x1000] to be set after translation failure")
	}
	// x1 should have a cycle count value (nonzero after at least 1 instruction)
	t.Logf("x1 (cycle) = %d", cpu.Reg(1))
}

// ── Test 12: Bail label safety net ───────────────────────────────────────

func TestJIT_BailLabel(t *testing.T) {
	csrrs := ienc(opSYSTEM, 2, 3, 0, 0xC00) // CSRRS x3, cycle, x0

	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 0, 1),    // ADDI x1, x0, 1
		benc(opBRANCH, 0, 0, 0, 8),   // BEQ x0, x0, +8 → 0x100C
		ienc(opOPIMM, 0, 2, 0, 999),  // ADDI x2, x0, 999 (skipped)
		csrrs,                          // 0x100C: untranslatable → bail label
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
	if jit.lastPC != 0x1000 {
		t.Errorf("lastPC = 0x%x, want 0x1000", jit.lastPC)
	}
	if jit.lastBlk == nil {
		t.Error("lastBlk is nil")
	}
	if len(jit.blocks) != 1 {
		t.Errorf("len(blocks) = %d, want 1", len(jit.blocks))
	}
}

// ── Test 14: Fault address correctness ───────────────────────────────────

func TestJIT_FaultAddress(t *testing.T) {
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),    // ADDI x10, x0, 1
		ienc(opOPIMM, 1, 10, 10, 26),  // SLLI x10, x10, 26 → 0x4000000
		ienc(opOPIMM, 0, 10, 10, 8),   // ADDI x10, x10, 8 → 0x4000008
		ienc(opLOAD, 2, 11, 10, 0),    // LW x11, 0(x10) → fault at 0x4000008
		instrECALL,
	})
	defer mem.Free()

	var faultAddr uint64
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseLoadFault {
			faultAddr = n.Tval
			return NoteFatal
		}
		return NoteForward
	})

	jit := NewJIT()
	err := jit.RunJIT(cpu)

	if faultAddr == 0 && err == nil {
		t.Fatal("expected load fault")
	}
	t.Logf("fault addr = 0x%x", faultAddr)
}
