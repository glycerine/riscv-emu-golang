package riscv

import "testing"

const (
	opAMONative = uint32(0x2F)

	amoFunct3W = uint32(0b010)
	amoFunct3D = uint32(0b011)

	amoFunct5Add  = uint32(0b00000)
	amoFunct5Swap = uint32(0b00001)
	amoFunct5LR   = uint32(0b00010)
	amoFunct5SC   = uint32(0b00011)
	amoFunct5OR   = uint32(0b01000)
	amoFunct5Min  = uint32(0b10000)
	amoFunct5MaxU = uint32(0b11100)
)

func amoenc(funct5, funct3, rd, rs1, rs2 uint32) uint32 {
	return funct5<<27 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opAMONative
}

func mustStore32AMO(t *testing.T, mem *GuestMemory, addr uint64, v uint32) {
	t.Helper()
	if f := mem.Store32(addr, v); f != nil {
		t.Fatalf("Store32(0x%x): %v", addr, f)
	}
}

func mustLoad32AMO(t *testing.T, mem *GuestMemory, addr uint64) uint32 {
	t.Helper()
	v, f := mem.Load32(addr)
	if f != nil {
		t.Fatalf("Load32(0x%x): %v", addr, f)
	}
	return v
}

func mustStore64AMO(t *testing.T, mem *GuestMemory, addr uint64, v uint64) {
	t.Helper()
	if f := mem.Store64(addr, v); f != nil {
		t.Fatalf("Store64(0x%x): %v", addr, f)
	}
}

func mustLoad64AMO(t *testing.T, mem *GuestMemory, addr uint64) uint64 {
	t.Helper()
	v, f := mem.Load64(addr)
	if f != nil {
		t.Fatalf("Load64(0x%x): %v", addr, f)
	}
	return v
}
