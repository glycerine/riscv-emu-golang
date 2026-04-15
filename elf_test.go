package riscv

import (
	"os"
	"strings"
	"testing"
)

// TestLoadELF_Header validates parsing of a known-good ELF header.
func TestLoadELF_Header(t *testing.T) {
	data, err := os.ReadFile("bench/libriscv_guest/bench_guest.elf")
	if err != nil {
		t.Skip("bench_guest.elf not present — run make bench-setup first")
	}

	mem, err := NewGuestMemory(Size64MB * 8)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	entry, err := LoadELFBytes(mem, data)
	if err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}
	if entry == 0 {
		t.Error("entry point is 0")
	}
	t.Logf("entry=0x%x loaded %d bytes", entry, len(data))

	// Verify we can fetch an instruction at the entry point
	insn, f := mem.Fetch32(entry)
	if f != nil {
		t.Fatalf("Fetch32 at entry 0x%x: %v", entry, f)
	}
	if insn == 0 {
		t.Error("instruction at entry is 0x00000000 — segment likely not loaded")
	}
	t.Logf("first insn at entry: 0x%08X", insn)
}

// TestLoadELF_Errors checks that bad inputs are rejected cleanly.
func TestLoadELF_Errors(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", []byte{}, "read header"},
		{"bad magic", append([]byte("NELF"), make([]byte, 60)...), "not an ELF"},
		{"32-bit", func() []byte {
			b := make([]byte, 64)
			copy(b, "\x7fELF")
			b[4] = 1 // ELFCLASS32
			b[5] = 1 // LE
			return b
		}(), "not a 64-bit"},
		{"big-endian", func() []byte {
			b := make([]byte, 64)
			copy(b, "\x7fELF")
			b[4] = 2 // 64-bit
			b[5] = 2 // BE
			return b
		}(), "not little-endian"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadELFBytes(mem, c.data)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

