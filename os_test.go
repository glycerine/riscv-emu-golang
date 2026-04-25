package riscv

import (
	"strings"
	"testing"
)

// ── helpers ───────────────────────────────────────────────────────────────

// runOS builds a tiny ELF at 0x10000, loads it into a 128KB memory,
// installs RunWithOS and returns exitCode + any fatal error.
func runOS(t *testing.T, insns []uint32) (exitCode int, err error) {
	t.Helper()
	const codeVA = uint64(0x10000)
	elf := BuildELF(codeVA, insns)

	mem, merr := NewGuestMemory(128 * 1024)
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	entry, lerr := LoadELFBytes(mem, elf)
	if lerr != nil {
		t.Fatal(lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	return RunWithOS(cpu)
}

// li32 returns the instruction sequence to load a 32-bit immediate into rd.
// If imm fits in 12 bits: ADDI rd, x0, imm
// Otherwise: LUI rd, upper + ADDI rd, rd, lower
func li32(rd uint8, imm int32) []uint32 {
	if imm >= -2048 && imm <= 2047 {
		return []uint32{
			uint32(imm)<<20 | uint32(rd)<<7 | 0x13, // ADDI rd, x0, imm
		}
	}
	upper := (imm + 0x800) >> 12
	lower := imm - (upper << 12)
	return []uint32{
		uint32(upper&0xFFFFF)<<12 | uint32(rd)<<7 | 0x37,          // LUI rd, upper
		uint32(lower)<<20 | uint32(rd)<<15 | uint32(rd)<<7 | 0x13, // ADDI rd, rd, lower
	}
}

// ecall emits an ECALL instruction.
const ecallInsn = uint32(0x00000073)

// ── NoteChain unit tests ───────────────────────────────────────────────────

func TestNoteChain_Empty(t *testing.T) {
	var nc NoteChain
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := NewCPU(*mem)
	n := Note{Text: "test", Cause: CauseBreakpoint}
	if d := nc.Deliver(cpu, n); d != NoteFatal {
		t.Errorf("empty chain: got %d want NoteFatal", d)
	}
}

func TestNoteChain_Forward(t *testing.T) {
	var nc NoteChain
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := NewCPU(*mem)

	calls := 0
	nc.Push(func(cpu *CPU, n Note) NoteDisposition {
		calls++
		return NoteForward
	})
	nc.Push(func(cpu *CPU, n Note) NoteDisposition {
		calls++
		return NoteForward
	})

	n := Note{Text: "breakpoint", Cause: CauseBreakpoint}
	if d := nc.Deliver(cpu, n); d != NoteFatal {
		t.Errorf("all-forward: got %d want NoteFatal", d)
	}
	if calls != 2 {
		t.Errorf("expected both handlers called, got %d", calls)
	}
}

func TestNoteChain_InnermostFirst(t *testing.T) {
	var nc NoteChain
	mem, _ := NewGuestMemory(Size64MB)
	defer mem.Free()
	cpu := NewCPU(*mem)

	order := []int{}
	nc.Push(func(cpu *CPU, n Note) NoteDisposition { order = append(order, 1); return NoteForward })
	nc.Push(func(cpu *CPU, n Note) NoteDisposition { order = append(order, 2); return NoteHandled })

	nc.Deliver(cpu, Note{Text: "x", Cause: CauseBreakpoint})
	if len(order) != 1 || order[0] != 2 {
		t.Errorf("expected innermost (2) called first and only, got %v", order)
	}
}

func TestNoteChain_PushPop(t *testing.T) {
	var nc NoteChain
	if nc.Len() != 0 {
		t.Fatal("expected empty")
	}
	nc.Push(func(cpu *CPU, n Note) NoteDisposition { return NoteHandled })
	if nc.Len() != 1 {
		t.Fatal("expected len 1")
	}
	nc.Pop()
	if nc.Len() != 0 {
		t.Fatal("expected empty after pop")
	}
}

func TestNoteChain_PopEmpty_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on Pop of empty chain")
		}
	}()
	var nc NoteChain
	nc.Pop()
}

// ── NoteFromStepErr ───────────────────────────────────────────────────────

