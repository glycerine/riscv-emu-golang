package main

import (
	"fmt"
	"os"

	riscv "github.com/glycerine/riscv-emu-golang"

	// embeds the bootables into our binary.
	"github.com/glycerine/riscv-emu-golang/xendor"
)

func main() {
	//biosPath := "fw_dynamic.elf"
	biosPath := "opensbi/build/platform/generic/firmware/fw_dynamic.elf"
	bios0, err := xendor.Bootables.Open(biosPath)
	panicOn(err)
	bios, err := bios0.Stat()

	//kernelPath := "Image"
	kernelPath := "linux-6.17-hand-built/Image"
	panicOn(err)
	kernel0, err := xendor.Bootables.Open(kernelPath)
	panicOn(err)
	kernel, err := kernel0.Stat()
	panicOn(err)

	//ramfsPath := "initramfs.cpio.gz"
	ramfsPath := "linux/initramfs.cpio.gz"
	ramfs0, err := xendor.Bootables.Open(ramfsPath)
	panicOn(err)
	ramfs, err := ramfs0.Stat()
	panicOn(err)

	fmt.Printf("embedded size of   bios: %v path: '%v'\n", bios.Size(), biosPath)
	fmt.Printf("embedded size of  ramfs: %v path: '%v'\n", ramfs.Size(), ramfsPath)
	fmt.Printf("embedded size of kernel: %v path: '%v'\n", kernel.Size(), kernelPath)

	cfg := &riscv.EmuConfig{
		Bootables:         &xendor.Bootables,
		Idle:              "1s",
		BiosPath:          biosPath,   // "fw_dynamic.elf",
		KernelPath:        kernelPath, // "Image",
		InitrdPath:        ramfsPath,  //"initramfs.cpio.gz",
		Net:               true,
		HostIO:            true,
		Append:            "console=ttyS0,115200 earlycon=uart8250,mmio,0x10000000 rdinit=/init panic=1 reboot=t init_on_alloc=0 init_on_free=0 audit=0 lsm=capability cma=0 numa=off slub_debug=- lpj=XXXXX",
		Machine:           "virt",
		Memory:            "256MB",
		MemorySize:        riscv.Size256MB,
		Budget:            "",
		InstructionBudget: ^uint64(0),
		RealtimeOffsetNS:  int64(946684800000000000), // 2000-01-01T00:00:00Z
		AttachConsole:     -1,
	}

	if err := cfg.ValidateConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	code, err := riscv.RunEmu(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emu: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func panicOn(err error) {
	if err != nil {
		panic(err)
	}
}
