package riscv

import "testing"

func TestLiveChainCompatibleRequiresMatchingTargetLiveIns(t *testing.T) {
	src := liveChainMeta{
		Enabled: true,
	}
	target := &compiledBlock{
		liveChainEntry: 0x2000,
		liveChain: liveChainMeta{
			Enabled: true,
		},
	}
	src.ValidExitArch[10] = true
	src.ArchHost[10] = 123
	src.ArchHostValid[10] = true
	target.liveChain.EntryLiveArch[10] = true
	target.liveChain.ArchHost[10] = 123
	target.liveChain.ArchHostValid[10] = true

	if !liveChainCompatible(src, target) {
		t.Fatal("compatible live-chain edge rejected")
	}

	target.liveChain.ArchHost[10] = 124
	if liveChainCompatible(src, target) {
		t.Fatal("live-chain edge with mismatched host register accepted")
	}
}

func TestLiveChainCompatibleRejectsDirtySource(t *testing.T) {
	src := liveChainMeta{
		Enabled:      true,
		HasDirtyArch: true,
	}
	target := &compiledBlock{
		liveChainEntry: 0x2000,
		liveChain: liveChainMeta{
			Enabled: true,
		},
	}
	if liveChainCompatible(src, target) {
		t.Fatal("dirty source live-chain edge accepted before dirty propagation exists")
	}
}
