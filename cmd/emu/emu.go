package main

import (
	"flag"
	"fmt"
	"os"

	riscv "github.com/glycerine/riscv-emu-golang"
)

const (
	programName                    = "emu"
	defaultEmuRealtimeStartNS      = int64(946684800000000000) // 2000-01-01T00:00:00Z
	defaultEmuRunBudgetDescription = "5ms"
)

// notes on Device Tree Blob (DTB) for describing hardware to Linux:
//
// The make linux command does not need explicit DTB flags.
// Current behavior:
//
// If you do not pass -dtb, emu generates a virt FDT internally
// in the riscv package's BIOS machine support.
//
// -machine defaults to virt; it is currently the only supported value, so
// adding -machine virt is redundant.
// -dump-dtb path just writes the generated DTB to disk for inspection/debugging.
//
// For OpenSBI, we load the DTB into guest memory and pass its address in a1
// before entering firmware. The generated FDT exposes an ns16550 UART at
// 0x10000000, so Linux console bootargs should target ttyS0/uart8250.
func main() {
	myflags := flag.NewFlagSet("emu", flag.ExitOnError)
	cfg := &riscv.EmuConfig{}
	defineFlags(myflags, cfg)

	if err := myflags.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s command line flag parse error: '%v'\n", programName, err)
		os.Exit(1)
	}
	if err := cfg.ValidateConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg.Args = append([]string{programPath(cfg)}, myflags.Args()...)

	code, err := riscv.RunEmu(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emu: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func defineFlags(fs *flag.FlagSet, c *riscv.EmuConfig) {
	if c.MemorySize == 0 {
		c.MemorySize = riscv.Size16GB
	}
	fs.StringVar(&c.RunPath, "run", "", "path to RISCV ELF binary to run")
	fs.StringVar(&c.BiosPath, "bios", "", "path to RISCV machine-mode BIOS/firmware ELF to boot. bios mode is non-deterministic and conflicts with -hermit")
	fs.StringVar(&c.KernelPath, "kernel", "", "path to kernel or next-stage payload to load with -bios")
	fs.Uint64Var(&c.KernelAddr, "kernel-addr", 0, "guest physical address for raw -kernel payloads; default 0x80200000")
	fs.StringVar(&c.InitrdPath, "initrd", "", "path to initrd image to load and advertise in the BIOS FDT")
	fs.StringVar(&c.Append, "append", "", "kernel command line for generated BIOS FDT bootargs")
	fs.StringVar(&c.DTBPath, "dtb", "", "path to external flattened device tree blob for -bios")
	fs.StringVar(&c.DumpDTBPath, "dump-dtb", "", "write the BIOS FDT blob to this path before boot")
	fs.BoolVar(&c.HostIO, "hostio", false, "enable non-hermetic custom MMIO host filesystem passthrough for -bios")
	fs.BoolVar(&c.Net, "net", false, "enable non-hermetic virtio-net MMIO device for -bios")
	fs.BoolVar(&c.NetDirectTailnet, "net-direct", false, "connect -net directly to one tsnet stack instead of the shared emunet leader")
	fs.StringVar(&c.EmunetAddr, "emunet-addr", "", "local emunet rendezvous address; empty uses 127.0.0.1:7557")
	fs.BoolVar(&c.EmunetTrace, "emunet-trace", false, "write emunet packet drop trace lines to the emunet oplog")
	fs.StringVar(&c.TsnetDir, "tsnet-dir", "", "tsnet state directory; empty uses $HOME/.emunet/riscv-emu")
	fs.StringVar(&c.TsnetHostname, "tsnet-hostname", "", "tsnet hostname; empty uses riscv-emu")
	fs.StringVar(&c.TsnetAuthKey, "tsnet-authkey", "", "tsnet auth key for unattended first authorization")
	fs.BoolVar(&c.TsnetEphemeral, "tsnet-ephemeral", false, "request ephemeral tsnet node state")
	fs.StringVar(&c.TsnetGuestIPv4, "tsnet-guest-ipv4", "", "override guest DHCP IPv4 address")
	fs.StringVar(&c.TsnetDHCPServerIPv4, "tsnet-dhcp-server-ipv4", "", "override DHCP server IPv4 advertised to the guest")
	fs.StringVar(&c.TsnetDNSIPv4, "tsnet-dns-ipv4", "", "override DNS IPv4 advertised to the guest")
	fs.StringVar(&c.Machine, "machine", "virt", "machine model for -bios; currently only virt")
	fs.Uint64Var(&c.Seed, "seed", 0, "pseudo random number generator seed")
	fs.StringVar(&c.Memory, "mem", "", "guest memory size as bytes or KB/MB/GB/TB; with -bios this is RAM advertised to Linux")
	fs.StringVar(&c.Budget, "budget", "", "scheduler/run budget as an instruction count, duration, or max; defaults to "+defaultEmuRunBudgetDescription+" for -run and max for -bios")
	fs.BoolVar(&c.JITLazy, "jitlazy", false, "run with the native lazy JIT instead of the interpreter")
	fs.BoolVar(&c.JITAOT, "jitaot", false, "run with explicit AOT JIT instead of the interpreter")
	fs.BoolVar(&c.Hermit, "hermit", false, "disable host filesystem passthrough and networking for determinism. conflicts with -bios and -net")
	fs.BoolVar(&c.Deadlock, "deadlock", false, "run each thread until it blocks before scheduling another thread (at most one of -deadlock -prng or -chaos may be given; if none the default is a fixed quantum of -budget duration)")
	fs.BoolVar(&c.PRNG, "prng", false, "use deterministic PRNG scheduling quantum and clock advancement")
	fs.BoolVar(&c.Chaos, "chaos", false, "use deterministic chaos scheduling")
	fs.Int64Var(&c.RealtimeOffsetNS, "init", defaultEmuRealtimeStartNS, "initial realtime clock value in nanoseconds since Unix epoch; default is 2000-01-01T00:00:00Z")
	fs.StringVar(&c.Idle, "idle", "", "BIOS/Linux WFI host sleep cap as a duration, e.g. 5ms; empty keeps the built-in 1ms default")
	fs.BoolVar(&c.List, "list", false, "list running emu instances with attachable consoles")
	fs.BoolVar(&c.Debug, "debug", false, "attach to console 1 of the single other running emu instance")
	fs.IntVar(&c.AttachPID, "pid", 0, "attach mode: host PID of an existing emu process")
	fs.IntVar(&c.AttachConsole, "console", -1, "attach mode: console index to attach to with -pid")
}

func programPath(c *riscv.EmuConfig) string {
	if c.BiosPath != "" {
		return c.BiosPath
	}
	return c.RunPath
}
