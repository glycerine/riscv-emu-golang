package riscv

import (
	"math/bits"
	"unsafe"
)

// RunCached is the fast-path dispatch loop. It uses a DecoderCache keyed by
// PC so each instruction pays for fetch + decode only once (on first visit)
// and subsequent visits dispatch straight from pre-decoded fields.
//
// Phase-C megaswitch: both RV32 and RVC opcode bodies live inline here under
// a single switch over slot.op. This eliminates the per-instruction method
// call into exec*Slot that was visible in the Phase-B profile.
//
// The interpreter-cached executors execRVCSlot / exec32Slot are still
// compiled and used by slowStep for cases the megaswitch defers (uncached
// first visit, sentinel slot for OOB PCs, FP/AMO/SYSTEM).
//
// pollBatch is how many instructions run between watchAddr polls. 1024 is
// ~4 µs at 250 MIPS — negligible latency for tohost-style exit, while
// removing per-instruction polling from the hot loop.
const pollBatch = 1024

func RunCached(cpu *CPU, cache *DecoderCache, nc *NoteChain) error {
	// pc stays in a local across the inner loop; cpu.pc is only written when
	// we exit the inner loop (watchAddr / note delivery) or when a delegate
	// / slow-path callee needs it (they read/write c.pc for fault context).
	pc := cpu.pc
	for {
		var err error
		var cycles uint64
		countdown := pollBatch
		slot := cache.lookup(pc)
	inner:
		for {
			switch slot.op {

			// ── case 0 ────────────────────────────────────────────────────
			// Sentinel slot (PC outside cache range) or uninitialized slot
			// (first visit within cache). All valid RV32 opcodes are ≥ 0x03
			// and all RVC classes are ≥ 0x80, so op==0 uniquely identifies
			// these cases.
			case 0:
				pc, err = slowStep(cpu, cache, slot, pc)

			// ══════════════════════════════════════════════════════════════
			//   RV32 opcodes (slot.len == 4)
			// ══════════════════════════════════════════════════════════════

			case 0x03: // LOAD
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				var v uint64
				var f *MemFault
				switch slot.funct3 {
				case 0x0: // LB
					var u uint8
					u, f = (&cpu.mem).Load8(addr)
					v = uint64(int64(int8(u)))
				case 0x1: // LH
					var u uint16
					u, f = (&cpu.mem).Load16(addr)
					if f != nil && f.Kind == FaultMisalign {
						u, f = (&cpu.mem).Load16U(addr)
					}
					v = uint64(int64(int16(u)))
				case 0x2: // LW
					var u uint32
					u, f = (&cpu.mem).Load32(addr)
					if f != nil && f.Kind == FaultMisalign {
						u, f = (&cpu.mem).Load32U(addr)
					}
					v = uint64(int64(int32(u)))
				case 0x3: // LD — aligned fast path via unsafe, OOB/misalign to Load64U
					if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
						v = *(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask)))
					} else {
						v, f = (&cpu.mem).Load64U(addr)
					}
				case 0x4: // LBU
					var u uint8
					u, f = (&cpu.mem).Load8(addr)
					v = uint64(u)
				case 0x5: // LHU
					var u uint16
					u, f = (&cpu.mem).Load16(addr)
					if f != nil && f.Kind == FaultMisalign {
						u, f = (&cpu.mem).Load16U(addr)
					}
					v = uint64(u)
				case 0x6: // LWU
					var u uint32
					u, f = (&cpu.mem).Load32(addr)
					if f != nil && f.Kind == FaultMisalign {
						u, f = (&cpu.mem).Load32U(addr)
					}
					v = uint64(u)
				default:
					err = ErrIllegalInstruction
					break inner
				}
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = v
				cpu.x[0] = 0
				pc += 4

			case 0x0F: // MISC-MEM (FENCE / FENCE.I) — no-op for single-hart interp
				pc += 4

			case 0x13: // OP-IMM (I-type)
				a := cpu.x[slot.rs1]
				imm := uint64(int64(slot.imm))
				delegate := false
				switch slot.funct3 {
				case 0x0: // ADDI
					cpu.x[slot.rd] = a + imm
				case 0x2: // SLTI
					if int64(a) < int64(slot.imm) {
						cpu.x[slot.rd] = 1
					} else {
						cpu.x[slot.rd] = 0
					}
				case 0x3: // SLTIU
					if a < imm {
						cpu.x[slot.rd] = 1
					} else {
						cpu.x[slot.rd] = 0
					}
				case 0x4: // XORI
					cpu.x[slot.rd] = a ^ imm
				case 0x6: // ORI
					cpu.x[slot.rd] = a | imm
				case 0x7: // ANDI
					cpu.x[slot.rd] = a & imm
				case 0x1: // SLLI
					if slot.funct7&^1 == 0 {
						cpu.x[slot.rd] = a << (uint(slot.insn>>20) & 0x3F)
					} else {
						delegate = true
					}
				case 0x5: // SRLI/SRAI
					shamt := uint(slot.insn>>20) & 0x3F
					switch slot.funct7 &^ 1 {
					case 0x00:
						cpu.x[slot.rd] = a >> shamt
					case 0x20:
						cpu.x[slot.rd] = uint64(int64(a) >> shamt)
					default:
						delegate = true
					}
				default:
					delegate = true
				}
				if delegate {
					pc, err = cpu.delegateInsn(slot, pc)
				} else {
					cpu.x[0] = 0
					pc += 4
				}

			case 0x17: // AUIPC
				cpu.x[slot.rd] = pc + uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case 0x1B: // OP-IMM-32
				a := uint32(cpu.x[slot.rs1])
				delegate := false
				switch slot.funct3 {
				case 0x0: // ADDIW
					cpu.x[slot.rd] = uint64(int64(int32(a) + slot.imm))
				case 0x1: // SLLIW
					if slot.funct7 == 0 {
						cpu.x[slot.rd] = uint64(int64(int32(a << (uint(slot.insn>>20) & 0x1F))))
					} else {
						delegate = true
					}
				case 0x5: // SRLIW/SRAIW
					shamt := uint(slot.insn>>20) & 0x1F
					switch slot.funct7 {
					case 0x00:
						cpu.x[slot.rd] = uint64(int64(int32(a >> shamt)))
					case 0x20:
						cpu.x[slot.rd] = uint64(int64(int32(a) >> shamt))
					default:
						delegate = true
					}
				default:
					delegate = true
				}
				if delegate {
					pc, err = cpu.delegateInsn(slot, pc)
				} else {
					cpu.x[0] = 0
					pc += 4
				}

			case 0x23: // STORE (S-type)
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				var f *MemFault
				switch slot.funct3 {
				case 0x0: // SB
					f = (&cpu.mem).Store8(addr, uint8(cpu.x[slot.rs2]))
				case 0x1: // SH
					f = (&cpu.mem).Store16(addr, uint16(cpu.x[slot.rs2]))
					if f != nil && f.Kind == FaultMisalign {
						f = (&cpu.mem).Store16U(addr, uint16(cpu.x[slot.rs2]))
					}
				case 0x2: // SW
					f = (&cpu.mem).Store32(addr, uint32(cpu.x[slot.rs2]))
					if f != nil && f.Kind == FaultMisalign {
						f = (&cpu.mem).Store32U(addr, uint32(cpu.x[slot.rs2]))
					}
				case 0x3: // SD — aligned fast path
					if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
						*(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask))) = cpu.x[slot.rs2]
					} else {
						f = (&cpu.mem).Store64U(addr, cpu.x[slot.rs2])
					}
				default:
					err = ErrIllegalInstruction
					break inner
				}
				if f != nil {
					err = f
					break inner
				}
				pc += 4

			case 0x33: // OP (R-type)
				a := cpu.x[slot.rs1]
				b := cpu.x[slot.rs2]
				delegate := false
				switch slot.funct7 {
				case 0x00: // ADD / SLL / SLT / SLTU / XOR / SRL / OR / AND
					switch slot.funct3 {
					case 0x0:
						cpu.x[slot.rd] = a + b
					case 0x1:
						cpu.x[slot.rd] = a << (b & 0x3F)
					case 0x2:
						if int64(a) < int64(b) {
							cpu.x[slot.rd] = 1
						} else {
							cpu.x[slot.rd] = 0
						}
					case 0x3:
						if a < b {
							cpu.x[slot.rd] = 1
						} else {
							cpu.x[slot.rd] = 0
						}
					case 0x4:
						cpu.x[slot.rd] = a ^ b
					case 0x5:
						cpu.x[slot.rd] = a >> (b & 0x3F)
					case 0x6:
						cpu.x[slot.rd] = a | b
					case 0x7:
						cpu.x[slot.rd] = a & b
					}
				case 0x20: // SUB / SRA
					switch slot.funct3 {
					case 0x0:
						cpu.x[slot.rd] = a - b
					case 0x5:
						cpu.x[slot.rd] = uint64(int64(a) >> (b & 0x3F))
					default:
						delegate = true
					}
				case 0x01: // RV64M
					switch slot.funct3 {
					case 0x0:
						cpu.x[slot.rd] = a * b
					case 0x1:
						hi, _ := bits.Mul64(a, b)
						if int64(a) < 0 {
							hi -= b
						}
						if int64(b) < 0 {
							hi -= a
						}
						cpu.x[slot.rd] = hi
					case 0x2:
						hi, _ := bits.Mul64(a, b)
						if int64(a) < 0 {
							hi -= b
						}
						cpu.x[slot.rd] = hi
					case 0x3:
						hi, _ := bits.Mul64(a, b)
						cpu.x[slot.rd] = hi
					case 0x4:
						if b == 0 {
							cpu.x[slot.rd] = ^uint64(0)
						} else if a == 0x8000000000000000 && b == ^uint64(0) {
							cpu.x[slot.rd] = a
						} else {
							cpu.x[slot.rd] = uint64(int64(a) / int64(b))
						}
					case 0x5:
						if b == 0 {
							cpu.x[slot.rd] = ^uint64(0)
						} else {
							cpu.x[slot.rd] = a / b
						}
					case 0x6:
						if b == 0 {
							cpu.x[slot.rd] = a
						} else if a == 0x8000000000000000 && b == ^uint64(0) {
							cpu.x[slot.rd] = 0
						} else {
							cpu.x[slot.rd] = uint64(int64(a) % int64(b))
						}
					case 0x7:
						if b == 0 {
							cpu.x[slot.rd] = a
						} else {
							cpu.x[slot.rd] = a % b
						}
					}
				default:
					delegate = true
				}
				if delegate {
					pc, err = cpu.delegateInsn(slot, pc)
				} else {
					cpu.x[0] = 0
					pc += 4
				}

			case 0x37: // LUI
				cpu.x[slot.rd] = uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case 0x3B: // OP-32 (RV64I word ops + RV64M word ops)
				a32 := uint32(cpu.x[slot.rs1])
				b32 := uint32(cpu.x[slot.rs2])
				delegate := false
				switch slot.funct7 {
				case 0x00:
					switch slot.funct3 {
					case 0x0: // ADDW
						cpu.x[slot.rd] = uint64(int64(int32(a32 + b32)))
					case 0x1: // SLLW
						cpu.x[slot.rd] = uint64(int64(int32(a32 << (b32 & 0x1F))))
					case 0x5: // SRLW
						cpu.x[slot.rd] = uint64(int64(int32(a32 >> (b32 & 0x1F))))
					default:
						delegate = true
					}
				case 0x20:
					switch slot.funct3 {
					case 0x0: // SUBW
						cpu.x[slot.rd] = uint64(int64(int32(a32 - b32)))
					case 0x5: // SRAW
						cpu.x[slot.rd] = uint64(int64(int32(a32) >> (b32 & 0x1F)))
					default:
						delegate = true
					}
				case 0x01:
					switch slot.funct3 {
					case 0x0: // MULW
						cpu.x[slot.rd] = uint64(int64(int32(a32 * b32)))
					case 0x4: // DIVW
						if b32 == 0 {
							cpu.x[slot.rd] = ^uint64(0)
						} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
							cpu.x[slot.rd] = uint64(int64(int32(a32)))
						} else {
							cpu.x[slot.rd] = uint64(int64(int32(a32) / int32(b32)))
						}
					case 0x5: // DIVUW
						if b32 == 0 {
							cpu.x[slot.rd] = ^uint64(0)
						} else {
							cpu.x[slot.rd] = uint64(int64(int32(a32 / b32)))
						}
					case 0x6: // REMW
						if b32 == 0 {
							cpu.x[slot.rd] = uint64(int64(int32(a32)))
						} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
							cpu.x[slot.rd] = 0
						} else {
							cpu.x[slot.rd] = uint64(int64(int32(a32) % int32(b32)))
						}
					case 0x7: // REMUW
						if b32 == 0 {
							cpu.x[slot.rd] = uint64(int64(int32(a32)))
						} else {
							cpu.x[slot.rd] = uint64(int64(int32(a32 % b32)))
						}
					default:
						delegate = true
					}
				default:
					delegate = true
				}
				if delegate {
					pc, err = cpu.delegateInsn(slot, pc)
				} else {
					cpu.x[0] = 0
					pc += 4
				}

			case 0x63: // BRANCH (B-type)
				a := cpu.x[slot.rs1]
				b := cpu.x[slot.rs2]
				taken := false
				switch slot.funct3 {
				case 0x0:
					taken = a == b
				case 0x1:
					taken = a != b
				case 0x4:
					taken = int64(a) < int64(b)
				case 0x5:
					taken = int64(a) >= int64(b)
				case 0x6:
					taken = a < b
				case 0x7:
					taken = a >= b
				default:
					err = ErrIllegalInstruction
					break inner
				}
				if taken {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}

			case 0x67: // JALR
				target := (cpu.x[slot.rs1] + uint64(int64(slot.imm))) &^ 1
				cpu.x[slot.rd] = pc + 4
				cpu.x[0] = 0
				pc = target

			case 0x6F: // JAL
				cpu.x[slot.rd] = pc + 4
				cpu.x[0] = 0
				pc = pc + uint64(int64(slot.imm))

			// ══════════════════════════════════════════════════════════════
			//   RVC synthetic classes (slot.len == 2)
			// ══════════════════════════════════════════════════════════════

			case opC_ADDI4SPN:
				if slot.imm == 0 {
					err = ErrIllegalInstruction
					break inner
				}
				cpu.x[slot.rd] = cpu.x[2] + uint64(slot.imm)
				pc += 2

			case opC_LW:
				v, f := (&cpu.mem).Load32(cpu.x[slot.rs1] + uint64(slot.imm))
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(int64(int32(v)))
				pc += 2

			case opC_LD:
				addr := cpu.x[slot.rs1] + uint64(slot.imm)
				if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
					cpu.x[slot.rd] = *(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask)))
				} else {
					v, f := (&cpu.mem).Load64U(addr)
					if f != nil {
						err = f
						break inner
					}
					cpu.x[slot.rd] = v
				}
				pc += 2

			case opC_SW:
				if f := (&cpu.mem).Store32(cpu.x[slot.rs1]+uint64(slot.imm), uint32(cpu.x[slot.rs2])); f != nil {
					err = f
					break inner
				}
				pc += 2

			case opC_SD:
				addr := cpu.x[slot.rs1] + uint64(slot.imm)
				if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
					*(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask))) = cpu.x[slot.rs2]
				} else {
					if f := (&cpu.mem).Store64U(addr, cpu.x[slot.rs2]); f != nil {
						err = f
						break inner
					}
				}
				pc += 2

			case opC_ADDI: // C.ADDI / C.NOP(rd=0)
				cpu.x[slot.rd] = cpu.x[slot.rd] + uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 2

			case opC_ADDIW:
				if slot.rd == 0 {
					err = ErrIllegalInstruction
					break inner
				}
				cpu.x[slot.rd] = uint64(int64(int32(cpu.x[slot.rd]) + slot.imm))
				pc += 2

			case opC_LI:
				cpu.x[slot.rd] = uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 2

			case opC_LUI_OR_ADDI16SP:
				if slot.rd == 2 { // C.ADDI16SP
					if slot.imm == 0 {
						err = ErrIllegalInstruction
						break inner
					}
					cpu.x[2] = cpu.x[2] + uint64(int64(slot.imm))
				} else { // C.LUI
					if slot.rd == 0 || slot.imm == 0 {
						err = ErrIllegalInstruction
						break inner
					}
					cpu.x[slot.rd] = uint64(int64(slot.imm))
				}
				pc += 2

			case opC_MISC_ALU: // funct3=100 q1: SRLI / SRAI / ANDI / SUB / XOR / OR / AND / SUBW / ADDW
				insn := uint16(slot.insn)
				funct2 := (insn >> 10) & 3
				rs1p := 8 + uint8((insn>>7)&7)
				rs2p := 8 + uint8((insn>>2)&7)
				bit12 := (insn >> 12) & 1
				bad := false
				switch funct2 {
				case 0b00: // C.SRLI
					shamt := uint8(bit12<<5 | (insn>>2)&0x1F)
					cpu.x[rs1p] = cpu.x[rs1p] >> shamt
				case 0b01: // C.SRAI
					shamt := uint8(bit12<<5 | (insn>>2)&0x1F)
					cpu.x[rs1p] = uint64(int64(cpu.x[rs1p]) >> shamt)
				case 0b10: // C.ANDI
					imm6 := int32((insn >> 2) & 0x1F)
					if bit12 != 0 {
						imm6 |= ^0x1F
					}
					cpu.x[rs1p] = cpu.x[rs1p] & uint64(int64(imm6))
				case 0b11:
					op := (insn >> 5) & 3
					if bit12 == 0 {
						switch op {
						case 0b00:
							cpu.x[rs1p] = cpu.x[rs1p] - cpu.x[rs2p]
						case 0b01:
							cpu.x[rs1p] = cpu.x[rs1p] ^ cpu.x[rs2p]
						case 0b10:
							cpu.x[rs1p] = cpu.x[rs1p] | cpu.x[rs2p]
						case 0b11:
							cpu.x[rs1p] = cpu.x[rs1p] & cpu.x[rs2p]
						}
					} else {
						switch op {
						case 0b00:
							cpu.x[rs1p] = uint64(int64(int32(cpu.x[rs1p]) - int32(cpu.x[rs2p])))
						case 0b01:
							cpu.x[rs1p] = uint64(int64(int32(cpu.x[rs1p]) + int32(cpu.x[rs2p])))
						default:
							bad = true
						}
					}
				}
				if bad {
					err = ErrIllegalInstruction
					break inner
				}
				pc += 2

			case opC_J:
				pc = pc + uint64(int64(slot.imm))

			case opC_BEQZ:
				if cpu.x[slot.rs1] == 0 {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 2
				}

			case opC_BNEZ:
				if cpu.x[slot.rs1] != 0 {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 2
				}

			case opC_SLLI:
				if slot.rd == 0 {
					err = ErrIllegalInstruction
					break inner
				}
				cpu.x[slot.rd] = cpu.x[slot.rd] << uint(slot.imm)
				pc += 2

			case opC_LWSP:
				if slot.rd == 0 {
					err = ErrIllegalInstruction
					break inner
				}
				v, f := (&cpu.mem).Load32(cpu.x[2] + uint64(slot.imm))
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(int64(int32(v)))
				pc += 2

			case opC_LDSP:
				if slot.rd == 0 {
					err = ErrIllegalInstruction
					break inner
				}
				addr := cpu.x[2] + uint64(slot.imm)
				if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
					cpu.x[slot.rd] = *(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask)))
				} else {
					v, f := (&cpu.mem).Load64U(addr)
					if f != nil {
						err = f
						break inner
					}
					cpu.x[slot.rd] = v
				}
				pc += 2

			case opC_JR: // rd != 0 by decode
				pc = cpu.x[slot.rd] &^ 1

			case opC_MV: // rd, rs2 != 0 by encoding
				cpu.x[slot.rd] = cpu.x[slot.rs2]
				pc += 2

			case opC_EBREAK:
				err = ErrEbreak
				pc += 2

			case opC_JALR: // rd != 0 by decode
				target := cpu.x[slot.rd] &^ 1
				cpu.x[1] = pc + 2
				pc = target

			case opC_ADD: // rd, rs2 != 0 by encoding
				cpu.x[slot.rd] = cpu.x[slot.rd] + cpu.x[slot.rs2]
				pc += 2

			case opC_SWSP:
				if f := (&cpu.mem).Store32(cpu.x[2]+uint64(slot.imm), uint32(cpu.x[slot.rs2])); f != nil {
					err = f
					break inner
				}
				pc += 2

			case opC_SDSP:
				addr := cpu.x[2] + uint64(slot.imm)
				if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
					*(*uint64)(unsafe.Pointer(cpu.mem.base + uintptr(addr&cpu.mem.mask))) = cpu.x[slot.rs2]
				} else {
					if f := (&cpu.mem).Store64U(addr, cpu.x[slot.rs2]); f != nil {
						err = f
						break inner
					}
				}
				pc += 2

			// ══════════════════════════════════════════════════════════════
			//   Default: populated slot with an opcode we don't inline
			//   (SYSTEM 0x73, AMO 0x2F, LOAD-FP 0x07, STORE-FP 0x27, FMA
			//   family, OP-FP 0x53, RVC FP classes, opFallback). Delegate
			//   to the full interpreter.
			// ══════════════════════════════════════════════════════════════
			default:
				pc, err = cpu.delegateInsn(slot, pc)
			}

			cycles++
			countdown--
			if err != nil || countdown == 0 {
				break inner
			}
			// Slot chaining: non-block-end and pre-resolved branches use
			// slot.next (zero lookups). Branches/jumps with unresolved
			// targets fall back to cache.lookup.
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
			// Handler may have advanced cpu.pc (e.g. ECALL returns past
			// the ecall). Reload so the inner loop resumes from the right
			// PC.
			pc = cpu.pc
			continue
		default:
			return err
		}
	}
}

