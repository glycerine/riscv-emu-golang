//go:build libriscv

package bench

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	libriscvbench "github.com/glycerine/riscv-emu-golang/bench/libriscv"
)

func BenchmarkCPU_ZygoFib10_Libriscv(b *testing.B) {
	if !libriscvbench.HasTCCJIT() {
		b.Fatal("libriscv build does not have RISCV_BINARY_TRANSLATION + RISCV_LIBTCC enabled")
	}
	elfData := loadELFFrom(b, "ZYGO_ELF", "zygo.elf")
	args := []string{"bench/zygo.elf", "-c", zygoFib10Program}
	const memSize = uint64(16 << 30)
	const insnLimit = uint64(10_000_000_000)

	b.ReportAllocs()
	b.ResetTimer()

	totalInsns := uint64(0)
	for i := 0; i < b.N; i++ {
		m := libriscvbench.NewMachineRealWriteWithArgs(elfData, memSize, args, true)
		if m == nil {
			b.Fatal("NewMachineRealWriteWithArgs returned nil")
		}
		allowLibriscvZygoZoneFiles(m)
		insns := m.RunToCompletion(insnLimit)
		code := m.ReturnValue()
		m.Close()
		if insns == 0 {
			b.Fatal("libriscv reported 0 instructions; program likely failed before completion")
		}
		if code != 0 {
			b.Fatalf("libriscv zygo exited with code %d, want 0", code)
		}
		totalInsns += insns
	}

	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 && totalInsns > 0 {
		b.ReportMetric(float64(totalInsns)/elapsed/1e6, "MIPS")
	}
	if b.N > 0 {
		b.ReportMetric(float64(totalInsns)/float64(b.N), "insns/op")
	}
}

func allowLibriscvZygoZoneFiles(m *libriscvbench.Machine) {
	for _, zone := range []string{
		"America/Los_Angeles",
		"America/New_York",
		"Europe/London",
	} {
		for _, root := range []string{
			"/usr/share/zoneinfo/",
			"/usr/share/lib/zoneinfo/",
			"/usr/lib/locale/TZ/",
			"/etc/zoneinfo",
		} {
			allowIfExists(m, filepath.ToSlash(filepath.Join(root, zone)))
		}
	}
	for _, goroot := range uniqueLibriscvZygoStrings([]string{runtime.GOROOT(), "/usr/local/go"}) {
		if goroot == "" {
			continue
		}
		allowIfExists(m, filepath.ToSlash(filepath.Join(goroot, "lib", "time", "zoneinfo.zip")))
	}
}

func allowIfExists(m *libriscvbench.Machine, path string) {
	if _, err := os.Stat(path); err == nil {
		m.AllowFile(path)
	}
}

func uniqueLibriscvZygoStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
