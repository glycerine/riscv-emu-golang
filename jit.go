package riscv

// jit.go — JIT manager: block cache, RunJIT dispatch loop.

import (
	"fmt"
	"os"
	"riscv/internal/jitcall"
	"riscv/ir"
)

// JIT status codes returned by compiled blocks.
const (
	jitOK         = 0
	jitEcall      = 1
	jitEbreak     = 2
	jitLoadFault  = 3
	jitStoreFault = 4
	jitIllegal    = 5
)

// Block cache: direct-mapped array replaces map[uint64]*compiledBlock.
// Eliminates Go map hash+probe overhead (~20-30ns) per dispatch cycle.
const (
	blockCacheShift = 12                   // 4096 entries
	blockCacheSize  = 1 << blockCacheShift // must be power of 2
	blockCacheMask  = blockCacheSize - 1
)

type blockCacheEntry struct {
	pc  uint64
	blk *compiledBlock
}

// JIT holds the cache of compiled basic blocks.
type JIT struct {
	cache      [blockCacheSize]blockCacheEntry
	noJIT      u64set // PCs where translation failed — don't retry
	InterpOnly bool   // debug: force all-interpreter mode
	UseV2      bool   // bench: use V2 lowerer for comparison
	DebugV1V2  bool   // debug: run every block through V1 AND V2, compare results
	trace      bool   // debug: log block executions to stderr

	irAlloc ir.RegAllocator
}

// NewJIT creates a new JIT translation cache using the ELS register allocator.
func NewJIT() *JIT {
	return &JIT{
		noJIT:   newU64set(),
		irAlloc: ir.NewAllocator(),
	}
}

// SetAllocStrategy switches the register allocator.
// Valid strategies: "els" (Extended Linear Scan), "fixed" (Fixed Static Mapping).
func (j *JIT) SetAllocStrategy(name string) {
	switch name {
	case "fixed":
		j.irAlloc = ir.NewFixedStaticAllocator()
	default:
		j.irAlloc = ir.NewAllocator()
	}
	// Clear block cache — compiled blocks used the old allocator.
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.noJIT = newU64set()
}

// cacheIdx computes the direct-mapped cache index for a PC.
// Shift right by 1 (not 2) because RVC instructions are 2-byte aligned.
func cacheIdx(pc uint64) uint64 {
	return (pc >> 1) & blockCacheMask
}

// lookupBlock returns the compiled block for pc, or nil.
func (j *JIT) lookupBlock(pc uint64) *compiledBlock {
	idx := cacheIdx(pc)
	if j.cache[idx].pc == pc {
		return j.cache[idx].blk
	}
	return nil
}

// insertBlock stores a compiled block in the cache.
func (j *JIT) insertBlock(pc uint64, blk *compiledBlock) {
	idx := cacheIdx(pc)
	j.cache[idx] = blockCacheEntry{pc, blk}
}

// StepBlock executes one dispatch cycle and returns.
func (j *JIT) StepBlock(cpu *CPU) (ic uint64, err error) {
	pc := cpu.pc

	blk := j.lookupBlock(pc)
	if blk != nil {
		// ── Debug V1-vs-V2 comparison ──
		if j.DebugV1V2 {
			return j.stepBlockDebugV1V2(cpu, pc, blk)
		}

		res := jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
			cpu.mem.Base(), cpu.mem.Mask())
		if j.trace {
			fmt.Fprintf(os.Stderr, "JIT pc=0x%x -> PC=0x%x IC=%d status=%d\n",
				pc, res.PC, res.IC, res.Status)
		}
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

	// Try to translate
	if !j.InterpOnly && !j.noJIT.has(pc) {
		res := emitBlock(&cpu.mem, pc)
		if res != nil && res.numInsns > 0 {
			compiled, cerr := j.jitCompileWith(res, j.UseV2)
			if cerr == nil {
				// When DebugV1V2, also compile a V2 shadow block.
				if j.DebugV1V2 && !j.UseV2 {
					v2, v2err := j.jitCompileWith(res, true)
					if v2err == nil {
						compiled.shadow = v2
					}
				}
				j.insertBlock(pc, compiled)
				return j.StepBlock(cpu) // retry with compiled block
			}
		}
		j.noJIT.add(pc)
	}

	// Interpreter fallback
	err = cpu.step()
	cpu.cycle++
	return 1, err
}

