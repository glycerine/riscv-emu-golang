package riscv

// RunCached is a fast-path dispatch loop that uses a DecoderCache to avoid
// re-fetching and re-detecting RVC on every instruction. Semantically
// identical to RunWithChain(cpu, nc), including cycle-counter increments,
// watchAddr polling, and NoteChain exception delivery.
//
// PCs outside the cache range fall back to cpu.step() which performs its
// own fetch. PCs inside the cache range pay one fetch (on first visit) and
// dispatch directly thereafter.
// pollBatch is how many instructions run between watchAddr polls.
// 1024 ≈ 4 µs at 250 MIPS — negligible latency for tohost-style exit,
// while removing the per-instruction polling overhead from the hot loop.
const pollBatch = 1024

func RunCached(cpu *CPU, cache *DecoderCache, nc *NoteChain) error {
	// Hoist pc into a local so the inner loop never touches cpu.pc on the hot
	// path. cpu.pc is only written when we exit the inner loop (watchAddr /
	// note delivery) or when a fallback into cpu.step() needs it.
	pc := cpu.pc
	for {
		var err error
		var cycles uint64
		countdown := pollBatch
		slot := cache.lookup(pc)
	inner:
		for {
			// Single load of slot.len drives three-way dispatch. The sentinel
			// slot for OOB PCs has len==0, which funnels through the default
			// arm into slowStep (same code path as an un-populated cached
			// slot).
			switch slot.len {
			case 2:
				pc, err = cpu.execRVCSlot(slot, pc)
			case 4:
				pc, err = cpu.exec32Slot(slot, pc)
			default:
				pc, err = slowStep(cpu, cache, slot, pc)
			}
			cycles++
			countdown--
			if err != nil || countdown == 0 {
				break inner
			}
			// Slot chaining: non-block-end insns pre-resolve their successor
			// at decode time, so we can skip cache.lookup entirely on the
			// ~85-90% of instructions that fall through linearly. Nil next
			// means "branch/jump/trap or slot is uninitialized" — do a full
			// lookup to find where control flow landed.
			if slot.next != nil {
				slot = slot.next
			} else {
				slot = cache.lookup(pc)
			}
		}
		cpu.cycle += cycles
		cpu.pc = pc

		if cpu.watchAddr != 0 {
			if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
				panic(&ExitError{Code: tohostExitCode(v)})
			}
		}
		if err == nil {
			continue
		}
		n := noteFromStepErr(err, cpu.PC())
		switch nc.Deliver(cpu, n) {
		case NoteHandled:
			// Handler may have advanced cpu.pc (e.g. ECALL returns to next
			// instruction). Reload so the inner loop resumes from the right
			// PC.
			pc = cpu.pc
			continue
		default:
			return err
		}
	}
}

// slowStep handles the cold paths: PCs outside the cache range (sentinel
// slot) or slots that aren't yet populated. Returns the new pc in addition
// to the error so the caller can keep pc in a local.
//
//go:noinline
func slowStep(cpu *CPU, cache *DecoderCache, slot *DecodedInsn, pc uint64) (uint64, error) {
	// Sentinel slot: pc is outside the cache range. Fall back to cpu.step().
	if slot == &cache.sentinel {
		cpu.pc = pc
		err := cpu.step()
		return cpu.pc, err
	}
	// slot.len == 0 (not yet decoded) — populate and dispatch.
	populateSlot(cpu, cache, slot, pc)
	if slot.len == 0 {
		cpu.pc = pc
		err := cpu.step()
		return cpu.pc, err
	}
	if slot.len == 2 {
		return cpu.execRVCSlot(slot, pc)
	}
	return cpu.exec32Slot(slot, pc)
}

// populateSlot fetches and records the instruction at pc. Leaves slot.len
// at 0 if the fetch faults (caller falls back to step() for fault delivery).
// For RVC instructions, additionally pre-decodes register fields and
// immediates so execRVCSlot can dispatch without re-extraction.
//
// After decoding, if the instruction is non-block-ending and the fall-through
// PC lands inside the cache range, wires slot.next to the successor slot so
// RunCached can skip cache.lookup on the linear-flow path.
//
//go:nosplit
func populateSlot(cpu *CPU, cache *DecoderCache, slot *DecodedInsn, pc uint64) {
	half, fh := (&cpu.mem).Fetch16(pc)
	if fh != nil {
		return // leave uninitialized; slow path handles the fault
	}
	if half&0x3 != 0x3 {
		decodeRVC(slot, half)
	} else {
		w, f := (&cpu.mem).Fetch32(pc)
		if f != nil {
			if f.Kind == FaultMisalign {
				w, f = (&cpu.mem).Fetch32U(pc)
			}
			if f != nil {
				return
			}
		}
		decodeInsn32(slot, w)
		// FENCE.I is not caught by decodeInsn32's opcode-level flagging — do it here.
		if slot.op == 0x0F && slot.funct3 == 0x1 {
			slot.flags |= flagBlockEnd
		}
	}
	// Wire up slot.next for non-block-end successors whose PC is in-range.
	// Block-ending insns leave next==nil so RunCached does a cache.lookup
	// after they execute (required to find the actual control-flow target).
	if slot.len > 0 && slot.flags&flagBlockEnd == 0 {
		succOff := pc + uint64(slot.len) - cache.base
		if succOff < cache.size {
			slot.next = &cache.slots[succOff>>1]
		}
	}
}
