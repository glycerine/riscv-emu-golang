package riscv

import (
	"encoding/binary"
	"os"
	"testing"
)

// TestFindExecLoads_BenchmarkELFs verifies that each benchmark ELF has
// exactly one PF_X PT_LOAD, covering .text's VA.
func TestFindExecLoads_BenchmarkELFs(t *testing.T) {
	cases := []struct {
		path      string
		wantVAddr uint64
		wantMinSz uint64 // memsz may round up past .text size
	}{
		{path: "bench/dhrystone.elf", wantVAddr: 0x1000, wantMinSz: 0xb5a},
		{path: "bench/coremark.elf", wantVAddr: 0x1000, wantMinSz: 0x1fbc},
	}
	for _, c := range cases {
		data, err := os.ReadFile(c.path)
		if err != nil {
			t.Skipf("%s: %v (ELF not built; run make bench-setup)", c.path, err)
			continue
		}
		loads, ok := FindExecLoads(data)
		if !ok {
			t.Errorf("%s: FindExecLoads !ok", c.path)
			continue
		}
		if len(loads) != 1 {
			t.Errorf("%s: got %d exec loads, want 1; loads=%+v", c.path, len(loads), loads)
			continue
		}
		if loads[0].VAddr != c.wantVAddr {
			t.Errorf("%s: VAddr=0x%x, want 0x%x", c.path, loads[0].VAddr, c.wantVAddr)
		}
		if loads[0].MemSz < c.wantMinSz {
			t.Errorf("%s: MemSz=0x%x, want >=0x%x", c.path, loads[0].MemSz, c.wantMinSz)
		}
		if loads[0].Writable {
			t.Errorf("%s: Writable=true, want false for R-X .text", c.path)
		}
	}
}

// TestFindExecLoads_BuildELF verifies FindExecLoads on a synthetic
// single-R-X ELF produced by BuildELF (PF_R|PF_X = 5).
func TestFindExecLoads_BuildELF(t *testing.T) {
	const va = uint64(0x10000)
	code := []uint32{0x00000013} // NOP
	data := BuildELF(va, code)

	loads, ok := FindExecLoads(data)
	if !ok {
		t.Fatal("FindExecLoads !ok on BuildELF output")
	}
	if len(loads) != 1 {
		t.Fatalf("got %d loads, want 1; %+v", len(loads), loads)
	}
	if loads[0].VAddr != va {
		t.Errorf("VAddr=0x%x, want 0x%x", loads[0].VAddr, va)
	}
	if loads[0].MemSz != uint64(len(code)*4) {
		t.Errorf("MemSz=%d, want %d", loads[0].MemSz, len(code)*4)
	}
	if loads[0].Writable {
		t.Error("Writable=true, want false (PF_R|PF_X has no PF_W)")
	}
}

// TestFindExecLoads_MultiSegment verifies FindExecLoads on a hand-crafted
// ELF with two R-X PT_LOADs and one R-W PT_LOAD. Expect two exec loads.
func TestFindExecLoads_MultiSegment(t *testing.T) {
	data := buildMultiLoadELF(t, []phdrSpec{
		{vaddr: 0x1000, size: 0x40, flags: pfR | pfX},           // .text
		{vaddr: 0x2000, size: 0x20, flags: pfR | pfW},           // .data (non-exec)
		{vaddr: 0x3000, size: 0x80, flags: pfR | pfX | pfW},     // JIT-style R-W-X
	})

	loads, ok := FindExecLoads(data)
	if !ok {
		t.Fatal("FindExecLoads !ok on multi-load ELF")
	}
	if len(loads) != 2 {
		t.Fatalf("got %d exec loads, want 2; loads=%+v", len(loads), loads)
	}
	if loads[0].VAddr != 0x1000 || loads[0].MemSz != 0x40 || loads[0].Writable {
		t.Errorf("loads[0] = %+v, want {0x1000, 0x40, false}", loads[0])
	}
	if loads[1].VAddr != 0x3000 || loads[1].MemSz != 0x80 || !loads[1].Writable {
		t.Errorf("loads[1] = %+v, want {0x3000, 0x80, true}", loads[1])
	}
}

// TestFindExecLoads_Invalid verifies graceful rejection of malformed input.
func TestFindExecLoads_Invalid(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"too short", []byte{0x7f, 'E', 'L', 'F'}},
		{"wrong magic", make([]byte, 128)},
		{"all zero", make([]byte, 4096)},
	}
	for _, c := range cases {
		loads, ok := FindExecLoads(c.data)
		if ok || loads != nil {
			t.Errorf("%s: got (loads=%+v, ok=%v), want (nil, false)", c.name, loads, ok)
		}
	}
}

// phdrSpec describes one PT_LOAD entry for buildMultiLoadELF.
type phdrSpec struct {
	vaddr uint64
	size  uint64
	flags uint32
}

// buildMultiLoadELF produces a minimal ELF64 with multiple PT_LOAD
// entries at the given VAs. No file content for the segments (p_filesz=0);
// only the program header table is populated. Sufficient for exercising
// FindExecLoads, which only reads phdrs.
func buildMultiLoadELF(t *testing.T, specs []phdrSpec) []byte {
	t.Helper()
	le := binary.LittleEndian

	const (
		ehSize    = 64
		phEntSize = 56
	)
	phOff := uint64(ehSize)
	total := ehSize + phEntSize*len(specs)

	buf := make([]byte, total)

	copy(buf[0:], "\x7fELF")
	buf[4], buf[5], buf[6] = 2, 1, 1              // ELFCLASS64, ELFDATA2LSB, EV_CURRENT
	le.PutUint16(buf[16:], 2)                      // ET_EXEC
	le.PutUint16(buf[18:], 0xF3)                   // EM_RISCV
	le.PutUint32(buf[20:], 1)                      // e_version
	le.PutUint64(buf[24:], specs[0].vaddr)         // e_entry
	le.PutUint64(buf[32:], phOff)                  // e_phoff
	le.PutUint16(buf[52:], ehSize)                 // e_ehsize
	le.PutUint16(buf[54:], phEntSize)              // e_phentsize
	le.PutUint16(buf[56:], uint16(len(specs)))     // e_phnum

	for i, s := range specs {
		off := int(phOff) + i*phEntSize
		ph := buf[off:]
		le.PutUint32(ph[0:], ptLoad)
		le.PutUint32(ph[4:], s.flags)
		// p_offset = 0 (no file bytes for these synthetic loads)
		le.PutUint64(ph[16:], s.vaddr) // p_vaddr
		le.PutUint64(ph[24:], s.vaddr) // p_paddr
		// p_filesz = 0
		le.PutUint64(ph[40:], s.size)  // p_memsz
	}
	return buf
}
