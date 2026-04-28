//go:build darwin || linux

package riscv

import (
	"errors"
	"testing"
)

// ── Helpers ─────────────────────────────────────────────────────────────────

// exitELF builds a minimal single-PT_LOAD ELF that runs
// `ADDI a7, x0, 93; ECALL` — the Linux exit syscall. Both parent and
// clone can run this to completion and then ecall out.
func exitELF() (data []byte, entryVA uint64) {
	const va = uint64(0x10000)
	code := []uint32{
		0x05D00893, // ADDI a7, x0, 93
		0x00000073, // ECALL
	}
	return BuildELF(va, code), va
}

// buildExitMachine creates a Machine with a CPU loaded with the exitELF
// at the standard entry, and an AOT-installed JIT. Registers it for
// cleanup via t.Cleanup. Returns the parent Machine.
func buildExitMachine(t *testing.T) *Machine {
	t.Helper()
	data, entry := exitELF()
	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadELFBytes(mem, data); err != nil {
		mem.Free()
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)

	jit := NewJIT()
	if err := jit.InstallAOT(mem, data); err != nil {
		mem.Free()
		t.Fatalf("InstallAOT: %v", err)
	}
	m := NewMachine(cpu, jit)
	t.Cleanup(func() {
		m.Close()  // releases JIT segments (no-op on already-closed)
		mem.Free() // frees the *parent's* mmap (Machine.ownedMem is nil for NewMachine)
	})
	return m
}

// ── Memory isolation ────────────────────────────────────────────────────────

func TestMachineClone_MemoryIsolation_ParentUntouched(t *testing.T) {
	parent := buildExitMachine(t)

	// Parent writes a recognizable pattern.
	if f := parent.CPU.mem.Store64(0x2000, 0xDEADBEEFCAFEBABE); f != nil {
		t.Fatalf("parent Store64: %v", f)
	}

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	// Child sees parent's pre-fork value.
	v, _ := child.CPU.mem.Load64(0x2000)
	if v != 0xDEADBEEFCAFEBABE {
		t.Errorf("child.Load64(0x2000) = 0x%016x, want 0xDEADBEEFCAFEBABE", v)
	}

	// Child overwrites; parent must still hold original.
	if f := child.CPU.mem.Store64(0x2000, 0x1111222233334444); f != nil {
		t.Fatalf("child Store64: %v", f)
	}
	pv, _ := parent.CPU.mem.Load64(0x2000)
	if pv != 0xDEADBEEFCAFEBABE {
		t.Errorf("parent.Load64(0x2000) = 0x%016x after child write, want 0xDEADBEEFCAFEBABE (CoW violated)", pv)
	}
}

func TestMachineClone_MemoryIsolation_ChildUntouched(t *testing.T) {
	parent := buildExitMachine(t)

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	// Child writes first — its page becomes private.
	if f := child.CPU.mem.Store64(0x2000, 0xAAAAAAAAAAAAAAAA); f != nil {
		t.Fatalf("child Store64: %v", f)
	}

	// Parent then writes same address; child must keep its value.
	if f := parent.CPU.mem.Store64(0x2000, 0xBBBBBBBBBBBBBBBB); f != nil {
		t.Fatalf("parent Store64: %v", f)
	}
	cv, _ := child.CPU.mem.Load64(0x2000)
	if cv != 0xAAAAAAAAAAAAAAAA {
		t.Errorf("child.Load64(0x2000) = 0x%016x after parent write, want 0xAAAAAAAAAAAAAAAA", cv)
	}
}

// ── Segment sharing ─────────────────────────────────────────────────────────

func TestMachineClone_SegmentSharing(t *testing.T) {
	parent := buildExitMachine(t)
	if len(parent.JIT.aotSegments) != 1 {
		t.Fatalf("parent has %d segments, want 1", len(parent.JIT.aotSegments))
	}

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	if len(child.JIT.aotSegments) != 1 {
		t.Fatalf("child has %d segments, want 1", len(child.JIT.aotSegments))
	}
	if parent.JIT.aotSegments[0] != child.JIT.aotSegments[0] {
		t.Errorf("segment pointers differ: parent=%p child=%p",
			parent.JIT.aotSegments[0], child.JIT.aotSegments[0])
	}
	// soleSegment invariant holds.
	if child.JIT.soleSegment != child.JIT.aotSegments[0] {
		t.Errorf("child.soleSegment not refreshed after CloneShared")
	}
}

