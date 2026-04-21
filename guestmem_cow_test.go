//go:build darwin || linux

package riscv

import (
	"testing"
)

func TestGuestMemory_CowClone_Basic(t *testing.T) {
	parent, err := NewGuestMemory(1 << 16) // 64 KB
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	t.Cleanup(parent.Free)

	// Parent writes a recognizable pattern.
	if f := parent.Store64(0x1000, 0xDEADBEEFCAFEBABE); f != nil {
		t.Fatalf("parent.Store64: %v", f)
	}

	child, err := parent.CowClone()
	if err != nil {
		t.Fatalf("CowClone: %v", err)
	}
	t.Cleanup(child.Free)

	// Child sees parent's pre-fork value.
	v, f := child.Load64(0x1000)
	if f != nil {
		t.Fatalf("child.Load64: %v", f)
	}
	if v != 0xDEADBEEFCAFEBABE {
		t.Errorf("child.Load64(0x1000)=0x%016x, want 0xDEADBEEFCAFEBABE", v)
	}

	// Child writes — parent unaffected.
	if f := child.Store64(0x1000, 0x1122334455667788); f != nil {
		t.Fatalf("child.Store64: %v", f)
	}
	pv, f := parent.Load64(0x1000)
	if f != nil {
		t.Fatalf("parent.Load64: %v", f)
	}
	if pv != 0xDEADBEEFCAFEBABE {
		t.Errorf("parent.Load64(0x1000)=0x%016x after child write, want 0xDEADBEEFCAFEBABE (CoW violated)", pv)
	}
	cv, _ := child.Load64(0x1000)
	if cv != 0x1122334455667788 {
		t.Errorf("child.Load64(0x1000)=0x%016x, want 0x1122334455667788", cv)
	}
}

func TestGuestMemory_CowClone_PreservesExecRegions(t *testing.T) {
	parent, err := NewGuestMemory(1 << 16)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	t.Cleanup(parent.Free)

	parent.AddExecRegion(0x1000, 0x2000, false)
	parent.AddExecRegion(0x3000, 0x4000, true)

	child, err := parent.CowClone()
	if err != nil {
		t.Fatalf("CowClone: %v", err)
	}
	t.Cleanup(child.Free)

	got := child.ExecRegions()
	if len(got) != 2 {
		t.Fatalf("child has %d exec regions, want 2", len(got))
	}
	if got[0].VAddrBegin != 0x1000 || got[0].VAddrEnd != 0x2000 || got[0].IsLikelyJIT {
		t.Errorf("child exec region[0] = %+v", got[0])
	}
	if got[1].VAddrBegin != 0x3000 || got[1].VAddrEnd != 0x4000 || !got[1].IsLikelyJIT {
		t.Errorf("child exec region[1] = %+v", got[1])
	}

	// Mutating the child's exec regions must not affect the parent.
	child.RemoveExecRegion(0x1000, 0x2000)
	if len(parent.ExecRegions()) != 2 {
		t.Errorf("parent exec regions mutated by child RemoveExecRegion: %d, want 2", len(parent.ExecRegions()))
	}
}
