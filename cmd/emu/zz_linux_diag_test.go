package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
)

func TestZZLinuxHangDiag(t *testing.T) {
	const biosPath = "../../xendor/opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	const kernelPath = "../../xendor/linux/boot/vmlinuz-6.17.0-35-generic"
	const initrdPath = "../../xendor/linux/initramfs.cpio.gz"
	for _, path := range []string{biosPath, kernelPath, initrdPath} {
		if !fileExists(path) {
			t.Skipf("Linux BIOS boot fixture not present: %s", path)
		}
	}

	var stdout, stderr bytes.Buffer
	guest, err := prepareBiosGuest(EmuConfig{
		BiosPath:   biosPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Append:     linuxUARTBootArgs,
		Memory:     "512MB",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}.withDefaults())
	if err != nil {
		t.Fatalf("prepareBiosGuest: %v", err)
	}
	defer guest.mem.Free()

	const chunk = uint64(10_000_000)
	for used := uint64(0); used < 1_200_000_000; used += chunk {
		res, err := riscv.RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, chunk)
		if err != nil {
			t.Fatalf("run at used=%d: %v\nstate=%s\nstdout tail:\n%s\nstderr:%s", used, err, diagCPU(guest.cpu), tailString(stdout.String(), 4096), stderr.String())
		}
		if res == riscv.RunBudgetExit {
			t.Fatalf("exit at used=%d state=%s\nstdout tail:\n%s", used, diagCPU(guest.cpu), tailString(stdout.String(), 4096))
		}
		if strings.Contains(stdout.String(), "SBI misaligned access exception delegation ok") && used%100_000_000 == 0 {
			t.Logf("used=%d state=%s stdout tail:\n%s", used, diagGuest(guest), tailString(stdout.String(), 1200))
		}
	}
	t.Fatalf("expired state=%s\nstdout tail:\n%s\nstderr:%s", diagGuest(guest), tailString(stdout.String(), 4096), stderr.String())
}

func diagGuest(guest *biosGuest) string {
	cpu := guest.cpu
	pc := cpu.PC()
	phys, trace, tf := diagTranslate(guest.mem, cpuField(cpu, "satp"), pc)
	if tf != nil && pc >= 0xffffffff00000000 {
		phys = pc - 0xffffffff00000000
	}
	half, hf := guest.mem.Load16(phys)
	nextHalf, nhf := guest.mem.Load16(phys + 2)
	wordU := uint32(half) | uint32(nextHalf)<<16
	word, wf := guest.mem.Load32(phys &^ 3)
	mtime, mtf := guest.mem.Load64(biosCLINTBase + 0xbff8)
	mtimecmp, mtcf := guest.mem.Load64(biosCLINTBase + 0x4000)
	s10v, s10f := diagLoadVirtual64(guest, cpu.Reg(26))
	parts := []string{
		diagCPU(cpu),
		"phys=" + hex64(phys),
		"walk=" + trace,
		"walkerr=" + faultString(tf),
		"half=" + hexFault16(half, hf),
		"nextHalf=" + hexFault16(nextHalf, nhf),
		"wordU=" + hexFault32(wordU, firstFault(hf, nhf)),
		"word@align=" + hexFault32(word, wf),
		"near=" + diagHalfwords(guest.mem, phys-16, 12),
		"mtime=" + hexFault64(mtime, mtf),
		"mtimecmp=" + hexFault64(mtimecmp, mtcf),
		"ra=" + hex64(cpu.Reg(1)),
		"sp=" + hex64(cpu.Reg(2)),
		"gp=" + hex64(cpu.Reg(3)),
		"tp=" + hex64(cpu.Reg(4)),
		"t0=" + hex64(cpu.Reg(5)),
		"t1=" + hex64(cpu.Reg(6)),
		"t2=" + hex64(cpu.Reg(7)),
		"a0=" + hex64(cpu.Reg(10)),
		"a1=" + hex64(cpu.Reg(11)),
		"a2=" + hex64(cpu.Reg(12)),
		"a3=" + hex64(cpu.Reg(13)),
		"a4=" + hex64(cpu.Reg(14)),
		"a5=" + hex64(cpu.Reg(15)),
		"a6=" + hex64(cpu.Reg(16)),
		"a7=" + hex64(cpu.Reg(17)),
		"s0=" + hex64(cpu.Reg(8)),
		"s1=" + hex64(cpu.Reg(9)),
		"s2=" + hex64(cpu.Reg(18)),
		"s3=" + hex64(cpu.Reg(19)),
		"s4=" + hex64(cpu.Reg(20)),
		"s5=" + hex64(cpu.Reg(21)),
		"s6=" + hex64(cpu.Reg(22)),
		"s7=" + hex64(cpu.Reg(23)),
		"s8=" + hex64(cpu.Reg(24)),
		"s9=" + hex64(cpu.Reg(25)),
		"s10=" + hex64(cpu.Reg(26)),
		"s10mem=" + hexFault64(s10v, s10f),
		"s11=" + hex64(cpu.Reg(27)),
	}
	return strings.Join(parts, " ")
}

