package riscv

import (
	"os"
	"testing"
)

// TestFindTextSection verifies that FindTextSection correctly locates
// the `.text` section VA and size for each benchmark ELF. Expected
// values come from `llvm-objdump -h`.
func TestFindTextSection(t *testing.T) {
	cases := []struct {
		path      string
		wantVAddr uint64
		wantSize  uint64
	}{
		{
			path:      "bench/dhrystone.elf",
			wantVAddr: 0x1000,
			wantSize:  0xb5a,
		},
		{
			path:      "bench/coremark.elf",
			wantVAddr: 0x1000,
			wantSize:  0x1fbc,
		},
	}
	for _, c := range cases {
		data, err := os.ReadFile(c.path)
		if err != nil {
			t.Skipf("%s: %v (ELF not built; run make bench-setup)", c.path, err)
			continue
		}
		vaddr, size, ok := FindTextSection(data)
		if !ok {
			t.Errorf("%s: FindTextSection returned !ok", c.path)
			continue
		}
		if vaddr != c.wantVAddr {
			t.Errorf("%s: vaddr = 0x%x, want 0x%x", c.path, vaddr, c.wantVAddr)
		}
		if size != c.wantSize {
			t.Errorf("%s: size = 0x%x, want 0x%x", c.path, size, c.wantSize)
		}
	}
}
