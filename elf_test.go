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
		// allowed now! in fact, preferred!
		//	t.Error("entry point is 0")
	}
	t.Logf("entry=0x%x loaded %d bytes", entry, len(data))

	// Verify we can fetch an instruction at the entry point
	// (linux vs darwin linking was producing different alignment):
	//insn, f := mem.Fetch32(entry) // requires 4-byte alignment.
	insn, f := mem.Fetch32U(entry) // allows 2-byte aligned C (compact) code.
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

func TestFindSymbolAddr(t *testing.T) {
	data, err := os.ReadFile("riscv-elf-tests/rv64ui-p-add")
	if err != nil {
		t.Skip("riscv-elf-tests not present")
	}

	// tohost must be found
	addr, ok := FindSymbolAddr(data, "tohost")
	if !ok {
		t.Fatal("tohost symbol not found")
	}
	if addr == 0 {
		t.Fatal("tohost address is 0")
	}
	t.Logf("tohost = 0x%x", addr)

	// fromhost should also exist
	addr2, ok2 := FindSymbolAddr(data, "fromhost")
	if !ok2 {
		t.Error("fromhost not found")
	}
	t.Logf("fromhost = 0x%x", addr2)

	// nonexistent symbol returns false
	_, ok3 := FindSymbolAddr(data, "no_such_symbol_xyz")
	if ok3 {
		t.Error("unexpected match for nonexistent symbol")
	}

	// empty/garbage data returns false
	_, ok4 := FindSymbolAddr([]byte{}, "tohost")
	if ok4 {
		t.Error("expected false for empty data")
	}
}
