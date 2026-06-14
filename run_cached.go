package riscv

import (
	"math/bits"
	"unsafe"
)

// runCached is the fast-path dispatch loop. It uses a DecoderCache keyed by
// PC so each instruction pays for fetch + decode only once (on first visit)
// and subsequent visits dispatch straight from pre-decoded fields.
//
// Flat megaswitch: every instruction handler is a top-level case in a single
// switch on slot.op — no nested switches on funct3/funct7. The flat opcode
// is resolved at decode time (flattenSlotOp) so dispatch is one indirect
// branch per instruction.
//
// pollBatch is how many instructions run between watchAddr polls. 1024 is
// ~4 µs at 250 MIPS — negligible latency for tohost-style exit, while
// removing per-instruction polling from the hot loop.
const pollBatch = 10240

type RunBudgetResult uint8

const (
	RunBudgetContinue RunBudgetResult = iota
	RunBudgetExpired
	RunBudgetExit
)

// Flat dispatch opcodes for the runCached megaswitch. Each constant
// uniquely identifies one instruction handler. Assigned at decode time
// by flattenSlotOp so the megaswitch is a single-level jump table —
// one indirect branch per dispatch, zero nesting.
//
// RVC synthetic classes (opC_* at 0x80+) are defined in decode.go and
// remain unchanged — they were already flat. Only opC_MISC_ALU gets
// split into 9 separate opcodes here.
const (
	// 0 = sentinel (uninitialized or OOB slot)

	opLB  uint8 = 1
	opLH  uint8 = 2
	opLW  uint8 = 3
	opLD  uint8 = 4
	opLBU uint8 = 5
	opLHU uint8 = 6
	opLWU uint8 = 7

	opSB uint8 = 8
	opSH uint8 = 9
	opSW uint8 = 10
	opSD uint8 = 11

	opBEQ  uint8 = 12
	opBNE  uint8 = 13
	opBLT  uint8 = 14
	opBGE  uint8 = 15
	opBLTU uint8 = 16
	opBGEU uint8 = 17

	opADDI  uint8 = 18
	opSLTI  uint8 = 19
	opSLTIU uint8 = 20
	opXORI  uint8 = 21
	opORI   uint8 = 22
	opANDI  uint8 = 23
	opSLLI  uint8 = 24
	opSRLI  uint8 = 25
	opSRAI  uint8 = 26

	opADDIW uint8 = 27
	opSLLIW uint8 = 28
	opSRLIW uint8 = 29
	opSRAIW uint8 = 30

	opADD  uint8 = 31
	opSLL  uint8 = 32
	opSLT  uint8 = 33
	opSLTU uint8 = 34
	opXOR  uint8 = 35
	opSRL  uint8 = 36
	opOR   uint8 = 37
	opAND  uint8 = 38

	opSUB uint8 = 39
	opSRA uint8 = 40

	opMUL    uint8 = 41
	opMULH   uint8 = 42
	opMULHSU uint8 = 43
	opMULHU  uint8 = 44
	opDIV    uint8 = 45
	opDIVU   uint8 = 46
	opREM    uint8 = 47
	opREMU   uint8 = 48

	opADDW uint8 = 49
	opSLLW uint8 = 50
	opSRLW uint8 = 51

	opSUBW uint8 = 52
	opSRAW uint8 = 53

	opMULW  uint8 = 54
	opDIVW  uint8 = 55
	opDIVUW uint8 = 56
	opREMW  uint8 = 57
	opREMUW uint8 = 58

	opFENCE  uint8 = 59
	opIAUIPC uint8 = 60
	opILUI   uint8 = 61
	opIJAL   uint8 = 62
	opIJALR  uint8 = 63

	opCaSRLI uint8 = 64
	opCaSRAI uint8 = 65
	opCaANDI uint8 = 66
	opCaSUB  uint8 = 67
	opCaXOR  uint8 = 68
	opCaOR   uint8 = 69
	opCaAND  uint8 = 70
	opCaSUBW uint8 = 71
	opCaADDW uint8 = 72

	opDelegate uint8 = 73
)

