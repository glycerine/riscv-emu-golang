package riscv

// jit_tcc_dispatch.go — TCC-backend dispatch: TccStepBlock and TccRunJIT.
// These mirror StepBlock and RunJIT but use tccEmitBlock + tccJitCompileWith
// instead of the native IR pipeline.

import (
	"fmt"
	"os"
	"riscv/internal/jitcall"
)

// TccStepBlock executes one dispatch cycle using TCC compilation.
func (j *JIT) TccStepBlock(cpu *CPU) (ic uint64, err error) {
	pc := cpu.pc

	blk := j.lookupBlock(pc)
	if blk != nil {
		res := jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
			cpu.mem.Base(), cpu.mem.Mask())
		cpu.pc = res.PC
		cpu.cycle += res.IC

		switch int(res.Status) {
		case jitOK:
			return res.IC, nil
		case jitEcall:
			if cpu.mtvec != 0 {
				cpu.mepc = cpu.pc
				cpu.mcause = 8
				cpu.mtval = 0
				cpu.pc = cpu.mtvec
				return res.IC, nil
			}
			return res.IC, ErrEcall
		case jitEbreak:
			return res.IC, ErrEbreak
		case jitLoadFault:
			return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
		case jitStoreFault:
			return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
		default:
			err = cpu.step()
			cpu.cycle++
			return res.IC + 1, err
		}
	}

	// Try to translate via TCC.
	if !j.InterpOnly && !j.noJIT.has(pc) {
		res := tccEmitBlock(&cpu.mem, pc)
		if res != nil && res.numInsns > 0 {
			compiled, cerr := j.tccJitCompileWith(res)
			if cerr == nil {
				j.insertBlock(pc, compiled)
				return j.TccStepBlock(cpu)
			}
		}
		j.noJIT.add(pc)
	}

	// Interpreter fallback.
	err = cpu.step()
	cpu.cycle++
	return 1, err
}

// TccRunJIT executes the CPU using TCC-compiled blocks.
func (j *JIT) TccRunJIT(cpu *CPU) error {
	for {
		if cpu.watchAddr != 0 {
			if v, _ := cpu.mem.Load64(cpu.watchAddr); v != 0 {
				panic(&ExitError{Code: tohostExitCode(v)})
			}
		}

		pc := cpu.pc

		blk := j.lookupBlock(pc)
		if blk != nil {
			res := jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
				cpu.mem.Base(), cpu.mem.Mask())
			cpu.pc = res.PC
			cpu.cycle += res.IC

			switch int(res.Status) {
			case jitOK:
				j.DispatchOK++
				continue

			case jitEcall:
				if cpu.mtvec != 0 {
					cpu.mepc = cpu.pc
					cpu.mcause = 8
					cpu.mtval = 0
					cpu.pc = cpu.mtvec
					continue
				}
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

		// No compiled block — try to translate via TCC.
		if !j.InterpOnly && !j.noJIT.has(pc) {
			res := tccEmitBlock(&cpu.mem, pc)
			if res != nil && res.numInsns > 0 {
				blk, err := j.tccJitCompileWith(res)
				if err == nil {
					j.DispatchCompile++
					j.insertBlock(pc, blk)
					continue
				}
				if debugJIT {
					fmt.Fprintf(os.Stderr, "TCC_COMPILE_FAIL pc=0x%x numInsns=%d err=%v\n", pc, res.numInsns, err)
				}
			} else if debugJIT {
				if res == nil {
					fmt.Fprintf(os.Stderr, "TCC_EMIT_NIL pc=0x%x\n", pc)
				} else {
					fmt.Fprintf(os.Stderr, "TCC_EMIT_ZERO pc=0x%x numInsns=%d\n", pc, res.numInsns)
				}
			}
			j.noJIT.add(pc)
		}

		// Interpret one instruction.
		j.DispatchInterp++
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
