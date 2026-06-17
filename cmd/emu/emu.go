package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	riscv "github.com/glycerine/riscv-emu-golang"
)

var ProgramName = "emu"

const (
	defaultEmuMemorySize        = riscv.Size16GB
	defaultEmuBudget            = "5ms"
	defaultEmuBiosBudget        = "max"
	defaultEmuInstructionBudget = uint64(5 * time.Millisecond)
	defaultEmuRealtimeStartNS   = int64(946684800000000000) // 2000-01-01T00:00:00Z
	emuPRNGMinBudget            = uint64(1 * time.Millisecond)
	emuPRNGMaxBudget            = uint64(500 * time.Millisecond)
)

type EmuConfig struct {
	RunPath           string
	BiosPath          string
	KernelPath        string
	KernelAddr        uint64
	InitrdPath        string
	Append            string
	DTBPath           string
	DumpDTBPath       string
	Machine           string
	Seed              uint64
	MemorySize        uint64
	Budget            string
	InstructionBudget uint64
	JITLazy           bool
	JITAOT            bool
	Hermit            bool
	Deadlock          bool
	PRNG              bool
	Chaos             bool
	RealtimeOffsetNS  int64
	Args              []string
	Env               []string
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	JITStats          *EmuJITStats
}

type EmuJITStats struct {
	DispatchOK             uint64
	DispatchCompile        uint64
	DispatchInterp         uint64
	ChainPatchedJalr       uint64
	JalrICMisses           uint64
	JalrICDeopts           uint64
	AOTSegmentsInstalled   uint64
	AOTBlocksInstalled     uint64
	AOTCompileFailures     uint64
	AOTDecoderCacheLookups uint64
	AOTDecoderCacheHits    uint64
	AOTDecoderCacheMisses  uint64
	AOTDecoderCacheOutside uint64
}

// FAQ: why is the default mode a "cached" interpreter?
//
// A: because the interpreter path does not use CPU.Step()
// instruction-by-instruction directly. It goes
// through the repo’s fast interpreter path:
//
// emu -> RunWithJea9Linux -> Jea9Linux.Run -> RunDefaultBudget -> runCachedBudget.
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

func (c *EmuConfig) DefineFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.RunPath, "run", "", "path to RISCV ELF binary to run")
	fs.StringVar(&c.BiosPath, "bios", "", "path to RISCV machine-mode BIOS/firmware ELF to boot")
	fs.StringVar(&c.KernelPath, "kernel", "", "path to kernel or next-stage payload to load with -bios")
	fs.Uint64Var(&c.KernelAddr, "kernel-addr", 0, "guest physical address for raw -kernel payloads; default 0x80200000")
	fs.StringVar(&c.InitrdPath, "initrd", "", "path to initrd image to load and advertise in the BIOS FDT")
	fs.StringVar(&c.Append, "append", "", "kernel command line for generated BIOS FDT bootargs")
	fs.StringVar(&c.DTBPath, "dtb", "", "path to external flattened device tree blob for -bios")
	fs.StringVar(&c.DumpDTBPath, "dump-dtb", "", "write the BIOS FDT blob to this path before boot")
	fs.StringVar(&c.Machine, "machine", "virt", "machine model for -bios; currently only virt")
	fs.Uint64Var(&c.Seed, "seed", 0, "pseudo random number generator seed")
	fs.Uint64Var(&c.MemorySize, "mem", defaultEmuMemorySize, "guest memory size in bytes")
	fs.StringVar(&c.Budget, "budget", "", "scheduler/run budget as an instruction count, duration, or max; defaults to 5ms for -run and max for -bios")
	fs.BoolVar(&c.JITLazy, "jitlazy", false, "run with the native lazy JIT instead of the interpreter")
	fs.BoolVar(&c.JITAOT, "jitaot", false, "run with explicit AOT JIT instead of the interpreter")
	fs.BoolVar(&c.Hermit, "hermit", false, "disable host filesystem passthrough")
	fs.BoolVar(&c.Deadlock, "deadlock", false, "run each thread until it blocks before scheduling another thread (at most one of -deadlock -prng or -chaos may be given; if none the default is a fixed quantum of -budget duration)")
	fs.BoolVar(&c.PRNG, "prng", false, "use deterministic PRNG scheduling quantum and clock advancement")
	fs.BoolVar(&c.Chaos, "chaos", false, "use deterministic chaos scheduling")
	fs.Int64Var(&c.RealtimeOffsetNS, "init", defaultEmuRealtimeStartNS, "initial realtime clock value in nanoseconds since Unix epoch; default is 2000-01-01T00:00:00Z")
}

