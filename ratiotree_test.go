package riscv

import (
	"testing"
)

func Test_riscvCodeTree(t *testing.T) {
	rvAddrs := []riscvElemAddr{1, 2, 3, 4, 5}
	codetree := newRiscvCodeTree(rvAddrs, nil)

	vv("codetree = '%v'", codetree)

	// keyBetween(a, b *big.Rat) *big.Rat
	for it := codetree.tree.Min(); !it.Limit(); it = it.Next() {

	}

	// codetree.insertJustBefore
	// codetree.insertJustAfter
}