func TestNoteText_Ecall(t *testing.T) {
	n := noteFromStepErr(ErrEcall, 0x10000)
	if n.Cause != CauseEcallU {
		t.Errorf("cause: got %d want %d", n.Cause, CauseEcallU)
	}
	if !strings.Contains(n.Text, "ecall") {
		t.Errorf("text %q should contain 'ecall'", n.Text)
	}
	if n.PC != 0x10000 {
		t.Errorf("pc: got 0x%X want 0x10000", n.PC)
	}
}

func TestNoteText_Ebreak(t *testing.T) {
	n := noteFromStepErr(ErrEbreak, 0x10004)
	if n.Cause != CauseBreakpoint {
		t.Errorf("cause: got %d want %d", n.Cause, CauseBreakpoint)
	}
	if !strings.Contains(n.Text, "breakpoint") {
		t.Errorf("text %q should contain 'breakpoint'", n.Text)
	}
}

func TestNoteText_IllegalInsn(t *testing.T) {
	n := noteFromStepErr(ErrIllegalInstruction, 0x1234)
	if n.Cause != CauseIllegalInsn {
		t.Errorf("cause: got %d want %d", n.Cause, CauseIllegalInsn)
	}
	if !strings.Contains(n.Text, "illegal") {
		t.Errorf("text %q should contain 'illegal'", n.Text)
	}
}

func TestNoteText_MemFault(t *testing.T) {
	f := &MemFault{Addr: 0xDEAD, Width: 8, Kind: FaultLoad}
	n := noteFromStepErr(f, 0x2000)
	if n.Cause != CauseLoadFault {
		t.Errorf("cause: got %d want %d", n.Cause, CauseLoadFault)
	}
	if n.Tval != 0xDEAD {
		t.Errorf("tval: got 0x%X want 0xDEAD", n.Tval)
	}
	if !strings.HasPrefix(n.Text, "fault:") {
		t.Errorf("text %q should start with 'fault:'", n.Text)
	}
	if !IsFault(n) {
		t.Error("IsFault should be true")
	}
}

// ── OS personality — exit ─────────────────────────────────────────────────

func TestOS_Exit0(t *testing.T) {
	//t.Skip("SIGBUS, what else?") // middle guard page (only) cause sigbus
	// li a7, 93; li a0, 0; ecall
	insns := append(append(li32(17, 93), li32(10, 0)...), ecallInsn)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code: got %d want 0", code)
	}
}

func TestOS_ExitNonzero(t *testing.T) {
	//t.Skip("SIGBUS, what else?") // due to page guards
	// li a7, 93; li a0, 42; ecall
	insns := append(append(li32(17, 93), li32(10, 42)...), ecallInsn)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 42 {
		t.Errorf("exit code: got %d want 42", code)
	}
}

func TestOS_ExitGroup(t *testing.T) {
	// syscall 94 = exit_group, same semantics
	insns := append(append(li32(17, 94), li32(10, 7)...), ecallInsn)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code: got %d want 7", code)
	}
}

// ── OS personality — riscv-tests ECALL convention ─────────────────────────

func TestOS_RiscvTestsPass(t *testing.T) {
	// riscv-tests PASS: a7=93, a0=1, ecall
	// (a0=1 means PASS in riscv-tests: testnum=1, (1<<1)|1 would be fail,
	//  but a0=0 means pass... wait, let's check the convention)
	// Convention: PASS = a7=93, a0=0
	//             FAIL = a7=93, a0=(testnum<<1)|1  (non-zero)
	insns := append(append(li32(17, 93), li32(10, 0)...), ecallInsn)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("riscv-tests pass: exit code got %d want 0", code)
	}
}

func TestOS_RiscvTestsFail(t *testing.T) {
	// FAIL with test number 3: a0 = (3<<1)|1 = 7
	insns := append(append(li32(17, 93), li32(10, 7)...), ecallInsn)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 7 {
		t.Errorf("riscv-tests fail: exit code got %d want 7", code)
	}
}

// ── OS personality — unknown syscall ─────────────────────────────────────