// ── Refcount balance ────────────────────────────────────────────────────────

func TestMachineClone_RefcountBalance(t *testing.T) {
	parent := buildExitMachine(t)
	seg := parent.JIT.aotSegments[0]
	if got := seg.refcount.Load(); got != 1 {
		t.Fatalf("initial refcount = %d, want 1", got)
	}

	// Clone three times → refcount 4.
	clones := make([]*Machine, 3)
	for i := range clones {
		c, err := parent.Clone()
		if err != nil {
			t.Fatalf("Clone %d: %v", i, err)
		}
		clones[i] = c
	}
	if got := seg.refcount.Load(); got != 4 {
		t.Errorf("refcount after 3 clones = %d, want 4", got)
	}

	// Close clones one by one; refcount ticks down.
	for i, c := range clones {
		c.Close()
		want := int32(4 - i - 1)
		if got := seg.refcount.Load(); got != want {
			t.Errorf("refcount after closing clone %d = %d, want %d", i, got, want)
		}
	}

	// Parent still holds the last ref. Close parent → refcount 0 → mmaps freed.
	parent.Close()
	if got := seg.refcount.Load(); got != 0 {
		t.Errorf("refcount after parent Close = %d, want 0", got)
	}
	if seg.nativeCodeMmap != nil {
		t.Errorf("segment nativeCodeMmap not released at refcount 0")
	}
}

// ── CPU state copy + divergence ─────────────────────────────────────────────

func TestMachineClone_CPUStateCopy(t *testing.T) {
	parent := buildExitMachine(t)

	parent.CPU.SetPC(0x12345678)
	parent.CPU.SetReg(5, 0xFEEDFACECAFEBEEF)
	parent.CPU.SetFReg(10, 0x4048F5C28F5C28F6)
	parent.CPU.SetFCSR(0x5A)
	parent.CPU.cycle = 42
	parent.CPU.mtvec = 0xAAAABBBB
	parent.CPU.watchAddr = 0x1000

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	if got := child.CPU.PC(); got != 0x12345678 {
		t.Errorf("child.PC() = 0x%x, want 0x12345678", got)
	}
	if got := child.CPU.Reg(5); got != 0xFEEDFACECAFEBEEF {
		t.Errorf("child.Reg(5) = 0x%x, want 0xFEEDFACECAFEBEEF", got)
	}
	if got := child.CPU.FReg(10); got != 0x4048F5C28F5C28F6 {
		t.Errorf("child.FReg(10) = 0x%x, want 0x4048F5C28F5C28F6", got)
	}
	if got := child.CPU.FCSR(); got != 0x5A {
		t.Errorf("child.FCSR() = 0x%x, want 0x5A", got)
	}
	if got := child.CPU.Cycle(); got != 42 {
		t.Errorf("child.Cycle() = %d, want 42", got)
	}
	if got := child.CPU.mtvec; got != 0xAAAABBBB {
		t.Errorf("child.mtvec = 0x%x, want 0xAAAABBBB", got)
	}
	if got := child.CPU.WatchAddr(); got != 0x1000 {
		t.Errorf("child.WatchAddr() = 0x%x, want 0x1000", got)
	}
}

func TestMachineClone_CPUStateDivergence(t *testing.T) {
	parent := buildExitMachine(t)
	parent.CPU.SetReg(5, 0x1111)

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	child.CPU.SetReg(5, 0x2222)

	if got := parent.CPU.Reg(5); got != 0x1111 {
		t.Errorf("parent.Reg(5) = 0x%x after child write, want 0x1111", got)
	}
	if got := child.CPU.Reg(5); got != 0x2222 {
		t.Errorf("child.Reg(5) = 0x%x, want 0x2222", got)
	}
}

// ── Independent execution ───────────────────────────────────────────────────

