package riscv

import (
// "time"
)

// see cfg.go for all EmuConfig

// FAQ: why is the default mode a "cached" interpreter?
//
// A: because the interpreter path does not use CPU.Step()
// instruction-by-instruction directly. It goes
// through the repo’s fast interpreter path:
//
// emu -> RunWithJea9LinuxInterp -> Jea9Linux.Run -> RunDefaultBudget -> runCachedBudget.
//
// What is cached is the decode, not execution results.
// runCached uses a DecoderCache keyed by guest PC;
// on first visit to an instruction, it fetches/decodes
// it into a DecodedInsn slot, flattens the opcode
// into slot.op, stores operands/immediates, and wires
// slot.next for common fall-through paths. Later visits
// dispatch straight from that predecoded slot through
// the big switch, avoiding repeated fetch/decode work.
//
// Relevant spots:
// run_cached.go (line 8): comment describing the decoder cache.
// run_cached.go (line 185): RunDefaultBudget creates a 256KB DecoderCache.
// run_cached.go (line 220): cold slots get populated once.
// decoder_cache.go (line 3): DecodedInsn/DecoderCache.
//
// So: it is still an interpreter, not JIT/native code.
// "Cached" just means "software decode cache". A completely
// descriptive but awful moniker would be "budgeted decoder-cached
// interpreter".

// RunEmu is the top-most entry point into the riscv
// package. It is called directly by the cmd/emu command line tool.
func RunEmu(cfg *EmuConfig) (int, error) {
	cfg.setDefaults()
	if err := cfg.ValidateConfig(); err != nil {
		return 0, err
	}
	if cfg.List {
		return 0, listEmuInstances(cfg.Stdout)
	}
	if cfg.Debug {
		err := resolveDebugAttach(cfg)
		if err != nil {
			return 0, err
		}
		return attachEmuConsole(cfg)
	}
	if cfg.AttachPID != 0 {
		return attachEmuConsole(cfg)
	}
	restoreIdle, err := cfg.applyIdleSleepCap()
	if err != nil {
		return 0, err
	}
	defer restoreIdle()

	budget, err := cfg.schedulerBudget()
	if err != nil {
		return 0, err
	}
	if cfg.BiosPath != "" {
		return runEmuBios(cfg, budget)
	}
	clockPolicy, err := cfg.clockPolicy()
	if err != nil {
		return 0, err
	}
	instructionBudget, err := cfg.runInstructionBudget(budget)
	if err != nil {
		return 0, err
	}

	mem, err := NewGuestMemory(cfg.MemorySize)
	if err != nil {
		return 0, err
	}
	defer mem.Free()

	elf, err := LoadELF(mem, cfg.RunPath)
	if err != nil {
		return 0, err
	}

	cpu := NewCPU(*mem)
	jlinux := NewJea9Linux(Jea9LinuxOptions{
		EntropySeed:       seedBytes(cfg.Seed),
		TimeMode:          cfg.timeMode(),
		ClockMode:         Jea9ClockIdleJump,
		ClockPolicy:       clockPolicy,
		MonotonicStartNS:  1,                        // cannot be 0, crashes Go runtime.
		RealtimeOffsetNS:  cfg.RealtimeOffsetNS - 1, // because MonotonicStartNS must start at 1.
		InstructionBudget: instructionBudget,
		Scheduler:         cfg.schedulerConfig(budget),
		Stdin:             cfg.Stdin,
		Stdout:            cfg.Stdout,
		Stderr:            cfg.Stderr,
		AllowAllHostFiles: !cfg.Hermit,
	})

	args := append([]string(nil), cfg.Args...)
	if len(args) == 0 {
		args = []string{cfg.RunPath}
	}
	if err := jlinux.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     args,
		Env:      append([]string(nil), cfg.Env...),
		ExecPath: args[0],
	}); err != nil {
		return 0, err
	}
	doJIT := cfg.JITLazy || cfg.JITAOT
	if !doJIT {
		// interpreter

		return RunWithJea9LinuxInterp(cpu, jlinux) // in jea9linux.go

	} else {
		// JIT. of some flavor. AOT or Lazy.

		jit := NewJIT()
		defer jit.Close()

		jit.SandboxMem = cfg.SandboxMem
		jit.AutoAOT = cfg.JITAOT
		if jit.AutoAOT {
			if err := jit.InstallAOTFromMem(mem); err != nil {
				panicf("jit.InstallAOTFromMem gave error: '%v'", err)
				return 0, err
			}
		}

		code, err := RunWithJea9LinuxJIT(cpu, jit, jlinux) // in jea9linux.go
		if cfg.JITStats != nil {
			*cfg.JITStats = EmuJITStats{
				DispatchOK:             jit.DispatchOK,
				DispatchCompile:        jit.DispatchCompile,
				DispatchInterp:         jit.DispatchInterp,
				ChainPatchedJalr:       jit.ChainPatchedJalr,
				JalrICMisses:           jit.JalrICMisses,
				JalrICDeopts:           jit.JalrICDeopts,
				AOTSegmentsInstalled:   jit.AOTSegmentsInstalled,
				AOTBlocksInstalled:     jit.AOTBlocksInstalled,
				AOTCompileFailures:     jit.AOTCompileFailures,
				AOTDecoderCacheLookups: jit.AOTDecoderCacheLookups,
				AOTDecoderCacheHits:    jit.AOTDecoderCacheHits,
				AOTDecoderCacheMisses:  jit.AOTDecoderCacheMisses,
				AOTDecoderCacheOutside: jit.AOTDecoderCacheOutside,
			}
		}
		return code, err
	}
}