func (c *EmuConfig) ValidateConfig() error {
	if c.RunPath == "" && c.BiosPath == "" {
		return fmt.Errorf("one of -run or -bios is required")
	}
	if c.RunPath != "" && c.BiosPath != "" {
		return fmt.Errorf("-run and -bios are mutually exclusive")
	}
	if c.machine() != "virt" {
		return fmt.Errorf("-machine %q is not supported; only \"virt\" is available", c.machine())
	}
	pathFlag := "-run"
	path := c.RunPath
	if c.BiosPath != "" {
		pathFlag = "-bios"
		path = c.BiosPath
	}
	if !fileExists(path) {
		return fmt.Errorf("%s path '%v' does not exist", pathFlag, path)
	}
	if c.RunPath != "" {
		switch {
		case c.KernelPath != "":
			return fmt.Errorf("-kernel requires -bios")
		case c.KernelAddr != 0:
			return fmt.Errorf("-kernel-addr requires -bios")
		case c.InitrdPath != "":
			return fmt.Errorf("-initrd requires -bios")
		case c.Append != "":
			return fmt.Errorf("-append requires -bios")
		case c.DTBPath != "":
			return fmt.Errorf("-dtb requires -bios")
		case c.DumpDTBPath != "":
			return fmt.Errorf("-dump-dtb requires -bios")
		}
	}
	if c.KernelPath != "" && !fileExists(c.KernelPath) {
		return fmt.Errorf("-kernel path '%v' does not exist", c.KernelPath)
	}
	if c.InitrdPath != "" && !fileExists(c.InitrdPath) {
		return fmt.Errorf("-initrd path '%v' does not exist", c.InitrdPath)
	}
	if c.DTBPath != "" && !fileExists(c.DTBPath) {
		return fmt.Errorf("-dtb path '%v' does not exist", c.DTBPath)
	}
	if c.DTBPath != "" && c.InitrdPath != "" {
		return fmt.Errorf("-dtb and -initrd cannot be combined yet; provide initrd properties in the external DTB")
	}
	if c.DTBPath != "" && c.Append != "" {
		return fmt.Errorf("-dtb and -append cannot be combined yet; provide bootargs in the external DTB")
	}
	if c.MemorySize == 0 || c.MemorySize&(c.MemorySize-1) != 0 {
		return fmt.Errorf("-mem must be a non-zero power of two, got %d", c.MemorySize)
	}
	if c.MemorySize > riscv.MaxGuestMemory {
		return fmt.Errorf("-mem %d exceeds max guest memory %d", c.MemorySize, riscv.MaxGuestMemory)
	}
	if c.JITLazy && c.JITAOT {
		return fmt.Errorf("-jitlazy and -jitaot are mutually exclusive")
	}
	if _, err := c.timingMode(); err != nil {
		return err
	}
	if _, err := c.schedulerBudget(); err != nil {
		return err
	}
	return nil
}

