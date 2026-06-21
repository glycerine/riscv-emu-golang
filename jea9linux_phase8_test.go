package riscv

import (
	"bytes"
	"os"
	"testing"
)

const (
	jea9TestSysMunmap   = uint64(215)
	jea9TestSysBrk      = uint64(214)
	jea9TestSysMmap     = uint64(222)
	jea9TestSysMprotect = uint64(226)
	jea9TestSysMincore  = uint64(232)
	jea9TestSysMadvise  = uint64(233)

	jea9TestProtRead  = uint64(1)
	jea9TestProtWrite = uint64(2)
	jea9TestProtExec  = uint64(4)

	jea9TestMapPrivate   = uint64(2)
	jea9TestMapFixed     = uint64(0x10)
	jea9TestMapAnonymous = uint64(0x20)
)

func requirePageAligned(t *testing.T, addr uint64) {
	t.Helper()
	if addr == 0 || addr%GuestPageSize != 0 {
		t.Fatalf("address 0x%x is not a nonzero page-aligned address", addr)
	}
}

func TestJea9Linux_BrkInitialGrowShrinkAndFault(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBrk, 0); d != NoteHandled {
		t.Fatalf("brk(0) disposition = %v", d)
	}
	initial := cpu.Reg(10)
	requirePageAligned(t, initial)

	grown := initial + GuestPageSize
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBrk, grown); d != NoteHandled {
		t.Fatalf("brk(grow) disposition = %v", d)
	}
	if got := cpu.Reg(10); got != grown {
		t.Fatalf("brk grow returned 0x%x, want 0x%x", got, grown)
	}
	if f := (&cpu.mem).Store8(grown-1, 0xAB); f != nil {
		t.Fatalf("store inside grown brk: %v", f)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBrk, initial); d != NoteHandled {
		t.Fatalf("brk(shrink) disposition = %v", d)
	}
	if got := cpu.Reg(10); got != initial {
		t.Fatalf("brk shrink returned 0x%x, want 0x%x", got, initial)
	}
	if f := (&cpu.mem).Store8(grown-1, 0xCD); f == nil {
		t.Fatal("store beyond shrunken brk succeeded, want VM fault")
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysBrk, grown); d != NoteHandled {
		t.Fatalf("brk(regrow) disposition = %v", d)
	}
	b, f := (&cpu.mem).Load8(grown - 1)
	if f != nil {
		t.Fatalf("load inside regrown brk: %v", f)
	}
	if b != 0 {
		t.Fatalf("regrown brk byte = 0x%x, want zero fill", b)
	}
}

func TestJea9Linux_MmapAnonymousFixedNoOverlap(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	length := uint64(2 * GuestPageSize)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, length, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap anon disposition = %v", d)
	}
	first := cpu.Reg(10)
	requirePageAligned(t, first)
	if f := (&cpu.mem).Store8(first, 0x11); f != nil {
		t.Fatalf("store first mapping: %v", f)
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, length, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("second mmap anon disposition = %v", d)
	}
	second := cpu.Reg(10)
	requirePageAligned(t, second)
	if second >= first && second < first+length {
		t.Fatalf("second mapping 0x%x overlaps first [0x%x,0x%x)", second, first, first+length)
	}

	fixed := uint64(0x02000000)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, fixed, GuestPageSize, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous|jea9TestMapFixed, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("fixed mmap disposition = %v", d)
	}
	if got := cpu.Reg(10); got != fixed {
		t.Fatalf("fixed mmap returned 0x%x, want 0x%x", got, fixed)
	}
}