// stepBlockDebugV1V2 runs a block through both V1 and V2, compares all
// register outputs, and panics with full diagnostics on first mismatch.
// The V1 result is used to update cpu state (it's the production path).
func (j *JIT) stepBlockDebugV1V2(cpu *CPU, pc uint64, blk *compiledBlock) (uint64, error) {
	if blk.shadow == nil {
		// No V2 shadow — run V1 only (V2 compilation may have failed).
		res := jitcall.Call(blk.fn, &cpu.x, &cpu.f, &cpu.fcsr,
			cpu.mem.Base(), cpu.mem.Mask())
		cpu.pc = res.PC
		cpu.cycle += res.IC
		return j.stepBlockResult(cpu, res)
	}

	// Snapshot input state.
	var x1, x2 [32]uint64
	var f1, f2 [32]uint64
	var fcsr1, fcsr2 uint32
	x1 = cpu.x
	x2 = cpu.x
	f1 = cpu.f
	f2 = cpu.f
	fcsr1 = cpu.fcsr
	fcsr2 = cpu.fcsr

	// Run V1.
	r1 := jitcall.Call(blk.fn, &x1, &f1, &fcsr1,
		cpu.mem.Base(), cpu.mem.Mask())
	// Run V2.
	r2 := jitcall.Call(blk.shadow.fn, &x2, &f2, &fcsr2,
		cpu.mem.Base(), cpu.mem.Mask())

	// Compare.
	mismatch := false
	if r1.PC != r2.PC || r1.IC != r2.IC || r1.Status != r2.Status {
		fmt.Fprintf(os.Stderr, "DEBUG V1V2 pc=0x%x: Result mismatch\n", pc)
		fmt.Fprintf(os.Stderr, "  V1: PC=0x%x IC=%d Status=%d\n", r1.PC, r1.IC, r1.Status)
		fmt.Fprintf(os.Stderr, "  V2: PC=0x%x IC=%d Status=%d\n", r2.PC, r2.IC, r2.Status)
		mismatch = true
	}
	for i := 0; i < 32; i++ {
		if x1[i] != x2[i] {
			fmt.Fprintf(os.Stderr, "  x[%d]: V1=0x%x V2=0x%x\n", i, x1[i], x2[i])
			mismatch = true
		}
	}
	for i := 0; i < 32; i++ {
		if f1[i] != f2[i] {
			fmt.Fprintf(os.Stderr, "  f[%d]: V1=0x%x V2=0x%x\n", i, f1[i], f2[i])
			mismatch = true
		}
	}
	if mismatch {
		// Dump input state for reproduction.
		fmt.Fprintf(os.Stderr, "  Input x[]: ")
		for i := 0; i < 32; i++ {
			if cpu.x[i] != 0 {
				fmt.Fprintf(os.Stderr, "x[%d]=0x%x ", i, cpu.x[i])
			}
		}
		fmt.Fprintf(os.Stderr, "\n")
		panic(fmt.Sprintf("DebugV1V2: V1/V2 mismatch at pc=0x%x", pc))
	}

	// Apply V1 result to cpu (production path).
	cpu.x = x1
	cpu.f = f1
	cpu.fcsr = fcsr1
	cpu.pc = r1.PC
	cpu.cycle += r1.IC
	return j.stepBlockResult(cpu, r1)
}

func (j *JIT) stepBlockResult(_ *CPU, res jitcall.Result) (uint64, error) {
	switch int(res.Status) {
	case jitOK:
		return res.IC, nil
	case jitEcall:
		return res.IC, ErrEcall
	case jitEbreak:
		return res.IC, ErrEbreak
	case jitLoadFault:
		return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultLoad}
	case jitStoreFault:
		return res.IC, &MemFault{Addr: res.FaultAddr, Width: 8, Kind: FaultStore}
	default:
		return res.IC, nil
	}
}

// RunJIT executes the CPU using JIT-compiled blocks where possible,
// falling back to the interpreter for untranslatable instructions.
func (j *JIT) RunJIT(cpu *CPU) error {
	for {
		// Tohost polling — once per dispatch cycle (block granularity).
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

		// No compiled block — try to translate.
		if !j.InterpOnly && !j.noJIT.has(pc) {
			res := emitBlock(&cpu.mem, pc)
			if res != nil && res.numInsns > 0 {
				blk, err := j.jitCompileWith(res, j.UseV2)
				if err == nil {
					j.insertBlock(pc, blk)
					continue
				}
			}
			j.noJIT.add(pc)
		}

		// Interpret one instruction.
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
