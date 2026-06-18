package riscv

import "testing"

func TestMMU_Sv39Translates4KiBPage(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const (
		root  = uint64(0x1000)
		l1    = uint64(0x2000)
		l0    = uint64(0x3000)
		phys  = uint64(0x5000)
		virt  = uint64(0x400000)
		value = uint32(0xfeedface)
	)
	writePTE(t, mem, root+vpnIndex(virt, 2)*8, tablePTE(l1))
	writePTE(t, mem, l1+vpnIndex(virt, 1)*8, tablePTE(l0))
	writePTE(t, mem, l0+vpnIndex(virt, 0)*8, leafPTE(phys, pteR|pteW|pteX))
	if fault := mem.Store32(phys+0x38, value); fault != nil {
		t.Fatal(fault)
	}

	cpu := NewCPU(*mem)
	cpu.EnableMMU()
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.satp = sv39SATP(root)

	got, fault := cpu.load32(virt + 0x38)
	if fault != nil {
		t.Fatalf("load32 translated fault: %v", fault)
	}
	if got != value {
		t.Fatalf("load32 translated = 0x%x, want 0x%x", got, value)
	}
	if fault := cpu.store32(virt+0x3c, 0x12345678); fault != nil {
		t.Fatalf("store32 translated: %v", fault)
	}
	if got, fault := mem.Load32(phys + 0x3c); fault != nil || got != 0x12345678 {
		t.Fatalf("physical store got 0x%x fault %v", got, fault)
	}
	pte, fault := mem.Load64(l0 + vpnIndex(virt, 0)*8)
	if fault != nil {
		t.Fatal(fault)
	}
	if pte&pteA == 0 || pte&pteD == 0 {
		t.Fatalf("leaf PTE A/D bits not set after load+store: 0x%x", pte)
	}
}

func TestMMU_Sv39TranslatesLargePages(t *testing.T) {
	mem, err := NewGuestMemory(Size4GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const root = uint64(0x1000)
	cases := []struct {
		name     string
		level    int
		virt     uint64
		physBase uint64
		offset   uint64
	}{
		{name: "2MiB", level: 1, virt: 0x0000000040000000, physBase: 0x00800000, offset: 0x12340},
		{name: "1GiB", level: 2, virt: 0x0000000080000000, physBase: 0x40000000, offset: 0x345678},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if fault := mem.ZeroRange(root, 0x3000); fault != nil {
				t.Fatal(fault)
			}
			if tc.level == 2 {
				writePTE(t, mem, root+vpnIndex(tc.virt, 2)*8, leafPTE(tc.physBase, pteR|pteW|pteX))
			} else {
				const l1 = uint64(0x2000)
				writePTE(t, mem, root+vpnIndex(tc.virt, 2)*8, tablePTE(l1))
				writePTE(t, mem, l1+vpnIndex(tc.virt, 1)*8, leafPTE(tc.physBase, pteR|pteW|pteX))
			}
			want := uint64(0xabcdef0123456789)
			if fault := mem.Store64(tc.physBase+tc.offset, want); fault != nil {
				t.Fatal(fault)
			}
			cpu := NewCPU(*mem)
			cpu.EnableMMU()
			cpu.SetPrivilegeMode(PrivSupervisor)
			cpu.satp = sv39SATP(root)
			got, fault := cpu.load64(tc.virt + tc.offset)
			if fault != nil {
				t.Fatalf("large page load fault: %v", fault)
			}
			if got != want {
				t.Fatalf("large page load = 0x%x, want 0x%x", got, want)
			}
		})
	}
}

