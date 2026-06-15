package riscv

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestClassifyFlow_EcallTerminatesBlock confirms all three SYSTEM-opcode
// instructions (ECALL, EBREAK, CSR*) return flowTerm. ECALL resumes at
// pc+4 only after the installed OS/personality handler completes.
func TestClassifyFlow_EcallTerminatesBlock(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	defer mem.Free()

	const pc = uint64(0x100)

	writeInsn := func(insn uint32) {
		if f := mem.Store16(pc, uint16(insn)); f != nil {
			t.Fatalf("Store16: %v", f)
		}
		if f := mem.Store16(pc+2, uint16(insn>>16)); f != nil {
			t.Fatalf("Store16: %v", f)
		}
	}

	type row struct {
		name string
		insn uint32
	}
	rows := []row{
		{"ECALL", 0x00000073},
		{"EBREAK", 0x00100073},
		{"CSRRW", 0x30001073},
	}

	for _, r := range rows {
		writeInsn(r.insn)
		gotFC, _, sz := classifyFlow(mem, pc)
		if gotFC != flowTerm || sz != 4 {
			t.Errorf("%s: got (fc=%v, sz=%d), want (fc=flowTerm, sz=4)",
				r.name, gotFC, sz)
		}
	}
}

// TestInlineEcall_HelloEndToEnd runs the full hello-world ELF through
// the JIT with an installed Linux OS personality. ECALL must return to
// Go for OS handling; JIT/native code must not issue host syscalls.
func TestInlineEcall_HelloEndToEnd(t *testing.T) {

	data, err := os.ReadFile("bench/hello_guest/hello_gocpu.elf")
	if err != nil {
		t.Skipf("bench/hello_guest/hello_gocpu.elf: %v", err)
	}

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	elf, err := LoadELFBytes(mem, data)
	if err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetReg(2, 0x03F00000)

	var out bytes.Buffer
	cleanup := InstallLinuxOS(cpu, &out)
	defer cleanup()

	j := NewJIT()
	runErr := j.RunJIT(cpu)
	if runErr != nil {
		if _, ok := runErr.(*ExitError); !ok {
			t.Fatalf("RunJIT: %v", runErr)
		}
	}

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if out.Len() != len(want) {
		t.Fatalf("output length = %d, want %d", out.Len(), len(want))
	}
	if out.String() != want {
		t.Fatal("output mismatch (same length, different content)")
	}
}

func TestInlineEcall_TinyLinuxWrite(t *testing.T) {

	const (
		codeVA = uint64(0x1000)
		msgVA  = uint64(0x2000)
	)
	msg := []byte("jit linux write\n")
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),               // a0 = stdout
		uenc(opLUI, 11, uint32(msgVA)),           // a1 = msgVA
		ienc(opOPIMM, 0, 12, 0, int32(len(msg))), // a2 = len
		ienc(opOPIMM, 0, 17, 0, 64),              // a7 = SYS_write
		instrECALL,
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}

	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()
	for i, b := range msg {
		if fault := mem.Store8(msgVA+uint64(i), b); fault != nil {
			t.Fatalf("Store8 msg[%d]: %v", i, fault)
		}
	}
	var out bytes.Buffer
	cleanup := InstallLinuxOS(cpu, &out)
	defer cleanup()

	j := NewJIT()
	err := j.RunJIT(cpu)
	if err != nil {
		if exit, ok := err.(*ExitError); !ok || exit.Code != 0 {
			t.Fatalf("RunJIT: %v", err)
		}
	}
	if out.String() != string(msg) {
		t.Fatalf("stdout = %q, want %q", out.String(), msg)
	}
}