// RunDefault is the "just run the guest" entry point used by cpu.Run().
// It allocates a fresh 256 KB decoder cache based at the current PC and
// hands off to runCached. Callers needing cross-call cache reuse or custom
// sizing should build their own *DecoderCache and call runCached directly
// (WAT? THIS DIRECTLY CONTRADICTS THE FOLLOWING!??)
//
// ── Why this helper exists (performance footgun) ─────────────────────────
//
// This function is a deliberate indirection layer for code-generation
// reasons, not a semantic one. Bisection (see git history around the
// "recover 419 MIPS" change) established that:
//
//	cpu.Run()   =>  calls runCached directly                =>  ~314 MIPS
//	cpu.Run()   =>  calls RunDefault   (defined here)       =>  ~406 MIPS
//
// Same runCached source, same CPU struct, same benchmarks. The ~25%
// slowdown is visible even in benchmarks that never call cpu.Run() (e.g.
// BenchmarkCPU_FullExecution_Cached calls riscv.runCached directly from
// the bench package). Per-line pprof listing shows the hit is spread
// across the top of the megaswitch: `switch slot.op`, the hot RVC cases
// (C.ADDI / C.MV / C.ADD / C.BNEZ), and the slot.next fallback — a
// register-allocation shift in the megaswitch itself.
//
// The likely mechanism is some package-level analysis in the Go compiler
// (inliner cost model, call-graph weighting, function ordering) that
// changes how runCached is compiled once it has an in-file caller inside
// cpu.go. The threshold isn't documented and isn't stable across Go
// versions. What IS stable: keeping every runCached callsite inside
// run_cached.go preserves the fast codegen.
//
// Rules for maintainers:
//   - Do NOT call runCached from any file in this package other than
//     run_cached.go. Route through RunDefault (or add a new helper here).
//   - When adding a new "default run" entry point on CPU or elsewhere,
//     have it land in run_cached.go and chain from there.
//   - Regression gate: BenchmarkCPU_FullExecution_Cached must stay
//     ≥ 400 MIPS on the baseline developer machine (i7 Ice Lake class).
//     If it drops without an obvious source-level explanation, look for
//     a new cross-file call into runCached.
func RunDefault(cpu *CPU, nc *NoteChain) error {

	// Cache covers [entry-4K, entry+256K). Anything outside falls back to step().
	base := cpu.pc &^ uint64(0xFFF)
	if base > 0x1000 {
		base -= 0x1000
	}
	cache := NewDecoderCache(base, 256<<10)
	err := runCached(cpu, cache, nc)
	return err
}

func runCached(cpu *CPU, cache *DecoderCache, nc *NoteChain) error {
	_, err := runCachedBudget(cpu, cache, nc, 0)
	return err
}

func RunDefaultBudget(cpu *CPU, nc *NoteChain, budget uint64) (RunBudgetResult, error) {
	base := cpu.pc &^ uint64(0xFFF)
	if base > 0x1000 {
		base -= 0x1000
	}
	cache := NewDecoderCache(base, 256<<10)
	res, err := runCachedBudget(cpu, cache, nc, budget)
	if _, ok := err.(*ExitError); ok {
		return RunBudgetExit, nil
	}
	return res, err
}