func main() {

	myflags := flag.NewFlagSet("emu", flag.ExitOnError)
	cfg := &EmuConfig{}
	cfg.DefineFlags(myflags)

	err := myflags.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s command line flag parse error: '%v'\n", ProgramName, err)
		os.Exit(1)
	}
	err = cfg.ValidateConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg.Args = append([]string{cfg.programPath()}, myflags.Args()...)
	vv("cfg.Args = '%#v'", cfg.Args)

	code, err := runEmu(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emu: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runEmu(cfg EmuConfig) (int, error) {
	cfg = cfg.withDefaults()
	if err := cfg.ValidateConfig(); err != nil {
		return 0, err
	}

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

	mem, err := riscv.NewGuestMemory(cfg.MemorySize)
	if err != nil {
		return 0, err
	}
	defer mem.Free()

	elf, err := riscv.LoadELF(mem, cfg.RunPath)
	if err != nil {
		return 0, err
	}

	cpu := riscv.NewCPU(*mem)
	jlinux := riscv.NewJea9Linux(riscv.Jea9LinuxOptions{
		EntropySeed:       seedBytes(cfg.Seed),
		TimeMode:          cfg.timeMode(),
		ClockMode:         riscv.Jea9ClockIdleJump,
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
	if err := jlinux.InitELFStack(cpu, elf, riscv.Jea9LinuxStartOptions{
		Args:     args,
		Env:      append([]string(nil), cfg.Env...),
		ExecPath: args[0],
	}); err != nil {
		return 0, err
	}

	if cfg.JITLazy || cfg.JITAOT {
		return runEmuJIT(cpu, mem, jlinux, cfg.JITAOT, cfg.JITStats)
	}
	return riscv.RunWithJea9Linux(cpu, jlinux)
}

func (c EmuConfig) programPath() string {
	if c.BiosPath != "" {
		return c.BiosPath
	}
	return c.RunPath
}

func (c EmuConfig) machine() string {
	if c.Machine == "" {
		return "virt"
	}
	return c.Machine
}

func runEmuJIT(cpu *riscv.CPU, mem *riscv.GuestMemory, jlinux *riscv.Jea9Linux, aot bool, stats *EmuJITStats) (int, error) {
	jit := riscv.NewJIT()
	defer jit.Close()

	jit.AutoAOT = aot
	if aot {
		if err := jit.InstallAOTFromMem(mem); err != nil {
			panicf("jit.InstallAOTFromMem gave error: '%v'", err)
			return 0, err
		}
	}

	code, err := riscv.RunWithJea9LinuxJIT(cpu, jit, jlinux)
	if stats != nil {
		*stats = EmuJITStats{
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

func (c EmuConfig) withDefaults() EmuConfig {
	if c.MemorySize == 0 {
		c.MemorySize = defaultEmuMemorySize
	}
	if c.Budget == "" && c.InstructionBudget == 0 {
		if c.BiosPath != "" {
			c.Budget = defaultEmuBiosBudget
		} else {
			c.Budget = defaultEmuBudget
		}
	}
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.Env == nil {
		if c.Hermit {
			c.Env = []string{}
		} else {
			c.Env = os.Environ()
		}
	}
	return c
}

func (c EmuConfig) timeMode() riscv.TimeMode {
	if c.Hermit {
		return riscv.HermitTime
	}
	return riscv.RealTime
}

type emuTimingMode uint8

const (
	emuTimingFixed emuTimingMode = iota
	emuTimingDeadlock
	emuTimingPRNG
	emuTimingChaos
)

func (c EmuConfig) timingMode() (emuTimingMode, error) {
	n := 0
	if c.Deadlock {
		n++
	}
	if c.PRNG {
		n++
	}
	if c.Chaos {
		n++
	}
	if n > 1 {
		return 0, fmt.Errorf("-deadlock, -prng, and -chaos are mutually exclusive")
	}
	switch {
	case c.Deadlock:
		return emuTimingDeadlock, nil
	case c.PRNG:
		return emuTimingPRNG, nil
	case c.Chaos:
		return emuTimingChaos, nil
	default:
		return emuTimingFixed, nil
	}
}

func (c EmuConfig) clockPolicy() (riscv.ClockPolicy, error) {
	mode, err := c.timingMode()
	if err != nil {
		return 0, err
	}
	switch mode {
	case emuTimingPRNG:
		return riscv.ClockPolicyPRNG, nil
	case emuTimingChaos:
		return riscv.ClockPolicyChaos, nil
	default:
		return riscv.ClockPolicyOnlyDeadlockAdvances, nil
	}
}

func (c EmuConfig) schedulerConfig(budget uint64) riscv.Jea9LinuxSchedulerConfig {
	mode, err := c.timingMode()
	if err != nil {
		return riscv.Jea9LinuxSchedulerConfig{}
	}
	switch mode {
	case emuTimingDeadlock:
		return riscv.Jea9LinuxSchedulerConfig{Mode: riscv.Jea9SchedulerDeadlock}
	case emuTimingPRNG:
		return riscv.Jea9LinuxSchedulerConfig{
			Mode:                   riscv.Jea9SchedulerDST,
			MinQuantumRetired:      emuPRNGMinBudget,
			MaxQuantumRetired:      emuPRNGMaxBudget,
			LowPriorityDenominator: 10,
		}
	case emuTimingChaos:
		return riscv.Jea9LinuxSchedulerConfig{
			Mode:              riscv.Jea9SchedulerChaos,
			MinQuantumRetired: budget,
			MaxQuantumRetired: emuPRNGMaxBudget,
		}
	default:
		return riscv.Jea9LinuxSchedulerConfig{
			Mode:              riscv.Jea9SchedulerRoundRobin,
			MinQuantumRetired: budget,
			MaxQuantumRetired: budget,
		}
	}
}

func (c EmuConfig) runInstructionBudget(budget uint64) (uint64, error) {
	mode, err := c.timingMode()
	if err != nil {
		return 0, err
	}
	switch mode {
	case emuTimingFixed, emuTimingChaos:
		return budget, nil
	default:
		return 0, nil
	}
}

func (c EmuConfig) schedulerBudget() (uint64, error) {
	if c.Budget == "" {
		if c.InstructionBudget != 0 {
			return c.InstructionBudget, nil
		}
		if c.BiosPath != "" {
			return ^uint64(0), nil
		}
		return parseEmuBudget(defaultEmuBudget)
	}
	return parseEmuBudget(c.Budget)
}

func parseEmuBudget(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("-budget must not be empty")
	}
	switch strings.ToLower(raw) {
	case "max", "maxuint64", "uint64max", "^uint64(0)":
		return ^uint64(0), nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("-budget must be positive, got %q", raw)
		}
		return uint64(d), nil
	}
	n := strings.ReplaceAll(raw, "_", "")
	if strings.HasPrefix(n, "-") {
		return 0, fmt.Errorf("-budget must be positive, got %q", raw)
	}
	if v, err := strconv.ParseUint(n, 10, 64); err == nil {
		if v == 0 {
			return 0, fmt.Errorf("-budget must be positive, got %q", raw)
		}
		return v, nil
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("-budget must be a positive instruction count or duration, got %q", raw)
	}
	if math.Trunc(f) != f {
		return 0, fmt.Errorf("-budget instruction count must be integral, got %q", raw)
	}
	if f > float64(^uint64(0)) {
		return 0, fmt.Errorf("-budget overflows uint64, got %q", raw)
	}
	v := uint64(f)
	if v == 0 {
		return 0, fmt.Errorf("-budget must be positive, got %q", raw)
	}
	return v, nil
}

func seedBytes(seed uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seed)
	return b[:]
}

func fileExists(name string) bool {
	fi, err := os.Stat(name)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		return false
	}
	return true
}
