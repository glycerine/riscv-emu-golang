package main

import (
	"flag"
	"fmt"
	"os"

	riscv "github.com/glycerine/riscv-emu-golang"
)

var ProgramName = "emux"

type EmuxConfig struct {
	RunPath string
	Seed    uint64
}

func (c *EmuxConfig) DefineFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.RunPath, "run", "", "path to RISCV ELF binary to run")
	fs.Uint64Var(&c.Seed, "seed", 0, "pseudo random number generator seed")
}

func (c *EmuxConfig) ValidateConfig() error {
	if c.RunPath == "" {
		return fmt.Errorf("-run path required and missing")
	}
	if !fileExists(c.RunPath) {
		return fmt.Errorf("-run path '%v' does not exist", c.RunPath)
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
	err = cfg.ValidateConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// instantiate a virtual machine, use the seed for
	// /dev/urandom, then run the RunPath binary on it.
	mem, err := riscv.NewGuestMemory(riscv.Size64MB)
	panicOn(err)
	cpu := riscv.NewCPU(*mem)
	_ = cpu
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