func runCachedBudget(cpu *CPU, cache *DecoderCache, nc *NoteChain, budget uint64) (RunBudgetResult, error) {
	// pc stays in a local across the inner loop; cpu.pc is only written when
	// we exit the inner loop (watchAddr / note delivery) or when a delegate
	// / slow-path callee needs it (they read/write c.pc for fault context).
	pc := cpu.pc
	var budgetUsed uint64
	for {
		var err error
		var instrBegun uint64
		countdown := pollBatch
		if budget != 0 {
			remaining := budget - budgetUsed
			if remaining < uint64(countdown) {
				countdown = int(remaining)
			}
		}
		slot := cache.lookup(pc)
	inner:
		for {
			switch slot.op {

			// ── case 0 ────────────────────────────────────────────────────
			// Sentinel slot (PC outside cache range) or uninitialized slot
			// (first visit within cache). For first-visit slots, slowStep
			// populates with a flat opcode and returns (pc, nil). The
			// continue re-dispatches through the megaswitch at no runtime
			// cost since it only happens once per instruction.
			case 0:
				pc, err = slowStep(cpu, cache, slot, pc)
				if err == nil && slot.op != 0 {
					continue
				}

			// ══════════════════════════════════════════════════════════════
			//   RV32 flat opcodes (slot.len == 4)
			// ══════════════════════════════════════════════════════════════

			case opLB:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				u, f := (&cpu.mem).Load8(addr)
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(int64(int8(u)))
				cpu.x[0] = 0
				pc += 4

			case opLH:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				u, f := (&cpu.mem).Load16(addr)
				if f != nil && f.Kind == FaultMisalign {
					u, f = (&cpu.mem).Load16U(addr)
				}
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(int64(int16(u)))
				cpu.x[0] = 0
				pc += 4

			case opLW:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				u, f := (&cpu.mem).Load32(addr)
				if f != nil && f.Kind == FaultMisalign {
					u, f = (&cpu.mem).Load32U(addr)
				}
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(int64(int32(u)))
				cpu.x[0] = 0
				pc += 4

			case opLD:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
					cpu.x[slot.rd] = *(*uint64)(unsafe.Add(cpu.mem.base, addr&cpu.mem.mask))
				} else {
					v, f := (&cpu.mem).Load64U(addr)
					if f != nil {
						err = f
						break inner
					}
					cpu.x[slot.rd] = v
				}
				cpu.x[0] = 0
				pc += 4

			case opLBU:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				u, f := (&cpu.mem).Load8(addr)
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(u)
				cpu.x[0] = 0
				pc += 4

			case opLHU:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				u, f := (&cpu.mem).Load16(addr)
				if f != nil && f.Kind == FaultMisalign {
					u, f = (&cpu.mem).Load16U(addr)
				}
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(u)
				cpu.x[0] = 0
				pc += 4

			case opLWU:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				u, f := (&cpu.mem).Load32(addr)
				if f != nil && f.Kind == FaultMisalign {
					u, f = (&cpu.mem).Load32U(addr)
				}
				if f != nil {
					err = f
					break inner
				}
				cpu.x[slot.rd] = uint64(u)
				cpu.x[0] = 0
				pc += 4

			case opFENCE:
				pc += 4

			case opADDI:
				cpu.x[slot.rd] = cpu.x[slot.rs1] + uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opSLTI:
				if int64(cpu.x[slot.rs1]) < int64(slot.imm) {
					cpu.x[slot.rd] = 1
				} else {
					cpu.x[slot.rd] = 0
				}
				cpu.x[0] = 0
				pc += 4

			case opSLTIU:
				if cpu.x[slot.rs1] < uint64(int64(slot.imm)) {
					cpu.x[slot.rd] = 1
				} else {
					cpu.x[slot.rd] = 0
				}
				cpu.x[0] = 0
				pc += 4

			case opXORI:
				cpu.x[slot.rd] = cpu.x[slot.rs1] ^ uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opORI:
				cpu.x[slot.rd] = cpu.x[slot.rs1] | uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opANDI:
				cpu.x[slot.rd] = cpu.x[slot.rs1] & uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opSLLI:
				cpu.x[slot.rd] = cpu.x[slot.rs1] << (uint(slot.insn>>20) & 0x3F)
				cpu.x[0] = 0
				pc += 4

			case opSRLI:
				cpu.x[slot.rd] = cpu.x[slot.rs1] >> (uint(slot.insn>>20) & 0x3F)
				cpu.x[0] = 0
				pc += 4

			case opSRAI:
				cpu.x[slot.rd] = uint64(int64(cpu.x[slot.rs1]) >> (uint(slot.insn>>20) & 0x3F))
				cpu.x[0] = 0
				pc += 4

			case opIAUIPC:
				cpu.x[slot.rd] = pc + uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opADDIW:
				cpu.x[slot.rd] = uint64(int64(int32(cpu.x[slot.rs1]) + slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opSLLIW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) << (uint(slot.insn>>20) & 0x1F))))
				cpu.x[0] = 0
				pc += 4

			case opSRLIW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) >> (uint(slot.insn>>20) & 0x1F))))
				cpu.x[0] = 0
				pc += 4

			case opSRAIW:
				cpu.x[slot.rd] = uint64(int64(int32(cpu.x[slot.rs1]) >> (uint(slot.insn>>20) & 0x1F)))
				cpu.x[0] = 0
				pc += 4

			case opSB:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				if f := (&cpu.mem).Store8(addr, uint8(cpu.x[slot.rs2])); f != nil {
					err = f
					break inner
				}
				pc += 4

			case opSH:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				f := (&cpu.mem).Store16(addr, uint16(cpu.x[slot.rs2]))
				if f != nil && f.Kind == FaultMisalign {
					f = (&cpu.mem).Store16U(addr, uint16(cpu.x[slot.rs2]))
				}
				if f != nil {
					err = f
					break inner
				}
				pc += 4

			case opSW:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				f := (&cpu.mem).Store32(addr, uint32(cpu.x[slot.rs2]))
				if f != nil && f.Kind == FaultMisalign {
					f = (&cpu.mem).Store32U(addr, uint32(cpu.x[slot.rs2]))
				}
				if f != nil {
					err = f
					break inner
				}
				pc += 4

			case opSD:
				addr := cpu.x[slot.rs1] + uint64(int64(slot.imm))
				if addr&7 == 0 && (addr|(addr+7))&^cpu.mem.mask == 0 {
					*(*uint64)(unsafe.Add(cpu.mem.base, addr&cpu.mem.mask)) = cpu.x[slot.rs2]
				} else {
					if f := (&cpu.mem).Store64U(addr, cpu.x[slot.rs2]); f != nil {
						err = f
						break inner
					}
				}
				pc += 4

			case opADD:
				cpu.x[slot.rd] = cpu.x[slot.rs1] + cpu.x[slot.rs2]
				cpu.x[0] = 0
				pc += 4
			case opSLL:
				cpu.x[slot.rd] = cpu.x[slot.rs1] << (cpu.x[slot.rs2] & 0x3F)
				cpu.x[0] = 0
				pc += 4
			case opSLT:
				if int64(cpu.x[slot.rs1]) < int64(cpu.x[slot.rs2]) {
					cpu.x[slot.rd] = 1
				} else {
					cpu.x[slot.rd] = 0
				}
				cpu.x[0] = 0
				pc += 4
			case opSLTU:
				if cpu.x[slot.rs1] < cpu.x[slot.rs2] {
					cpu.x[slot.rd] = 1
				} else {
					cpu.x[slot.rd] = 0
				}
				cpu.x[0] = 0
				pc += 4
			case opXOR:
				cpu.x[slot.rd] = cpu.x[slot.rs1] ^ cpu.x[slot.rs2]
				cpu.x[0] = 0
				pc += 4
			case opSRL:
				cpu.x[slot.rd] = cpu.x[slot.rs1] >> (cpu.x[slot.rs2] & 0x3F)
				cpu.x[0] = 0
				pc += 4
			case opOR:
				cpu.x[slot.rd] = cpu.x[slot.rs1] | cpu.x[slot.rs2]
				cpu.x[0] = 0
				pc += 4
			case opAND:
				cpu.x[slot.rd] = cpu.x[slot.rs1] & cpu.x[slot.rs2]
				cpu.x[0] = 0
				pc += 4

			case opSUB:
				cpu.x[slot.rd] = cpu.x[slot.rs1] - cpu.x[slot.rs2]
				cpu.x[0] = 0
				pc += 4
			case opSRA:
				cpu.x[slot.rd] = uint64(int64(cpu.x[slot.rs1]) >> (cpu.x[slot.rs2] & 0x3F))
				cpu.x[0] = 0
				pc += 4

			case opMUL:
				cpu.x[slot.rd] = cpu.x[slot.rs1] * cpu.x[slot.rs2]
				cpu.x[0] = 0
				pc += 4
			case opMULH:
				a, b := cpu.x[slot.rs1], cpu.x[slot.rs2]
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 {
					hi -= b
				}
				if int64(b) < 0 {
					hi -= a
				}
				cpu.x[slot.rd] = hi
				cpu.x[0] = 0
				pc += 4
			case opMULHSU:
				a, b := cpu.x[slot.rs1], cpu.x[slot.rs2]
				hi, _ := bits.Mul64(a, b)
				if int64(a) < 0 {
					hi -= b
				}
				cpu.x[slot.rd] = hi
				cpu.x[0] = 0
				pc += 4
			case opMULHU:
				hi, _ := bits.Mul64(cpu.x[slot.rs1], cpu.x[slot.rs2])
				cpu.x[slot.rd] = hi
				cpu.x[0] = 0
				pc += 4
			case opDIV:
				a, b := cpu.x[slot.rs1], cpu.x[slot.rs2]
				if b == 0 {
					cpu.x[slot.rd] = ^uint64(0)
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					cpu.x[slot.rd] = a
				} else {
					cpu.x[slot.rd] = uint64(int64(a) / int64(b))
				}
				cpu.x[0] = 0
				pc += 4
			case opDIVU:
				a, b := cpu.x[slot.rs1], cpu.x[slot.rs2]
				if b == 0 {
					cpu.x[slot.rd] = ^uint64(0)
				} else {
					cpu.x[slot.rd] = a / b
				}
				cpu.x[0] = 0
				pc += 4
			case opREM:
				a, b := cpu.x[slot.rs1], cpu.x[slot.rs2]
				if b == 0 {
					cpu.x[slot.rd] = a
				} else if a == 0x8000000000000000 && b == ^uint64(0) {
					cpu.x[slot.rd] = 0
				} else {
					cpu.x[slot.rd] = uint64(int64(a) % int64(b))
				}
				cpu.x[0] = 0
				pc += 4
			case opREMU:
				a, b := cpu.x[slot.rs1], cpu.x[slot.rs2]
				if b == 0 {
					cpu.x[slot.rd] = a
				} else {
					cpu.x[slot.rd] = a % b
				}
				cpu.x[0] = 0
				pc += 4

			case opILUI:
				cpu.x[slot.rd] = uint64(int64(slot.imm))
				cpu.x[0] = 0
				pc += 4

			case opADDW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) + uint32(cpu.x[slot.rs2]))))
				cpu.x[0] = 0
				pc += 4
			case opSLLW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) << (uint32(cpu.x[slot.rs2]) & 0x1F))))
				cpu.x[0] = 0
				pc += 4
			case opSRLW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) >> (uint32(cpu.x[slot.rs2]) & 0x1F))))
				cpu.x[0] = 0
				pc += 4

			case opSUBW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) - uint32(cpu.x[slot.rs2]))))
				cpu.x[0] = 0
				pc += 4
			case opSRAW:
				cpu.x[slot.rd] = uint64(int64(int32(cpu.x[slot.rs1]) >> (uint32(cpu.x[slot.rs2]) & 0x1F)))
				cpu.x[0] = 0
				pc += 4

			case opMULW:
				cpu.x[slot.rd] = uint64(int64(int32(uint32(cpu.x[slot.rs1]) * uint32(cpu.x[slot.rs2]))))
				cpu.x[0] = 0
				pc += 4
			case opDIVW:
				a32, b32 := uint32(cpu.x[slot.rs1]), uint32(cpu.x[slot.rs2])
				if b32 == 0 {
					cpu.x[slot.rd] = ^uint64(0)
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					cpu.x[slot.rd] = uint64(int64(int32(a32)))
				} else {
					cpu.x[slot.rd] = uint64(int64(int32(a32) / int32(b32)))
				}
				cpu.x[0] = 0
				pc += 4
			case opDIVUW:
				a32, b32 := uint32(cpu.x[slot.rs1]), uint32(cpu.x[slot.rs2])
				if b32 == 0 {
					cpu.x[slot.rd] = ^uint64(0)
				} else {
					cpu.x[slot.rd] = uint64(int64(int32(a32 / b32)))
				}
				cpu.x[0] = 0
				pc += 4
			case opREMW:
				a32, b32 := uint32(cpu.x[slot.rs1]), uint32(cpu.x[slot.rs2])
				if b32 == 0 {
					cpu.x[slot.rd] = uint64(int64(int32(a32)))
				} else if a32 == 0x80000000 && b32 == 0xFFFFFFFF {
					cpu.x[slot.rd] = 0
				} else {
					cpu.x[slot.rd] = uint64(int64(int32(a32) % int32(b32)))
				}
				cpu.x[0] = 0
				pc += 4
			case opREMUW:
				a32, b32 := uint32(cpu.x[slot.rs1]), uint32(cpu.x[slot.rs2])
				if b32 == 0 {
					cpu.x[slot.rd] = uint64(int64(int32(a32)))
				} else {
					cpu.x[slot.rd] = uint64(int64(int32(a32 % b32)))
				}
				cpu.x[0] = 0
				pc += 4

			case opBEQ:
				if cpu.x[slot.rs1] == cpu.x[slot.rs2] {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}
			case opBNE:
				if cpu.x[slot.rs1] != cpu.x[slot.rs2] {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}
			case opBLT:
				if int64(cpu.x[slot.rs1]) < int64(cpu.x[slot.rs2]) {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}
			case opBGE:
				if int64(cpu.x[slot.rs1]) >= int64(cpu.x[slot.rs2]) {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}
			case opBLTU:
				if cpu.x[slot.rs1] < cpu.x[slot.rs2] {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}
			case opBGEU:
				if cpu.x[slot.rs1] >= cpu.x[slot.rs2] {
					pc = pc + uint64(int64(slot.imm))
				} else {
					pc += 4
				}

			case opIJALR:
				target := (cpu.x[slot.rs1] + uint64(int64(slot.imm))) &^ 1
				cpu.x[slot.rd] = pc + 4
				cpu.x[0] = 0
				pc = target

			case opIJAL:
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
					cpu.x[slot.rd] = *(*uint64)(unsafe.Add(cpu.mem.base, addr&cpu.mem.mask))
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
					*(*uint64)(unsafe.Add(cpu.mem.base, addr&cpu.mem.mask)) = cpu.x[slot.rs2]
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

			case opCaSRLI:
				cpu.x[slot.rd] >>= uint(slot.imm)
				pc += 2
			case opCaSRAI:
				cpu.x[slot.rd] = uint64(int64(cpu.x[slot.rd]) >> uint(slot.imm))
				pc += 2
			case opCaANDI:
				cpu.x[slot.rd] &= uint64(int64(slot.imm))
				pc += 2
			case opCaSUB:
				cpu.x[slot.rd] -= cpu.x[slot.rs2]
				pc += 2
			case opCaXOR:
				cpu.x[slot.rd] ^= cpu.x[slot.rs2]
				pc += 2
			case opCaOR:
				cpu.x[slot.rd] |= cpu.x[slot.rs2]
				pc += 2
			case opCaAND:
				cpu.x[slot.rd] &= cpu.x[slot.rs2]
				pc += 2
			case opCaSUBW:
				cpu.x[slot.rd] = uint64(int64(int32(cpu.x[slot.rd]) - int32(cpu.x[slot.rs2])))
				pc += 2
			case opCaADDW:
				cpu.x[slot.rd] = uint64(int64(int32(cpu.x[slot.rd]) + int32(cpu.x[slot.rs2])))
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
					cpu.x[slot.rd] = *(*uint64)(unsafe.Add(cpu.mem.base, addr&cpu.mem.mask))
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
					*(*uint64)(unsafe.Add(cpu.mem.base, addr&cpu.mem.mask)) = cpu.x[slot.rs2]
				} else {
					if f := (&cpu.mem).Store64U(addr, cpu.x[slot.rs2]); f != nil {
						err = f
						break inner
					}
				}
				pc += 2

			// ══════════════════════════════════════════════════════════════
			//   Default: opDelegate (FP/AMO/SYSTEM/Zb*) or RVC FP classes.
			// ══════════════════════════════════════════════════════════════
			default:
				pc, err = cpu.delegateInsn(slot, pc)
			}

			instrBegun++
			if budget != 0 {
				budgetUsed++
			}
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
		cpu.riscvInstrBegun += instrBegun
		cpu.pc = pc

		if cpu.watchAddr != 0 {
			if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
				return RunBudgetExit, &ExitError{Code: tohostExitCode(v)}
			}
		}
		if err == nil {
			if budget != 0 && budgetUsed >= budget {
				return RunBudgetExpired, nil
			}
			continue
		}
		n := noteFromStepErr(err, cpu.PC())
		switch nc.Deliver(cpu, n) {
		case NoteHandled:
			// Handler may have advanced cpu.pc (e.g. ECALL returns past
			// the ecall). Reload so the inner loop resumes from the right
			// PC.
			pc = cpu.pc
			if budget != 0 && budgetUsed >= budget {
				return RunBudgetExpired, nil
			}
			continue
		case NoteExit:
			return RunBudgetExit, &ExitError{Code: cpu.ExitCode}
		default:
			return RunBudgetContinue, err
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
// or slots that haven't been decoded yet. For sentinel slots, falls back to
// cpu.step(). For first-visit slots, populates and returns (pc, nil) so the
// megaswitch can re-dispatch via the now-flat opcode.
//
//go:noinline
func slowStep(cpu *CPU, cache *DecoderCache, slot *DecodedInsn, pc uint64) (uint64, error) {
	if slot == &cache.sentinel {
		cpu.pc = pc
		err := cpu.step()
		return cpu.pc, err
	}
	populateSlot(cpu, cache, slot, pc)
	if slot.len == 0 {
		cpu.pc = pc
		err := cpu.step()
		return cpu.pc, err
	}
	return pc, nil
}

// populateSlot fetches and records the instruction at pc. Leaves slot.len
// at 0 if the fetch faults (caller falls back to step() for fault delivery).
// For RVC instructions, additionally pre-decodes register fields and
// immediates so execRVCSlot can dispatch without re-extraction.
//
// After decoding, if the instruction is non-block-ending and the fall-through
// PC lands inside the cache range, wires slot.next to the successor slot so
// runCached can skip cache.lookup on the linear-flow path.
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
		// Must check raw opcode (0x0F) before flattenSlotOp converts it.
		if slot.op == 0x0F && slot.funct3 == 0x1 {
			slot.flags |= flagBlockEnd
		}
	}
	flattenSlotOp(slot)
	// Wire up slot.next for non-block-end successors whose PC is in-range.
	// Block-ending insns normally leave next==nil so runCached does a
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
	// Block-ending, but JAL and C.J jump to pc+imm — known at decode time.
	// If the target is inside the cache, wire next so the driver's fast
	// chain absorbs the jump with zero lookups.
	if slot.op == opIJAL || slot.op == opC_J {
		tgtOff := pc + uint64(int64(slot.imm)) - cache.base
		if tgtOff < cache.size {
			slot.next = &cache.slots[tgtOff>>1]
		}
	}
}

