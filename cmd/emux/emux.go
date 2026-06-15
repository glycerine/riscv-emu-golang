package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	riscv "github.com/glycerine/riscv-emu-golang"
)

var ProgramName = "emux"

const (
	defaultEmuxMemorySize        = riscv.Size16GB
	defaultEmuxInstructionBudget = uint64(1 << 20)
	defaultEmuxClockMode         = "idle-jump"
	defaultEmuxMonotonicStartNS  = int64(1)
	defaultEmuxNSPerInstruction  = int64(1)
)

type EmuxConfig struct {
	RunPath           string
	Seed              uint64
	MemorySize        uint64
	InstructionBudget uint64
	JITLazy           bool
	JITAOT            bool
	ClockMode         string
	MonotonicStartNS  int64
	MonotonicStartSet bool
	RealtimeOffsetNS  int64
	NSPerInstruction  int64
	Args              []string
	Env               []string
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	JITStats          *EmuxJITStats
}

type EmuxJITStats struct {
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
// emux -> RunWithJea9Linux -> Jea9Linux.Run -> RunDefaultBudget -> runCachedBudget.
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

func (c *EmuxConfig) DefineFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.RunPath, "run", "", "path to RISCV ELF binary to run")
	fs.Uint64Var(&c.Seed, "seed", 0, "pseudo random number generator seed")
	fs.Uint64Var(&c.MemorySize, "mem", defaultEmuxMemorySize, "guest memory size in bytes")
	fs.Uint64Var(&c.InstructionBudget, "budget", defaultEmuxInstructionBudget, "jea9linux instruction budget per scheduler slice")
	fs.BoolVar(&c.JITLazy, "jitlazy", false, "run with the native lazy JIT instead of the interpreter")
	fs.BoolVar(&c.JITAOT, "jitaot", false, "run with explicit AOT JIT instead of the interpreter")
	fs.StringVar(&c.ClockMode, "clock", defaultEmuxClockMode, "clock mode: idle-jump, ic-tick, or manual")
	fs.Int64Var(&c.MonotonicStartNS, "monotonic-ns", defaultEmuxMonotonicStartNS, "initial monotonic clock value in nanoseconds")
	fs.Int64Var(&c.RealtimeOffsetNS, "realtime-offset-ns", 0, "realtime clock offset from monotonic time in nanoseconds")
	fs.Int64Var(&c.NSPerInstruction, "ns-per-instruction", defaultEmuxNSPerInstruction, "nanoseconds advanced per retired instruction in ic-tick mode")
}

