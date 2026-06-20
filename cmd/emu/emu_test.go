package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	riscv "github.com/glycerine/riscv-emu-golang"
	"github.com/tetratelabs/wazero/rv64emu"
)

func TestDefineFlagsJIT0InstallsRV64EmuAccelerator(t *testing.T) {
	cfg := &riscv.EmuConfig{}
	fs := flag.NewFlagSet("emu", flag.ContinueOnError)
	defineFlags(fs, cfg)

	if err := fs.Parse([]string{"-jit0", "-run", "tiny.elf"}); err != nil {
		t.Fatalf("Parse -jit0: %v", err)
	}
	if cfg.AccelFactory == nil {
		t.Fatal("-jit0 did not install an accelerator factory")
	}
	accel, err := cfg.AccelFactory()
	if err != nil {
		t.Fatalf("AccelFactory: %v", err)
	}
	defer accel.Close()
	if _, ok := accel.(*rv64emu.Engine); !ok {
		t.Fatalf("AccelFactory returned %T, want *rv64emu.Engine", accel)
	}
	if !cfg.AccelOptions.ExactInstructionAccounting {
		t.Fatal("-jit0 did not enable exact instruction accounting")
	}
	if !cfg.AccelOptions.DebugInterpreterFallback {
		t.Fatal("-jit0 did not enable bring-up interpreter fallback")
	}
}

func TestDefineFlagsJIT0FalseLeavesInterpreter(t *testing.T) {
	cfg := &riscv.EmuConfig{}
	fs := flag.NewFlagSet("emu", flag.ContinueOnError)
	defineFlags(fs, cfg)

	if err := fs.Parse([]string{"-jit0=false", "-run", "tiny.elf"}); err != nil {
		t.Fatalf("Parse -jit0=false: %v", err)
	}
	if cfg.AccelFactory != nil {
		t.Fatal("-jit0=false installed an accelerator factory")
	}
}

func TestDefineFlagsJIT0AOTBreadthOptions(t *testing.T) {
	cfg := &riscv.EmuConfig{}
	fs := flag.NewFlagSet("emu", flag.ContinueOnError)
	defineFlags(fs, cfg)

	if err := fs.Parse([]string{"-jit0", "-jit0-sweep", "-jit0-max-aot-blocks", "512", "-run", "tiny.elf"}); err != nil {
		t.Fatalf("Parse -jit0 AOT options: %v", err)
	}
	if cfg.AccelFactory == nil {
		t.Fatal("-jit0 did not install an accelerator factory")
	}
	if !cfg.AccelOptions.ConservativeAOTSweep {
		t.Fatal("-jit0-sweep did not enable conservative AOT sweep")
	}
	if cfg.AccelOptions.MaxAOTPrecompileBlocks != 512 {
		t.Fatalf("MaxAOTPrecompileBlocks = %d, want 512", cfg.AccelOptions.MaxAOTPrecompileBlocks)
	}
}

func TestDefineFlagsJIT0RejectsOldJITsThroughConfig(t *testing.T) {
	elfPath := filepath.Join(t.TempDir(), "tiny.elf")
	if err := os.WriteFile(elfPath, []byte{0}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &riscv.EmuConfig{}
	fs := flag.NewFlagSet("emu", flag.ContinueOnError)
	defineFlags(fs, cfg)

	if err := fs.Parse([]string{"-jit0", "-jitlazy", "-run", elfPath}); err != nil {
		t.Fatalf("Parse -jit0 -jitlazy: %v", err)
	}
	err := cfg.ValidateConfig()
	if err == nil || !strings.Contains(err.Error(), "accelerator cannot be combined with -jitlazy or -jitaot") {
		t.Fatalf("ValidateConfig error = %v, want accelerator/old-JIT conflict", err)
	}
}
