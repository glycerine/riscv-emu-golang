package main

import (
	"bytes"
	"encoding/binary"
	"fmt"

	riscv "github.com/glycerine/riscv-emu-golang"
)

const (
	biosFDTAddr   = uint64(0x88000000)
	virtRAMBase   = uint64(0x80000000)
	virtCPUIntcPH = uint32(1)
	virtPLICPH    = uint32(2)
)

type biosGuest struct {
	mem     *riscv.GuestMemory
	cpu     *riscv.CPU
	elf     *riscv.ELF
	fdtAddr uint64
}

func runEmuBios(cfg EmuConfig, budget uint64) (int, error) {
	if cfg.JITLazy || cfg.JITAOT {
		return 0, fmt.Errorf("-jitlazy and -jitaot are not supported with -bios yet")
	}
	guest, err := prepareBiosGuest(cfg)
	if err != nil {
		return 0, err
	}
	defer guest.mem.Free()

	res, err := riscv.RunDefaultBudget(guest.cpu, &guest.cpu.Notes, budget)
	if err != nil {
		return 0, err
	}
	if res == riscv.RunBudgetExit {
		return guest.cpu.ExitCode, nil
	}
	return 0, nil
}

func prepareBiosGuest(cfg EmuConfig) (*biosGuest, error) {
	cfg = cfg.withDefaults()
	if cfg.BiosPath == "" {
		return nil, fmt.Errorf("prepare BIOS guest requires -bios")
	}
	if cfg.MemorySize <= biosFDTAddr {
		return nil, fmt.Errorf("-mem %#x is too small for BIOS FDT address %#x", cfg.MemorySize, biosFDTAddr)
	}

	mem, err := riscv.NewGuestMemory(cfg.MemorySize)
	if err != nil {
		return nil, err
	}
	elf, err := riscv.LoadELF(mem, cfg.BiosPath)
	if err != nil {
		mem.Free()
		return nil, err
	}

	fdt, err := buildVirtFDT(cfg.MemorySize)
	if err != nil {
		mem.Free()
		return nil, err
	}
	if fault := mem.WriteBytes(biosFDTAddr, fdt); fault != nil {
		mem.Free()
		return nil, fault
	}

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetReg(10, 0)           // a0: boot hart id
	cpu.SetReg(11, biosFDTAddr) // a1: flattened device tree pointer

	return &biosGuest{
		mem:     mem,
		cpu:     cpu,
		elf:     elf,
		fdtAddr: biosFDTAddr,
	}, nil
}

func buildVirtFDT(memSize uint64) ([]byte, error) {
	if memSize <= virtRAMBase {
		return nil, fmt.Errorf("memory size %#x does not cover virt RAM base %#x", memSize, virtRAMBase)
	}
	ramSize := memSize - virtRAMBase
	b := newFDTBuilder()

	b.beginNode("")
	b.propCells("#address-cells", 2)
	b.propCells("#size-cells", 2)
	b.propStringList("compatible", "riscv-emu-golang,virt", "riscv-virtio")
	b.propString("model", "riscv-emu-golang virt")

	b.beginNode("chosen")
	b.propString("bootargs", "")
	b.propString("stdout-path", "/soc/uart@10000000")
	b.endNode()

	b.beginNode("memory@80000000")
	b.propString("device_type", "memory")
	b.propCells64("reg", virtRAMBase, ramSize)
	b.endNode()

	b.beginNode("cpus")
	b.propCells("#address-cells", 1)
	b.propCells("#size-cells", 0)
	b.propCells("timebase-frequency", 10000000)

	b.beginNode("cpu@0")
	b.propString("device_type", "cpu")
	b.propCells("reg", 0)
	b.propString("status", "okay")
	b.propString("compatible", "riscv")
	b.propString("riscv,isa", "rv64imafdcsu")
	b.propString("mmu-type", "riscv,sv48")

	b.beginNode("interrupt-controller")
	b.propEmpty("interrupt-controller")
	b.propCells("#interrupt-cells", 1)
	b.propString("compatible", "riscv,cpu-intc")
	b.propCells("phandle", virtCPUIntcPH)
	b.endNode()

	b.endNode()
	b.endNode()

	b.beginNode("soc")
	b.propCells("#address-cells", 2)
	b.propCells("#size-cells", 2)
	b.propEmpty("ranges")
	b.propString("compatible", "simple-bus")

	b.beginNode("clint@2000000")
	b.propStringList("compatible", "sifive,clint0", "riscv,clint0")
	b.propCells64("reg", 0x02000000, 0x00010000)
	b.propCells("interrupts-extended", virtCPUIntcPH, 3, virtCPUIntcPH, 7)
	b.endNode()

	b.beginNode("interrupt-controller@c000000")
	b.propEmpty("interrupt-controller")
	b.propCells("#interrupt-cells", 1)
	b.propString("compatible", "riscv,plic0")
	b.propCells64("reg", 0x0c000000, 0x04000000)
	b.propCells("riscv,ndev", 0x35)
	b.propCells("phandle", virtPLICPH)
	b.endNode()

	b.beginNode("uart@10000000")
	b.propString("compatible", "ns16550a")
	b.propCells64("reg", 0x10000000, 0x100)
	b.propCells("clock-frequency", 3686400)
	b.propCells("current-speed", 115200)
	b.propCells("interrupt-parent", virtPLICPH)
	b.propCells("interrupts", 10)
	b.endNode()

	b.endNode()
	b.endNode()

	return b.finish(), nil
}