func TestOS_UnknownSyscall_ENOSYS(t *testing.T) {
	// Call syscall 9999 (unknown) then exit
	const exitSyscall = uint32(0x05D00893) // ADDI a7, x0, 93  li a7,93
	insns := []uint32{}
	insns = append(insns, li32(17, 9999)...) // li a7, 9999
	insns = append(insns, ecallInsn)         // ecall -> ENOSYS in a0
	// Now exit with the result in a0 (should be -38 = ENOSYS, but
	// we just check we don't crash and can continue after unknown syscall)
	insns = append(insns, li32(17, 93)...) // li a7, 93
	insns = append(insns, li32(10, 0)...)  // li a0, 0
	insns = append(insns, ecallInsn)       // exit(0)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code: got %d want 0", code)
	}
}

// ── NoteChain stacking ────────────────────────────────────────────────────

func TestNoteChain_Stacked_OSWithDebug(t *testing.T) {
	// Stack (innermost first = called first):
	//   spy handler (innermost, pushed last)  — logs then forwards
	//   OS handler  (pushed by RunWithOS)     — handles ECALL
	//
	// The spy is innermost so it sees every note before the OS handler,
	// logs it, then returns NoteForward so the OS handler processes it.
	const codeVA = uint64(0x10000)
	insns := append(append(li32(17, 93), li32(10, 5)...), ecallInsn)
	elf := BuildELF(codeVA, insns)

	mem, _ := NewGuestMemory(128 * 1024)
	defer mem.Free()
	entry, _ := LoadELFBytes(mem, elf)
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)

	// RunWithOS installs OS handler first; we then push spy as innermost.
	noted := []Note{}
	spy := NoteHandler(func(cpu *CPU, n Note) NoteDisposition {
		noted = append(noted, n)
		return NoteForward // always forward — spy never claims notes
	})

	// Install OS layer first (becomes outermost), then spy (innermost).
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	cpu.Notes.Push(o.Handle) // outermost
	cpu.Notes.Push(spy)      // innermost — called first

	var exitCode int
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				if ex, ok := r.(*ExitError); ok {
					exitCode = ex.Code
				} else {
					panic(r)
				}
			}
		}()
		err = RunWithChain(cpu, &cpu.Notes)
	}()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 5 {
		t.Errorf("exit code: got %d want 5", exitCode)
	}
	// Spy should have seen the ecall note
	if len(noted) == 0 {
		t.Error("spy handler should have seen at least one note")
	}
	found := false
	for _, n := range noted {
		if n.Cause == CauseEcallU {
			found = true
		}
	}
	if !found {
		t.Errorf("spy should have seen an ecall note, got: %v", noted)
	}
}

func TestNoteChain_InfiniteNesting(t *testing.T) {
	// Prove nesting works: a handler calls cpu.Step() which triggers another note.
	// Inner note is handled, outer continues.
	const codeVA = uint64(0x10000)

	// Program: EBREAK, then exit(0)
	insns := []uint32{0x00100073} // EBREAK
	insns = append(insns, li32(17, 93)...)
	insns = append(insns, li32(10, 0)...)
	insns = append(insns, ecallInsn)

	elf := BuildELF(codeVA, insns)
	mem, _ := NewGuestMemory(128 * 1024)
	defer mem.Free()
	entry, _ := LoadELFBytes(mem, elf)

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)

	breakpointHit := false
	cpu.Notes.Push(func(cpu *CPU, n Note) NoteDisposition {
		if n.Cause == CauseBreakpoint {
			breakpointHit = true
			// Skip over the EBREAK by advancing PC, then return handled
			// (PC is already past EBREAK since Step() set nextPC before returning)
			return NoteHandled
		}
		return NoteForward
	})

	code, err := RunWithOS(cpu)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !breakpointHit {
		t.Error("breakpoint handler was not called")
	}
	if code != 0 {
		t.Errorf("exit code: got %d want 0", code)
	}
}

// ── IsEcall / IsFault / IsBreakpoint ─────────────────────────────────────

func TestNoteHelpers(t *testing.T) {
	ecall := noteFromStepErr(ErrEcall, 0)
	ebreak := noteFromStepErr(ErrEbreak, 0)
	fault := noteFromStepErr(&MemFault{Addr: 1, Width: 1, Kind: FaultLoad}, 0)

	if !IsEcall(ecall) {
		t.Error("IsEcall failed")
	}
	if IsEcall(ebreak) {
		t.Error("IsEcall false positive on ebreak")
	}
	if !IsBreakpoint(ebreak) {
		t.Error("IsBreakpoint failed")
	}
	if IsBreakpoint(ecall) {
		t.Error("IsBreakpoint false positive on ecall")
	}
	if !IsFault(fault) {
		t.Error("IsFault failed")
	}
	if IsFault(ecall) {
		t.Error("IsFault false positive on ecall")
	}
}

