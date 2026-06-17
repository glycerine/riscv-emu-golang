package riscv

import "testing"

func TestRVCUnalignedScalarLoadStoreFallback(t *testing.T) {
	mem, err := NewGuestMemory(Size64KB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const (
		code = uint64(0x1000)
		data = uint64(0x2001)
	)

	t.Run("C.LD", func(t *testing.T) {
		if fault := mem.Store16(code, 0x6000); fault != nil { // c.ld x8, 0(x8)
			t.Fatal(fault)
		}
		if fault := mem.WriteBytes(data, []byte{0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11}); fault != nil {
			t.Fatal(fault)
		}
		cpu := NewCPU(*mem)
		cpu.SetPC(code)
		cpu.SetReg(8, data)
		if err := cpu.Step(); err != nil {
			t.Fatalf("Step C.LD: %v", err)
		}
		if got, want := cpu.Reg(8), uint64(0x1122334455667788); got != want {
			t.Fatalf("C.LD x8 = 0x%x, want 0x%x", got, want)
		}
	})

	t.Run("C.SD", func(t *testing.T) {
		if fault := mem.Store16(code, 0xe004); fault != nil { // c.sd x9, 0(x8)
			t.Fatal(fault)
		}
		cpu := NewCPU(*mem)
		cpu.SetPC(code)
		cpu.SetReg(8, data)
		cpu.SetReg(9, 0xaabbccddeeff0011)
		if err := cpu.Step(); err != nil {
			t.Fatalf("Step C.SD: %v", err)
		}
		got, fault := mem.Load64U(data)
		if fault != nil {
			t.Fatal(fault)
		}
		if want := uint64(0xaabbccddeeff0011); got != want {
			t.Fatalf("stored value = 0x%x, want 0x%x", got, want)
		}
	})
}
