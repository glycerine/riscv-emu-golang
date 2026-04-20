package riscv

// RunCached is a fast-path dispatch loop that uses a DecoderCache to avoid
// re-fetching and re-detecting RVC on every instruction. Semantically
// identical to RunWithChain(cpu, nc), including cycle-counter increments,
// watchAddr polling, and NoteChain exception delivery.
//
// PCs outside the cache range fall back to cpu.step() which performs its
// own fetch. PCs inside the cache range pay one fetch (on first visit) and
// dispatch directly thereafter.
func RunCached(cpu *CPU, cache *DecoderCache, nc *NoteChain) error {
	for {
		// Inner block loop: run instructions until blockEnd or fault.
		// Cycle counter and watchAddr poll are batched once per block.
		var cycles uint64
		var err error
	block:
		for {
			pc := cpu.pc
			slot := cache.lookup(pc)

			if slot == nil {
				err = cpu.step()
				cycles++
				break block // out-of-cache PCs always end the inner loop
			}
			if slot.len == 0 {
				populateSlot(cpu, slot, pc)
				if slot.len == 0 {
					err = cpu.step()
					cycles++
					break block
				}
			}

			if slot.len == 2 {
				err = cpu.execRVCSlot(slot)
			} else {
				err = cpu.stepFromInsn(slot.insn)
			}
			cycles++

			if err != nil || slot.blockEnd() {
				break block
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