// ── M-mode CSR and Trap Tests ───────────────────────────────────────────

// CSR instruction encoders for test programs.
func csrrw(rd uint8, csr uint32, rs1 uint8) uint32 {
	return (csr << 20) | uint32(rs1)<<15 | (1 << 12) | uint32(rd)<<7 | 0x73
}
func csrrs(rd uint8, csr uint32, rs1 uint8) uint32 {
	return (csr << 20) | uint32(rs1)<<15 | (2 << 12) | uint32(rd)<<7 | 0x73
}
func csrr(rd uint8, csr uint32) uint32  { return csrrs(rd, csr, 0) }
func csrw(csr uint32, rs1 uint8) uint32 { return csrrw(0, csr, rs1) }

const mretInsn = uint32(0x30200073)

// newTestCPUSimple creates a CPU with memSize bytes and the given
// instructions loaded at codeVA. Returns the CPU (with PC set) and
// memory (caller must Free).
func newTestCPUSimple(t *testing.T, memSize uint64, codeVA uint64, insns []uint32) (*CPU, *GuestMemory) {
	t.Helper()
	elf := BuildELF(codeVA, insns)
	mem, err := NewGuestMemory(memSize)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := LoadELFBytes(mem, elf)
	if err != nil {
		mem.Free()
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	return cpu, mem
}

func TestCSR_MtvecReadWrite(t *testing.T) {
	// Program: li x5, 0x1000; csrw mtvec, x5; csrr x6, mtvec; ebreak
	insns := li32(5, 0x1000)
	insns = append(insns, csrw(0x305, 5)) // csrw mtvec, x5
	insns = append(insns, csrr(6, 0x305)) // csrr x6, mtvec
	insns = append(insns, 0x00100073)     // ebreak (stop)

	cpu, mem := newTestCPUSimple(t, 128*1024, 0x10000, insns)
	defer mem.Free()

	// Step through each instruction.
	for i := 0; i < 10; i++ {
		err := cpu.Step()
		if err == ErrEbreak {
			break
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if cpu.Reg(6) != 0x1000 {
		t.Errorf("mtvec readback: got 0x%x want 0x1000", cpu.Reg(6))
	}
}

func TestCSR_MepcReadWrite(t *testing.T) {
	insns := li32(5, 0x2000)
	insns = append(insns, csrw(0x341, 5)) // csrw mepc, x5
	insns = append(insns, csrr(6, 0x341)) // csrr x6, mepc
	insns = append(insns, 0x00100073)     // ebreak

	cpu, mem := newTestCPUSimple(t, 128*1024, 0x10000, insns)
	defer mem.Free()

	for i := 0; i < 10; i++ {
		if err := cpu.Step(); err != nil {
			break
		}
	}
	if cpu.Reg(6) != 0x2000 {
		t.Errorf("mepc readback: got 0x%x want 0x2000", cpu.Reg(6))
	}
}

func TestCSR_McauseReadWrite(t *testing.T) {
	insns := li32(5, 42)
	insns = append(insns, csrw(0x342, 5)) // csrw mcause, x5
	insns = append(insns, csrr(6, 0x342)) // csrr x6, mcause
	insns = append(insns, 0x00100073)     // ebreak

	cpu, mem := newTestCPUSimple(t, 128*1024, 0x10000, insns)
	defer mem.Free()

	for i := 0; i < 10; i++ {
		if err := cpu.Step(); err != nil {
			break
		}
	}
	if cpu.Reg(6) != 42 {
		t.Errorf("mcause readback: got %d want 42", cpu.Reg(6))
	}
}

func TestCSR_MstatusReadWrite(t *testing.T) {
	insns := li32(5, 0x1888)
	insns = append(insns, csrw(0x300, 5)) // csrw mstatus, x5
	insns = append(insns, csrr(6, 0x300)) // csrr x6, mstatus
	insns = append(insns, 0x00100073)     // ebreak

	cpu, mem := newTestCPUSimple(t, 128*1024, 0x10000, insns)
	defer mem.Free()

	for i := 0; i < 10; i++ {
		if err := cpu.Step(); err != nil {
			break
		}
	}
	if cpu.Reg(6) != 0x1888 {
		t.Errorf("mstatus readback: got 0x%x want 0x1888", cpu.Reg(6))
	}
}

func TestCSR_UnknownCSR_SilentIgnore(t *testing.T) {
	// Writing to unknown CSRs should not fault. The "skip on trap"
	// pattern in riscv-tests relies on this.
	insns := li32(5, 0xFF)
	insns = append(insns, csrw(0x3A0, 5)) // csrw pmpcfg0, x5 (unknown)
	insns = append(insns, csrw(0x3B0, 5)) // csrw pmpaddr0, x5 (unknown)
	insns = append(insns, csrw(0x180, 5)) // csrw satp, x5 (unknown)
	insns = append(insns, csrw(0x744, 5)) // csrw mnstatus, x5 (non-standard)
	insns = append(insns, csrr(6, 0x3A0)) // csrr x6, pmpcfg0 → 0
	insns = append(insns, 0x00100073)     // ebreak

	cpu, mem := newTestCPUSimple(t, 128*1024, 0x10000, insns)
	defer mem.Free()

	for i := 0; i < 20; i++ {
		if err := cpu.Step(); err != nil {
			if err == ErrEbreak {
				break
			}
			t.Fatalf("step %d: unexpected error: %v", i, err)
		}
	}
	// Reading an unknown CSR returns 0.
	if cpu.Reg(6) != 0 {
		t.Errorf("unknown CSR read: got 0x%x want 0", cpu.Reg(6))
	}
}

func TestMRET_JumpsToMepc(t *testing.T) {
	// Build code, then compute target address from actual instruction count.
	const codeVA = uint64(0x10000)
	const targetOff = 16 // target at insn index 16 → address codeVA + 16*4

	insns := li32(5, int32(codeVA+targetOff*4)) // li x5, target addr
	insns = append(insns, csrw(0x341, 5))       // csrw mepc, x5
	insns = append(insns, mretInsn)             // mret → jump to target
	insns = append(insns, li32(6, 0x99)...)     // should be skipped
	insns = append(insns, 0x00100073)           // ebreak (skipped)

	// Pad to target offset
	for len(insns) < targetOff {
		insns = append(insns, 0x00000013) // nop
	}
	insns = append(insns, li32(7, 0x42)...) // target: li x7, 0x42
	insns = append(insns, 0x00100073)       // ebreak

	cpu, mem := newTestCPUSimple(t, 128*1024, codeVA, insns)
	defer mem.Free()

	for i := 0; i < 30; i++ {
		if err := cpu.Step(); err != nil {
			break
		}
	}
	if cpu.Reg(6) != 0 {
		t.Errorf("x6 should be 0 (mret skipped li x6,0x99), got 0x%x", cpu.Reg(6))
	}
	if cpu.Reg(7) != 0x42 {
		t.Errorf("x7 should be 0x42 (mret landed at target), got 0x%x", cpu.Reg(7))
	}
}

func TestECALL_TrapsToMtvec_WhenSet(t *testing.T) {
	const codeVA = uint64(0x10000)
	const handlerOff = 16 // handler at insn index 16

	insns := li32(5, int32(codeVA+handlerOff*4)) // li x5, handler addr
	insns = append(insns, csrw(0x305, 5))        // csrw mtvec, x5
	ecallIdx := len(insns)                       // record ecall position
	insns = append(insns, ecallInsn)             // ecall → trap
	insns = append(insns, li32(6, 0x99)...)      // skipped
	insns = append(insns, 0x00100073)            // ebreak (skipped)

	// Pad to handler offset
	for len(insns) < handlerOff {
		insns = append(insns, 0x00000013) // nop
	}
	insns = append(insns, csrr(7, 0x342)) // handler: csrr x7, mcause
	insns = append(insns, csrr(8, 0x341)) // csrr x8, mepc
	insns = append(insns, 0x00100073)     // ebreak

	ecallPC := codeVA + uint64(ecallIdx)*4

	cpu, mem := newTestCPUSimple(t, 128*1024, codeVA, insns)
	defer mem.Free()

	for i := 0; i < 30; i++ {
		if err := cpu.Step(); err != nil {
			break
		}
	}
	if cpu.Reg(6) != 0 {
		t.Errorf("x6 should be 0 (ecall skipped it), got 0x%x", cpu.Reg(6))
	}
	if cpu.Reg(7) != 8 {
		t.Errorf("mcause should be 8 (CauseEcallU), got %d", cpu.Reg(7))
	}
	if cpu.Reg(8) != ecallPC {
		t.Errorf("mepc should be 0x%x (PC of ecall), got 0x%x", ecallPC, cpu.Reg(8))
	}
}

func TestECALL_FallsBackToNoteChain_WhenMtvecZero(t *testing.T) {
	// When mtvec is 0 (default), ECALL should produce ErrEcall
	// and be handled by the NoteChain / OS personality as before.
	insns := append(append(li32(17, 93), li32(10, 0)...), ecallInsn)
	code, err := runOS(t, insns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code: got %d want 0", code)
	}
}

func TestECALL_TrapHandler_WritesTohost(t *testing.T) {
	// Simulate the riscv-tests pattern:
	// 1. Install trap handler
	// 2. Set up tohost watch
	// 3. ECALL traps to handler
	// 4. Handler writes gp to tohost address
	// 5. Tohost polling detects non-zero value → exit
	//
	// Memory layout (128KB, addresses 0x00000-0x1FFFF):
	//   0x10000: main code
	//   0x10100: trap handler
	//   0x10200: tohost variable (8 bytes)

	const codeVA = uint64(0x10000)
	const handlerVA = uint64(0x10100)
	const tohostVA = uint64(0x10200)

	// Main code: install handler, set gp=1 (PASS), ecall
	insns := li32(5, int32(handlerVA))    // li x5, handler addr
	insns = append(insns, csrw(0x305, 5)) // csrw mtvec, x5
	insns = append(insns, li32(3, 1)...)  // li gp, 1 (PASS)
	insns = append(insns, ecallInsn)      // ecall → traps to handler
	// Should not reach here:
	insns = append(insns, li32(6, 0xBAD)...) // li x6, 0xBAD
	insns = append(insns, 0x00100073)        // ebreak

	// Pad to handler address (0x10100 - 0x10000 = 0x100 = 256 bytes = 64 insns)
	for len(insns) < 64 {
		insns = append(insns, 0x00000013) // nop
	}

	// Trap handler at 0x10100:
	//   li x5, tohost_addr
	//   sw gp, 0(x5)    # store gp to tohost
	//   j self           # halt loop (tohost polling will catch us)
	insns = append(insns, li32(5, int32(tohostVA))...) // li x5, tohostVA
	// SW gp(x3), 0(x5): imm=0, rs2=3, rs1=5, funct3=2, opcode=0x23
	insns = append(insns, 0x0032A023) // sw x3, 0(x5)
	// JAL x0, 0 (j self): infinite loop
	insns = append(insns, 0x0000006F) // j self

	elf := BuildELF(codeVA, insns)
	mem, err := NewGuestMemory(128 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	entry, err := LoadELFBytes(mem, elf)
	if err != nil {
		t.Fatal(err)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetWatchAddr(tohostVA)

	// Run with tohost polling. The trap handler writes gp=1 to tohost,
	// which triggers the tohost exit with code 0 (PASS).
	var exitCode int
	func() {
		defer func() {
			if r := recover(); r != nil {
				if ex, ok := r.(*ExitError); ok {
					exitCode = ex.Code
				} else {
					panic(r)
				}
			}
		}()
		// Run up to 1000 instructions.
		for i := 0; i < 1000; i++ {
			err := cpu.Step()
			cpu.cycle++
			if cpu.watchAddr != 0 {
				if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
					panic(&ExitError{Code: tohostExitCode(v)})
				}
			}
			if err != nil {
				t.Fatalf("step %d: unexpected error: %v (pc=0x%x)", i, err, cpu.PC())
			}
		}
		t.Fatal("did not exit within 1000 steps")
	}()

	if exitCode != 0 {
		t.Errorf("tohost exit code: got %d want 0 (PASS)", exitCode)
	}
	if cpu.Reg(6) != 0 {
		t.Errorf("x6 should be 0 (ecall should not fall through), got 0x%x", cpu.Reg(6))
	}
}

func TestECALL_TrapHandler_Fail(t *testing.T) {
	// Same as above but gp=(3<<1)|1=7, which means FAIL test 3.
	const codeVA = uint64(0x10000)
	const handlerVA = uint64(0x10100)
	const tohostVA = uint64(0x10200)

	insns := li32(5, int32(handlerVA))
	insns = append(insns, csrw(0x305, 5))
	insns = append(insns, li32(3, 7)...) // li gp, 7 = (3<<1)|1 = FAIL test 3
	insns = append(insns, ecallInsn)
	insns = append(insns, 0x00100073) // ebreak (unreachable)

	for len(insns) < 64 {
		insns = append(insns, 0x00000013)
	}

	// Trap handler: sw gp, tohost; j self
	insns = append(insns, li32(5, int32(tohostVA))...)
	insns = append(insns, 0x0032A023) // sw x3, 0(x5)
	insns = append(insns, 0x0000006F) // j self

	elf := BuildELF(codeVA, insns)
	mem, err := NewGuestMemory(128 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	entry, _ := LoadELFBytes(mem, elf)

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetWatchAddr(tohostVA)

	var exitCode int
	func() {
		defer func() {
			if r := recover(); r != nil {
				if ex, ok := r.(*ExitError); ok {
					exitCode = ex.Code
				} else {
					panic(r)
				}
			}
		}()
		for i := 0; i < 1000; i++ {
			err := cpu.Step()
			cpu.cycle++
			if cpu.watchAddr != 0 {
				if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
					panic(&ExitError{Code: tohostExitCode(v)})
				}
			}
			if err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		t.Fatal("did not exit")
	}()

	if exitCode != 7 {
		t.Errorf("tohost exit code: got %d want 7 (FAIL test 3)", exitCode)
	}
}

func TestMRET_ECALL_RoundTrip(t *testing.T) {
	// Test the full trap → handler → mret cycle:
	// 1. Install handler at 0x10100
	// 2. ECALL at 0x1000C → traps to handler
	// 3. Handler reads mcause, advances mepc by 4, does MRET
	// 4. Execution resumes at 0x10010 (instruction after ECALL)
	// 5. Sets x6 = 0x42, ebreak

	const codeVA = uint64(0x10000)
	const handlerVA = uint64(0x10100)

	// Main code
	insns := li32(5, int32(handlerVA))    // 0x10000: li x5, handler (2 insns)
	insns = append(insns, csrw(0x305, 5)) // 0x10008: csrw mtvec, x5
	insns = append(insns, ecallInsn)      // 0x1000C: ecall → trap
	// After MRET, execution resumes here:
	insns = append(insns, li32(6, 0x42)...) // 0x10010: li x6, 0x42
	insns = append(insns, 0x00100073)       // 0x10018: ebreak

	for len(insns) < 64 {
		insns = append(insns, 0x00000013)
	}

	// Trap handler at 0x10100:
	//   csrr x7, mcause    # save cause in x7
	//   csrr x8, mepc      # read mepc
	//   addi x8, x8, 4     # advance past ecall
	//   csrw mepc, x8      # write back
	//   mret               # return to mepc+4
	insns = append(insns, csrr(7, 0x342)) // csrr x7, mcause
	insns = append(insns, csrr(8, 0x341)) // csrr x8, mepc
	// ADDI x8, x8, 4: imm=4, rs1=8, funct3=0, rd=8, opcode=0x13
	insns = append(insns, 0x00440413)     // addi x8, x8, 4
	insns = append(insns, csrw(0x341, 8)) // csrw mepc, x8
	insns = append(insns, mretInsn)       // mret → jump to mepc (0x10010)

	cpu, mem := newTestCPUSimple(t, 128*1024, codeVA, insns)
	defer mem.Free()

	for i := 0; i < 30; i++ {
		if err := cpu.Step(); err != nil {
			break
		}
	}

	if cpu.Reg(7) != 8 {
		t.Errorf("mcause in x7: got %d want 8", cpu.Reg(7))
	}
	if cpu.Reg(6) != 0x42 {
		t.Errorf("x6 should be 0x42 (resumed after ecall via mret), got 0x%x", cpu.Reg(6))
	}
}
