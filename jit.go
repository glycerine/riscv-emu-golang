package riscv

// jit.go — JIT manager: block cache, RunJIT dispatch loop.

import (
	"encoding/binary"
	"fmt"
	"os"
	"riscv/internal/jitcall"
	"riscv/ir"
	"unsafe"
)

// debugJIT enables diagnostic logging in emitBlock.
var debugJIT bool

// SetDebugJIT enables/disables emitBlock diagnostic logging.
func SetDebugJIT(on bool) { debugJIT = on }

// chainPatchInfo describes a chain exit that can be patched by Go.
type chainPatchInfo struct {
	targetPC    uint64 // guest PC this exit targets
	patchOffset int    // byte offset of imm64 in MOVABS within the code page
}

// compiledBlock holds a compiled function pointer (native IR or TCC).
type compiledBlock struct {
	fn         uintptr          // native function pointer
	chainEntry uintptr          // entry point for chaining (native IR only)
	chainExits []chainPatchInfo // chain exits for patching (native IR only)
	tccState   unsafe.Pointer   // *C.TCCState for TCC-compiled blocks (nil for native)
	shadow     *compiledBlock   // V2 shadow block for DebugV1V2 comparison
}

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
	noJIT      map[uint64]bool // PCs where translation failed — don't retry
	InterpOnly bool            // debug: force all-interpreter mode
	UseV2      bool            // bench: use V2 lowerer for comparison
	DebugV1V2  bool            // debug: run every block through V1 AND V2, compare results
	trace      bool            // debug: log block executions to stderr

	irAlloc ir.RegAllocator

	// Dispatch counters (for diagnostics).
	DispatchOK      uint64 // jitOK returns to Go dispatch
	DispatchOther   uint64 // non-OK returns (ecall, fault, etc.)
	DispatchInterp  uint64 // interpreter fallback
	DispatchCompile uint64 // new block compilations
	ChainPatched    uint64 // chain exits successfully patched
}

// NewJIT creates a new JIT translation cache using the Fixed Static Mapping allocator.
func NewJIT() *JIT {
	return &JIT{
		noJIT:   make(map[uint64]bool),
		irAlloc: ir.NewFixedStaticAllocator(),
	}
}

// SetAllocStrategy switches the register allocator.
// Valid strategies: "els" (Extended Linear Scan), "fixed" (Fixed Static Mapping).
func (j *JIT) SetAllocStrategy(name string) {
	switch name {
	case "els":
		j.irAlloc = ir.NewAllocator()
	default:
		j.irAlloc = ir.NewFixedStaticAllocator()
	}
	// Clear block cache — compiled blocks used the old allocator.
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.noJIT = make(map[uint64]bool)
}

// NoJITSize returns the number of PCs in the noJIT set (translation failures).
func (j *JIT) NoJITSize() int { return len(j.noJIT) }

// SetTrace enables/disables trace logging to stderr.
func (j *JIT) SetTrace(on bool) { j.trace = on }

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
	if !j.InterpOnly && !j.noJIT[pc] {
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
		j.noJIT[pc] = true
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
				j.DispatchOK++
				// Patch this block's chain exit to jump directly to the target.
				// When a chain exit isn't patched, the slow stub returns here.
				// After patching, future executions jump directly — bypassing Go.
				if len(blk.chainExits) > 0 {
					j.tryPatchChain(blk, cpu.pc)
				}
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
		if !j.InterpOnly && !j.noJIT[pc] {
			res := emitBlock(&cpu.mem, pc)
			if res != nil && res.numInsns > 0 {
				blk, err := j.jitCompileWith(res, j.UseV2)
				if err == nil {
					j.DispatchCompile++
					j.insertBlock(pc, blk)
					continue
				}
				if debugJIT {
					fmt.Fprintf(os.Stderr, "COMPILE_FAIL pc=0x%x numInsns=%d err=%v\n", pc, res.numInsns, err)
				}
			} else if debugJIT {
				if res == nil {
					fmt.Fprintf(os.Stderr, "EMIT_NIL pc=0x%x\n", pc)
				} else {
					fmt.Fprintf(os.Stderr, "EMIT_ZERO pc=0x%x numInsns=%d\n", pc, res.numInsns)
				}
			}
			j.noJIT[pc] = true
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

// patchChainTarget overwrites the 8-byte imm64 in a MOVABS instruction
// at codeBase+patchOffset to redirect to targetAddr.
//
//go:nosplit
func patchChainTarget(codeBase uintptr, patchOffset int, targetAddr uintptr) {
	//nolint:gosec // JIT code patching requires direct memory writes to RWX pages.
	p := (*[8]byte)(unsafe.Pointer(codeBase + uintptr(patchOffset))) //nolint:govet
	binary.LittleEndian.PutUint64(p[:], uint64(targetAddr))
}

// tryPatchChain patches a previous block's chain exit to jump directly
// to the target block, bypassing the Go dispatch loop on future executions.
func (j *JIT) tryPatchChain(blk *compiledBlock, targetPC uint64) {
	target := j.lookupBlock(targetPC)
	if target == nil || target.chainEntry == 0 {
		return
	}
	for _, ce := range blk.chainExits {
		if ce.targetPC == targetPC {
			patchChainTarget(blk.fn, ce.patchOffset, target.chainEntry)
			j.ChainPatched++
			break
		}
	}
}