func TestMMU_SatpWriteFlushesTLB(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const (
		rootA = uint64(0x1000)
		rootB = uint64(0x4000)
		l1A   = uint64(0x2000)
		l0A   = uint64(0x3000)
		l1B   = uint64(0x5000)
		l0B   = uint64(0x6000)
		virt  = uint64(0x400000)
		physA = uint64(0x8000)
		physB = uint64(0x9000)
	)
	writePTE(t, mem, rootA+vpnIndex(virt, 2)*8, tablePTE(l1A))
	writePTE(t, mem, l1A+vpnIndex(virt, 1)*8, tablePTE(l0A))
	writePTE(t, mem, l0A+vpnIndex(virt, 0)*8, leafPTE(physA, pteR|pteW))
	writePTE(t, mem, rootB+vpnIndex(virt, 2)*8, tablePTE(l1B))
	writePTE(t, mem, l1B+vpnIndex(virt, 1)*8, tablePTE(l0B))
	writePTE(t, mem, l0B+vpnIndex(virt, 0)*8, leafPTE(physB, pteR|pteW))
	if fault := mem.Store32(physA, 0xaaaaaaaa); fault != nil {
		t.Fatal(fault)
	}
	if fault := mem.Store32(physB, 0xbbbbbbbb); fault != nil {
		t.Fatal(fault)
	}

	cpu := NewCPU(*mem)
	cpu.EnableMMU()
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.writeCSR(0x180, sv39SATP(rootA))
	got, fault := cpu.load32(virt)
	if fault != nil || got != 0xaaaaaaaa {
		t.Fatalf("rootA load got 0x%x fault %v", got, fault)
	}
	cpu.writeCSR(0x180, sv39SATP(rootB))
	got, fault = cpu.load32(virt)
	if fault != nil || got != 0xbbbbbbbb {
		t.Fatalf("rootB load after satp flush got 0x%x fault %v", got, fault)
	}
}

func TestMMU_SatpWriteRejectsUnsupportedMode(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	cpu := NewCPU(*mem)
	cpu.EnableMMU()

	unsupportedSv57 := (uint64(10) << 60) | 0x12345
	if !cpu.writeCSR(0x180, unsupportedSv57) {
		t.Fatal("unsupported satp mode should be WARL, not illegal")
	}
	if got := cpu.satp; got != 0 {
		t.Fatalf("satp after unsupported write from zero = %#x, want unchanged zero", got)
	}

	const root = uint64(0x4000)
	want := sv39SATP(root)
	if !cpu.writeCSR(0x180, want) {
		t.Fatal("Sv39 satp write rejected")
	}
	if got := cpu.satp; got != want {
		t.Fatalf("satp after Sv39 write = %#x, want %#x", got, want)
	}

	if !cpu.writeCSR(0x180, unsupportedSv57) {
		t.Fatal("unsupported satp mode should be WARL, not illegal")
	}
	if got := cpu.satp; got != want {
		t.Fatalf("satp after unsupported write = %#x, want unchanged %#x", got, want)
	}
}

func TestRunMachineBudget_DelegatesMMUPageFault(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	const (
		root    = uint64(0x1000)
		faultPC = uint64(0x400000)
		handler = uint64(0x8000)
	)
	cpu := NewCPU(*mem)
	cpu.EnableMMU()
	cpu.SetPrivilegeMode(PrivSupervisor)
	cpu.satp = sv39SATP(root)
	cpu.stvec = handler
	cpu.medeleg = uint64(1) << CauseInsnPageFault
	cpu.SetPC(faultPC)

	res, err := RunMachineBudget(cpu, &cpu.Notes, 1)
	if err != nil {
		t.Fatalf("RunMachineBudget: %v", err)
	}
	if res != RunBudgetExpired {
		t.Fatalf("RunMachineBudget result = %v, want RunBudgetExpired", res)
	}
	if cpu.PC() != handler {
		t.Fatalf("PC after delegated page fault = 0x%x, want handler 0x%x", cpu.PC(), handler)
	}
	if cpu.scause != CauseInsnPageFault || cpu.sepc != faultPC || cpu.stval != faultPC {
		t.Fatalf("supervisor trap scause=%d sepc=0x%x stval=0x%x", cpu.scause, cpu.sepc, cpu.stval)
	}
}

func writePTE(t *testing.T, mem *GuestMemory, addr, pte uint64) {
	t.Helper()
	if fault := mem.Store64(addr, pte); fault != nil {
		t.Fatal(fault)
	}
}

func vpnIndex(addr uint64, level int) uint64 {
	return (addr >> (12 + 9*uint(level))) & 0x1ff
}

func tablePTE(addr uint64) uint64 {
	return ((addr >> 12) << 10) | pteV
}

func leafPTE(addr uint64, flags uint64) uint64 {
	return ((addr >> 12) << 10) | flags | pteV
}

func sv39SATP(root uint64) uint64 {
	return (satpModeSv39 << 60) | (root >> 12)
}
