package riscv

import "riscv/ir"

// u64set is a deterministic set of uint64 values.
// Iteration order is sorted by key. No generics.
type u64set struct {
	m     map[uint64]struct{}
	keys  []uint64
	dirty bool
}

func newU64set() u64set {
	return u64set{m: make(map[uint64]struct{})}
}

func (s *u64set) add(key uint64) {
	if _, ok := s.m[key]; !ok {
		s.m[key] = struct{}{}
		s.dirty = true
	}
}

func (s *u64set) has(key uint64) bool {
	_, ok := s.m[key]
	return ok
}

func (s *u64set) len() int {
	return len(s.m)
}

func (s *u64set) sortedKeys() []uint64 {
	if !s.dirty && len(s.keys) == len(s.m) {
		return s.keys
	}
	s.keys = s.keys[:0]
	for k := range s.m {
		s.keys = append(s.keys, k)
	}
	// Insertion sort — fast for small N.
	for i := 1; i < len(s.keys); i++ {
		key := s.keys[i]
		j := i - 1
		for j >= 0 && s.keys[j] > key {
			s.keys[j+1] = s.keys[j]
			j--
		}
		s.keys[j+1] = key
	}
	s.dirty = false
	return s.keys
}

// each iterates in deterministic sorted order.
func (s *u64set) each(fn func(key uint64)) {
	for _, k := range s.sortedKeys() {
		fn(k)
	}
}

// u64labelmap is a deterministic map from uint64 to ir.Label.
type u64labelmap struct {
	m     map[uint64]ir.Label
	keys  []uint64
	dirty bool
}

func newU64labelmap() u64labelmap {
	return u64labelmap{m: make(map[uint64]ir.Label)}
}

func (u *u64labelmap) set(key uint64, val ir.Label) {
	if _, ok := u.m[key]; !ok {
		u.dirty = true
	}
	u.m[key] = val
}

func (u *u64labelmap) get(key uint64) (ir.Label, bool) {
	v, ok := u.m[key]
	return v, ok
}