// delegateInsn routes a populated slot whose opcode the megaswitch doesn't
// inline (FP, AMO, SYSTEM, Zb* with unusual funct7) through the full
// interpreter. Sets c.pc so the interpreter has the instruction's PC, lets
// it mutate c.pc as it executes, and returns the updated pc.
//
//go:nosplit
func (c *CPU) delegateInsn(slot *DecodedInsn, pc uint64) (uint64, error) {
	c.pc = pc
	var err error
	if slot.len == 2 {
		err = c.stepRVC(uint16(slot.insn))
	} else {
		err = c.stepFromInsn(slot.insn)
	}
	return c.pc, err
}

// slowStep handles cold paths: PCs outside the cache range (sentinel slot)
// or slots that haven't been decoded yet. Returns the new pc in addition to
// the error so the caller can keep pc in a local.
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
	// Block-ending insns normally leave next==nil so RunCached does a
	// cache.lookup after they execute — but for unconditional direct-target
	// branches (JAL, C.J) the target is a decode-time constant, so we can
	// pre-resolve it and chain straight to the target slot (Phase E).
	if slot.len == 0 {
		return
	}
	if slot.flags&flagBlockEnd == 0 {
		succOff := pc + uint64(slot.len) - cache.base
		if succOff < cache.size {
			slot.next = &cache.slots[succOff>>1]
		}
		return
	}
	// Block-ending, but JAL (0x6F) and C.J (opC_J) jump to pc+imm — known
	// at decode time. If the target is inside the cache, wire next so the
	// driver's fast chain absorbs the jump with zero lookups.
	if slot.op == 0x6F || slot.op == opC_J {
		tgtOff := pc + uint64(int64(slot.imm)) - cache.base
		if tgtOff < cache.size {
			slot.next = &cache.slots[tgtOff>>1]
		}
	}
}