func TestJea9Linux_MunmapFaultsAfterUnmap(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap disposition = %v", d)
	}
	addr := cpu.Reg(10)
	if f := (&cpu.mem).Store8(addr, 0x22); f != nil {
		t.Fatalf("store mapped page: %v", f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMunmap, addr, GuestPageSize); d != NoteHandled {
		t.Fatalf("munmap disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if _, f := (&cpu.mem).Load8(addr); f == nil {
		t.Fatal("load after munmap succeeded, want VM fault")
	}
	if f := (&cpu.mem).Store8(addr, 0x33); f == nil {
		t.Fatal("store after munmap succeeded, want VM fault")
	}
}

func TestJea9Linux_MmapProtNoneReserveCanBeMprotected(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, 0, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap PROT_NONE disposition = %v", d)
	}
	addr := cpu.Reg(10)
	requirePageAligned(t, addr)
	if j.vm.rangeFree(addr, GuestPageSize) {
		t.Fatalf("PROT_NONE mapping 0x%x is free to mmap allocator, want reserved", addr)
	}
	if _, f := (&cpu.mem).Load8(addr); f == nil {
		t.Fatal("load from PROT_NONE mapping succeeded, want VM fault")
	}
	if f := (&cpu.mem).Store8(addr, 0x77); f == nil {
		t.Fatal("store to PROT_NONE mapping succeeded, want VM fault")
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMprotect, addr, GuestPageSize, jea9TestProtRead|jea9TestProtWrite); d != NoteHandled {
		t.Fatalf("mprotect PROT_NONE to RW disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if f := (&cpu.mem).Store8(addr, 0x88); f != nil {
		t.Fatalf("store after PROT_NONE mprotect to RW: %v", f)
	}
}

func TestJea9Linux_MmapProtNoneReserveZeroesOnCommit(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("initial mmap disposition = %v", d)
	}
	addr := cpu.Reg(10)
	if f := (&cpu.mem).Store8(addr, 0x77); f != nil {
		t.Fatalf("store initial mapping: %v", f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMunmap, addr, GuestPageSize); d != NoteHandled {
		t.Fatalf("munmap disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, addr, GuestPageSize, 0, jea9TestMapPrivate|jea9TestMapAnonymous|jea9TestMapFixed, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("fixed PROT_NONE mmap disposition = %v", d)
	}
	if got := cpu.Reg(10); got != addr {
		t.Fatalf("fixed PROT_NONE mmap returned 0x%x, want 0x%x", got, addr)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMprotect, addr, GuestPageSize, jea9TestProtRead|jea9TestProtWrite); d != NoteHandled {
		t.Fatalf("mprotect disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	got, f := (&cpu.mem).Load8(addr)
	if f != nil {
		t.Fatalf("load after commit: %v", f)
	}
	if got != 0 {
		t.Fatalf("committed PROT_NONE reservation byte = 0x%x, want zero", got)
	}
}

func TestJea9Linux_MprotectReadOnlyRejectsStoreAndExecMetadata(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap disposition = %v", d)
	}
	addr := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMprotect, addr, GuestPageSize, jea9TestProtRead); d != NoteHandled {
		t.Fatalf("mprotect read disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if _, f := (&cpu.mem).Load8(addr); f != nil {
		t.Fatalf("read from read-only mapping faulted: %v", f)
	}
	if f := (&cpu.mem).Store8(addr, 0x44); f == nil {
		t.Fatal("store to read-only mapping succeeded, want VM fault")
	}

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMprotect, addr, GuestPageSize, jea9TestProtRead|jea9TestProtExec); d != NoteHandled {
		t.Fatalf("mprotect exec disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if r := (&cpu.mem).FindExecRegion(addr); r == nil || !r.Contains(addr) {
		t.Fatalf("exec metadata missing for 0x%x", addr)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMprotect, addr, GuestPageSize, jea9TestProtRead|jea9TestProtWrite); d != NoteHandled {
		t.Fatalf("mprotect rw disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if r := (&cpu.mem).FindExecRegion(addr); r != nil {
		t.Fatalf("exec metadata remained after dropping PROT_EXEC: %+v", r)
	}
}

func TestJea9Linux_PageZeroFaults(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if _, f := (&cpu.mem).Load8(0); f == nil {
		t.Fatal("null load succeeded, want VM fault")
	}
	if f := (&cpu.mem).Store8(0, 1); f == nil {
		t.Fatal("null store succeeded, want VM fault")
	}
	if _, f := (&cpu.mem).Fetch16(0); f == nil {
		t.Fatal("null fetch succeeded, want VM fault")
	}
}

func TestJea9Linux_CachedLDPageZeroFaults(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const code = uint64(0x1000)
	if f := mem.Store32(code, 0x00003503); f != nil { // ld a0, 0(zero)
		t.Fatal(f)
	}
	cpu.SetPC(code)
	_, err := RunDefaultBudget(cpu, &cpu.Notes, 4)
	if err == nil {
		t.Fatal("cached LD from page zero succeeded, want MemFault")
	}
	if fault, ok := err.(*MemFault); !ok || fault.Addr != 0 || fault.Kind != FaultLoad {
		t.Fatalf("cached LD error = %#v, want page-zero load MemFault", err)
	}
}

func TestJea9Linux_CachedSDPageZeroFaults(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	const code = uint64(0x1000)
	if f := mem.Store32(code, 0x00a03023); f != nil { // sd a0, 0(zero)
		t.Fatal(f)
	}
	cpu.SetPC(code)
	cpu.SetReg(10, 0x1234)
	_, err := RunDefaultBudget(cpu, &cpu.Notes, 4)
	if err == nil {
		t.Fatal("cached SD to page zero succeeded, want MemFault")
	}
	if fault, ok := err.(*MemFault); !ok || fault.Addr != 0 || fault.Kind != FaultStore {
		t.Fatalf("cached SD error = %#v, want page-zero store MemFault", err)
	}
}

func TestJea9Linux_MincoreAndMadvise(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	length := uint64(2 * GuestPageSize)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, length, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap disposition = %v", d)
	}
	addr := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMincore, addr, length, 0x5000); d != NoteHandled {
		t.Fatalf("mincore mapped disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	vec := readGuestBytes(t, mem, 0x5000, 2)
	if vec[0] != 1 || vec[1] != 1 {
		t.Fatalf("mincore vec = %v, want [1 1]", vec)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMadvise, addr, length, 0); d != NoteHandled {
		t.Fatalf("madvise disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, 0)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMunmap, addr+GuestPageSize, GuestPageSize); d != NoteHandled {
		t.Fatalf("munmap second page disposition = %v", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMincore, addr, length, 0x5000); d != NoteHandled {
		t.Fatalf("mincore unmapped disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -12)
}

func TestJea9Linux_VMOverlayScopedToInstall(t *testing.T) {
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	cpu := NewCPU(*mem)

	if _, f := (&cpu.mem).Load8(0); f != nil {
		t.Fatalf("pre-install null load faulted: %v", f)
	}
	j := NewJea9Linux(Jea9LinuxOptions{})
	cleanup := InstallJea9Linux(cpu, j)
	if _, f := (&cpu.mem).Load8(0); f == nil {
		t.Fatal("installed jea9linux null load succeeded, want VM fault")
	}
	cleanup()
	if _, f := (&cpu.mem).Load8(0); f != nil {
		t.Fatalf("post-cleanup null load faulted: %v", f)
	}
}

func TestJea9Linux_InitELFStackReservesStackMapping(t *testing.T) {
	cpu, mem, elf := loadTinyELFForStack(t)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{})
	const stackTop = uint64(0x03F00000)
	if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     []string{"/tiny"},
		ExecPath: "/tiny",
		StackTop: stackTop,
	}); err != nil {
		t.Fatalf("InitELFStack: %v", err)
	}

	vm := j.ensureVM(cpu)
	vectorPage := jea9LinuxAlignDown(cpu.Reg(2))
	if vm.rangeFree(vectorPage, GuestPageSize) {
		t.Fatalf("initial vector page 0x%x is free to mmap allocator, want reserved stack mapping", vectorPage)
	}
	lowerStackPage := jea9LinuxAlignDown(stackTop - Size1MB/2)
	if vm.rangeFree(lowerStackPage, GuestPageSize) {
		t.Fatalf("lower stack page 0x%x is free to mmap allocator, want reserved stack mapping", lowerStackPage)
	}
}

func TestJea9Linux_VMSyscallBuffersRespectProtection(t *testing.T) {
	var out bytes.Buffer
	j := NewJea9Linux(Jea9LinuxOptions{
		Stdin:  bytes.NewBufferString("abc"),
		Stdout: &out,
	})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, jea9TestProtRead, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap read-only disposition = %v", d)
	}
	readOnly := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysRead, 0, readOnly, 3); d != NoteHandled {
		t.Fatalf("read into read-only disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -14)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap write-only disposition = %v", d)
	}
	writeOnly := cpu.Reg(10)
	if f := (&cpu.mem).Store8(writeOnly, 'x'); f != nil {
		t.Fatalf("store write-only setup: %v", f)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysWrite, 1, writeOnly, 1); d != NoteHandled {
		t.Fatalf("write from write-only disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -14)
}

func TestJea9Linux_VMInvalidRangeErrors(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{})
	cpu, mem := newJea9LinuxSyscallCPU(t, j)
	defer mem.Free()

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 123, GuestPageSize, jea9TestProtRead, jea9TestMapPrivate|jea9TestMapAnonymous|jea9TestMapFixed, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("unaligned fixed mmap disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -22)

	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMmap, 0, GuestPageSize, jea9TestProtRead|jea9TestProtWrite, jea9TestMapPrivate|jea9TestMapAnonymous, ^uint64(0), 0); d != NoteHandled {
		t.Fatalf("mmap disposition = %v", d)
	}
	addr := cpu.Reg(10)
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMunmap, addr, GuestPageSize); d != NoteHandled {
		t.Fatalf("munmap disposition = %v", d)
	}
	if d := invokeJea9LinuxSyscall(cpu, jea9TestSysMprotect, addr, GuestPageSize, jea9TestProtRead); d != NoteHandled {
		t.Fatalf("mprotect unmapped disposition = %v", d)
	}
	requireSyscallReturn(t, cpu, -12)
}

func TestJea9Linux_Phase8VMELFFixtures(t *testing.T) {
	for _, path := range []string{
		"testvectors/jea9linux/elf/brk_basic.elf",
		"testvectors/jea9linux/elf/mmap_rw.elf",
		"testvectors/jea9linux/elf/mprotect_ro.elf",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			mem, err := NewGuestMemory(Size64MB)
			if err != nil {
				t.Fatal(err)
			}
			defer mem.Free()
			elf, err := LoadELFBytes(mem, data)
			if err != nil {
				t.Fatalf("LoadELFBytes: %v", err)
			}
			cpu := NewCPU(*mem)
			cpu.SetPC(elf.Entry)
			cpu.SetReg(2, 0x03F00000)
			j := NewJea9Linux(Jea9LinuxOptions{})
			if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{StackTop: 0x03F00000}); err != nil {
				t.Fatalf("InitELFStack: %v", err)
			}
			code, err := RunWithJea9LinuxInterp(cpu, j)
			if err != nil {
				t.Fatalf("RunWithJea9LinuxInterp: %v", err)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
		})
	}
}
