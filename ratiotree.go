package riscv

import (
	"fmt"
	"math/big"
	"riscv/ir"
	"strings"

	rb "github.com/glycerine/rbtree"
)

var _ = &ir.Block{}

// riscvElemAddr tracks the original guest RISCV code
// address from the ELF, for organization in a riscvCodeTree.
// The riscvCodeTree facilitates linear native code generation
// keeping labels in a 1:1/strictly monotonic mapping
// with the original RISCV guest code. We forbid code at 0.
// As a result we can always insert new keys before a
// a non-zero key.
//
// PRE: a critical pre-condition is that the user/caller
// must make sure that the address
// is unique among all other addresses in the riscvCodeTree.
// Hence if the JIT has created
// new code out of thin air, then the JIT must ensure
// that it is assigning a non-conflicting address to the new code.
// They may choose to cast a memory pointer for this, for
// instance. Any other uniqueness-ensuring scheme is viable oo.
// We do not care. It is always a simple int64 for us.
//
// Given a valid riscvElem != 0 (an int64 != 0) we
// can always insert before or after it.
type riscvElemAddr int64

type riscvCodeBasicBlock struct {
	key *big.Rat

	// rvAddr == 0 means not in guest RISCV code; this
	// is a synthetic block, made by the JIT.
	rvAddr riscvElemAddr

	payload fmt.Stringer
	// todo: convert payload to *ir.Block
	// blocking: does it have String() method?
}

type riscvCodeTree struct {
	tree *rb.Tree
}

func (tr *riscvCodeTree) String() (r string) {
	n := tr.tree.Len()
	if n == 0 {
		return "riscvCodeTree{}"
	}
	sb := &strings.Builder{}
	sb.WriteString("riscvCodeTree{\n")
	for it := tr.tree.Min(); !it.Limit(); it = it.Next() {
		bb := it.Item().(*riscvCodeBasicBlock)
		var pay string
		if bb.payload == nil || isNil(bb.payload) {
			pay = "<nil>"
		} else {
			pay = bb.payload.String()
		}
		fmt.Fprintf(sb, "[rvAddr: 0x%x]: %v\n", bb.rvAddr, pay)
	}
	sb.WriteString("}\n")
	return sb.String()
}

// newRiscvCodeTree creates and returns a new riscvCodeTree.
//
// PRE: if len(payloads) > 0, then len(payloads) must == len(rvAddrs)
// Note: payloads can be nil or an empty slice. This allows
// pre-creation of all addresses and payload addition later.
func newRiscvCodeTree(rvAddrs []riscvElemAddr, payloads []fmt.Stringer) (s *riscvCodeTree) {
	s = &riscvCodeTree{
		tree: rb.NewTree(func(a, b rb.Item) int {
			av := a.(*riscvCodeBasicBlock)
			bv := b.(*riscvCodeBasicBlock)
			if av == bv {
				return 0
			}
			return av.key.Cmp(bv.key)
		}),
	}
	for k, rva := range rvAddrs {
		var payload fmt.Stringer
		if len(payloads) > 0 {
			payload = payloads[k]
		}
		elem := &riscvCodeBasicBlock{
			key:     big.NewRat(int64(rva), 1),
			rvAddr:  rva,
			payload: payload,
		}
		s.tree.Insert(elem)
	}
	return
}

// disallow code residing at actual 0x0 address, to catch bugs.
// hence if we insert before any riscvElemAddr a > 0, we can
// always split the difference between a and zero to get
// our new insertion point. We support riscvElemAddr < 0
// too using negative rational big.Rat numbers.
var bigZero = big.NewRat(0, 1)
var bigOne = big.NewRat(1, 1)
var bigOneHalf = big.NewRat(1, 2)

func (s *riscvCodeTree) del(d riscvElemAddr) {
	//vv("del d=%v", d)
	dkey := big.NewRat(int64(d), 1)
	query := &riscvCodeBasicBlock{
		key: dkey,

		// rvAddr: this field must be left at 0.
		// The 0 => the rvAddr is not available b/c this is a
		// synthetic insertion.
		// synthetic addressed code is always stuff we the
		// GoCPU JIT have inserted. It does
		// not correspond directly to any original riscv guest address.
	}
	s.tree.DeleteWithKey(query)
}

// maybe should be?
//func (s *riscvCodeTree) insertJustBefore(beforeMe *riscvCodeBasicBlock, insertMe *riscvCodeBasicBlock) {

