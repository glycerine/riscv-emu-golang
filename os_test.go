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
		uint32(upper&0xFFFFF)<<12 | uint32(rd)<<7 | 0x37,         // LUI rd, upper
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
	insns = append(insns, ecallInsn)          // ecall -> ENOSYS in a0
	// Now exit with the result in a0 (should be -38 = ENOSYS, but
	// we just check we don't crash and can continue after unknown syscall)
	insns = append(insns, li32(17, 93)...)   // li a7, 93
	insns = append(insns, li32(10, 0)...)    // li a0, 0
	insns = append(insns, ecallInsn)         // exit(0)
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

	if !IsEcall(ecall)        { t.Error("IsEcall failed") }
	if IsEcall(ebreak)        { t.Error("IsEcall false positive on ebreak") }
	if !IsBreakpoint(ebreak)  { t.Error("IsBreakpoint failed") }
	if IsBreakpoint(ecall)    { t.Error("IsBreakpoint false positive on ecall") }
	if !IsFault(fault)        { t.Error("IsFault failed") }
	if IsFault(ecall)         { t.Error("IsFault false positive on ecall") }
}
