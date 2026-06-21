package riscv

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultEmuMemorySize        = Size16GB
	defaultEmuBudget            = "5ms"
	defaultEmuBiosBudget        = "max"
	defaultEmuInstructionBudget = uint64(5 * time.Millisecond)
	defaultEmuRealtimeStartNS   = int64(946684800000000000) // 2000-01-01T00:00:00Z
	emuPRNGMinBudget            = uint64(1 * time.Millisecond)
	emuPRNGMaxBudget            = uint64(500 * time.Millisecond)
)

type EmuConfig struct {
	RunPath             string
	BiosPath            string
	KernelPath          string
	KernelAddr          uint64
	InitrdPath          string
	Append              string
	DTBPath             string
	DumpDTBPath         string
	HostIO              bool
	Net                 bool
	NetDirectTailnet    bool
	EmunetAddr          string
	EmunetTrace         bool
	TsnetDir            string
	TsnetHostname       string
	TsnetAuthKey        string
	TsnetEphemeral      bool
	TsnetGuestIPv4      string
	TsnetDHCPServerIPv4 string
	TsnetDNSIPv4        string
	Machine             string
	Seed                uint64
	Memory              string
	MemorySize          uint64
	BiosRAMSize         uint64
	Budget              string
	InstructionBudget   uint64
	JITLazy             bool
	JITAOT              bool
	Hermit              bool
	Deadlock            bool
	PRNG                bool
	Chaos               bool
	RealtimeOffsetNS    int64
	Idle                string
	List                bool
	Debug               bool
	AttachPID           int
	AttachConsole       int
	AttachEnter         bool
	Args                []string
	Env                 []string
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
	JITStats            *EmuJITStats
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

func (c *EmuConfig) ValidateConfig() error {
	attachMode := c.AttachPID != 0
	if c.List {
		if c.Debug || attachMode {
			return fmt.Errorf("-list cannot be combined with -debug, -pid, or -console")
		}
		if c.RunPath != "" || c.BiosPath != "" {
			return fmt.Errorf("-list cannot be combined with -run or -bios")
		}
		return nil
	}
	if c.Debug {
		if attachMode || c.AttachConsole > 0 {
			return fmt.Errorf("-debug cannot be combined with -pid or -console")
		}
		if c.RunPath != "" || c.BiosPath != "" {
			return fmt.Errorf("-debug cannot be combined with -run or -bios")
		}
		return nil
	}
	if c.AttachPID == 0 && c.AttachConsole > 0 {
		return fmt.Errorf("-console requires -pid")
	}
	if attachMode {
		if c.AttachPID <= 0 {
			return fmt.Errorf("-pid must be positive in attach mode")
		}
		if c.AttachConsole < 0 {
			return fmt.Errorf("-console must be >= 0 in attach mode")
		}
		if c.RunPath != "" || c.BiosPath != "" {
			return fmt.Errorf("-pid/-console attach mode cannot be combined with -run or -bios")
		}
		return nil
	}
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
		case c.HostIO:
			return fmt.Errorf("-hostio requires -bios")
		case c.Net:
			return fmt.Errorf("-net requires -bios")
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
	if err := c.resolveMemory(); err != nil {
		return err
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
	if _, _, err := c.idleSleepCap(); err != nil {
		return err
	}
	for _, flag := range []struct {
		name  string
		value string
	}{
		{"-tsnet-guest-ipv4", c.TsnetGuestIPv4},
		{"-tsnet-dhcp-server-ipv4", c.TsnetDHCPServerIPv4},
		{"-tsnet-dns-ipv4", c.TsnetDNSIPv4},
	} {
		if err := validateOptionalIPv4Flag(flag.name, flag.value); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalIPv4Flag(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !ip.Is4() {
		if err == nil {
			err = fmt.Errorf("not an IPv4 address")
		}
		return fmt.Errorf("%s must be an IPv4 address, got %q: %w", name, value, err)
	}
	return nil
}

func RunEmu(cfg EmuConfig) (int, error) {
	cfg = cfg.withDefaults()
	if err := cfg.ValidateConfig(); err != nil {
		return 0, err
	}
	if cfg.List {
		return 0, listEmuInstances(cfg.Stdout)
	}
	if cfg.Debug {
		resolved, err := resolveDebugAttach(cfg)
		if err != nil {
			return 0, err
		}
		return attachEmuConsole(resolved)
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

	if cfg.JITLazy || cfg.JITAOT {
		return runEmuJIT(cpu, mem, jlinux, cfg.JITAOT, cfg.JITStats)
	}
	return RunWithJea9Linux(cpu, jlinux)
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

func runEmuJIT(cpu *CPU, mem *GuestMemory, jlinux *Jea9Linux, aot bool, stats *EmuJITStats) (int, error) {
	jit := NewJIT()
	defer jit.Close()

	jit.AutoAOT = aot
	if aot {
		if err := jit.InstallAOTFromMem(mem); err != nil {
			panicf("jit.InstallAOTFromMem gave error: '%v'", err)
			return 0, err
		}
	}

	code, err := RunWithJea9LinuxJIT(cpu, jit, jlinux)
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

func (c *EmuConfig) resolveMemory() error {
	if c.Memory != "" {
		parsed, err := parseEmuMemorySize(c.Memory)
		if err != nil {
			return err
		}
		if c.BiosPath != "" {
			c.BiosRAMSize = parsed
			slab, err := biosSlabSizeForRAM(parsed)
			if err != nil {
				return err
			}
			c.MemorySize = slab
		} else {
			c.MemorySize = parsed
			c.BiosRAMSize = 0
		}
	} else if c.MemorySize == 0 {
		c.MemorySize = defaultEmuMemorySize
	}
	if c.MemorySize == 0 || c.MemorySize&(c.MemorySize-1) != 0 {
		return fmt.Errorf("-mem must resolve to a non-zero power-of-two guest memory slab, got %d", c.MemorySize)
	}
	if c.MemorySize > MaxGuestMemory {
		return fmt.Errorf("-mem %d exceeds max guest memory %d", c.MemorySize, MaxGuestMemory)
	}
	if c.BiosPath != "" {
		if c.BiosRAMSize == 0 {
			if c.MemorySize <= virtRAMBase {
				return fmt.Errorf("-mem %#x is too small for BIOS RAM base %#x", c.MemorySize, virtRAMBase)
			}
			c.BiosRAMSize = c.MemorySize - virtRAMBase
		}
		if c.BiosRAMSize == 0 {
			return fmt.Errorf("-mem must provide non-zero BIOS RAM")
		}
		if c.BiosRAMSize > c.MemorySize-virtRAMBase {
			return fmt.Errorf("-mem BIOS RAM %#x exceeds guest slab %#x above RAM base %#x", c.BiosRAMSize, c.MemorySize, virtRAMBase)
		}
	}
	return nil
}

func parseEmuMemorySize(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("-mem must not be empty")
	}
	n := strings.ReplaceAll(raw, "_", "")
	n = strings.ReplaceAll(n, " ", "")
	if strings.HasPrefix(n, "-") {
		return 0, fmt.Errorf("-mem must be positive, got %q", raw)
	}
	suffixStart := len(n)
	for suffixStart > 0 {
		ch := n[suffixStart-1]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
			suffixStart--
			continue
		}
		break
	}
	digits := n[:suffixStart]
	suffix := strings.ToLower(n[suffixStart:])
	if digits == "" {
		return 0, fmt.Errorf("-mem must start with a number, got %q", raw)
	}
	mul := uint64(1)
	switch suffix {
	case "":
	case "b":
	case "k", "kb", "kib":
		mul = 1 << 10
	case "m", "mb", "mib":
		mul = 1 << 20
	case "g", "gb", "gib":
		mul = 1 << 30
	case "t", "tb", "tib":
		mul = 1 << 40
	default:
		return 0, fmt.Errorf("-mem has unknown size suffix %q in %q", suffix, raw)
	}
	base := 10
	if suffix == "" {
		base = 0
	}
	v, err := strconv.ParseUint(digits, base, 64)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("-mem must be a positive size, got %q", raw)
	}
	if v > ^uint64(0)/mul {
		return 0, fmt.Errorf("-mem %q overflows uint64", raw)
	}
	return v * mul, nil
}

func biosSlabSizeForRAM(ram uint64) (uint64, error) {
	if ram == 0 {
		return 0, fmt.Errorf("-mem must provide non-zero BIOS RAM")
	}
	if ram > ^uint64(0)-virtRAMBase {
		return 0, fmt.Errorf("-mem BIOS RAM %#x overflows RAM base %#x", ram, virtRAMBase)
	}
	need := virtRAMBase + ram
	slab, ok := nextPowerOfTwo64(need)
	if !ok || slab > MaxGuestMemory {
		return 0, fmt.Errorf("-mem BIOS RAM %#x needs guest slab %#x, exceeding max %#x", ram, slab, MaxGuestMemory)
	}
	return slab, nil
}

func nextPowerOfTwo64(v uint64) (uint64, bool) {
	if v == 0 {
		return 1, true
	}
	if v&(v-1) == 0 {
		return v, true
	}
	if v > 1<<63 {
		return 0, false
	}
	return uint64(1) << bits.Len64(v), true
}

func (c EmuConfig) timeMode() TimeMode {
	if c.Hermit {
		return HermitTime
	}
	return RealTime
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

func (c EmuConfig) clockPolicy() (ClockPolicy, error) {
	mode, err := c.timingMode()
	if err != nil {
		return 0, err
	}
	switch mode {
	case emuTimingPRNG:
		return ClockPolicyPRNG, nil
	case emuTimingChaos:
		return ClockPolicyChaos, nil
	default:
		return ClockPolicyOnlyDeadlockAdvances, nil
	}
}

func (c EmuConfig) schedulerConfig(budget uint64) Jea9LinuxSchedulerConfig {
	mode, err := c.timingMode()
	if err != nil {
		return Jea9LinuxSchedulerConfig{}
	}
	switch mode {
	case emuTimingDeadlock:
		return Jea9LinuxSchedulerConfig{Mode: Jea9SchedulerDeadlock}
	case emuTimingPRNG:
		return Jea9LinuxSchedulerConfig{
			Mode:                   Jea9SchedulerDST,
			MinQuantumRetired:      emuPRNGMinBudget,
			MaxQuantumRetired:      emuPRNGMaxBudget,
			LowPriorityDenominator: 10,
		}
	case emuTimingChaos:
		return Jea9LinuxSchedulerConfig{
			Mode:              Jea9SchedulerChaos,
			MinQuantumRetired: budget,
			MaxQuantumRetired: emuPRNGMaxBudget,
		}
	default:
		return Jea9LinuxSchedulerConfig{
			Mode:              Jea9SchedulerRoundRobin,
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

func (c EmuConfig) applyIdleSleepCap() (func(), error) {
	d, ok, err := c.idleSleepCap()
	if err != nil {
		return nil, err
	}
	if !ok {
		return func() {}, nil
	}
	return SetBiosIdleSleepCap(d), nil
}

func (c EmuConfig) idleSleepCap() (time.Duration, bool, error) {
	raw := strings.TrimSpace(c.Idle)
	if raw == "" {
		return 0, false, nil
	}
	if raw == "0" {
		return 0, true, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, false, fmt.Errorf("-idle must be a non-negative duration, got %q", c.Idle)
	}
	return d, true, nil
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
