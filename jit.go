package riscv

// jit.go — JIT manager: block cache, RunJIT dispatch loop.

import "riscv/internal/jitcall"

// JIT status codes returned by compiled blocks.
const (
	jitOK         = 0
	jitEcall      = 1
	jitEbreak     = 2
	jitLoadFault  = 3
	jitStoreFault = 4
	jitIllegal    = 5
)

// JIT holds the cache of compiled basic blocks.
type JIT struct {
	blocks     map[uint64]*compiledBlock
	noJIT      map[uint64]bool // PCs where translation failed — don't retry
	InterpOnly bool            // debug: force all-interpreter mode
}

// NewJIT creates a new JIT translation cache.
func NewJIT() *JIT {
	return &JIT{
		blocks: make(map[uint64]*compiledBlock),
		noJIT:  make(map[uint64]bool),
	}
}

// RunJIT executes the CPU using JIT-compiled blocks where possible,
// falling back to the interpreter for untranslatable instructions.
// Integrates with the CPU's NoteChain for exception handling.
func (j *JIT) RunJIT(cpu *CPU) error {
	for {
		pc := cpu.pc

		// Try the JIT cache first.
		if blk, ok := j.blocks[pc]; ok {
			res := jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
				cpu.mem.Base(), cpu.mem.Mask())
			cpu.pc = res.PC
			cpu.cycle += res.IC

			switch int(res.Status) {
			case jitOK:
				continue // normal block exit → dispatch next block

			case jitEcall:
				n := noteFromStepErr(ErrEcall, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return ErrEcall
				}

			case jitEbreak:
				n := noteFromStepErr(ErrEbreak, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return ErrEbreak
				}

			case jitLoadFault:
				f := &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
				n := noteFromStepErr(f, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return f
				}

			case jitStoreFault:
				f := &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
				n := noteFromStepErr(f, cpu.pc)
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return f
				}

			default:
				// Unknown status — fall back to interpreter for this instruction.
				err := cpu.step()
				cpu.cycle++
				if err == nil {
					continue
				}
				n := noteFromStepErr(err, cpu.PC())
				switch cpu.Notes.Deliver(cpu, n) {
				case NoteHandled:
					continue
				default:
					return err
				}
			}
		}

		// No compiled block for this PC — try to translate one.
		if !j.InterpOnly && !j.noJIT[pc] {
			res := emitBlock(&cpu.mem, pc)
			if res != nil && res.numInsns > 0 {
				blk, err := tccCompile(res.csrc)
				if err == nil {
					j.blocks[pc] = blk
					continue
				}
				// Compilation failed.
			}
			// Remember this PC can't be JIT'd (RVC, FP, CSR, etc.)
			j.noJIT[pc] = true
		}

		// Can't translate — interpret one instruction and try again.
		// Interpret one instruction and try again.
		err := cpu.step()
		cpu.cycle++
		if err == nil {
			continue
		}
		n := noteFromStepErr(err, cpu.PC())
		switch cpu.Notes.Deliver(cpu, n) {
		case NoteHandled:
			continue
		default:
			return err
		}
	}
}
