package riscv

import "testing"

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