func diagLoadVirtual64(guest *biosGuest, addr uint64) (uint64, *riscv.MemFault) {
	phys, _, f := diagTranslate(guest.mem, cpuField(guest.cpu, "satp"), addr)
	if f != nil {
		return 0, f
	}
	return guest.mem.Load64(phys)
}

func diagHalfwords(mem *riscv.GuestMemory, addr uint64, count int) string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		a := addr + uint64(i*2)
		v, f := mem.Load16(a)
		out = append(out, hex64(a)+":"+hexFault16(v, f))
	}
	return strings.Join(out, ",")
}

func diagTranslate(mem *riscv.GuestMemory, satp, addr uint64) (uint64, string, *riscv.MemFault) {
	mode := satp >> 60
	levels := 0
	switch mode {
	case 8:
		levels = 3
	case 9:
		levels = 4
	default:
		return addr, "bare", nil
	}
	vpn := [4]uint64{}
	for i := 0; i < levels; i++ {
		vpn[i] = (addr >> (12 + 9*uint(i))) & 0x1ff
	}
	pt := (satp & ((uint64(1) << 44) - 1)) << 12
	parts := []string{"root=" + hex64(pt)}
	for level := levels - 1; level >= 0; level-- {
		pteAddr := pt + vpn[level]*8
		pte, f := mem.Load64(pteAddr)
		parts = append(parts, "l"+hexN(uint64(level), 1)+"@"+hex64(pteAddr)+"="+hexFault64(pte, f))
		if f != nil {
			return 0, strings.Join(parts, ","), f
		}
		if pte&1 == 0 || (pte&4 != 0 && pte&2 == 0) {
			return 0, strings.Join(parts, ","), &riscv.MemFault{Addr: addr, Width: 1, Kind: riscv.FaultPageFetch}
		}
		if pte&(2|8) == 0 {
			pt = ((pte >> 10) & ((uint64(1) << 44) - 1)) << 12
			continue
		}
		ppn := (pte >> 10) & ((uint64(1) << 44) - 1)
		pageShift := uint(12 + 9*level)
		mask := (uint64(1) << pageShift) - 1
		return (ppn << 12 &^ mask) | (addr & mask), strings.Join(parts, ","), nil
	}
	return 0, strings.Join(parts, ","), &riscv.MemFault{Addr: addr, Width: 1, Kind: riscv.FaultPageFetch}
}

func diagCPU(cpu *riscv.CPU) string {
	return strings.Join([]string{
		"pc=" + hex64(cpu.PC()),
		"priv=" + hex64(cpuField(cpu, "priv")),
		"mstatus=" + hex64(cpuField(cpu, "mstatus")),
		"mip=" + hex64(cpuField(cpu, "mip")),
		"mie=" + hex64(cpuField(cpu, "mie")),
		"mideleg=" + hex64(cpuField(cpu, "mideleg")),
		"mtvec=" + hex64(cpuField(cpu, "mtvec")),
		"mcause=" + hex64(cpuField(cpu, "mcause")),
		"mepc=" + hex64(cpuField(cpu, "mepc")),
		"stvec=" + hex64(cpuField(cpu, "stvec")),
		"scause=" + hex64(cpuField(cpu, "scause")),
		"sepc=" + hex64(cpuField(cpu, "sepc")),
		"satp=" + hex64(cpuField(cpu, "satp")),
	}, " ")
}

func cpuField(cpu *riscv.CPU, name string) uint64 {
	v := reflect.ValueOf(cpu).Elem().FieldByName(name)
	return v.Uint()
}

func hexFault16(v uint16, f *riscv.MemFault) string {
	if f != nil {
		return f.Error()
	}
	return "0x" + hexN(uint64(v), 4)
}

func firstFault(fs ...*riscv.MemFault) *riscv.MemFault {
	for _, f := range fs {
		if f != nil {
			return f
		}
	}
	return nil
}

func hexFault32(v uint32, f *riscv.MemFault) string {
	if f != nil {
		return f.Error()
	}
	return "0x" + hexN(uint64(v), 8)
}

func hexFault64(v uint64, f *riscv.MemFault) string {
	if f != nil {
		return f.Error()
	}
	return hex64(v)
}

func faultString(f *riscv.MemFault) string {
	if f == nil {
		return "-"
	}
	return f.Error()
}

func hex64(v uint64) string {
	return "0x" + hexN(v, 16)
}

func hexN(v uint64, n int) string {
	const digits = "0123456789abcdef"
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		shift := uint(4 * (n - 1 - i))
		buf[i] = digits[(v>>shift)&0xf]
	}
	return string(buf)
}