// flattenSlotOp converts the raw opcode in slot.op to a flat dispatch ID.
// Called once per instruction (at decode time in populateSlot). RVC opcodes
// >= 0x80 are already flat except opC_MISC_ALU which is split into 9 IDs.
//
//go:nosplit
func flattenSlotOp(slot *DecodedInsn) {
	if slot.op >= 0x80 {
		if slot.op == opC_MISC_ALU {
			flattenMiscALU(slot)
		}
		return
	}
	switch slot.op {
	case 0x03: // LOAD
		switch slot.funct3 {
		case 0x0:
			slot.op = opLB
		case 0x1:
			slot.op = opLH
		case 0x2:
			slot.op = opLW
		case 0x3:
			slot.op = opLD
		case 0x4:
			slot.op = opLBU
		case 0x5:
			slot.op = opLHU
		case 0x6:
			slot.op = opLWU
		default:
			slot.op = opDelegate
		}
	case 0x23: // STORE
		switch slot.funct3 {
		case 0x0:
			slot.op = opSB
		case 0x1:
			slot.op = opSH
		case 0x2:
			slot.op = opSW
		case 0x3:
			slot.op = opSD
		default:
			slot.op = opDelegate
		}
	case 0x63: // BRANCH
		switch slot.funct3 {
		case 0x0:
			slot.op = opBEQ
		case 0x1:
			slot.op = opBNE
		case 0x4:
			slot.op = opBLT
		case 0x5:
			slot.op = opBGE
		case 0x6:
			slot.op = opBLTU
		case 0x7:
			slot.op = opBGEU
		default:
			slot.op = opDelegate
		}
	case 0x13: // OP-IMM
		switch slot.funct3 {
		case 0x0:
			slot.op = opADDI
		case 0x2:
			slot.op = opSLTI
		case 0x3:
			slot.op = opSLTIU
		case 0x4:
			slot.op = opXORI
		case 0x6:
			slot.op = opORI
		case 0x7:
			slot.op = opANDI
		case 0x1:
			if slot.funct7&^1 == 0 {
				slot.op = opSLLI
			} else {
				slot.op = opDelegate
			}
		case 0x5:
			switch slot.funct7 &^ 1 {
			case 0x00:
				slot.op = opSRLI
			case 0x20:
				slot.op = opSRAI
			default:
				slot.op = opDelegate
			}
		default:
			slot.op = opDelegate
		}
	case 0x1B: // OP-IMM-32
		switch slot.funct3 {
		case 0x0:
			slot.op = opADDIW
		case 0x1:
			if slot.funct7 == 0 {
				slot.op = opSLLIW
			} else {
				slot.op = opDelegate
			}
		case 0x5:
			switch slot.funct7 {
			case 0x00:
				slot.op = opSRLIW
			case 0x20:
				slot.op = opSRAIW
			default:
				slot.op = opDelegate
			}
		default:
			slot.op = opDelegate
		}
	case 0x33: // OP (R-type)
		switch slot.funct7 {
		case 0x00:
			slot.op = opADD + slot.funct3
		case 0x20:
			switch slot.funct3 {
			case 0x0:
				slot.op = opSUB
			case 0x5:
				slot.op = opSRA
			default:
				slot.op = opDelegate
			}
		case 0x01:
			slot.op = opMUL + slot.funct3
		default:
			slot.op = opDelegate
		}
	case 0x3B: // OP-32
		switch slot.funct7 {
		case 0x00:
			switch slot.funct3 {
			case 0x0:
				slot.op = opADDW
			case 0x1:
				slot.op = opSLLW
			case 0x5:
				slot.op = opSRLW
			default:
				slot.op = opDelegate
			}
		case 0x20:
			switch slot.funct3 {
			case 0x0:
				slot.op = opSUBW
			case 0x5:
				slot.op = opSRAW
			default:
				slot.op = opDelegate
			}
		case 0x01:
			switch slot.funct3 {
			case 0x0:
				slot.op = opMULW
			case 0x4:
				slot.op = opDIVW
			case 0x5:
				slot.op = opDIVUW
			case 0x6:
				slot.op = opREMW
			case 0x7:
				slot.op = opREMUW
			default:
				slot.op = opDelegate
			}
		default:
			slot.op = opDelegate
		}
	case 0x0F:
		slot.op = opFENCE
	case 0x17:
		slot.op = opIAUIPC
	case 0x37:
		slot.op = opILUI
	case 0x6F:
		slot.op = opIJAL
	case 0x67:
		slot.op = opIJALR
	default:
		slot.op = opDelegate
	}
}