const (
	fdtMagic     = uint32(0xd00dfeed)
	fdtVersion   = uint32(17)
	fdtCompatVer = uint32(16)

	fdtBeginNode = uint32(1)
	fdtEndNode   = uint32(2)
	fdtProp      = uint32(3)
	fdtEnd       = uint32(9)
)

type fdtBuilder struct {
	structs      bytes.Buffer
	strings      bytes.Buffer
	stringOffset map[string]uint32
}

func newFDTBuilder() *fdtBuilder {
	return &fdtBuilder{stringOffset: make(map[string]uint32)}
}

func (b *fdtBuilder) beginNode(name string) {
	b.putStruct32(fdtBeginNode)
	b.structs.WriteString(name)
	b.structs.WriteByte(0)
	alignBuffer4(&b.structs)
}

func (b *fdtBuilder) endNode() {
	b.putStruct32(fdtEndNode)
}

func (b *fdtBuilder) propEmpty(name string) {
	b.prop(name, nil)
}

func (b *fdtBuilder) propString(name, value string) {
	b.prop(name, []byte(value+"\x00"))
}

func (b *fdtBuilder) propStringList(name string, values ...string) {
	var data bytes.Buffer
	for _, value := range values {
		data.WriteString(value)
		data.WriteByte(0)
	}
	b.prop(name, data.Bytes())
}

func (b *fdtBuilder) propCells(name string, cells ...uint32) {
	var data bytes.Buffer
	for _, cell := range cells {
		_ = binary.Write(&data, binary.BigEndian, cell)
	}
	b.prop(name, data.Bytes())
}

func (b *fdtBuilder) propCells64(name string, cells ...uint64) {
	var data bytes.Buffer
	for _, cell := range cells {
		_ = binary.Write(&data, binary.BigEndian, uint32(cell>>32))
		_ = binary.Write(&data, binary.BigEndian, uint32(cell))
	}
	b.prop(name, data.Bytes())
}

func (b *fdtBuilder) prop(name string, data []byte) {
	b.putStruct32(fdtProp)
	b.putStruct32(uint32(len(data)))
	b.putStruct32(b.stringRef(name))
	b.structs.Write(data)
	alignBuffer4(&b.structs)
}

func (b *fdtBuilder) stringRef(name string) uint32 {
	if off, ok := b.stringOffset[name]; ok {
		return off
	}
	off := uint32(b.strings.Len())
	b.strings.WriteString(name)
	b.strings.WriteByte(0)
	b.stringOffset[name] = off
	return off
}

func (b *fdtBuilder) putStruct32(v uint32) {
	_ = binary.Write(&b.structs, binary.BigEndian, v)
}

func (b *fdtBuilder) finish() []byte {
	b.putStruct32(fdtEnd)
	structBlock := b.structs.Bytes()
	stringBlock := b.strings.Bytes()

	const headerSize = 40
	memReserve := make([]byte, 16)
	offMemRsvMap := uint32(headerSize)
	offDtStruct := uint32(align4(headerSize + len(memReserve)))
	offDtStrings := uint32(align4(int(offDtStruct) + len(structBlock)))
	totalSize := uint32(int(offDtStrings) + len(stringBlock))

	out := make([]byte, totalSize)
	putFDTHeader(out[0:4], fdtMagic)
	putFDTHeader(out[4:8], totalSize)
	putFDTHeader(out[8:12], offDtStruct)
	putFDTHeader(out[12:16], offDtStrings)
	putFDTHeader(out[16:20], offMemRsvMap)
	putFDTHeader(out[20:24], fdtVersion)
	putFDTHeader(out[24:28], fdtCompatVer)
	putFDTHeader(out[28:32], 0)
	putFDTHeader(out[32:36], uint32(len(stringBlock)))
	putFDTHeader(out[36:40], uint32(len(structBlock)))
	copy(out[offMemRsvMap:], memReserve)
	copy(out[offDtStruct:], structBlock)
	copy(out[offDtStrings:], stringBlock)
	return out
}

func putFDTHeader(dst []byte, v uint32) {
	binary.BigEndian.PutUint32(dst, v)
}

func alignBuffer4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

func align4(v int) int {
	return (v + 3) &^ 3
}