func (s *riscvCodeTree) insertJustBefore(w riscvElemAddr, payload fmt.Stringer) {

	// maybe want
	//wkey := beforeMe.key

	wkey := big.NewRat(int64(w), 1)
	query := &riscvCodeBasicBlock{
		key: wkey,
	}

	var key *big.Rat
	var prevBBkey *big.Rat

	it, exact := s.tree.FindGE_isEqual(query)
	_ = exact
	n := s.tree.Len()
	if it.Limit() {
		// there is nothing in tree >= query.  [tree0 ... treeN) query
		// So query is > everything in the tree.
		// Inserting before query is not well defined.
		// Only allow w == 0 here. Else error.
		if w != 0 {
			panicf("insertJustBefore(w) error: could not find anything in tree >= w(%v)", w)
		}
		// INVAR: w == 0
		if n == 0 {
			// empty tree, want to insert before 0. Insert at -1.
			// A little arbitrary. This is just our convention, for now.
			key = big.NewRat(-1, 1)
		} else {
			// non-empty, all negative tree (since all tree < 0),
			// want to insert between the negative key closest to 0 (largest key).
			curMaxKey := s.tree.Max().Item().(*riscvCodeBasicBlock).key
			key = keyBetween(curMaxKey, bigZero)
		}
		// INVAR: key is set.
	} else {
		// INVAR: still need to determine key. The iterator `it` is valid.

		ahead := it.Item().(*riscvCodeBasicBlock)

		prev := it.Prev()
		if prev.NegativeLimit() {
			// no prior item, split between here and zero if positive
			switch {
			case w > 0:
				prevBBkey = bigZero
			default:
				// w <= 0. Two cases:
				//
				// For w < 0: We cannot split between 0. Would
				// put us AFTER w. We want BEFORE w.
				//
				// For w == 0: put key at -1.
				// Split between w-2 and w either way.
				prevBBkey = big.NewRat(int64(w-2), 1)
			}
		} else {
			prevBBkey = prev.Item().(*riscvCodeBasicBlock).key
		}
		key = keyBetween(prevBBkey, ahead.key)
	}

	add := &riscvCodeBasicBlock{
		key:     key,
		payload: payload,
	}
	added := s.tree.Insert(add)
	if !added {
		panicf("why was not added to tree key '%v'?", key)
	}
}

// to insert between two existing keys
func keyBetween(a, b *big.Rat) *big.Rat {
	sum := new(big.Rat).Add(a, b)
	return sum.Quo(sum, big.NewRat(2, 1))
}

/*
// insertJustAfter(w)
//
// positive w cases:
//
// [tree0, ..., w, treeX, ... treeN): insert at min(w+1/2, (w + treeX)/2)
// [tree0, ..., w]: insert at w + 1/2
// w [tree0, ..., treeN): insert at min(w+1/2, (w+tree0)/2)
//
// negative w cases?
//
// .
func (s *riscvCodeTree) insertJustAfter(w riscvElemAddr, payload fmt.Stringer) {

	panic("unfinished converstion from insertJustBefore! finish first!")
	wkey := big.NewRat(int64(w), 1)
	query := &riscvCodeBasicBlock{
		key: wkey,
	}

	var key *big.Rat
	var nextBBkey *big.Rat

	it, exact := s.tree.FindLE_isEqual(query)
	_ = exact
	n := s.tree.Len()
	if it.NegativeLimit() {

		// there is nothing in tree <= query.  query [tree0 ... treeN)
		// So query is < everything in the tree.
		// Inserting after query is not well defined.

		// Only allow w == 0 here. Else error.
		if w != 0 {
			panicf("insertJustAfter(w) error: could not find anything in tree <= w(%v)", w)
		}
		// INVAR: w == 0
		if n == 0 {
			// empty tree, want to insert after 0. Insert at 0.5
			// A little arbitrary. This is just our convention, for now.
			key = bigOneHalf
		} else {
			// non-empty, all positive tree (since all tree > 0),
			// want to insert between the 0 and the positive key closest to 0 (minimum key).
			curMinKey := s.tree.Min().Item().(*riscvCodeBasicBlock).key
			// if curMinKey > 1, use 1/2 still.
			if curMinKey.Cmp(curMinKey, bigOne) > 0 {
				key = bigOneHalf
			} else {
				key = keyBetween(bigZero, curMinKey)
			}
		}
		// INVAR: key is set.
	} else {
		// INVAR: still need to determine key.
		// The iterator `it` is valid from FindLE().

		// INVAR: it <= w. Either way, insert at w + 1/2, if w > 0.

		// If w < 0, we want to insert just after w

		// here in insertJustAfter(w)

		behind := it.Item().(*riscvCodeBasicBlock)

		next := it.Next()
		if next.Limit() {
			// no next item. w is the last/largest item.
			// If w is positive, insert at w + 1/2.

			// If w is negative,
			// split between here and next integer if w positive
			// If w negative, split between here and zero.
			switch {
			case w > 0:
				nextBBkey = big.NewRat(int64(w+1), 1)
			default:
				// w <= 0. Two cases:
				//
				// For w < 0: We cannot split between 0. Would
				// put us AFTER w. We want BEFORE w.
				//
				// For w == 0: put key at -1.
				// Split between w-2 and w either way.
				nextBBkey = big.NewRat(int64(w-2), 1)
			}
		} else {
			nextBBkey = next.Item().(*riscvCodeBasicBlock).key
		}
		key = keyBetween(behind.key, nextBBkey)
	}

	add := &riscvCodeBasicBlock{
		key:     key,
		payload: payload,
	}
	added := s.tree.Insert(add)
	if !added {
		panicf("why was not added to tree key '%v'?", key)
	}
}
*/
