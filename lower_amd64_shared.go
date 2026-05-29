package riscv

// lower_amd64_shared.go — Types and helpers shared across AMD64 lowerers.

import (
	"sort"

	"github.com/glycerine/riscv-emu-golang/goasm"
	"github.com/glycerine/riscv-emu-golang/goasm/obj"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/x86"
)

// ── Per-VReg interval lookup ──

type regEntry struct {
	start, end int
	host       int16
}

type regIndex [][]regEntry

type regEntriesByStart []regEntry

func (s regEntriesByStart) Len() int           { return len(s) }
func (s regEntriesByStart) Less(i, j int) bool { return s[i].start < s[j].start }
func (s regEntriesByStart) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func buildRegIndex(alloc *Allocation) regIndex {
	maxVR := len(alloc.Kind)
	idx := make(regIndex, maxVR)

	counts := make([]int, maxVR)
	for i := range alloc.IntervalMap {
		vr := int(alloc.IntervalMap[i].Interval.VReg)
		if vr < maxVR {
			counts[vr]++
		}
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	flat := make([]regEntry, total)

	off := 0
	for vr, c := range counts {
		if c > 0 {
			idx[vr] = flat[off : off : off+c]
			off += c
		}
	}

	for i := range alloc.IntervalMap {
		ia := &alloc.IntervalMap[i]
		vr := int(ia.Interval.VReg)
		if vr < maxVR {
			idx[vr] = append(idx[vr], regEntry{
				start: ia.Interval.Start,
				end:   ia.Interval.End,
				host:  ia.Host,
			})
		}
	}

	for vr := range idx {
		entries := idx[vr]
		if len(entries) > 1 {
			sort.Sort(regEntriesByStart(entries))
		}
	}
	return idx
}

func (ri regIndex) lookup(v VReg, idx int) int16 {
	vr := int(v)
	if vr >= len(ri) {
		return -1
	}
	entries := ri[vr]
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if entries[mid].end < idx {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(entries) && entries[lo].start <= idx && idx <= entries[lo].end {
		return entries[lo].host
	}
	return -1
}

// ── Chain exit / JALR IC types ──

type chainExitInfo struct {
	targetPC uint64
	movProg  *obj.Prog
	stubProg *obj.Prog
}

type ChainExitDesc struct {
	TargetPC uint64
	MovProg  *obj.Prog
	StubProg *obj.Prog
}

type jalrICInfo struct {
	siteIdx  int
	pcMov    [2]*obj.Prog
	fnMov    [2]*obj.Prog
	jeq0Prog *obj.Prog
	jne1Prog *obj.Prog
	hit0Prog *obj.Prog
	stubProg *obj.Prog
}

type JalrICDesc struct {
	SiteIdx  int
	PcMov    [2]*obj.Prog
	FnMov    [2]*obj.Prog
	StubProg *obj.Prog
}

type GocallResumeDesc struct {
	AddrMov    *obj.Prog // MOVABS sentinel holding the resume address
	ResumeProg *obj.Prog // NOP at the resume point
}

type LowerResult struct {
	ChainEntryProg *obj.Prog
	ChainExits     []ChainExitDesc
	JalrICs        []JalrICDesc
	GocallResumes  []GocallResumeDesc
}

// ── Shared helpers ──

func isXMMReg(r int16) bool {
	return r >= goasm.REG_AMD64_X0 && r <= goasm.REG_AMD64_X15
}

func byteReg(r int16) int16 {
	return r - 16
}

func loadOp(t Type) obj.As {
	switch t {
	case I8:
		return x86.AMOVBQZX
	case I16:
		return x86.AMOVWQZX
	case I32:
		return x86.AMOVL
	case I64:
		return x86.AMOVQ
	case F32:
		return x86.AMOVSS
	case F64:
		return x86.AMOVSD
	default:
		return x86.AMOVQ
	}
}

func storeOp(t Type) obj.As {
	switch t {
	case I8:
		return x86.AMOVB
	case I16:
		return x86.AMOVW
	case I32:
		return x86.AMOVL
	case I64:
		return x86.AMOVQ
	case F32:
		return x86.AMOVSS
	case F64:
		return x86.AMOVSD
	default:
		return x86.AMOVQ
	}
}

func predToSETcc(p Pred) obj.As {
	switch p {
	case EQ:
		return x86.ASETEQ
	case NE:
		return x86.ASETNE
	case LT:
		return x86.ASETLT
	case LE:
		return x86.ASETLE
	case GT:
		return x86.ASETGT
	case GE:
		return x86.ASETGE
	case LTU:
		return x86.ASETCS
	case LEU:
		return x86.ASETLS
	case GTU:
		return x86.ASETHI
	case GEU:
		return x86.ASETCC
	default:
		return x86.ASETEQ
	}
}

func predToJcc(p Pred) obj.As {
	switch p {
	case EQ:
		return x86.AJEQ
	case NE:
		return x86.AJNE
	case LT:
		return x86.AJLT
	case LE:
		return x86.AJLE
	case GT:
		return x86.AJGT
	case GE:
		return x86.AJGE
	case LTU:
		return x86.AJCS
	case LEU:
		return x86.AJLS
	case GTU:
		return x86.AJHI
	case GEU:
		return x86.AJCC
	default:
		return x86.AJEQ
	}
}

func predToFPSETcc(p Pred) obj.As {
	switch p {
	case LT:
		return x86.ASETCS
	case LE:
		return x86.ASETLS
	case GT:
		return x86.ASETHI
	case GE:
		return x86.ASETCC
	default:
		return x86.ASETEQ
	}
}