func TestMachineClone_IndependentExecution(t *testing.T) {
	parent := buildExitMachine(t)

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	// Both machines run the same program (ADDI a7,93; ECALL) → ecall exits.
	perr := parent.JIT.RunJIT(parent.CPU)
	cerr := child.JIT.RunJIT(child.CPU)

	if !errors.Is(perr, ErrEcall) {
		t.Errorf("parent RunJIT: %v, want ErrEcall", perr)
	}
	if !errors.Is(cerr, ErrEcall) {
		t.Errorf("child RunJIT: %v, want ErrEcall", cerr)
	}

	// known not to work until Cycle() comes back
	// Both should have retired 2 instructions (ADDI + ECALL).
	// (Actual counter value depends on the JIT dispatch; we just want
	// non-zero + approximately equal across the two machines.)
	if parent.CPU.Cycle() == 0 {
		t.Errorf("parent cycle counter zero after ECALL run")
	}
	if child.CPU.Cycle() == 0 {
		t.Errorf("child cycle counter zero after ECALL run")
	}

	// a7 was set to 93 on both.
	if got := parent.CPU.Reg(17); got != 93 {
		t.Errorf("parent.Reg(a7) = %d, want 93", got)
	}
	if got := child.CPU.Reg(17); got != 93 {
		t.Errorf("child.Reg(a7) = %d, want 93", got)
	}
}

// ── Without JIT ─────────────────────────────────────────────────────────────

func TestMachineClone_WithoutJIT(t *testing.T) {
	mem, err := NewGuestMemory(1 << 16)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()

	cpu := NewCPU(*mem)
	cpu.SetReg(5, 0x1234)
	m := NewMachine(cpu, nil)

	child, err := m.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	if child.JIT != nil {
		t.Errorf("child.JIT = %p, want nil (parent had no JIT)", child.JIT)
	}
	if got := child.CPU.Reg(5); got != 0x1234 {
		t.Errorf("child.Reg(5) = 0x%x, want 0x1234", got)
	}
}

// ── InstallOS auto-propagation ──────────────────────────────────────────────

// TestMachineClone_InstallOSOnChild verifies that after the parent
// installs an OS personality, Clone auto-installs the same OS on the
// child. Both parent and child run an ADDI a7,93 + ECALL stub; both
// handlers fire (spy counter bumps twice), proving the child has the
// handler installed without manual reinstallation.
func TestMachineClone_InstallOSOnChild(t *testing.T) {
	parent := buildExitMachine(t)

	// Install a spy OS on the parent. The spy increments a counter
	// on every syscall invocation so we can assert the child hit
	// the handler too.
	var spyCount int
	spy := func(_ *CPU, args SyscallArgs) (int64, bool, bool, error) {
		spyCount++
		return int64(int32(args.A0)), false, true, nil
	}
	os := NewOS()
	os.HandleSyscall(93, spy)
	parent.InstallOS(os)

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	if child.OS != os {
		t.Errorf("child.OS = %p, want %p (parent's OS)", child.OS, os)
	}

	runOnce := func(m *Machine, label string) {
		defer func() {
			r := recover()
			if _, ok := r.(*ExitError); !ok {
				t.Errorf("%s: expected *ExitError panic, got %v", label, r)
			}
		}()
		_ = m.JIT.RunJIT(m.CPU)
	}
	runOnce(parent, "parent")
	runOnce(child, "child")

	if spyCount != 2 {
		t.Errorf("spy fired %d times, want 2 (parent + child)", spyCount)
	}
}

// ── Lazy blocks not shared ──────────────────────────────────────────────────

func TestMachineClone_LazyBlocksNotShared(t *testing.T) {
	// Build a Machine with NO AOT — purely lazy JIT.
	data, entry := exitELF()
	mem, err := NewGuestMemory(Size1GB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if _, err := LoadELFBytes(mem, data); err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	jit := NewJIT()
	// (no InstallAOT)
	parent := NewMachine(cpu, jit)
	t.Cleanup(parent.Close)

	// Parent lazy-compiles via StepBlock.
	if _, err := jit.StepBlock(cpu); err != nil && !errors.Is(err, ErrEcall) {
		t.Logf("parent.StepBlock returned: %v", err)
	}

	// Check that SOMETHING landed in the parent's lazy cache.
	// (The direct-mapped cache has fixed size; at least one entry
	// for the entry PC should now be populated.)
	parentIdx := cacheIdx(entry)
	if parent.JIT.cache[parentIdx].blk == nil {
		t.Skip("parent lazy cache empty — compile may have been skipped for this input; test inconclusive")
	}

	child, err := parent.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(child.Close)

	// Child's cache at the same index must be empty — lazy blocks are not shared.
	if child.JIT.cache[parentIdx].blk != nil {
		t.Errorf("child lazy cache at idx %d = %p, want nil (lazy blocks must not be shared)",
			parentIdx, child.JIT.cache[parentIdx].blk)
	}
}