//go:nosplit
func flattenMiscALU(slot *DecodedInsn) {
	insn := uint16(slot.insn)
	slot.rs1 = 8 + uint8((insn>>7)&7)
	slot.rd = slot.rs1
	slot.rs2 = 8 + uint8((insn>>2)&7)
	funct2 := (insn >> 10) & 3
	bit12 := (insn >> 12) & 1
	switch funct2 {
	case 0b00:
		slot.op = opCaSRLI
		slot.imm = int32(bit12<<5 | (insn>>2)&0x1F)
	case 0b01:
		slot.op = opCaSRAI
		slot.imm = int32(bit12<<5 | (insn>>2)&0x1F)
	case 0b10:
		imm6 := int32((insn >> 2) & 0x1F)
		if bit12 != 0 {
			imm6 |= ^0x1F
		}
		slot.op = opCaANDI
		slot.imm = imm6
	case 0b11:
		op := (insn >> 5) & 3
		if bit12 == 0 {
			switch op {
			case 0b00:
				slot.op = opCaSUB
			case 0b01:
				slot.op = opCaXOR
			case 0b10:
				slot.op = opCaOR
			case 0b11:
				slot.op = opCaAND
			}
		} else {
			switch op {
			case 0b00:
				slot.op = opCaSUBW
			case 0b01:
				slot.op = opCaADDW
			default:
				slot.op = opDelegate
			}
		}
	}
}
