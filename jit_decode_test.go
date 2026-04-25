package riscv

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestClassifyFlow_EcallNotGated confirms ECALL classification is
// independent of InlineEcallEnabled — all three SYSTEM-opcode
// instructions (ECALL, EBREAK, CSR*) must always return flowTerm.
// Under Option D the AOT enumerator relies on flowTerm to register
// pc+4 as a new block entry, which lowerSyscall then targets with a
// chain exit when the flag is on.
func TestClassifyFlow_EcallNotGated(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
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

	saved := InlineEcallEnabled()
	defer SetInlineEcallEnabled(saved)

	type row struct {
		name string
		insn uint32
		flag bool
	}
	rows := []row{
		{"ECALL flag=off", 0x00000073, false},
		{"ECALL flag=on", 0x00000073, true},
		{"EBREAK flag=off", 0x00100073, false},
		{"EBREAK flag=on", 0x00100073, true},
		{"CSRRW flag=off", 0x30001073, false},
		{"CSRRW flag=on", 0x30001073, true},
	}

	for _, r := range rows {
		writeInsn(r.insn)
		SetInlineEcallEnabled(r.flag)
		gotFC, _, sz := classifyFlow(mem, pc)
		if gotFC != flowTerm || sz != 4 {
			t.Errorf("%s: got (fc=%v, sz=%d), want (fc=flowTerm, sz=4)",
				r.name, gotFC, sz)
		}
	}
}

// TestInlineEcall_HelloEndToEnd runs the full hello-world ELF with
// the InlineEcallEnabled flag on. After Step 4 (Option D) and until
// Step 5 lands, flag-on behavior is bit-identical to flag-off: the
// emitter terminates at every ECALL and the V1 lowerer still emits
// an unconditional epilogue. This test guards that identity, so when
// Step 5 starts emitting the inline TESTQ+JNZ+ChainExit pattern we
// can attribute any regression to Step 5 specifically.
func TestInlineEcall_HelloEndToEnd(t *testing.T) {
	if !DirectSyscallEnabled() {
		t.Skip("direct syscall fast path disabled")
	}
	data, err := os.ReadFile("bench/hello_guest/hello_gocpu.elf")
	if err != nil {
		t.Skipf("bench/hello_guest/hello_gocpu.elf: %v", err)
	}

	saved := InlineEcallEnabled()
	defer SetInlineEcallEnabled(saved)
	SetInlineEcallEnabled(true)

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	entry, err := LoadELFBytes(mem, data)
	if err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	cpu.SetReg(2, 0x03F00000)

	cleanup := InstallLinuxOS(cpu, io.Discard)
	defer cleanup()

	j := NewJIT()

	captured := captureStdout(t, func() {
		var runErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, ok := r.(*ExitError); ok {
						return
					}
					panic(r)
				}
			}()
			runErr = j.RunJIT(cpu)
		}()
		if runErr != nil {
			t.Fatalf("RunJIT: %v", runErr)
		}
	})

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if len(captured) != len(want) {
		t.Fatalf("captured length = %d, want %d", len(captured), len(want))
	}
	if string(captured) != want {
		t.Fatal("captured mismatch (same length, different content)")
	}
}
