package riscv

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestClassifyFlow_EcallGated confirms the InlineEcallEnabled flag
// is the only lever that changes ECALL classification. EBREAK and
// CSR* must remain terminators under both settings.
func TestClassifyFlow_EcallGated(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	defer mem.Free()

	const pc = uint64(0x100)

	// Helper: write one 32-bit insn at pc and restore prior flag state
	// between cases.
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
		name   string
		insn   uint32
		flag   bool
		wantFC flowClass
	}
	rows := []row{
		{"ECALL flag=off → flowTerm", 0x00000073, false, flowTerm},
		{"ECALL flag=on → flowSeq", 0x00000073, true, flowSeq},
		{"EBREAK flag=off → flowTerm", 0x00100073, false, flowTerm},
		{"EBREAK flag=on → flowTerm", 0x00100073, true, flowTerm},
		{"CSRRW flag=off → flowTerm", 0x30001073, false, flowTerm},
		{"CSRRW flag=on → flowTerm", 0x30001073, true, flowTerm},
	}

	for _, r := range rows {
		writeInsn(r.insn)
		SetInlineEcallEnabled(r.flag)
		gotFC, _, sz := classifyFlow(mem, pc)
		if gotFC != r.wantFC || sz != 4 {
			t.Errorf("%s: got (fc=%v, sz=%d), want (fc=%v, sz=4)",
				r.name, gotFC, sz, r.wantFC)
		}
	}
}

// TestInlineEcall_HelloEndToEnd runs the full hello-world ELF with the
// InlineEcallEnabled flag on. Until Step 5 lands the V1 lowerer still
// emits a RET after IRSyscall, so post-ECALL IR is dead at the host
// level and behavior should be bit-identical to the flag-off run —
// but the emitter will have continued past the ECALL, and
// classifyFlow will have treated ECALL as sequential when scanning
// the region. This test exists to catch a regression in Step 3 where
// the emitter continuation inadvertently breaks pre-ECALL branch
// exits (as happened with the dirty[] clear experiment).
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