func (c *EmuxConfig) ValidateConfig() error {
	if c.RunPath == "" {
		return fmt.Errorf("-run path required and missing")
	}
	if !fileExists(c.RunPath) {
		return fmt.Errorf("-run path '%v' does not exist", c.RunPath)
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
	if _, err := parseClockMode(c.ClockMode); err != nil {
		return err
	}
	return nil
}

func main() {

	myflags := flag.NewFlagSet("emux", flag.ExitOnError)
	cfg := &EmuxConfig{}
	cfg.DefineFlags(myflags)

	err := myflags.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s command line flag parse error: '%v'\n", ProgramName, err)
		os.Exit(1)
	}
	myflags.Visit(func(f *flag.Flag) {
		if f.Name == "monotonic-ns" {
			cfg.MonotonicStartSet = true
		}
	})
	err = cfg.ValidateConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg.Args = append([]string{cfg.RunPath}, myflags.Args()...)
	cfg.Env = []string{}

	code, err := runEmux(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emux: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runEmux(cfg EmuxConfig) (int, error) {
	cfg = cfg.withDefaults()
	if err := cfg.ValidateConfig(); err != nil {
		return 0, err
	}

	clockMode, err := parseClockMode(cfg.ClockMode)
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
		ClockMode:         clockMode,
		MonotonicStartNS:  cfg.MonotonicStartNS,
		RealtimeOffsetNS:  cfg.RealtimeOffsetNS,
		NSPerInstruction:  cfg.NSPerInstruction,
		InstructionBudget: cfg.InstructionBudget,
		Stdin:             cfg.Stdin,
		Stdout:            cfg.Stdout,
		Stderr:            cfg.Stderr,
		Files:             emuxTimeZoneFiles(),
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
		return runEmuxJIT(cpu, mem, jlinux, cfg.JITAOT, cfg.JITStats)
	}
	return riscv.RunWithJea9Linux(cpu, jlinux)
}

func runEmuxJIT(cpu *riscv.CPU, mem *riscv.GuestMemory, jlinux *riscv.Jea9Linux, aot bool, stats *EmuxJITStats) (int, error) {
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
		*stats = EmuxJITStats{
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

func (c EmuxConfig) withDefaults() EmuxConfig {
	if c.MemorySize == 0 {
		c.MemorySize = defaultEmuxMemorySize
	}
	if c.InstructionBudget == 0 {
		c.InstructionBudget = defaultEmuxInstructionBudget
	}
	if c.ClockMode == "" {
		c.ClockMode = defaultEmuxClockMode
	}
	if c.MonotonicStartNS == 0 && !c.MonotonicStartSet {
		c.MonotonicStartNS = defaultEmuxMonotonicStartNS
	}
	if c.NSPerInstruction == 0 {
		c.NSPerInstruction = defaultEmuxNSPerInstruction
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
	return c
}

func parseClockMode(name string) (riscv.Jea9LinuxClockMode, error) {
	switch strings.ToLower(name) {
	case "", "idle-jump", "idlejump":
		return riscv.Jea9ClockIdleJump, nil
	case "ic-tick", "ictick":
		return riscv.Jea9ClockICTick, nil
	case "manual":
		return riscv.Jea9ClockManual, nil
	default:
		return 0, fmt.Errorf("-clock must be idle-jump, ic-tick, or manual, got %q", name)
	}
}

func seedBytes(seed uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seed)
	return b[:]
}

type emuxZoneInfoSource struct {
	guestPath string
	hostPath  string
}

var (
	emuxTimeZoneFilesOnce sync.Once
	emuxTimeZoneFilesMemo map[string][]byte
)

func emuxTimeZoneFiles() map[string][]byte {
	emuxTimeZoneFilesOnce.Do(func() {
		emuxTimeZoneFilesMemo = loadEmuxTimeZoneFiles(defaultEmuxTimeZoneSources())
	})
	return emuxTimeZoneFilesMemo
}

func defaultEmuxTimeZoneSources() []emuxZoneInfoSource {
	sources := []emuxZoneInfoSource{
		{guestPath: "/usr/share/zoneinfo/", hostPath: "/usr/share/zoneinfo/"},
		{guestPath: "/usr/share/lib/zoneinfo/", hostPath: "/usr/share/lib/zoneinfo/"},
		{guestPath: "/usr/lib/locale/TZ/", hostPath: "/usr/lib/locale/TZ/"},
		{guestPath: "/etc/zoneinfo", hostPath: "/etc/zoneinfo"},
	}
	for _, goroot := range uniqueStrings([]string{runtime.GOROOT(), "/usr/local/go"}) {
		if goroot == "" {
			continue
		}
		zip := filepath.ToSlash(filepath.Join(goroot, "lib", "time", "zoneinfo.zip"))
		sources = append(sources, emuxZoneInfoSource{guestPath: zip, hostPath: zip})
	}
	return sources
}

func uniqueStrings(in []string) []string {
	var out []string
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func loadEmuxTimeZoneFiles(sources []emuxZoneInfoSource) map[string][]byte {
	files := make(map[string][]byte)
	for _, src := range sources {
		if strings.HasSuffix(src.guestPath, ".zip") {
			addEmuxTimeZoneFile(files, src.guestPath, src.hostPath)
			continue
		}
		addEmuxTimeZoneDir(files, src.guestPath, src.hostPath)
	}
	if len(files) == 0 {
		return nil
	}
	return files
}

func addEmuxTimeZoneFile(files map[string][]byte, guestPath, hostPath string) {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return
	}
	files[guestPath] = data
}

func addEmuxTimeZoneDir(files map[string][]byte, guestRoot, hostRoot string) {
	walkRoot, err := filepath.EvalSymlinks(hostRoot)
	if err != nil {
		walkRoot = hostRoot
	}
	info, err := os.Stat(walkRoot)
	if err != nil || !info.IsDir() {
		return
	}
	_ = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeType != 0 && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(walkRoot, path)
		if err != nil || rel == "." {
			return nil
		}
		files[guestRoot+"/"+filepath.ToSlash(rel)] = data
		return nil
	})
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
