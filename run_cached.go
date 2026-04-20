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
	for {
		// Tight inner loop: run up to pollBatch instructions with no
		// per-instruction watchAddr/note checks. Only err terminates early.
		var err error
		var cycles uint64
		countdown := pollBatch
	inner:
		for {
			pc := cpu.pc
			slot := cache.lookup(pc)

			if slot != nil && slot.len > 0 {
				// Fast path: slot is populated. No branch on slot.len=0.
				if slot.len == 2 {
					err = cpu.execRVCSlot(slot)
				} else {
					err = cpu.stepFromInsn(slot.insn)
				}
			} else {
				err = slowStep(cpu, cache, slot, pc)
			}
			cycles++
			countdown--
			if err != nil || countdown == 0 {
				break inner
			}
		}
		cpu.cycle += cycles

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
			continue
		default:
			return err
		}
	}
}

// slowStep handles the cold paths: PCs outside the cache range or slots
// that aren't yet populated. Kept out of RunCached to keep the hot loop
// tight.
//
//go:noinline
func slowStep(cpu *CPU, cache *DecoderCache, slot *DecodedInsn, pc uint64) error {
	if slot == nil {
		return cpu.step()
	}
	// slot.len == 0 (not yet decoded) — populate and dispatch.
	populateSlot(cpu, slot, pc)
	if slot.len == 0 {
		return cpu.step()
	}
	if slot.len == 2 {
		return cpu.execRVCSlot(slot)
	}
	return cpu.stepFromInsn(slot.insn)
}

// populateSlot fetches and records the instruction at pc. Leaves slot.len
// at 0 if the fetch faults (caller falls back to step() for fault delivery).
// For RVC instructions, additionally pre-decodes register fields and
// immediates so execRVCSlot can dispatch without re-extraction.
//
//go:nosplit
func populateSlot(cpu *CPU, slot *DecodedInsn, pc uint64) {
	half, fh := (&cpu.mem).Fetch16(pc)
	if fh != nil {
		return // leave uninitialized; slow path handles the fault
	}
	if half&0x3 != 0x3 {
		decodeRVC(slot, half)
		return
	}
	w, f := (&cpu.mem).Fetch32(pc)
	if f != nil {
		if f.Kind == FaultMisalign {
			w, f = (&cpu.mem).Fetch32U(pc)
		}
		if f != nil {
			return
		}
	}
	slot.insn = w
	slot.len = 4
	// Set blockEnd for 32-bit control-transfer / system opcodes.
	switch uint8(w & 0x7F) {
	case 0x63, 0x67, 0x6F, 0x73: // BRANCH, JALR, JAL, SYSTEM
		slot.flags |= flagBlockEnd
	case 0x0F: // MISC-MEM — FENCE.I invalidates icache; conservatively end block
		if uint8((w>>12)&0x7) == 0x1 { // funct3 = FENCE.I
			slot.flags |= flagBlockEnd
		}
	}
}
